package service

// openai_images_legacy.go — web2api（f/conversation）路线的生图实现。
//
// 启用方式：在账号 extra 字段设置 "openai_oauth_legacy_images": true。
// 适用场景：Free/Plus 账号通过 f/conversation 接口生图（配额有限，sentinel 协议不稳定）。
//
// 协议链：
//   bootstrap（解析 chatgpt.com 获取 sentinel SDK URL 和 data-build）
//   → chat-requirements（获取 PoW seed/difficulty + sentinel token）
//   → 生成 requirements token + proof token（sha3-512 工作量证明）
//   → conversation/init → f/conversation/prepare（获取 conduit_token）
//   → 上传图片（三步：POST /files → PUT Azure blob → POST /files/{id}/uploaded）
//   → POST f/conversation SSE（stream 解析图片 pointer）
//   → 兜底 polling GET /backend-api/conversation/{id}
//   → 下载图片 → 构造 OpenAI 标准响应

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/imroc/req/v3"
	"github.com/tidwall/gjson"
	"golang.org/x/crypto/sha3"
)

// ─── 常量 ─────────────────────────────────────────────────────────────────────

const (
	openAIChatGPTConversationInitURL    = "https://chatgpt.com/backend-api/conversation/init"
	openAIChatGPTConversationURL        = "https://chatgpt.com/backend-api/f/conversation"
	openAIChatGPTConversationPrepareURL = "https://chatgpt.com/backend-api/f/conversation/prepare"
	openAIChatGPTChatRequirementsURL    = "https://chatgpt.com/backend-api/sentinel/chat-requirements"
	openAIImageRequirementsDiff         = "0fffff"
	openAIImageLifecycleTimeout         = 5 * time.Minute
	openAIDefaultSentinelSDKURL         = "https://chatgpt.com/backend-api/sentinel/sdk.js"
)

// ─── PoW 数据表（移植自 chatgpt2api utils/pow.py） ─────────────────────────────

var legacyPowCores = []int{8, 16, 24, 32}

var legacyPowDocumentKeys = []string{
	"_reactListeningo743lnnpvdg",
	"location",
}

var legacyPowNavigatorKeys = []string{
	"registerProtocolHandler\u2212function registerProtocolHandler() { [native code] }",
	"storage\u2212[object StorageManager]",
	"locks\u2212[object LockManager]",
	"appCodeName\u2212Mozilla",
	"permissions\u2212[object Permissions]",
	"share\u2212function share() { [native code] }",
	"webdriver\u2212false",
	"managed\u2212[object NavigatorManagedData]",
	"canShare\u2212function canShare() { [native code] }",
	"vendor\u2212Google Inc.",
	"mediaDevices\u2212[object MediaDevices]",
	"vibrate\u2212function vibrate() { [native code] }",
	"storageBuckets\u2212[object StorageBucketManager]",
	"mediaCapabilities\u2212[object MediaCapabilities]",
	"cookieEnabled\u2212true",
	"virtualKeyboard\u2212[object VirtualKeyboard]",
	"product\u2212Gecko",
	"presentation\u2212[object Presentation]",
	"onLine\u2212true",
	"mimeTypes\u2212[object MimeTypeArray]",
	"credentials\u2212[object CredentialsContainer]",
	"serviceWorker\u2212[object ServiceWorkerContainer]",
	"keyboard\u2212[object Keyboard]",
	"gpu\u2212[object GPU]",
	"doNotTrack",
	"serial\u2212[object Serial]",
	"pdfViewerEnabled\u2212true",
	"language\u2212zh-CN",
	"geolocation\u2212[object Geolocation]",
	"userAgentData\u2212[object NavigatorUAData]",
	"getUserMedia\u2212function getUserMedia() { [native code] }",
	"sendBeacon\u2212function sendBeacon() { [native code] }",
	"hardwareConcurrency\u221232",
	"windowControlsOverlay\u2212[object WindowControlsOverlay]",
}

var legacyPowWindowKeys = []string{
	"0", "window", "self", "document", "name", "location",
	"customElements", "history", "navigation", "innerWidth", "innerHeight",
	"scrollX", "scrollY", "visualViewport", "screenX", "screenY",
	"outerWidth", "outerHeight", "devicePixelRatio", "screen", "chrome",
	"navigator", "onresize", "performance", "crypto", "indexedDB",
	"sessionStorage", "localStorage", "scheduler", "alert", "atob", "btoa",
	"fetch", "matchMedia", "postMessage", "queueMicrotask", "requestAnimationFrame",
	"setInterval", "setTimeout", "caches",
	"__NEXT_DATA__", "__BUILD_MANIFEST", "__NEXT_PRELOADREADY",
}

// legacyPowProcessStart 用于计算进程运行时长（PoW config[13]）。
var legacyPowProcessStart = time.Now()

// ─── 账号能力检测 ─────────────────────────────────────────────────────────────

// IsOpenAILegacyImagesEnabled 判定 OAuth 账号是否走旧版 ChatGPT Web 生图链路。
//
// 三态语义（优先级 account > group）：
//   - 账号 extra.openai_oauth_legacy_images = true  → 强制启用（覆盖分组）
//   - 账号 extra.openai_oauth_legacy_images = false → 强制禁用（覆盖分组）
//   - 账号未设置该键 → 回落分组 OpenAILegacyImagesDefault
//   - group 为 nil 时仅看账号字段，未设置则视为 false
//
// 仅 OpenAI OAuth 账号生效；其他类型一律返回 false。
func (a *Account) IsOpenAILegacyImagesEnabled(group *Group) bool {
	if a == nil || !a.IsOpenAIOAuth() {
		return false
	}
	if a.Extra != nil {
		if enabled, ok := a.Extra["openai_oauth_legacy_images"].(bool); ok {
			return enabled
		}
	}
	if group != nil {
		return group.OpenAILegacyImagesDefault
	}
	return false
}

// ─── 失败熔断（仅作用于 image 维度，不影响 text/codex 调度） ─────────────────────

const (
	// legacyImagesScope 是 model_rate_limits 中 image 维度限流的 key。
	// 调度层 isAccountImageCapabilityCompatible 会读取该 key 进行过滤。
	legacyImagesScope = "legacy_images"

	// legacyImagesConsecutiveFailureThreshold 控制连续失败几次后熔断。
	legacyImagesConsecutiveFailureThreshold = 3
	// legacyImagesFailureCooldown 是连续失败触发的冷却窗口。
	legacyImagesFailureCooldown = 10 * time.Minute
	// legacyImagesRateLimitCooldown 是上游显式限流（429/quota）后的冷却窗口。
	legacyImagesRateLimitCooldown = time.Hour
)

// legacyImagesFailureCounter 记录每个账号连续失败次数（in-memory，重启清零）。
var legacyImagesFailureCounter sync.Map

func legacyImagesIncrementFailure(accountID int64) int64 {
	v, _ := legacyImagesFailureCounter.LoadOrStore(accountID, new(int64))
	ptr := v.(*int64)
	// 简化处理：单写入路径下不需要原子；本调用场景已是请求级序列化。
	*ptr++
	return *ptr
}

func legacyImagesResetFailure(accountID int64) {
	legacyImagesFailureCounter.Delete(accountID)
}

// recordLegacyImagesSuccess 在生图成功后清理失败计数。
func (s *OpenAIGatewayService) recordLegacyImagesSuccess(account *Account) {
	if account == nil {
		return
	}
	legacyImagesResetFailure(account.ID)
}

