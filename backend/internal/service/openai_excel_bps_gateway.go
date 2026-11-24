package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const excelBPSResponsesEndpoint = "/basispoints/api/responses"

var excelBPSReplayCache basispoints.ReplayCache
var excelBPSCatalogCache basispoints.CatalogCache
var excelBPSAttachments basispoints.AttachmentCache
var excelBPSSearchSlots = make(chan struct{}, 16)

func (a *Account) IsExcelBPSEnabled() bool {
	return a.connectionProtocol() == excelConnectionKind
}

func newExcelBPSRequest(ctx context.Context, endpoint string, body []byte, token, workspace string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(ctx), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Chatgpt-Account-Id", workspace)
	req.Header.Set("Content-Type", "application/json")
	if endpoint == basispoints.SearchURL {
		req.Header.Set("Accept", "application/json")
		return req, nil
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Openai-Account-Id", workspace)
	req.Header.Set("X-Basispoints-Auth-Mode", "chatgpt")
	req.Header.Set("Origin", "https://bps.openai.com")
	req.Header.Set("X-Openai-Internal-Basispoints-Client-Product", "basispoints-excel-plugin")
	req.Header.Set("X-Openai-Internal-Basispoints-Client-Agent-Profile", "excel")
	return req, nil
}

func (s *OpenAIGatewayService) forwardExcelBPSChat(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	var chat apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &chat); err != nil {
		return bpsResponseFailureWriter(c, &chat)(http.StatusBadRequest, "excel_bps_invalid_request")
	}
	return s.forwardExcelBPS(ctx, c, account, body, &chat)
}

func (s *OpenAIGatewayService) forwardExcelBPS(ctx context.Context, c *gin.Context, account *Account, body []byte, chat *apicompat.ChatCompletionsRequest) (*OpenAIForwardResult, error) {
	start := time.Now()
	fail := bpsResponseFailureWriter(c, chat)
	if openAIResponsesRequestPathSuffix(c) != "" || OpenAIImageGenerationIntentFromContext(ctx) {
		return fail(http.StatusBadRequest, "excel_bps_unsupported_endpoint")
	}
	originalModel := gjson.GetBytes(body, "model").String()
	billingModel, mapped := resolveOpenAIForwardMappedModels(account, originalModel, false)
	if mapped == "" || !supportsOpenAIRequestModel(account, originalModel, false) {
		return fail(http.StatusBadRequest, "excel_bps_model_unavailable")
	}
	stream := gjson.GetBytes(body, "stream").Bool()
	if chat != nil {
		stream = chat.Stream
		conversion := *chat
		conversion.Model = mapped
		converted, err := chatCompletionsToResponses(s.cfg, &conversion)
		if err != nil {
			return fail(http.StatusBadRequest, "excel_bps_invalid_request")
		}
		body, err = json.Marshal(converted)
		if err != nil {
			return fail(http.StatusBadRequest, "excel_bps_invalid_request")
		}
	}
	body, err := sjson.SetBytes(body, "model", mapped)
	if err != nil {
		return fail(http.StatusBadRequest, "excel_bps_invalid_request")
	}
	searchEnabled, _ := account.Extra[excelBPSSearchKey].(bool)
	body, searchPlan, err := basispoints.NormalizeWebSearchRequest(body, searchEnabled, false)
	if err != nil {
		return fail(http.StatusBadRequest, "excel_bps_invalid_search")
	}
	var images *basispoints.NativeImages
	if gjson.GetBytes(body, "input").IsArray() && bytes.Contains(body, []byte("input_image")) {
		images, err = basispoints.PrepareNativeImages(body)
		if err != nil {
			return fail(http.StatusBadRequest, "excel_bps_invalid_attachment")
		}
		body, err = images.Body()
		if err != nil {
			return fail(http.StatusBadRequest, "excel_bps_invalid_attachment")
		}
	}
	seed := strings.TrimSpace(codexClientSessionID(c))
	if seed == "" {
		seed = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	if seed == "" || getAPIKeyIDFromContext(c) == 0 {
		seed = uuid.NewString()
	}
	// Do not share tool history between tenants, accounts, mapped models or sessions.
	scope := isolateOpenAIUpstreamSessionID(getAPIKeyIDFromContext(c), account, mapped+"\x00"+seed)
	wire, bridge, err := basispoints.PrepareWithCatalog(body, scope, &excelBPSReplayCache, &excelBPSCatalogCache)
	if err != nil {
		return fail(http.StatusBadRequest, "excel_bps_incompatible_request")
	}

	auth, token, proxy, err := s.excelBPSCredential(ctx, account)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "excel_bps_authorization_unavailable")
	}
	send := func(requestCtx context.Context, endpoint string, payload []byte) (*http.Response, error) {
		req, err := newExcelBPSRequest(requestCtx, endpoint, payload, token, account.GetChatGPTAccountID())
		if err != nil {
			return nil, err
		}
		return s.sendExcelBPSRequest(req, account, auth, token, proxy)
	}

	if images != nil && images.HasImages() {
		attachmentScope := scope + "\x00" + token
		body, err = images.Upload(ctx, &excelBPSAttachments, attachmentScope, func(uploadCtx context.Context, image basispoints.InlineAttachment) (string, error) {
			return s.uploadExcelBPSAttachment(uploadCtx, account, auth, token, proxy, image)
		})
		if err != nil {
			return fail(http.StatusBadGateway, "excel_bps_attachment_upload_failed")
		}
		wire, bridge, err = bridge.Reprepare(body)
		if err != nil {
			return fail(http.StatusBadRequest, "excel_bps_incompatible_request")
		}
	}

	// No account or transport retry after submitting a generation.
	upstreamStart := time.Now()
	resp, err := send(ctx, basispoints.ResponsesURL, wire)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return fail(http.StatusBadGateway, "excel_bps_result_unconfirmed")
	}
	if !bpsIsStream(resp) {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			return fail(http.StatusTooManyRequests, "excel_bps_rate_limited")
		}
		return fail(http.StatusBadGateway, "excel_bps_upstream_rejected")
	}
	bodyReader := resp.Body
	if searchPlan == nil {
		bodyReader = bridge.Stream(resp.Body)
	} else {
		bodyReader = bridge.StreamWithSearch(ctx, resp.Body, wire, searchPlan,
			func(searchCtx context.Context, query string) (basispoints.SearchResult, error) {
				return doExcelBPSSearch(searchCtx, searchPlan, mapped, query, send)
			}, func(nextCtx context.Context, request []byte) (io.ReadCloser, error) {
				next, err := send(nextCtx, basispoints.ResponsesURL, request)
				if err != nil {
					return nil, errors.New("BPS continuation result unconfirmed")
				}
				if !bpsIsStream(next) {
					_ = next.Body.Close()
					return nil, fmt.Errorf("BPS continuation returned HTTP %d", next.StatusCode)
				}
				return next.Body, nil
			})
	}
	defer func() { _ = bodyReader.Close() }()
	resp.Body = bodyReader
	result, err := s.deliverBPSResponse(ctx, c, resp, chat, stream, originalModel, mapped, billingModel, excelBPSResponsesEndpoint, 0, start, nil)
	if result != nil {
		result.ReasoningEffort = &bridge.Effort
		result.RequestedReasoningEffort = &bridge.RequestedEffort
	}
	return result, err
}

