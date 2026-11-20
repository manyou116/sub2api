package gateway

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Done is consulted only after acquireSlot has completed its admission/queue
// transaction, allowing tests to synchronize without wall-clock sleeps.
type slotObservedContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *slotObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

type testSlotLease struct {
	slot    int
	release func()
	err     error
}

func startSlotAcquire(a *admission, ctx context.Context, account, session string) (<-chan struct{}, <-chan testSlotLease) {
	observed := &slotObservedContext{Context: ctx, observed: make(chan struct{})}
	result := make(chan testSlotLease, 1)
	go func() {
		slot, release, err := a.acquireSlot(observed, account, session)
		result <- testSlotLease{slot, release, err}
	}()
	return observed.observed, result
}

func receiveSlot(t *testing.T, result <-chan testSlotLease) testSlotLease {
	t.Helper()
	select {
	case lease := <-result:
		return lease
	case <-time.After(5 * time.Second):
		t.Fatal("slot acquisition did not complete")
		return testSlotLease{}
	}
}

func assertNoSlotLeak(t *testing.T, a *admission) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != 0 || len(a.accounts) != 0 || len(a.slots) != 0 || len(a.sessions) != 0 || len(a.queue) != 0 {
		t.Fatalf("admission leaked: active=%d accounts=%v slots=%v sessions=%v queued=%d", a.active, a.accounts, a.slots, a.sessions, len(a.queue))
	}
}

