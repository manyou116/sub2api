package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge"
	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/proxy"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

type bpsResponsesKey struct{}

func WithBPSResponses(ctx context.Context, model string) context.Context {
	if strings.TrimSpace(model) != "" {
		return context.WithValue(ctx, bpsResponsesKey{}, true)
	}
	return ctx
}

// WithBPSChatCompletions admits the Chat message format supported by the BPS
// adapter while keeping existing routing for other request shapes.
func WithBPSChatCompletions(ctx context.Context, model string, body []byte) context.Context {
	input := gjson.ParseBytes(body)
	if messages := input.Get("messages"); !messages.IsArray() || !messages.Get("0").Exists() ||
		input.Get("input").Exists() || input.Get("previous_response_id").String() != "" || input.Get("n").Int() > 1 {
		return ctx
	}
	for _, tool := range input.Get("tools").Array() {
		if tool.Get("type").String() != "function" || !tool.Get("function").IsObject() {
			return ctx
		}
	}
	return WithBPSResponses(ctx, model)
}

func bpsRouteMismatch(ctx context.Context, a *Account, model string, compact bool) bool {
	if !a.IsBPSShadow() {
		return strings.HasPrefix(model, bpsModelPrefix)
	}
	mapped, matched := a.ResolveMappedModel(model)
	return compact || ctx.Value(bpsResponsesKey{}) != true || IsGPTImageGenerationModel(mapped) ||
		(strings.HasPrefix(model, bpsModelPrefix) && !matched)
}

// Older shadows had one forced alias; keep its shared quota check for existing bindings.
func bpsTargetModel(child *Account) string {
	for alias, model := range child.GetModelMapping() {
		if model != "" && alias == bpsModelPrefix+model {
			return model
		}
	}
	return ""
}

func bpsCheckRuntime(ctx context.Context, p bpsbridge.Runtime, temp string, bound bpsbridge.BoundGrant) error {
	auth := filepath.Join(temp, "probe.json")
	if err := bpsbridge.Save(auth, bound); err != nil {
		return err
	}
	worker, err := p.Start(ctx, auth, filepath.Join(temp, "task"), 1)
	if err != nil {
		return err
	}
	worker.Stop()
	return nil
}

type bpsRuntimeKey struct {
	shadowID    int64
	dir         string
	proxy       string
	concurrency int
}
type bpsResponseWorker interface {
	Responses(context.Context, bpsbridge.BoundGrant, []byte) (*http.Response, error)
	Done() <-chan struct{}
	Stop()
}

type bpsRuntimeWorker struct {
	mu     sync.Mutex
	key    bpsRuntimeKey
	worker bpsResponseWorker
	users  int
	idle   *time.Timer
}

var bpsWorkers sync.Map

func bpsAcquireWorker(ctx context.Context, p bpsbridge.Runtime, child *Account) (bpsResponseWorker, func(), error) {
	key := bpsRuntimeKey{child.ID, p.Dir, p.Proxy, child.Concurrency}
	entry, _ := bpsWorkers.LoadOrStore(child.ID, &bpsRuntimeWorker{})
	slot, ok := entry.(*bpsRuntimeWorker)
	if !ok {
		return nil, nil, bpsbridge.ErrUnavailable
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.worker != nil {
		select {
		case <-slot.worker.Done():
			slot.worker = nil
		default:
		}
	}
	if slot.worker != nil && slot.key != key {
		if slot.users != 0 {
			return nil, nil, bpsbridge.ErrUnavailable
		}
		slot.worker.Stop()
		slot.worker = nil
	}
	if slot.worker == nil {
		worker, err := p.Start(ctx, bpsGrantPath(p), filepath.Join(p.Dir, "task"), child.Concurrency)
		if err != nil {
			return nil, nil, err
		}
		slot.worker, slot.key = worker, key
	}
	slot.users++
	if slot.idle != nil {
		slot.idle.Stop()
	}
	return slot.worker, func() {
		slot.mu.Lock()
		defer slot.mu.Unlock()
		slot.users--
		if slot.users == 0 && slot.worker != nil {
			worker := slot.worker
			slot.idle = time.AfterFunc(time.Minute, func() {
				slot.mu.Lock()
				defer slot.mu.Unlock()
				if slot.users == 0 && slot.worker == worker {
					slot.worker.Stop()
					slot.worker = nil
				}
			})
		}
	}, nil
}

func bpsIsStream(resp *http.Response) bool {
	return resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}

func bpsReadStream(r io.Reader, maxLine int, write func(string, []byte) error) (gjson.Result, error) {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 16<<10), maxLine)
	for scan.Scan() {
		line := scan.Text()
		data, ok := extractOpenAISSEDataLine(line)
		if !ok {
			continue
		} // The CLI emits one compact JSON data line per event.
		if !gjson.Valid(data) {
			return gjson.Result{}, bpsbridge.ErrUnavailable
		}
		event := gjson.Parse(data)
		switch event.Get("type").String() {
		case "response.completed", "response.incomplete", "response.failed", "error":
			return event, write(data, []byte(line))
		default:
			if err := write(data, []byte(line)); err != nil {
				return gjson.Result{}, err
			}
		}
	}
	if err := scan.Err(); err != nil {
		return gjson.Result{}, err
	}
	return gjson.Result{}, io.ErrUnexpectedEOF
}

