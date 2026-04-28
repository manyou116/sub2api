package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// OpenAIImagesV2Handler 是基于 service/openaiimages 重写的图片网关 handler。
//
// 与旧 OpenAIGatewayHandler.Images 相比：
//   - 调度池与 codex 文本通道完全隔离（ImagePool 仅读 account.extra.image_*）
//   - 单 dispatch 循环（错误分类 + 自动换号 + cooldown 回写）取代多套 retry 逻辑
//   - 5 种 ResponseWriter（Standard/ChatSync/ChatSSE/RespSync/RespSSE）由 EntryKind+Stream 自动派发
//
// 当前版本聚焦"端到端打通路径"，billing / user-slot / ops-monitor 等横切关注点
// 在 Stage 6 集成时再接入。
type OpenAIImagesV2Handler struct {
	accountRepo service.AccountRepository
	groupRepo   service.GroupRepository

	gatewayService        *service.OpenAIGatewayService
	billingCacheService   *service.BillingCacheService
	apiKeyService         *service.APIKeyService
	usageRecordWorkerPool *service.UsageRecordWorkerPool

	pool      *openaiimages.ImagePool
	probe     *openaiimages.AccountProbe
	source    *openaiimages.PoolBackedSource
	registry  openaiimages.MapDriverRegistry
	dispatchO openaiimages.DispatchOptions
	cache     *openaiimages.ImageCache
	settings  *service.SettingService
}

// NewOpenAIImagesV2Handler 装配新图片网关。
func NewOpenAIImagesV2Handler(
	accountRepo service.AccountRepository,
	groupRepo service.GroupRepository,
	gatewayService *service.OpenAIGatewayService,
	billingCacheService *service.BillingCacheService,
	apiKeyService *service.APIKeyService,
	usageRecordWorkerPool *service.UsageRecordWorkerPool,
	settingService *service.SettingService,
) *OpenAIImagesV2Handler {
	probe := openaiimages.NewAccountProbe(accountRepo)

	pool := &openaiimages.ImagePool{
		Probe: probe,
		Now:   time.Now,
		List: func(ctx context.Context, f openaiimages.PoolFilter) ([]openaiimages.PoolAccount, error) {
			return listOpenAIImageAccounts(ctx, accountRepo, f)
		},
	}

	h := &OpenAIImagesV2Handler{
		accountRepo:           accountRepo,
		groupRepo:             groupRepo,
		gatewayService:        gatewayService,
		billingCacheService:   billingCacheService,
		apiKeyService:         apiKeyService,
		usageRecordWorkerPool: usageRecordWorkerPool,
		pool:                  pool,
		probe:                 probe,
		settings:              settingService,
		registry: openaiimages.MapDriverRegistry{
			openaiimages.DriverAPIKey:    openaiimages.NewAPIKeyDriver(),
			openaiimages.DriverResponses: openaiimages.NewResponsesToolDriver(),
			openaiimages.DriverWeb:       openaiimages.NewWebDriverAdapter(),
		},
		dispatchO: openaiimages.DispatchOptions{
			MaxAttempts:              8,
			AuthCooldown:             time.Hour,
			DefaultRateLimitCooldown: 5 * time.Minute,
			Sleep:                    time.Sleep,
		},
	}

	if cache, cacheErr := openaiimages.NewImageCache(filepath.Join(settingService.PricingDataDir(), "image_cache"), 24*time.Hour); cacheErr == nil {
		h.cache = cache
	} else {
		logger.L().Warn("openaiimages.image_cache_init_failed", zap.Error(cacheErr))
	}

	h.source = openaiimages.NewPoolBackedSource(openaiimages.PoolSourceDeps{
		Pool: pool,
		LookupAccount: func(ctx context.Context, pa openaiimages.PoolAccount) (openaiimages.AccountView, error) {
			return h.lookupAccountView(ctx, pa)
		},
	})

	return h
}

