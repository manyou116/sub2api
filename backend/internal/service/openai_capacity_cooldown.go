package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	LockOpenAICapacity(context.Context, int64) (func(), error)
	CurrentOpenAICapacity(context.Context, int64) (OpenAICapacityDecision, error)
	ResetOpenAICapacity(context.Context, int64) error
}

// The stable prefix lets web-image selection exempt only this text pause.
const OpenAICapacityCooldownReasonPrefix = `{"source":"openai_capacity_cooldown",`
const openAICapacityCooldownKeyword = "openai_capacity_cooldown"

func IsOpenAICapacityCooldownReason(reason string) bool {
	return strings.HasPrefix(reason, OpenAICapacityCooldownReasonPrefix)
}

type OpenAICapacityDecision struct {
	Allowed, Probe, Tripped bool
	Generation, UntilMS     int64
	Failures, Total, Level  int64
}

type openAICapacityAttempt struct {
	store                         OpenAICapacityCooldownStore
	settings                      OpenAICapacityCooldownSettings
	model, id                     string
	generation                    int64
	rateLimit                     *RateLimitService
	ctx                           context.Context
	slowMu                        sync.Mutex
	slowOutput                    *openAISlowFirstOutput
	slowObserved, fastFirstOutput bool
	slowRecordDone                chan struct{}
}

type openAICapacityContextKey struct{}
type openAICapacityRequest struct {
	store    OpenAICapacityCooldownStore
	settings OpenAICapacityCooldownSettings
	compact  bool
	mu       sync.Mutex
	blocked  map[int64]bool
	loaded   bool
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
	if err != nil || (!settings.Enabled && !settings.SlowFirstOutputEnabled) || !ok {
		return ctx
	}
	return context.WithValue(ctx, openAICapacityContextKey{}, &openAICapacityRequest{store: store, settings: settings, compact: compact})
}

func openAICapacityEligible(account *Account, model string) bool {
	return isOpenAIOAuthAccount(account) && !account.IsShadow() && model != "" && !isOpenAIImageGenerationModel(model)
}

// Fetch only active blocked IDs once per selection pass, not
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
	if !v.loaded {
		v.blocked = make(map[int64]bool)
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		ids, err := v.store.BlockedOpenAICapacityAccounts(readCtx, model)
		if err != nil {
			slog.Warn("openai.capacity_cooldown_read_failed", "error", err)
		}
		for _, id := range ids {
			v.blocked[id] = true
		}
		v.loaded = true
	}
	return v.blocked[account.ID]
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
	account.capacityAttempt = &openAICapacityAttempt{store: v.store, settings: v.settings, model: model, id: id,
		generation: decision.Generation, rateLimit: s.rateLimitService, ctx: ctx}
	if err != nil {
		account.capacityAttempt.store = nil
	}
	selection.Account = &account
	if decision.Probe {
		context.AfterFunc(ctx, func() {
			a := account.capacityAttempt
			a.slowMu.Lock()
			pending := a.slowRecordDone
			a.slowMu.Unlock()
			if pending != nil {
				<-pending // Settle a recorded slow probe before cancellation releases its owner.
			}
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = v.store.ReleaseOpenAICapacity(cleanup, account.ID, model, id, decision.Generation)
		})
	}
	return true
}

// outcome: 0=inconclusive, 1=capacity shed, 2=healthy success; latency uses 3.
// Redis deduplicates by logical request ID, including retries and repeated SSE frames.
func observeOpenAICapacity(account *Account, outcome int) {
	if account != nil && account.capacityAttempt != nil && outcome == 1 && !account.capacityAttempt.settings.Enabled {
		outcome = 0
	}
	if account != nil && account.capacityAttempt != nil {
		a := account.capacityAttempt
		a.slowMu.Lock()
		slow := a.slowObserved
		a.slowMu.Unlock()
		if slow && outcome != 1 {
			return // The pending latency observation owns this result and probe lease.
		}
	}
	recordOpenAITextCooldown(account, outcome, nil)
}

