package openaiimages

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// PoolAccount 是 ImagePool 选号时使用的账号最小投影。
//
// 由调用方（handler）从 service.Account 投影：
//
//	PoolAccount{
//	    ID: a.ID,
//	    Status: a.Status,
//	    Schedulable: a.Schedulable,
//	    Extra: a.Extra,
//	    LastUsedAt: a.LastUsedAt,
//	}
//
// 这样保持 service/openaiimages 与 service.Account 解耦，方便测试。
type PoolAccount struct {
	ID          int64
	Status      string
	Schedulable bool
	GroupIDs    []int64
	Extra       map[string]any
	LastUsedAt  *time.Time

	// AccessToken / ProxyURL：probe 触发或 driver 回调时使用。
	AccessToken string
	ProxyURL    string
}

// PoolFilter 决定哪些账号会被纳入候选。
type PoolFilter struct {
	GroupID  int64  // 0 表示不限分组
	Driver   string // "web" / "responses-tool" / "api-key"，用于过滤账号能力
	AuthMode string // "oauth" / "api_key" 等；与 Driver 配套
}

// PoolListAccounts 由上层注入：返回符合筛选条件的活跃账号。
type PoolListAccounts func(ctx context.Context, f PoolFilter) ([]PoolAccount, error)

// ImagePool 是图片调度池，与 codex 文本调度池完全隔离。
//
// 隔离手段：
//   - 仅读 account.extra 中以 "image_" 为前缀的字段
//   - 不读 model_rate_limits / codex_quota_*
//   - 内存 lease 表与 codex scheduler 不共享
type ImagePool struct {
	List   PoolListAccounts
	Probe  *AccountProbe
	Now    func() time.Time

	mu     sync.Mutex
	leased map[int64]time.Time // accountID → lease 到期时刻
}

// NewImagePool 构造 pool。
func NewImagePool(list PoolListAccounts, probe *AccountProbe) *ImagePool {
	return &ImagePool{
		List:   list,
		Probe:  probe,
		Now:    time.Now,
		leased: map[int64]time.Time{},
	}
}

// ReleaseFn 由 SelectAccount 返回，调用方在请求处理结束后必须调用，释放 lease。
type ReleaseFn func()

func (p *ImagePool) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// 错误：无可用账号。
var ErrNoImageAccount = errors.New("no image account available")

// SelectAccount 选号：
//  1. List(ctx, filter) 拿候选
//  2. 过滤 status != active / 不可调度 / 已 lease / 仍在 cooldown
//  3. 按 (image_quota_remaining DESC, last_used_at ASC) 排序
//  4. 取第一个并加 30 秒 lease（防 SSE 阶段并发重选）
//
// 若全部账号仍在 cooldown，对最近一个到期的账号触发一次 probe（带节流），再重试一轮。
func (p *ImagePool) SelectAccount(ctx context.Context, filter PoolFilter) (PoolAccount, ReleaseFn, error) {
	candidates, err := p.List(ctx, filter)
	if err != nil {
		return PoolAccount{}, noopRelease, err
	}
	now := p.now()

	picked, ok := p.pickLocked(candidates, now)
	if ok {
		p.maybeStaleProbe(picked, now)
		return picked, p.lease(picked.ID, now), nil
	}

	// 全部 cooldown：找到 cooldown 最早过期的账号补 probe（异步可能不及时，故同步）
	if probed := p.probeEarliestExpired(ctx, candidates, now); probed {
		// 重新读一次（可能 probe 写了新的 cooldown_until）
		candidates, err = p.List(ctx, filter)
		if err == nil {
			now = p.now()
			if picked, ok = p.pickLocked(candidates, now); ok {
				p.maybeStaleProbe(picked, now)
				return picked, p.lease(picked.ID, now), nil
			}
		}
	}
	return PoolAccount{}, noopRelease, ErrNoImageAccount
}

func (p *ImagePool) pickLocked(candidates []PoolAccount, now time.Time) (PoolAccount, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLeaseLocked(now)

	type scored struct {
		PoolAccount
		quota int
		used  time.Time
	}
	var ready []scored
	for _, a := range candidates {
		if a.Status != "" && a.Status != "active" {
			continue
		}
		if !a.Schedulable {
			continue
		}
		if _, busy := p.leased[a.ID]; busy {
			continue
		}
		if cool := readCooldown(a.Extra); !cool.IsZero() && cool.After(now) {
			continue
		}
		quota := readQuotaRemaining(a.Extra)
		used := time.Time{}
		if a.LastUsedAt != nil {
			used = *a.LastUsedAt
		}
		ready = append(ready, scored{a, quota, used})
	}
	if len(ready) == 0 {
		return PoolAccount{}, false
	}
	sort.SliceStable(ready, func(i, j int) bool {
		if ready[i].quota != ready[j].quota {
			return ready[i].quota > ready[j].quota
		}
		return ready[i].used.Before(ready[j].used)
	})
	return ready[0].PoolAccount, true
}

func (p *ImagePool) lease(accountID int64, now time.Time) ReleaseFn {
	p.mu.Lock()
	if p.leased == nil {
		p.leased = map[int64]time.Time{}
	}
	p.leased[accountID] = now.Add(2 * time.Minute)
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		delete(p.leased, accountID)
		p.mu.Unlock()
	}
}

