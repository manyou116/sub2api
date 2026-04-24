package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

type openAIImagesCompatMode string

const (
	openAIImagesCompatModeChat      openAIImagesCompatMode = "chat_completions"
	openAIImagesCompatModeResponses openAIImagesCompatMode = "responses"
)

type openAIImagesCompatCaptureWriter struct {
	header  http.Header
	body    bytes.Buffer
	status  int
	written bool
}

func newOpenAIImagesCompatCaptureWriter() *openAIImagesCompatCaptureWriter {
	return &openAIImagesCompatCaptureWriter{
		header: make(http.Header),
		status: http.StatusOK,
	}
}

func (w *openAIImagesCompatCaptureWriter) Header() http.Header {
	return w.header
}

func (w *openAIImagesCompatCaptureWriter) WriteHeader(code int) {
	if code > 0 && !w.written {
		w.status = code
	}
}

func (w *openAIImagesCompatCaptureWriter) WriteHeaderNow() {
	if !w.written {
		w.written = true
	}
}

func (w *openAIImagesCompatCaptureWriter) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	return w.body.Write(data)
}

func (w *openAIImagesCompatCaptureWriter) WriteString(data string) (int, error) {
	w.WriteHeaderNow()
	return w.body.WriteString(data)
}

func (w *openAIImagesCompatCaptureWriter) Status() int {
	return w.status
}

func (w *openAIImagesCompatCaptureWriter) Size() int {
	if !w.written {
		return -1
	}
	return w.body.Len()
}

func (w *openAIImagesCompatCaptureWriter) Written() bool {
	return w.written
}

func (w *openAIImagesCompatCaptureWriter) Flush() {}

func (w *openAIImagesCompatCaptureWriter) CloseNotify() <-chan bool {
	ch := make(chan bool)
	return ch
}

func (w *openAIImagesCompatCaptureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("hijack is not supported")
}

func (w *openAIImagesCompatCaptureWriter) Pusher() http.Pusher {
	return nil
}

func (h *OpenAIGatewayHandler) tryHandleOpenAIImagesCompat(
	c *gin.Context,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
	body []byte,
	reqStream bool,
	requestStart time.Time,
	reqLog *zap.Logger,
	mode openAIImagesCompatMode,
) bool {
	var (
		compatBody []byte
		matched    bool
		err        error
	)

	switch mode {
	case openAIImagesCompatModeChat:
		compatBody, matched, err = service.BuildOpenAIImageCompatRequestBodyFromChatCompletions(body)
	case openAIImagesCompatModeResponses:
		compatBody, matched, err = service.BuildOpenAIImageCompatRequestBodyFromResponses(body)
	default:
		return false
	}
	if !matched {
		return false
	}
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return true
	}

	h.handleOpenAIImagesCompat(c, apiKey, subject, body, compatBody, reqStream, requestStart, reqLog, mode)
	return true
}