func TestAdmissionSlotsLowestFreeAndAccountIsolation(t *testing.T) {
	a, err := newAdmission(Limits{PerAccount: 3, Queue: 0, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	releases := make([]func(), 0, 3)
	for expected := 1; expected <= 3; expected++ {
		slot, release, err := a.acquireSlot(context.Background(), "account-a", "")
		if err != nil || slot != expected {
			t.Fatalf("slot=%d err=%v expected=%d", slot, err, expected)
		}
		releases = append(releases, release)
	}
	if slot, release, err := a.acquireSlot(context.Background(), "account-a", ""); !errors.Is(err, errQueueFull) || slot != 0 || release != nil {
		t.Fatal("saturated account returned a lease", slot, err)
	}
	otherSlot, otherRelease, err := a.acquireSlot(context.Background(), "account-b", "")
	if err != nil || otherSlot != 1 {
		t.Fatal("another account did not get its own slot 1", otherSlot, err)
	}
	otherRelease()
	releases[1]()
	releases[1]()
	slot, replacement, err := a.acquireSlot(context.Background(), "account-a", "")
	if err != nil || slot != 2 {
		t.Fatal("released lowest slot was not reused", slot, err)
	}
	rows := fixtureValue[map[string]any](a.snapshotAccounts([]string{"account-a", "account-b"})["accounts"])
	row := fixtureValue[map[string]any](rows["account-a"])
	if row["task_pool_size"] != 3 || !reflect.DeepEqual(row["active_task_slots"], []int{1, 2, 3}) {
		t.Fatal("slot snapshot incorrect", row)
	}
	if !reflect.DeepEqual(fixtureValue[map[string]any](rows["account-b"])["active_task_slots"], []int{}) {
		t.Fatal("idle account retained a busy slot")
	}
	replacement()
	for _, release := range releases {
		release()
	}
	assertNoSlotLeak(t, a)
	slot, reuseRelease, err := a.acquireSlot(context.Background(), "account-a", "")
	if err != nil || slot != 1 {
		t.Fatal("idle cleanup changed reusable slot numbering", slot, err)
	}
	reuseRelease()
	assertNoSlotLeak(t, a)
}

func TestAdmissionSlotsResizeDrainsHigherSlots(t *testing.T) {
	a, _ := newAdmission(Limits{PerAccount: 3, Queue: 2, Wait: time.Second})
	releases := make([]func(), 3)
	for i := range releases {
		slot, release, err := a.acquireSlot(context.Background(), "account", "")
		if err != nil || slot != i+1 {
			t.Fatal(slot, err)
		}
		releases[i] = release
	}
	if err := a.configureAccount("account", 2, 1); err != nil {
		t.Fatal(err)
	}
	observed, result := startSlotAcquire(a, context.Background(), "account", "")
	<-observed
	releases[0]()
	a.mu.Lock()
	if a.accounts["account"] != 2 || len(a.queue) != 1 || !a.slots["account"][3] {
		t.Error("shrink killed or overbooked a running slot")
	}
	a.mu.Unlock()
	releases[2]()
	next := receiveSlot(t, result)
	if next.err != nil || next.slot != 1 {
		t.Fatal("shrink granted a high or occupied slot", next.slot, next.err)
	}
	next.release()
	releases[1]()
	assertNoSlotLeak(t, a)
	if err := a.configureAccount("account", 4, 2); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		slot, release, err := a.acquireSlot(context.Background(), "account", "")
		if err != nil || slot != i {
			t.Fatal("expanded slots not reusable", slot, err)
		}
		defer release()
	}
	if err := a.configureAccount("account", 0, 3); err != nil {
		t.Fatal(err)
	}
	row := fixtureValue[map[string]any](fixtureValue[map[string]any](a.snapshotAccounts([]string{"account"})["accounts"])["account"])
	if row["task_pool_size"] != 3 || row["active"] != 4 {
		t.Fatal("reset did not restore default while draining existing leases", row)
	}
}

func TestAdmissionSlotsQueueCancellationTimeoutAndDrain(t *testing.T) {
	for _, kind := range []string{"cancel", "timeout", "drain"} {
		t.Run(kind, func(t *testing.T) {
			a, _ := newAdmission(Limits{PerAccount: 1, Queue: 1, Wait: time.Second})
			_, held, err := a.acquireSlot(context.Background(), "account", "session")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "timeout" {
				// The first acquire has returned; only the blocked acquisition
				// should observe the already-expired test wait interval.
				a.limits.Wait = time.Nanosecond
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed, result := startSlotAcquire(a, ctx, "account", "session")
			<-observed
			want := errQueueTimeout
			switch kind {
			case "cancel":
				cancel()
				want = context.Canceled
			case "drain":
				a.close()
				want = errDraining
			}
			got := receiveSlot(t, result)
			if !errors.Is(got.err, want) || got.slot != 0 || got.release != nil {
				t.Fatal("failed acquisition retained a lease", got.slot, got.err)
			}
			held()
			assertNoSlotLeak(t, a)
		})
	}
}

type slotCancelAfterGrantContext struct {
	context.Context
	cancel context.CancelFunc
}

func (c slotCancelAfterGrantContext) Done() <-chan struct{} {
	c.cancel()
	return c.Context.Done()
}

func TestAdmissionSlotsCancellationAfterGrantReturnsLease(t *testing.T) {
	a, _ := newAdmission(Limits{PerAccount: 1, Queue: 1, Wait: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The ready channel and cancelled context are both ready. Regardless of
	// select's choice, acquireSlot must release the granted slot exactly once.
	slot, release, err := a.acquireSlot(slotCancelAfterGrantContext{ctx, cancel}, "account", "session")
	if !errors.Is(err, context.Canceled) || slot != 0 || release != nil {
		t.Fatal("cancelled admission returned a slot", slot, err)
	}
	assertNoSlotLeak(t, a)
	slot, release, err = a.acquireSlot(context.Background(), "account", "session")
	if err != nil || slot != 1 {
		t.Fatal("cancelled grant stranded its slot or session lock", slot, err)
	}
	release()
	assertNoSlotLeak(t, a)
}

func TestAdmissionSlotsKeepSessionOrder(t *testing.T) {
	a, _ := newAdmission(Limits{PerAccount: 3, Queue: 3, Wait: time.Second})
	_, held, _ := a.acquireSlot(context.Background(), "account", "same-chat")
	firstObserved, first := startSlotAcquire(a, context.Background(), "account", "same-chat")
	<-firstObserved
	secondObserved, second := startSlotAcquire(a, context.Background(), "account", "same-chat")
	<-secondObserved
	otherSlot, otherRelease, err := a.acquireSlot(context.Background(), "account", "other-chat")
	if err != nil || otherSlot != 2 {
		t.Fatal("session queue blocked an independent slot", otherSlot, err)
	}
	held()
	firstLease := receiveSlot(t, first)
	if firstLease.err != nil || firstLease.slot != 1 {
		t.Fatal("first queued session did not reuse slot", firstLease.slot, firstLease.err)
	}
	a.mu.Lock()
	if len(a.queue) != 1 || a.accounts["account"] != 2 {
		t.Error("same session overlapped")
	}
	a.mu.Unlock()
	firstLease.release()
	secondLease := receiveSlot(t, second)
	if secondLease.err != nil || secondLease.slot != 1 {
		t.Fatal("second queued session did not retain order", secondLease.slot, secondLease.err)
	}
	secondLease.release()
	otherRelease()
	assertNoSlotLeak(t, a)
}

func TestAdmissionSlotsConcurrentOwnership(t *testing.T) {
	a, _ := newAdmission(Limits{PerAccount: 4, Queue: 128, Wait: 5 * time.Second})
	var wg sync.WaitGroup
	var mu sync.Mutex
	owned := map[string]bool{}
	start := make(chan struct{})
	for i := 0; i < 96; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			account := fmt.Sprintf("account-%d", i%3)
			slot, release, err := a.acquireSlot(context.Background(), account, "")
			if err != nil {
				t.Error(err)
				return
			}
			key := fmt.Sprintf("%s/%d", account, slot)
			mu.Lock()
			if slot < 1 || slot > 4 || owned[key] {
				t.Errorf("invalid or concurrently reused slot: %s", key)
			}
			owned[key] = true
			mu.Unlock()
			mu.Lock()
			delete(owned, key)
			mu.Unlock()
			release()
			release()
		}(i)
	}
	close(start)
	wg.Wait()
	assertNoSlotLeak(t, a)
}
