package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/google/uuid"
)

// OpenAICapacityCooldownStore is an optional extension of the existing Redis store.
// No quota or account authentication state is changed by this circuit.
type OpenAICapacityCooldownStore interface {
	AdmitOpenAICapacity(context.Context, int64, string, string, int) (OpenAICapacityDecision, error)
	ObserveOpenAICapacity(context.Context, int64, string, string, int64, int, OpenAICapacityCooldownSettings) (OpenAICapacityDecision, error)
	ReleaseOpenAICapacity(context.Context, int64, string, string, int64) error
	BlockedOpenAICapacityAccounts(context.Context, string) ([]int64, error)
}

type OpenAICapacityDecision struct {
	Allowed, Probe, Tripped bool
	Generation, UntilMS     int64
	Failures, Total, Level  int64
}

type openAICapacityAttempt struct {
	store      OpenAICapacityCooldownStore
	settings   OpenAICapacityCooldownSettings
	model, id  string
	generation int64
}

type openAICapacityContextKey struct{}
type openAICapacityRequest struct {
	store    OpenAICapacityCooldownStore
	settings OpenAICapacityCooldownSettings
	compact  bool
	mu       sync.Mutex
	blocked  map[string]map[int64]bool
}

func (s *OpenAIGatewayService) selectAccountWithScheduler(
	ctx context.Context, groupID *int64, previousResponseID, sessionHash, requestedModel string,
	excludedIDs map[int64]struct{}, requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability,
	requireCompact bool, platform string, previousResponseCanMove, useUpstreamTokenCost bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	// A WebSocket ingress connection contains multiple independent turns; its
	// connection ID must not be mistaken for an HTTP logical request ID.
	if requiredImageCapability == "" && requiredTransport != OpenAIUpstreamTransportResponsesWebsocketV2Ingress && !OpenAIImageGenerationIntentFromContext(ctx) {
		ctx = s.withOpenAICapacityCooldown(ctx, requireCompact)
	}
	effective := excludedIDs
	for {
		selection, decision, err := s.selectAccountWithSchedulerBase(ctx, groupID, previousResponseID, sessionHash, requestedModel,
			effective, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost)
		if err != nil || selection == nil || selection.Account == nil || s.admitOpenAICapacity(ctx, selection, requestedModel) {
			return selection, decision, err
		}
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		if _, repeated := effective[selection.Account.ID]; repeated {
			return nil, decision, ErrNoAvailableAccounts
		}
		if len(effective) == len(excludedIDs) {
			effective = cloneExcludedAccountIDs(excludedIDs)
		}
		if effective == nil {
			effective = make(map[int64]struct{})
		}
		effective[selection.Account.ID] = struct{}{}
	}
}