// Generations 处理 POST /v1/images/generations。
func (h *OpenAIImagesV2Handler) Generations(c *gin.Context) {
	body, ok := h.readBody(c)
	if !ok {
		return
	}
	req, err := openaiimages.ParseImagesGenerations(body)
	if err != nil {
		writeOpenAIImageError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	h.run(c, req)
}

// Edits 处理 POST /v1/images/edits（multipart/form-data）。
func (h *OpenAIImagesV2Handler) Edits(c *gin.Context) {
	req, err := openaiimages.ParseImagesEdits(c.Request)
	if err != nil {
		writeOpenAIImageError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	h.run(c, req)
}

// ChatCompletions 处理面向图片场景的 POST /v1/chat/completions。
// 调用方应在路由层判断 model 是否走图片分支（通过 openaiimages.IsImageModel）。
func (h *OpenAIImagesV2Handler) ChatCompletions(c *gin.Context) {
	body, ok := h.readBody(c)
	if !ok {
		return
	}
	req, err := openaiimages.ParseFromChatCompletions(body)
	if err != nil {
		writeOpenAIImageError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	h.run(c, req)
}

// Responses 处理面向图片场景的 POST /v1/responses。
func (h *OpenAIImagesV2Handler) Responses(c *gin.Context) {
	body, ok := h.readBody(c)
	if !ok {
		return
	}
	req, err := openaiimages.ParseFromResponses(body)
	if err != nil {
		writeOpenAIImageError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	h.run(c, req)
}

// ShouldHandle 判断请求 body 中的 model 是否应由本图片网关处理。
//
// 用于 /v1/chat/completions 和 /v1/responses 路由前置：peek body 中的 "model"
// 字段，若是已知图片模型（openaiimages.IsImageModel）则返回 true，路由层应直接调
// ChatCompletions / Responses 入口；否则返回 false，请求继续走文本流水线。
//
// 调用后 c.Request.Body 会被替换成可重读的 io.NopCloser(bytes.NewReader(body))，
// 同时 body 缓存到 c.Set("openai_chat_body_cache", body)，下游 readBody 复用避免再读。
func (h *OpenAIImagesV2Handler) ShouldHandle(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return false
	}
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil || len(body) == 0 {
		// body 未消耗或失败：保留原 body（已可能被 Read 部分），简单恢复为空 reader。
		if len(body) > 0 {
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		}
		return false
	}
	// body 已被消耗，必须 restore，否则下游文本 handler 拿不到。
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Set("openai_chat_body_cache", body)

	model := openaiimages.PeekModel(body)
	return openaiimages.IsImageModel(model)
}

// run 执行 dispatch + write 的核心流程。
func (h *OpenAIImagesV2Handler) run(c *gin.Context, req *openaiimages.ImagesRequest) {
	requestStart := time.Now()
	reqLog := requestLogger(c, "handler.openai_images.v2",
		zap.String("model", req.Model),
		zap.String("entry", string(req.Entry)),
		zap.Bool("stream", req.Stream),
	)
	reqLog.Info("openaiimages.run_enter")

	defer func() {
		if r := recover(); r != nil {
			reqLog.Error("openaiimages.run_panic",
				zap.Any("panic", r),
				zap.Stack("stack"),
			)
			writeOpenAIImageError(c, http.StatusInternalServerError, "internal_error",
				fmt.Sprintf("panic: %v", r))
		}
	}()

	cap, ok := openaiimages.LookupCapability(req.Model)
	if !ok {
		writeOpenAIImageError(c, http.StatusBadRequest, "invalid_request_error",
			"unsupported image model: "+req.Model)
		return
	}

	h.applyDefaultResponseFormat(c, req)

	apiKey, _ := middleware2.GetAPIKeyFromContext(c)
	groupID := int64(0)
	if apiKey != nil && apiKey.GroupID != nil {
		groupID = *apiKey.GroupID
	}

	// 计费 / 余额前置检查（与 chat/completions 入口保持一致）。
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if h.billingCacheService != nil && apiKey != nil {
		if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription); err != nil {
			reqLog.Info("openaiimages.billing_eligibility_check_failed", zap.Error(err))
			status, code, message, retryAfter := billingErrorDetails(err)
			if retryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(retryAfter))
			}
			writeOpenAIImageError(c, status, code, message)
			return
		}
	}

	// 解析渠道级模型映射（ToUsageFields 需要）。
	var channelMapping service.ChannelMappingResult
	if h.gatewayService != nil && apiKey != nil {
		channelMapping, _ = h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, req.Model)
	}

	in := openaiimages.DispatchInput{
		Capability: cap,
		Filter:     openaiimages.PoolFilter{GroupID: groupID, Driver: cap.DriverName},
		Request:    req,
	}
	reqLog.Info("openaiimages.before_dispatch",
		zap.Int64("group_id", groupID),
		zap.String("driver", cap.DriverName),
	)

	dispatchCtx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	res, err := openaiimages.Dispatch(dispatchCtx, h.source, h.registry, in, h.dispatchO)
	if err != nil {
		reqLog.Warn("openaiimages.dispatch_failed", zap.Error(err))
		status, code, message := classifyDispatchError(err)
		writeOpenAIImageError(c, status, code, message)
		return
	}

	reqLog.Info("openaiimages.dispatch_ok",
		zap.Int64("account_id", res.Account.ID()),
		zap.String("driver", res.DriverUsed),
		zap.Int("attempts", res.Attempts),
	)

	setOpsSelectedAccount(c, res.Account.ID(), service.PlatformOpenAI)

	if req.ResponseFormat == openaiimages.ResponseFormatURL {
		h.materializeAsURLs(c, res.Result, reqLog)
	}

	sink := openaiimages.NewGinSink(c)
	if err := openaiimages.WriteResult(sink, req, res.Result, openaiimages.WriteOptions{
		ClientModel: req.Model,
	}); err != nil {
		reqLog.Warn("openaiimages.write_failed", zap.Error(err))
	}

	// 异步写入 usage_logs / 扣余额 / 更新 api_key 配额，与文本入口保持一致。
	h.recordImageUsage(c, req, res, apiKey, subscription, channelMapping, requestStart, reqLog)
}

