package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const startRateWindow = time.Second

type startRateWaitKey struct{}
type startRateWaitObservation struct{ elapsed time.Duration }

// StartRateError contains only a safe local error code and HTTP status.
type StartRateError struct {
	Code   string
	Status int
}

func (e *StartRateError) Error() string { return e.Code }
func startRateError(code string, status int) error {
	return &StartRateError{Code: code, Status: status}
}

type startAuthorization struct {
	id       string
	revision uint64
}
type startAuthorizationState struct {
	revision uint64
	blocked  bool
	dispatch chan struct{}
}

// A nonzero size gives each queued waiter a distinct pointer identity.
type startWaiter struct{ _ byte }

type accountStartRate struct {
	mu             sync.Mutex
	rps            int
	revision       int64
	starts         []time.Time
	waiters        []*startWaiter
	authorizations map[string]*startAuthorizationState
	changed        chan struct{}
	closed         bool
}
type startRates struct {
	mu                     sync.Mutex
	accounts               map[string]*accountStartRate
	defaultRPS, queueLimit int
	wait                   time.Duration
	closed                 bool
}

func newStartRates(cfg Config) (*startRates, error) {
	if cfg.UpstreamRPS < 0 || cfg.UpstreamRPS > 10000 {
		return nil, fmt.Errorf("upstream RPS must be between 0 and 10000")
	}
	if cfg.RateQueueLimit < 0 || cfg.RateWaitTimeout < 0 {
		return nil, fmt.Errorf("rate queue and wait limits must not be negative")
	}
	queueLimit, wait := cfg.RateQueueLimit, cfg.RateWaitTimeout
	if queueLimit == 0 {
		queueLimit = 64
	}
	if wait == 0 {
		wait = 30 * time.Second
	}
	return &startRates{accounts: make(map[string]*accountStartRate), defaultRPS: cfg.UpstreamRPS, queueLimit: queueLimit, wait: wait}, nil
}
func (r *startRates) account(id string) *accountStartRate {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.accounts[id]
	if a == nil {
		a = &accountStartRate{rps: r.defaultRPS, revision: -1, authorizations: make(map[string]*startAuthorizationState), changed: make(chan struct{}), closed: r.closed}
		r.accounts[id] = a
	}
	return a
}
func (a *accountStartRate) notify() { close(a.changed); a.changed = make(chan struct{}) }
func (a *accountStartRate) prune(now time.Time) {
	i := 0
	for i < len(a.starts) && !now.Before(a.starts[i].Add(startRateWindow)) {
		i++
	}
	if i > 0 {
		copy(a.starts, a.starts[i:])
		a.starts = a.starts[:len(a.starts)-i]
	}
}

// A newer revision resumes only this identity, without clearing start history.
// All authorization state is protected by the account lock.
func (a *accountStartRate) authorization(auth startAuthorization, activate bool) (*startAuthorizationState, error) {
	if a.closed {
		return nil, startRateError("bps_rate_limiter_closed", http.StatusServiceUnavailable)
	}
	state := a.authorizations[auth.id]
	if state == nil {
		state = &startAuthorizationState{revision: auth.revision, dispatch: make(chan struct{}, 1)}
		state.dispatch <- struct{}{}
		a.authorizations[auth.id] = state
	}
	if auth.id == "" {
		return state, nil
	}
	if activate && auth.revision > state.revision {
		state.revision, state.blocked = auth.revision, false
		a.notify()
	}
	if auth.revision != state.revision {
		return nil, startRateError("bps_start_authorization_stale", http.StatusConflict)
	}
	if state.blocked {
		return nil, startRateError("bps_start_authorization_blocked", http.StatusConflict)
	}
	return state, nil
}
func (r *startRates) check(account string, auth startAuthorization) error {
	a := r.account(account)
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.authorization(auth, true)
	return err
}
func (r *startRates) reject(account string, auth startAuthorization) {
	if auth.id == "" {
		return
	}
	a := r.account(account)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rejectLocked(auth)
}