func bpsSharedQuotaReset(errPayload gjson.Result, now time.Time) (time.Time, bool) {
	if errPayload.Get("type").String() != "usage_limit_reached" &&
		errPayload.Get("code").String() != "usage_limit_reached" {
		return time.Time{}, false
	}
	unix := errPayload.Get("resets_at").Int()
	if unix <= 0 {
		return time.Time{}, false
	}
	reset := time.Unix(unix, 0)
	return reset, reset.After(now) && reset.Before(now.Add(8*24*time.Hour))
}

func bpsWebSocketReuseScope(c *gin.Context, account *Account, model string) string {
	enabled, _ := account.Extra["bps_ws_session_reuse"].(bool)
	source := SourceUserFromContext(c.Request.Context())
	keyID := getAPIKeyIDFromContext(c)
	session := explicitOpenAIHeaderSessionID(c)
	if !enabled || source == nil || source.SourceID == "" || source.UserID == "" ||
		keyID <= 0 || session == "" || len(session) > 256 {
		return ""
	}
	return fmt.Sprintf("%d/%q/%q/%q/%q", keyID, source.SourceID, source.UserID, session, model)
}

func (s *OpenAIGatewayService) forwardBPS(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	return s.forwardBPSWithProtocol(ctx, c, account, body, nil)
}

func (s *OpenAIGatewayService) forwardBPSChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	var chat apicompat.ChatCompletionsRequest
	if json.Unmarshal(body, &chat) != nil {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "Invalid Chat Completions request")
		return nil, errors.New("bps_invalid_request")
	}
	return s.forwardBPSWithProtocol(ctx, c, account, body, &chat)
}

