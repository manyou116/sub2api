package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func webImgTestCfg(backend string) *config.Config {
	return &config.Config{Gateway: config.GatewayConfig{OpenAIWebImages: config.OpenAIWebImagesConfig{
		DefaultMaxInflight: 2, QuotaCacheTTLSeconds: 300, UnknownQuotaPolicy: "strict",
		SuccessDecrementLocal: true, RateLimitCooldownSeconds: 600, TransportMaxRetries: 1, PollTimeoutSeconds: 30,
		InflightBackend: backend, InflightTTLSeconds: 60, RedisKeyPrefix: "test:webimg:", BulkMaxAccounts: 100, BulkProbeConcurrency: 2,
	}}}
}

func TestOpenAIWebImages_ParseAndShouldUse(t *testing.T) {
	svc := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, nil)
	acc := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"openai_web_images": map[string]any{"enabled": true, "max_inflight": 3, "priority": 5, "stats": map[string]any{"success": float64(2), "fail": float64(1)}},
	}}
	cfg := svc.ParseAccountConfig(acc)
	require.True(t, cfg.Enabled)
	require.Equal(t, 3, cfg.MaxInflight)
	require.Equal(t, int64(2), cfg.Stats.Success)
	require.True(t, svc.ShouldUseWebPath(acc))
	// Account-level switch is the only gate (no global config switch).
	accOff := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"openai_web_images": map[string]any{"enabled": false},
	}}
	require.False(t, svc.ShouldUseWebPath(accOff))
	require.False(t, svc.ShouldUseWebPath(&Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{
		"openai_web_images": map[string]any{"enabled": true},
	}}))
}

func TestOpenAIWebImages_SchedulableAndInflight(t *testing.T) {
	svc := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, nil)
	acc := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Extra: map[string]any{"openai_web_images": map[string]any{"enabled": true, "max_inflight": 1}}}
	st, err := svc.GetStatus(context.Background(), acc)
	require.NoError(t, err)
	require.False(t, st.Schedulable)
	require.Equal(t, "quota_unknown", st.UnschedulableReason)
	svc.setQuotaCache(context.Background(), acc.ID, webImageQuotaCache{Remaining: 2, ProbedAt: time.Now().UTC()})
	st, err = svc.GetStatus(context.Background(), acc)
	require.NoError(t, err)
	require.True(t, st.Schedulable)
	ok, err := svc.Acquire(context.Background(), acc.ID, 1, "r1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = svc.Acquire(context.Background(), acc.ID, 1, "r2")
	require.NoError(t, err)
	require.False(t, ok)
	svc.Release(context.Background(), acc.ID, "r1")
}

func TestOpenAIWebImages_OptimisticAllowsUnknownQuota(t *testing.T) {
	cfg := webImgTestCfg("memory")
	cfg.Gateway.OpenAIWebImages.UnknownQuotaPolicy = "optimistic"
	svc := NewOpenAIWebImagesService(cfg, nil, nil)
	acc := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Extra: map[string]any{"openai_web_images": map[string]any{"enabled": true, "max_inflight": 1}}}
	st, err := svc.GetStatus(context.Background(), acc)
	require.NoError(t, err)
	require.True(t, st.Schedulable, "optimistic policy should schedule when quota cache is cold")
	require.Empty(t, st.UnschedulableReason)
}

