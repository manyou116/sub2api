//go:build unit && (darwin || linux)

package repository

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// Run against a private real Redis process so B/op measures client allocations,
// not an in-process Redis emulator. No existing server or upstream is contacted.
//
//	GOMAXPROCS=2 GOCACHE=/Volumes/Dev/Cache/Caches/go-build go test -tags=unit ./internal/repository \
//	  -run '^$' -bench '^BenchmarkSchedulerLargeGroup' -benchmem -benchtime=3x -count=3
type largeGroupRedisCounter struct {
	commands atomic.Int64
	batches  atomic.Int64
}

func (h *largeGroupRedisCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *largeGroupRedisCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.commands.Add(1)
		h.batches.Add(1)
		return next(ctx, cmd)
	}
}

func (h *largeGroupRedisCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.commands.Add(int64(len(cmds)))
		h.batches.Add(1)
		return next(ctx, cmds)
	}
}

func (h *largeGroupRedisCounter) reset() {
	h.commands.Store(0)
	h.batches.Store(0)
}

func (h *largeGroupRedisCounter) report(b *testing.B) {
	b.ReportMetric(float64(h.commands.Load())/float64(b.N), "redis_commands/op")
	b.ReportMetric(float64(h.batches.Load())/float64(b.N), "redis_batches/op")
}

func newLargeGroupBenchmarkRedis(b *testing.B) (*redis.Client, *largeGroupRedisCounter) {
	b.Helper()
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		b.Skip("redis-server is required for the isolated large-group benchmark")
	}
	// macOS Unix socket paths have a short length limit; b.TempDir can exceed it.
	dir, err := os.MkdirTemp("/tmp", "sub2api-sched-bench-")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = os.RemoveAll(dir) })
	logFile, err := os.Create(filepath.Join(dir, "redis.log"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = logFile.Close() })
	socket := filepath.Join(dir, "redis.sock")
	cmd := exec.Command(binary, "--port", "0", "--unixsocket", socket,
		"--unixsocketperm", "700", "--save", "", "--appendonly", "no", "--dir", dir)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	rdb := redis.NewClient(&redis.Options{
		Network: "unix", Addr: socket, PoolSize: 2, MaxRetries: -1,
		DialTimeout: time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
	})
	b.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if err := rdb.Ping(ctx).Err(); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			b.Fatal("private Redis did not become ready:", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	counter := &largeGroupRedisCounter{}
	rdb.AddHook(counter)
	return rdb, counter
}

func largeGroupBenchmarkAccount(id int64, platform string) service.Account {
	lastUsed := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Second)
	expires := lastUsed.Add(24 * time.Hour)
	rate := 1.0
	models := map[string]any{"gpt-5.4": "gpt-5.4", "gpt-5.4-mini": "gpt-5.4-mini", "gpt-5.3-codex": "gpt-5.3-codex"}
	extra := map[string]any{
		"codex_5h_used_percent": float64(id % 90), "codex_7d_used_percent": float64(id % 80),
		"codex_5h_reset_at": expires.Format(time.RFC3339), "codex_usage_updated_at": lastUsed.Format(time.RFC3339),
		"auto_pause_5h_threshold": 95, "auto_pause_7d_threshold": 95,
		"openai_oauth_responses_websockets_v2_enabled": true,
	}
	if platform == service.PlatformGrok {
		models = map[string]any{"grok-4": "grok-4", "grok-4.1-fast": "grok-4.1-fast", "grok-4.5": "grok-4.5"}
		extra = map[string]any{"grok_media_eligible": true, "grok_billing_snapshot": map[string]any{
			"status": "ok", "plan": "free", "updated_at": lastUsed.Format(time.RFC3339),
		}}
	}
	return service.Account{
		ID: id, Name: fmt.Sprintf("synthetic-%s-%d", platform, id), Platform: platform,
		Type: service.AccountTypeOAuth, Concurrency: 10, Priority: 1, RateMultiplier: &rate,
		Status: service.StatusActive, Schedulable: true, LastUsedAt: &lastUsed, ExpiresAt: &expires,
		Credentials: map[string]any{
			"access_token":  "synthetic-not-a-credential-" + strings.Repeat("x", 256),
			"refresh_token": "synthetic-not-a-credential-" + strings.Repeat("y", 128),
			"plan_type":     "free", "model_mapping": models,
		}, Extra: extra,
		GroupIDs: []int64{1}, AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: 1, Priority: 1, CreatedAt: lastUsed}},
	}
}

