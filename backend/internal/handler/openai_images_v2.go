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
	"sync"
	"sync/atomic"
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
	concurrencyService    *service.ConcurrencyService

	pool       *openaiimages.ImagePool
	probe      *openaiimages.AccountProbe
	source     *openaiimages.PoolBackedSource
	registry   openaiimages.MapDriverRegistry
	dispatchO  openaiimages.DispatchOptions
	cache      *openaiimages.ImageCache
	settings   *service.SettingService
	opsService *service.OpsService

	// 每实例图片网关并发上限（防 OOM）。
	// inFlight 是当前正在执行的图片请求数；max 由 OpsService.GetImageGatewayRuntimeSettings 配置。
	// max=0 表示不限。配置带 5s 缓存避免每请求都查 DB。
	inFlight         atomic.Int64
	maxConcurrent    atomic.Int64                                        // 0 = unlimited
	maxConcExpires   atomic.Int64                                        // unix nano，缓存过期时间
	cachedLimiterCfg atomic.Pointer[service.ImageGatewayRuntimeSettings] // 完整配置缓存

	// 排队等待支持（mode="queue"）。
	// limiter 是动态构建的信号量；limiterMu 保护 limiter 重建，按 max 变化时重新分配 channel。
	// queued 是当前排队中（未拿到槽）的请求数，用于 MaxQueueSize 反压。
	limiterMu  sync.Mutex
	limiter    chan struct{} // capacity = current MaxConcurrent
	limiterMax int64         // limiter 当前容量
	queued     atomic.Int64
}

