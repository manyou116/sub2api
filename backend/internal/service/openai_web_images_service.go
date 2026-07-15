package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/google/uuid"
	"github.com/imroc/req/v3"
	"github.com/redis/go-redis/v9"
)

const openAIWebImagesExtraKey = "openai_web_images"

type OpenAIWebImagesService struct {
	cfg         *config.Config
	rdb         *redis.Client
	accountRepo AccountRepository
	driver      *webdriver.Driver
	// accessTokenFn optionally resolves a refreshed access token (wired from OpenAIGatewayService).
	accessTokenFn func(ctx context.Context, account *Account) (string, error)

	memMu       sync.Mutex
	memInflight map[int64]int
	memQuota    map[int64]webImageQuotaCache
	memCooldown map[int64]time.Time
	bulkJobs    sync.Map
}

type webImageQuotaCache struct {
	Remaining int
	ResetAt   *time.Time
	ProbedAt  time.Time
}

type OpenAIWebImagesAccountConfig struct {
	Enabled bool `json:"enabled"`
	// EnabledSet is true when account.extra.openai_web_images explicitly has "enabled".
	// When false, Enabled comes from gateway.openai_web_images.default_enabled (global inherit).
	EnabledSet   bool `json:"-"`
	MaxInflight  int  `json:"max_inflight"`
	Priority     int  `json:"priority"`
	ProbeEnabled bool `json:"probe_enabled"`
	// ModelMode: auto (plan preset) or fixed (custom model/thinking). Empty => inherit global default (auto).
	ModelMode string `json:"model_mode,omitempty"`
	// Model is upstream ChatGPT model slug when model_mode=fixed (e.g. gpt-5-6-thinking).
	Model string `json:"model,omitempty"`
	// ThinkingEffort when model_mode=fixed (extended/high/medium/low/minimal).
	ThinkingEffort string               `json:"thinking_effort,omitempty"`
	Stats          OpenAIWebImagesStats `json:"stats"`
}

type OpenAIWebImagesStats struct {
	Success       int64  `json:"success"`
	Fail          int64  `json:"fail"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastFailAt    string `json:"last_fail_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUsedAt    string `json:"last_used_at,omitempty"`
	// LastRateLimitReason: quota_daily | rate_limit | soft (observability; durable gate is still DB reset_at).
	LastRateLimitReason string `json:"last_rate_limit_reason,omitempty"`
}

type OpenAIWebImagesStatus struct {
	AccountID int64  `json:"account_id"`
	Email     string `json:"email,omitempty"`
	Enabled   bool   `json:"enabled"`
	// EnabledSource: "global" inherits gateway.openai_web_images.default_enabled; "account" is an explicit override.
	EnabledSource string `json:"enabled_source,omitempty"`
	// DefaultEnabled is the current global default (for admin UI).
	DefaultEnabled      bool                 `json:"default_enabled"`
	MaxInflight         int                  `json:"max_inflight"`
	Priority            int                  `json:"priority"`
	ModelMode           string               `json:"model_mode"`
	Model               string               `json:"model,omitempty"`
	ThinkingEffort      string               `json:"thinking_effort,omitempty"`
	PlanType            string               `json:"plan_type,omitempty"`
	ResolvedModel       string               `json:"resolved_model,omitempty"`
	ResolvedEffort      string               `json:"resolved_thinking_effort,omitempty"`
	ResolveSource       string               `json:"resolve_source,omitempty"`
	CurrentInflight     int                  `json:"current_inflight"`
	AvailableSlots      int                  `json:"available_slots"`
	Remaining           *int                 `json:"remaining"`
	ResetAt             *time.Time           `json:"reset_at,omitempty"`
	ProbedAt            *time.Time           `json:"probed_at,omitempty"`
	QuotaKnown          bool                 `json:"quota_known"`
	Schedulable         bool                 `json:"schedulable"`
	UnschedulableReason string               `json:"unschedulable_reason,omitempty"`
	RateLimited         bool                 `json:"rate_limited"`
	CooldownUntil       *time.Time           `json:"cooldown_until,omitempty"`
	CooldownSeconds     int64                `json:"cooldown_seconds,omitempty"`
	Stats               OpenAIWebImagesStats `json:"stats"`
}

type OpenAIWebImagesBulkPatch struct {
	// Enabled sets an explicit per-account override when non-nil.
	Enabled *bool `json:"enabled"`
	// EnabledMode: "inherit" | "on" | "off". When set, takes precedence over Enabled.
	// "inherit" removes the account override so gateway.openai_web_images.default_enabled applies.
	EnabledMode    *string `json:"enabled_mode,omitempty"`
	MaxInflight    *int    `json:"max_inflight"`
	Priority       *int    `json:"priority"`
	ModelMode      *string `json:"model_mode"`
	Model          *string `json:"model"`
	ThinkingEffort *string `json:"thinking_effort"`
}

type OpenAIWebImagesBulkResult struct {
	Matched int                        `json:"matched"`
	Updated int                        `json:"updated"`
	Failed  int                        `json:"failed"`
	Errors  []OpenAIWebImagesBulkError `json:"errors,omitempty"`
	JobID   string                     `json:"probe_job_id,omitempty"`
}

type OpenAIWebImagesBulkError struct {
	AccountID int64  `json:"account_id"`
	Error     string `json:"error"`
}

type OpenAIWebImagesBulkJob struct {
	ID        string                     `json:"id"`
	Type      string                     `json:"type"`
	Total     int                        `json:"total"`
	Done      int                        `json:"done"`
	Success   int                        `json:"success"`
	Failed    int                        `json:"failed"`
	Status    string                     `json:"status"`
	CreatedAt time.Time                  `json:"created_at"`
	UpdatedAt time.Time                  `json:"updated_at"`
	Errors    []OpenAIWebImagesBulkError `json:"errors,omitempty"`
	mu        sync.Mutex
}