// recordLegacyImagesFailure 在生图失败后维护连续失败计数；
// 达到阈值后写入 model_rate_limits[legacy_images] reset_at = now+cooldown。
// 仅在非显式 rate-limit 场景调用（429/quota 由 wrap 单独处理）。
func (s *OpenAIGatewayService) recordLegacyImagesFailure(ctx context.Context, account *Account) {
	if account == nil || s.accountRepo == nil {
		return
	}
	count := legacyImagesIncrementFailure(account.ID)
	if count < legacyImagesConsecutiveFailureThreshold {
		return
	}
	resetAt := time.Now().Add(legacyImagesFailureCooldown).UTC()
	if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, legacyImagesScope, resetAt); err != nil {
		logger.LegacyPrintf(
			"service.openai_gateway",
			"[OpenAI] Legacy image circuit-breaker SetModelRateLimit failed account_id=%d err=%v",
			account.ID, err,
		)
		return
	}
	logger.LegacyPrintf(
		"service.openai_gateway",
		"[OpenAI] Legacy image circuit-breaker tripped account_id=%d consecutive=%d reset_at=%s",
		account.ID, count, resetAt.Format(time.RFC3339),
	)
	legacyImagesResetFailure(account.ID)
}

// recordLegacyImagesUpstreamRateLimit 处理 429/quota 显式限流：写入 1h 冷却。
func (s *OpenAIGatewayService) recordLegacyImagesUpstreamRateLimit(ctx context.Context, account *Account) {
	if account == nil || s.accountRepo == nil {
		return
	}
	resetAt := time.Now().Add(legacyImagesRateLimitCooldown).UTC()
	if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, legacyImagesScope, resetAt); err != nil {
		logger.LegacyPrintf(
			"service.openai_gateway",
			"[OpenAI] Legacy image upstream rate-limit SetModelRateLimit failed account_id=%d err=%v",
			account.ID, err,
		)
		return
	}
	logger.LegacyPrintf(
		"service.openai_gateway",
		"[OpenAI] Legacy image upstream rate-limited account_id=%d reset_at=%s",
		account.ID, resetAt.Format(time.RFC3339),
	)
	legacyImagesResetFailure(account.ID)
}

// isLegacyOpenAIImageRateLimitStatus 判定状态错误是否表示上游显式限流。
func isLegacyOpenAIImageRateLimitStatus(statusErr *legacyOpenAIImageStatusError) bool {
	if statusErr == nil {
		return false
	}
	if statusErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	msg := strings.ToLower(statusErr.Message)
	if strings.Contains(msg, "rate limit") || strings.Contains(msg, "quota") ||
		strings.Contains(msg, "you've reached") || strings.Contains(msg, "you have reached") {
		return true
	}
	bodyMsg := strings.ToLower(strings.TrimSpace(extractUpstreamErrorMessage(statusErr.ResponseBody)))
	return strings.Contains(bodyMsg, "rate limit") || strings.Contains(bodyMsg, "quota") ||
		strings.Contains(bodyMsg, "you've reached") || strings.Contains(bodyMsg, "you have reached")
}

// ─── 主入口 ──────────────────────────────────────────────────────────────────

