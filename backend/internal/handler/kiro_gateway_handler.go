// Kiro gateway adapters (P5).
//
// Reuses OpenAI gateway skeleton (auth, slots, billing, failover) while
// forwarding via KiroChatService. Sticky session + tenant-isolated
// conversationId are applied from the first day.
//
// Supported endpoints for platform=kiro groups:
//   - POST /v1/chat/completions
//   - POST /v1/responses  (bridged through Chat Completions + apicompat)
package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	pkgip "github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

type kiroForwardProtocol int

const (
	kiroProtocolChatCompletions kiroForwardProtocol = iota
	kiroProtocolResponses
)

// KiroChatCompletions handles /v1/chat/completions when group.platform = kiro.
func (h *OpenAIGatewayHandler) KiroChatCompletions(c *gin.Context) {
	h.kiroGateway(c, kiroProtocolChatCompletions)
}

// KiroResponses handles /v1/responses when group.platform = kiro.
// Upstream Kiro only speaks generateAssistantResponse; this path bridges
// Responses ↔ Chat Completions via apicompat (same completeness model as
// OpenAI force-chat-completions fallback).
func (h *OpenAIGatewayHandler) KiroResponses(c *gin.Context) {
	h.kiroGateway(c, kiroProtocolResponses)
}

func (h *OpenAIGatewayHandler) kiroGateway(c *gin.Context, protocol kiroForwardProtocol) {
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)

	requestStart := time.Now()
	logName := "handler.kiro_gateway.chat_completions"
	if protocol == kiroProtocolResponses {
		logName = "handler.kiro_gateway.responses"
	}

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
		logName,
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

	setOpsRequestContext(c, reqModel, reqStream)
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

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("kiro_gateway.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHash(c, body)
	// Prompt-cache identity is computed once per request and reused across account failover.
	mappedModel := service.MapKiroModel(reqModel)
	conversationID := service.ResolveKiroCacheIdentity(c, body, "", mappedModel)

	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	var lastFailoverErr *service.UpstreamFailoverError

	kiroChatService := service.NewKiroChatService()
	if h.kiroTokenProvider != nil {
		kiroChatService.SetTokenProvider(h.kiroTokenProvider)
	}

	userAgent := c.GetHeader("User-Agent")
	clientIP := pkgip.GetClientIP(c)

	for {
		reqLog.Debug("kiro_gateway.account_selecting", zap.Int("excluded", len(failedAccountIDs)))
		selection, err := h.gatewayService.SelectKiroAccount(
			c.Request.Context(),
			apiKey.GroupID,
			sessionHash,
			failedAccountIDs,
			reqModel,
		)
		if err != nil || selection == nil || selection.Account == nil {
			reqLog.Warn("kiro_gateway.account_select_failed", zap.Error(err))
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, streamStarted)
			} else {
				h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available Kiro accounts", streamStarted)
			}
			return
		}
		account := selection.Account
		setOpsSelectedAccount(c, account.ID, account.Platform)
		reqLog.Debug("kiro_gateway.account_selected", zap.Int64("account_id", account.ID))

		accountReleaseFunc, acquired := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !acquired {
			return
		}

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())

		var result *service.KiroChatResult
		switch protocol {
		case kiroProtocolResponses:
			result, err = kiroChatService.Responses(c.Request.Context(), c, account, body, conversationID)
		default:
			result, err = kiroChatService.ChatCompletions(c.Request.Context(), c, account, body, conversationID)
		}

		if accountReleaseFunc != nil {
			accountReleaseFunc()
		}

		if err != nil {
			if c.Writer.Written() {
				// Headers/body already committed by Kiro service (stream or JSON error).
				reqLog.Warn("kiro_gateway.forward_failed_after_write", zap.Error(err))
				hasPartial := result != nil && (result.AssembledContent != "" || result.OutputTokens > 0 || result.FirstTokenMs != nil)
				if !hasPartial {
					h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
					return
				}
				// fall through: record partial schedule/usage from result
			} else {
				var failoverErr *service.UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					h.gatewayService.RecordOpenAIAccountSwitch()
					lastFailoverErr = failoverErr
					if account == nil {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					failedAccountIDs[account.ID] = struct{}{}

					// Quarantine keys must match upstream/internal model ids.
					accountMapped := service.MapKiroModel(account.GetMappedModel(reqModel))
					if accountMapped == "" {
						accountMapped = mappedModel
						if accountMapped == "" {
							accountMapped = service.MapKiroModel(reqModel)
						}
					}
					decision := service.HandleKiroUpstreamError(account.ID, accountMapped, failoverErr.StatusCode, failoverErr.ResponseBody)
					reqLog.Warn("kiro_gateway.upstream_error",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.String("class", decision.Class.String()),
						zap.String("mapped_model", accountMapped),
						zap.Duration("account_cooldown", decision.AccountCooldown),
						zap.Duration("model_cooldown", decision.ModelCooldown),
						zap.Bool("should_failover", decision.ShouldFailover),
						zap.Bool("client_flood", decision.ClientFlood),
					)

					if decision.Class != service.KiroErrModelCapacity &&
						decision.Class != service.KiroErrConversationTooLong &&
						decision.Class != service.KiroErrInvalidRequest {
						h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
					}

					if !decision.ShouldFailover || decision.PassthroughBody || decision.ClientFlood {
						msg := string(failoverErr.ResponseBody)
						if msg == "" {
							msg = "upstream error"
						}
						h.handleStreamingAwareError(c, failoverErr.StatusCode, "api_error", msg, streamStarted)
						return
					}

					switchCount++
					if switchCount >= maxAccountSwitches {
						h.handleFailoverExhausted(c, lastFailoverErr, streamStarted)
						return
					}
					continue
				}

				h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
				reqLog.Error("kiro_gateway.forward_failed", zap.Error(err))
				h.handleStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", streamStarted)
				return
			}
		}

		if result != nil && result.FirstTokenMs != nil {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, result.FirstTokenMs)
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, nil)
		}

		// Clear model-capacity backoff after a successful upstream response.
		successModel := mappedModel
		if result != nil && result.InternalModel != "" {
			successModel = result.InternalModel
		}
		if successModel != "" {
			service.ClearKiroModelCapacity(account.ID, successModel)
		}

		// Successful path: record usage asynchronously.
		if result != nil {
			requestPayloadHash := service.HashUsageRequestPayload(body)
			inboundEndpoint := GetInboundEndpoint(c)
			upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)

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

			forwardResult := newKiroOpenAIForwardResult(traceID, reqModel, result)
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
					reqLog.Warn("kiro_gateway.record_usage_failed", zap.Error(err))
				}
			}()

			reqLog.Debug("kiro_gateway.request_completed",
				zap.Int64("account_id", account.ID),
				zap.Int("switch_count", switchCount),
				zap.Int64("input_tokens", result.InputTokens),
				zap.Int64("output_tokens", result.OutputTokens),
				zap.Int64("cache_creation_input_tokens", result.CacheCreationInputTokens),
				zap.Int64("cache_read_input_tokens", result.CacheReadInputTokens),
			)
		}
		return
	}
}

func newKiroOpenAIForwardResult(traceID string, requestedModel string, result *service.KiroChatResult) *service.OpenAIForwardResult {
	if result == nil {
		return nil
	}
	return &service.OpenAIForwardResult{
		RequestID:       traceID,
		Model:           requestedModel,
		UpstreamModel:   result.InternalModel,
		Stream:          result.Stream,
		ResponseHeaders: result.UpstreamHeaders,
		Duration:        result.Duration,
		FirstTokenMs:    result.FirstTokenMs,
		Usage: service.OpenAIUsage{
			InputTokens:              int(result.InputTokens),
			OutputTokens:             int(result.OutputTokens),
			CacheCreationInputTokens: int(result.CacheCreationInputTokens),
			CacheReadInputTokens:     int(result.CacheReadInputTokens),
		},
	}
}
