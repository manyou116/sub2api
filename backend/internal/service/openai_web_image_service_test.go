//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type webImageTestRepo struct {
	AccountRepository
	accounts  map[int64]*Account
	cooldowns map[int64]time.Time
}

func (r *webImageTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if account := r.accounts[id]; account != nil {
		return account, nil
	}
	return nil, errors.New("not found")
}
func (r *webImageTestRepo) WebImageCooldown(_ context.Context, id int64) (*time.Time, error) {
	if until, ok := r.cooldowns[id]; ok {
		return &until, nil
	}
	return nil, nil
}
func (r *webImageTestRepo) SetWebImageCooldown(_ context.Context, id int64, until time.Time) error {
	r.cooldowns[id] = until
	return nil
}
func (r *webImageTestRepo) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.accounts[id].TempUnschedulableUntil = &until
	r.accounts[id].TempUnschedulableReason = reason
	return nil
}

type fakeWebImageDriver struct {
	generate func(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error)
	probe    func(context.Context, webdriver.Auth) (*webdriver.Quota, error)
}

func (d *fakeWebImageDriver) Generate(ctx context.Context, auth webdriver.Auth, r webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
	return d.generate(ctx, auth, r)
}
func (d *fakeWebImageDriver) Edit(ctx context.Context, auth webdriver.Auth, r webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
	return d.generate(ctx, auth, r)
}
func (d *fakeWebImageDriver) ProbeQuota(ctx context.Context, auth webdriver.Auth) (*webdriver.Quota, error) {
	if d.probe == nil {
		return &webdriver.Quota{Known: false, ProbedAt: time.Now()}, nil
	}
	return d.probe(ctx, auth)
}
func webImageFixture(t *testing.T) (*OpenAIWebImageService, *webImageTestRepo, *Account, *Account, *fakeWebImageQuotaCache) {
	t.Helper()
	parentID := int64(1)
	parent := &Account{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 8, Credentials: map[string]any{"access_token": "parent-token", "chatgpt_account_id": "parent-chatgpt", "plan_type": "free", "user_agent": "parent-agent"}, Extra: map[string]any{"openai_device_id": "parent-device", "openai_session_id": "parent-session"}}
	shadow := &Account{ID: 2, ParentAccountID: &parentID, QuotaDimension: QuotaDimensionWebImages, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-image-2": "gpt-image-2"}}}
	repo := &webImageTestRepo{accounts: map[int64]*Account{1: parent, 2: shadow}, cooldowns: map[int64]time.Time{}}
	cache := &fakeWebImageQuotaCache{statuses: map[int64]WebImageStatus{}, owners: map[int64]string{}}
	gateway := &OpenAIGatewayService{accountRepo: repo}
	service := NewOpenAIWebImageService(repo, cache, gateway)
	return service, repo, parent, shadow, cache
}

func TestWebImageCurrentParentIdentityAndPurpose(t *testing.T) {
	s, _, parent, shadow, _ := webImageFixture(t)
	ctx := context.Background()
	parent.Schedulable = false
	future := time.Now().Add(time.Hour)
	parent.RateLimitResetAt = &future
	parent.Proxy = &Proxy{Protocol: "http", Host: "127.0.0.9", Port: 3128}
	parent.Credentials["access_token"] = "rotated-parent-token"
	parent.Extra["openai_web_images_client_profile"] = "codex_browser"
	calls := 0
	s.driver = &fakeWebImageDriver{generate: func(_ context.Context, auth webdriver.Auth, request webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
		calls++
		require.Equal(t, "rotated-parent-token", auth.AccessToken)
		require.Equal(t, "parent-chatgpt", auth.AccountID)
		require.Equal(t, "parent-device", auth.DeviceID)
		require.Equal(t, "parent-session", auth.SessionID)
		require.Equal(t, "codex_browser", auth.ClientProfile)
		require.Equal(t, "parent-agent", auth.UserAgent)
		require.Equal(t, parent.Proxy.URL(), auth.ProxyURL)
		require.Equal(t, "gpt-5-6-thinking", request.Model)
		return &webdriver.GenerateResult{Data: []webdriver.ImageData{{B64JSON: "aW1hZ2U="}}}, nil
	}}
	_, _, err := s.generate(ctx, shadow, &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "test", N: 1})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, "web_image_only", s.gateway.webImageSchedulingReason(ctx, shadow, "gpt-image-2"))
	require.Equal(t, "web_image_only", s.gateway.webImageSchedulingReason(WithOpenAIImageGenerationIntent(ctx), shadow, ""))
	shadow.Credentials["model_mapping"] = map[string]any{"gpt-5.4": "gpt-5.4"}
	require.Equal(t, "web_image_only", s.gateway.webImageSchedulingReason(WithOpenAIImageGenerationIntent(ctx), shadow, "gpt-5.4"))
	require.False(t, accountSupportsOpenAICapabilities(shadow, OpenAIEndpointCapabilityResponses, ""))
	require.True(t, accountSupportsOpenAICapabilities(shadow, "", OpenAIImagesCapabilityBasic))
}