// forwardOpenAIImagesOAuthLegacy 使用旧 web2api（f/conversation）接口生图。
// 由 ForwardImages 在账号设置了 openai_oauth_legacy_images: true 时调用。
func (s *OpenAIGatewayService) forwardOpenAIImagesOAuthLegacy(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	parsed *OpenAIImagesRequest,
	channelMappedModel string,
) (result *OpenAIForwardResult, retErr error) {
	// 失败熔断：成功 → 清失败计数；失败 → 累加，达到阈值后写入 image 维度限流。
	defer func() {
		if retErr == nil {
			s.recordLegacyImagesSuccess(account)
			// 同步本地用量缓存：避免「缓存未过期但配额已满」导致放行 1 张超额请求。
			if result != nil && result.ImageCount > 0 && account != nil {
				legacyImagesUsageCache.bumpAccount(account.ID, result.ImageCount)
			}
		} else {
			s.recordLegacyImagesFailure(ctx, account)
		}
	}()
	startTime := time.Now()
	requestModel := strings.TrimSpace(parsed.Model)
	if mapped := strings.TrimSpace(channelMappedModel); mapped != "" {
		requestModel = mapped
	}
	if err := validateOpenAIImagesModel(requestModel); err != nil {
		return nil, err
	}
	logger.LegacyPrintf(
		"service.openai_gateway",
		"[OpenAI] Images legacy request routing request_model=%s endpoint=%s account_type=%s uploads=%d",
		requestModel, parsed.Endpoint, account.Type, len(parsed.Uploads),
	)

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	client, err := newLegacyOpenAIBackendAPIClient(resolveLegacyOpenAIProxyURL(account))
	if err != nil {
		return nil, err
	}
	headers, err := s.buildLegacyOpenAIBackendAPIHeaders(account, token)
	if err != nil {
		return nil, err
	}

	// bootstrap：GET chatgpt.com 拿浏览器指纹 cookie 并解析 sentinel SDK 资源。
	scriptSources, dataBuild := bootstrapLegacyOpenAIBackendAPI(ctx, client, headers)

	// sentinel握手：获取 PoW 参数、arkose 检测、sentinel token。
	chatReqs, err := fetchLegacyOpenAIChatRequirements(ctx, client, headers, scriptSources, dataBuild)
	if err != nil {
		return nil, s.wrapLegacyOpenAIImageBackendError(ctx, c, account, err)
	}
	if chatReqs.Arkose.Required {
		return nil, s.wrapLegacyOpenAIImageBackendError(ctx, c, account,
			newLegacyOpenAIImageSyntheticStatusError(
				http.StatusForbidden,
				"chat-requirements requires unsupported challenge (arkose)",
				openAIChatGPTChatRequirementsURL,
			),
		)
	}

	ua := headers.Get("User-Agent")
	proofToken, err := buildLegacyProofToken(
		chatReqs.ProofOfWork.Required,
		chatReqs.ProofOfWork.Seed,
		chatReqs.ProofOfWork.Difficulty,
		ua, scriptSources, dataBuild,
	)
	if err != nil {
		// PoW 解题失败 → 该账号当次不可重试，触发换号。
		return nil, s.wrapLegacyOpenAIImageBackendError(ctx, c, account,
			newLegacyOpenAIImageSyntheticStatusError(
				http.StatusForbidden,
				"proof token solve failed: "+err.Error(),
				openAIChatGPTChatRequirementsURL,
			),
		)
	}

	parentMessageID := uuid.NewString()
	_ = initializeLegacyOpenAIImageConversation(ctx, client, headers)
	conduitToken, err := prepareLegacyOpenAIImageConversation(
		ctx, client, headers, parsed.Prompt, parentMessageID, chatReqs.Token, proofToken,
	)
	if err != nil {
		return nil, s.wrapLegacyOpenAIImageBackendError(ctx, c, account, err)
	}

	uploads, err := uploadLegacyOpenAIImageFiles(ctx, client, headers, parsed.Uploads)
	if err != nil {
		return nil, s.wrapLegacyOpenAIImageBackendError(ctx, c, account, err)
	}

	convReq := buildLegacyOpenAIImageConversationRequest(parsed, parentMessageID, uploads)
	if parsedContent, marshalErr := json.Marshal(convReq); marshalErr == nil {
		setOpsUpstreamRequestBody(c, parsedContent)
	}
	convHeaders := cloneHTTPHeader(headers)
	convHeaders.Set("Accept", "text/event-stream")
	convHeaders.Set("Content-Type", "application/json")
	convHeaders.Set("openai-sentinel-chat-requirements-token", chatReqs.Token)
	if conduitToken != "" {
		convHeaders.Set("x-conduit-token", conduitToken)
	}
	if proofToken != "" {
		convHeaders.Set("openai-sentinel-proof-token", proofToken)
	}

	resp, err := client.R().
		SetContext(ctx).
		DisableAutoReadResponse().
		SetHeaders(headerToMap(convHeaders)).
		SetBodyJsonMarshal(convReq).
		Post(openAIChatGPTConversationURL)
	if err != nil {
		return nil, fmt.Errorf("openai legacy image conversation request failed: %w", err)
	}
	// streamHandedOff 标记 SSE body 是否已交给后台 drain goroutine 接管关闭。
	// 早退优化场景下，body 由 drain goroutine 关闭；其他路径走 deferred Close。
	streamHandedOff := false
	defer func() {
		if !streamHandedOff && resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode >= 400 {
		return nil, s.wrapLegacyOpenAIImageBackendError(ctx, c, account, handleLegacyOpenAIImageBackendError(resp))
	}

	conversationID, pointerInfos, usage, firstTokenMs, earlyExit, err := readLegacyOpenAIImageConversationStream(resp, startTime, parsed.N)
	if err != nil {
		return nil, err
	}
	if earlyExit {
		streamHandedOff = true
	}
	pointerInfos = mergeLegacyOpenAIImagePointerInfos(pointerInfos, nil)
	logger.LegacyPrintf(
		"service.openai_gateway",
		"[OpenAI] Legacy image stream conversation_id=%s total_assets=%d file_service_assets=%d expected=%d elapsed_ms=%d",
		conversationID, len(pointerInfos), countLegacyOpenAIFileServicePointerInfos(pointerInfos),
		parsed.N, time.Since(startTime).Milliseconds(),
	)

	lifecycleCtx, releaseLifecycleCtx := detachOpenAIImageLifecycleContext(ctx, openAIImageLifecycleTimeout)
	defer releaseLifecycleCtx()

	// 若 SSE 未返回可下载 pointer，兜底轮询 conversation 接口。
	if conversationID != "" && !hasLegacyOpenAIDownloadablePointerInfos(pointerInfos) {
		polledPointers, pollErr := pollLegacyOpenAIImageConversation(lifecycleCtx, client, headers, conversationID)
		if pollErr != nil {
			return nil, s.wrapLegacyOpenAIImageBackendError(ctx, c, account, pollErr)
		}
		pointerInfos = mergeLegacyOpenAIImagePointerInfos(pointerInfos, polledPointers)
	}
	pointerInfos = preferLegacyOpenAIFileServicePointerInfos(pointerInfos)
	if len(pointerInfos) == 0 {
		logger.LegacyPrintf("service.openai_gateway",
			"[OpenAI] Legacy image no assets returned conversation_id=%s", conversationID)
		return nil, fmt.Errorf("openai legacy image conversation returned no downloadable images")
	}

	responseBody, imageCount, err := buildLegacyOpenAIImageResponse(lifecycleCtx, client, headers, conversationID, pointerInfos)
	if err != nil {
		return nil, s.wrapLegacyOpenAIImageBackendError(ctx, c, account, err)
	}

	c.Data(http.StatusOK, "application/json; charset=utf-8", responseBody)
	return &OpenAIForwardResult{
		RequestID:     resp.Header.Get("x-request-id"),
		Usage:         usage,
		Model:         requestModel,
		UpstreamModel: requestModel,
		Stream:        false,
		Duration:      time.Since(startTime),
		FirstTokenMs:  firstTokenMs,
		ImageCount:    imageCount,
		ImageSize:     parsed.SizeTier,
	}, nil
}

// ─── 错误处理 ─────────────────────────────────────────────────────────────────

type legacyOpenAIImageStatusError struct {
	StatusCode      int
	Message         string
	ResponseBody    []byte
	ResponseHeaders http.Header
	RequestID       string
	URL             string
}

func (e *legacyOpenAIImageStatusError) Error() string {
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return fmt.Sprintf("openai legacy image backend request failed: status %d", e.StatusCode)
}

func newLegacyOpenAIImageStatusError(resp *req.Response, fallback string) error {
	if resp == nil {
		if strings.TrimSpace(fallback) == "" {
			fallback = "openai legacy image backend request failed"
		}
		return fmt.Errorf("%s", fallback)
	}
	statusCode := resp.StatusCode
	var headers http.Header
	requestID := ""
	requestURL := ""
	var body []byte
	if resp.Response != nil {
		headers = resp.Header.Clone()
		requestID = strings.TrimSpace(resp.Header.Get("x-request-id"))
		if resp.Request != nil && resp.Request.URL != nil {
			requestURL = resp.Request.URL.String()
		}
		if resp.Body != nil {
			body, _ = io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
		}
	}
	message := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
	if message == "" {
		prefix := strings.TrimSpace(fallback)
		if prefix == "" {
			prefix = "openai legacy image backend request failed"
		}
		message = fmt.Sprintf("%s: status %d", prefix, statusCode)
	}
	return &legacyOpenAIImageStatusError{
		StatusCode:      statusCode,
		Message:         message,
		ResponseBody:    body,
		ResponseHeaders: headers,
		RequestID:       requestID,
		URL:             requestURL,
	}
}

func newLegacyOpenAIImageSyntheticStatusError(statusCode int, message string, requestURL string) *legacyOpenAIImageStatusError {
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = "openai legacy image backend request failed"
	}
	var body []byte
	if payload, err := json.Marshal(map[string]string{"detail": message}); err == nil {
		body = payload
	}
	return &legacyOpenAIImageStatusError{
		StatusCode:   statusCode,
		Message:      message,
		ResponseBody: body,
		URL:          strings.TrimSpace(requestURL),
	}
}

func isLegacyOpenAIImageTransientConversationNotFoundError(err error) bool {
	statusErr := &legacyOpenAIImageStatusError{}
	if !errors.As(err, &statusErr) || statusErr == nil || statusErr.StatusCode != http.StatusNotFound {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(statusErr.Message))
	if strings.Contains(msg, "conversation_not_found") || (strings.Contains(msg, "conversation") && strings.Contains(msg, "not found")) {
		return true
	}
	bodyMsg := strings.ToLower(strings.TrimSpace(extractUpstreamErrorMessage(statusErr.ResponseBody)))
	return strings.Contains(bodyMsg, "conversation_not_found") ||
		(strings.Contains(bodyMsg, "conversation") && strings.Contains(bodyMsg, "not found"))
}

func handleLegacyOpenAIImageBackendError(resp *req.Response) error {
	return newLegacyOpenAIImageStatusError(resp, "openai legacy image backend error")
}