func NewOpenAIWebImagesService(cfg *config.Config, rdb *redis.Client, accountRepo AccountRepository) *OpenAIWebImagesService {
	svc := &OpenAIWebImagesService{
		cfg: cfg, rdb: rdb, accountRepo: accountRepo,
		memInflight: map[int64]int{}, memQuota: map[int64]webImageQuotaCache{}, memCooldown: map[int64]time.Time{},
	}
	// Web image SSE/poll/download routinely exceeds the shared privacy client's 30s timeout.
	svc.driver = webdriver.NewDriver(func(proxyURL string) (*req.Client, error) {
		// Note: do not EnableForceHTTP1() together with ImpersonateChrome — ALPN mismatch
		// yields "malformed HTTP response" against chatgpt.com. Prefer longer timeouts +
		// SSE partial-close resilience instead.
		client := req.C().ImpersonateChrome().SetTimeout(15 * time.Minute)
		if trimmed := strings.TrimSpace(proxyURL); trimmed != "" {
			client.SetProxyURL(trimmed)
		}
		return client, nil
	})
	if cfg != nil {
		w := cfg.Gateway.OpenAIWebImages
		if w.PollTimeoutSeconds > 0 {
			svc.driver.PollTimeout = time.Duration(w.PollTimeoutSeconds) * time.Second
		}
		if w.PollIntervalSeconds > 0 {
			svc.driver.PollInterval = time.Duration(w.PollIntervalSeconds) * time.Second
		}
		if w.PollInitialWaitSeconds > 0 {
			svc.driver.PollInitialWait = time.Duration(w.PollInitialWaitSeconds) * time.Second
		}
		// PollInitialWaitSeconds == 0: keep driver default (10s). Operators who need
		// legacy immediate first GET can set a negative sentinel is not supported; use 1s min.
		svc.driver.KeepConversation = w.KeepConversationAfter
	}
	return svc
}

// SetAccessTokenFunc wires token resolution that can refresh OAuth credentials.
func (s *OpenAIWebImagesService) SetAccessTokenFunc(fn func(ctx context.Context, account *Account) (string, error)) {
	if s == nil {
		return
	}
	s.accessTokenFn = fn
}

func (s *OpenAIWebImagesService) cfgOrDefault() config.OpenAIWebImagesConfig {
	var c config.OpenAIWebImagesConfig
	if s != nil && s.cfg != nil {
		c = s.cfg.Gateway.OpenAIWebImages
	}
	if c.DefaultMaxInflight <= 0 {
		c.DefaultMaxInflight = 1
	}
	if c.QuotaCacheTTLSeconds <= 0 {
		c.QuotaCacheTTLSeconds = 300
	}
	if c.RateLimitCooldownSeconds <= 0 {
		c.RateLimitCooldownSeconds = 600
	}
	if c.TransportMaxRetries < 0 {
		c.TransportMaxRetries = 0
	}
	if c.PollTimeoutSeconds <= 0 {
		c.PollTimeoutSeconds = 180
	}
	if c.InflightTTLSeconds <= 0 {
		c.InflightTTLSeconds = 900
	}
	if c.RedisKeyPrefix == "" {
		c.RedisKeyPrefix = "sub2api:webimg:"
	}
	if c.BulkMaxAccounts <= 0 {
		c.BulkMaxAccounts = 5000
	}
	if c.BulkProbeConcurrency <= 0 {
		c.BulkProbeConcurrency = 5
	}
	if c.UnknownQuotaPolicy == "" {
		// optimistic: allow first request while quota has not been probed yet.
		// strict blocks with quota_unknown until admin/manual probe fills the cache.
		c.UnknownQuotaPolicy = "optimistic"
	}
	if c.InflightBackend == "" {
		c.InflightBackend = "redis"
	}
	if c.DefaultModelMode == "" {
		c.DefaultModelMode = "auto"
	}
	if c.DefaultUpstreamModel == "" {
		c.DefaultUpstreamModel = "gpt-5-6-thinking"
	}
	if c.DefaultThinkingEffort == "" {
		c.DefaultThinkingEffort = "extended"
	}
	return c
}

func (s *OpenAIWebImagesService) ParseAccountConfig(account *Account) OpenAIWebImagesAccountConfig {
	g := s.cfgOrDefault()
	cfg := OpenAIWebImagesAccountConfig{
		Enabled:      g.DefaultEnabled,
		EnabledSet:   false,
		MaxInflight:  g.DefaultMaxInflight,
		Priority:     0,
		ProbeEnabled: true,
	}
	if account == nil || account.Extra == nil {
		return cfg
	}
	raw, ok := account.Extra[openAIWebImagesExtraKey]
	if !ok || raw == nil {
		// Legacy single-bool flag is always an explicit account override.
		if v, ok2 := account.Extra["openai_oauth_legacy_images"].(bool); ok2 {
			cfg.Enabled = v
			cfg.EnabledSet = true
		}
		return cfg
	}
	// Treat "enabled" as optional so missing key falls back to global DefaultEnabled
	// (json.Unmarshal would otherwise force false zero-value).
	enabledOverride, hasEnabled := openAIWebImagesEnabledOverride(raw)
	b, _ := json.Marshal(raw)
	_ = json.Unmarshal(b, &cfg)
	if hasEnabled {
		cfg.Enabled = enabledOverride
		cfg.EnabledSet = true
	} else {
		cfg.Enabled = g.DefaultEnabled
		cfg.EnabledSet = false
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = g.DefaultMaxInflight
	}
	cfg.ModelMode = normalizeWebImageModelMode(cfg.ModelMode)
	cfg.ThinkingEffort = strings.TrimSpace(cfg.ThinkingEffort)
	cfg.Model = strings.TrimSpace(cfg.Model)
	return cfg
}

