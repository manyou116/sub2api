package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitQueued(t *testing.T, a *admission, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		got := len(a.queue)
		a.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("queue did not reach expected length")
}
func TestAdmissionIsolationCancellationAndOrder(t *testing.T) {
	a, _ := newAdmission(Limits{Global: 2, PerAccount: 1, Queue: 2, Wait: time.Second})
	release, _ := a.acquire(context.Background(), "a", "s")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		r, e := a.acquire(ctx, "a", "s")
		if r != nil {
			r()
		}
		result <- e
	}()
	waitQueued(t, a, 1)
	other, e := a.acquire(context.Background(), "b", "other")
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	if !errors.Is(<-result, context.Canceled) {
		t.Fatal("cancel lost")
	}
	waitQueued(t, a, 0)
	other()
	release()
	release()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != 0 || len(a.accounts) != 0 || len(a.sessions) != 0 {
		t.Fatal("leaked capacity")
	}
}
func TestAdmissionBoundsTimeoutAndShutdown(t *testing.T) {
	a, _ := newAdmission(Limits{Global: 1, PerAccount: 1, Queue: 1, Wait: 30 * time.Millisecond})
	release, _ := a.acquire(context.Background(), "a", "")
	defer release()
	result := make(chan error, 1)
	go func() { _, e := a.acquire(context.Background(), "b", ""); result <- e }()
	waitQueued(t, a, 1)
	if _, e := a.acquire(context.Background(), "c", ""); !errors.Is(e, errQueueFull) {
		t.Fatal(e)
	}
	if e := <-result; !errors.Is(e, errQueueTimeout) {
		t.Fatal(e)
	}
	go func() { _, e := a.acquire(context.Background(), "b", ""); result <- e }()
	waitQueued(t, a, 1)
	a.close()
	if e := <-result; !errors.Is(e, errDraining) {
		t.Fatal(e)
	}
	if _, e := a.acquire(context.Background(), "a", ""); !errors.Is(e, errDraining) {
		t.Fatal(e)
	}
}
func TestAdmissionConcurrentStress(t *testing.T) {
	a, _ := newAdmission(Limits{Global: 8, PerAccount: 2, Queue: 256, Wait: 5 * time.Second})
	var wg sync.WaitGroup
	var active atomic.Int32
	var max atomic.Int32
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, e := a.acquire(context.Background(), string(rune('a'+i%4)), "")
			if e != nil {
				t.Error(e)
				return
			}
			defer r()
			v := active.Add(1)
			for prev := max.Load(); v > prev; prev = max.Load() {
				if max.CompareAndSwap(prev, v) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
		}(i)
	}
	wg.Wait()
	if max.Load() > 8 {
		t.Fatal("global limit exceeded")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != 0 || len(a.queue) != 0 {
		t.Fatal("leaked operations")
	}
}
func TestAdmissionSerializesSession(t *testing.T) {
	a, _ := newAdmission(Limits{Global: 4, PerAccount: 4, Queue: 4, Wait: time.Second})
	r, _ := a.acquire(context.Background(), "a", "same")
	result := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			release, e := a.acquire(context.Background(), "a", "same")
			if e != nil {
				t.Error(e)
				return
			}
			result <- i
			release()
		}(i)
		waitQueued(t, a, i+1)
	}
	r()
	if <-result != 0 || <-result != 1 {
		t.Fatal("session reordered")
	}
}

func TestSaturatedAccountDoesNotBlockIndependentAccount(t *testing.T) {
	a, _ := newAdmission(Limits{Global: 2, PerAccount: 1, Queue: 1, Wait: time.Second})
	r, _ := a.acquire(context.Background(), "a", "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		release, e := a.acquire(context.Background(), "a", "")
		if e != nil {
			t.Error(e)
			return
		}
		release()
	}()
	waitQueued(t, a, 1)
	other, e := a.acquire(context.Background(), "b", "")
	if e != nil {
		t.Fatal("independent account blocked", e)
	}
	other()
	r()
	<-done
}

func TestAccountSnapshotLiveLimits(t *testing.T) {
	a, err := newAdmission(Limits{Global: 8, PerAccount: 4, Queue: 8, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.configureAccount("a", 2, 1); err != nil {
		t.Fatal(err)
	}
	release, err := a.acquire(context.Background(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	rows := fixtureValue[map[string]any](a.snapshotAccounts([]string{"a", "b"})["accounts"])
	row := fixtureValue[map[string]any](rows["a"])
	if row["active"] != 1 || row["max_concurrent"] != 2 || row["queued"] != 0 {
		t.Fatal(row)
	}
	if fixtureValue[map[string]any](rows["b"])["max_concurrent"] != 4 {
		t.Fatal(rows)
	}
	release()
	if fixtureValue[map[string]any](fixtureValue[map[string]any](a.snapshotAccounts([]string{"a"})["accounts"])["a"])["active"] != 0 {
		t.Fatal("stale count")
	}
}
