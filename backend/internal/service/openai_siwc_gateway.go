package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func normalizeSIWCBody(body []byte) ([]byte, error) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, infraerrors.BadRequest("OPENAI_SIWC_INVALID_REQUEST", "Invalid Responses request")
	}
	// This route has no server-side conversation storage or hosted tool_search.
	for _, key := range []string{"background", "conversation", "previous_response_id", "max_output_tokens", "max_completion_tokens", "max_tool_calls", "metadata", "moderation", "multi_agent", "prompt", "prompt_cache_retention", "prompt_cache_options", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "client_metadata", "turn_state", "x-codex-turn-state"} {
		delete(payload, key)
	}
	payload["store"] = false
	payload["stream"] = true
	if text, ok := payload["input"].(string); ok {
		payload["input"] = []any{map[string]any{"type": "message", "role": "user", "content": text}}
	}
	if payload["input"] == nil {
		payload["input"] = []any{}
	}
	if items, ok := payload["input"].([]any); ok {
		kept := make([]any, 0, len(items))
		for _, raw := range items {
			if item, ok := raw.(map[string]any); ok {
				if item["type"] == "configuration_update" {
					if update, ok := item["reasoning"].(map[string]any); ok {
						if effort, ok := update["effort"].(string); ok && effort != "" {
							reasoning, _ := payload["reasoning"].(map[string]any)
							if reasoning == nil {
								reasoning = map[string]any{}
							}
							reasoning["effort"] = effort
							payload["reasoning"] = reasoning
						}
					}
					continue
				}
				if item["role"] == "system" {
					item["role"] = "developer"
				}
			}
			kept = append(kept, raw)
		}
		payload["input"] = kept
	}
	if tools, ok := payload["tools"].([]any); ok {
		var local []any
		grouped := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("invalid SIWC tool declaration")
			}
			switch tool["type"] {
			case "function", "custom":
				local = append(local, tool)
			case "namespace":
				children, _ := tool["tools"].([]any)
				for _, child := range children {
					entry, ok := child.(map[string]any)
					if !ok || (entry["type"] != "function" && entry["type"] != "custom") {
						return nil, infraerrors.BadRequest("OPENAI_SIWC_UNSUPPORTED_TOOL", "SIWC namespaces support function/custom tools")
					}
				}
				grouped = append(grouped, tool)
			case "web_search", "web_search_preview":
				grouped = append(grouped, tool)
			default:
				return nil, infraerrors.BadRequest("OPENAI_SIWC_UNSUPPORTED_TOOL", "SIWC does not support this hosted tool; select Codex or use function/custom tools")
			}
		}
		if len(local) > 0 {
			merged := false
			for _, raw := range grouped {
				if namespace, ok := raw.(map[string]any); ok && namespace["type"] == "namespace" && namespace["name"] == "functions" {
					children, _ := namespace["tools"].([]any)
					namespace["tools"] = append(children, local...)
					merged = true
					break
				}
			}
			if !merged {
				grouped = append(grouped, map[string]any{"type": "namespace", "name": "functions", "tools": local})
			}
		}
		payload["tools"] = grouped
	}
	return json.Marshal(payload)
}

func (s *OpenAIGatewayService) buildSIWCRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string) (*http.Request, error) {
	if suffix := openAIResponsesRequestPathSuffix(c); suffix != "" {
		return nil, infraerrors.BadRequest("OPENAI_SIWC_UNSUPPORTED_ENDPOINT", "SIWC supports /v1/responses; compact and other endpoints are unavailable")
	}
	if token == "" {
		return nil, infraerrors.BadRequest("OPENAI_SIWC_AUTH_REQUIRED", "Authorize SIWC for this account first")
	}
	normalized, err := normalizeSIWCBody(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(ctx), http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader(normalized))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req = withSIWCRequestIdentity(req, c, account)
	applySIWCRequestHeaders(req)
	return req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI)), nil
}
func (s *OpenAIGatewayService) getSIWCRequestToken(ctx context.Context, account *Account) (string, string, error) {
	if s.openAITokenProvider == nil || s.openAITokenProvider.openAIOAuthService == nil {
		return "", "", errors.New("SIWC OAuth service is unavailable")
	}
	token, err := s.openAITokenProvider.openAIOAuthService.openAIConnectionToken(ctx, account.ID, siwcConnectionKind)
	return token, "siwc", err
}

func (s *OpenAIGatewayService) forwardSIWC(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	if openAIResponsesRequestPathSuffix(c) != "" {
		return nil, infraerrors.BadRequest("OPENAI_SIWC_UNSUPPORTED_ENDPOINT", "SIWC supports /v1/responses only")
	}
	model := gjson.GetBytes(body, "model").String()
	mapped := normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, model, ""))
	upstream, err := sjson.SetBytes(body, "model", mapped)
	if err != nil {
		return nil, err
	}
	if key := gjson.GetBytes(upstream, "prompt_cache_key").String(); key != "" {
		upstream, err = sjson.SetBytes(upstream, "prompt_cache_key", isolateOpenAIUpstreamSessionID(getAPIKeyIDFromContext(c), account, key))
		if err != nil {
			return nil, err
		}
	}
	SetActualOpenAIUpstreamEndpoint(c, openAIResponsesUpstreamEndpoint)
	return s.forwardOpenAIPassthrough(ctx, c, account, upstream, body, model, false, extractOpenAIReasoningEffortFromBody(body, model), gjson.GetBytes(body, "stream").Bool(), time.Now())
}

func (s *OpenAIGatewayService) doSIWCUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if request.URL == nil || request.URL.Scheme != "https" || request.URL.Host != "api.openai.com" ||
		request.URL.Path != "/v1/responses" || request.URL.User != nil || request.Method != http.MethodPost {
		return nil, errors.New("SIWC requests require the official Responses endpoint")
	}
	if _, ok := request.Context().Value(siwcRequestIdentityKey{}).(siwcRequestIdentity); !ok {
		return nil, errors.New("SIWC request did not use the SIWC builder")
	}
	authorization := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if s.openAITokenProvider != nil && s.openAITokenProvider.openAIOAuthService != nil {
		if err := s.openAITokenProvider.openAIOAuthService.checkOpenAIConnectionEgress(request.Context(), account.ID, siwcConnectionKind, authorization); err != nil {
			return nil, err
		}
	}
	applySIWCRequestHeaders(request)
	request.Host = ""
	resp, err := s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	if err == nil && resp != nil && resp.StatusCode == http.StatusUnauthorized && s.openAITokenProvider != nil && s.openAITokenProvider.openAIOAuthService != nil {
		s.openAITokenProvider.openAIOAuthService.invalidateOpenAIConnectionToken(request.Context(), account.ID, siwcConnectionKind, strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	}
	return resp, err
}

func (s *OpenAIGatewayService) getOpenAIHTTPInferenceToken(ctx context.Context, account *Account) (string, string, error) {
	if account.IsSIWCEnabled() {
		return s.getSIWCRequestToken(ctx, account)
	}
	return s.GetAccessToken(ctx, account)
}

func (s *OpenAIGatewayService) getOpenAIMessageInferenceToken(ctx context.Context, c *gin.Context, account *Account) (string, string, error) {
	if account.IsSIWCEnabled() {
		return s.getSIWCRequestToken(ctx, account)
	}
	return s.getRequestCredential(ctx, c, account)
}
