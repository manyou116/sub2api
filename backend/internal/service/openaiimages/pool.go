package openaiimages

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
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
	List  PoolListAccounts
	Probe *AccountProbe
	Now   func() time.Time

	// TopKPick 在排序后从前 K 个候选中随机挑 1 个，用于把负载分散到多账号上避免热点。
	// <=1 退化为强单调（旧行为）；生产建议 10。
	TopKPick int

	// ExploreUnknownProb 给 quota=0 / 未探测过的账号一定概率被选中，让几千账号池
	// 真正"流动"起来，避免少数已 probe 账号被反复打。范围 [0, 1]，<=0 关闭。
	// 生产建议 0.05（5%）。
	ExploreUnknownProb float64

	// Rand 可注入测试随机源；nil 时按需用全局 rand。
	Rand *rand.Rand

	// WaitMaxFraction：SelectAccount 第一次 pick 失败时，最多等多久（占 ctx 剩余时间的比例）让其他请求 release lease。
	// 范围 (0, 1)；<=0 关闭 wait（保留旧行为：直接返回 ErrNoImageAccount）。生产建议 0.5。
	WaitMaxFraction float64
	// WaitMaxDuration：wait 的绝对上限，避免 ctx 巨大时无限等。<=0 表示无绝对上限（仅受 fraction/ctx 限制）。
	// 生产建议 30s。
	WaitMaxDuration time.Duration

	mu      sync.Mutex
	leased  map[int64]time.Time // accountID → lease 到期时刻
	waiters []*poolWaiter       // FIFO 等待队列：lease release 时按顺序唤醒
}

// poolWaiter 表示一个等待 lease 释放的 SelectAccount 调用。
// ch 用 buffered(1)，唤醒方非阻塞 send；done 标记 waiter 已离开（超时/ctx 取消），唤醒时跳过。
type poolWaiter struct {
	ch   chan struct{}
	done bool
}

func (p *ImagePool) intn(n int) int {
	if n <= 1 {
		return 0
	}
	if p.Rand != nil {
		return p.Rand.Intn(n)
	}
	return rand.Intn(n)
}