func TestOpenAIWebImages_RedisInflight(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := NewOpenAIWebImagesService(webImgTestCfg("redis"), rdb, nil)
	ok, err := svc.Acquire(context.Background(), 42, 1, "req-1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = svc.Acquire(context.Background(), 42, 1, "req-2")
	require.NoError(t, err)
	require.False(t, ok)
	svc.Release(context.Background(), 42, "req-1")
	ok, err = svc.Acquire(context.Background(), 42, 1, "req-3")
	require.NoError(t, err)
	require.True(t, ok)
}

type webImgAccountRepo struct{ accounts map[int64]*Account }

func (r *webImgAccountRepo) Create(context.Context, *Account) error { panic("unused") }
func (r *webImgAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	acc, ok := r.accounts[id]
	if !ok {
		return nil, ErrAccountNotFound
	}
	return acc, nil
}
func (r *webImgAccountRepo) GetByIDs(context.Context, []int64) ([]*Account, error) { panic("unused") }
func (r *webImgAccountRepo) ExistsByID(context.Context, int64) (bool, error)       { panic("unused") }
func (r *webImgAccountRepo) GetByCRSAccountID(context.Context, string) (*Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) FindByExtraField(context.Context, string, any) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListCRSAccountIDs(context.Context) (map[string]int64, error) {
	panic("unused")
}
func (r *webImgAccountRepo) Update(context.Context, *Account) error { panic("unused") }
func (r *webImgAccountRepo) Delete(context.Context, int64) error    { panic("unused") }
func (r *webImgAccountRepo) List(context.Context, pagination.PaginationParams) ([]Account, *pagination.PaginationResult, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListWithFilters(context.Context, pagination.PaginationParams, string, string, string, string, int64, string) ([]Account, *pagination.PaginationResult, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListAllWithFilters(context.Context, string, string, string, string, int64, string) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListByGroup(context.Context, int64) ([]Account, error) { panic("unused") }
func (r *webImgAccountRepo) ListActive(context.Context) ([]Account, error)         { panic("unused") }
func (r *webImgAccountRepo) ListOAuthRefreshCandidates(context.Context) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) UpdateLastUsed(context.Context, int64) error { panic("unused") }
func (r *webImgAccountRepo) BatchUpdateLastUsed(context.Context, map[int64]time.Time) error {
	panic("unused")
}
func (r *webImgAccountRepo) SetError(context.Context, int64, string) error { panic("unused") }
func (r *webImgAccountRepo) ClearError(context.Context, int64) error       { panic("unused") }
func (r *webImgAccountRepo) SetSchedulable(context.Context, int64, bool) error {
	panic("unused")
}
func (r *webImgAccountRepo) AutoPauseExpiredAccounts(context.Context, time.Time) (int64, error) {
	panic("unused")
}
func (r *webImgAccountRepo) BindGroups(context.Context, int64, []int64) error { panic("unused") }
func (r *webImgAccountRepo) ListSchedulable(context.Context) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListSchedulableByGroupID(context.Context, int64) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListSchedulableByPlatform(context.Context, string) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListSchedulableByPlatforms(context.Context, []string) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListSchedulableByGroupIDAndPlatforms(context.Context, int64, []string) ([]Account, error) {
	panic("unused")
}

func (r *webImgAccountRepo) ListActiveAllowingTextRateLimitByGroupIDAndPlatforms(context.Context, int64, []string) ([]Account, error) {
	panic("unexpected ListActiveAllowingTextRateLimitByGroupIDAndPlatforms call")
}
func (r *webImgAccountRepo) ListActiveAllowingTextRateLimitByPlatforms(context.Context, []string) ([]Account, error) {
	panic("unexpected ListActiveAllowingTextRateLimitByPlatforms call")
}

func (r *webImgAccountRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) ListSchedulableUngroupedByPlatforms(context.Context, []string) ([]Account, error) {
	panic("unused")
}
func (r *webImgAccountRepo) SetRateLimited(context.Context, int64, time.Time) error {
	panic("unused")
}
func (r *webImgAccountRepo) SetModelRateLimit(context.Context, int64, string, time.Time, ...string) error {
	panic("unused")
}
func (r *webImgAccountRepo) SetOverloaded(context.Context, int64, time.Time) error {
	panic("unused")
}
func (r *webImgAccountRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	panic("unused")
}
func (r *webImgAccountRepo) ClearTempUnschedulable(context.Context, int64) error { panic("unused") }
func (r *webImgAccountRepo) ClearRateLimit(context.Context, int64) error         { panic("unused") }
func (r *webImgAccountRepo) SetWebImageRateLimited(_ context.Context, id int64, until time.Time) error {
	acc, ok := r.accounts[id]
	if !ok {
		return ErrAccountNotFound
	}
	now := time.Now().UTC()
	acc.WebImageRateLimitedAt = &now
	u := until.UTC()
	acc.WebImageRateLimitResetAt = &u
	return nil
}
func (r *webImgAccountRepo) ClearWebImageRateLimit(_ context.Context, id int64) error {
	acc, ok := r.accounts[id]
	if !ok {
		return ErrAccountNotFound
	}
	acc.WebImageRateLimitedAt = nil
	acc.WebImageRateLimitResetAt = nil
	return nil
}

func (r *webImgAccountRepo) ClearAntigravityQuotaScopes(context.Context, int64) error {
	panic("unused")
}
func (r *webImgAccountRepo) ClearModelRateLimits(context.Context, int64) error { panic("unused") }
func (r *webImgAccountRepo) UpdateSessionWindow(context.Context, int64, *time.Time, *time.Time, string) error {
	panic("unused")
}
func (r *webImgAccountRepo) UpdateSessionWindowEnd(context.Context, int64, time.Time) error {
	panic("unused")
}
func (r *webImgAccountRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	acc, ok := r.accounts[id]
	if !ok {
		return ErrAccountNotFound
	}
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	for k, v := range updates {
		acc.Extra[k] = v
	}
	return nil
}
func (r *webImgAccountRepo) BulkUpdate(context.Context, []int64, AccountBulkUpdate) (int64, error) {
	panic("unused")
}
func (r *webImgAccountRepo) IncrementQuotaUsed(context.Context, int64, float64) error {
	panic("unused")
}
func (r *webImgAccountRepo) ResetQuotaUsed(context.Context, int64) error { panic("unused") }
func (r *webImgAccountRepo) RevertProxyFallback(context.Context, int64) error {
	panic("unused")
}
func (r *webImgAccountRepo) ListShadowsByParent(context.Context, int64) ([]*Account, error) {
	panic("unused")
}

var _ AccountRepository = (*webImgAccountRepo)(nil)

func TestOpenAIWebImages_BulkPatchAndStats(t *testing.T) {
	repo := &webImgAccountRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{}},
		2: {ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{}},
	}}
	svc := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, repo)
	enabled := true
	maxInf := 2
	result, err := svc.BulkPatch(context.Background(), []int64{1, 2, 999}, OpenAIWebImagesBulkPatch{Enabled: &enabled, MaxInflight: &maxInf})
	require.NoError(t, err)
	require.Equal(t, 2, result.Updated)
	require.Equal(t, 1, result.Failed)
	svc.MarkSuccess(context.Background(), repo.accounts[1])
	cfg := svc.ParseAccountConfig(repo.accounts[1])
	require.Equal(t, int64(1), cfg.Stats.Success)
}

