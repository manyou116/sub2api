package service

import (
	"context"
	"time"
)

// KiroTokenRefresher 处理 Kiro OAuth token 自动刷新。
//
// 实现 TokenRefresher + OAuthRefreshExecutor 接口，被两处使用：
//   1. TokenRefreshService 后台 worker：周期扫描快过期的 active 账号，提前 refresh
//   2. OAuthRefreshAPI：按需 refresh（带分布式锁，避免多副本竞态）
//
// 与 Claude/OpenAI 一致的策略：
//   - 仅处理 Kiro 平台账号
//   - expires_at 缺失则不主动 refresh（avoid 浪费一次性 refresh_token）
//     （注：Social/IdC 流程都返回 expires_in，账号导入即写入，正常账号都有）
type KiroTokenRefresher struct {
	tokenSvc *KiroTokenService
}

// NewKiroTokenRefresher 创建 Kiro token 刷新器
func NewKiroTokenRefresher(tokenSvc *KiroTokenService) *KiroTokenRefresher {
	if tokenSvc == nil {
		tokenSvc = NewKiroTokenService()
	}
	return &KiroTokenRefresher{tokenSvc: tokenSvc}
}

// CacheKey 返回用于分布式锁的缓存键
func (r *KiroTokenRefresher) CacheKey(account *Account) string {
	return KiroTokenCacheKey(account)
}

// CanRefresh 仅处理 Kiro 平台账号
func (r *KiroTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && account.IsKiro()
}

// NeedsRefresh 基于 credentials.expires_at（unix 秒）判断是否在刷新窗口内
func (r *KiroTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if account == nil {
		return false
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		// 历史账号缺 expires_at，无法判断，保守不主动刷
		// 仍可被 401/403 invalid_token 触发的按需 refresh 救回
		return false
	}
	return time.Until(*expiresAt) < refreshWindow
}

// Refresh 执行 token 刷新。保留原有 credentials 中的所有字段，只更新 token 相关字段。
//
// 注意：当前不传 proxyURL（账号绑定 proxy 由调用方在更高层处理）。
// 如未来需要按账号 proxy 刷新，可在此通过 ProxyRepo 解析。
func (r *KiroTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	tokenInfo, err := r.tokenSvc.RefreshAccountToken(ctx, account, "")
	if err != nil {
		return nil, err
	}
	// ApplyKiroTokenInfo 已经做了 merge（保留旧字段+覆盖新 token）
	return ApplyKiroTokenInfo(account, tokenInfo), nil
}
