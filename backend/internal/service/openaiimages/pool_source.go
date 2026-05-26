package openaiimages

import (
	"context"
	"errors"
	"strings"
	"time"
)

// PoolAccountView 把 PoolAccount 包装为 driver 看到的 AccountView。
//
// 字段提取规则参照 account_probe.go 写入 extra 时的 key 命名：
//
//	account_email          → 邮箱（仅展示，driver 不读）
//	openai_oauth_legacy_images → "enabled"/"disabled"/"inherit"
//	image_account_plan     → 账号 plan（用于 quota 判定）
//	image_quota_remaining  → 剩余次数
//	image_quota_total      → 总额度
//	image_cooldown_until   → RFC3339 字符串
//	image_last_probed_at   → RFC3339
type PoolAccountView struct {
	pa            PoolAccount
	groupLegacy   bool // 分组 default = enabled
	apiKey        string
	deviceID      string
	sessionID     string
	chatGPTAcctID string
	userAgent     string
	maxConc       int
}

// PoolAccountViewOption 用于注入 PoolAccount 之外的字段（这些字段不在 PoolAccount 里，
// 调用方一般从 service.Account 中取出后注入）。
type PoolAccountViewOption func(*PoolAccountView)

func WithGroupLegacyDefault(enabled bool) PoolAccountViewOption {
	return func(v *PoolAccountView) { v.groupLegacy = enabled }
}
func WithAPIKey(key string) PoolAccountViewOption {
	return func(v *PoolAccountView) { v.apiKey = key }
}
func WithDeviceSession(deviceID, sessionID, chatGPTAcctID, userAgent string) PoolAccountViewOption {
	return func(v *PoolAccountView) {
		v.deviceID = deviceID
		v.sessionID = sessionID
		v.chatGPTAcctID = chatGPTAcctID
		v.userAgent = userAgent
	}
}

// WithMaxConcurrency 注入账号的最大并发数（用于 ConcurrencyService 槽位申请）。
func WithMaxConcurrency(n int) PoolAccountViewOption {
	return func(v *PoolAccountView) { v.maxConc = n }
}

// NewPoolAccountView 构造一个 driver 视图。
func NewPoolAccountView(pa PoolAccount, opts ...PoolAccountViewOption) *PoolAccountView {
	v := &PoolAccountView{pa: pa}
	for _, o := range opts {
		o(v)
	}
	return v
}

func (v *PoolAccountView) ID() int64                { return v.pa.ID }
func (v *PoolAccountView) AccessToken() string      { return v.pa.AccessToken }
func (v *PoolAccountView) ChatGPTAccountID() string { return v.chatGPTAcctID }
func (v *PoolAccountView) UserAgent() string        { return v.userAgent }
func (v *PoolAccountView) DeviceID() string         { return v.deviceID }
func (v *PoolAccountView) SessionID() string        { return v.sessionID }
func (v *PoolAccountView) ProxyURL() string         { return v.pa.ProxyURL }
func (v *PoolAccountView) IsAPIKey() bool           { return v.apiKey != "" }
func (v *PoolAccountView) APIKey() string           { return v.apiKey }
func (v *PoolAccountView) MaxConcurrency() int      { return v.maxConc }

// LegacyImagesEnabled 实现"账号覆盖分组"的三态逻辑：
//
//	account.extra.openai_oauth_legacy_images:
//	  "enabled"  → true
//	  "disabled" → false
//	  "inherit" / 缺省 → 跟随 group default
func (v *PoolAccountView) LegacyImagesEnabled() bool {
	switch raw := v.pa.Extra["openai_oauth_legacy_images"].(type) {
	case bool:
		return raw
	case string:
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "enabled", "true", "on":
			return true
		case "disabled", "false", "off":
			return false
		}
	}
	return v.groupLegacy
}

func (v *PoolAccountView) QuotaSnapshot() *AccountQuotaSnapshot {
	plan, _ := v.pa.Extra["image_account_plan"].(string)
	rem := readQuotaRemaining(v.pa.Extra)
	total := 0
	if t, ok := v.pa.Extra["image_quota_total"].(float64); ok {
		total = int(t)
	}
	cd := readCooldown(v.pa.Extra)
	probed, _ := v.pa.Extra["image_last_probed_at"].(string)
	probedAt, _ := time.Parse(time.RFC3339, probed)
	return &AccountQuotaSnapshot{
		Plan:           plan,
		QuotaRemaining: rem,
		QuotaTotal:     total,
		CooldownUntil:  cd,
		ObservedAt:     probedAt,
	}
}

