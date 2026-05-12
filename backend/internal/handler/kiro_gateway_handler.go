// Kiro adapter handler.
//
// 复刻 ChatCompletions 的骨架（请求解析、用户认证、账号调度、计费、失败切换、
// panic 恢复），但 forward 阶段调用 KiroChatService 而不是 OpenAIGatewayService。
//
// MVP A 范围：
//   - 复用 SelectAccountWithScheduler 选 active Kiro 账号
//   - 复用 billing eligibility 检查
//   - 失败 → UpstreamFailoverError → 切换账号
//   - usage record 暂未接入（Phase 4 一并补）
//   - /v1/chat/completions 与 /v1/responses 共用本入口（Responses 为 chat 包装）
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	pkgip "github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// KiroChatCompletions 处理 /v1/chat/completions 当 group platform = kiro。
func (h *OpenAIGatewayHandler) KiroChatCompletions(c *gin.Context) {
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)

	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.kiro_gateway.chat_completions",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)

	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}
	if !gjson.ValidBytes(body) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}
	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()
	reqStream := gjson.GetBytes(body, "stream").Bool()
	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))

	setOpsRequestContext(c, reqModel, reqStream, body)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	subscription, _ := middleware2.GetSubscriptionFromContext(c)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription); err != nil {
		reqLog.Info("kiro_chat_completions.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHash(c, body)

	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	var lastFailoverErr *service.UpstreamFailoverError

	kiroChatService := service.NewKiroChatService()
	if h.kiroTokenProvider != nil {
		kiroChatService.SetTokenProvider(h.kiroTokenProvider)
	}

	for {
		reqLog.Debug("kiro_chat_completions.account_selecting", zap.Int("excluded", len(failedAccountIDs)))
		selection, err := h.gatewayService.SelectKiroAccount(
			c.Request.Context(),
			apiKey.GroupID,
			failedAccountIDs,
			reqModel,
		)
		_ = sessionHash
		if err != nil || selection == nil || selection.Account == nil {
			reqLog.Warn("kiro_chat_completions.account_select_failed", zap.Error(err))
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, streamStarted)
			} else {
				h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available Kiro accounts", streamStarted)
			}
			return
		}
		account := selection.Account
		setOpsSelectedAccount(c, account.ID, account.Platform)
		reqLog.Debug("kiro_chat_completions.account_selected", zap.Int64("account_id", account.ID))

		accountReleaseFunc, acquired := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !acquired {
			return
		}

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())

		result, err := kiroChatService.ChatCompletions(c.Request.Context(), c, account, body)

		if accountReleaseFunc != nil {
			accountReleaseFunc()
		}

		if err != nil {
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				h.gatewayService.RecordOpenAIAccountSwitch()
				failedAccountIDs[account.ID] = struct{}{}
				lastFailoverErr = failoverErr

				// 双层 quarantine + 错误分类：决定隔离 / 切号 / 透传
				decision := service.HandleKiroUpstreamError(account.ID, reqModel, failoverErr.StatusCode, failoverErr.ResponseBody)
				reqLog.Warn("kiro_chat_completions.upstream_error",
					zap.Int64("account_id", account.ID),
					zap.Int("upstream_status", failoverErr.StatusCode),
					zap.String("class", decision.Class.String()),
					zap.Duration("account_cooldown", decision.AccountCooldown),
					zap.Duration("model_cooldown", decision.ModelCooldown),
					zap.Bool("client_flood", decision.ClientFlood),
				)

				// 仅在「真正应当归罪于本账号」时才扣调度分。
				// ModelCapacity（上游模型整体过载）不是账号问题，跳过。
				if decision.Class != service.KiroErrModelCapacity &&
					decision.Class != service.KiroErrConversationTooLong &&
					decision.Class != service.KiroErrInvalidRequest {
					h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
				}

				// 不切号场景：直接把上游响应原样回给客户端
				//   - InvalidRequest / ConversationTooLong：客户端自己的请求问题
				//   - ModelCapacity 且客户端 flood：避免一个客户端拖垮所有账号
				if !decision.ShouldFailover {
					h.handleFailoverExhausted(c, failoverErr, streamStarted)
					return
				}

				if switchCount >= maxAccountSwitches {
					h.handleFailoverExhausted(c, failoverErr, streamStarted)
					return
				}
				switchCount++
				reqLog.Warn("kiro_chat_completions.upstream_failover_switching",
					zap.Int64("account_id", account.ID),
					zap.Int("upstream_status", failoverErr.StatusCode),
					zap.Int("switch_count", switchCount),
				)
				streamStarted = false
				continue
			}
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
			wroteFallback := h.ensureForwardErrorResponse(c, streamStarted)
			reqLog.Warn("kiro_chat_completions.forward_failed",
				zap.Int64("account_id", account.ID),
				zap.Bool("fallback_error_response_written", wroteFallback),
				zap.Error(err),
			)
			return
		}

		if result != nil && result.FirstTokenMs != nil {
			service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, result.FirstTokenMs)
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, nil)
		}
		// 成功后清除可能残留的隔离记录
		service.ClearKiroQuarantine(account.ID)
		service.ClearKiroModelCapacity(account.ID, reqModel)

		// 接 RecordUsage：把 Kiro 估算 token 写入 usage 表（DB 统计/计费可见）。
		// channelMapping 暂用 nil，billing 走默认；ResponseHeaders/Duration 等字段补齐。
		userAgent := c.GetHeader("User-Agent")
		clientIP := pkgip.GetClientIP(c)
		requestPayloadHash := service.HashUsageRequestPayload(body)
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)

		// 提取 Kiro 上游 trace id 透传到客户端响应头 + 写入 usage 记录，便于回查 quarantine。
		traceID := ""
		if result.UpstreamHeaders != nil {
			for _, k := range []string{"X-Amzn-Requestid", "X-Amzn-Trace-Id", "X-Request-Id"} {
				if v := result.UpstreamHeaders.Get(k); v != "" {
					traceID = v
					break
				}
			}
		}
		if traceID != "" {
			c.Writer.Header().Set("X-Upstream-Request-Id", traceID)
		}

		forwardResult := &service.OpenAIForwardResult{
			RequestID:       traceID,
			Model:           reqModel,
			UpstreamModel:   result.InternalModel,
			Stream:          result.Stream,
			ResponseHeaders: result.UpstreamHeaders,
			FirstTokenMs:    result.FirstTokenMs,
			Usage: service.OpenAIUsage{
				InputTokens:  int(result.InputTokens),
				OutputTokens: int(result.OutputTokens),
			},
		}
		recordResult := forwardResult
		recordAPIKey := apiKey
		recordAccount := account
		recordSubscription := subscription
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
				Result:             recordResult,
				APIKey:             recordAPIKey,
				User:               recordAPIKey.User,
				Account:            recordAccount,
				Subscription:       recordSubscription,
				InboundEndpoint:    inboundEndpoint,
				UpstreamEndpoint:   upstreamEndpoint,
				UserAgent:          userAgent,
				IPAddress:          clientIP,
				RequestPayloadHash: requestPayloadHash,
				APIKeyService:      h.apiKeyService,
			}); err != nil {
				reqLog.Warn("kiro_chat_completions.record_usage_failed", zap.Error(err))
			}
		}()

		reqLog.Debug("kiro_chat_completions.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
			zap.Int64("input_tokens", result.InputTokens),
			zap.Int64("output_tokens", result.OutputTokens),
		)
		return
	}
}

