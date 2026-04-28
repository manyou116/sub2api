package openaiimages

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AccountSource 抽象 driver 选号 + 事后回写的协作接口。
//
// dispatch 不直接依赖 ImagePool / AccountProbe，便于：
//  1. 单测用 fake 替换；
//  2. handler 层把 ImagePool.SelectAccount 返回的 PoolAccount 包装为 AccountView；
//  3. 未来切换调度策略（例如轮询 / 静态列表）无需改 dispatch。
type AccountSource interface {
	// Select 选一个可用账号；返回的 release 必须在调用方完成请求后调用一次。
	// 若没有可用账号应返回非 nil error（推荐 ErrNoAccountAvailable）。
	Select(ctx context.Context, filter PoolFilter) (AccountView, func(), error)
	// OnSuccess 记录成功结果；通常用于刷新 quota snapshot 与 last_used。
	OnSuccess(ctx context.Context, account AccountView, result *ImageResult) error
	// OnRateLimit 记录账号限流；resetAt 来自 driver 错误（可能为零，零则使用默认 cooldown）。
	OnRateLimit(ctx context.Context, account AccountView, resetAt time.Time) error
	// OnTransient 记录临时性失败（5xx / 网络），上层会换号重试。
	OnTransient(ctx context.Context, account AccountView, err error) error
	// OnAuthFailure 记录账号鉴权失败；上层会按 cooldown 隔离一段时间避免反复打。
	OnAuthFailure(ctx context.Context, account AccountView, err error) error
}

// DriverRegistry 把 driver 名（"web" / "apikey" / "responses"）映射到实例。
type DriverRegistry interface {
	Get(name string) (Driver, bool)
}

// MapDriverRegistry 是基于 map 的简单注册表实现。
type MapDriverRegistry map[string]Driver

func (m MapDriverRegistry) Get(name string) (Driver, bool) {
	d, ok := m[name]
	return d, ok
}

// DispatchOptions 控制 dispatch 行为。
type DispatchOptions struct {
	// MaxAttempts 最大尝试次数（含首次）；<=0 视为 3。
	MaxAttempts int
	// AuthCooldown 触发 AuthError 后给该账号的隔离窗口；零值默认 1h。
	AuthCooldown time.Duration
	// DefaultRateLimitCooldown 当 RateLimitError.ResetAfter==0 时使用的兜底窗口；零值默认 5min。
	DefaultRateLimitCooldown time.Duration
	// Sleep 用于在 transient 重试之间退避；<=0 不睡。测试可注入 no-op。
	Sleep func(time.Duration)
	// Now 时钟注入；nil 用 time.Now。
	Now func() time.Time
}

// DispatchInput 是 Dispatch 的输入。
type DispatchInput struct {
	Capability Capability
	Filter     PoolFilter
	Request    *ImagesRequest
}

// DispatchResult 是 Dispatch 成功路径的输出。
type DispatchResult struct {
	Result     *ImageResult
	Account    AccountView
	DriverUsed string
	Attempts   int
}

// 错误：调度专属。
var (
	ErrNoAccountAvailable  = errors.New("openaiimages: no account available")
	ErrDriverNotRegistered = errors.New("openaiimages: driver not registered")
	ErrMaxAttemptsExceeded = errors.New("openaiimages: max attempts exceeded")
)

// Dispatch 执行"选号 → 调用 driver → 错误分类回写 → 必要时换号重试"的核心循环。
//
// 该函数完全无 gin 依赖，便于单测。Handler 层把它包到 gin.HandlerFunc 中，
// 在循环外做 slot 获取、billing 校验、ops 打点等横切关注点。
func Dispatch(
	ctx context.Context,
	src AccountSource,
	drivers DriverRegistry,
	in DispatchInput,
	opts DispatchOptions,
) (*DispatchResult, error) {
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	authCD := opts.AuthCooldown
	if authCD <= 0 {
		authCD = time.Hour
	}
	rlDefault := opts.DefaultRateLimitCooldown
	if rlDefault <= 0 {
		rlDefault = 5 * time.Minute
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		account, release, err := src.Select(ctx, in.Filter)
		if err != nil {
			// 选号失败本身不重试（调度层一次性拿不到号说明全部冷却中或没匹配）
			return nil, fmt.Errorf("%w: %v", ErrNoAccountAvailable, err)
		}

		// 选号成功后，无论是否成功一律 release lease。
		result, driverName, callErr := callDriver(ctx, drivers, in, account)
		release()

		if callErr == nil {
			if rerr := src.OnSuccess(ctx, account, result); rerr != nil {
				// success 回写失败不影响业务结果，记下来供观察。
				lastErr = rerr
			}
			return &DispatchResult{
				Result:     result,
				Account:    account,
				DriverUsed: driverName,
				Attempts:   attempt,
			}, nil
		}

		lastErr = callErr

		// 错误分类
		switch {
		case IsAuth(callErr):
			_ = src.OnAuthFailure(ctx, account, callErr)
			// 鉴权失败：账号本身坏了，临时拉黑后换号。
			_ = src.OnRateLimit(ctx, account, now().Add(authCD))
			continue

		case IsRateLimit(callErr):
			var rl *RateLimitError
			_ = errors.As(callErr, &rl)
			resetAt := time.Time{}
			if rl != nil && rl.ResetAfter > 0 {
				resetAt = now().Add(rl.ResetAfter)
			} else {
				resetAt = now().Add(rlDefault)
			}
			_ = src.OnRateLimit(ctx, account, resetAt)
			continue

		case IsRetryable(callErr):
			_ = src.OnTransient(ctx, account, callErr)
			if opts.Sleep != nil && attempt < maxAttempts {
				opts.Sleep(backoffFor(attempt))
			}
			continue

		default:
			// 不可重试（4xx / driver 未注册 / 客户端错误等），直接回客户端。
			return nil, callErr
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrMaxAttemptsExceeded, lastErr)
	}
	return nil, ErrMaxAttemptsExceeded
}

// callDriver 解析 driver 名并执行 Forward。
func callDriver(
	ctx context.Context,
	drivers DriverRegistry,
	in DispatchInput,
	account AccountView,
) (*ImageResult, string, error) {
	name := ResolveDriverName(in.Capability, account)
	driver, ok := drivers.Get(name)
	if !ok {
		return nil, name, fmt.Errorf("%w: %s", ErrDriverNotRegistered, name)
	}
	res, err := driver.Forward(ctx, account, in.Request)
	return res, name, err
}

// backoffFor 在 transient 重试之间提供一个温和的退避：100ms / 300ms / 700ms ...
func backoffFor(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 100 * time.Millisecond
	case 2:
		return 300 * time.Millisecond
	default:
		return 700 * time.Millisecond
	}
}