func TestWebImageQuotaIndependenceWorkModeAndHealth(t *testing.T) {
	s, _, parent, shadow, _ := webImageFixture(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	parent.RateLimitResetAt = &future
	parent.OverloadUntil = &future
	parent.TempUnschedulableUntil = &future
	parent.TempUnschedulableReason = OpenAICapacityCooldownReasonPrefix + `"until":1}`
	s.gateway.openaiAccountRuntimeBlockUntil.Store(parent.ID, future)
	_, err := s.eligible(ctx, shadow)
	require.NoError(t, err, "ordinary Web must ignore text quota/runtime limit")
	shadow.QuotaDimension = QuotaDimensionWorkImages
	_, err = s.eligible(ctx, shadow)
	require.ErrorContains(t, err, "text quota")
	parent.RateLimitResetAt = nil
	parent.OverloadUntil = nil
	_, err = s.eligible(ctx, shadow)
	require.NoError(t, err, "overload-only capacity exception includes Work Mode")
	parent.TempUnschedulableReason = "transport/proxy failure"
	_, err = s.eligible(ctx, shadow)
	require.ErrorContains(t, err, "credentials")
	parent.TempUnschedulableUntil = nil
	shadow.Schedulable = false
	_, err = s.eligible(ctx, shadow)
	require.ErrorContains(t, err, "not schedulable")
	shadow.Schedulable = true
	parent.Status = StatusError
	_, err = s.eligible(ctx, shadow)
	require.ErrorContains(t, err, "credentials")
}

func TestWebImageUnknownQuotaAndProbeFencing(t *testing.T) {
	s, _, parent, shadow, mr := webImageFixture(t)
	ctx := context.Background()
	s.driver = &fakeWebImageDriver{probe: func(_ context.Context, auth webdriver.Auth) (*webdriver.Quota, error) {
		require.Equal(t, "parent-token", auth.AccessToken)
		return &webdriver.Quota{Known: false, ProbedAt: time.Now()}, nil
	}}
	status, err := s.Probe(ctx, shadow.ID)
	require.NoError(t, err)
	require.False(t, status.QuotaKnown)
	require.Nil(t, status.Remaining)
	s.driver = &fakeWebImageDriver{probe: func(_ context.Context, _ webdriver.Auth) (*webdriver.Quota, error) {
		mr.replaceOwner(parent.ID, "new-owner")
		return &webdriver.Quota{Known: true, Remaining: 9, ProbedAt: time.Now()}, nil
	}}
	_, err = s.Probe(ctx, shadow.ID)
	require.ErrorContains(t, err, "lease expired")
	owner := mr.owner(parent.ID)
	require.Equal(t, "new-owner", owner)
	status, err = s.Status(ctx, shadow.ID)
	require.NoError(t, err)
	require.False(t, status.QuotaKnown)
}

func TestWebImageExhaustionSurvivesRecreateWithoutConflatingPoll429(t *testing.T) {
	s, repo, parent, shadow, cache := webImageFixture(t)
	ctx := context.Background()
	poll := webdriver.NewError(webdriver.ErrorKindRateLimited, "poll", "conversation poll rate limited (429 x8)", 429, true)
	s.observeError(ctx, parent, "gpt-5-6-thinking", poll)
	require.Empty(t, repo.cooldowns)
	require.Equal(t, "rate_limit", cache.backoffs[parent.ID].CooldownReason)
	require.Less(t, time.Until(*cache.backoffs[parent.ID].CooldownUntil), time.Minute)
	delete(cache.backoffs, parent.ID)
	quota := webdriver.NewError(webdriver.ErrorKindRateLimited, "poll", "You've hit the Free plan limit for image generations requests.", 429, true)
	s.observeError(ctx, parent, "gpt-5-6-thinking-wm", quota)
	require.Empty(t, repo.cooldowns)
	require.Equal(t, "rate_limit", cache.backoffs[parent.ID].CooldownReason)
	require.Less(t, time.Until(*cache.backoffs[parent.ID].CooldownUntil), time.Minute)
	delete(cache.backoffs, parent.ID)
	s.observeError(ctx, parent, "gpt-5-6-thinking", quota)
	require.Contains(t, repo.cooldowns, parent.ID)
	delete(repo.accounts, shadow.ID)
	copyShadow := *shadow
	copyShadow.ID = 3
	repo.accounts[3] = &copyShadow
	status, err := s.Status(ctx, 3)
	require.NoError(t, err)
	require.NotNil(t, status.CooldownUntil)
	require.NotNil(t, status.Remaining)
	require.Zero(t, *status.Remaining)
	_, err = s.eligible(ctx, &copyShadow)
	require.ErrorContains(t, err, "quota is exhausted")
	// A migrated cooldown whose source is unknown must not invent remaining=0.
	require.NoError(t, s.quotaCache.Delete(ctx, parent.ID))
	status, err = s.Status(ctx, 3)
	require.NoError(t, err)
	require.False(t, status.QuotaKnown)
	require.Nil(t, status.Remaining)
	require.NotNil(t, status.CooldownUntil)
}

type webImageSlotCache struct {
	stubConcurrencyCacheForTest
	active, acquisitions int
}

func (c *webImageSlotCache) AcquireAccountSlot(_ context.Context, _ int64, max int, _ string) (bool, error) {
	c.acquisitions++
	if c.active >= max {
		return false, nil
	}
	c.active++
	return true, nil
}
func (c *webImageSlotCache) ReleaseAccountSlot(_ context.Context, _ int64, _ string) error {
	c.active--
	return nil
}
func TestWebImageAdminUsesExactlyOneSlotAndReleases(t *testing.T) {
	for _, outcome := range []string{"success", "error", "cancel", "saturated"} {
		t.Run(outcome, func(t *testing.T) {
			s, _, _, shadow, _ := webImageFixture(t)
			cache := &webImageSlotCache{}
			s.gateway.concurrencyService = NewConcurrencyService(cache)
			if outcome == "saturated" {
				cache.active = 1
			}
			calls := 0
			s.driver = &fakeWebImageDriver{generate: func(ctx context.Context, _ webdriver.Auth, _ webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
				calls++
				require.Equal(t, 1, cache.active)
				if outcome == "error" {
					return nil, errors.New("fake failure")
				}
				if outcome == "cancel" {
					return nil, context.Canceled
				}
				return &webdriver.GenerateResult{Data: []webdriver.ImageData{{B64JSON: "aW1hZ2U="}}}, nil
			}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
			tester := &AccountTestService{openaiGatewayService: s.gateway}
			_ = tester.testWebImage(c, shadow, "gpt-image-2", "test")
			require.Equal(t, 1, cache.acquisitions)
			if outcome == "saturated" {
				require.Zero(t, calls)
				require.Equal(t, 1, cache.active)
			} else {
				require.Equal(t, 1, calls)
				require.Zero(t, cache.active)
			}
		})
	}
}

func TestWebImageCompletedResponseHasBillableResult(t *testing.T) {
	s, _, _, shadow, _ := webImageFixture(t)
	s.driver = &fakeWebImageDriver{generate: func(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
		return &webdriver.GenerateResult{Created: 7, Data: []webdriver.ImageData{{B64JSON: "aW1hZ2U="}}}, nil
	}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	result, err := s.gateway.ForwardImages(context.Background(), c, shadow, nil, &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "test", N: 1}, "")
	require.NoError(t, err)
	require.Equal(t, 1, result.ImageCount)
	require.Equal(t, "/backend-api/conversation", result.UpstreamEndpoint)
	var response map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response["data"], 1)
}

func (r *webImageTestRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]Account, error) {
	return r.ListSchedulableByPlatform(context.Background(), platform)
}
func (r *webImageTestRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}
func (r *webImageTestRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]Account, error) {
	var accounts []Account
	for _, a := range r.accounts {
		if a.Platform == platform && a.IsSchedulable() {
			accounts = append(accounts, *a)
		}
	}
	return accounts, nil
}
func TestWebImageSchedulerSelectsShadowWithOneSlot(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(fmt.Sprint(advanced), func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			s, _, parent, shadow, _ := webImageFixture(t)
			parent.Schedulable = false
			cfg := &config.Config{}
			cfg.Gateway.Scheduling.LoadBatchEnabled = true
			s.gateway.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService(fmt.Sprint(advanced))
			s.gateway.cfg = cfg
			s.gateway.cache = &schedulerTestGatewayCache{}
			var acquired, released []int64
			s.gateway.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired, releasedIDs: &released})
			ctx := WithOpenAIImageGenerationIntent(context.Background())
			selection, _, err := s.gateway.SelectAccountWithSchedulerForImages(ctx, nil, "", "gpt-image-2", nil, OpenAIImagesCapabilityNative)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, shadow.ID, selection.Account.ID)
			require.True(t, selection.Acquired)
			require.Equal(t, []int64{shadow.ID}, acquired)
			selection.ReleaseFunc()
			require.Equal(t, []int64{shadow.ID}, released)
			shadow.Credentials["model_mapping"] = map[string]any{"gpt-5.4": "gpt-5.4"}
			_, _, err = s.gateway.SelectAccountWithScheduler(context.Background(), nil, "", "", "gpt-5.4", nil, OpenAIUpstreamTransportAny, false)
			require.Error(t, err)
		})
	}
}