func (s *OpenAIGatewayService) wrapLegacyOpenAIImageBackendError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	err error,
) error {
	var statusErr *legacyOpenAIImageStatusError
	if !errors.As(err, &statusErr) || statusErr == nil {
		return err
	}

	upstreamMsg := sanitizeUpstreamErrorMessage(statusErr.Message)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: statusErr.StatusCode,
		UpstreamRequestID:  statusErr.RequestID,
		UpstreamURL:        safeUpstreamURL(statusErr.URL),
		Kind:               "request_error",
		Message:            upstreamMsg,
	})
	setOpsUpstreamError(c, statusErr.StatusCode, upstreamMsg, "")

	if s.shouldFailoverOpenAIUpstreamResponse(statusErr.StatusCode, upstreamMsg, statusErr.ResponseBody) {
		// 与普通 text/codex 路径不同：image 失败仅写 scope=legacy_images 维度的限流，
		// 不调用 HandleUpstreamError，避免污染该账号的 text/codex 调度。
		if isLegacyOpenAIImageRateLimitStatus(statusErr) {
			s.recordLegacyImagesUpstreamRateLimit(ctx, account)
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: statusErr.StatusCode,
			UpstreamRequestID:  statusErr.RequestID,
			UpstreamURL:        safeUpstreamURL(statusErr.URL),
			Kind:               "failover",
			Message:            upstreamMsg,
		})
		retryableOnSameAccount := account.IsPoolMode() && isPoolModeRetryableStatus(statusErr.StatusCode)
		if strings.Contains(strings.ToLower(statusErr.Message), "unsupported challenge") ||
			strings.Contains(strings.ToLower(statusErr.Message), "proof token solve failed") {
			retryableOnSameAccount = false
		}
		return &UpstreamFailoverError{
			StatusCode:             statusErr.StatusCode,
			ResponseBody:           statusErr.ResponseBody,
			RetryableOnSameAccount: retryableOnSameAccount,
		}
	}
	return statusErr
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

func resolveLegacyOpenAIProxyURL(account *Account) string {
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func newLegacyOpenAIBackendAPIClient(proxyURL string) (*req.Client, error) {
	client := req.C().
		SetTimeout(180 * time.Second).
		ImpersonateChrome()
	trimmed, _, err := proxyurl.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if trimmed != "" {
		client.SetProxyURL(trimmed)
	}
	return client, nil
}

// ─── Headers（完整浏览器指纹）─────────────────────────────────────────────────

func (s *OpenAIGatewayService) buildLegacyOpenAIBackendAPIHeaders(account *Account, token string) (http.Header, error) {
	deviceID, sessionID := s.ensureOpenAIImageSessionCredentials(context.Background(), account)

	ua := openAIImageBackendUserAgent
	if customUA := strings.TrimSpace(account.GetOpenAIUserAgent()); customUA != "" {
		ua = customUA
	}

	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("Origin", "https://chatgpt.com")
	headers.Set("Referer", "https://chatgpt.com/")
	headers.Set("User-Agent", ua)
	headers.Set("Accept-Language", "en-US,en;q=0.9")
	headers.Set("Accept-Encoding", "identity")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")

	// Chromium 131 sec-ch-ua headers。
	headers.Set("sec-ch-ua", `"Chromium";v="131", "Not_A Brand";v="24", "Google Chrome";v="131"`)
	headers.Set("sec-ch-ua-arch", `"arm"`)
	headers.Set("sec-ch-ua-bitness", `"64"`)
	headers.Set("sec-ch-ua-full-version", `"131.0.6778.86"`)
	headers.Set("sec-ch-ua-full-version-list", `"Chromium";v="131.0.6778.86", "Not_A Brand";v="24.0.0.0", "Google Chrome";v="131.0.6778.86"`)
	headers.Set("sec-ch-ua-mobile", "?0")
	headers.Set("sec-ch-ua-model", `""`)
	headers.Set("sec-ch-ua-platform", `"macOS"`)
	headers.Set("sec-ch-ua-platform-version", `"15.0.0"`)
	headers.Set("Sec-Fetch-Dest", "empty")
	headers.Set("Sec-Fetch-Mode", "cors")
	headers.Set("Sec-Fetch-Site", "same-origin")

	// OpenAI 客户端标识。
	headers.Set("OAI-Client-Version", "prod-be885abb369f1c0a0e0f91de5adcb1e59eb1fed0")
	headers.Set("OAI-Client-Build-Number", "5955942")
	headers.Set("OAI-Language", "en-US")

	if chatgptAccountID := strings.TrimSpace(account.GetChatGPTAccountID()); chatgptAccountID != "" {
		headers.Set("chatgpt-account-id", chatgptAccountID)
	}
	if deviceID != "" {
		headers.Set("oai-device-id", deviceID)
		headers.Set("Cookie", "oai-did="+deviceID)
	}
	if sessionID != "" {
		headers.Set("oai-session-id", sessionID)
	}
	return headers, nil
}

func (s *OpenAIGatewayService) ensureOpenAIImageSessionCredentials(ctx context.Context, account *Account) (string, string) {
	if account == nil {
		return "", ""
	}
	deviceID := account.GetOpenAIDeviceID()
	sessionID := account.GetOpenAISessionID()
	if deviceID != "" && sessionID != "" {
		return deviceID, sessionID
	}

	updates := map[string]any{}
	if deviceID == "" {
		deviceID = uuid.NewString()
		updates["openai_device_id"] = deviceID
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
		updates["openai_session_id"] = sessionID
	}
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	for key, value := range updates {
		account.Extra[key] = value
	}
	if len(updates) == 0 || s == nil || s.accountRepo == nil {
		return deviceID, sessionID
	}

	updateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(updateCtx, account.ID, updates); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "persist openai image session creds failed: account=%d err=%v", account.ID, err)
	}
	return deviceID, sessionID
}

// ─── Bootstrap + sentinel 资源解析 ───────────────────────────────────────────

var (
	legacyScriptSrcRe = regexp.MustCompile(`<script[^>]+src="(https?://[^"]+)"`)
	legacyDataBuildRe = regexp.MustCompile(`data-build="([^"]+)"`)
	legacySdkPathRe   = regexp.MustCompile(`/c/[^/]*/_`)
)

// legacyBootstrapTTL 控制 sentinel SDK / data-build 的内存缓存有效期。
// 这些资源在分钟级稳定，缓存可避免每次请求都 GET chatgpt.com 首页。
const legacyBootstrapTTL = 5 * time.Minute

type legacyBootstrapCacheEntry struct {
	scripts   []string
	dataBuild string
	expiry    time.Time
}

var (
	legacyBootstrapCacheMu sync.RWMutex
	legacyBootstrapCache   *legacyBootstrapCacheEntry
)

func loadLegacyBootstrapCache() ([]string, string, bool) {
	legacyBootstrapCacheMu.RLock()
	defer legacyBootstrapCacheMu.RUnlock()
	if legacyBootstrapCache == nil {
		return nil, "", false
	}
	if time.Now().After(legacyBootstrapCache.expiry) {
		return nil, "", false
	}
	scripts := make([]string, len(legacyBootstrapCache.scripts))
	copy(scripts, legacyBootstrapCache.scripts)
	return scripts, legacyBootstrapCache.dataBuild, true
}

func storeLegacyBootstrapCache(scripts []string, dataBuild string) {
	legacyBootstrapCacheMu.Lock()
	defer legacyBootstrapCacheMu.Unlock()
	cached := make([]string, len(scripts))
	copy(cached, scripts)
	legacyBootstrapCache = &legacyBootstrapCacheEntry{
		scripts:   cached,
		dataBuild: dataBuild,
		expiry:    time.Now().Add(legacyBootstrapTTL),
	}
}