func (s *OpenAIGatewayService) withOpenAICapacityCooldown(ctx context.Context, compact bool) context.Context {
	if ctx.Value(openAICapacityContextKey{}) != nil || s == nil || s.rateLimitService == nil {
		return ctx
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	settings, err := s.settingService.GetOpenAICapacityCooldownSettings(readCtx)
	store, ok := s.rateLimitService.tempUnschedCache.(OpenAICapacityCooldownStore)
	if err != nil || !settings.Enabled || !ok {
		return ctx
	}
	return context.WithValue(ctx, openAICapacityContextKey{}, &openAICapacityRequest{store: store, settings: settings, compact: compact, blocked: make(map[string]map[int64]bool)})
}

func openAICapacityEligible(account *Account, model string) bool {
	return isOpenAIOAuthAccount(account) && !account.IsShadow() && model != "" && !isOpenAIImageGenerationModel(model)
}

// Fetch only active blocked IDs once per canonical model and selection pass, not
// once per pool candidate. Admission below closes snapshot/concurrency races.
func openAICapacityBlocked(ctx context.Context, account *Account, requestedModel string) bool {
	v, _ := ctx.Value(openAICapacityContextKey{}).(*openAICapacityRequest)
	if v == nil || !openAICapacityEligible(account, requestedModel) {
		return false
	}
	model := normalizeOpenAIAccountModelTransientModel(resolveOpenAIAccountUpstreamModelForRequest(account, requestedModel, v.compact))
	if !openAICapacityEligible(account, model) {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	blocked, found := v.blocked[model]
	if !found {
		blocked = make(map[int64]bool)
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		ids, err := v.store.BlockedOpenAICapacityAccounts(readCtx, model)
		if err != nil {
			slog.Warn("openai.capacity_cooldown_read_failed", "error", err)
		}
		for _, id := range ids {
			blocked[id] = true
		}
		v.blocked[model] = blocked
	}
	return blocked[account.ID]
}

func (s *OpenAIGatewayService) admitOpenAICapacity(ctx context.Context, selection *AccountSelectionResult, requestedModel string) bool {
	v, _ := ctx.Value(openAICapacityContextKey{}).(*openAICapacityRequest)
	if v == nil || selection == nil || !openAICapacityEligible(selection.Account, requestedModel) {
		return true
	}
	model := normalizeOpenAIAccountModelTransientModel(resolveOpenAIAccountUpstreamModelForRequest(selection.Account, requestedModel, v.compact))
	if !openAICapacityEligible(selection.Account, model) {
		return true
	}
	id, _ := ctx.Value(ctxkey.ClientRequestID).(string)
	if id == "" {
		id = uuid.NewString()
	}
	lease := 1800 // Root cancellation releases abandoned probes; lease bounds process crashes.
	if deadline, ok := ctx.Deadline(); ok {
		lease = max(1, int(time.Until(deadline).Seconds())+30)
	}
	admitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	decision, err := v.store.AdmitOpenAICapacity(admitCtx, selection.Account.ID, model, id, lease)
	if err != nil {
		slog.Warn("openai.capacity_cooldown_admit_failed", "account_id", selection.Account.ID, "error", err)
		decision.Allowed = true // Keep serving and permit request-local failover during a Redis outage.
	}
	if !decision.Allowed {
		return false
	}
	account := *selection.Account // Never attach request state to a shared scheduler snapshot.
	account.capacityAttempt = &openAICapacityAttempt{v.store, v.settings, model, id, decision.Generation}
	if err != nil {
		account.capacityAttempt.store = nil
	}
	selection.Account = &account
	if decision.Probe {
		context.AfterFunc(ctx, func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = v.store.ReleaseOpenAICapacity(cleanup, account.ID, model, id, decision.Generation)
		})
	}
	return true
}

// outcome: 0=inconclusive/non-capacity error, 1=capacity shed, 2=completed success.
// Redis deduplicates by logical request ID, including retries and repeated SSE frames.
func observeOpenAICapacity(account *Account, outcome int) {
	if account == nil || account.capacityAttempt == nil || account.capacityAttempt.store == nil {
		return
	}
	a := account.capacityAttempt
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := a.store.ObserveOpenAICapacity(ctx, account.ID, a.model, a.id, a.generation, outcome, a.settings)
	if err != nil {
		slog.Warn("openai.capacity_cooldown_record_failed", "account_id", account.ID, "error", err)
	} else if d.Tripped {
		slog.Warn("openai.capacity_cooldown_tripped", "account_id", account.ID, "model", a.model,
			"failures", d.Failures, "requests", d.Total, "level", d.Level, "until_unix_ms", d.UntilMS)
	}
}

func observeOpenAICapacityResult(account *Account, success bool, observedErr ...error) {
	if success {
		observeOpenAICapacity(account, 2)
		return
	}
	var failure *UpstreamFailoverError
	if len(observedErr) > 0 && errors.As(observedErr[0], &failure) && isOpenAIRequestScopedCapacityShed("", failure.ResponseBody) {
		observeOpenAICapacity(account, 1)
		return
	}
	observeOpenAICapacity(account, 0)
}

// Reuse the handler's bounded, pre-output failover flow. Quota 429s and other
// failures retain their current retry policy; no replay is allowed after output.
func (s *OpenAIGatewayService) applyOpenAICapacitySwitchPolicy(account *Account, failure *UpstreamFailoverError) {
	if failure != nil && failure.RequestScopedTransient && s.shouldSwitchOpenAICapacity(account, failure.ResponseBody) {
		failure.RetryableOnSameAccount = false
	}
}

func (s *OpenAIGatewayService) shouldSwitchOpenAICapacity(account *Account, body []byte) bool {
	if s == nil || account == nil || account.capacityAttempt == nil || !isOpenAIRequestScopedCapacityShed("", body) {
		return false
	}
	if hit, _, _ := detectOpenAICyberPolicy(body); hit || isOpenAIContextWindowError("", body) {
		return false
	}
	v := account.capacityAttempt.settings
	return v.Enabled && v.SwitchAccountOnOverload
}