// KiroResponses 是 /v1/responses 的 Kiro 适配。
//
// 实现策略：把 OpenAI Responses 协议的 `input` 字段转换为 chat-completions 的 `messages`，
// 然后复用 KiroChatCompletions 走 CodeWhisperer。响应 shape 仍是 chat.completion——这是
// MVP 妥协，让 SDK 至少能拿到内容；完整的 Responses 输出包装（response.created 等）后续再做。
func (h *OpenAIGatewayHandler) KiroResponses(c *gin.Context) {
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	// 已是 chat 形式（messages 字段存在）则不转
	if !gjson.GetBytes(body, "messages").Exists() {
		messages, convErr := convertResponsesInputToChatMessages(body)
		if convErr != nil {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", convErr.Error())
			return
		}
		newBody, err := sjson.SetBytes(body, "messages", messages)
		if err != nil {
			h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to rewrite request body")
			return
		}
		newBody, _ = sjson.DeleteBytes(newBody, "input")
		body = newBody
	}

	// Responses 协议的 tools 字段是平铺的 {type:"function", name, description, parameters}，
	// 而 chat-completions 期待 {type:"function", function:{name, description, parameters}}。
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		converted := make([]map[string]any, 0, len(tools.Array()))
		for _, t := range tools.Array() {
			if t.Get("function").Exists() {
				// 已是 chat 形式，原样保留
				var raw any
				_ = json.Unmarshal([]byte(t.Raw), &raw)
				if m, ok := raw.(map[string]any); ok {
					converted = append(converted, m)
				}
				continue
			}
			if t.Get("type").String() != "function" {
				continue
			}
			fn := map[string]any{
				"name":        t.Get("name").String(),
				"description": t.Get("description").String(),
			}
			if params := t.Get("parameters"); params.Exists() {
				var p any
				if err := json.Unmarshal([]byte(params.Raw), &p); err == nil {
					fn["parameters"] = p
				}
			}
			converted = append(converted, map[string]any{"type": "function", "function": fn})
		}
		raw, _ := json.Marshal(converted)
		body, _ = sjson.SetRawBytes(body, "tools", raw)
	}

	// 替换 request body 给下游 ReadRequestBodyWithPrealloc 重新读
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))

	// 包装 ResponseWriter：拦截 chat-completion 输出 → 翻译成 Responses 协议
	stream := gjson.GetBytes(body, "stream").Bool()
	originalWriter := c.Writer
	shapeWriter := newResponsesShapeWriter(originalWriter, stream)
	c.Writer = shapeWriter

	defer func() {
		// 非流式：把缓冲的 chat JSON 翻译为 Responses JSON 写出
		shapeWriter.finalize()
		c.Writer = originalWriter
	}()

	h.KiroChatCompletions(c)
}