func NewOpenAIImagesV2Handler(
	accountRepo service.AccountRepository,
	groupRepo service.GroupRepository,
	gatewayService *service.OpenAIGatewayService,
	billingCacheService *service.BillingCacheService,
	apiKeyService *service.APIKeyService,
	usageRecordWorkerPool *service.UsageRecordWorkerPool,
	settingService *service.SettingService,
	concurrencyService *service.ConcurrencyService,
	opsService *service.OpsService,
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
		concurrencyService:    concurrencyService,
		opsService:            opsService,
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
			// 单次 driver 调用预算：4K 通过 codex /responses 路径正常 60-180s，
			// 留 200s 容忍偶发慢响应（Plus 账号繁忙时段）。
			AttemptBudget:     200 * time.Second,
			RefusalRetryLimit: 3,
			Sleep:             time.Sleep,
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
		AcquireSlot: func(ctx context.Context, accountID int64, maxConc int) (func(), error) {
			if h.concurrencyService == nil {
				return func() {}, nil
			}
			res, err := h.concurrencyService.AcquireImageAccountSlot(ctx, accountID, maxConc)
			if err != nil {
				// 槽位申请出错时降级（不阻断图片调度）
				return func() {}, nil
			}
			if !res.Acquired || res.ReleaseFunc == nil {
				return func() {}, nil
			}
			return res.ReleaseFunc, nil
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

	// 全局图片网关并发限流（防 OOM）。max=0 表示不限。
	// mode="reject": 立即返回 429；mode="queue": 等待直至超时或队列满。
	if cfg := h.currentLimiterConfig(c.Request.Context()); cfg != nil && cfg.MaxConcurrent > 0 {
		release, err := h.acquireLimiter(c.Request.Context(), cfg, reqLog)
		if err != nil {
			writeOpenAIImageError(c, http.StatusTooManyRequests, "rate_limit_exceeded", err.Error())
			return
		}
		defer release()
	}

	// Wire ops monitoring context (model/stream/body) so admin /ops/requests
	// shows model, and so OpsErrorLoggerMiddleware can record full error rows.
	// Body cache may be absent for multipart (Edits) — pass nil in that case.
	var bodyForOps []byte
	if cached, ok := c.Get("openai_chat_body_cache"); ok {
		if b, ok2 := cached.([]byte); ok2 {
			bodyForOps = b
		}
	}
	setOpsRequestContext(c, req.Model, req.Stream, bodyForOps)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())

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

	// 整个分发流程的硬上限：覆盖 1 次重试（200s）+ 重选号 + 网络抖动余量。
	// 4K 在 codex /responses 路径下偶发 150s+，旧值 90s 远不够，调到 240s。
	dispatchCtx, cancel := context.WithTimeout(c.Request.Context(), 240*time.Second)
	defer cancel()
	upstreamStart := time.Now()
	res, err := openaiimages.Dispatch(dispatchCtx, h.source, h.registry, in, h.dispatchO)
	service.SetOpsLatencyMs(c, service.OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
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
		// 仅当客户端显式要 url 时清空 b64_json；
		// 否则（管理后台默认覆盖触发）保留 b64_json，让 Cherry/Vercel-AI-SDK 等
		// 强 zod schema 校验 b64_json 的客户端也能解析。
		h.materializeAsURLs(c, res.Result, reqLog, req.ResponseFormatExplicit)
	}

	sink := openaiimages.NewGinSink(c)
	writeStart := time.Now()
	if err := openaiimages.WriteResult(sink, req, res.Result, openaiimages.WriteOptions{
		ClientModel: req.Model,
	}); err != nil {
		reqLog.Warn("openaiimages.write_failed", zap.Error(err))
	}
	service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, time.Since(writeStart).Milliseconds())

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
		openaiimages.WithMaxConcurrency(acct.Concurrency),
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
	if openaiimages.IsContentPolicy(err) {
		var cp *openaiimages.ContentPolicyError
		errors.As(err, &cp)
		msg := "request rejected by upstream content policy"
		if cp != nil && cp.UpstreamMessage != "" {
			msg = cp.UpstreamMessage
		}
		return http.StatusBadRequest, "content_policy_violation", msg
	}
	if openaiimages.IsModelNoImage(err) {
		var ni *openaiimages.ModelNoImageError
		errors.As(err, &ni)
		msg := "model produced no image (please refine your prompt)"
		if ni != nil && ni.UpstreamMessage != "" {
			msg = ni.UpstreamMessage
		}
		return http.StatusBadRequest, "model_no_image", msg
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
//   - clearB64=true（客户端显式要 url）→ 清空 b64_json/Bytes
//   - clearB64=false（管理后台默认覆盖）→ 同时保留 b64_json，兼容 Cherry/Vercel SDK
func (h *OpenAIImagesV2Handler) materializeAsURLs(c *gin.Context, res *openaiimages.ImageResult, reqLog *zap.Logger, clearB64 bool) {
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
		b64 := it.B64JSON
		if len(data) == 0 && b64 != "" {
			if decoded, err := openaiimages.DecodeBase64(b64); err == nil {
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
		if clearB64 {
			it.B64JSON = ""
		} else if b64 == "" {
			it.B64JSON = openaiimages.EncodeBase64(data)
		}
		it.Bytes = nil
	}
}

// ServeCachedFile 处理 GET /v1/files/cached/:id，返回原始字节。
// 公开访问（id 不可猜，且 24h 后 GC）。
// 默认放行所有跨域来源，方便前端 <img>/fetch 直接拉取。
func (h *OpenAIImagesV2Handler) ServeCachedFile(c *gin.Context) {
	// 公开图片资源：允许任意来源跨域读取。
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	c.Header("Access-Control-Allow-Headers", "Content-Type, Range")
	c.Header("Access-Control-Expose-Headers", "Content-Length, Content-Type, Cache-Control, ETag")

	if c.Request.Method == http.MethodOptions {
		c.AbortWithStatus(http.StatusNoContent)
		return
	}

	if h.cache == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	raw := c.Param("id")
	id := raw
	if dot := strings.IndexByte(raw, '.'); dot > 0 {
		id = raw[:dot]
	}
	file, mime, modTime, ok := h.cache.OpenForServe(id)
	if !ok {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	defer func() { _ = file.Close() }()
	c.Header("Cache-Control", "public, max-age=86400, immutable")
	c.Header("Content-Type", mime)
	// http.ServeContent 自动处理 ETag/Range/Content-Length，并用 io.Copy 流式响应。
	http.ServeContent(c.Writer, c.Request, raw, modTime, file)
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

// currentLimiterConfig 返回当前生效的图片网关运行时配置（带 5s 缓存）。
// 返回 nil 或 MaxConcurrent=0 表示不限流。
func (h *OpenAIImagesV2Handler) currentLimiterConfig(ctx context.Context) *service.ImageGatewayRuntimeSettings {
	if h == nil || h.opsService == nil {
		return nil
	}
	now := time.Now().UnixNano()
	cached := h.cachedLimiterCfg.Load()
	if exp := h.maxConcExpires.Load(); now < exp && cached != nil {
		return cached
	}
	cfg, err := h.opsService.GetImageGatewayRuntimeSettings(ctx)
	if err != nil || cfg == nil {
		// 失败时沿用旧值（avoid 临时 DB 抖动放行所有请求）
		return cached
	}
	h.cachedLimiterCfg.Store(cfg)
	h.maxConcurrent.Store(int64(cfg.MaxConcurrent))
	h.maxConcExpires.Store(now + int64(5*time.Second))
	return cfg
}

// ensureLimiter 按 max 容量重建/复用 limiter channel。
// 调用方必须已持有 limiterMu。
func (h *OpenAIImagesV2Handler) ensureLimiterLocked(max int64) {
	if h.limiter != nil && h.limiterMax == max {
		return
	}
	// 容量变化时重建。在途请求会照常 release 到旧 channel（被 GC）；
	// 新请求走新 channel。瞬时偏差可接受。
	h.limiter = make(chan struct{}, max)
	h.limiterMax = max
}

// acquireLimiter 按配置 mode 申请并发槽。reject 模式立即返回；queue 模式等待。
// 返回 release closure（必须 defer 调用）；error 表示拒绝，调用方应回 429。
// 闭包捕获本次拿到 token 的 channel，避免 limiter 容量变化时从错误的 channel 释放。
func (h *OpenAIImagesV2Handler) acquireLimiter(ctx context.Context, cfg *service.ImageGatewayRuntimeSettings, reqLog *zap.Logger) (func(), error) {
	max := int64(cfg.MaxConcurrent)
	h.limiterMu.Lock()
	h.ensureLimiterLocked(max)
	limiter := h.limiter
	h.limiterMu.Unlock()

	releaseFn := func() {
		h.inFlight.Add(-1)
		select {
		case <-limiter:
		default:
			// 防御性：不应发生，但避免 panic
		}
	}

	// 快路径：立即拿到槽
	select {
	case limiter <- struct{}{}:
		h.inFlight.Add(1)
		return releaseFn, nil
	default:
	}

	// 满了：根据 mode 决定 reject 或 queue
	if cfg.Mode != service.ImageGatewayModeQueue {
		reqLog.Warn("openaiimages.global_concurrency_exceeded",
			zap.Int64("in_flight", h.inFlight.Load()),
			zap.Int64("max", max),
			zap.String("mode", "reject"),
		)
		return nil, fmt.Errorf("image gateway is at capacity (max %d in-flight), please retry later", max)
	}

	// queue 模式：检查队列上限
	if cfg.MaxQueueSize > 0 {
		if q := h.queued.Add(1); q > int64(cfg.MaxQueueSize) {
			h.queued.Add(-1)
			reqLog.Warn("openaiimages.queue_full",
				zap.Int64("queued", q-1),
				zap.Int("max_queue", cfg.MaxQueueSize),
			)
			return nil, fmt.Errorf("image gateway queue is full (max %d waiting), please retry later", cfg.MaxQueueSize)
		}
		defer h.queued.Add(-1)
	} else {
		h.queued.Add(1)
		defer h.queued.Add(-1)
	}

	waitSec := cfg.MaxWaitSeconds
	if waitSec <= 0 {
		waitSec = 15
	}
	waitStart := time.Now()
	timer := time.NewTimer(time.Duration(waitSec) * time.Second)
	defer timer.Stop()

	select {
	case limiter <- struct{}{}:
		h.inFlight.Add(1)
		reqLog.Info("openaiimages.queue_acquired",
			zap.Duration("waited", time.Since(waitStart)),
		)
		return releaseFn, nil
	case <-timer.C:
		reqLog.Warn("openaiimages.queue_wait_timeout",
			zap.Int("max_wait_seconds", waitSec),
			zap.Int64("max", max),
		)
		return nil, fmt.Errorf("image gateway queue wait timeout (%ds), please retry later", waitSec)
	case <-ctx.Done():
		reqLog.Info("openaiimages.queue_canceled",
			zap.Duration("waited", time.Since(waitStart)),
			zap.Error(ctx.Err()),
		)
		return nil, fmt.Errorf("request canceled while waiting for capacity")
	}
}

// CurrentInFlight 返回当前正在执行的图片请求数（用于 ops snapshot 暴露给 UI）。
func (h *OpenAIImagesV2Handler) CurrentInFlight() int64 {
	if h == nil {
		return 0
	}
	return h.inFlight.Load()
}

// CurrentQueued 返回当前排队等待的请求数（queue 模式下）。
func (h *OpenAIImagesV2Handler) CurrentQueued() int64 {
	if h == nil {
		return 0
	}
	return h.queued.Load()
}