// PoolSourceDeps 是 PoolBackedSource 需要的外部依赖。
type PoolSourceDeps struct {
	Pool *ImagePool

	// LookupAccount 在选号成功后由上层提供：根据 PoolAccount.ID 取出 service.Account
	// 并构造完整 AccountView（包括 api_key / device_id / session_id 等 PoolAccount
	// 不带的字段）。返回的 view 必须与 PoolAccount.ID() 一致。
	LookupAccount func(ctx context.Context, pa PoolAccount) (AccountView, error)

	// AcquireSlot 可选：lookup 成功后立即占用账号级并发槽位（接入 ConcurrencyService），
	// 这样图片请求会同步出现在 OpsConcurrencyCard 的账号/分组/平台聚合里。
	// 返回的 release 会被串接到 Select() 返回的 release 函数末尾。
	// 失败或槽位满时应返回 error，调用方会释放 pool lease 并把本次选号视为不可用。
	AcquireSlot func(ctx context.Context, accountID int64, maxConcurrency int) (release func(), err error)
}

// PoolBackedSource 把 ImagePool 适配为 dispatch.AccountSource。
//
// 不直接依赖 service.Account，组装上下文由 LookupAccount 注入回调完成；
// 这样保持 service/openaiimages 与 service 包解耦。
type PoolBackedSource struct {
	deps PoolSourceDeps
}

func NewPoolBackedSource(deps PoolSourceDeps) *PoolBackedSource {
	return &PoolBackedSource{deps: deps}
}

func (s *PoolBackedSource) Select(ctx context.Context, filter PoolFilter) (AccountView, func(), error) {
	excluded := map[int64]struct{}{}
	for {
		pa, release, err := s.deps.Pool.selectAccount(ctx, filter, excluded)
		if err != nil {
			return nil, nil, err
		}
		view, err := s.deps.LookupAccount(ctx, pa)
		if err != nil {
			release()
			return nil, nil, err
		}
		if view == nil {
			release()
			return nil, nil, errors.New("openaiimages: lookup returned nil view")
		}

		// 接入账号级并发槽位（用于 OpsConcurrencyCard 实时观测和多实例并发约束）。
		slotRelease := func() {}
		if s.deps.AcquireSlot != nil {
			rel, err := s.deps.AcquireSlot(ctx, view.ID(), view.MaxConcurrency())
			if err != nil {
				release()
				if errors.Is(err, ErrNoImageAccount) {
					excluded[pa.ID] = struct{}{}
					continue
				}
				return nil, nil, err
			}
			if rel != nil {
				slotRelease = rel
			}
		}

		combined := func() {
			slotRelease()
			release()
		}
		return view, combined, nil
	}
}

func (s *PoolBackedSource) OnSuccess(ctx context.Context, account AccountView, result *ImageResult) error {
	currentExtra := map[string]any{}
	if snap := account.QuotaSnapshot(); snap != nil {
		if snap.QuotaRemaining > 0 {
			currentExtra["image_quota_remaining"] = float64(snap.QuotaRemaining)
		}
		if snap.QuotaTotal > 0 {
			currentExtra["image_quota_total"] = float64(snap.QuotaTotal)
		}
	}
	// driver 顺便观测到的 quota 优先（覆盖 stale 缓存）。
	if result != nil && result.QuotaSnapshot != nil {
		snap := result.QuotaSnapshot
		if snap.QuotaRemaining >= 0 {
			currentExtra["image_quota_remaining"] = float64(snap.QuotaRemaining)
		}
		if snap.QuotaTotal > 0 {
			currentExtra["image_quota_total"] = float64(snap.QuotaTotal)
		}
		if !snap.CooldownUntil.IsZero() {
			currentExtra["image_cooldown_until"] = snap.CooldownUntil.UTC().Format(time.RFC3339)
		}
	}
	return s.deps.Pool.RecordSuccess(ctx, account.ID(), currentExtra)
}

func (s *PoolBackedSource) OnRateLimit(ctx context.Context, account AccountView, resetAt time.Time) error {
	return s.deps.Pool.RecordRateLimit(ctx, account.ID(), resetAt)
}

func (s *PoolBackedSource) OnTransient(_ context.Context, _ AccountView, _ error) error {
	// transient 失败不影响 cooldown，仅日志（dispatch 已记录 lastErr）。
	return nil
}

func (s *PoolBackedSource) OnAuthFailure(_ context.Context, _ AccountView, _ error) error {
	// dispatch 会同步调用 OnRateLimit 把账号关进短期黑屋；这里无需额外动作。
	return nil
}