// openAIWebImagesEnabledOverride returns (value, true) when account extra explicitly sets enabled.
func openAIWebImagesEnabledOverride(raw any) (bool, bool) {
	switch v := raw.(type) {
	case map[string]any:
		ev, ok := v["enabled"]
		if !ok {
			return false, false
		}
		switch t := ev.(type) {
		case bool:
			return t, true
		case string:
			s := strings.TrimSpace(strings.ToLower(t))
			if s == "true" || s == "1" || s == "yes" {
				return true, true
			}
			if s == "false" || s == "0" || s == "no" {
				return false, true
			}
		case float64:
			return t != 0, true
		case int:
			return t != 0, true
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return false, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m == nil {
		return false, false
	}
	return openAIWebImagesEnabledOverride(m)
}

func (s *OpenAIWebImagesService) ShouldUseWebPath(account *Account) bool {
	if account == nil {
		return false
	}
	if !account.IsOAuth() || account.Platform != PlatformOpenAI {
		return false
	}
	return s.ParseAccountConfig(account).Enabled
}

func (s *OpenAIWebImagesService) prefix() string { return s.cfgOrDefault().RedisKeyPrefix }
func (s *OpenAIWebImagesService) useRedis() bool {
	return s.cfgOrDefault().InflightBackend != "memory" && s.rdb != nil
}

func (s *OpenAIWebImagesService) GetStatus(ctx context.Context, account *Account) (*OpenAIWebImagesStatus, error) {
	if account == nil {
		return nil, fmt.Errorf("account required")
	}
	cfg := s.ParseAccountConfig(account)
	inflight, _ := s.getInflight(ctx, account.ID)
	quota, known := s.getQuotaCache(ctx, account.ID)
	cooldown, _ := s.getCooldown(ctx, account.ID)
	sel := s.ResolveUpstream(account)
	enabledSource := "global"
	if cfg.EnabledSet {
		enabledSource = "account"
	}
	st := &OpenAIWebImagesStatus{
		AccountID: account.ID, Email: account.GetCredential("email"), Enabled: cfg.Enabled,
		EnabledSource: enabledSource, DefaultEnabled: s.cfgOrDefault().DefaultEnabled,
		MaxInflight: cfg.MaxInflight, Priority: cfg.Priority,
		ModelMode: sel.ModelMode, Model: cfg.Model, ThinkingEffort: cfg.ThinkingEffort,
		PlanType: sel.PlanType, ResolvedModel: sel.UpstreamModel, ResolvedEffort: sel.ThinkingEffort, ResolveSource: sel.Source,
		CurrentInflight: inflight,
		AvailableSlots:  webImgMaxInt(0, cfg.MaxInflight-inflight), QuotaKnown: known, Stats: cfg.Stats,
	}
	if known {
		r := quota.Remaining
		st.Remaining = &r
		st.ResetAt = quota.ResetAt
		t := quota.ProbedAt
		st.ProbedAt = &t
	}
	if !cooldown.IsZero() {
		st.CooldownUntil = &cooldown
		if time.Now().Before(cooldown) {
			st.RateLimited = true
			st.CooldownSeconds = int64(time.Until(cooldown).Seconds())
			if st.CooldownSeconds < 0 {
				st.CooldownSeconds = 0
			}
		}
	}
	st.Schedulable, st.UnschedulableReason = s.evaluateSchedulable(account, cfg, inflight, quota, known, cooldown)
	return st, nil
}

func (s *OpenAIWebImagesService) evaluateSchedulable(account *Account, cfg OpenAIWebImagesAccountConfig, inflight int, quota webImageQuotaCache, known bool, cooldown time.Time) (bool, string) {
	if !cfg.Enabled {
		return false, "disabled"
	}
	if account != nil && account.Status != "" && account.Status != StatusActive {
		return false, "account_inactive"
	}
	if account != nil && account.IsWebImageRateLimited() {
		return false, "cooldown"
	}
	if !cooldown.IsZero() && time.Now().Before(cooldown) {
		return false, "cooldown"
	}
	if inflight >= cfg.MaxInflight {
		return false, "inflight_full"
	}
	if !known {
		if s.cfgOrDefault().UnknownQuotaPolicy == "optimistic" {
			return true, ""
		}
		return false, "quota_unknown"
	}
	if quota.Remaining <= 0 {
		return false, "no_quota"
	}
	return true, ""
}

func (s *OpenAIWebImagesService) PatchAccount(ctx context.Context, accountID int64, patch OpenAIWebImagesBulkPatch) error {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	cfg := s.ParseAccountConfig(account)
	if patch.EnabledMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*patch.EnabledMode))
		switch mode {
		case "inherit", "default", "global":
			cfg.EnabledSet = false
			cfg.Enabled = s.cfgOrDefault().DefaultEnabled
		case "on", "enabled", "true", "1":
			cfg.EnabledSet = true
			cfg.Enabled = true
		case "off", "disabled", "false", "0":
			cfg.EnabledSet = true
			cfg.Enabled = false
		default:
			return fmt.Errorf("enabled_mode must be inherit, on, or off")
		}
	} else if patch.Enabled != nil {
		cfg.Enabled = *patch.Enabled
		cfg.EnabledSet = true
	}
	if patch.MaxInflight != nil {
		if *patch.MaxInflight <= 0 {
			return fmt.Errorf("max_inflight must be > 0")
		}
		cfg.MaxInflight = *patch.MaxInflight
	}
	if patch.Priority != nil {
		cfg.Priority = *patch.Priority
	}
	if patch.ModelMode != nil {
		mode := normalizeWebImageModelMode(*patch.ModelMode)
		if mode != "auto" && mode != "fixed" {
			return fmt.Errorf("model_mode must be auto or fixed")
		}
		cfg.ModelMode = mode
	}
	if patch.Model != nil {
		cfg.Model = strings.TrimSpace(*patch.Model)
	}
	if patch.ThinkingEffort != nil {
		effort := strings.ToLower(strings.TrimSpace(*patch.ThinkingEffort))
		if effort != "" && !isValidWebImageThinkingEffort(effort) {
			return fmt.Errorf("invalid thinking_effort")
		}
		cfg.ThinkingEffort = effort
	}
	if cfg.ModelMode == "fixed" {
		if cfg.Model == "" {
			cfg.Model = s.cfgOrDefault().DefaultUpstreamModel
		}
		if cfg.ThinkingEffort == "" {
			cfg.ThinkingEffort = s.cfgOrDefault().DefaultThinkingEffort
		}
	}
	return s.saveAccountConfig(ctx, accountID, account, cfg)
}

