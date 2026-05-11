package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

const (
	// kiroTokenRefreshSkew 距离 expires_at 还剩多少时主动 refresh
	// 与 OpenAI 一致：3 分钟（足够覆盖一次完整请求 + 重试）
	kiroTokenRefreshSkew = 3 * time.Minute
)

// KiroTokenProvider 提供按需获取有效 Kiro access_token 的能力。
//
// 与 OpenAITokenProvider 相比，当前 Kiro 实现轻量化：
//   - 不引入 token cache 层（直接落 DB；Kiro 请求频次远低于 OpenAI）
//   - 复用 OAuthRefreshAPI 的分布式锁 + DB 重读 + 竞争恢复
//   - 提供两种入口：EnsureFreshToken（按需，过期前 skew 内才刷）
//     ForceRefresh（401/403 兜底，强制刷一次）
type KiroTokenProvider struct {
	accountRepo AccountRepository
	refreshAPI  *OAuthRefreshAPI
	executor    OAuthRefreshExecutor // KiroTokenRefresher
}

// NewKiroTokenProvider 构造 provider
func NewKiroTokenProvider(accountRepo AccountRepository, refreshAPI *OAuthRefreshAPI, executor OAuthRefreshExecutor) *KiroTokenProvider {
	return &KiroTokenProvider{
		accountRepo: accountRepo,
		refreshAPI:  refreshAPI,
		executor:    executor,
	}
}

// EnsureFreshToken 返回有效的 access_token。
//
// 行为：
//  1. 检查 expires_at，若距过期 < skew 则触发 refresh
//  2. refresh 在分布式锁保护下执行，多副本/多 goroutine 不会重复刷
//  3. 锁被持有时（其他 worker 在刷）直接复用 DB 重读到的最新 token
//  4. refresh 失败时返回错误（调用方决定 cooldown / 重试 / 换号）
//
// 返回值：access_token 字符串。失败时 token 为空、err 非空。
func (p *KiroTokenProvider) EnsureFreshToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("kiro_provider: account is nil")
	}
	if !account.IsKiro() {
		return "", errors.New("kiro_provider: not a kiro account")
	}

	// 1. 优先按 expires_at 判断是否需要预刷
	if p.refreshAPI != nil && p.executor != nil && p.executor.NeedsRefresh(account, kiroTokenRefreshSkew) {
		result, err := p.refreshAPI.RefreshIfNeeded(ctx, account, p.executor, kiroTokenRefreshSkew)
		if err != nil {
			slog.Warn("kiro_token_refresh_failed",
				"account_id", account.ID,
				"error", err,
			)
			return "", err
		}
		if result != nil && result.Account != nil {
			account = result.Account
		}
	}

	token := strings.TrimSpace(account.KiroAccessToken())
	if token == "" {
		return "", errors.New("kiro_provider: access_token empty after refresh")
	}
	return token, nil
}

// ForceRefresh 强制刷新一次（用于 401/403 invalid_token 的按需兜底）。
//
// 与 EnsureFreshToken 区别：忽略 expires_at 检查，无条件请求 refresh。
// 仍走分布式锁，多副本同时收到 401 也只刷一次。
//
// 返回 (新 access_token, 刷新后的 account, error)。
// 调用方应该用返回的 account 重发请求（其内部 access_token 已是新的）。
func (p *KiroTokenProvider) ForceRefresh(ctx context.Context, account *Account) (string, *Account, error) {
	if account == nil {
		return "", nil, errors.New("kiro_provider: account is nil")
	}
	if !account.IsKiro() {
		return "", nil, errors.New("kiro_provider: not a kiro account")
	}
	if p.refreshAPI == nil || p.executor == nil {
		return "", account, errors.New("kiro_provider: refresh api/executor not configured")
	}

	// 用一个非常大的窗口（24h）让 NeedsRefresh 一定返回 true，
	// 即使 token 显示未过期也强制刷。
	const forceWindow = 24 * time.Hour

	// RefreshIfNeeded 内部会拿锁、DB 重读、二次检查；
	// 二次检查用同样大窗口确保不被跳过。
	result, err := p.refreshAPI.RefreshIfNeeded(ctx, account, &alwaysNeedsRefresh{p.executor}, forceWindow)
	if err != nil {
		slog.Warn("kiro_token_force_refresh_failed",
			"account_id", account.ID,
			"error", err,
		)
		return "", account, err
	}

	finalAccount := account
	if result != nil && result.Account != nil {
		finalAccount = result.Account
	}

	token := strings.TrimSpace(finalAccount.KiroAccessToken())
	if token == "" {
		return "", finalAccount, errors.New("kiro_provider: access_token empty after force refresh")
	}
	return token, finalAccount, nil
}

// alwaysNeedsRefresh 包装 executor，让 NeedsRefresh 始终返回 true。
// 用于 ForceRefresh 场景（401/403 兜底，无视 expires_at）。
type alwaysNeedsRefresh struct {
	OAuthRefreshExecutor
}

func (a *alwaysNeedsRefresh) NeedsRefresh(_ *Account, _ time.Duration) bool {
	return true
}