func TestOpenAIWebImages_ResolveUpstream(t *testing.T) {
	svc := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, nil)

	autoAcc := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": "plus"}, Extra: map[string]any{
		"openai_web_images": map[string]any{"enabled": true, "model_mode": "auto"},
	}}
	sel := svc.ResolveUpstream(autoAcc)
	require.Equal(t, "auto", sel.ModelMode)
	require.Equal(t, "gpt-5-6-thinking", sel.UpstreamModel)
	require.Equal(t, "extended", sel.ThinkingEffort)
	require.Equal(t, "auto_plan", sel.Source)
	require.Equal(t, "plus", sel.PlanType)

	fixedAcc := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": "team"}, Extra: map[string]any{
		"openai_web_images": map[string]any{"enabled": true, "model_mode": "fixed", "model": "gpt-5-6-thinking", "thinking_effort": "high"},
	}}
	sel = svc.ResolveUpstream(fixedAcc)
	require.Equal(t, "fixed", sel.ModelMode)
	require.Equal(t, "gpt-5-6-thinking", sel.UpstreamModel)
	require.Equal(t, "high", sel.ThinkingEffort)
	require.Equal(t, "account_fixed", sel.Source)
}

func TestParseWebImageResetDuration(t *testing.T) {
	d := parseWebImageResetDuration("limit resets in 23 hours and 5 minutes")
	if d < 23*time.Hour || d > 24*time.Hour {
		t.Fatalf("duration=%s", d)
	}
}