func (s *OpenAIWebImagesService) saveAccountConfig(ctx context.Context, accountID int64, account *Account, cfg OpenAIWebImagesAccountConfig) error {
	payload := map[string]any{
		"max_inflight": cfg.MaxInflight, "priority": cfg.Priority, "probe_enabled": cfg.ProbeEnabled,
		"model_mode": cfg.ModelMode, "model": cfg.Model, "thinking_effort": cfg.ThinkingEffort,
		"stats": map[string]any{
			"success": cfg.Stats.Success, "fail": cfg.Stats.Fail,
			"last_success_at": cfg.Stats.LastSuccessAt, "last_fail_at": cfg.Stats.LastFailAt,
			"last_error": cfg.Stats.LastError, "last_used_at": cfg.Stats.LastUsedAt,
			"last_rate_limit_reason": cfg.Stats.LastRateLimitReason,
		},
	}
	// Omit "enabled" when inheriting global default so fleet-wide GATEWAY_OPENAI_WEB_IMAGES_DEFAULT_ENABLED applies.
	if cfg.EnabledSet {
		payload["enabled"] = cfg.Enabled
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{openAIWebImagesExtraKey: payload})
}

func (s *OpenAIWebImagesService) BulkPatch(ctx context.Context, accountIDs []int64, patch OpenAIWebImagesBulkPatch) (*OpenAIWebImagesBulkResult, error) {
	c := s.cfgOrDefault()
	if len(accountIDs) == 0 {
		return nil, infraerrors.BadRequest("WEIMG_BULK_EMPTY", "account_ids required")
	}
	if len(accountIDs) > c.BulkMaxAccounts {
		return nil, infraerrors.BadRequest(
			"WEIMG_BULK_TOO_MANY",
			fmt.Sprintf("too many accounts: %d (max %d); reduce selection or raise gateway.openai_web_images.bulk_max_accounts", len(accountIDs), c.BulkMaxAccounts),
		)
	}
	result := &OpenAIWebImagesBulkResult{Matched: len(accountIDs)}
	// Concurrent patches: 500 sequential GetByID+UpdateExtra often exceeds admin client 30s timeout
	// and previously surfaced as opaque 500 when only the limit path returned fmt.Errorf.
	workers := c.BulkProbeConcurrency
	if workers <= 0 {
		workers = 8
	}
	if workers > 32 {
		workers = 32
	}
	if workers > len(accountIDs) {
		workers = len(accountIDs)
	}
	type itemErr struct {
		id  int64
		err error
	}
	jobs := make(chan int64, workers)
	errs := make(chan itemErr, len(accountIDs))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if ctx.Err() != nil {
					errs <- itemErr{id: id, err: ctx.Err()}
					continue
				}
				if err := s.PatchAccount(ctx, id, patch); err != nil {
					errs <- itemErr{id: id, err: err}
				} else {
					errs <- itemErr{id: id, err: nil}
				}
			}
		}()
	}
	for _, id := range accountIDs {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e.err != nil {
			result.Failed++
			result.Errors = append(result.Errors, OpenAIWebImagesBulkError{AccountID: e.id, Error: e.err.Error()})
			continue
		}
		result.Updated++
	}
	return result, nil
}

func (s *OpenAIWebImagesService) StartBulkProbe(ctx context.Context, accountIDs []int64) (*OpenAIWebImagesBulkJob, error) {
	c := s.cfgOrDefault()
	if len(accountIDs) == 0 {
		return nil, infraerrors.BadRequest("WEIMG_BULK_EMPTY", "account_ids required")
	}
	if len(accountIDs) > c.BulkMaxAccounts {
		return nil, infraerrors.BadRequest("WEIMG_BULK_TOO_MANY", fmt.Sprintf("too many accounts: %d (max %d)", len(accountIDs), c.BulkMaxAccounts))
	}
	job := &OpenAIWebImagesBulkJob{ID: uuid.NewString(), Type: "probe", Total: len(accountIDs), Status: "running", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	s.bulkJobs.Store(job.ID, job)
	go s.runBulkProbe(job, accountIDs)
	return job, nil
}

func (s *OpenAIWebImagesService) GetBulkJob(id string) (*OpenAIWebImagesBulkJob, bool) {
	v, ok := s.bulkJobs.Load(id)
	if !ok {
		return nil, false
	}
	job, ok := v.(*OpenAIWebImagesBulkJob)
	if !ok || job == nil {
		return nil, false
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	// Copy fields only — never copy the mutex.
	cp := &OpenAIWebImagesBulkJob{
		ID: job.ID, Type: job.Type, Total: job.Total, Done: job.Done,
		Success: job.Success, Failed: job.Failed, Status: job.Status,
		CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
	}
	if len(job.Errors) > 0 {
		cp.Errors = append([]OpenAIWebImagesBulkError(nil), job.Errors...)
	}
	return cp, true
}

func (s *OpenAIWebImagesService) runBulkProbe(job *OpenAIWebImagesBulkJob, ids []int64) {
	c := s.cfgOrDefault()
	sem := make(chan struct{}, c.BulkProbeConcurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		id := id
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			_, err := s.ProbeAccount(ctx, id, true)
			job.mu.Lock()
			job.Done++
			if err != nil {
				job.Failed++
				if len(job.Errors) < 50 {
					job.Errors = append(job.Errors, OpenAIWebImagesBulkError{AccountID: id, Error: err.Error()})
				}
			} else {
				job.Success++
			}
			job.UpdatedAt = time.Now().UTC()
			job.mu.Unlock()
		}()
	}
	wg.Wait()
	job.mu.Lock()
	job.Status = "completed"
	job.UpdatedAt = time.Now().UTC()
	job.mu.Unlock()
}

func (s *OpenAIWebImagesService) ProbeAccount(ctx context.Context, accountID int64, force bool) (*OpenAIWebImagesStatus, error) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !force {
		if _, ok := s.getQuotaCache(ctx, accountID); ok {
			return s.GetStatus(ctx, account)
		}
	}
	if s.useRedis() {
		ok, _ := s.rdb.SetNX(ctx, s.prefix()+"probe:lock:"+strconv.FormatInt(accountID, 10), "1", 45*time.Second).Result()
		if !ok && !force {
			return s.GetStatus(ctx, account)
		}
		defer s.rdb.Del(ctx, s.prefix()+"probe:lock:"+strconv.FormatInt(accountID, 10))
	}
	token, _, err := s.getAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	quota, err := s.driver.ProbeQuota(ctx, webdriver.Auth{AccessToken: token, ProxyURL: proxyURL})
	if err != nil {
		return nil, err
	}
	s.setQuotaCache(ctx, accountID, webImageQuotaCache{Remaining: quota.Remaining, ResetAt: quota.ResetAt, ProbedAt: quota.ProbedAt})
	if quota.Remaining <= 0 && quota.ResetAt != nil {
		s.setCooldown(ctx, accountID, *quota.ResetAt)
	}
	return s.GetStatus(ctx, account)
}