func BenchmarkSchedulerLargeGroupSnapshot(b *testing.B) {
	for _, tc := range []struct {
		platform string
		size     int
	}{{service.PlatformOpenAI, 18_000}, {service.PlatformGrok, 27_000}} {
		b.Run(fmt.Sprintf("%s_%d", tc.platform, tc.size), func(b *testing.B) {
			rdb, counter := newLargeGroupBenchmarkRedis(b)
			cache := NewSchedulerCache(rdb)
			ctx := context.Background()
			bucket := service.SchedulerBucket{GroupID: 1, Platform: tc.platform, Mode: service.SchedulerModeSingle}
			accounts := make([]service.Account, tc.size)
			lastUsed := make(map[int64]time.Time, tc.size)
			var metadataBytes int64
			for i := range accounts {
				accounts[i] = largeGroupBenchmarkAccount(int64(i+1), tc.platform)
				lastUsed[accounts[i].ID] = *accounts[i].LastUsedAt
				_, meta, err := marshalSchedulerCacheAccount(accounts[i])
				if err != nil {
					b.Fatal(err)
				}
				metadataBytes += int64(len(meta))
			}
			token, err := cache.CaptureBucketWriteToken(ctx, bucket)
			if err != nil {
				b.Fatal(err)
			}
			if err := cache.SetSnapshot(ctx, bucket, token, accounts); err != nil {
				b.Fatal(err)
			}
			if err := cache.UpdateLastUsed(ctx, lastUsed); err != nil {
				b.Fatal(err)
			}
			got, hit, err := cache.GetSnapshot(ctx, bucket)
			if err != nil || !hit || len(got) != tc.size {
				b.Fatalf("warm snapshot: hit=%v accounts=%d err=%v", hit, len(got), err)
			}
			counter.reset()
			b.ReportAllocs()
			b.SetBytes(metadataBytes)
			cpuStart := largeGroupBenchmarkCPUTime(b)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, hit, err := cache.GetSnapshot(ctx, bucket)
				if err != nil || !hit || len(got) != tc.size {
					b.Fatalf("snapshot: hit=%v accounts=%d err=%v", hit, len(got), err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(largeGroupBenchmarkCPUTime(b)-cpuStart)/float64(b.N), "go_cpu_ns/op")
			counter.report(b)
			b.ReportMetric(float64(metadataBytes), "metadata_bytes/op")
		})
	}
}

func BenchmarkSchedulerLargeGroupLoadBatch(b *testing.B) {
	for _, size := range []int{18_000, 27_000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			rdb, counter := newLargeGroupBenchmarkRedis(b)
			cache := NewConcurrencyCache(rdb, 15, 120)
			ctx := context.Background()
			accounts := make([]service.AccountWithConcurrency, size)
			for i := range accounts {
				accounts[i] = service.AccountWithConcurrency{ID: int64(i + 1), MaxConcurrency: 10}
			}
			// Seed 1% ordinary and 1% live occupancy; most accounts in a large pool
			// are idle. Read workload remains proportional to the whole pool.
			now := time.Now().Unix()
			pipe := rdb.Pipeline()
			for i := 0; i < size; i += 100 {
				pipe.ZAdd(ctx, accountSlotKey(accounts[i].ID), redis.Z{Score: float64(now), Member: "synthetic-request"})
				pipe.ZAdd(ctx, liveAccountSlotKey(accounts[i+1].ID), redis.Z{Score: float64(now), Member: "synthetic-live"})
			}
			if _, err := pipe.Exec(ctx); err != nil {
				b.Fatal(err)
			}
			counter.reset()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				loads, err := cache.GetAccountsLoadBatch(ctx, accounts)
				if err != nil || len(loads) != size {
					b.Fatalf("load batch: accounts=%d err=%v", len(loads), err)
				}
			}
			b.StopTimer()
			counter.report(b)
		})
	}
}

