package service

import (
	"sync"
	"sync/atomic"
	"time"
)

// legacyImagesInflight tracks the number of in-flight ChatGPT Web image generation
// requests per account. Used by the scheduler to prevent assigning multiple concurrent
// image jobs to the same account (image gen is serial per ChatGPT session).
//
// NOTE: This is a single-process counter. In multi-instance deployments the
// scheduler may occasionally assign the same account across instances.
// That scenario is currently considered acceptable.
var legacyImagesInflight sync.Map // map[int64]*atomic.Int32

func legacyImagesInflightCount(accountID int64) int32 {
	if v, ok := legacyImagesInflight.Load(accountID); ok {
		return v.(*atomic.Int32).Load()
	}
	return 0
}

// legacyImagesInflightTryAcquire atomically claims the image slot for an account
// (CAS 0→1). Returns true if acquired; false if another request is already in-flight.
func legacyImagesInflightTryAcquire(accountID int64) bool {
	v, _ := legacyImagesInflight.LoadOrStore(accountID, new(atomic.Int32))
	return v.(*atomic.Int32).CompareAndSwap(0, 1)
}

func legacyImagesInflightRelease(accountID int64) {
	if v, ok := legacyImagesInflight.Load(accountID); ok {
		v.(*atomic.Int32).Add(-1)
	}
}

// legacyImagesUsageCacheTTL 决定调度路径上每个账号的 24h 用量缓存有效期。
// 60s 是 ChatGPT Web 默认刷新节奏的下限，缓存到期后下次调度会自然回源；
// 同时 forwardOpenAIImagesOAuthLegacy 在请求成功后会主动 bump（见 bumpAccount）。
const legacyImagesUsageCacheTTL = 60 * time.Second

type legacyImagesUsageCacheEntry struct {
	value     int
	expiresAt time.Time
}

// legacyImagesUsageCacheStore 进程内缓存账号 24h 已成功生成图片数量。
// 调度热路径每秒可能多次命中同一账号，60s TTL 足够避免 DB 压力又能在合理时间内反映新数据。
type legacyImagesUsageCacheStore struct {
	mu      sync.Mutex
	entries map[int64]legacyImagesUsageCacheEntry
}

var legacyImagesUsageCache = &legacyImagesUsageCacheStore{
	entries: make(map[int64]legacyImagesUsageCacheEntry),
}

func (c *legacyImagesUsageCacheStore) get(accountID int64, loader func() int) int {
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.entries[accountID]; ok && now.Before(e.expiresAt) {
		c.mu.Unlock()
		return e.value
	}
	c.mu.Unlock()
	v := loader()
	c.mu.Lock()
	defer c.mu.Unlock()
	// Don't overwrite a bumpAccount write that happened while loader was running.
	// Take max(db_value, cached_value) so neither the DB total nor the bump is lost.
	if e, ok := c.entries[accountID]; ok && now.Before(e.expiresAt) {
		if e.value >= v {
			return e.value
		}
		c.entries[accountID] = legacyImagesUsageCacheEntry{value: v, expiresAt: e.expiresAt}
		return v
	}
	c.entries[accountID] = legacyImagesUsageCacheEntry{value: v, expiresAt: now.Add(legacyImagesUsageCacheTTL)}
	return v
}

// bumpAccount 在请求成功后主动 +n，避免「窗口刚到 quota-1，缓存还在」导致放行 1 张超额请求。
// n 通常是新生成的图片数量。
func (c *legacyImagesUsageCacheStore) bumpAccount(accountID int64, n int) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	e, ok := c.entries[accountID]
	if !ok || now.After(e.expiresAt) {
		// 缓存已过期/缺失：生成完成时图片已写入 DB，直接删除让下次 get() 回源重读真实总数。
		// 写入 n=1 做兜底会导致 quota 检查永远看到 1 而非累计值，属于误报宽松。
		delete(c.entries, accountID)
		return
	}
	e.value += n
	c.entries[accountID] = e
}

// peek 仅读缓存；命中且未过期返回 (value, true)，否则 (0, false)。供批量查询路径使用。
func (c *legacyImagesUsageCacheStore) peek(accountID int64, now time.Time) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[accountID]; ok && now.Before(e.expiresAt) {
		return e.value, true
	}
	return 0, false
}

// setBaseline 写入回源得到的基准值（重置过期时间）。用于批量回源后的回填。
func (c *legacyImagesUsageCacheStore) setBaseline(accountID int64, value int, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[accountID] = legacyImagesUsageCacheEntry{value: value, expiresAt: now.Add(legacyImagesUsageCacheTTL)}
}

// invalidateAccount 主动失效缓存（管理员重置等场景）。
func (c *legacyImagesUsageCacheStore) invalidateAccount(accountID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, accountID)
}

// normalizeLegacyImagesDailyQuota 把 < 0 的输入归一化为 0（不限）。
// 0 仍然表示「不限制」，保持与 ent default(3) 不冲突——管理员可显式 set 0。
func normalizeLegacyImagesDailyQuota(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