// convertResponsesInputToChatMessages 将 /v1/responses 的 input 字段转换为
// /v1/chat/completions 的 messages 数组。支持 3 种 input 形态：
//  1. 字符串 → [{role:user, content:<string>}]
//  2. 数组 of {role, content[{type,text}]} → 直接映射
//  3. 数组 of {type:"input_text"|"input_image",...}（无 role）→ 全部塞进单条 user 消息
func convertResponsesInputToChatMessages(body []byte) ([]map[string]any, error) {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return nil, errors.New("input is required")
	}
	if input.Type == gjson.String {
		return []map[string]any{{"role": "user", "content": input.String()}}, nil
	}
	if !input.IsArray() {
		return nil, errors.New("input must be string or array")
	}

	items := input.Array()
	if len(items) == 0 {
		return nil, errors.New("input array is empty")
	}

	// 探测是 message 数组还是 content-part 数组
	first := items[0]
	isMessageArray := first.Get("role").Exists() || first.Get("content").Exists() ||
		first.Get("type").String() == "function_call" || first.Get("type").String() == "function_call_output"

	if !isMessageArray {
		// content-part 数组：合并成一条 user 消息
		parts := make([]map[string]any, 0, len(items))
		for _, it := range items {
			parts = append(parts, responsesPartToChatPart(it))
		}
		return []map[string]any{{"role": "user", "content": parts}}, nil
	}

	// message 数组
	messages := make([]map[string]any, 0, len(items))
	for _, it := range items {
		// Responses 协议：function_call / function_call_output 是顶层 item
		itType := it.Get("type").String()
		if itType == "function_call" {
			messages = append(messages, map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   it.Get("call_id").String(),
					"type": "function",
					"function": map[string]any{
						"name":      it.Get("name").String(),
						"arguments": it.Get("arguments").String(),
					},
				}},
			})
			continue
		}
		if itType == "function_call_output" {
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": it.Get("call_id").String(),
				"content":      it.Get("output").String(),
			})
			continue
		}

		role := it.Get("role").String()
		if role == "" {
			role = "user"
		}
		msg := map[string]any{"role": role}

		content := it.Get("content")
		switch {
		case content.Type == gjson.String:
			msg["content"] = content.String()
		case content.IsArray():
			parts := make([]map[string]any, 0)
			for _, p := range content.Array() {
				parts = append(parts, responsesPartToChatPart(p))
			}
			msg["content"] = parts
		default:
			msg["content"] = ""
		}
		messages = append(messages, msg)
	}
	return messages, nil
}

// responsesPartToChatPart 把 Responses 协议的 content part 转为 chat 协议格式。
//   - input_text/output_text → {type:"text", text:"..."}
//   - input_image            → {type:"image_url", image_url:{url:"..."}}
//   - 其他类型按原样保留
func responsesPartToChatPart(p gjson.Result) map[string]any {
	t := p.Get("type").String()
	switch t {
	case "input_text", "output_text", "text":
		return map[string]any{"type": "text", "text": p.Get("text").String()}
	case "input_image":
		url := p.Get("image_url").String()
		if url == "" {
			url = p.Get("image_url.url").String()
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
	default:
		// 直接落原始 JSON 对象
		var raw map[string]any
		_ = json.Unmarshal([]byte(p.Raw), &raw)
		if raw == nil {
			raw = map[string]any{}
		}
		return raw
	}
}