func (s *OpenAIWebImagesService) getAccessToken(ctx context.Context, account *Account) (string, string, error) {
	if account == nil {
		return "", "", fmt.Errorf("account required")
	}
	if s != nil && s.accessTokenFn != nil {
		token, err := s.accessTokenFn(ctx, account)
		if err != nil {
			return "", "", err
		}
		token = strings.TrimSpace(token)
		if token == "" {
			return "", "", fmt.Errorf("account %d missing access_token", account.ID)
		}
		return token, "", nil
	}
	token := strings.TrimSpace(account.GetCredential("access_token"))
	if token == "" {
		token = strings.TrimSpace(account.GetCredential("accessToken"))
	}
	if token == "" {
		// Prefer helper that also understands OpenAI credential aliases.
		token = strings.TrimSpace(account.GetOpenAIAccessToken())
	}
	if token == "" {
		return "", "", fmt.Errorf("account %d missing access_token", account.ID)
	}
	return token, "", nil
}

func (s *OpenAIWebImagesService) Acquire(ctx context.Context, accountID int64, max int, requestID string) (bool, error) {
	if max <= 0 {
		max = s.cfgOrDefault().DefaultMaxInflight
	}
	if s.useRedis() {
		return s.redisAcquire(ctx, accountID, max, requestID)
	}
	s.memMu.Lock()
	defer s.memMu.Unlock()
	if s.memInflight[accountID] >= max {
		return false, nil
	}
	s.memInflight[accountID]++
	return true, nil
}

func (s *OpenAIWebImagesService) Release(ctx context.Context, accountID int64, requestID string) {
	if s.useRedis() {
		rctx := ctx
		if rctx == nil || rctx.Err() != nil {
			rctx = context.Background()
		}
		_ = s.redisRelease(rctx, accountID, requestID)
		return
	}
	s.memMu.Lock()
	defer s.memMu.Unlock()
	if s.memInflight[accountID] <= 1 {
		delete(s.memInflight, accountID)
	} else {
		s.memInflight[accountID]--
	}
}

