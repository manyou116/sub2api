package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/openaiimages/webdriver"
	"github.com/google/uuid"
)

// WebImageStateRepository extends the account repository without coupling other
// account implementations to a protocol-specific resource window.
type WebImageStateRepository interface {
	WebImageCooldown(context.Context, int64) (*time.Time, error)
	SetWebImageCooldown(context.Context, int64, time.Time) error
}

// WebImageQuotaCache stores parent-scoped snapshots and fences probe ownership.
// Get returns nil for a missing snapshot. Exhaust invalidates any older probe.
type WebImageQuotaCache interface {
	Get(context.Context, int64) (*WebImageStatus, error)
	Backoff(context.Context, int64, string, time.Time) error
	AcquireProbe(context.Context, int64, string, time.Duration) (bool, error)
	PublishProbe(context.Context, int64, string, *WebImageStatus) (bool, error)
	ReleaseProbe(context.Context, int64, string) error
	Exhaust(context.Context, int64, *WebImageStatus) error
	Delete(context.Context, int64) error
}

type webImageDriver interface {
	Generate(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error)
	Edit(context.Context, webdriver.Auth, webdriver.GenerateRequest) (*webdriver.GenerateResult, error)
	ProbeQuota(context.Context, webdriver.Auth) (*webdriver.Quota, error)
}

type WebImageStatus struct {
	ParentAccountID   int64      `json:"parent_account_id"`
	QuotaKnown        bool       `json:"quota_known"`
	Remaining         *int       `json:"remaining"`
	ResetAt           *time.Time `json:"reset_at"`
	ProbedAt          *time.Time `json:"probed_at"`
	CooldownUntil     *time.Time `json:"cooldown_until"`
	CooldownReason    string     `json:"cooldown_reason,omitempty"`
	RetryAfterSeconds int        `json:"retry_after_seconds,omitempty"`
}

type OpenAIWebImageService struct {
	accounts   AccountRepository
	gateway    *OpenAIGatewayService
	quotaCache WebImageQuotaCache
	driver     webImageDriver
}

func NewOpenAIWebImageService(accounts AccountRepository, quotaCache WebImageQuotaCache, gateway *OpenAIGatewayService) *OpenAIWebImageService {
	s := &OpenAIWebImageService{accounts: accounts, quotaCache: quotaCache, gateway: gateway, driver: webdriver.NewDriver(newWebImageClientPool().client)}
	gateway.webImages = s
	return s
}

var errWebImageProbeBusy = errors.New("web image quota probe already running")

func (s *OpenAIWebImageService) parent(ctx context.Context, account *Account) (*Account, error) {
	if account == nil || !account.IsWebImageShadow() {
		return nil, errors.New("account is not a Web image shadow")
	}
	if snapshot, ok := ctx.Value(webImageCandidateKey{}).(*webImageCandidateState); ok {
		parent := snapshot.parents[*account.ParentAccountID]
		if err := ValidateWebImageShadowParent(parent); err != nil {
			return nil, err
		}
		return parent, nil
	}
	parent, err := resolveCredentialAccount(ctx, s.accounts, account)
	if err != nil {
		return nil, err
	}
	return parent, nil
}

func (s *OpenAIWebImageService) Status(ctx context.Context, id int64) (*WebImageStatus, error) {
	account, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	parent, err := s.parent(ctx, account)
	if err != nil {
		return nil, err
	}
	return s.parentStatus(ctx, parent.ID)
}

func (s *OpenAIWebImageService) parentStatus(ctx context.Context, id int64) (*WebImageStatus, error) {
	if snapshot, ok := ctx.Value(webImageCandidateKey{}).(*webImageCandidateState); ok {
		if snapshot.err != nil {
			return nil, snapshot.err
		}
		if status := snapshot.statuses[id]; status != nil {
			return status, nil
		}
	}
	statuses, err := s.parentStatuses(ctx, []int64{id})
	if err != nil {
		return nil, err
	}
	return statuses[id], nil
}

func (s *OpenAIWebImageService) auth(ctx context.Context, parent *Account) (webdriver.Auth, error) {
	token, _, err := s.gateway.GetAccessToken(ctx, parent)
	if err != nil {
		return webdriver.Auth{}, err
	}
	device := parent.GetOpenAIDeviceID()
	if device == "" {
		device = uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("sub2api:webimg:%d", parent.ID))).String()
	}
	auth := webdriver.Auth{AccessToken: token, AccountID: parent.GetChatGPTAccountID(), UserAgent: parent.GetOpenAIUserAgent(), DeviceID: device, SessionID: parent.GetOpenAISessionID(), ClientProfile: parent.GetExtraString("openai_web_images_client_profile")}
	if parent.Proxy != nil {
		auth.ProxyURL = parent.Proxy.URL()
	}
	return auth, nil
}

