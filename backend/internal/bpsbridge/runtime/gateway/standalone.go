package gateway

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/auth"
	"github.com/Wei-Shaw/sub2api/internal/bpsbridge/runtime/proxy"
)

// StandaloneHandler adapts the account-slot scheduler to the public loopback
// Responses endpoint. Each request supplies its own complete history.
type StandaloneHandler struct {
	bridge    *proxy.Server
	store     proxy.CredentialSource
	fallback  http.Handler
	key       string
	admission *admission
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closing   bool
	handlers  sync.WaitGroup
}

type requestCredentialsKey struct{}

// WithRequestCredentials supplies the snapshot already checked by the caller.
func WithRequestCredentials(ctx context.Context, token, account string) context.Context {
	return context.WithValue(ctx, requestCredentialsKey{}, fixed{token: token, account: account})
}

func NewStandaloneHandler(bridge *proxy.Server, store proxy.CredentialSource, key string, limits Limits) (*StandaloneHandler, error) {
	if bridge == nil || store == nil || len(key) < 32 {
		return nil, errors.New("standalone bridge, credentials and persistent task key are required")
	}
	if limits.PerAccount > 10000 {
		return nil, errors.New("standalone max concurrency must be between 1 and 10000")
	}
	gate, err := newAdmission(limits)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &StandaloneHandler{bridge: bridge, store: store, fallback: bridge.Handler(), key: key, admission: gate, ctx: ctx, cancel: cancel}, nil
}

// Close stops admission and cancels active work; Wait must complete before the
// owner releases its state lock so another process cannot reuse a live slot.
func (s *StandaloneHandler) Close() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.admission.close()
	s.bridge.CloseStartRate()
	s.cancel()
}

func (s *StandaloneHandler) Wait() { s.handlers.Wait() }

func (s *StandaloneHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		fail(w, http.StatusServiceUnavailable, "bps_gateway_draining")
		return
	}
	s.handlers.Add(1)
	s.mu.Unlock()
	defer s.handlers.Done()

	if r.Method != http.MethodPost || (r.URL.Path != "/v1/responses" && r.URL.Path != "/responses") {
		s.fallback.ServeHTTP(w, r)
		return
	}
	// Snapshot once: a credential file replaced while this request waits must
	// not change the account behind an already selected slot and task.
	source := s.store
	if snapshot, ok := r.Context().Value(requestCredentialsKey{}).(fixed); ok {
		source = snapshot
	}
	token, account, err := source.Credentials()
	if err != nil || token == "" {
		fail(w, http.StatusUnauthorized, "basispoints_auth_unavailable")
		return
	}
	admissionKey := auth.AdmissionKeyFromToken(token)
	if fromToken := auth.AccountIDFromToken(token); fromToken != "" {
		account = fromToken
	}
	if account == "" {
		fail(w, http.StatusUnauthorized, "basispoints_account_id_missing")
		return
	}
	if admissionKey == "" {
		admissionKey = auth.AdmissionKey(account, "")
	}
	// Client session_id is neither an upstream task selector nor a trusted
	// history boundary. Each request carries its own full context.
	ctx, cancel := context.WithCancel(proxy.WithLogicalSession(r.Context(), ""))
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	queueStarted := time.Now()
	slot, release, err := s.admission.acquireSlot(ctx, admissionKey, "")
	if err != nil {
		status, code := http.StatusTooManyRequests, "bps_queue_timeout"
		if errors.Is(err, errQueueFull) {
			code = "bps_queue_full"
		}
		if errors.Is(err, errDraining) || s.ctx.Err() != nil {
			status, code = http.StatusServiceUnavailable, "bps_gateway_draining"
		} else if ctx.Err() != nil {
			status, code = 499, "request_cancelled"
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		fail(w, status, code)
		return
	}
	defer release()
	turn, err := newSlotTurnID()
	if err != nil {
		fail(w, http.StatusInternalServerError, "bps_task_identity_unavailable")
		return
	}
	ctx = proxy.WithUpstreamTask(ctx, slotTaskID(s.key, admissionKey, slot), turn)
	w.Header().Set("X-BPS-Task-Slot", strconv.Itoa(slot))
	w.Header().Set("X-BPS-Queue-Ms", strconv.FormatInt(time.Since(queueStarted).Milliseconds(), 10))
	bridge := s.bridge.WithStartAuthorization("standalone", 0).
		WithStartDeadline(queueStarted.Add(s.admission.limits.Wait)).WithIsolatedReplay()
	bridge.WithCredentials(fixed{token: token, account: account}).ServeHTTP(w, r.WithContext(ctx))
}
