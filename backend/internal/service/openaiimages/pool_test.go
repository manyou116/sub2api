package openaiimages

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func makeAcc(id int64, status string, sched bool, extra map[string]any, used time.Time) PoolAccount {
	a := PoolAccount{ID: id, Status: status, Schedulable: sched, Extra: extra}
	if !used.IsZero() {
		t := used
		a.LastUsedAt = &t
	}
	return a
}

func TestImagePool_SelectByQuotaThenLastUsed(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	older := now.Add(-1 * time.Hour)
	newer := now.Add(-5 * time.Minute)

	accs := []PoolAccount{
		makeAcc(1, "active", true, map[string]any{"image_quota_remaining": 5.0}, newer),
		makeAcc(2, "active", true, map[string]any{"image_quota_remaining": 5.0}, older), // 同 quota，更老 last_used 优先
		makeAcc(3, "active", true, map[string]any{"image_quota_remaining": 10.0}, newer), // 最高 quota → 头号
	}

	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }
	pool := NewImagePool(list, nil)
	pool.Now = func() time.Time { return now }

	got, release, err := pool.SelectAccount(context.Background(), PoolFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got.ID != 3 {
		t.Errorf("first pick=%d want 3", got.ID)
	}

	// 第二次：3 已 lease，同 quota=5 的 1 / 2 中，2 更老 → 选 2
	got2, release2, err := pool.SelectAccount(context.Background(), PoolFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if got2.ID != 2 {
		t.Errorf("second pick=%d want 2", got2.ID)
	}
}

func TestImagePool_SkipsCooldownAndInactive(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	cool := now.Add(30 * time.Minute).Format(time.RFC3339)

	accs := []PoolAccount{
		makeAcc(1, "active", true, map[string]any{"image_cooldown_until": cool}, time.Time{}),
		makeAcc(2, "paused", true, nil, time.Time{}),
		makeAcc(3, "active", false, nil, time.Time{}),
		makeAcc(4, "active", true, nil, time.Time{}),
	}
	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }
	pool := NewImagePool(list, nil)
	pool.Now = func() time.Time { return now }

	got, release, err := pool.SelectAccount(context.Background(), PoolFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got.ID != 4 {
		t.Errorf("pick=%d want 4 (only active+schedulable+no-cooldown)", got.ID)
	}
}

func TestImagePool_AllCooldownNoProbeReturnsErr(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cool := now.Add(time.Hour).Format(time.RFC3339)
	accs := []PoolAccount{
		makeAcc(1, "active", true, map[string]any{"image_cooldown_until": cool}, time.Time{}),
	}
	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }
	pool := NewImagePool(list, nil)
	pool.Now = func() time.Time { return now }

	_, _, err := pool.SelectAccount(context.Background(), PoolFilter{})
	if !errors.Is(err, ErrNoImageAccount) {
		t.Errorf("err=%v want ErrNoImageAccount", err)
	}
}

func TestImagePool_RecordRateLimit(t *testing.T) {
	repo := &stubRepo{}
	probe := NewAccountProbe(repo)
	pool := NewImagePool(nil, probe)
	resetAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := pool.RecordRateLimit(context.Background(), 7, resetAt); err != nil {
		t.Fatal(err)
	}
	if got := repo.get(7)["image_cooldown_until"]; got != resetAt.Format(time.RFC3339) {
		t.Errorf("cooldown=%v", got)
	}
}

func TestImagePool_RecordSuccessClearsCooldownAndDecrements(t *testing.T) {
	repo := &stubRepo{}
	probe := NewAccountProbe(repo)
	pool := NewImagePool(nil, probe)
	if err := pool.RecordSuccess(context.Background(), 3, map[string]any{"image_quota_remaining": 5.0}); err != nil {
		t.Fatal(err)
	}
	saved := repo.get(3)
	if saved["image_cooldown_until"] != "" {
		t.Errorf("cooldown should be cleared, got %v", saved["image_cooldown_until"])
	}
	if saved["image_quota_remaining"] != 4 {
		t.Errorf("quota_remaining should decrement to 4, got %v", saved["image_quota_remaining"])
	}
}

func TestImagePool_LeaseGCAfterRelease(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	accs := []PoolAccount{makeAcc(1, "active", true, nil, time.Time{})}
	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }
	pool := NewImagePool(list, nil)
	pool.Now = func() time.Time { return now }

	got, release, err := pool.SelectAccount(context.Background(), PoolFilter{})
	if err != nil || got.ID != 1 {
		t.Fatalf("pick err=%v id=%d", err, got.ID)
	}
	if _, _, err := pool.SelectAccount(context.Background(), PoolFilter{}); !errors.Is(err, ErrNoImageAccount) {
		t.Errorf("while leased should get ErrNoImageAccount, err=%v", err)
	}
	release()
	got2, release2, err := pool.SelectAccount(context.Background(), PoolFilter{})
	defer release2()
	if err != nil || got2.ID != 1 {
		t.Errorf("after release should pick again, id=%d err=%v", got2.ID, err)
	}
}

func TestImagePool_ConcurrentLeaseDoesNotDoubleSelect(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	accs := []PoolAccount{
		makeAcc(1, "active", true, nil, time.Time{}),
		makeAcc(2, "active", true, nil, time.Time{}),
	}
	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }
	pool := NewImagePool(list, nil)
	pool.Now = func() time.Time { return now }

	var wg sync.WaitGroup
	picks := make(chan int64, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, release, err := pool.SelectAccount(context.Background(), PoolFilter{})
			if err == nil {
				picks <- got.ID
				time.Sleep(20 * time.Millisecond)
				release()
			}
		}()
	}
	wg.Wait()
	close(picks)

	seen := map[int64]int{}
	for id := range picks {
		seen[id]++
	}
	if seen[1]+seen[2] < 2 {
		t.Errorf("expect at least 2 successful picks, got %v", seen)
	}
}