func TestWebImageCanceledTransportDoesNotPauseParent(t *testing.T) {
	for _, stage := range []string{"probe", "generate"} {
		t.Run(stage, func(t *testing.T) {
			s, repo, parent, shadow, _ := webImageFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := func() error {
				cancel()
				return webdriver.NewError(webdriver.ErrorKindTransport, stage, "context canceled", 0, true)
			}
			driver := &fakeWebImageDriver{}
			driver.probe = func(context.Context, webdriver.Auth) (*webdriver.Quota, error) {
				if stage == "probe" {
					return nil, failure()
				}
				return &webdriver.Quota{ProbedAt: time.Now()}, nil
			}
			driver.generate = func(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
				return nil, failure()
			}
			s.driver = driver
			var err error
			if stage == "probe" {
				_, err = s.Probe(ctx, shadow.ID)
			} else {
				_, _, err = s.generate(ctx, shadow, &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "test", N: 1})
			}
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, parent.TempUnschedulableUntil)
			require.Empty(t, repo.cooldowns)
			require.False(t, s.gateway.isOpenAIAccountRuntimeBlocked(parent))
		})
	}
}

func TestWebImageNonRetryableErrorsAreTerminalButAuthCanFailover(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     webdriver.ErrorKind
		stage    string
		status   int
		want     int
		failover bool
	}{
		{"policy", webdriver.ErrorKindPolicy, "generate", 400, 400, false},
		{"invalid_input", webdriver.ErrorKindInternal, "edit", 0, 400, false},
		{"invalid_request", webdriver.ErrorKindUpstream, "generate", 422, 422, false},
		{"completed_asset_error", webdriver.ErrorKindInternal, "download", 0, 502, false},
		{"auth", webdriver.ErrorKindAuth, "generate", 401, 401, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, shadow, _ := webImageFixture(t)
			calls := 0
			s.driver = &fakeWebImageDriver{generate: func(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
				calls++
				return nil, webdriver.NewError(tc.kind, tc.stage, "rejected", tc.status, false)
			}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			_, err := s.gateway.ForwardImages(context.Background(), c, shadow, nil, &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "test", N: 1}, "")
			require.Error(t, err)
			require.Equal(t, 1, calls)
			var failover *UpstreamFailoverError
			var terminal *OpenAIImagesUpstreamError
			if tc.failover {
				require.ErrorAs(t, err, &failover)
				require.False(t, c.Writer.Written())
			} else {
				require.ErrorAs(t, err, &terminal)
				require.False(t, errors.As(err, &failover))
				require.Equal(t, tc.want, rec.Code)
				require.Contains(t, rec.Body.String(), "rejected")
			}
		})
	}
}

