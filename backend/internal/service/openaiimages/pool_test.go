package openaiimages

import (
	"context"
	"errors"
	"math/rand"
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
		makeAcc(2, "active", true, map[string]any{"image_quota_remaining": 5.0}, older),  // 同 quota，更老 last_used 优先
		makeAcc(3, "active", true, map[string]any{"image_quota_remaining": 10.0}, newer), // 最高 quota → 头号
	}

	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }
	pool := NewImagePool(list, nil)
	pool.Now = func() time.Time { return now }
	// TopKPick=0 → 强单调（旧行为）；本测试验证排序键正确性

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

// TestImagePool_TopKPickRandomizes 验证 TopKPick > 1 时,前 K 个候选都会被采样到,
// 而不是永远只返回 ready[0]。这是修复"几千账号池只用 34 个"热点 bug 的关键测试。
func TestImagePool_TopKPickRandomizes(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	accs := make([]PoolAccount, 0, 5)
	for i := 1; i <= 5; i++ {
		// 5 个账号 quota 递减 (50/40/30/20/10),按当前排序应永远选 id=1
		accs = append(accs, makeAcc(int64(i), "active", true,
			map[string]any{"image_quota_remaining": float64(60 - i*10)}, time.Time{}))
	}
	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }
	pool := NewImagePool(list, nil)
	pool.Now = func() time.Time { return now }
	pool.TopKPick = 5
	pool.Rand = rand.New(rand.NewSource(42))

	seen := map[int64]int{}
	for i := 0; i < 200; i++ {
		got, release, err := pool.SelectAccount(context.Background(), PoolFilter{})
		if err != nil {
			t.Fatal(err)
		}
		seen[got.ID]++
		release()
	}
	if len(seen) < 4 {
		t.Errorf("TopKPick=5 expected to hit at least 4 distinct accounts in 200 picks, got %d (distribution=%v)", len(seen), seen)
	}
}

// TestImagePool_ExploreUnknownProb 验证 ExploreUnknownProb > 0 时,quota=0/未探测的账号
// 也会被偶尔选中(自然探测),避免几千账号被永久打入冷宫。
func TestImagePool_ExploreUnknownProb(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	accs := []PoolAccount{
		makeAcc(1, "active", true, map[string]any{"image_quota_remaining": 25.0}, time.Time{}),
		makeAcc(2, "active", true, nil, time.Time{}), // quota=-1 (未探测)
		makeAcc(3, "active", true, nil, time.Time{}),
		makeAcc(4, "active", true, nil, time.Time{}),
	}
	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }
	pool := NewImagePool(list, nil)
	pool.Now = func() time.Time { return now }
	pool.TopKPick = 1
	pool.ExploreUnknownProb = 0.5 // 高概率方便统计
	pool.Rand = rand.New(rand.NewSource(1))

	hitUnknown := 0
	for i := 0; i < 200; i++ {
		got, release, _ := pool.SelectAccount(context.Background(), PoolFilter{})
		release()
		if got.ID >= 2 {
			hitUnknown++
		}
	}
	if hitUnknown < 50 {
		t.Errorf("ExploreUnknownProb=0.5 expected ~100 unknown hits in 200, got %d", hitUnknown)
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

func TestImagePool_WaitForLeaseRelease(t *testing.T) {
	now := time.Now()
	accs := []PoolAccount{
		makeAcc(1, "active", true, map[string]any{"image_quota_remaining": 5.0}, now),
	}
	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }

	pool := NewImagePool(list, nil)
	pool.WaitMaxFraction = 0.5
	pool.WaitMaxDuration = 2 * time.Second

	// 先占用唯一账号
	_, release, err := pool.SelectAccount(context.Background(), PoolFilter{})
	if err != nil {
		t.Fatalf("first select: %v", err)
	}

	// 后台 100ms 后 release
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	start := time.Now()
	got, release2, err := pool.SelectAccount(ctx, PoolFilter{})
	if err != nil {
		t.Fatalf("waiter should succeed after release: %v", err)
	}
	defer release2()
	elapsed := time.Since(start)
	if got.ID != 1 {
		t.Errorf("got id=%d want 1", got.ID)
	}
	if elapsed < 50*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("expected wait ~100ms, got %v", elapsed)
	}
}

func TestImagePool_WaitTimeoutReturnsErr(t *testing.T) {
	now := time.Now()
	accs := []PoolAccount{
		makeAcc(1, "active", true, map[string]any{"image_quota_remaining": 5.0}, now),
	}
	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }

	pool := NewImagePool(list, nil)
	pool.WaitMaxFraction = 0.5
	pool.WaitMaxDuration = 200 * time.Millisecond

	// 占用且不 release
	_, _, err := pool.SelectAccount(context.Background(), PoolFilter{})
	if err != nil {
		t.Fatalf("first select: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err = pool.SelectAccount(ctx, PoolFilter{})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrNoImageAccount) {
		t.Errorf("want ErrNoImageAccount, got %v", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("expected wait ~200ms before err, got %v", elapsed)
	}
}

func TestImagePool_FIFOWaiterOrder(t *testing.T) {
	now := time.Now()
	accs := []PoolAccount{
		makeAcc(1, "active", true, map[string]any{"image_quota_remaining": 5.0}, now),
	}
	list := func(_ context.Context, _ PoolFilter) ([]PoolAccount, error) { return accs, nil }

	pool := NewImagePool(list, nil)
	pool.WaitMaxFraction = 0.9
	pool.WaitMaxDuration = 2 * time.Second

	_, release, err := pool.SelectAccount(context.Background(), PoolFilter{})
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	order := make(chan int, 3)
	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		i := i
		wg.Add(1)
		// 按时间间隔启动，保证入队顺序确定
		time.Sleep(20 * time.Millisecond)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			_, rel, err := pool.SelectAccount(ctx, PoolFilter{})
			if err != nil {
				t.Errorf("waiter %d: %v", i, err)
				return
			}
			order <- i
			time.Sleep(50 * time.Millisecond)
			rel()
		}()
	}
	time.Sleep(100 * time.Millisecond)
	release()
	wg.Wait()
	close(order)

	got := []int{}
	for v := range order {
		got = append(got, v)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("FIFO order broken, got %v", got)
	}
}