func recordOpenAITextCooldown(account *Account, outcome int, slow *openAISlowFirstOutputObservation) {
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
		if slow != nil {
			slog.Warn("openai.slow_first_output_cooldown_tripped", "account_id", account.ID, "model", a.model,
				"elapsed_ms", slow.elapsed.Milliseconds(), "threshold_ms", slow.threshold.Milliseconds(), "count", d.Failures,
				"window_seconds", a.settings.SlowFirstOutputWindowSeconds, "level", d.Level, "until_unix_ms", d.UntilMS)
		} else {
			slog.Warn("openai.capacity_cooldown_tripped", "account_id", account.ID, "model", a.model,
				"failures", d.Failures, "requests", d.Total, "level", d.Level, "until_unix_ms", d.UntilMS)
		}
		if err := a.rateLimit.persistOpenAICapacityPause(account, a, d, slow); err != nil {
			slog.Warn("openai.capacity_cooldown_persist_failed", "account_id", account.ID, "error", err)
		}
	}
}

func (s *RateLimitService) persistOpenAICapacityPause(account *Account, attempt *openAICapacityAttempt, decision OpenAICapacityDecision, observation ...*openAISlowFirstOutputObservation) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	unlock, err := attempt.store.LockOpenAICapacity(ctx, account.ID)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := attempt.store.CurrentOpenAICapacity(ctx, account.ID)
	if err != nil || current.Generation != decision.Generation || current.UntilMS != decision.UntilMS {
		return err // A manual recovery or newer overload superseded this result.
	}
	until := time.UnixMilli(decision.UntilMS)
	state := TempUnschedState{
		UntilUnix: (decision.UntilMS + 999) / 1000, TriggeredAtUnix: time.Now().Unix(),
		StatusCode: 503, MatchedKeyword: openAICapacityCooldownKeyword, RuleIndex: -1,
		Scope: TempUnschedScopeAccount, Model: attempt.model,
		TriggerCount: decision.Failures, TriggerThreshold: attempt.settings.FailureThreshold,
		ErrorMessage: fmt.Sprintf("OpenAI 文本过载：%s；%d 秒内 %d 次过载达到阈值 %d；第 %d 级回避。整个账号暂停文本调用，Web 生图除外。", attempt.model, attempt.settings.WindowSeconds, decision.Failures, attempt.settings.FailureThreshold, decision.Level),
	}
	if len(observation) > 0 && observation[0] != nil {
		slow := observation[0]
		state.StatusCode = 0 // A slow successful request is not an HTTP error.
		state.MatchedKeyword = "openai_slow_first_output"
		state.TriggerThreshold = attempt.settings.SlowFirstOutputThreshold
		state.ErrorMessage = fmt.Sprintf("OpenAI 首字慢：%s；等待有效内容至少 %d 毫秒，阈值 %d 毫秒；%d 秒内 %d 次达到阈值 %d；第 %d 级回避。整个账号暂停文本调用，Web 生图除外。",
			attempt.model, slow.elapsed.Milliseconds(), slow.threshold.Milliseconds(), attempt.settings.SlowFirstOutputWindowSeconds, decision.Failures, state.TriggerThreshold, decision.Level)
	}
	payload, err := json.Marshal(struct {
		Source string `json:"source"`
		TempUnschedState
	}{openAICapacityCooldownKeyword, state})
	if err != nil {
		return err
	}
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, string(payload)); err != nil {
		return err
	}
	// Another failure may have a stricter account-wide reason. Cache the DB
	// winner, rather than making an authentication pause look image-exempt.
	fresh, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		return err
	}
	if fresh != nil && fresh.TempUnschedulableUntil != nil && s.tempUnschedCache != nil {
		actual := tempUnschedStateFromStoredReason(fresh.TempUnschedulableReason, fresh.TempUnschedulableUntil.Unix())
		if err := s.tempUnschedCache.SetTempUnsched(ctx, account.ID, actual); err != nil {
			return err
		}
	}
	s.notifyAccountSchedulingBlocked(account, until, openAICapacityCooldownKeyword)
	return nil
}

// Hold the same account lock until the existing DB/cache recovery finishes.
// Reset advances the Redis generation so old in-flight results cannot re-pause it.
func (s *RateLimitService) resetOpenAICapacityPause(ctx context.Context, accountID int64) (func(), error) {
	store, ok := s.tempUnschedCache.(OpenAICapacityCooldownStore)
	if !ok {
		return func() {}, nil
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !isOpenAIOAuthAccount(account) {
		return func() {}, nil
	}
	unlock, err := store.LockOpenAICapacity(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if err := store.ResetOpenAICapacity(ctx, accountID); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func observeOpenAICapacityResult(account *Account, success bool, observedErr ...error) {
	if success {
		if !openAISlowFirstOutputHealthy(account) {
			observeOpenAICapacity(account, 0)
			return
		}
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
