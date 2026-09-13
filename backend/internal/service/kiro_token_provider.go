package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

const (
	kiroTokenCacheSkew        = 5 * time.Minute
	kiroTokenRefreshSkew      = 3 * time.Minute
	kiroRequestRefreshTimeout = 8 * time.Second
)

// KiroTokenCache reuses the shared access-token cache interface.
type KiroTokenCache = GeminiTokenCache

// KiroTokenProvider provides on-demand Kiro access tokens with Redis caching
// and OAuthRefreshAPI-backed refresh (distributed lock + DB reread).
type KiroTokenProvider struct {
	accountRepo AccountRepository
	tokenCache  KiroTokenCache
	refreshAPI  *OAuthRefreshAPI
	executor    OAuthRefreshExecutor
}

// NewKiroTokenProvider constructs a provider. Call SetRefreshAPI after wire.
func NewKiroTokenProvider(accountRepo AccountRepository, tokenCache KiroTokenCache) *KiroTokenProvider {
	return &KiroTokenProvider{
		accountRepo: accountRepo,
		tokenCache:  tokenCache,
	}
}

// SetRefreshAPI injects the shared OAuth refresh API and Kiro executor.
func (p *KiroTokenProvider) SetRefreshAPI(api *OAuthRefreshAPI, executor OAuthRefreshExecutor) {
	p.refreshAPI = api
	p.executor = executor
}

// EnsureFreshToken returns a usable access_token, refreshing when near expiry.
func (p *KiroTokenProvider) EnsureFreshToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("kiro_provider: account is nil")
	}
	if !account.IsKiro() {
		return "", errors.New("kiro_provider: not a kiro account")
	}

	cacheKey := KiroTokenCacheKey(account)
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
			return token, nil
		}
	}

	if p.refreshAPI != nil && p.executor != nil && p.executor.NeedsRefresh(account, kiroTokenRefreshSkew) {
		refreshCtx, cancel := context.WithTimeout(ctx, kiroRequestRefreshTimeout)
		defer cancel()
		result, err := p.refreshAPI.RefreshIfNeeded(refreshCtx, account, p.executor, kiroTokenRefreshSkew)
		if err != nil {
			slog.Warn("kiro_token_refresh_failed", "account_id", account.ID, "error", err)
			return "", err
		}
		if result != nil {
			if result.LockHeld && p.accountRepo != nil {
				if latest, err := p.accountRepo.GetByID(ctx, account.ID); err == nil && latest != nil {
					account = latest
				}
			} else if result.Account != nil {
				account = result.Account
			}
		}
	}

	token := strings.TrimSpace(account.KiroAccessToken())
	if token == "" {
		return "", errors.New("kiro_provider: access_token empty after refresh")
	}
	p.cacheToken(ctx, cacheKey, account, token)
	return token, nil
}

// ForceRefresh forces a token refresh (401/403 recovery path).
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

	const forceWindow = 24 * time.Hour
	refreshCtx, cancel := context.WithTimeout(ctx, kiroRequestRefreshTimeout)
	defer cancel()
	result, err := p.refreshAPI.RefreshIfNeeded(refreshCtx, account, &kiroAlwaysNeedsRefresh{p.executor}, forceWindow)
	if err != nil {
		slog.Warn("kiro_token_force_refresh_failed", "account_id", account.ID, "error", err)
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
	p.cacheToken(ctx, KiroTokenCacheKey(finalAccount), finalAccount, token)
	return token, finalAccount, nil
}

func (p *KiroTokenProvider) cacheToken(ctx context.Context, cacheKey string, account *Account, token string) {
	if p.tokenCache == nil || strings.TrimSpace(token) == "" {
		return
	}
	ttl := 50 * time.Minute
	if expiresAt := account.GetCredentialAsTime("expires_at"); expiresAt != nil {
		until := time.Until(*expiresAt)
		switch {
		case until > kiroTokenCacheSkew:
			ttl = until - kiroTokenCacheSkew
		case until > 0:
			ttl = until
		default:
			return
		}
	}
	_ = p.tokenCache.SetAccessToken(ctx, cacheKey, token, ttl)
}

type kiroAlwaysNeedsRefresh struct {
	OAuthRefreshExecutor
}

func (a *kiroAlwaysNeedsRefresh) NeedsRefresh(_ *Account, _ time.Duration) bool {
	return true
}