func (p *ImagePool) float64Rand() float64 {
	if p.Rand != nil {
		return p.Rand.Float64()
	}
	return rand.Float64()
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
//  3. 按 (image_quota_remaining DESC, last_used_at ASC) 排序 + top-K 随机
//  4. 取一个并加 2min lease（防 SSE 阶段并发重选 / dispatch 卡死兜底）
//
// 若全部账号在 cooldown，对最近过期的账号触发一次 probe 后重试。
// 若 ready 候选都被 lease 占用且 WaitMaxFraction>0，进入 FIFO 等待队列，
// 等其它请求 release（或超时/ctx 取消），避免突发并发瞬时 503。
func (p *ImagePool) SelectAccount(ctx context.Context, filter PoolFilter) (PoolAccount, ReleaseFn, error) {
	candidates, err := p.List(ctx, filter)
	if err != nil {
		return PoolAccount{}, noopRelease, err
	}
	now := p.now()

	if picked, ok := p.pickLocked(candidates, now); ok {
		p.maybeStaleProbe(picked, now)
		return picked, p.lease(picked.ID, now), nil
	}

	// 全部 cooldown：找到 cooldown 最早过期的账号补 probe（异步可能不及时，故同步）
	if probed := p.probeEarliestExpired(ctx, candidates, now); probed {
		candidates, err = p.List(ctx, filter)
		if err == nil {
			now = p.now()
			if picked, ok := p.pickLocked(candidates, now); ok {
				p.maybeStaleProbe(picked, now)
				return picked, p.lease(picked.ID, now), nil
			}
		}
	}

	// 等待队列：若启用 WaitMaxFraction 且池子里"如果没 lease 就有候选"，等其它请求 release。
	if p.WaitMaxFraction > 0 && p.hasLeaseBlockedCandidate(candidates, p.now()) {
		picked, ok := p.waitForLease(ctx, filter)
		if ok {
			now = p.now()
			p.maybeStaleProbe(picked, now)
			return picked, p.lease(picked.ID, now), nil
		}
	}

	return PoolAccount{}, noopRelease, ErrNoImageAccount
}

// hasLeaseBlockedCandidate 判断：是否存在"被 lease 挡掉但本来 ready 的账号"。
// 只有这种情况才值得排队等 release；如果池子里全是 cooldown / inactive，等也没用。
func (p *ImagePool) hasLeaseBlockedCandidate(candidates []PoolAccount, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLeaseLocked(now)
	for _, a := range candidates {
		if (a.Status != "" && a.Status != "active") || !a.Schedulable {
			continue
		}
		if cool := readCooldown(a.Extra); !cool.IsZero() && cool.After(now) {
			continue
		}
		if _, busy := p.leased[a.ID]; busy {
			return true
		}
	}
	return false
}

// waitForLease 注册 FIFO waiter，等待其它请求 release lease 后重新 pick。
// 返回 (account, true) 表示成功；(_, false) 表示超时 / ctx 取消 / 池子已空。
func (p *ImagePool) waitForLease(ctx context.Context, filter PoolFilter) (PoolAccount, bool) {
	maxWait := p.computeMaxWait(ctx)
	if maxWait <= 0 {
		return PoolAccount{}, false
	}
	deadline := p.now().Add(maxWait)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return PoolAccount{}, false
		}

		w := p.enqueueWaiter()
		timer := time.NewTimer(remaining)
		select {
		case <-w.ch:
			timer.Stop()
		case <-timer.C:
			p.markWaiterDone(w)
			return PoolAccount{}, false
		case <-ctx.Done():
			p.markWaiterDone(w)
			timer.Stop()
			return PoolAccount{}, false
		}

		// 被唤醒后重新拉一次 list（last_used_at / extra 可能变了）
		candidates, err := p.List(ctx, filter)
		if err != nil {
			return PoolAccount{}, false
		}
		if picked, ok := p.pickLocked(candidates, p.now()); ok {
			return picked, true
		}
		// 被唤醒但仍抢不到（其它 waiter 抢先 / 新 lease 立刻到来）→ 继续等
	}
}

func (p *ImagePool) computeMaxWait(ctx context.Context) time.Duration {
	frac := p.WaitMaxFraction
	if frac <= 0 {
		return 0
	}
	if frac > 1 {
		frac = 1
	}
	// 默认上限：避免 ctx 没设 deadline 时无限等。
	upper := p.WaitMaxDuration
	if upper <= 0 {
		upper = 30 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining <= 0 {
			return 0
		}
		w := time.Duration(float64(remaining) * frac)
		if w > upper {
			w = upper
		}
		return w
	}
	return upper
}

func (p *ImagePool) enqueueWaiter() *poolWaiter {
	w := &poolWaiter{ch: make(chan struct{}, 1)}
	p.mu.Lock()
	p.waiters = append(p.waiters, w)
	p.mu.Unlock()
	return w
}

func (p *ImagePool) markWaiterDone(w *poolWaiter) {
	p.mu.Lock()
	w.done = true
	// 如果 chan 已有信号（race：release 唤醒到了同一时刻），转发给下一个 waiter
	select {
	case <-w.ch:
		p.wakeOneLocked()
	default:
	}
	p.mu.Unlock()
}