func (s *OpenAIWebImagesService) redisAcquire(ctx context.Context, accountID int64, max int, requestID string) (bool, error) {
	key := s.prefix() + "inflight:" + strconv.FormatInt(accountID, 10)
	ttl := time.Duration(s.cfgOrDefault().InflightTTLSeconds) * time.Second
	script := redis.NewScript(`
local n = tonumber(redis.call("GET", KEYS[1]) or "0")
local max = tonumber(ARGV[1])
if n >= max then return 0 end
n = redis.call("INCR", KEYS[1])
redis.call("EXPIRE", KEYS[1], ARGV[2])
if ARGV[3] ~= "" then
  redis.call("SADD", KEYS[2], ARGV[3])
  redis.call("EXPIRE", KEYS[2], ARGV[2])
end
return 1
`)
	reqKey := s.prefix() + "inflight:" + strconv.FormatInt(accountID, 10) + ":reqs"
	res, err := script.Run(ctx, s.rdb, []string{key, reqKey}, max, int(ttl.Seconds()), requestID).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

func (s *OpenAIWebImagesService) redisRelease(ctx context.Context, accountID int64, requestID string) error {
	key := s.prefix() + "inflight:" + strconv.FormatInt(accountID, 10)
	reqKey := s.prefix() + "inflight:" + strconv.FormatInt(accountID, 10) + ":reqs"
	script := redis.NewScript(`
if ARGV[1] ~= "" then
  redis.call("SREM", KEYS[2], ARGV[1])
end
local n = tonumber(redis.call("GET", KEYS[1]) or "0")
if n <= 1 then
  redis.call("DEL", KEYS[1])
  return 0
end
return redis.call("DECR", KEYS[1])
`)
	return script.Run(ctx, s.rdb, []string{key, reqKey}, requestID).Err()
}

// GetInflightBatch returns current web-image inflight counts for account IDs (best-effort).
func (s *OpenAIWebImagesService) GetInflightBatch(ctx context.Context, accountIDs []int64) map[int64]int {
	out := make(map[int64]int, len(accountIDs))
	if s == nil || len(accountIDs) == 0 {
		return out
	}
	if s.useRedis() {
		pipe := s.rdb.Pipeline()
		type cmd struct {
			id  int64
			get *redis.StringCmd
		}
		cmds := make([]cmd, 0, len(accountIDs))
		for _, id := range accountIDs {
			if id <= 0 {
				continue
			}
			key := s.prefix() + "inflight:" + strconv.FormatInt(id, 10)
			cmds = append(cmds, cmd{id: id, get: pipe.Get(ctx, key)})
		}
		_, _ = pipe.Exec(ctx)
		for _, c := range cmds {
			n, err := c.get.Int()
			if err == nil && n > 0 {
				out[c.id] = n
			}
		}
		return out
	}
	s.memMu.Lock()
	defer s.memMu.Unlock()
	for _, id := range accountIDs {
		if n := s.memInflight[id]; n > 0 {
			out[id] = n
		}
	}
	return out
}

func (s *OpenAIWebImagesService) getInflight(ctx context.Context, accountID int64) (int, error) {
	if s.useRedis() {
		n, err := s.rdb.Get(ctx, s.prefix()+"inflight:"+strconv.FormatInt(accountID, 10)).Int()
		if err == redis.Nil {
			return 0, nil
		}
		return n, err
	}
	s.memMu.Lock()
	defer s.memMu.Unlock()
	return s.memInflight[accountID], nil
}

func (s *OpenAIWebImagesService) getQuotaCache(ctx context.Context, accountID int64) (webImageQuotaCache, bool) {
	ttl := time.Duration(s.cfgOrDefault().QuotaCacheTTLSeconds) * time.Second
	if s.useRedis() {
		key := s.prefix() + "quota:" + strconv.FormatInt(accountID, 10)
		m, err := s.rdb.HGetAll(ctx, key).Result()
		if err != nil || len(m) == 0 {
			return webImageQuotaCache{}, false
		}
		q := webImageQuotaCache{}
		q.Remaining, _ = strconv.Atoi(m["remaining"])
		if v := m["probed_at"]; v != "" {
			if t, e := time.Parse(time.RFC3339, v); e == nil {
				q.ProbedAt = t
			}
		}
		if v := m["reset_at"]; v != "" {
			if t, e := time.Parse(time.RFC3339, v); e == nil {
				q.ResetAt = &t
			}
		}
		if q.ProbedAt.IsZero() || time.Since(q.ProbedAt) > ttl {
			return q, false
		}
		return q, true
	}
	s.memMu.Lock()
	defer s.memMu.Unlock()
	q, ok := s.memQuota[accountID]
	if !ok || q.ProbedAt.IsZero() || time.Since(q.ProbedAt) > ttl {
		return webImageQuotaCache{}, false
	}
	return q, true
}

func (s *OpenAIWebImagesService) setQuotaCache(ctx context.Context, accountID int64, q webImageQuotaCache) {
	ttl := time.Duration(s.cfgOrDefault().QuotaCacheTTLSeconds) * time.Second
	if s.useRedis() {
		key := s.prefix() + "quota:" + strconv.FormatInt(accountID, 10)
		fields := map[string]any{"remaining": q.Remaining, "probed_at": q.ProbedAt.UTC().Format(time.RFC3339)}
		if q.ResetAt != nil {
			fields["reset_at"] = q.ResetAt.UTC().Format(time.RFC3339)
		}
		_ = s.rdb.HSet(ctx, key, fields).Err()
		_ = s.rdb.Expire(ctx, key, ttl).Err()
		return
	}
	s.memMu.Lock()
	s.memQuota[accountID] = q
	s.memMu.Unlock()
}

func (s *OpenAIWebImagesService) getCooldown(ctx context.Context, accountID int64) (time.Time, bool) {
	if accountID <= 0 {
		return time.Time{}, false
	}
	now := time.Now()
	// 1) Hot cache (Redis / memory) — may be missing after restart; never sole truth.
	if t, ok := s.getCooldownCache(ctx, accountID); ok && now.Before(t) {
		return t, true
	}
	// 2) Durable Postgres columns (source of truth).
	if s.accountRepo != nil {
		if acc, err := s.accountRepo.GetByID(ctx, accountID); err == nil && acc != nil {
			if acc.WebImageRateLimitResetAt != nil && now.Before(*acc.WebImageRateLimitResetAt) {
				until := acc.WebImageRateLimitResetAt.UTC()
				s.setCooldownCache(ctx, accountID, until)
				return until, true
			}
		}
	}
	return time.Time{}, false
}

func (s *OpenAIWebImagesService) getCooldownCache(ctx context.Context, accountID int64) (time.Time, bool) {
	if s.useRedis() {
		v, err := s.rdb.Get(ctx, s.prefix()+"cooldown:"+strconv.FormatInt(accountID, 10)).Result()
		if err != nil {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil || time.Now().After(t) {
			return time.Time{}, false
		}
		return t, true
	}
	s.memMu.Lock()
	defer s.memMu.Unlock()
	t, ok := s.memCooldown[accountID]
	if !ok || time.Now().After(t) {
		return time.Time{}, false
	}
	return t, true
}

func (s *OpenAIWebImagesService) setCooldownCache(ctx context.Context, accountID int64, until time.Time) {
	if until.IsZero() {
		return
	}
	if s.useRedis() {
		ttl := time.Until(until)
		if ttl <= 0 {
			return
		}
		_ = s.rdb.Set(ctx, s.prefix()+"cooldown:"+strconv.FormatInt(accountID, 10), until.UTC().Format(time.RFC3339), ttl).Err()
		return
	}
	s.memMu.Lock()
	s.memCooldown[accountID] = until
	s.memMu.Unlock()
}

func (s *OpenAIWebImagesService) setCooldown(ctx context.Context, accountID int64, until time.Time) {
	if until.IsZero() || accountID <= 0 {
		return
	}
	until = until.UTC()
	// Durable first — survives restarts and multi-instance without shared hot cache.
	if s.accountRepo != nil {
		if err := s.accountRepo.SetWebImageRateLimited(ctx, accountID, until); err != nil {
			// Fall through to cache so same-process still enforces.
			logger.LegacyPrintf("service.openai_web_images", "set web image rate limit failed account=%d err=%v", accountID, err)
		}
	}
	s.setCooldownCache(ctx, accountID, until)
}

// ClearCooldown removes web-image rate-limit/cooldown state for an account.
func (s *OpenAIWebImagesService) ClearCooldown(ctx context.Context, accountID int64) error {
	if s == nil || accountID <= 0 {
		return nil
	}
	// Never depend on a request context that may already be canceled after batch work.
	rctx := context.Background()
	// Durable DB truth first.
	if s.accountRepo != nil {
		_ = s.accountRepo.ClearWebImageRateLimit(rctx, accountID)
	}
	if s.useRedis() {
		keyCooldown := s.prefix() + "cooldown:" + strconv.FormatInt(accountID, 10)
		keyQuota := s.prefix() + "quota:" + strconv.FormatInt(accountID, 10)
		_ = s.rdb.Del(rctx, keyCooldown, keyQuota).Err()
	}
	s.memMu.Lock()
	delete(s.memCooldown, accountID)
	delete(s.memQuota, accountID)
	s.memMu.Unlock()

	// Clear sticky last_error in account.extra so admin UI stops showing old Free-plan text.
	if s.accountRepo != nil {
		if acc, err := s.accountRepo.GetByID(rctx, accountID); err == nil && acc != nil {
			cfg := s.ParseAccountConfig(acc)
			if cfg.Stats.LastError != "" {
				cfg.Stats.LastError = ""
				_ = s.saveAccountConfig(rctx, accountID, acc, cfg)
			}
		}
	}
	_ = ctx
	return nil
}

// BulkClearCooldown clears web-image cooldown/quota cache for many accounts.
func (s *OpenAIWebImagesService) BulkClearCooldown(ctx context.Context, accountIDs []int64) (int, error) {
	if s == nil {
		return 0, nil
	}
	n := 0
	for _, id := range accountIDs {
		if id <= 0 {
			continue
		}
		if err := s.ClearCooldown(ctx, id); err != nil {
			continue
		}
		n++
	}
	return n, nil
}

// IsWebRateLimited reports whether the account is currently in web-image cooldown.
func (s *OpenAIWebImagesService) IsWebRateLimited(ctx context.Context, accountID int64) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	until, ok := s.getCooldown(ctx, accountID)
	return ok && !until.IsZero() && time.Now().Before(until)
}

func (s *OpenAIWebImagesService) MarkSuccess(ctx context.Context, account *Account) {
	if account == nil {
		return
	}
	cfg := s.ParseAccountConfig(account)
	cfg.Stats.Success++
	cfg.Stats.LastSuccessAt = time.Now().UTC().Format(time.RFC3339)
	cfg.Stats.LastUsedAt = cfg.Stats.LastSuccessAt
	_ = s.saveAccountConfig(ctx, account.ID, account, cfg)
	if s.cfgOrDefault().SuccessDecrementLocal {
		if q, ok := s.getQuotaCache(ctx, account.ID); ok {
			if q.Remaining > 0 {
				q.Remaining--
			}
			q.ProbedAt = time.Now().UTC()
			s.setQuotaCache(ctx, account.ID, q)
		}
	}
}

func (s *OpenAIWebImagesService) MarkFail(ctx context.Context, account *Account, errMsg string, rateLimited bool) {
	if account == nil {
		return
	}
	cfg := s.ParseAccountConfig(account)
	cfg.Stats.Fail++
	cfg.Stats.LastFailAt = time.Now().UTC().Format(time.RFC3339)
	cfg.Stats.LastUsedAt = cfg.Stats.LastFailAt
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	cfg.Stats.LastError = errMsg
	if rateLimited {
		reason := classifyWebImageRateLimitReason(errMsg)
		cfg.Stats.LastRateLimitReason = reason
		// Soft transport throttle: short Redis-only cool-down; do NOT zero quota or durable-pin.
		if reason == "soft" {
			softUntil := time.Now().UTC().Add(90 * time.Second)
			s.setCooldownCache(ctx, account.ID, softUntil)
			_ = s.saveAccountConfig(ctx, account.ID, account, cfg)
			return
		}
		s.setQuotaCache(ctx, account.ID, webImageQuotaCache{Remaining: 0, ProbedAt: time.Now().UTC()})
		// Hard / daily quota: durable DB reset_at (aligned with text rate_limit_reset_at design).
		until := time.Now().Add(time.Duration(s.cfgOrDefault().RateLimitCooldownSeconds) * time.Second)
		if d := parseWebImageResetDuration(errMsg); d > 0 {
			if cand := time.Now().Add(d); cand.After(until) {
				until = cand
			}
		}
		s.setCooldown(ctx, account.ID, until)
	}
	_ = s.saveAccountConfig(ctx, account.ID, account, cfg)
}

// classifyWebImageRateLimitReason mirrors text-channel thinking: durable window vs short fuse.
// quota_daily = plan image cap with reset hint; rate_limit = hard image 429; soft = poll/CF noise.
func classifyWebImageRateLimitReason(errMsg string) string {
	msg := strings.ToLower(strings.TrimSpace(errMsg))
	// Soft transport throttle (must not durable-pin accounts for ~day).
	if strings.Contains(msg, "soft poll") ||
		strings.Contains(msg, "conversation poll rate limited") ||
		strings.Contains(msg, "conversation get 429") {
		return "soft"
	}
	// Daily / plan image quota — prefer upstream reset duration when present.
	if parseWebImageResetDuration(errMsg) > 0 ||
		strings.Contains(msg, "free plan limit") ||
		strings.Contains(msg, "plan limit for image") ||
		strings.Contains(msg, "image generation limit") ||
		(strings.Contains(msg, "image generations") && strings.Contains(msg, "limit")) {
		return "quota_daily"
	}
	// Bare "too many requests" without image-quota phrasing → soft.
	if strings.Contains(msg, "too many requests") &&
		!strings.Contains(msg, "image") &&
		parseWebImageResetDuration(errMsg) == 0 {
		return "soft"
	}
	return "rate_limit"
}

// parseWebImageResetDuration extracts "resets in 23 hours and 5 minutes" style hints.
func parseWebImageResetDuration(msg string) time.Duration {
	if strings.TrimSpace(msg) == "" {
		return 0
	}
	var hours, minutes, days int
	// English
	re := regexp.MustCompile(`(?i)(\d+)\s*(day|days|hour|hours|minute|minutes)`)
	for _, m := range re.FindAllStringSubmatch(msg, -1) {
		n, _ := strconv.Atoi(m[1])
		switch strings.ToLower(m[2]) {
		case "day", "days":
			days += n
		case "hour", "hours":
			hours += n
		case "minute", "minutes":
			minutes += n
		}
	}
	// Chinese
	reCN := regexp.MustCompile(`(\d+)\s*(天|小时|分钟)`)
	for _, m := range reCN.FindAllStringSubmatch(msg, -1) {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "天":
			days += n
		case "小时":
			hours += n
		case "分钟":
			minutes += n
		}
	}
	total := time.Duration(days)*24*time.Hour + time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute
	if total > 0 && total < time.Minute {
		total = time.Minute
	}
	if total > 48*time.Hour {
		total = 48 * time.Hour
	}
	return total
}