// bootstrapLegacyOpenAIBackendAPI 预热 chatgpt.com 并解析 sentinel SDK 资源。
// 返回 (scriptSources, dataBuild)；失败时返回安全默认值，不影响主流程。
// 解析结果会缓存 legacyBootstrapTTL 时间，避免重复抓取首页。
func bootstrapLegacyOpenAIBackendAPI(ctx context.Context, client *req.Client, headers http.Header) ([]string, string) {
	if scripts, dataBuild, ok := loadLegacyBootstrapCache(); ok {
		return scripts, dataBuild
	}
	resp, err := client.R().
		SetContext(ctx).
		SetHeaders(headerToMap(headers)).
		DisableAutoReadResponse().
		Get(openAIChatGPTStartURL)
	if err != nil {
		return []string{openAIDefaultSentinelSDKURL}, ""
	}
	if resp == nil || resp.Body == nil {
		return []string{openAIDefaultSentinelSDKURL}, ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	html := string(body)
	var scripts []string
	for _, m := range legacyScriptSrcRe.FindAllStringSubmatch(html, -1) {
		src := m[1]
		if strings.Contains(src, "chatgpt.com") {
			scripts = append(scripts, src)
		}
	}
	if len(scripts) == 0 {
		scripts = []string{openAIDefaultSentinelSDKURL}
	}

	dataBuild := ""
	if m := legacyDataBuildRe.FindStringSubmatch(html); len(m) > 1 {
		dataBuild = m[1]
	}
	if dataBuild == "" {
		for _, s := range scripts {
			if m := legacySdkPathRe.FindString(s); m != "" {
				dataBuild = m
				break
			}
		}
	}
	storeLegacyBootstrapCache(scripts, dataBuild)
	return scripts, dataBuild
}

// ─── Sentinel 握手 ────────────────────────────────────────────────────────────

func initializeLegacyOpenAIImageConversation(ctx context.Context, client *req.Client, headers http.Header) error {
	payload := map[string]any{
		"gizmo_id":                nil,
		"requested_default_model": nil,
		"conversation_id":         nil,
		"timezone_offset_min":     openAITimezoneOffsetMinutes(),
		"system_hints":            []string{"picture_v2"},
	}
	resp, err := client.R().
		SetContext(ctx).
		SetHeaders(headerToMap(headers)).
		SetBodyJsonMarshal(payload).
		Post(openAIChatGPTConversationInitURL)
	if err != nil {
		return err
	}
	if !resp.IsSuccessState() {
		return newLegacyOpenAIImageStatusError(resp, "conversation init failed")
	}
	return nil
}

type legacyOpenAIChatRequirements struct {
	Token     string `json:"token"`
	Turnstile struct {
		Required bool `json:"required"`
	} `json:"turnstile"`
	Arkose struct {
		Required bool `json:"required"`
	} `json:"arkose"`
	ProofOfWork struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
}

// fetchLegacyOpenAIChatRequirements 获取 sentinel chat-requirements token 及 PoW 参数。
// 先用 requirements token 尝试，失败时用空 p 重试一次。
func fetchLegacyOpenAIChatRequirements(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	scriptSources []string,
	dataBuild string,
) (*legacyOpenAIChatRequirements, error) {
	ua := headers.Get("User-Agent")
	reqToken := buildLegacyRequirementsToken(ua, scriptSources, dataBuild)

	payloads := []map[string]any{
		{"p": reqToken},
		{"p": nil},
	}
	var lastErr error
	for _, payload := range payloads {
		var result legacyOpenAIChatRequirements
		resp, err := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(headers)).
			SetBodyJsonMarshal(payload).
			SetSuccessResult(&result).
			Post(openAIChatGPTChatRequirementsURL)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.IsSuccessState() {
			return &result, nil
		}
		lastErr = newLegacyOpenAIImageStatusError(resp, "chat requirements request failed")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("chat requirements request failed")
	}
	return nil, lastErr
}

// ─── 会话准备 ─────────────────────────────────────────────────────────────────

func prepareLegacyOpenAIImageConversation(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	prompt string,
	parentMessageID string,
	chatRequirementsToken string,
	proofToken string,
) (string, error) {
	prepareHeaders := cloneHTTPHeader(headers)
	prepareHeaders.Set("openai-sentinel-chat-requirements-token", chatRequirementsToken)
	if proofToken != "" {
		prepareHeaders.Set("openai-sentinel-proof-token", proofToken)
	}

	payload := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     uuid.NewString(),
		"model":                 legacyOpenAIImageModelSlug("gpt-image-2"),
		"client_prepare_state":  "success",
		"timezone_offset_min":   openAITimezoneOffsetMinutes(),
		"timezone":              openAITimezoneName(),
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []string{"picture_v2"},
		"partial_query": map[string]any{
			"id":     parentMessageID,
			"author": map[string]any{"role": "user"},
			"content": map[string]any{
				"content_type": "text",
				"parts":        []string{prompt},
			},
		},
		"supports_buffering":  true,
		"supported_encodings": []string{"v1"},
		"client_contextual_info": map[string]any{
			"app_name": "chatgpt.com",
		},
	}

	var result struct {
		ConduitToken string `json:"conduit_token"`
	}
	resp, err := client.R().
		SetContext(ctx).
		SetHeaders(headerToMap(prepareHeaders)).
		SetBodyJsonMarshal(payload).
		SetSuccessResult(&result).
		Post(openAIChatGPTConversationPrepareURL)
	if err != nil {
		return "", fmt.Errorf("prepare conversation failed: %w", err)
	}
	if !resp.IsSuccessState() {
		return "", newLegacyOpenAIImageStatusError(resp, "prepare conversation failed")
	}
	return strings.TrimSpace(result.ConduitToken), nil
}

// ─── 图片上传（三步协议）─────────────────────────────────────────────────────

type legacyOpenAIUploadedImage struct {
	FileID      string
	FileName    string
	ContentType string
	Width       int
	Height      int
}

// uploadLegacyOpenAIImageFiles 执行真实的三步上传：
//  1. POST /backend-api/files → 获取 file_id 和 Azure upload_url
//  2. PUT upload_url (Azure Blob Storage)
//  3. POST /backend-api/files/{file_id}/uploaded → 确认完成
func uploadLegacyOpenAIImageFiles(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	uploads []OpenAIImagesUpload,
) ([]legacyOpenAIUploadedImage, error) {
	if len(uploads) == 0 {
		return nil, nil
	}
	results := make([]legacyOpenAIUploadedImage, 0, len(uploads))
	for _, upload := range uploads {
		if len(upload.Data) == 0 {
			continue
		}

		// Step 1: 创建上传槽，获取 file_id 和 Azure upload_url。
		var created struct {
			FileID    string `json:"file_id"`
			UploadURL string `json:"upload_url"`
		}
		resp1, err := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(headers)).
			SetBodyJsonMarshal(map[string]any{
				"file_name":    coalesceOpenAIFileName(upload.FileName, "image.png"),
				"file_size":    len(upload.Data),
				"use_case":     "multimodal",
				"content_type": upload.ContentType,
				"width":        upload.Width,
				"height":       upload.Height,
			}).
			SetSuccessResult(&created).
			Post(openAIChatGPTFilesURL)
		if err != nil {
			return nil, fmt.Errorf("create upload slot failed: %w", err)
		}
		if !resp1.IsSuccessState() || strings.TrimSpace(created.FileID) == "" {
			return nil, newLegacyOpenAIImageStatusError(resp1, "create upload slot failed")
		}

		// Step 2: 直传 Azure Blob Storage（不带 ChatGPT 认证头）。
		if strings.TrimSpace(created.UploadURL) != "" {
			resp2, err2 := client.R().
				SetContext(ctx).
				SetHeader("x-ms-blob-type", "BlockBlob").
				SetHeader("x-ms-version", "2020-04-08").
				SetHeader("Content-Type", upload.ContentType).
				SetBodyBytes(upload.Data).
				Put(created.UploadURL)
			if err2 != nil {
				return nil, fmt.Errorf("azure blob upload failed: %w", err2)
			}
			if !resp2.IsSuccessState() {
				return nil, newLegacyOpenAIImageStatusError(resp2, "azure blob upload failed")
			}
		}

		// Step 3: 通知 ChatGPT 上传完成。
		resp3, err3 := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(headers)).
			SetBodyJsonMarshal(map[string]any{}).
			Post(fmt.Sprintf("%s/%s/uploaded", openAIChatGPTFilesURL, created.FileID))
		if err3 != nil {
			return nil, fmt.Errorf("confirm upload failed: %w", err3)
		}
		if !resp3.IsSuccessState() {
			return nil, newLegacyOpenAIImageStatusError(resp3, "confirm upload failed")
		}

		results = append(results, legacyOpenAIUploadedImage{
			FileID:      created.FileID,
			FileName:    coalesceOpenAIFileName(upload.FileName, "image.png"),
			ContentType: upload.ContentType,
			Width:       upload.Width,
			Height:      upload.Height,
		})
	}
	return results, nil
}