func doExcelBPSSearch(ctx context.Context, plan *basispoints.SearchPlan, model, query string,
	send func(context.Context, string, []byte) (*http.Response, error),
) (basispoints.SearchResult, error) {
	select {
	case excelBPSSearchSlots <- struct{}{}:
		defer func() { <-excelBPSSearchSlots }()
	case <-ctx.Done():
		return basispoints.SearchResult{}, ctx.Err()
	}
	payload, err := plan.SearchRequestBody(model, uuid.NewString(), query)
	if err != nil {
		return basispoints.SearchResult{}, err
	}
	response, err := send(ctx, basispoints.SearchURL, payload)
	if err != nil {
		return basispoints.SearchResult{}, errors.New("BPS search result unconfirmed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return basispoints.SearchResult{}, fmt.Errorf("BPS search returned HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return basispoints.SearchResult{}, errors.New("BPS search result could not be read")
	}
	return basispoints.ParseSearchResult(raw)
}

func (s *OpenAIGatewayService) excelBPSCredential(ctx context.Context, account *Account) (*OpenAIOAuthService, string, string, error) {
	if s.openAITokenProvider == nil || s.openAITokenProvider.openAIOAuthService == nil {
		return nil, "", "", errors.New("authorization for Excel is unavailable")
	}
	proxy := ""
	if account.ProxyID != nil {
		if account.Proxy == nil {
			return nil, "", "", errors.New("account proxy for Excel is unavailable")
		}
		proxy = account.Proxy.URL()
	}
	auth := s.openAITokenProvider.openAIOAuthService
	token, err := auth.openAIConnectionToken(ctx, account.ID, excelConnectionKind)
	return auth, token, proxy, err
}

func (s *OpenAIGatewayService) sendExcelBPSRequest(req *http.Request, account *Account, auth *OpenAIOAuthService, token, proxy string) (*http.Response, error) {
	if err := auth.checkOpenAIConnectionEgress(req.Context(), account.ID, excelConnectionKind, token); err != nil {
		return nil, err
	}
	response, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err == nil && response != nil && response.StatusCode == http.StatusUnauthorized {
		auth.invalidateOpenAIConnectionToken(req.Context(), account.ID, excelConnectionKind, token)
	}
	return response, err
}

func excelBPSConfiguredModels(account *Account) []string {
	if account == nil {
		return nil
	}
	switch models := account.Extra[excelBPSModelsKey].(type) {
	case []string:
		return models
	case []any:
		names := make([]string, 0, len(models))
		for _, model := range models {
			if name, ok := model.(string); ok {
				names = append(names, name)
			}
		}
		return names
	}
	return nil
}

// Model-level routing happens before any upstream send. Protocol failures
// never cause a retry with the primary Codex credential.
func chooseOpenAIConnectionForModel(account *Account, model string) *Account {
	if !account.IsExcelBPSEnabled() {
		return account
	}
	allowed := excelBPSConfiguredModels(account)
	if len(allowed) == 0 {
		return account
	}
	mapped := account.GetMappedModel(model)
	for _, name := range allowed {
		if name == model || name == mapped {
			return account
		}
	}
	copy := *account
	copy.Extra = shallowCopyMap(account.Extra)
	copy.Extra[openAIConnectionProtocolKey] = "codex"
	return &copy
}