func (s *OpenAIGatewayService) forwardBPSWithProtocol(ctx context.Context, c *gin.Context, account *Account, body []byte, chat *apicompat.ChatCompletionsRequest) (*OpenAIForwardResult, error) {
	fail := func(status int, code string) (*OpenAIForwardResult, error) {
		switch {
		case chat != nil && chat.Stream && c.Writer.Written():
			MarkOpsStreamFailure(c, "upstream_error", code, code, status)
			MarkResponseCommitted(c)
			_, _ = c.Writer.WriteString(buildChatStreamErrorSSE(code, code) + "data: [DONE]\n\n")
			c.Writer.Flush()
		default:
			if chat != nil {
				MarkResponseCommitted(c)
			}
			c.JSON(status, gin.H{"error": gin.H{"type": code, "message": code}})
		}
		return nil, errors.New(code) // Never a scheduler failover or retry.
	}
	start := time.Now()
	if c.Request.Context().Value(bpsResponsesKey{}) != true {
		return fail(http.StatusBadRequest, "bps_responses_only")
	}
	model := gjson.GetBytes(body, "model").String()
	billingModel, mapped := resolveOpenAIForwardMappedModels(account, model, false)
	if mapped == "" || bpsRouteMismatch(c.Request.Context(), account, model, false) ||
		!supportsOpenAIRequestModel(account, model, false) || gjson.GetBytes(body, "previous_response_id").String() != "" ||
		OpenAIImageGenerationIntentFromContext(ctx) {
		return fail(http.StatusBadRequest, "bps_requires_text_and_full_history")
	}
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		switch tool.Get("type").String() {
		case "function", "custom", "namespace":
		default:
			return fail(http.StatusBadRequest, "bps_hosted_tool_unsupported")
		}
	}
	stream := gjson.GetBytes(body, "stream").Bool()
	if chat != nil {
		stream = chat.Stream
		chat.Model = mapped
		request, err := chatCompletionsToResponses(s.cfg, chat)
		if err != nil {
			return fail(http.StatusBadRequest, "bps_invalid_request")
		}
		body, err = json.Marshal(request)
		if err != nil {
			return fail(http.StatusBadRequest, "bps_invalid_request")
		}
	}
	bound, p, err := bpsReadBound(account.ID)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "bps_authorization_required")
	}
	parent, err := s.accountRepo.GetByID(ctx, *account.ParentAccountID)
	if err != nil || !bpsMatchesParent(bound, parent) {
		return fail(http.StatusServiceUnavailable, "bps_authorization_required")
	}
	if !bpsParentQuotaAllows(ctx, parent, mapped) {
		return fail(http.StatusTooManyRequests, "bps_shared_quota_unavailable")
	}
	if parent.ProxyID != nil {
		if parent.Proxy == nil {
			return fail(http.StatusServiceUnavailable, "bps_parent_proxy_unavailable")
		}
		p.Proxy = parent.Proxy.URL()
	}
	body, err = sjson.SetBytes(body, "model", mapped)
	if err != nil {
		return fail(http.StatusBadRequest, "bps_invalid_request")
	}
	worker, release, err := bpsAcquireWorker(ctx, p, account)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "bps_bridge_unavailable")
	}
	defer release()
	reuseScope := bpsWebSocketReuseScope(c, account, mapped)
	ctx, reused := proxy.WithWebSocketReuse(ctx, reuseScope)
	resp, err := worker.Responses(ctx, bound, body)
	if err != nil {
		return fail(http.StatusBadGateway, "bps_result_unconfirmed")
	}
	defer func() { _ = resp.Body.Close() }()
	if !bpsIsStream(resp) {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		if resp.StatusCode == http.StatusTooManyRequests {
			s.noteBPSQuota(ctx, parent.ID, gjson.GetBytes(raw, "error"))
			return fail(http.StatusTooManyRequests, "bps_rate_limited")
		}
		return fail(http.StatusBadGateway, "bps_upstream_rejected")
	}
	SetActualOpenAIUpstreamEndpoint(c, "/bps/responses")
	var chatState *apicompat.ResponsesEventToChatState
	if chat != nil && stream {
		chatState = apicompat.NewResponsesEventToChatState()
		chatState.Model = model
		chatState.IncludeUsage = true
	}
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
	}
	stats := &OpenAIForwardResult{Model: model, UpstreamModel: mapped, BillingModel: billingModel, Stream: stream, UpstreamEndpoint: "/bps/responses"}
	terminal, err := bpsReadStream(resp.Body, 16<<20, func(data string, _ []byte) error {
		event := gjson.Parse(data)
		eventType := event.Get("type").String()
		if eventType == "" || strings.ContainsAny(eventType, "\r\n") {
			return bpsbridge.ErrUnavailable
		}
		if stats.FirstTokenMs == nil && strings.HasSuffix(eventType, ".delta") {
			ms := int(time.Since(start).Milliseconds())
			stats.FirstTokenMs = &ms
		}
		if !stream {
			return nil
		}
		if chatState != nil {
			if eventType == "response.failed" || eventType == "error" {
				return nil // Handle terminal errors without emitting a success finish chunk.
			}
			var event apicompat.ResponsesStreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return err
			}
			for _, chunk := range apicompat.ResponsesEventToChatChunks(&event, chatState) {
				frame, err := apicompat.ChatChunkToSSE(chunk)
				if err != nil {
					return err
				}
				if _, err := c.Writer.WriteString(frame); err != nil {
					return err
				}
				c.Writer.Flush()
			}
			return nil
		}
		line := "data: " + data
		if model != mapped {
			line = s.replaceModelInSSELine(line, mapped, model)
		}
		if _, err := c.Writer.WriteString("event: " + eventType + "\n" + line + "\n\n"); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	})
	if reused != nil {
		logger.FromContext(ctx).Debug("bps.ws_turn", zap.Bool("reused", reused.Load()),
			zap.Int64("duration_ms", time.Since(start).Milliseconds()), zap.Any("first_text_ms", stats.FirstTokenMs),
			zap.Bool("completed", err == nil && terminal.Get("type").String() == "response.completed"))
		if err == nil && terminal.Get("type").String() == "response.completed" {
			// Let the embedded reader park the socket before closing the response pipe.
			_, _ = io.Copy(io.Discard, resp.Body)
		}
	}
	if err != nil {
		if chat != nil && ctx.Err() == nil {
			return fail(http.StatusBadGateway, "bps_result_unconfirmed")
		}
		if !stream && ctx.Err() == nil {
			return fail(http.StatusBadGateway, "bps_result_unconfirmed")
		}
		if ctx.Err() == nil {
			_, _ = c.Writer.WriteString("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"bps_result_unconfirmed\"}}}\n\n")
			c.Writer.Flush()
		}
		return nil, errors.New("bps_result_unconfirmed")
	}
	switch event := terminal.Get("type").String(); event {
	case "response.completed", "response.incomplete":
		if chat != nil {
			if stream {
				if _, err := c.Writer.WriteString("data: [DONE]\n\n"); err != nil {
					return nil, err
				}
				c.Writer.Flush()
			} else {
				var final *apicompat.ResponsesResponse
				if json.Unmarshal([]byte(terminal.Get("response").Raw), &final) != nil || final == nil {
					return fail(http.StatusBadGateway, "bps_result_unconfirmed")
				}
				c.JSON(http.StatusOK, apicompat.ResponsesToChatCompletions(final, model))
			}
		} else if !stream {
			if response := terminal.Get("response"); response.IsObject() {
				c.Data(http.StatusOK, "application/json", s.replaceModelInResponseBody([]byte(response.Raw), mapped, model))
			} else {
				return fail(http.StatusBadGateway, "bps_result_unconfirmed")
			}
		} else if event == "response.incomplete" {
			return nil, errors.New("bps_generation_incomplete")
		}
		stats.UpstreamResponseModel = terminal.Get("response.model").String()
		stats.ResponseID = terminal.Get("response.id").String()
		stats.RequestID = stats.ResponseID
		stats.Duration = time.Since(start)
		u := terminal.Get("response.usage")
		stats.Usage.InputTokens = int(u.Get("input_tokens").Int())
		stats.Usage.OutputTokens = int(u.Get("output_tokens").Int())
		stats.Usage.CacheReadInputTokens = int(u.Get("input_tokens_details.cached_tokens").Int())
		return stats, nil
	case "response.failed", "error":
		s.noteBPSQuota(ctx, parent.ID, terminal.Get("response.error"))
		s.noteBPSQuota(ctx, parent.ID, terminal.Get("error"))
		if !stream || chat != nil {
			return fail(http.StatusBadGateway, "bps_generation_failed")
		}
		return nil, errors.New("bps_generation_failed")
	default:
		return nil, errors.New("bps_generation_incomplete")
	}
}

func bpsParentQuotaAllows(ctx context.Context, parent *Account, model string) bool {
	if parent.IsRateLimited() || parent.isModelRateLimitedWithContext(ctx, model) {
		return false
	}
	paused, _ := shouldAutoPauseOpenAIAccountByQuota(ctx, parent)
	return !paused
}

func (s *OpenAIGatewayService) noteBPSQuota(ctx context.Context, parentID int64, payload gjson.Result) {
	reset, ok := bpsSharedQuotaReset(payload, time.Now())
	if !ok {
		return
	}
	if repo, ok := s.accountRepo.(interface {
		SetRateLimitedIfLater(context.Context, int64, time.Time) error
	}); ok {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = repo.SetRateLimitedIfLater(work, parentID, reset)
	}
}