func TestWebImageInvalidInputURLDoesNotFailover(t *testing.T) {
	s, _, _, shadow, _ := webImageFixture(t)
	s.driver = &fakeWebImageDriver{generate: func(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
		t.Fatal("invalid image must not reach generation")
		return nil, nil
	}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	_, err := s.gateway.ForwardImages(context.Background(), c, shadow, nil, &OpenAIImagesRequest{Endpoint: openAIImagesEditsEndpoint, Model: "gpt-image-2", Prompt: "test", InputImageURLs: []string{"data:image/png;base64,!invalid!"}}, "")
	var terminal *OpenAIImagesUpstreamError
	require.ErrorAs(t, err, &terminal)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWebImageConcurrentColdQuotaSharesProbe(t *testing.T) {
	s, _, _, shadow, _ := webImageFixture(t)
	shadow.Concurrency = 2
	entered := make(chan struct{})
	release := make(chan struct{})
	ownerDone := make(chan error, 1)
	var probes, generations atomic.Int32
	s.driver = &fakeWebImageDriver{
		probe: func(context.Context, webdriver.Auth) (*webdriver.Quota, error) {
			probes.Add(1)
			close(entered)
			<-release
			return &webdriver.Quota{Known: false, ProbedAt: time.Now()}, nil
		},
		generate: func(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error) {
			generations.Add(1)
			return &webdriver.GenerateResult{Data: []webdriver.ImageData{{B64JSON: "aW1hZ2U="}}}, nil
		},
	}
	go func() { _, err := s.Probe(context.Background(), shadow.ID); ownerDone <- err }()
	<-entered
	// Another execution observes the occupied parent lease while holding its own
	// independent account slot; publication wakes it without a second probe.
	timer := time.AfterFunc(20*time.Millisecond, func() { close(release) })
	defer timer.Stop()
	_, _, err := s.generate(context.Background(), shadow, &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "test", N: 1})
	require.NoError(t, err)
	require.NoError(t, <-ownerDone)
	require.Equal(t, int32(1), probes.Load())
	require.Equal(t, int32(1), generations.Load())
}

// Service tests use a typed cache double; repository tests exercise the Redis
// ownership scripts and expiry against miniredis.
type fakeWebImageQuotaCache struct {
	mu       sync.Mutex
	statuses map[int64]WebImageStatus
	owners   map[int64]string
	backoffs map[int64]WebImageStatus
}

func (c *fakeWebImageQuotaCache) Get(_ context.Context, id int64) (*WebImageStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status, ok := c.statuses[id]
	if backoff, found := c.backoffs[id]; found && backoff.CooldownUntil.After(time.Now()) {
		status.ParentAccountID = id
		status.CooldownUntil = backoff.CooldownUntil
		status.CooldownReason = backoff.CooldownReason
		ok = true
	}
	if !ok {
		return nil, nil
	}
	return &status, nil
}
func (c *fakeWebImageQuotaCache) AcquireProbe(_ context.Context, id int64, owner string, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owners[id] != "" {
		return false, nil
	}
	c.owners[id] = owner
	return true, nil
}
func (c *fakeWebImageQuotaCache) PublishProbe(_ context.Context, id int64, owner string, status *WebImageStatus) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owners[id] != owner {
		return false, nil
	}
	c.statuses[id] = *status
	return true, nil
}
func (c *fakeWebImageQuotaCache) ReleaseProbe(_ context.Context, id int64, owner string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owners[id] == owner {
		delete(c.owners, id)
	}
	return nil
}
func (c *fakeWebImageQuotaCache) Exhaust(_ context.Context, id int64, status *WebImageStatus) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.owners, id)
	c.statuses[id] = *status
	return nil
}
func (c *fakeWebImageQuotaCache) Delete(_ context.Context, id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.statuses, id)
	return nil
}
func (c *fakeWebImageQuotaCache) replaceOwner(id int64, owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.owners[id] = owner
}
func (c *fakeWebImageQuotaCache) owner(id int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owners[id]
}