// recordImageUsage 在请求成功后构造 OpenAIForwardResult 并提交到 usage worker pool。
// 失败时只记日志，不影响已写入的图片响应。
func (h *OpenAIImagesV2Handler) recordImageUsage(
	c *gin.Context,
	req *openaiimages.ImagesRequest,
	res *openaiimages.DispatchResult,
	apiKey *service.APIKey,
	subscription *service.UserSubscription,
	channelMapping service.ChannelMappingResult,
	requestStart time.Time,
	reqLog *zap.Logger,
) {
	if h.gatewayService == nil || apiKey == nil || res == nil || res.Result == nil {
		return
	}

	imageCount := len(res.Result.Items)
	if imageCount == 0 {
		return
	}

	upstreamModel := res.Result.Model
	if upstreamModel == "" {
		upstreamModel = req.Model
	}
	imageSize := strings.TrimSpace(req.Size)

	forwardResult := &service.OpenAIForwardResult{
		RequestID:     c.GetHeader("X-Request-Id"),
		Model:         req.Model,
		UpstreamModel: upstreamModel,
		Stream:        req.Stream,
		Duration:      time.Since(requestStart),
		ImageCount:    imageCount,
		ImageSize:     imageSize,
	}

	userAgent := c.GetHeader("User-Agent")
	clientIP := ip.GetClientIP(c)
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := GetUpstreamEndpoint(c, service.PlatformOpenAI)
	accountID := res.Account.ID()

	h.submitUsageRecordTask(func(ctx context.Context) {
		account, err := h.accountRepo.GetByID(ctx, accountID)
		if err != nil || account == nil {
			reqLog.Warn("openaiimages.record_usage_account_load_failed",
				zap.Int64("account_id", accountID),
				zap.Error(err),
			)
			return
		}
		if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result:             forwardResult,
			APIKey:             apiKey,
			User:               apiKey.User,
			Account:            account,
			Subscription:       subscription,
			InboundEndpoint:    inboundEndpoint,
			UpstreamEndpoint:   upstreamEndpoint,
			UserAgent:          userAgent,
			IPAddress:          clientIP,
			APIKeyService:      h.apiKeyService,
			ChannelUsageFields: channelMapping.ToUsageFields(req.Model, upstreamModel),
		}); err != nil {
			logger.L().With(
				zap.String("component", "handler.openai_images.v2"),
				zap.Int64("api_key_id", apiKey.ID),
				zap.Any("group_id", apiKey.GroupID),
				zap.String("model", req.Model),
				zap.Int64("account_id", accountID),
				zap.Int("image_count", imageCount),
			).Error("openaiimages.record_usage_failed", zap.Error(err))
		}
	})
}