// WebImageUpstreamSelection is the resolved ChatGPT web model + thinking effort for one request.
type WebImageUpstreamSelection struct {
	PlanType       string
	ModelMode      string // auto|fixed
	UpstreamModel  string
	ThinkingEffort string
	Source         string // auto_plan|account_fixed|global_default
}

func normalizeWebImageModelMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "fixed", "custom", "manual":
		return "fixed"
	case "auto", "preset", "inherit", "":
		// empty kept as empty for account-level "inherit global"
		if strings.TrimSpace(v) == "" {
			return ""
		}
		return "auto"
	default:
		return "auto"
	}
}

func isValidWebImageThinkingEffort(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "none", "minimal", "low", "standard", "medium", "high", "extended", "max", "pro", "xhigh":
		return true
	default:
		return false
	}
}

func webImagePlanPreset(planType string) (model, effort string) {
	// Auto mode keeps the verified web image path across plans.
	// Pro plan can still override to gpt-5-*-pro + pro effort via fixed mode.
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "free":
		return "gpt-5-6-thinking", "extended"
	case "pro":
		return "gpt-5-6-thinking", "extended"
	case "team", "plus", "go":
		return "gpt-5-6-thinking", "extended"
	default:
		return "gpt-5-6-thinking", "extended"
	}
}

