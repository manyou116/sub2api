// Package service provides business logic and domain services for the application.
package service

import (
	"context"
	"errors"
	"math/rand"
	"sort"
)

// SelectKiroAccount 为 KiroGatewayHandler 选一个可用的 Kiro 账号。
//
// 调度策略（按优先级递减）：
//  1. schedulable + IsKiro
//  2. 排除 quarantine 与 excludedIDs
//  3. 排除 KiroIsCapped（已耗尽配额且无 overage）
//  4. 按 (Account.Priority desc, KiroRemainingQuota desc) 排序
//  5. 同等关键字内随机打散，避免雪崩集中第一个账号
//  6. 占用 concurrency slot；满则返回 wait plan
func (s *OpenAIGatewayService) SelectKiroAccount(
	ctx context.Context,
	groupID *int64,
	excludedIDs map[int64]struct{},
	model string,
) (*AccountSelectionResult, error) {
	if s.accountRepo == nil {
		return nil, errors.New("account repo not initialized")
	}

	var (
		raw []Account
		err error
	)
	if groupID != nil {
		raw, err = s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformKiro)
	} else {
		raw, err = s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, PlatformKiro)
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, ErrNoAvailableAccounts
	}

	// 过滤可调度账号
	candidates := make([]*Account, 0, len(raw))
	for i := range raw {
		acct := &raw[i]
		if !acct.IsSchedulable() || !acct.IsKiro() {
			continue
		}
		if IsKiroQuarantined(acct.ID) {
			continue
		}
		if model != "" && IsKiroModelQuarantined(acct.ID, model) {
			continue
		}
		if model != "" && !acct.IsModelSupported(model) {
			continue
		}
		if excludedIDs != nil {
			if _, skip := excludedIDs[acct.ID]; skip {
				continue
			}
		}
		if acct.KiroIsCapped() {
			continue
		}
		candidates = append(candidates, acct)
	}
	if len(candidates) == 0 {
		return nil, ErrNoAvailableAccounts
	}

	// 同优先级内随机打散，避免每次都打第一个
	rand.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		// 剩余配额降序
		return candidates[i].KiroRemainingQuota() > candidates[j].KiroRemainingQuota()
	})

	cfg := s.schedulingConfig()
	// 候选已按剩余配额降序排序；这里只对配额最高的账号尝试一次 acquire，
	// 失败则返回该账号的 wait plan，由上层决定排队 vs 切下一账号。
	if len(candidates) == 0 {
		return nil, ErrNoAvailableAccounts
	}
	acct := candidates[0]
	result, err := s.tryAcquireAccountSlot(ctx, acct.ID, acct.Concurrency)
	if err == nil && result.Acquired {
		return s.newSelectionResult(ctx, acct, true, result.ReleaseFunc, nil)
	}
	return s.newSelectionResult(ctx, acct, false, nil, &AccountWaitPlan{
		AccountID:      acct.ID,
		MaxConcurrency: acct.Concurrency,
		Timeout:        cfg.FallbackWaitTimeout,
		MaxWaiting:     cfg.FallbackMaxWaiting,
	})
}