func (h *OpenAIGatewayHandler) handleOpenAIImagesCompat(
	c *gin.Context,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
	originalBody []byte,
	compatBody []byte,
	reqStream bool,
	requestStart time.Time,
	reqLog *zap.Logger,
	mode openAIImagesCompatMode,
) {
	parseCtx, _ := cloneOpenAIImagesCompatContext(c, compatBody)
	parsed, err := h.gatewayService.ParseOpenAIImagesRequest(parseCtx, compatBody)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	setUpstreamEndpointOverride(c, parsed.Endpoint)

	reqLog = reqLog.With(
		zap.Bool("image_markdown_compat", true),
		zap.String("image_model", parsed.Model),
		zap.String("compat_mode", string(mode)),
	)

	setOpsRequestContext(c, parsed.Model, reqStream, originalBody)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, parsed.Model)
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	streamStarted := false
	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription); err != nil {
		reqLog.Info("openai.images_compat.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	sessionHash := h.gatewayService.GenerateImageSessionHash(c, compatBody, parsed.StickySessionSeed())

	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError

	for {
		reqLog.Debug("openai.images_compat.account_selecting", zap.Int("excluded_account_count", len(failedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForImages(
			c.Request.Context(),
			apiKey.GroupID,
			sessionHash,
			parsed.Model,
			failedAccountIDs,
			parsed.RequiredCapability,
		)
		if err != nil {
			reqLog.Warn("openai.images_compat.account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available compatible accounts", streamStarted)
				return
			}
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, streamStarted)
			} else {
				h.handleFailoverExhaustedSimple(c, 502, streamStarted)
			}
			return
		}
		if selection == nil || selection.Account == nil {
			h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available compatible accounts", streamStarted)
			return
		}

		reqLog.Debug("openai.images_compat.account_schedule_decision",
			zap.String("layer", scheduleDecision.Layer),
			zap.Bool("sticky_session_hit", scheduleDecision.StickySessionHit),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
			zap.Int("top_k", scheduleDecision.TopK),
			zap.Int64("latency_ms", scheduleDecision.LatencyMs),
			zap.Float64("load_skew", scheduleDecision.LoadSkew),
		)

		account := selection.Account
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai.images_compat.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		setOpsSelectedAccount(c, account.ID, account.Platform)

		accountReleaseFunc, acquired := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !acquired {
			return
		}

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()

		tempCtx, capture := cloneOpenAIImagesCompatContext(c, compatBody)
		if h.errorPassthroughService != nil {
			service.BindErrorPassthroughService(tempCtx, h.errorPassthroughService)
		}
		result, err := h.gatewayService.ForwardImages(c.Request.Context(), tempCtx, account, compatBody, parsed, channelMapping.MappedModel)

		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		if accountReleaseFunc != nil {
			accountReleaseFunc()
		}
		upstreamLatencyMs, _ := getContextInt64(tempCtx, service.OpsUpstreamLatencyMsKey)
		if upstreamLatencyMs == 0 {
			upstreamLatencyMs, _ = getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		}
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		if err == nil && result != nil && result.FirstTokenMs != nil {
			service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
		}

		if err != nil {
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
				if failoverErr.RetryableOnSameAccount {
					retryLimit := account.GetPoolModeRetryCount()
					if sameAccountRetryCount[account.ID] < retryLimit {
						sameAccountRetryCount[account.ID]++
						reqLog.Warn("openai.images_compat.pool_mode_same_account_retry",
							zap.Int64("account_id", account.ID),
							zap.Int("upstream_status", failoverErr.StatusCode),
							zap.Int("retry_limit", retryLimit),
							zap.Int("retry_count", sameAccountRetryCount[account.ID]),
						)
						select {
						case <-c.Request.Context().Done():
							return
						case <-time.After(sameAccountRetryDelay):
						}
						continue
					}
				}
				h.gatewayService.RecordOpenAIAccountSwitch()
				failedAccountIDs[account.ID] = struct{}{}
				lastFailoverErr = failoverErr
				if switchCount >= maxAccountSwitches {
					h.handleFailoverExhausted(c, failoverErr, streamStarted)
					return
				}
				switchCount++
				reqLog.Warn("openai.images_compat.upstream_failover_switching",
					zap.Int64("account_id", account.ID),
					zap.Int("upstream_status", failoverErr.StatusCode),
					zap.Int("switch_count", switchCount),
					zap.Int("max_switches", maxAccountSwitches),
				)
				continue
			}

			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
			if !reqStream && writeOpenAIImagesCompatCapturedError(c, capture) {
				reqLog.Warn("openai.images_compat.forward_failed",
					zap.Int64("account_id", account.ID),
					zap.Bool("captured_error_response_written", true),
					zap.Error(err),
				)
				return
			}
			wroteFallback := h.ensureForwardErrorResponse(c, streamStarted)
			reqLog.Warn("openai.images_compat.forward_failed",
				zap.Int64("account_id", account.ID),
				zap.Bool("fallback_error_response_written", wroteFallback),
				zap.Error(err),
			)
			return
		}

		markdown := bytes.TrimSpace(capture.body.Bytes())
		if len(markdown) == 0 {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
			h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", "Image generation returned empty markdown output", streamStarted)
			return
		}

		if writeErr := writeOpenAIImagesCompatResponse(c, mode, parsed.Model, markdown, result.Usage, reqStream, originalBody); writeErr != nil {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
			h.handleStreamingAwareError(c, http.StatusBadGateway, "api_error", "Failed to write markdown image response", streamStarted)
			reqLog.Warn("openai.images_compat.response_write_failed",
				zap.Int64("account_id", account.ID),
				zap.Error(writeErr),
			)
			return
		}

		if account.Type == service.AccountTypeOAuth {
			h.gatewayService.UpdateCodexUsageSnapshotFromHeaders(c.Request.Context(), account.ID, result.ResponseHeaders)
		}
		h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, result.FirstTokenMs)

		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)
		requestPayloadHash := service.HashUsageRequestPayload(originalBody)

		h.submitUsageRecordTask(func(ctx context.Context) {
			if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
				Result:             result,
				APIKey:             apiKey,
				User:               apiKey.User,
				Account:            account,
				Subscription:       subscription,
				InboundEndpoint:    GetInboundEndpoint(c),
				UpstreamEndpoint:   GetUpstreamEndpoint(c, account.Platform),
				UserAgent:          userAgent,
				IPAddress:          clientIP,
				RequestPayloadHash: requestPayloadHash,
				APIKeyService:      h.apiKeyService,
				ChannelUsageFields: channelMapping.ToUsageFields(parsed.Model, result.UpstreamModel),
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.images_compat"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", apiKey.ID),
					zap.Any("group_id", apiKey.GroupID),
					zap.String("model", parsed.Model),
					zap.Int64("account_id", account.ID),
				).Error("openai.images_compat.record_usage_failed", zap.Error(err))
			}
		})

		reqLog.Debug("openai.images_compat.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

func cloneOpenAIImagesCompatContext(parent *gin.Context, body []byte) (*gin.Context, *openAIImagesCompatCaptureWriter) {
	req := parent.Request.Clone(parent.Request.Context())
	clonedURL := *req.URL
	clonedURL.Path = EndpointImagesGenerations
	req.URL = &clonedURL
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header = req.Header.Clone()
	req.Header.Set("Content-Type", "application/json")

	capture := newOpenAIImagesCompatCaptureWriter()
	temp := parent.Copy()
	temp.Request = req
	temp.Writer = capture
	return temp, capture
}

func writeOpenAIImagesCompatCapturedError(c *gin.Context, capture *openAIImagesCompatCaptureWriter) bool {
	if capture == nil || capture.Status() < http.StatusBadRequest || capture.body.Len() == 0 {
		return false
	}
	contentType := strings.TrimSpace(capture.Header().Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	c.Data(capture.Status(), contentType, capture.body.Bytes())
	return true
}

func writeOpenAIImagesCompatResponse(
	c *gin.Context,
	mode openAIImagesCompatMode,
	model string,
	markdown []byte,
	usage service.OpenAIUsage,
	stream bool,
	originalBody []byte,
) error {
	switch mode {
	case openAIImagesCompatModeChat:
		resp, err := service.BuildOpenAIImagesMarkdownChatCompletionsResponse(markdown, model, usage)
		if err != nil {
			return err
		}
		return writeOpenAIImagesCompatChatCompletionsResponse(c, resp, stream, gjson.GetBytes(originalBody, "stream_options.include_usage").Bool())
	case openAIImagesCompatModeResponses:
		resp := service.BuildOpenAIImagesMarkdownResponsesResponse(markdown, model, usage)
		return writeOpenAIImagesCompatResponsesResponse(c, resp, stream)
	default:
		return fmt.Errorf("unsupported images compat mode: %s", mode)
	}
}

func writeOpenAIImagesCompatChatCompletionsResponse(
	c *gin.Context,
	resp *apicompat.ChatCompletionsResponse,
	stream bool,
	includeUsage bool,
) error {
	if resp == nil {
		return fmt.Errorf("chat completions response is required")
	}
	if !stream {
		body, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", body)
		return nil
	}

	content := ""
	if len(resp.Choices) > 0 {
		_ = json.Unmarshal(resp.Choices[0].Message.Content, &content)
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Status(http.StatusOK)

	roleChunk := apicompat.ChatCompletionsChunk{
		ID:      resp.ID,
		Object:  "chat.completion.chunk",
		Created: resp.Created,
		Model:   resp.Model,
		Choices: []apicompat.ChatChunkChoice{{
			Index: 0,
			Delta: apicompat.ChatDelta{Role: "assistant"},
		}},
	}
	if err := writeOpenAIImagesCompatChatChunk(c, roleChunk); err != nil {
		return err
	}
	if content != "" {
		contentChunk := roleChunk
		contentChunk.Choices = []apicompat.ChatChunkChoice{{
			Index: 0,
			Delta: apicompat.ChatDelta{Content: &content},
		}}
		if err := writeOpenAIImagesCompatChatChunk(c, contentChunk); err != nil {
			return err
		}
	}
	finishReason := "stop"
	finishChunk := roleChunk
	finishChunk.Choices = []apicompat.ChatChunkChoice{{
		Index:        0,
		Delta:        apicompat.ChatDelta{},
		FinishReason: &finishReason,
	}}
	if err := writeOpenAIImagesCompatChatChunk(c, finishChunk); err != nil {
		return err
	}
	if includeUsage && resp.Usage != nil {
		usageChunk := roleChunk
		usageChunk.Choices = []apicompat.ChatChunkChoice{}
		usageChunk.Usage = resp.Usage
		if err := writeOpenAIImagesCompatChatChunk(c, usageChunk); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	if err == nil {
		c.Writer.Flush()
	}
	return err
}

func writeOpenAIImagesCompatChatChunk(c *gin.Context, chunk apicompat.ChatCompletionsChunk) error {
	sse, err := apicompat.ChatChunkToSSE(chunk)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(c.Writer, sse); err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}

func writeOpenAIImagesCompatResponsesResponse(c *gin.Context, resp *apicompat.ResponsesResponse, stream bool) error {
	if resp == nil {
		return fmt.Errorf("responses response is required")
	}
	if !stream {
		body, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", body)
		return nil
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Status(http.StatusOK)

	itemID := ""
	text := ""
	if len(resp.Output) > 0 {
		itemID = resp.Output[0].ID
		if len(resp.Output[0].Content) > 0 {
			text = resp.Output[0].Content[0].Text
		}
	}

	createdEvent := apicompat.ResponsesStreamEvent{
		Type: "response.created",
		Response: &apicompat.ResponsesResponse{
			ID:     resp.ID,
			Object: resp.Object,
			Model:  resp.Model,
			Status: "in_progress",
		},
	}
	if err := writeOpenAIImagesCompatResponsesEvent(c, createdEvent); err != nil {
		return err
	}
	if len(resp.Output) > 0 {
		item := resp.Output[0]
		addedItem := item
		addedItem.Content = nil
		addedItem.Status = "in_progress"
		if err := writeOpenAIImagesCompatResponsesEvent(c, apicompat.ResponsesStreamEvent{
			Type:        "response.output_item.added",
			OutputIndex: 0,
			Item:        &addedItem,
		}); err != nil {
			return err
		}
		if text != "" {
			if err := writeOpenAIImagesCompatResponsesEvent(c, apicompat.ResponsesStreamEvent{
				Type:         "response.output_text.delta",
				ItemID:       itemID,
				OutputIndex:  0,
				ContentIndex: 0,
				Delta:        text,
			}); err != nil {
				return err
			}
			if err := writeOpenAIImagesCompatResponsesEvent(c, apicompat.ResponsesStreamEvent{
				Type:         "response.output_text.done",
				ItemID:       itemID,
				OutputIndex:  0,
				ContentIndex: 0,
				Text:         text,
			}); err != nil {
				return err
			}
		}
		doneItem := item
		doneItem.Content = nil
		doneItem.Status = "completed"
		if err := writeOpenAIImagesCompatResponsesEvent(c, apicompat.ResponsesStreamEvent{
			Type:        "response.output_item.done",
			OutputIndex: 0,
			Item:        &doneItem,
		}); err != nil {
			return err
		}
	}
	if err := writeOpenAIImagesCompatResponsesEvent(c, apicompat.ResponsesStreamEvent{
		Type:     "response.completed",
		Response: resp,
	}); err != nil {
		return err
	}
	_, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	if err == nil {
		c.Writer.Flush()
	}
	return err
}

func writeOpenAIImagesCompatResponsesEvent(c *gin.Context, evt apicompat.ResponsesStreamEvent) error {
	sse, err := apicompat.ResponsesEventToSSE(evt)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(c.Writer, sse); err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}
