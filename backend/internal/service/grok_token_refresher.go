package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

const grokTokenRefreshSkew = 5 * time.Minute

type GrokTokenRefresher struct {
	grokOAuthService GrokOAuthTokenService
	refreshGroup     singleflight.Group
}

func NewGrokTokenRefresher(grokOAuthService GrokOAuthTokenService) *GrokTokenRefresher {
	return &GrokTokenRefresher{grokOAuthService: grokOAuthService}
}

func (r *GrokTokenRefresher) CacheKey(account *Account) string {
	return GrokTokenCacheKey(account)
}

func (r *GrokTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && account.Platform == PlatformGrok && account.Type == AccountTypeOAuth
}

func (r *GrokTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if account == nil || strings.TrimSpace(account.GetGrokRefreshToken()) == "" {
		return false
	}
	expiresAt := getGrokTokenExpiresAt(account)
	if expiresAt == nil {
		return true
	}
	if refreshWindow < grokTokenRefreshSkew {
		refreshWindow = grokTokenRefreshSkew
	}
	return time.Until(*expiresAt) < refreshWindow
}

func (r *GrokTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if r == nil || r.grokOAuthService == nil {
		return nil, errors.New("grok oauth service is not configured")
	}
	if account == nil {
		return nil, errors.New("account is nil")
	}
	flightKey := strings.TrimSpace(account.GetGrokRefreshToken())
	if flightKey == "" {
		flightKey = GrokTokenCacheKey(account)
	}
	v, err, _ := r.refreshGroup.Do("grok-rt:"+flightKey, func() (any, error) {
		tokenInfo, err := r.grokOAuthService.RefreshAccountToken(ctx, account)
		if err != nil {
			return nil, err
		}
		creds := r.grokOAuthService.BuildAccountCredentials(tokenInfo)
		creds = MergeCredentials(account.Credentials, creds)
		if baseURL := strings.TrimSpace(account.GetCredential("base_url")); baseURL != "" {
			creds["base_url"] = baseURL
		}
		return creds, nil
	})
	if err != nil {
		return nil, err
	}
	creds, _ := v.(map[string]any)
	if creds == nil {
		return nil, errors.New("grok token refresh: empty singleflight result")
	}
	return creds, nil
}
