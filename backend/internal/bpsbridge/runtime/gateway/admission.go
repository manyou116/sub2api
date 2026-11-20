package gateway

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

var errQueueFull = errors.New("bps_queue_full")
var errQueueTimeout = errors.New("bps_queue_timeout")
var errDraining = errors.New("bps_gateway_draining")

type Limits struct {
	Global     int
	PerAccount int
	Queue      int
	Wait       time.Duration
}

func DefaultLimits() Limits {
	return Limits{Global: 0, PerAccount: 4, Queue: 64, Wait: 30 * time.Second}
}

type ticket struct {
	account, session string
	ready            chan struct{}
	granted          bool
	slot             int
	released         bool
}
type accountLimit struct {
	Value    int
	Revision int64
}
type admission struct {
	mu        sync.Mutex
	limits    Limits
	active    int
	accounts  map[string]int
	slots     map[string]map[int]bool
	overrides map[string]accountLimit
	sessions  map[string]bool
	queue     []*ticket
	draining  bool
}

func newAdmission(l Limits) (*admission, error) {
	if l.Global < 0 || l.PerAccount < 1 || l.Queue < 0 || l.Wait <= 0 {
		return nil, errors.New("invalid gateway admission limits")
	}
	return &admission{limits: l, accounts: map[string]int{}, slots: map[string]map[int]bool{}, overrides: map[string]accountLimit{}, sessions: map[string]bool{}}, nil
}
func (a *admission) accountLimitLocked(account string) int {
	limit := a.limits.PerAccount
	if override := a.overrides[account]; override.Value > 0 {
		limit = override.Value
	}
	return limit
}
func (a *admission) eligible(t *ticket) bool {
	limit := a.accountLimitLocked(t.account)
	return (a.limits.Global == 0 || a.active < a.limits.Global) && a.accounts[t.account] < limit && (t.session == "" || !a.sessions[t.session])
}
func (a *admission) grant(t *ticket) {
	// Callers hold mu and have checked eligible. Fewer active leases than the
	// effective limit guarantees a free slot within that limit, even if a
	// previous reduction left higher numbered slots running temporarily.
	slots := a.slots[t.account]
	if slots == nil {
		slots = make(map[int]bool)
		a.slots[t.account] = slots
	}
	limit := a.accountLimitLocked(t.account)
	for slot := 1; slot <= limit; slot++ {
		if !slots[slot] {
			t.slot = slot
			slots[slot] = true
			break
		}
	}
	a.active++
	a.accounts[t.account]++
	if t.session != "" {
		a.sessions[t.session] = true
	}
	t.granted = true
	close(t.ready)
}
func (a *admission) release(t *ticket) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.releaseLocked(t)
	a.dispatch()
}
func (a *admission) releaseLocked(t *ticket) {
	if t.released || !t.granted {
		return
	}
	t.released = true
	a.active--
	a.accounts[t.account]--
	delete(a.slots[t.account], t.slot)
	if a.accounts[t.account] == 0 {
		delete(a.accounts, t.account)
		delete(a.slots, t.account)
	}
	delete(a.sessions, t.session)
}
func (a *admission) dispatch() {
	if a.draining {
		return
	}
	// Preserve arrival order per session, while allowing independent accounts to progress.
	for i := 0; i < len(a.queue); {
		t := a.queue[i]
		if a.eligible(t) {
			a.queue = append(a.queue[:i], a.queue[i+1:]...)
			a.grant(t)
		} else {
			i++
		}
	}
}
func (a *admission) acquire(ctx context.Context, account, session string) (func(), error) {
	_, release, err := a.acquireSlot(ctx, account, session)
	return release, err
}

// acquireSlot atomically reserves account capacity and its lowest free slot.
// The caller owns that slot until release, including response and WS cleanup.
// Slot numbers carry no history; stable upstream task identity is derived by
// the gateway from its persistent key, account identity and this number.
func (a *admission) acquireSlot(ctx context.Context, account, session string) (int, func(), error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	t := &ticket{account: account, session: session, ready: make(chan struct{})}
	a.mu.Lock()
	if a.draining {
		a.mu.Unlock()
		return 0, nil, errDraining
	}
	if a.eligible(t) {
		a.grant(t)
	} else {
		if len(a.queue) >= a.limits.Queue {
			a.mu.Unlock()
			return 0, nil, errQueueFull
		}
		a.queue = append(a.queue, t)
		a.dispatch()
	}
	a.mu.Unlock()
	timer := time.NewTimer(a.limits.Wait)
	defer timer.Stop()
	var err error
	select {
	case <-t.ready:
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errQueueTimeout
	}
	a.mu.Lock()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && a.draining && !t.granted {
		err = errDraining
	}
	if err != nil && !t.granted {
		for i, q := range a.queue {
			if q == t {
				a.queue = append(a.queue[:i], a.queue[i+1:]...)
				break
			}
		}
		a.dispatch()
	}
	granted := t.granted
	a.mu.Unlock()
	if err != nil {
		if granted {
			a.release(t)
		}
		return 0, nil, err
	}
	var once sync.Once
	return t.slot, func() { once.Do(func() { a.release(t) }) }, nil
}
func (a *admission) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.draining {
		return
	}
	a.draining = true
	for _, t := range a.queue {
		close(t.ready)
	}
	a.queue = nil
}

func (a *admission) snapshot() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked()
}

func (a *admission) snapshotLocked() map[string]any {
	return map[string]any{"active": a.active, "queued": len(a.queue), "active_accounts": len(a.accounts), "active_sessions": len(a.sessions), "draining": a.draining, "max_concurrent": a.limits.Global, "max_per_account": a.limits.PerAccount, "max_queue": a.limits.Queue, "queue_timeout_seconds": a.limits.Wait.Seconds()}
}

// Updates only affect admission; running generations are allowed to finish.
func (a *admission) configureAccount(account string, limit int, revision int64) error {
	if account == "" || limit < 0 || limit > 10000 || revision <= 0 {
		return errors.New("invalid account concurrency")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if old := a.overrides[account]; revision > old.Revision {
		a.overrides[account] = accountLimit{limit, revision}
		a.dispatch()
	}
	return nil
}

// Report account-scoped admission state, not persistent usage ledger entries.
func (a *admission) snapshotAccounts(accounts []string) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.snapshotLocked()
	rows := map[string]any{}
	for _, account := range accounts {
		limit := a.accountLimitLocked(account)
		queued := 0
		for _, ticket := range a.queue {
			if ticket.account == account {
				queued++
			}
		}
		slots := make([]int, 0, len(a.slots[account]))
		for slot := range a.slots[account] {
			slots = append(slots, slot)
		}
		sort.Ints(slots)
		rows[account] = map[string]any{"active": a.accounts[account], "queued": queued, "max_concurrent": limit, "task_pool_size": limit, "active_task_slots": slots}
	}
	out["accounts"] = rows
	return out
}