func coalesceOpenAIFileName(value string, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

// ─── Conversation 请求构建 ─────────────────────────────────────────────────────

// legacyOpenAIImageModelSlug 将 OpenAI 模型名映射到 ChatGPT Web 内部 slug。
func legacyOpenAIImageModelSlug(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(m, "gpt-image-2") || strings.Contains(m, "gpt-5-3") {
		return "gpt-5-3"
	}
	return "auto"
}

func buildLegacyOpenAIImageConversationRequest(
	parsed *OpenAIImagesRequest,
	parentMessageID string,
	uploads []legacyOpenAIUploadedImage,
) map[string]any {
	parts := []any{}
	attachments := []map[string]any{}
	for _, upload := range uploads {
		parts = append(parts, map[string]any{
			"content_type":  "image_asset_pointer",
			"asset_pointer": "file-service://" + upload.FileID,
			"size_bytes":    0,
			"width":         upload.Width,
			"height":        upload.Height,
		})
		attachments = append(attachments, map[string]any{
			"id":       upload.FileID,
			"mimeType": upload.ContentType,
			"name":     upload.FileName,
			"size":     0,
			"width":    upload.Width,
			"height":   upload.Height,
		})
	}
	parts = append(parts, parsed.Prompt)

	contentType := "text"
	if len(uploads) > 0 {
		contentType = "multimodal_text"
	}

	metadata := map[string]any{
		"developer_mode_connector_ids": []any{},
		"selected_github_repos":        []any{},
		"selected_all_github_repos":    false,
		"system_hints":                 []string{"picture_v2"},
		"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
	}
	if len(uploads) > 0 {
		metadata["attachments"] = attachments
	}

	messages := []map[string]any{
		{
			"id":          parentMessageID,
			"author":      map[string]any{"role": "user"},
			"create_time": float64(time.Now().UnixNano()) / 1e9,
			"content": map[string]any{
				"content_type": contentType,
				"parts":        parts,
			},
			"metadata": metadata,
		},
	}

	return map[string]any{
		"action":                              "next",
		"messages":                            messages,
		"parent_message_id":                   uuid.NewString(),
		"model":                               legacyOpenAIImageModelSlug(parsed.Model),
		"client_prepare_state":                "sent",
		"timezone_offset_min":                 openAITimezoneOffsetMinutes(),
		"timezone":                            openAITimezoneName(),
		"conversation_mode":                   map[string]any{"kind": "primary_assistant"},
		"enable_message_followups":            true,
		"system_hints":                        []string{"picture_v2"},
		"supports_buffering":                  true,
		"supported_encodings":                 []string{"v1"},
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":               "auto",
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 1200,
			"page_height":       1072,
			"page_width":        1724,
			"pixel_ratio":       1.2,
			"screen_height":     1440,
			"screen_width":      2560,
			"app_name":          "chatgpt.com",
		},
	}
}

// ─── SSE 流解析 ──────────────────────────────────────────────────────────────

type legacyOpenAIImagePointerInfo struct {
	Pointer string
}

var legacyImagePointerRe = regexp.MustCompile(`(?:file-service|sediment)://[^\\"\\s\]]+`)

func readLegacyOpenAIImageConversationStream(
	resp *req.Response,
	startTime time.Time,
	expectedImages int,
) (string, []legacyOpenAIImagePointerInfo, OpenAIUsage, *int, bool, error) {
	var conversationID string
	var pointerInfos []legacyOpenAIImagePointerInfo
	var usage OpenAIUsage
	var firstTokenMs *int

	if expectedImages < 1 {
		expectedImages = 1
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if firstTokenMs == nil {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			text := strings.TrimRight(string(line), "\r\n")
			if data, ok := extractOpenAISSEDataLine(text); ok && data != "" && data != "[DONE]" {
				dataBytes := []byte(data)
				mergeOpenAIUsage(&usage, dataBytes)

				// 用 gjson 提取 conversation_id（比手写 bytes.Index 更健壮）。
				if id := gjson.GetBytes(dataBytes, "conversation_id").String(); id != "" {
					conversationID = id
				}
				pointerInfos = append(pointerInfos, collectLegacyOpenAIImagePointers(dataBytes)...)

				// 早退优化：拿到足够数量的可下载 pointer（file-service:// 或 sediment://）
				// 即立即返回，不再等待 SSE 全量结束（可省 50-200s）。
				// 兼容多图请求：必须收齐 expectedImages 张才退；usage 字段对 image
				// 计费无关紧要（按张数计价），故可放弃。
				if conversationID != "" &&
					countLegacyOpenAIDownloadablePointerInfos(pointerInfos) >= expectedImages {
					// 后台 drain 剩余流并关闭连接，避免上游误判客户端早断 → 触发反作弊。
					go drainLegacyOpenAIImageConversationStream(resp.Body)
					return conversationID, pointerInfos, usage, firstTokenMs, true, nil
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return conversationID, pointerInfos, usage, firstTokenMs, false, err
		}
	}
	return conversationID, pointerInfos, usage, firstTokenMs, false, nil
}

// drainLegacyOpenAIImageConversationStream 在早退后异步消费剩余 SSE 数据并关闭连接，
// 避免上游将"客户端不读完流"识别为异常断连而触发反作弊策略。
func drainLegacyOpenAIImageConversationStream(body io.ReadCloser) {
	if body == nil {
		return
	}
	defer body.Close()
	// 限制 drain 时长与字节数，防止异常长尾占用 goroutine。
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 8<<20))
}

func collectLegacyOpenAIImagePointers(body []byte) []legacyOpenAIImagePointerInfo {
	var results []legacyOpenAIImagePointerInfo
	for _, match := range legacyImagePointerRe.FindAll(body, -1) {
		results = append(results, legacyOpenAIImagePointerInfo{Pointer: string(match)})
	}
	return results
}