// The lease is fenced both when publishing and releasing. A late probe can
// neither overwrite a newer snapshot nor unlock another caller's probe.
func (s *OpenAIWebImageService) Probe(ctx context.Context, id int64) (*WebImageStatus, error) {
	account, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	parent, err := s.parent(ctx, account)
	if err != nil {
		return nil, err
	}
	if !parent.IsCredentialUsableForWebImages() {
		return nil, errors.New("web image parent credentials are unavailable")
	}
	if s.quotaCache == nil {
		return nil, errors.New("web image quota cache unavailable")
	}
	observed, err := s.readCooldowns(ctx, []int64{parent.ID})
	if err != nil {
		return nil, err
	}
	owner := uuid.NewString()
	acquired, err := s.quotaCache.AcquireProbe(ctx, parent.ID, owner, 45*time.Second)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errWebImageProbeBusy
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.quotaCache.ReleaseProbe(releaseCtx, parent.ID, owner)
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	auth, err := s.auth(ctx, parent)
	if err != nil {
		return nil, err
	}
	quota, err := s.driver.ProbeQuota(ctx, auth)
	if err != nil {
		s.redactDriverError(err, parent)
		s.observeError(ctx, parent, resolveWebImageOptions(account).Model, err)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if quota == nil {
		return nil, errors.New("empty Web image quota response")
	}
	status := &WebImageStatus{ParentAccountID: parent.ID, QuotaKnown: quota.Known, ResetAt: quota.ResetAt, ProbedAt: &quota.ProbedAt}
	if quota.Known {
		remaining := quota.Remaining
		status.Remaining = &remaining
	}
	written, err := s.quotaCache.PublishProbe(ctx, parent.ID, owner, status)
	if err != nil {
		return nil, err
	}
	if !written {
		return nil, errors.New("web image quota probe lease expired")
	}
	if quota.Known && quota.Remaining > 0 {
		if repo, ok := s.accounts.(webImageCooldownRepository); ok {
			if err := repo.ClearWebImageCooldownIfObserved(ctx, parent.ID, observed[parent.ID]); err != nil {
				return nil, err
			}
		}
	}
	return s.parentStatus(ctx, parent.ID)
}

// A busy probe is local coordination, not an upstream failure. Briefly share
// its result, then retain the normal unknown-quota policy if it is still pending.
func (s *OpenAIWebImageService) waitForQuotaProbe(ctx context.Context, parentID int64) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := s.parentStatus(ctx, parentID)
		if err != nil {
			return err
		}
		if status.ProbedAt != nil || status.CooldownUntil != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
		}
	}
}

type webImageOptions struct {
	ModelMode                 string `json:"model_mode"`
	Model                     string `json:"model"`
	ThinkingEffort            string `json:"thinking_effort"`
	RequireDownloadAttachment *bool  `json:"require_download_attachment"`
}

func resolveWebImageOptions(account *Account) webImageOptions {
	options := webImageOptions{ModelMode: "auto", Model: "gpt-5-6-thinking", ThinkingEffort: "extended"}
	if account != nil {
		if raw, err := json.Marshal(account.Extra["openai_web_images"]); err == nil {
			_ = json.Unmarshal(raw, &options)
		}
	}
	if options.ModelMode != "fixed" || strings.TrimSpace(options.Model) == "" {
		options.Model = "gpt-5-6-thinking"
	}
	if options.Model == "auto" {
		options.ThinkingEffort = ""
	}
	return options
}

func (s *OpenAIWebImageService) eligible(ctx context.Context, account *Account) (*Account, error) {
	if account == nil || !account.IsWebImageShadow() || !account.IsSchedulable() || account.Concurrency <= 0 {
		return nil, errors.New("web image shadow is not schedulable")
	}
	parent, err := s.parent(ctx, account)
	if err != nil {
		return nil, err
	}
	if !parent.IsCredentialUsableForWebImages() || s.gateway.isOpenAIProxyStreamQuarantined(ctx, parent) {
		return nil, errors.New("web image parent credentials are unavailable")
	}
	options := resolveWebImageOptions(account)
	if webdriver.IsWorkModeModel(options.Model) {
		// Manual text pause does not disable a child; actual model quota and runtime
		// restrictions still apply to the shared upstream Work Mode identity.
		copyParent := *parent
		copyParent.Schedulable = true
		model := options.Model
		if IsOpenAICapacityCooldownReason(copyParent.TempUnschedulableReason) {
			copyParent.TempUnschedulableUntil = nil
		}
		if !copyParent.IsSchedulableForModelWithContext(ctx, model) || s.gateway.isOpenAIAccountModelRuntimeBlocked(parent, model) {
			return nil, errors.New("web Work Mode parent text quota is unavailable")
		}
		if paused, _ := shouldAutoPauseOpenAIAccountByQuota(ctx, parent); paused {
			return nil, errors.New("web Work Mode parent text quota is exhausted")
		}
	}
	status, err := s.parentStatus(ctx, parent.ID)
	if err != nil {
		return nil, err
	}
	if status.CooldownUntil != nil || (status.QuotaKnown && status.Remaining != nil && *status.Remaining <= 0) {
		return nil, webImageUnavailableError(status)
	}
	return parent, nil
}
