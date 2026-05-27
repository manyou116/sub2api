//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type rateLimit429AccountRepoStub struct {
	mockAccountRepoForGemini
	rateLimitCalls     int
	lastRateLimitID    int64
	lastRateLimitReset time.Time
	accounts           []Account
	clearRateLimitIDs  []int64
}

func (r *rateLimit429AccountRepoStub) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.rateLimitCalls++
	r.lastRateLimitID = id
	r.lastRateLimitReset = resetAt
	return nil
}

func (r *rateLimit429AccountRepoStub) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	result := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform {
			result = append(result, account)
		}
	}
	return result, nil
}

func (r *rateLimit429AccountRepoStub) ClearRateLimit(_ context.Context, id int64) error {
	r.clearRateLimitIDs = append(r.clearRateLimitIDs, id)
	return nil
}

func TestGetRateLimit429CooldownSettings_DefaultsWhenNotSet(t *testing.T) {
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetRateLimit429CooldownSettings(context.Background())
	require.NoError(t, err)
	require.True(t, settings.Enabled)
	require.Equal(t, 5, settings.CooldownSeconds)
}

func TestGetRateLimit429CooldownSettings_ReadsFromDB(t *testing.T) {
	repo := newMockSettingRepo()
	data, _ := json.Marshal(RateLimit429CooldownSettings{Enabled: false, CooldownSeconds: 12})
	repo.data[SettingKeyRateLimit429CooldownSettings] = string(data)
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetRateLimit429CooldownSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled)
	require.Equal(t, 12, settings.CooldownSeconds)
}

func TestSetRateLimit429CooldownSettings_EnabledRejectsOutOfRange(t *testing.T) {
	svc := NewSettingService(newMockSettingRepo(), &config.Config{})

	for _, seconds := range []int{0, -1, 7201, 99999} {
		err := svc.SetRateLimit429CooldownSettings(context.Background(), &RateLimit429CooldownSettings{
			Enabled: true, CooldownSeconds: seconds,
		})
		require.Error(t, err, "should reject enabled=true + cooldown_seconds=%d", seconds)
		require.Contains(t, err.Error(), "cooldown_seconds must be between 1-7200")
	}
}

func TestHandle429_FallbackUsesDBSeconds(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	data, _ := json.Marshal(RateLimit429CooldownSettings{Enabled: true, CooldownSeconds: 12})
	settingRepo.data[SettingKeyRateLimit429CooldownSettings] = string(data)

	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)

	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	before := time.Now()
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	after := time.Now()

	require.Equal(t, 1, accountRepo.rateLimitCalls)
	require.Equal(t, int64(42), accountRepo.lastRateLimitID)
	require.True(t, !accountRepo.lastRateLimitReset.Before(before.Add(12*time.Second)) && !accountRepo.lastRateLimitReset.After(after.Add(12*time.Second)))
}

func TestHandle429_FallbackDisabledSkipsLocalMark(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	data, _ := json.Marshal(RateLimit429CooldownSettings{Enabled: false, CooldownSeconds: 12})
	settingRepo.data[SettingKeyRateLimit429CooldownSettings] = string(data)

	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)

	account := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))

	require.Zero(t, accountRepo.rateLimitCalls)
}