// submitUsageRecordTask 与 GatewayHandler.submitUsageRecordTask 行为一致：
// 优先走 worker pool，未注入时同步执行兜底，避免无界 goroutine。
func (h *OpenAIImagesV2Handler) submitUsageRecordTask(task service.UsageRecordTask) {
	if task == nil {
		return
	}
	if h.usageRecordWorkerPool != nil {
		h.usageRecordWorkerPool.Submit(task)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			logger.L().With(
				zap.String("component", "handler.openai_images.v2"),
				zap.Any("panic", r),
			).Error("openaiimages.usage_record_task_panic_recovered")
		}
	}()
	task(ctx)
}

// readBody 读请求 body 并写错误。
// 如果 ShouldHandle 已经预读并缓存了 body，会直接复用缓存避免再读一次。
func (h *OpenAIImagesV2Handler) readBody(c *gin.Context) ([]byte, bool) {
	if cached, ok := c.Get("openai_chat_body_cache"); ok {
		if b, ok2 := cached.([]byte); ok2 && len(b) > 0 {
			return b, true
		}
	}
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		writeOpenAIImageError(c, http.StatusBadRequest, "invalid_request_error",
			"Failed to read request body: "+err.Error())
		return nil, false
	}
	if len(body) == 0 {
		writeOpenAIImageError(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return nil, false
	}
	return body, true
}

// lookupAccountView 把 PoolAccount 包装为 AccountView，注入 api_key / device 等
// PoolAccount 不带的字段。
func (h *OpenAIImagesV2Handler) lookupAccountView(ctx context.Context, pa openaiimages.PoolAccount) (openaiimages.AccountView, error) {
	acct, err := h.accountRepo.GetByID(ctx, pa.ID)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, errors.New("openaiimages: account vanished")
	}

	apiKey, _ := acct.Credentials["api_key"].(string)
	chatGPTAcctID, _ := acct.Credentials["chatgpt_account_id"].(string)
	userAgent, _ := acct.Credentials["user_agent"].(string)
	// device_id / session_id 解析见 openaiimages.ResolveDeviceSession：
	// 持久化(extra) → credentials 兼容 → stable UUIDv5 派生。
	credDeviceID, _ := acct.Credentials["device_id"].(string)
	credSessionID, _ := acct.Credentials["session_id"].(string)
	deviceID, sessionID := openaiimages.ResolveDeviceSession(
		acct.ID,
		acct.GetOpenAIDeviceID(),
		acct.GetOpenAISessionID(),
		credDeviceID,
		credSessionID,
	)

	groupLegacy := false
	for _, g := range acct.Groups {
		if g != nil && g.OpenAILegacyImagesDefault {
			groupLegacy = true
			break
		}
	}

	view := openaiimages.NewPoolAccountView(
		openaiimages.PoolAccount{
			ID:          acct.ID,
			Status:      acct.Status,
			Schedulable: acct.Schedulable,
			GroupIDs:    acct.GroupIDs,
			Extra:       acct.Extra,
			LastUsedAt:  acct.LastUsedAt,
			AccessToken: extractOAuthAccessToken(acct),
			ProxyURL:    extractProxyURL(acct),
		},
		openaiimages.WithAPIKey(apiKey),
		openaiimages.WithGroupLegacyDefault(groupLegacy),
		openaiimages.WithDeviceSession(deviceID, sessionID, chatGPTAcctID, userAgent),
	)
	return view, nil
}

// --- helpers ---

func writeOpenAIImageError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    code,
			"code":    code,
			"message": message,
		},
	})
}