func (a *accountStartRate) rejectLocked(auth startAuthorization) {
	state := a.authorizations[auth.id]
	// An old stream cannot re-disable a manually resumed authorization.
	if state != nil && state.revision == auth.revision && !state.blocked {
		state.blocked = true
		a.notify()
	}
}

// run keeps FIFO order up to final dispatch. No account/global lock spans I/O.
// A per-authorization dispatch gate is acquired before committing a timestamp,
// so slow dispatches cannot accumulate permits then emit a delayed burst.
func (r *startRates) run(ctx context.Context, account string, auth startAuthorization, outerDeadline time.Time, send func() error) error {
	return r.runDispatch(ctx, account, auth, outerDeadline, func(release func()) error {
		defer release()
		return send()
	})
}

func (r *startRates) runDispatch(ctx context.Context, account string, auth startAuthorization, outerDeadline time.Time, send func(release func()) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a := r.account(account)
	waiter := &startWaiter{}
	deadline := time.Now().Add(r.wait)
	waitStarted := time.Now()
	if !outerDeadline.IsZero() && outerDeadline.Before(deadline) {
		deadline = outerDeadline
	}
	a.mu.Lock()
	state, err := a.authorization(auth, true)
	if err != nil {
		a.mu.Unlock()
		return err
	}
	if a.rps == 0 {
		// Disabled pacing preserves existing request concurrency. Authorization
		// fencing and closure still apply, but there is no rate queue or timeout.
		a.mu.Unlock()
		return send(func() {})
	}
	now := time.Now()
	a.prune(now)
	if len(a.waiters) >= r.queueLimit {
		a.mu.Unlock()
		return startRateError("bps_rate_queue_full", http.StatusTooManyRequests)
	}
	a.waiters = append(a.waiters, waiter)
	a.notify()
	a.mu.Unlock()
	var waitFinished sync.Once
	finishWait := func() {
		waitFinished.Do(func() {
			if observed, _ := ctx.Value(startRateWaitKey{}).(*startRateWaitObservation); observed != nil {
				observed.elapsed += time.Since(waitStarted)
			}
		})
	}
	defer finishWait()
	holding := false
	defer func() {
		if holding {
			state.dispatch <- struct{}{}
		}
		a.mu.Lock()
		for i, queued := range a.waiters {
			if queued == waiter {
				a.waiters = append(a.waiters[:i], a.waiters[i+1:]...)
				a.notify()
				break
			}
		}
		a.mu.Unlock()
	}()
	for {
		a.mu.Lock()
		if err := ctx.Err(); err != nil {
			a.mu.Unlock()
			return err
		}
		if _, err := a.authorization(auth, false); err != nil {
			a.mu.Unlock()
			return err
		}
		now := time.Now()
		a.prune(now)
		if a.rps > 0 && !now.Before(deadline) {
			a.mu.Unlock()
			return startRateError("bps_rate_wait_timeout", http.StatusTooManyRequests)
		}
		ready := a.waiters[0] == waiter && (a.rps == 0 || len(a.starts) < a.rps)
		if ready && (holding || a.rps == 0) {
			// Final start/authorization linearization point. No lock acquisition or
			// request preparation follows before send; already started I/O survives
			// a later rejection, while every pending waiter observes that rejection.
			if a.rps > 0 {
				a.starts = append(a.starts, now)
			}
			finishWait()
			a.waiters = a.waiters[1:]
			a.notify()
			a.mu.Unlock()
			if !holding {
				return send(func() {})
			}
			holding = false
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { state.dispatch <- struct{}{} }) }
			defer release()
			return send(release)
		}
		if !ready && holding {
			state.dispatch <- struct{}{}
			holding = false
		}
		changed := a.changed
		var wake time.Time
		if a.rps > 0 {
			wake = deadline
			if len(a.starts) >= a.rps {
				next := a.starts[len(a.starts)-a.rps].Add(startRateWindow)
				if next.Before(wake) {
					wake = next
				}
			}
		}
		a.mu.Unlock()
		var dispatch <-chan struct{}
		if ready {
			dispatch = state.dispatch
		}
		var timer *time.Timer
		var timerC <-chan time.Time
		if !wake.IsZero() {
			timer = time.NewTimer(time.Until(wake))
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timerC:
		case <-dispatch:
			holding = true
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// ConfigureAccountRate applies monotonically versioned settings. Zero disables
// pacing. Existing timestamps survive changes and credential renewal.
func (s *Server) ConfigureAccountRate(account string, rps int, revision int64) error {
	if account == "" || rps < 0 || rps > 10000 || revision < 0 {
		return fmt.Errorf("invalid account start rate")
	}
	s.startRates.configure(account, rps, revision)
	if s.dialRates != nil {
		s.dialRates.configure(account, rps, revision)
	}
	return nil
}

func (r *startRates) configure(account string, rps int, revision int64) {
	a := r.account(account)
	a.mu.Lock()
	defer a.mu.Unlock()
	if revision > a.revision {
		a.rps, a.revision = rps, revision
		a.notify()
	}
}

// RateSnapshot exposes counts and delays, never authorization identities.
func (s *Server) RateSnapshot(account string) map[string]any {
	out := s.startRates.snapshot(account)
	if s.dialRates != nil {
		dial := s.dialRates.snapshot(account)
		out["dial_waiting"] = dial["rate_waiting"]
		out["dials_last_second"] = dial["starts_last_second"]
		out["next_dial_ms"] = dial["next_start_ms"]
	}
	return out
}

func (r *startRates) snapshot(account string) map[string]any {
	a := r.account(account)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.prune(now)
	next := int64(0)
	if a.rps > 0 && len(a.starts) >= a.rps {
		delay := a.starts[len(a.starts)-a.rps].Add(startRateWindow).Sub(now)
		next = (delay.Nanoseconds() + int64(time.Millisecond) - 1) / int64(time.Millisecond)
	}
	return map[string]any{"upstream_rps": a.rps, "rate_waiting": len(a.waiters), "starts_last_second": len(a.starts), "next_start_ms": next}
}
func (s *Server) WithStartAuthorization(authID string, revision uint64) *Server {
	copy := *s
	copy.startAuthorization = startAuthorization{id: authID, revision: revision}
	return &copy
}

// WithStartDeadline bounds pacing waits without shortening an admitted stream.
func (s *Server) WithStartDeadline(deadline time.Time) *Server {
	copy := *s
	copy.startDeadline = deadline
	return &copy
}

// CloseStartRate wakes pending requests. All clones share this lifecycle.
func (s *Server) CloseStartRate() {
	s.startRates.close()
	if s.dialRates != nil {
		s.dialRates.close()
	}
}

func (r *startRates) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for _, a := range r.accounts {
		a.mu.Lock()
		a.closed = true
		a.notify()
		a.mu.Unlock()
	}
}

func (s *Server) rejectStarts(account string) {
	// Fence queued connection attempts as well as creates already waiting on
	// an established socket. Work whose dispatch has started is allowed to end.
	if s.dialRates == nil {
		s.startRates.reject(account, s.startAuthorization)
		return
	}
	create, dial := s.startRates.account(account), s.dialRates.account(account)
	create.mu.Lock()
	dial.mu.Lock()
	defer create.mu.Unlock()
	defer dial.mu.Unlock()
	create.rejectLocked(s.startAuthorization)
	dial.rejectLocked(s.startAuthorization)
}
func isStartRejection(kind string, event map[string]json.RawMessage) bool {
	if kind != "error" && kind != "response.failed" && kind != "response.incomplete" {
		return false
	}
	var detail struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Response struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"response"`
	}
	raw, _ := json.Marshal(event)
	if json.Unmarshal(raw, &detail) != nil {
		return false
	}
	code := detail.Code
	if code == "" {
		code = detail.Error.Code
	}
	if code == "" {
		code = detail.Response.Error.Code
	}
	return code == "request_rejected"
}