func BenchmarkSchedulerLargeGroupSnapshotWindow(b *testing.B) {
	for _, tc := range []struct {
		platform string
		size     int
	}{{service.PlatformOpenAI, 18_000}, {service.PlatformGrok, 27_000}} {
		for _, width := range []int{256, 1024} {
			b.Run(fmt.Sprintf("%s_%d/width_%d", tc.platform, tc.size, width), func(b *testing.B) {
				rdb, counter := newLargeGroupBenchmarkRedis(b)
				cache := NewSchedulerCache(rdb).(*schedulerCache)
				ctx := context.Background()
				bucket := service.SchedulerBucket{GroupID: 1, Platform: tc.platform, Mode: service.SchedulerModeSingle}
				accounts := make([]service.Account, tc.size)
				lastUsed := make(map[int64]time.Time, tc.size)
				for i := range accounts {
					accounts[i] = largeGroupBenchmarkAccount(int64(i+1), tc.platform)
					lastUsed[accounts[i].ID] = *accounts[i].LastUsedAt
				}
				token, err := cache.CaptureBucketWriteToken(ctx, bucket)
				if err != nil {
					b.Fatal(err)
				}
				if err := cache.SetSnapshot(ctx, bucket, token, accounts); err != nil {
					b.Fatal(err)
				}
				if err := cache.UpdateLastUsed(ctx, lastUsed); err != nil {
					b.Fatal(err)
				}
				if got, hit, err := cache.GetSnapshotWindow(ctx, bucket, width); err != nil || !hit || len(got) != width {
					b.Fatalf("warm window: hit=%v accounts=%d err=%v", hit, len(got), err)
				}
				counter.reset()
				b.ReportAllocs()
				cpuStart := largeGroupBenchmarkCPUTime(b)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					got, hit, err := cache.GetSnapshotWindow(ctx, bucket, width)
					if err != nil || !hit || len(got) != width {
						b.Fatalf("window: hit=%v accounts=%d err=%v", hit, len(got), err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(largeGroupBenchmarkCPUTime(b)-cpuStart)/float64(b.N), "go_cpu_ns/op")
				counter.report(b)
			})
		}
	}
}

// This comparison isolates cache work for a credential-only refresh. It does
// not include PostgreSQL loads, event polling, rebuild locks, or other buckets.
// The after case models the cache-only event handler; this is not an
// end-to-end event benchmark or a production measurement.
func BenchmarkSchedulerCredentialRefreshCacheWork(b *testing.B) {
	for _, tc := range []struct {
		platform string
		size     int
	}{{service.PlatformOpenAI, 18_000}, {service.PlatformGrok, 27_000}} {
		for _, rebuild := range []bool{true, false} {
			variant := "after_account_only"
			if rebuild {
				variant = "before_account_and_group"
			}
			b.Run(fmt.Sprintf("%s_%d/%s", tc.platform, tc.size, variant), func(b *testing.B) {
				rdb, counter := newLargeGroupBenchmarkRedis(b)
				cache := NewSchedulerCache(rdb).(*schedulerCache)
				ctx := context.Background()
				accounts := make([]service.Account, tc.size)
				for i := range accounts {
					accounts[i] = largeGroupBenchmarkAccount(int64(i+1), tc.platform)
				}
				single := service.SchedulerBucket{GroupID: 1, Platform: tc.platform, Mode: service.SchedulerModeSingle}
				forced := service.SchedulerBucket{GroupID: 1, Platform: tc.platform, Mode: service.SchedulerModeForced}
				singleToken, err := cache.CaptureBucketWriteToken(ctx, single)
				if err != nil {
					b.Fatal(err)
				}
				forcedToken, err := cache.CaptureBucketWriteToken(ctx, forced)
				if err != nil {
					b.Fatal(err)
				}
				ids, err := cache.SetSnapshotAndReturnAccountIDs(ctx, single, singleToken, accounts)
				if err != nil {
					b.Fatal(err)
				}
				if err := cache.SetSnapshotByAccountIDs(ctx, forced, forcedToken, ids); err != nil {
					b.Fatal(err)
				}
				// Updating a token must still refresh the full and metadata account
				// payloads; all group memberships and routing attributes are unchanged.
				accounts[0].Credentials["access_token"] = "synthetic-refreshed-" + strings.Repeat("z", 256)
				runtime.GC()
				counter.reset()
				b.ReportAllocs()
				cpuStart := largeGroupBenchmarkCPUTime(b)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := cache.SetAccount(ctx, &accounts[0]); err != nil {
						b.Fatal(err)
					}
					if rebuild {
						ids, err := cache.SetSnapshotAndReturnAccountIDs(ctx, single, singleToken, accounts)
						if err != nil {
							b.Fatal(err)
						}
						if err := cache.SetSnapshotByAccountIDs(ctx, forced, forcedToken, ids); err != nil {
							b.Fatal(err)
						}
					}
				}
				b.StopTimer()
				cpuElapsed := largeGroupBenchmarkCPUTime(b) - cpuStart
				b.ReportMetric(float64(cpuElapsed)/float64(b.N), "go_cpu_ns/op")
				counter.report(b)
			})
		}
	}
}

// RUSAGE_SELF reports client Go-process CPU, excluding the Redis subprocess.
func largeGroupBenchmarkCPUTime(b *testing.B) int64 {
	b.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return usage.Utime.Nano() + usage.Stime.Nano()
}