func TestOpenAIWebImages_ClearCooldownRemovesRedisKeys(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := NewOpenAIWebImagesService(webImgTestCfg("redis"), rdb, nil)
	// force prefix like production
	svc.cfg = &config.Config{Gateway: config.GatewayConfig{OpenAIWebImages: config.OpenAIWebImagesConfig{
		InflightBackend: "redis", RedisKeyPrefix: "sub2api:webimg:", RateLimitCooldownSeconds: 600,
	}}}
	ctx := context.Background()
	svc.setCooldown(ctx, 66, time.Now().Add(2*time.Hour))
	svc.setQuotaCache(ctx, 66, webImageQuotaCache{Remaining: 0, ProbedAt: time.Now().UTC()})
	require.True(t, svc.IsWebRateLimited(ctx, 66))
	require.NoError(t, svc.ClearCooldown(ctx, 66))
	require.False(t, svc.IsWebRateLimited(ctx, 66))
	_, ok := svc.getQuotaCache(ctx, 66)
	require.False(t, ok)
}

func TestOpenAIWebImages_ParseAccountConfig_GlobalInherit(t *testing.T) {
	cfg := webImgTestCfg("memory")
	cfg.Gateway.OpenAIWebImages.DefaultEnabled = true
	svc := NewOpenAIWebImagesService(cfg, nil, nil)

	// No extra -> inherit global
	acc := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	parsed := svc.ParseAccountConfig(acc)
	require.True(t, parsed.Enabled)
	require.False(t, parsed.EnabledSet)
	require.True(t, svc.ShouldUseWebPath(acc))

	// extra without enabled key -> still inherit
	acc2 := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"openai_web_images": map[string]any{"max_inflight": 4},
	}}
	parsed2 := svc.ParseAccountConfig(acc2)
	require.True(t, parsed2.Enabled)
	require.False(t, parsed2.EnabledSet)
	require.Equal(t, 4, parsed2.MaxInflight)

	// explicit off overrides global on
	acc3 := &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"openai_web_images": map[string]any{"enabled": false},
	}}
	parsed3 := svc.ParseAccountConfig(acc3)
	require.False(t, parsed3.Enabled)
	require.True(t, parsed3.EnabledSet)
	require.False(t, svc.ShouldUseWebPath(acc3))

	// global off + no override
	cfgOff := webImgTestCfg("memory")
	cfgOff.Gateway.OpenAIWebImages.DefaultEnabled = false
	svcOff := NewOpenAIWebImagesService(cfgOff, nil, nil)
	acc4 := &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.False(t, svcOff.ParseAccountConfig(acc4).Enabled)
	require.False(t, svcOff.ShouldUseWebPath(acc4))

	// legacy flag is explicit override
	acc5 := &Account{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"openai_oauth_legacy_images": true,
	}}
	parsed5 := svcOff.ParseAccountConfig(acc5)
	require.True(t, parsed5.Enabled)
	require.True(t, parsed5.EnabledSet)
}