func TestHandle429_FallbackUsesDefaultSecondsWhenSettingServiceMissing(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	cfg := &config.Config{}
	svc := NewRateLimitService(accountRepo, nil, cfg, nil, nil)

	account := &Account{ID: 44, Platform: PlatformGemini, Type: AccountTypeAPIKey}
	before := time.Now()
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"message":"slow down"}}`))
	after := time.Now()

	require.Equal(t, 1, accountRepo.rateLimitCalls)
	require.Equal(t, int64(44), accountRepo.lastRateLimitID)
	require.True(t, !accountRepo.lastRateLimitReset.Before(before.Add(5*time.Second)) && !accountRepo.lastRateLimitReset.After(after.Add(5*time.Second)))
}

func TestGetOpenAICodexQuotaGuardSettings_DefaultsWhenNotSet(t *testing.T) {
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetOpenAICodexQuotaGuardSettings(context.Background())
	require.NoError(t, err)
	require.True(t, settings.Enabled)
	require.Equal(t, 90.0, settings.ThresholdPercent)
}

func TestGetOpenAICodexQuotaGuardSettings_ReadsFromDB(t *testing.T) {
	repo := newMockSettingRepo()
	data, _ := json.Marshal(OpenAICodexQuotaGuardSettings{Enabled: false, ThresholdPercent: 75})
	repo.data[SettingKeyOpenAICodexQuotaGuardSettings] = string(data)
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetOpenAICodexQuotaGuardSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled)
	require.Equal(t, 75.0, settings.ThresholdPercent)
}

func TestSetOpenAICodexQuotaGuardSettings_EnabledRejectsOutOfRange(t *testing.T) {
	svc := NewSettingService(newMockSettingRepo(), &config.Config{})

	for _, threshold := range []float64{0, -1, 100.1} {
		err := svc.SetOpenAICodexQuotaGuardSettings(context.Background(), &OpenAICodexQuotaGuardSettings{
			Enabled: true, ThresholdPercent: threshold,
		})
		require.Error(t, err, "should reject enabled=true + threshold_percent=%f", threshold)
		require.Contains(t, err.Error(), "threshold_percent must be between 1-100")
	}
}

func TestSetOpenAICodexQuotaGuardSettings_DisabledNormalizesOutOfRange(t *testing.T) {
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})

	err := svc.SetOpenAICodexQuotaGuardSettings(context.Background(), &OpenAICodexQuotaGuardSettings{
		Enabled: false, ThresholdPercent: 0,
	})
	require.NoError(t, err)

	settings, err := svc.GetOpenAICodexQuotaGuardSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled)
	require.Equal(t, 90.0, settings.ThresholdPercent)
}

func TestSetOpenAICodexQuotaGuardSettings_RaisingThresholdClearsMatchingPause(t *testing.T) {
	settingRepo := newMockSettingRepo()
	previousData, err := json.Marshal(OpenAICodexQuotaGuardSettings{Enabled: true, ThresholdPercent: 1})
	require.NoError(t, err)
	settingRepo.data[SettingKeyOpenAICodexQuotaGuardSettings] = string(previousData)

	now := time.Now().UTC()
	updatedAt := now.Add(-2 * time.Minute)
	resetAt := updatedAt.Add(5 * time.Hour)
	accountRepo := &rateLimit429AccountRepoStub{accounts: []Account{
		{
			ID:               66,
			Platform:         PlatformOpenAI,
			Type:             AccountTypeOAuth,
			RateLimitResetAt: &resetAt,
			Extra: map[string]any{
				"codex_usage_updated_at":       updatedAt.Format(time.RFC3339),
				"codex_5h_used_percent":        3.0,
				"codex_5h_reset_after_seconds": int(resetAt.Sub(updatedAt).Seconds()),
				"codex_5h_window_minutes":      300,
				"codex_7d_used_percent":        0.5,
				"codex_7d_reset_after_seconds": 7 * 24 * 3600,
				"codex_7d_window_minutes":      7 * 24 * 60,
			},
		},
	}}

	svc := NewSettingService(settingRepo, &config.Config{})
	svc.SetOpenAICodexQuotaGuardAccountRepository(accountRepo)

	err = svc.SetOpenAICodexQuotaGuardSettings(context.Background(), &OpenAICodexQuotaGuardSettings{Enabled: true, ThresholdPercent: 90})
	require.NoError(t, err)
	require.Equal(t, []int64{66}, accountRepo.clearRateLimitIDs)
}

func TestSetOpenAICodexQuotaGuardSettings_DoesNotClearUnrelatedOrStillExceededPause(t *testing.T) {
	settingRepo := newMockSettingRepo()
	previousData, err := json.Marshal(OpenAICodexQuotaGuardSettings{Enabled: true, ThresholdPercent: 90})
	require.NoError(t, err)
	settingRepo.data[SettingKeyOpenAICodexQuotaGuardSettings] = string(previousData)

	now := time.Now().UTC()
	updatedAt := now.Add(-2 * time.Minute)
	matchingResetAt := updatedAt.Add(5 * time.Hour)
	unrelatedResetAt := now.Add(30 * time.Minute)
	accountRepo := &rateLimit429AccountRepoStub{accounts: []Account{
		{
			ID:               67,
			Platform:         PlatformOpenAI,
			Type:             AccountTypeOAuth,
			RateLimitResetAt: &matchingResetAt,
			Extra: map[string]any{
				"codex_usage_updated_at":       updatedAt.Format(time.RFC3339),
				"codex_7d_used_percent":        95.0,
				"codex_7d_reset_after_seconds": int(matchingResetAt.Sub(updatedAt).Seconds()),
				"codex_7d_window_minutes":      7 * 24 * 60,
			},
		},
		{
			ID:               68,
			Platform:         PlatformOpenAI,
			Type:             AccountTypeOAuth,
			RateLimitResetAt: &unrelatedResetAt,
			Extra: map[string]any{
				"codex_usage_updated_at":       updatedAt.Format(time.RFC3339),
				"codex_5h_used_percent":        3.0,
				"codex_5h_reset_after_seconds": int(matchingResetAt.Sub(updatedAt).Seconds()),
				"codex_5h_window_minutes":      300,
			},
		},
		{
			ID:               69,
			Platform:         PlatformOpenAI,
			Type:             AccountTypeOAuth,
			RateLimitResetAt: &unrelatedResetAt,
		},
	}}

	svc := NewSettingService(settingRepo, &config.Config{})
	svc.SetOpenAICodexQuotaGuardAccountRepository(accountRepo)

	err = svc.SetOpenAICodexQuotaGuardSettings(context.Background(), &OpenAICodexQuotaGuardSettings{Enabled: true, ThresholdPercent: 90})
	require.NoError(t, err)
	require.Empty(t, accountRepo.clearRateLimitIDs)
}

func TestSetOpenAICodexQuotaGuardSettings_ReSavingCurrentThresholdClearsStaleCodexPause(t *testing.T) {
	settingRepo := newMockSettingRepo()
	previousData, err := json.Marshal(OpenAICodexQuotaGuardSettings{Enabled: true, ThresholdPercent: 91})
	require.NoError(t, err)
	settingRepo.data[SettingKeyOpenAICodexQuotaGuardSettings] = string(previousData)

	now := time.Now().UTC()
	updatedAt := now.Add(-2 * time.Minute)
	resetAt := updatedAt.Add(5 * 24 * time.Hour)
	accountRepo := &rateLimit429AccountRepoStub{accounts: []Account{
		{
			ID:               66,
			Platform:         PlatformOpenAI,
			Type:             AccountTypeOAuth,
			RateLimitResetAt: &resetAt,
			Extra: map[string]any{
				"codex_usage_updated_at":  updatedAt.Format(time.RFC3339),
				"codex_5h_used_percent":   0.0,
				"codex_5h_reset_at":       updatedAt.Add(5 * time.Hour).Format(time.RFC3339),
				"codex_5h_window_minutes": 300,
				"codex_7d_used_percent":   3.0,
				"codex_7d_reset_at":       resetAt.Format(time.RFC3339),
				"codex_7d_window_minutes": 7 * 24 * 60,
			},
		},
	}}

	svc := NewSettingService(settingRepo, &config.Config{})
	svc.SetOpenAICodexQuotaGuardAccountRepository(accountRepo)

	err = svc.SetOpenAICodexQuotaGuardSettings(context.Background(), &OpenAICodexQuotaGuardSettings{Enabled: true, ThresholdPercent: 91})
	require.NoError(t, err)
	require.Equal(t, []int64{66}, accountRepo.clearRateLimitIDs)
}