// wakeOneLocked 在持锁状态下唤醒队首一个未 done 的 waiter；调用方持 p.mu。
func (p *ImagePool) wakeOneLocked() {
	for len(p.waiters) > 0 {
		w := p.waiters[0]
		p.waiters = p.waiters[1:]
		if w.done {
			continue
		}
		select {
		case w.ch <- struct{}{}:
		default:
			// chan 已有信号（不应该，但兜底）
		}
		return
	}
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
	var known, unknown []scored // known: quota 已探测 (>0)；unknown: quota=0 / 未 probe (-1)
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
		s := scored{a, quota, used}
		if quota > 0 {
			known = append(known, s)
		} else {
			unknown = append(unknown, s)
		}
	}
	if len(known) == 0 && len(unknown) == 0 {
		return PoolAccount{}, false
	}

	// 二级梯队：以 ExploreUnknownProb 概率从未探测账号里挑（"自然探测"）。
	// 池里只有 unknown 时直接走 unknown 分支。
	useUnknown := len(known) == 0 || (len(unknown) > 0 && p.ExploreUnknownProb > 0 && p.float64Rand() < p.ExploreUnknownProb)

	pickFrom := known
	sortByQuota := true
	if useUnknown {
		pickFrom = unknown
		sortByQuota = false
	}

	sort.SliceStable(pickFrom, func(i, j int) bool {
		if sortByQuota && pickFrom[i].quota != pickFrom[j].quota {
			return pickFrom[i].quota > pickFrom[j].quota
		}
		return pickFrom[i].used.Before(pickFrom[j].used)
	})

	// top-K 随机：在前 K 个里随机挑 1 个，避免 ready[0] 永远被打。
	// TopKPick <= 0 退化为强单调（旧行为），便于测试与渐进上线。
	idx := 0
	if p.TopKPick > 1 {
		upper := p.TopKPick
		if upper > len(pickFrom) {
			upper = len(pickFrom)
		}
		if upper > 1 {
			idx = p.intn(upper)
		}
	}
	return pickFrom[idx].PoolAccount, true
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
		p.wakeOneLocked()
		p.mu.Unlock()
	}
}

func (p *ImagePool) gcLeaseLocked(now time.Time) {
	if p.leased == nil {
		p.leased = map[int64]time.Time{}
		return
	}
	expired := 0
	for id, expire := range p.leased {
		if !expire.After(now) {
			delete(p.leased, id)
			expired++
		}
	}
	// 兜底过期也应该唤醒等待方（dispatch 卡死场景）
	for i := 0; i < expired; i++ {
		p.wakeOneLocked()
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
		"image_cooldown_until": "",
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

// PoolStats 描述当前 pool 的健康度，供 handler 周期日志/可观测使用。
type PoolStats struct {
	Total    int // 候选账号总数（已 active+schedulable 过滤）
	Ready    int // quota>0 且未 leased / 未 cooldown 的账号
	Unknown  int // quota=0 / 未探测的账号（可被 ExploreUnknownProb 自然探测）
	Cooldown int // 仍在 cooldown 的账号
	Leased   int // 当前持有 lease 的账号
	Inactive int // status!=active 或 !schedulable
}

// Stats 拉一次候选并统计当前池子分布。仅用于观测，可重型。
func (p *ImagePool) Stats(ctx context.Context, filter PoolFilter) (PoolStats, error) {
	candidates, err := p.List(ctx, filter)
	if err != nil {
		return PoolStats{}, err
	}
	now := p.now()

	p.mu.Lock()
	p.gcLeaseLocked(now)
	leasedSnapshot := make(map[int64]struct{}, len(p.leased))
	for id := range p.leased {
		leasedSnapshot[id] = struct{}{}
	}
	p.mu.Unlock()

	st := PoolStats{Total: len(candidates)}
	for _, a := range candidates {
		if (a.Status != "" && a.Status != "active") || !a.Schedulable {
			st.Inactive++
			continue
		}
		if _, busy := leasedSnapshot[a.ID]; busy {
			st.Leased++
			continue
		}
		if cool := readCooldown(a.Extra); !cool.IsZero() && cool.After(now) {
			st.Cooldown++
			continue
		}
		if readQuotaRemaining(a.Extra) > 0 {
			st.Ready++
		} else {
			st.Unknown++
		}
	}
	return st, nil
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