func classifyDispatchError(err error) (int, string, string) {
	switch {
	case errors.Is(err, openaiimages.ErrNoAccountAvailable):
		return http.StatusServiceUnavailable, "no_account_available",
			"no usable image account; check group bindings and account quota"
	case errors.Is(err, openaiimages.ErrDriverNotRegistered):
		return http.StatusInternalServerError, "internal_error", err.Error()
	case errors.Is(err, openaiimages.ErrMaxAttemptsExceeded):
		return http.StatusServiceUnavailable, "exhausted_retries",
			"all candidate accounts failed; please retry later"
	}
	if openaiimages.IsAuth(err) {
		return http.StatusUnauthorized, "authentication_error", err.Error()
	}
	if openaiimages.IsRateLimit(err) {
		return http.StatusTooManyRequests, "rate_limit_exceeded", err.Error()
	}
	var ue *openaiimages.UpstreamError
	if errors.As(err, &ue) {
		status := ue.HTTPStatus
		if status < 400 || status >= 600 {
			status = http.StatusBadGateway
		}
		return status, "upstream_error", err.Error()
	}
	var tr *openaiimages.TransportError
	if errors.As(err, &tr) {
		return http.StatusBadGateway, "upstream_error", err.Error()
	}
	return http.StatusInternalServerError, "internal_error", err.Error()
}

// listOpenAIImageAccounts 是 ImagePool.List 的实现：拉取 OpenAI 平台 / 指定分组下
// 全部可调度账号，按 PoolAccount 投影返回。
func listOpenAIImageAccounts(
	ctx context.Context,
	repo service.AccountRepository,
	f openaiimages.PoolFilter,
) ([]openaiimages.PoolAccount, error) {
	var (
		accounts []service.Account
		err      error
	)
	if f.GroupID > 0 {
		accounts, err = repo.ListSchedulableByGroupIDAndPlatform(ctx, f.GroupID, service.PlatformOpenAI)
	} else {
		accounts, err = repo.ListSchedulableByPlatform(ctx, service.PlatformOpenAI)
	}
	if err != nil {
		return nil, err
	}

	out := make([]openaiimages.PoolAccount, 0, len(accounts))
	for i := range accounts {
		a := &accounts[i]
		if !filterByDriver(a, f.Driver) {
			continue
		}
		out = append(out, openaiimages.PoolAccount{
			ID:          a.ID,
			Status:      a.Status,
			Schedulable: a.Schedulable,
			GroupIDs:    a.GroupIDs,
			Extra:       cloneExtra(a.Extra),
			LastUsedAt:  a.LastUsedAt,
			AccessToken: extractOAuthAccessToken(a),
			ProxyURL:    extractProxyURL(a),
		})
	}
	return out, nil
}

// filterByDriver 按 driver 类型过滤账号。
//
//	apikey   → 仅接受 type=api_key 的账号
//	web      → 仅接受 OAuth 账号
//	responses → 同 web（OAuth 账号）
func filterByDriver(a *service.Account, driver string) bool {
	isAPIKey := strings.EqualFold(a.Type, "api_key") || a.Credentials["api_key"] != nil
	switch driver {
	case openaiimages.DriverAPIKey:
		return isAPIKey
	case openaiimages.DriverWeb, openaiimages.DriverResponses:
		return !isAPIKey
	}
	return true
}

func extractOAuthAccessToken(a *service.Account) string {
	if a == nil {
		return ""
	}
	if v, ok := a.Credentials["access_token"].(string); ok {
		return v
	}
	if v, ok := a.Credentials["accessToken"].(string); ok {
		return v
	}
	return ""
}

func extractProxyURL(a *service.Account) string {
	if a == nil || a.Proxy == nil {
		return ""
	}
	return a.Proxy.URL()
}