func mergeLegacyOpenAIImagePointerInfos(existing, next []legacyOpenAIImagePointerInfo) []legacyOpenAIImagePointerInfo {
	seen := map[string]struct{}{}
	var result []legacyOpenAIImagePointerInfo
	for _, item := range append(existing, next...) {
		key := strings.TrimSpace(item.Pointer)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	return result
}

func hasLegacyOpenAIFileServicePointerInfos(items []legacyOpenAIImagePointerInfo) bool {
	for _, item := range items {
		if strings.HasPrefix(item.Pointer, "file-service://") {
			return true
		}
	}
	return false
}

// hasLegacyOpenAIDownloadablePointerInfos 判定是否存在可下载的 pointer。
// file-service:// 与 sediment:// 都对应 ChatGPT backend-api/.../attachment/{pointer}/download
// 接口，实测均可成功取图。Web 网页常用 sediment://，对齐之即可获得 ~15s 体感。
func hasLegacyOpenAIDownloadablePointerInfos(items []legacyOpenAIImagePointerInfo) bool {
	for _, item := range items {
		if strings.HasPrefix(item.Pointer, "file-service://") ||
			strings.HasPrefix(item.Pointer, "sediment://") {
			return true
		}
	}
	return false
}

func countLegacyOpenAIFileServicePointerInfos(items []legacyOpenAIImagePointerInfo) int {
	n := 0
	for _, item := range items {
		if strings.HasPrefix(item.Pointer, "file-service://") {
			n++
		}
	}
	return n
}

// countLegacyOpenAIDownloadablePointerInfos 统计可下载的 pointer 数量
// （file-service:// + sediment://），用于判定是否已收齐 N 张图。
func countLegacyOpenAIDownloadablePointerInfos(items []legacyOpenAIImagePointerInfo) int {
	n := 0
	for _, item := range items {
		if strings.HasPrefix(item.Pointer, "file-service://") ||
			strings.HasPrefix(item.Pointer, "sediment://") {
			n++
		}
	}
	return n
}

// summarizeLegacyOpenAIPointerKind 用于日志，展示 pointer 类型分布。
func summarizeLegacyOpenAIPointerKind(items []legacyOpenAIImagePointerInfo) string {
	var fs, sd, other int
	for _, item := range items {
		switch {
		case strings.HasPrefix(item.Pointer, "file-service://"):
			fs++
		case strings.HasPrefix(item.Pointer, "sediment://"):
			sd++
		default:
			other++
		}
	}
	return fmt.Sprintf("file_service=%d sediment=%d other=%d", fs, sd, other)
}

func preferLegacyOpenAIFileServicePointerInfos(items []legacyOpenAIImagePointerInfo) []legacyOpenAIImagePointerInfo {
	var fileService []legacyOpenAIImagePointerInfo
	for _, item := range items {
		if strings.HasPrefix(item.Pointer, "file-service://") || strings.HasPrefix(item.Pointer, "sediment://") {
			fileService = append(fileService, item)
		}
	}
	if len(fileService) > 0 {
		return fileService
	}
	return items
}

// ─── 兜底轮询 ─────────────────────────────────────────────────────────────────

func pollLegacyOpenAIImageConversation(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	conversationID string,
) ([]legacyOpenAIImagePointerInfo, error) {
	pollURL := fmt.Sprintf("https://chatgpt.com/backend-api/conversation/%s", conversationID)
	deadline := time.Now().Add(4 * time.Minute)
	var lastPointers []legacyOpenAIImagePointerInfo
	startedAt := time.Now()
	iter := 0

	for time.Now().Before(deadline) {
		iter++
		var body json.RawMessage
		resp, err := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(headers)).
			SetSuccessResult(&body).
			Get(pollURL)
		if err != nil {
			return nil, fmt.Errorf("poll conversation failed: %w", err)
		}
		if !resp.IsSuccessState() {
			return nil, newLegacyOpenAIImageStatusError(resp, "poll conversation failed")
		}
		pointers := collectLegacyOpenAIImagePointers(body)
		if len(pointers) > 0 {
			lastPointers = pointers
			// 实测 sediment:// 与 file-service:// 都可下载，提前命中即返回，
			// 不再等待"升级"为 file-service:// → 与 web 网页 ~15s 出图体感对齐。
			if hasLegacyOpenAIDownloadablePointerInfos(pointers) {
				logger.LegacyPrintf("service.openai_gateway",
					"[OpenAI] Legacy image polling success conversation_id=%s iter=%d elapsed_ms=%d kind=%s",
					conversationID, iter, time.Since(startedAt).Milliseconds(),
					summarizeLegacyOpenAIPointerKind(pointers))
				return pointers, nil
			}
		}

		// 指数退避：前 6 次每秒一查（贴近 web 体感），随后 2s，再之后 4s。
		// 与原先固定 4s 相比，n=1 命中典型场景可省 ~12s。
		var backoff time.Duration
		switch {
		case iter <= 6:
			backoff = time.Second
		case iter <= 15:
			backoff = 2 * time.Second
		default:
			backoff = 4 * time.Second
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return lastPointers, nil
		case <-timer.C:
		}
	}
	return lastPointers, nil
}

// ─── 图片下载 + 响应构建 ──────────────────────────────────────────────────────

func buildLegacyOpenAIImageResponse(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	conversationID string,
	pointerInfos []legacyOpenAIImagePointerInfo,
) ([]byte, int, error) {
	type imageData struct {
		B64JSON string `json:"b64_json"`
	}
	var images []imageData
	for _, info := range pointerInfos {
		downloadURL, err := fetchLegacyOpenAIImageDownloadURL(ctx, client, headers, conversationID, info.Pointer)
		if err != nil {
			logger.LegacyPrintf("service.openai_gateway",
				"[OpenAI] Legacy image download URL fetch failed pointer=%s err=%v", info.Pointer, err)
			continue
		}
		imageBytes, err := downloadLegacyOpenAIImageBytes(ctx, client, headers, downloadURL)
		if err != nil {
			logger.LegacyPrintf("service.openai_gateway",
				"[OpenAI] Legacy image download failed url=%s err=%v", downloadURL, err)
			continue
		}
		images = append(images, imageData{B64JSON: base64.StdEncoding.EncodeToString(imageBytes)})
	}
	if len(images) == 0 {
		return nil, 0, fmt.Errorf("no images downloaded")
	}
	result := map[string]any{
		"created": time.Now().Unix(),
		"data":    images,
	}
	body, err := json.Marshal(result)
	if err != nil {
		return nil, 0, err
	}
	return body, len(images), nil
}

func fetchLegacyOpenAIImageDownloadURL(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	conversationID string,
	pointer string,
) (string, error) {
	var url string
	var allowConversationRetry bool
	switch {
	case strings.HasPrefix(pointer, "file-service://"):
		fileID := strings.TrimPrefix(pointer, "file-service://")
		url = fmt.Sprintf("%s/%s/download", openAIChatGPTFilesURL, fileID)
	case strings.HasPrefix(pointer, "sediment://"):
		attachmentID := strings.TrimPrefix(pointer, "sediment://")
		url = fmt.Sprintf("https://chatgpt.com/backend-api/conversation/%s/attachment/%s/download",
			conversationID, attachmentID)
		allowConversationRetry = true
	default:
		return "", fmt.Errorf("unsupported image pointer: %s", pointer)
	}

	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		var result struct {
			DownloadURL string `json:"download_url"`
		}
		resp, err := client.R().
			SetContext(ctx).
			SetHeaders(headerToMap(headers)).
			SetSuccessResult(&result).
			Get(url)
		if err != nil {
			lastErr = err
		} else if resp.IsSuccessState() && strings.TrimSpace(result.DownloadURL) != "" {
			return strings.TrimSpace(result.DownloadURL), nil
		} else {
			statusErr := newLegacyOpenAIImageStatusError(resp, "fetch image download url failed")
			if !allowConversationRetry || !isLegacyOpenAIImageTransientConversationNotFoundError(statusErr) {
				return "", statusErr
			}
			lastErr = statusErr
		}
		if attempt == 7 {
			break
		}
		timer := time.NewTimer(750 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("fetch image download url failed")
	}
	return "", lastErr
}

