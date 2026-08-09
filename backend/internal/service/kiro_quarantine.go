// Kiro 账号 / (账号,模型) 双层隔离。
//
// **背景**：Kiro 上游返回错误时按归属维度区分处理：
//
//	错误类型               隔离维度        Cooldown 策略
//	-----------------------------------------------------
//	ModelCapacity          (account,model) 指数退避（30→60→120→300s）
//	AccountQuotaDaily      account         至次日 00:00 UTC
//	AccountQuotaMonthly    account         至下月 1 日（DB 持久化，见 account_repo）
//	AccountSuspended       account         DB persist banned
//	AccessDenied           account         30min
//	Auth                   account         5min（先 ForceRefresh 兜底）
//	ConversationTooLong    —              不隔离（原样回客户端）
//	InvalidRequest         —              不隔离（原样回客户端）
//	Transient              —              不隔离（仅本次切号）
//
// **设计要点**：
//   - 模型级 (account,model) → 指数退避；成功一次清零 attempts
//   - 账号级 → 固定时长 cooldown
//   - 当前为进程内 in-memory cooldown（重启丢失）；DB 持久化尚未做
//   - 客户端续命循环防御：同 (account,model) 1min 内 >5 次命中
//     ModelCapacity → 提前透传，不切号（避免拉满所有账号）
package service

import (
	"sync"
	"time"
)

// === Cooldown 默认值 ===
const (
	kiroQuarantineAccountAccessDenied = 30 * time.Minute
	kiroQuarantineAccountAuth         = 5 * time.Minute
	kiroQuarantineAccountTransient    = 60 * time.Second // ModelCapacity 无 model 时的账号级兜底

	kiroModelCapacityBaseDelay         = 30 * time.Second
	kiroModelCapacityMaxDelay          = 5 * time.Minute
	kiroModelCapacityResetAfter        = 10 * time.Minute // attempts 多久不命中后清零
	kiroModelCapacityClientFloodN      = 5
	kiroModelCapacityClientFloodWindow = time.Minute
)

// === in-memory 状态 ===

// 账号级 cooldown：accountID(int64) → notBefore(time.Time)
var kiroAccountQuarantineMap sync.Map

// 模型级状态：(accountID, model) → *kiroModelState
var kiroModelQuarantineMap sync.Map

type kiroModelState struct {
	mu         sync.Mutex
	notBefore  time.Time
	attempts   int         // 连续 ModelCapacity 命中次数（决定退避指数）
	lastHit    time.Time   // 最近一次命中
	clientHits []time.Time // 最近 N 次命中（防客户端续命循环）
}

type kiroModelKey struct {
	AccountID int64
	Model     string
}

// === 账号级 API ===

// IsKiroAccountQuarantined 账号级 cooldown 检查（已过期自动清理）。
func IsKiroAccountQuarantined(accountID int64) bool {
	v, ok := kiroAccountQuarantineMap.Load(accountID)
	if !ok {
		return false
	}
	notBefore, ok := v.(time.Time)
	if !ok {
		kiroAccountQuarantineMap.Delete(accountID)
		return false
	}
	if time.Now().After(notBefore) {
		kiroAccountQuarantineMap.Delete(accountID)
		return false
	}
	return true
}

// QuarantineKiroAccount 账号级隔离，duration<=0 视为不隔离。
func QuarantineKiroAccount(accountID int64, duration time.Duration) {
	if duration <= 0 {
		return
	}
	kiroAccountQuarantineMap.Store(accountID, time.Now().Add(duration))
}

// ClearKiroQuarantine 清除账号级 cooldown（同时清掉该账号所有模型级状态）。
func ClearKiroQuarantine(accountID int64) {
	kiroAccountQuarantineMap.Delete(accountID)
	kiroModelQuarantineMap.Range(func(k, _ any) bool {
		if mk, ok := k.(kiroModelKey); ok && mk.AccountID == accountID {
			kiroModelQuarantineMap.Delete(k)
		}
		return true
	})
}

// === 模型级 API ===