func (c *fakeWebImageQuotaCache) Backoff(_ context.Context, id int64, reason string, until time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backoffs == nil {
		c.backoffs = map[int64]WebImageStatus{}
	}
	c.backoffs[id] = WebImageStatus{ParentAccountID: id, CooldownUntil: &until, CooldownReason: reason}
	return nil
}
func (r *webImageTestRepo) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	result := []*Account{}
	for _, id := range ids {
		if a := r.accounts[id]; a != nil {
			result = append(result, a)
		}
	}
	return result, nil
}

func (r *webImageTestRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	var accounts []Account
	for _, a := range r.accounts {
		if a.Platform == platform {
			accounts = append(accounts, *a)
		}
	}
	return accounts, nil
}
func (r *webImageTestRepo) ListByGroup(ctx context.Context, _ int64) ([]Account, error) {
	return r.ListByPlatform(ctx, PlatformOpenAI)
}
func (r *webImageTestRepo) SetRateLimited(_ context.Context, id int64, until time.Time) error {
	r.accounts[id].RateLimitResetAt = &until
	return nil
}
func (r *webImageTestRepo) SetModelRateLimit(_ context.Context, id int64, model string, until time.Time, reason ...string) error {
	if r.accounts[id].Extra == nil {
		r.accounts[id].Extra = map[string]any{}
	}
	r.accounts[id].Extra[modelRateLimitsKey] = map[string]any{model: map[string]any{"rate_limit_reset_at": until.Format(time.RFC3339)}}
	return nil
}
