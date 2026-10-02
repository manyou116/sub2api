//go:build unit && (darwin || linux)

package repository

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type largeGroupSelectionAccountRepo struct {
	service.AccountRepository
	accounts map[int64]*service.Account
	rechecks atomic.Int64
}

func (r *largeGroupSelectionAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.rechecks.Add(1)
	account := r.accounts[id]
	if account == nil {
		return nil, fmt.Errorf("synthetic account %d is missing", id)
	}
	return account, nil
}

// Exercise the public legacy selector, including snapshot decoding, eligibility
// filters, load lookup, ordering, account recheck, hydration, and slot acquire /
// release. Only the authoritative account recheck is in-memory: this benchmark
// measures no PostgreSQL time, HTTP forwarding, token refresh, or upstream TTFT.
// The production default 200 ms load cache remains enabled for both variants.
func BenchmarkSchedulerLargeGroupLegacySelection(b *testing.B) {
	for _, tc := range []struct {
		platform string
		model    string
		size     int
	}{{service.PlatformOpenAI, "gpt-5.4", 18_000}, {service.PlatformGrok, "grok-4.1-fast", 27_000}} {
		for _, bounded := range []bool{false, true} {
			variant, flag := "full", ""
			if bounded {
				variant, flag = "window_1024", "true"
			}
			b.Run(fmt.Sprintf("%s_%d/%s", tc.platform, tc.size, variant), func(b *testing.B) {
				b.Setenv("SUB2API_SCHEDULER_BOUNDED_PROBE", flag)
				rdb, counter := newLargeGroupBenchmarkRedis(b)
				cache := NewSchedulerCache(rdb)
				ctx := context.Background()
				bucket := service.SchedulerBucket{GroupID: 1, Platform: tc.platform, Mode: service.SchedulerModeSingle}
				accounts := make([]service.Account, tc.size)
				repo := &largeGroupSelectionAccountRepo{accounts: make(map[int64]*service.Account, tc.size)}
				for i := range accounts {
					accounts[i] = largeGroupBenchmarkAccount(int64(i+1), tc.platform)
					// Keep the fixture valid at any date and use equal LRU timestamps
					// so the production tie randomization is exercised.
					accounts[i].ExpiresAt = nil
					accounts[i].LastUsedAt = nil
					repo.accounts[accounts[i].ID] = &accounts[i]
				}
				token, err := cache.CaptureBucketWriteToken(ctx, bucket)
				if err != nil {
					b.Fatal(err)
				}
				if err := cache.SetSnapshot(ctx, bucket, token, accounts); err != nil {
					b.Fatal(err)
				}
				cfg := &config.Config{Gateway: config.GatewayConfig{
					Scheduling: config.GatewaySchedulingConfig{LoadBatchEnabled: true, LoadBatchCacheTTLMS: 200},
				}}
				snapshot := service.NewSchedulerSnapshotService(cache, nil, repo, nil, cfg)
				concurrency := service.NewConcurrencyService(NewConcurrencyCache(rdb, 15, 120))
				gateway := service.NewOpenAIGatewayService(
					repo, nil, nil, nil, nil, nil, nil, cfg, snapshot, concurrency,
					nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				)
				selectAndRelease := func() {
					selection, _, err := gateway.SelectAccountWithSchedulerForCapability(
						ctx, &bucket.GroupID, "", "", tc.model, nil,
						service.OpenAIUpstreamTransportAny, "", false, false, false, tc.platform,
					)
					if err != nil || selection == nil || !selection.Acquired || selection.ReleaseFunc == nil {
						b.Fatalf("select/acquire: selection=%v err=%v", selection, err)
					}
					selection.ReleaseFunc()
				}
				selectAndRelease()
				counter.reset()
				repo.rechecks.Store(0)
				b.ReportAllocs()
				cpuStart := largeGroupBenchmarkCPUTime(b)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					selectAndRelease()
				}
				b.StopTimer()
				b.ReportMetric(float64(largeGroupBenchmarkCPUTime(b)-cpuStart)/float64(b.N), "go_cpu_ns/op")
				b.ReportMetric(float64(repo.rechecks.Load())/float64(b.N), "simulated_db_rechecks/op")
				counter.report(b)
				metrics := gateway.SnapshotOpenAIAccountSchedulerMetrics()
				if bounded && (metrics.BoundedProbeTotal != int64(b.N+1) || metrics.BoundedProbeFallbacks != 0) {
					b.Fatalf("bounded path was not used consistently: attempts=%d fallbacks=%d", metrics.BoundedProbeTotal, metrics.BoundedProbeFallbacks)
				}
			})
		}
	}
}