// IsKiroModelQuarantined (账号,模型) 维度 cooldown 检查。
func IsKiroModelQuarantined(accountID int64, model string) bool {
	v, ok := kiroModelQuarantineMap.Load(kiroModelKey{accountID, model})
	if !ok {
		return false
	}
	st, ok := v.(*kiroModelState)
	if !ok {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return time.Now().Before(st.notBefore)
}

// HitModelCapacity 记录一次 ModelCapacity 命中，返回 (cooldown, isClientFlood)。
//
// isClientFlood = true 表示 1 分钟内同 (account,model) >N 次命中，
// 上层应直接透传 429 给客户端而不再切号。
func HitModelCapacity(accountID int64, model string) (time.Duration, bool) {
	key := kiroModelKey{accountID, model}
	v, _ := kiroModelQuarantineMap.LoadOrStore(key, &kiroModelState{})
	st, ok := v.(*kiroModelState)
	if !ok || st == nil {
		return 0, false
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	now := time.Now()

	// 长时间未命中 → attempts 清零
	if !st.lastHit.IsZero() && now.Sub(st.lastHit) > kiroModelCapacityResetAfter {
		st.attempts = 0
		st.clientHits = st.clientHits[:0]
	}

	st.attempts++
	st.lastHit = now

	// 指数退避：base * 2^(attempts-1)，封顶 max
	delay := kiroModelCapacityBaseDelay << (st.attempts - 1)
	if delay > kiroModelCapacityMaxDelay || delay <= 0 {
		delay = kiroModelCapacityMaxDelay
	}
	st.notBefore = now.Add(delay)

	// 客户端 flood 检测：保留窗口内时间戳
	cutoff := now.Add(-kiroModelCapacityClientFloodWindow)
	kept := st.clientHits[:0]
	for _, t := range st.clientHits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	st.clientHits = kept
	flood := len(kept) >= kiroModelCapacityClientFloodN

	return delay, flood
}

// ClearKiroModelCapacity 成功一次清零（在 chat 200 响应后调用）。
func ClearKiroModelCapacity(accountID int64, model string) {
	kiroModelQuarantineMap.Delete(kiroModelKey{accountID, model})
}

// === 错误归一入口 ===

// QuarantineDecision 处理结果，handler 据此决定客户端响应与切号。
type QuarantineDecision struct {
	Class           KiroErrorClass
	AccountCooldown time.Duration // >0 = 账号级隔离时长
	ModelCooldown   time.Duration // >0 = 模型级隔离时长
	ShouldFailover  bool          // true = 切其他账号；false = 直接回客户端
	PassthroughBody bool          // true = 用上游原始 body 回客户端
	ClientFlood     bool          // true = 客户端在反复重试，建议直接透传
}

// HandleKiroUpstreamError 综合分类 + 应用隔离 + 返回处理决策。
//
// model 参数为本次请求的目标模型（已是 Kiro 内部模型 ID，如 claude-opus-4.7）。
// 若 model == ""（路由前出错等），ModelCapacity 退化为账号级 60s cooldown。
func HandleKiroUpstreamError(accountID int64, model string, statusCode int, body []byte) QuarantineDecision {
	class := ClassifyKiroError(statusCode, body)
	d := QuarantineDecision{Class: class, ShouldFailover: true}

	switch class {
	case KiroErrModelCapacity:
		if model != "" {
			delay, flood := HitModelCapacity(accountID, model)
			d.ModelCooldown = delay
			d.ClientFlood = flood
			// flood 时透传给客户端，不再切号（避免一个客户端拖垮所有账号）
			if flood {
				d.ShouldFailover = false
				d.PassthroughBody = true
			}
		} else {
			d.AccountCooldown = kiroQuarantineAccountTransient
			QuarantineKiroAccount(accountID, d.AccountCooldown)
		}

	case KiroErrAccountQuotaDaily:
		// 至次日 00:00 UTC
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		d.AccountCooldown = next.Sub(now)
		QuarantineKiroAccount(accountID, d.AccountCooldown)

	case KiroErrAccountQuotaMonthly:
		// 至下月 1 日 00:00 UTC（in-memory）
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		d.AccountCooldown = next.Sub(now)
		QuarantineKiroAccount(accountID, d.AccountCooldown)

	case KiroErrAccountSuspended:
		// 临时封禁：先给 30min in-memory 冷却
		d.AccountCooldown = kiroQuarantineAccountAccessDenied
		QuarantineKiroAccount(accountID, d.AccountCooldown)

	case KiroErrAccessDenied:
		d.AccountCooldown = kiroQuarantineAccountAccessDenied
		QuarantineKiroAccount(accountID, d.AccountCooldown)

	case KiroErrAuth:
		// ForceRefresh 由 chat_service 完成；这里只在最终失败兜底
		d.AccountCooldown = kiroQuarantineAccountAuth
		QuarantineKiroAccount(accountID, d.AccountCooldown)

	case KiroErrConversationTooLong, KiroErrInvalidRequest:
		// 客户端 bug，原样回，不切号也不隔离
		d.ShouldFailover = false
		d.PassthroughBody = true

	case KiroErrTransient, KiroErrUnknown:
		// 切号本次重试，不隔离
	}

	return d
}