func TestOpenAIWebImages_PatchInheritOmitsEnabled(t *testing.T) {
	repo := &webImgAccountRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
			"openai_web_images": map[string]any{"enabled": true, "max_inflight": 2},
		}},
	}}
	cfg := webImgTestCfg("memory")
	cfg.Gateway.OpenAIWebImages.DefaultEnabled = true
	svc := NewOpenAIWebImagesService(cfg, nil, repo)

	mode := "inherit"
	require.NoError(t, svc.PatchAccount(context.Background(), 1, OpenAIWebImagesBulkPatch{EnabledMode: &mode}))
	raw := repo.accounts[1].Extra["openai_web_images"].(map[string]any)
	_, hasEnabled := raw["enabled"]
	require.False(t, hasEnabled, "enabled key must be omitted on inherit: %#v", raw)

	parsed := svc.ParseAccountConfig(repo.accounts[1])
	require.True(t, parsed.Enabled, "should inherit global default true")
	require.False(t, parsed.EnabledSet)

	st, err := svc.GetStatus(context.Background(), repo.accounts[1])
	require.NoError(t, err)
	require.Equal(t, "global", st.EnabledSource)
	require.True(t, st.DefaultEnabled)
	require.True(t, st.Enabled)

	// force off override
	modeOff := "off"
	require.NoError(t, svc.PatchAccount(context.Background(), 1, OpenAIWebImagesBulkPatch{EnabledMode: &modeOff}))
	raw = repo.accounts[1].Extra["openai_web_images"].(map[string]any)
	require.Equal(t, false, raw["enabled"])
	st, err = svc.GetStatus(context.Background(), repo.accounts[1])
	require.NoError(t, err)
	require.Equal(t, "account", st.EnabledSource)
	require.False(t, st.Enabled)
}

func TestClassifyWebImageRateLimitReason(t *testing.T) {
	require.Equal(t, "soft", classifyWebImageRateLimitReason("conversation poll rate limited (too many 429)"))
	require.Equal(t, "soft", classifyWebImageRateLimitReason("soft poll 429"))
	require.Equal(t, "soft", classifyWebImageRateLimitReason("Too Many Requests"))
	require.Equal(t, "quota_daily", classifyWebImageRateLimitReason(
		`You've hit the Free plan limit for image generations requests. You can create more images when the limit resets in 23 hours and 5 minutes.`))
	require.Equal(t, "rate_limit", classifyWebImageRateLimitReason("image generation rate limit exceeded"))
}

func TestOpenAIWebImages_MarkFailSoftDoesNotDurablePin(t *testing.T) {
	repo := &webImgAccountRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
			"openai_web_images": map[string]any{"enabled": true},
		}},
	}}
	svc := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, repo)
	acc := repo.accounts[1]
	svc.MarkFail(context.Background(), acc, "conversation poll rate limited (too many 429)", true)
	// soft uses cache only — durable DB field not set via repo SetWebImageRateLimited noop returns nil but account fields unchanged
	require.Nil(t, acc.WebImageRateLimitResetAt)
	// soft cool-down present in memory cache
	require.True(t, svc.IsWebRateLimited(context.Background(), 1))
}

func TestOpenAIWebImages_MarkFailDailyDurable(t *testing.T) {
	repo := &webImgAccountRepo{accounts: map[int64]*Account{
		2: {ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
			"openai_web_images": map[string]any{"enabled": true},
		}},
	}}
	svc := NewOpenAIWebImagesService(webImgTestCfg("memory"), nil, repo)
	acc := repo.accounts[2]
	msg := `You've hit the Free plan limit for image generations requests. resets in 22 hours and 10 minutes.`
	svc.MarkFail(context.Background(), acc, msg, true)
	cfg := svc.ParseAccountConfig(repo.accounts[2])
	require.Equal(t, "quota_daily", cfg.Stats.LastRateLimitReason)
	require.True(t, repo.accounts[2].IsWebImageRateLimited(), "daily quota must durable-pin via DB fields")
	require.NotNil(t, repo.accounts[2].WebImageRateLimitResetAt)
	require.True(t, repo.accounts[2].WebImageRateLimitResetAt.After(time.Now().Add(20*time.Hour)))
}