// ResolveUpstream picks ChatGPT web model + thinking_effort.
// Priority: account fixed > global fixed > plan preset (auto).
func (s *OpenAIWebImagesService) ResolveUpstream(account *Account) WebImageUpstreamSelection {
	g := s.cfgOrDefault()
	cfg := OpenAIWebImagesAccountConfig{}
	if account != nil {
		cfg = s.ParseAccountConfig(account)
	}
	plan := ""
	if account != nil {
		plan = strings.ToLower(strings.TrimSpace(account.GetCredential("plan_type")))
	}
	presetModel, presetEffort := webImagePlanPreset(plan)
	// Prefer global defaults as baseline for unknown plans.
	if presetModel == "gpt-5-6-thinking" && strings.TrimSpace(g.DefaultUpstreamModel) != "" {
		presetModel = strings.TrimSpace(g.DefaultUpstreamModel)
	}
	if presetEffort == "extended" && strings.TrimSpace(g.DefaultThinkingEffort) != "" && isValidWebImageThinkingEffort(g.DefaultThinkingEffort) {
		presetEffort = strings.ToLower(strings.TrimSpace(g.DefaultThinkingEffort))
	}

	mode := normalizeWebImageModelMode(cfg.ModelMode)
	if mode == "" {
		mode = normalizeWebImageModelMode(g.DefaultModelMode)
	}
	if mode == "" {
		mode = "auto"
	}

	sel := WebImageUpstreamSelection{
		PlanType: plan, ModelMode: mode,
		UpstreamModel: presetModel, ThinkingEffort: presetEffort, Source: "auto_plan",
	}
	if mode != "fixed" {
		return sel
	}

	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = strings.TrimSpace(g.DefaultUpstreamModel)
	}
	if model == "" {
		model = presetModel
	}
	effort := strings.ToLower(strings.TrimSpace(cfg.ThinkingEffort))
	if !isValidWebImageThinkingEffort(effort) {
		effort = strings.ToLower(strings.TrimSpace(g.DefaultThinkingEffort))
	}
	if !isValidWebImageThinkingEffort(effort) {
		effort = presetEffort
	}
	sel.UpstreamModel = model
	sel.ThinkingEffort = effort
	if strings.TrimSpace(cfg.Model) != "" || strings.TrimSpace(cfg.ThinkingEffort) != "" {
		sel.Source = "account_fixed"
	} else {
		sel.Source = "global_default"
	}
	return sel
}

func (s *OpenAIWebImagesService) Driver() *webdriver.Driver { return s.driver }

func (s *OpenAIWebImagesService) Overview(ctx context.Context, ids []int64) ([]OpenAIWebImagesStatus, error) {
	out := make([]OpenAIWebImagesStatus, 0, len(ids))
	for _, id := range ids {
		acc, err := s.accountRepo.GetByID(ctx, id)
		if err != nil {
			continue
		}
		st, err := s.GetStatus(ctx, acc)
		if err != nil {
			continue
		}
		out = append(out, *st)
	}
	return out, nil
}

func webImgMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