func (p *ImagePool) gcLeaseLocked(now time.Time) {
	if p.leased == nil {
		p.leased = map[int64]time.Time{}
		return
	}
	for id, expire := range p.leased {
		if !expire.After(now) {
			delete(p.leased, id)
		}
	}
}

func (p *ImagePool) probeEarliestExpired(ctx context.Context, candidates []PoolAccount, now time.Time) bool {
	if p.Probe == nil {
		return false
	}
	type cand struct {
		acc      PoolAccount
		cooldown time.Time
	}
	var pool []cand
	for _, a := range candidates {
		if !a.Schedulable || (a.Status != "" && a.Status != "active") {
			continue
		}
		c := readCooldown(a.Extra)
		if c.IsZero() || c.After(now.Add(5*time.Minute)) {
			continue // 无 cooldown 不需要 probe；或还远未到期不强制 probe
		}
		pool = append(pool, cand{a, c})
	}
	if len(pool) == 0 {
		return false
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].cooldown.Before(pool[j].cooldown) })
	for _, c := range pool {
		if !ShouldProbeNow(c.acc.ID, now) {
			continue
		}
		_, err := p.Probe.RefreshAccount(ctx, ProbeAccount{
			ID:          c.acc.ID,
			AccessToken: c.acc.AccessToken,
			ProxyURL:    c.acc.ProxyURL,
		})
		if err == nil {
			return true
		}
	}
	return false
}

// RecordRateLimit 由 driver 命中 429 后调用：写 cooldown_until 到账号 extra。
func (p *ImagePool) RecordRateLimit(ctx context.Context, accountID int64, resetAt time.Time) error {
	if p.Probe == nil {
		return nil
	}
	return p.Probe.MarkRateLimited(ctx, accountID, resetAt)
}

// RecordSuccess 成功后调用：清空 cooldown 并扣减 quota（若已知）。
//
// 注意：实际上游 quota 由 chatgpt 端维护，这里只做本地猜测式扣减以减少
// 立即下次重选时再选到同一账号的概率；下一次 RefreshAccount 会以上游为准刷新。
func (p *ImagePool) RecordSuccess(ctx context.Context, accountID int64, currentExtra map[string]any) error {
	if p.Probe == nil || p.Probe.Repo == nil {
		return nil
	}
	updates := map[string]any{
		"image_cooldown_until":  "",
	}
	if q := readQuotaRemaining(currentExtra); q > 0 {
		updates["image_quota_remaining"] = q - 1
	}
	return p.Probe.Repo.UpdateExtra(ctx, accountID, updates)
}

// readCooldown 解析 extra.image_cooldown_until。
func readCooldown(extra map[string]any) time.Time {
	if extra == nil {
		return time.Time{}
	}
	raw, _ := extra["image_cooldown_until"].(string)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// readQuotaRemaining 解析 extra.image_quota_remaining。
// JSON 数字解为 float64；这里两种类型都接受。-1 表示未知。
func readQuotaRemaining(extra map[string]any) int {
	if extra == nil {
		return -1
	}
	raw, ok := extra["image_quota_remaining"]
	if !ok {
		return -1
	}
	switch v := raw.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return -1
}

func noopRelease() {}

// staleProbeAfter 决定 last_probed_at 多久之后视为陈旧并触发懒 probe。
const staleProbeAfter = 6 * time.Hour

// maybeStaleProbe 在 SelectAccount 选号成功后异步触发：若账号从未 probe 过，
// 或 image_last_probed_at 距今超过 staleProbeAfter，则后台拉一次刷新，让前端能看到
// image_quota_* / image_account_plan / image_last_probed_at 等字段。
//
// 不阻塞当前请求；context 用 Background() 避免 handler 取消。
// 内部依赖 ShouldProbeNow 节流，避免短时间重复打 chatgpt /me。
func (p *ImagePool) maybeStaleProbe(acc PoolAccount, now time.Time) {
	if p.Probe == nil || p.Probe.Repo == nil || acc.AccessToken == "" {
		return
	}
	probedAt, _ := acc.Extra["image_last_probed_at"].(string)
	if probedAt != "" {
		if t, err := time.Parse(time.RFC3339, probedAt); err == nil && now.Sub(t) < staleProbeAfter {
			return
		}
	}
	if !ShouldProbeNow(acc.ID, now) {
		return
	}
	go func(a PoolAccount) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := p.Probe.RefreshAccount(ctx, ProbeAccount{
			ID:          a.ID,
			AccessToken: a.AccessToken,
			ProxyURL:    a.ProxyURL,
		})
		if err != nil {
			slog.Default().Warn("openaiimages.lazy_probe_failed",
				slog.Int64("account_id", a.ID),
				slog.String("err", err.Error()))
			return
		}
		slog.Default().Info("openaiimages.lazy_probe_ok",
			slog.Int64("account_id", a.ID),
			slog.String("plan", res.AccountPlan),
			slog.Int("quota_remaining", res.QuotaRemaining))
	}(acc)
}