func cloneExtra(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// materializeAsURLs 把每个 ImageItem 的字节落入 ImageCache，签发本服务可访问的短链。
//
// 适用场景：用户传 response_format=url，但 driver（WebDriver / ResponsesToolDriver）
// 拿到的是上游签名 URL（带短期 token + 强鉴权），无法直接给客户端访问。
//
// 处理规则：
//   - 已有可用 URL（http/https 直连，例如 ApiKeyDriver 透传 OpenAI 官方签名）→ 保留
//   - 否则用 Bytes（webdriver 默认）或 base64-decoded B64JSON 写入 cache
//   - 写入后清空 B64JSON / Bytes，避免响应体重复携带二进制
func (h *OpenAIImagesV2Handler) materializeAsURLs(c *gin.Context, res *openaiimages.ImageResult, reqLog *zap.Logger) {
if h.cache == nil || res == nil {
return
}
base := h.publicBaseURL(c)
for i := range res.Items {
it := &res.Items[i]
if strings.HasPrefix(it.URL, "http://") || strings.HasPrefix(it.URL, "https://") {
continue
}
data := it.Bytes
if len(data) == 0 && it.B64JSON != "" {
if decoded, err := openaiimages.DecodeBase64(it.B64JSON); err == nil {
data = decoded
}
}
if len(data) == 0 {
continue
}
mime := it.MimeType
if mime == "" {
mime = "image/png"
}
id, err := h.cache.Put(data, mime)
if err != nil {
reqLog.Warn("openaiimages.cache_put_failed", zap.Error(err))
continue
}
it.URL = base + "/v1/files/cached/" + id + extForMimePublic(mime)
it.B64JSON = ""
it.Bytes = nil
}
}

// ServeCachedFile 处理 GET /v1/files/cached/:id，返回原始字节。
// 公开访问（id 不可猜，且 24h 后 GC）。
func (h *OpenAIImagesV2Handler) ServeCachedFile(c *gin.Context) {
if h.cache == nil {
c.AbortWithStatus(http.StatusNotFound)
return
}
raw := c.Param("id")
id := raw
if dot := strings.IndexByte(raw, '.'); dot > 0 {
id = raw[:dot]
}
data, mime, ok := h.cache.Get(id)
if !ok {
c.AbortWithStatus(http.StatusNotFound)
return
}
c.Header("Cache-Control", "public, max-age=86400, immutable")
c.Data(http.StatusOK, mime, data)
}

// applyDefaultResponseFormat 在客户端未显式指定 response_format 时，
// 用管理后台「默认图片返回方式」(SettingKeyDefaultImageResponseFormat) 覆盖请求。
//   - "auto"：保持解析阶段的入口默认（images/* → b64_json，chat/responses → markdown）
//   - "b64_json" / "url" / "markdown"：覆盖为指定值
func (h *OpenAIImagesV2Handler) applyDefaultResponseFormat(c *gin.Context, req *openaiimages.ImagesRequest) {
	if req == nil || req.ResponseFormatExplicit || h.settings == nil {
		return
	}
	cfg, err := h.settings.GetAllSettings(c.Request.Context())
	if err != nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(cfg.DefaultImageResponseFormat)) {
	case "b64_json":
		req.ResponseFormat = openaiimages.ResponseFormatB64JSON
	case "url":
		req.ResponseFormat = openaiimages.ResponseFormatURL
	case "markdown":
		req.ResponseFormat = openaiimages.ResponseFormatMarkdown
	}
}

// publicBaseURL 解析签发短链所用的 base URL：
//   - 优先读管理后台「图片缓存 Base URL」(SettingKeyImageCacheBaseURL)
//   - 缺省时回落到请求头推断（X-Forwarded-Proto / X-Forwarded-Host），
//     再回落到 c.Request.Host
//
// 返回值不带末尾 slash，调用方负责拼接 path（须以 "/" 起始）。
func (h *OpenAIImagesV2Handler) publicBaseURL(c *gin.Context) string {
	if h.settings != nil {
		if cfg, err := h.settings.GetAllSettings(c.Request.Context()); err == nil {
			if v := strings.TrimRight(strings.TrimSpace(cfg.ImageCacheBaseURL), "/"); v != "" {
				return v
			}
		}
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	return scheme + "://" + host
}

func extForMimePublic(mime string) string {
switch strings.ToLower(mime) {
case "image/jpeg", "image/jpg":
return ".jpg"
case "image/webp":
return ".webp"
case "image/gif":
return ".gif"
default:
return ".png"
}
}