func downloadLegacyOpenAIImageBytes(
	ctx context.Context,
	client *req.Client,
	headers http.Header,
	downloadURL string,
) ([]byte, error) {
	request := client.R().SetContext(ctx).DisableAutoReadResponse()
	if strings.HasPrefix(downloadURL, openAIChatGPTStartURL) {
		request = request.SetHeaders(headerToMap(headers))
	}
	resp, err := request.Get(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("download image failed: %w", err)
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if !resp.IsSuccessState() {
		return nil, newLegacyOpenAIImageStatusError(resp, "download image failed")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, openAIImageMaxDownloadBytes))
	if err != nil {
		return nil, fmt.Errorf("read image bytes failed: %w", err)
	}
	return data, nil
}

// ─── 时区辅助 ─────────────────────────────────────────────────────────────────

func openAITimezoneOffsetMinutes() int {
	_, offset := time.Now().Zone()
	return offset / 60
}

func openAITimezoneName() string {
	return time.Now().Location().String()
}

// ─── context 辅助 ─────────────────────────────────────────────────────────────

func detachOpenAIImageLifecycleContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	if timeout <= 0 {
		return base, func() {}
	}
	return context.WithTimeout(base, timeout)
}

// ─── PoW / Sentinel token 生成（移植自 chatgpt2api utils/pow.py）───────────────

// buildLegacyPowConfig 构建 PoW 计算配置（18 字段）。
// 关键字段（navigator_key、window_key、cores）从候选列表随机选取，模拟真实浏览器指纹。
func buildLegacyPowConfig(ua, scriptSource, dataBuild string) []any {
	if strings.TrimSpace(scriptSource) == "" {
		scriptSource = openAIDefaultSentinelSDKURL
	}
	now := time.Now()
	uptime := float64(time.Since(legacyPowProcessStart).Milliseconds())
	return []any{
		[]int{3000, 4000, 5000}[rand.Intn(3)], // [0] screen
		legacyPowFormatTime(),                  // [1] current time in EST
		4294705152,                             // [2] hardcoded sentinel constant
		0,                                      // [3] placeholder → PoW nonce i
		ua,                                     // [4]
		scriptSource,                           // [5] sentinel SDK script URL
		dataBuild,                              // [6] data-build hash
		"en-US",                                // [7]
		"en-US,es-US,en,es",                    // [8]
		0,                                      // [9] placeholder → i >> 1
		legacyPowNavigatorKeys[rand.Intn(len(legacyPowNavigatorKeys))],   // [10]
		legacyPowDocumentKeys[rand.Intn(len(legacyPowDocumentKeys))],     // [11]
		legacyPowWindowKeys[rand.Intn(len(legacyPowWindowKeys))],         // [12]
		uptime,                                 // [13] process uptime ms
		uuid.NewString(),                       // [14]
		"",                                     // [15]
		legacyPowCores[rand.Intn(len(legacyPowCores))],                   // [16] cores
		float64(now.UnixMilli()) - uptime,      // [17] approx boot epoch ms
	}
}

// legacyPowFormatTime 以东部时间格式返回当前时间字符串（匹配 ChatGPT Web 浏览器行为）。
func legacyPowFormatTime() string {
	loc := time.FixedZone("EST", -5*60*60)
	return time.Now().In(loc).Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)"
}

// solveLegacyPow 执行 sha3-512 工作量证明。
// 与 chatgpt2api 的 _pow_generate 完全一致：两个循环变量 i 和 i>>1 被嵌入 JSON 数组。
func solveLegacyPow(seed, difficulty string, config []any, limit int) (string, bool) {
	diffBytes, err := hex.DecodeString(difficulty)
	if err != nil {
		return "", false
	}
	diffLen := len(diffBytes)

	// config[:3] → "[e0,e1,e2]" → strip "]" → "[e0,e1,e2,"
	raw13, _ := json.Marshal(config[:3])
	static1 := make([]byte, len(raw13))
	copy(static1, raw13)
	static1[len(static1)-1] = ','

	// config[4:9] → "[e4,...,e8]" → strip "[" and "]" → ",e4,...,e8,"
	raw49, _ := json.Marshal(config[4:9])
	inner49 := raw49[1 : len(raw49)-1]
	static2 := make([]byte, 0, len(inner49)+2)
	static2 = append(static2, ',')
	static2 = append(static2, inner49...)
	static2 = append(static2, ',')

	// config[10:] → "[e10,...,eN]" → strip "[" → ",e10,...,eN]"
	raw10, _ := json.Marshal(config[10:])
	static3 := make([]byte, 0, len(raw10))
	static3 = append(static3, ',')
	static3 = append(static3, raw10[1:]...)

	seedBytes := []byte(seed)

	for i := 0; i < limit; i++ {
		iStr := strconv.Itoa(i)
		iHalfStr := strconv.Itoa(i >> 1)

		// 拼装完整 JSON 数组：[e0,e1,e2,i,e4,...,e8,i>>1,e10,...,eN]
		finalJSON := make([]byte, 0, len(static1)+len(iStr)+len(static2)+len(iHalfStr)+len(static3))
		finalJSON = append(finalJSON, static1...)
		finalJSON = append(finalJSON, iStr...)
		finalJSON = append(finalJSON, static2...)
		finalJSON = append(finalJSON, iHalfStr...)
		finalJSON = append(finalJSON, static3...)

		encoded := base64.StdEncoding.EncodeToString(finalJSON)
		sum := sha3.Sum512(append(seedBytes, encoded...))
		if bytes.Compare(sum[:diffLen], diffBytes) <= 0 {
			return encoded, true
		}
	}
	return "", false
}

// buildLegacyRequirementsToken 生成发送给 /sentinel/chat-requirements 的 p 字段。
// 使用随机浮点数作 seed，难度固定为 "0fffff"（比 proof token 宽松得多）。
func buildLegacyRequirementsToken(ua string, scriptSources []string, dataBuild string) string {
	src := openAIDefaultSentinelSDKURL
	if len(scriptSources) > 0 {
		src = scriptSources[rand.Intn(len(scriptSources))]
	}
	config := buildLegacyPowConfig(ua, src, dataBuild)
	seed := strconv.FormatFloat(rand.Float64(), 'f', -1, 64)
	encoded, ok := solveLegacyPow(seed, openAIImageRequirementsDiff, config, 500000)
	if !ok {
		return ""
	}
	return "gAAAAAC" + encoded
}

// buildLegacyProofToken 生成 openai-sentinel-proof-token 头的值。
// 若解题失败（超过 500000 次迭代），返回 error 触发上层换号，而不是发送伪造 token。
func buildLegacyProofToken(
	required bool,
	seed, difficulty, ua string,
	scriptSources []string,
	dataBuild string,
) (string, error) {
	if !required || strings.TrimSpace(seed) == "" || strings.TrimSpace(difficulty) == "" {
		return "", nil
	}
	src := openAIDefaultSentinelSDKURL
	if len(scriptSources) > 0 {
		src = scriptSources[rand.Intn(len(scriptSources))]
	}
	config := buildLegacyPowConfig(ua, src, dataBuild)
	encoded, ok := solveLegacyPow(seed, difficulty, config, 500000)
	if !ok {
		return "", fmt.Errorf("pow solve failed after 500000 iterations (difficulty=%s)", difficulty)
	}
	return "gAAAAAB" + encoded, nil
}
