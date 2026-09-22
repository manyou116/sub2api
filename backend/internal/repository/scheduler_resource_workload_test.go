//go:build unit && linux

package repository

// This opt-in workload uses production repositories and Redis cache methods, but
// deliberately never starts the server, refresh workers, or upstream clients.
// It models the account-event consumer's calls, not outbox polling, selection
// policy, slot acquisition, or an end-to-end gateway request.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const syntheticBenchDSN = "host=/tmp/sub2api-perf/pgsocket user=postgres dbname=postgres sslmode=disable application_name=sub2api_synthetic_bench"

type syntheticSQLStats struct {
	Calls               int64   `json:"calls"`
	AccountQueryCalls   int64   `json:"account_query_calls"`
	GroupAggregateCalls int64   `json:"group_aggregate_calls"`
	Rows                int64   `json:"rows"`
	ExecMS              float64 `json:"exec_ms"`
	TempReadBlocks      int64   `json:"temp_read_blocks"`
	TempWrittenBlocks   int64   `json:"temp_written_blocks"`
	SharedReadBlocks    int64   `json:"shared_read_blocks"`
	SharedHitBlocks     int64   `json:"shared_hit_blocks"`
}

func syntheticReadSQLStats(t *testing.T, ctx context.Context, db *sql.DB) syntheticSQLStats {
	t.Helper()
	var s syntheticSQLStats
	err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(calls),0),
		COALESCE(SUM(calls) FILTER (WHERE query ILIKE '%accounts%'),0),
		COALESCE(SUM(calls) FILTER (WHERE query ILIKE '%COUNT(*) FILTER%' AND query ILIKE '%account_groups ag%'),0),
		COALESCE(SUM(rows),0), COALESCE(SUM(total_exec_time),0),
		COALESCE(SUM(temp_blks_read),0), COALESCE(SUM(temp_blks_written),0),
		COALESCE(SUM(shared_blks_read),0), COALESCE(SUM(shared_blks_hit),0)
		FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname=current_database())
		AND query NOT ILIKE '%pg_stat_%'`).Scan(&s.Calls, &s.AccountQueryCalls, &s.GroupAggregateCalls,
		&s.Rows, &s.ExecMS, &s.TempReadBlocks, &s.TempWrittenBlocks, &s.SharedReadBlocks, &s.SharedHitBlocks)
	syntheticRequire(t, err)
	return s
}

func (s syntheticSQLStats) subtract(b syntheticSQLStats) syntheticSQLStats {
	return syntheticSQLStats{s.Calls - b.Calls, s.AccountQueryCalls - b.AccountQueryCalls,
		s.GroupAggregateCalls - b.GroupAggregateCalls, s.Rows - b.Rows, s.ExecMS - b.ExecMS,
		s.TempReadBlocks - b.TempReadBlocks, s.TempWrittenBlocks - b.TempWrittenBlocks,
		s.SharedReadBlocks - b.SharedReadBlocks, s.SharedHitBlocks - b.SharedHitBlocks}
}

func syntheticRequire(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func syntheticEnv(name, fallback string) string {
	if s := os.Getenv(name); s != "" {
		return s
	}
	return fallback
}

func syntheticRedisInfo(t *testing.T, ctx context.Context, rdb *redis.Client) map[string]float64 {
	t.Helper()
	raw, err := rdb.Info(ctx, "stats", "memory", "cpu").Result()
	syntheticRequire(t, err)
	values := make(map[string]float64)
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(parts) == 2 {
			if value, err := strconv.ParseFloat(parts[1], 64); err == nil {
				values[parts[0]] = value
			}
		}
	}
	return values
}

func syntheticLatency(values []float64) map[string]any {
	if len(values) == 0 {
		return map[string]any{"count": 0}
	}
	sort.Float64s(values)
	percentile := func(p int) float64 {
		idx := (len(values)*p+99)/100 - 1
		return values[idx]
	}
	return map[string]any{"count": len(values), "p50_ms": percentile(50),
		"p95_ms": percentile(95), "p99_ms": percentile(99), "max_ms": values[len(values)-1]}
}

func syntheticCPUSeconds(r syscall.Rusage) float64 {
	return float64(r.Utime.Sec+r.Stime.Sec) + float64(r.Utime.Usec+r.Stime.Usec)/1e6
}

func TestSchedulerResourceWorkload(t *testing.T) {
	if os.Getenv("SUB2API_SYNTHETIC_BENCH") != "1" {
		t.Skip("opt-in isolated synthetic workload; fixed Unix sockets only")
	}
	runtime.GOMAXPROCS(2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	db, err := sql.Open("postgres", syntheticBenchDSN)
	syntheticRequire(t, err)
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	syntheticRequire(t, db.PingContext(ctx))
	rdb := redis.NewClient(&redis.Options{Network: "unix", Addr: "/tmp/sub2api-perf/redis.sock", PoolSize: 4})
	defer func() { _ = rdb.Close() }()
	syntheticRequire(t, rdb.Ping(ctx).Err())
	mode := syntheticEnv("BENCH_MODE", "run")
	if mode == "prepare" {
		syntheticPrepare(t, ctx, db)
		fmt.Println(`BENCH_RESULT {"mode":"prepare","openai_accounts":18000,"grok_accounts":27000,"synthetic_only":true}`)
		return
	}
	if mode != "run" {
		t.Fatalf("invalid BENCH_MODE %q", mode)
	}
	variant := syntheticEnv("BENCH_VARIANT", "after")
	if variant != "before" && variant != "after" {
		t.Fatalf("invalid BENCH_VARIANT %q", variant)
	}
	workload := syntheticEnv("BENCH_WORKLOAD", "mixed")
	if workload != "mixed" && workload != "group" && workload != "credentials" {
		t.Fatalf("invalid BENCH_WORKLOAD %q", workload)
	}
	requests, err := strconv.Atoi(syntheticEnv("BENCH_REQUESTS", "200"))
	syntheticRequire(t, err)
	if requests < 50 || requests > 10000 || requests%50 != 0 {
		t.Fatal("BENCH_REQUESTS must be a multiple of 50 between 50 and 10000")
	}
	parallelism, err := strconv.Atoi(syntheticEnv("BENCH_PARALLELISM", "1"))
	syntheticRequire(t, err)
	if parallelism < 1 || parallelism > 4 {
		t.Fatal("BENCH_PARALLELISM must be between 1 and 4")
	}
	readSnapshot := workload == "mixed" && syntheticEnv("BENCH_SNAPSHOT_READ", "1") == "1"
	readLoad := readSnapshot && syntheticEnv("BENCH_LOAD_READ", "1") == "1"
	syntheticVerifyDataset(t, ctx, db)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	cache := NewSchedulerCache(rdb).(*schedulerCache)
	concurrencyCache := NewConcurrencyCache(rdb, 30, 1800)
	accounts := newAccountRepositoryWithSQL(client, db, cache)
	groups := newGroupRepositoryWithSQL(client, db)
	buckets := []service.SchedulerBucket{
		{GroupID: 1, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle},
		{GroupID: 2, Platform: service.PlatformGrok, Mode: service.SchedulerModeSingle},
	}
	// Setup is outside all reported deltas. Each run starts with the same Redis
	// contents and fully prepared buckets. FlushDB can only reach the fixed socket.
	syntheticRequire(t, rdb.FlushDB(ctx).Err())
	_, err = db.ExecContext(ctx, "DELETE FROM scheduler_outbox")
	syntheticRequire(t, err)
	for i, bucket := range buckets {
		syntheticPublishGroup(t, ctx, accounts, cache, bucket, []int{18000, 27000}[i])
		full, err := groups.GetByID(ctx, bucket.GroupID)
		syntheticRequire(t, err)
		if full.AccountCount != int64([]int{18000, 27000}[i]) {
			t.Fatal("group account count query failed or returned an incomplete group")
		}
		lite, err := groups.GetByIDLite(ctx, bucket.GroupID)
		syntheticRequire(t, err)
		full.AccountCount, full.ActiveAccountCount, full.RateLimitedAccountCount = 0, 0, 0
		if !reflect.DeepEqual(full, lite) {
			t.Fatal("group configuration differs beyond the intentionally omitted counts")
		}
	}
	// Release setup's account slices before measuring the long-lived client path.
	runtime.GC()
	debug.FreeOSMemory()
	beforeSQL := syntheticReadSQLStats(t, ctx, db)
	beforeRedis := syntheticRedisInfo(t, ctx, rdb)
	var beforeMem, afterMem runtime.MemStats
	var beforeCPU, afterCPU syscall.Rusage
	runtime.ReadMemStats(&beforeMem)
	syntheticRequire(t, syscall.Getrusage(syscall.RUSAGE_SELF, &beforeCPU))
	fmt.Printf("BENCH_MEASURE_START variant=%s workload=%s requests=%d parallelism=%d snapshot_read=%t load_read=%t\n", variant, workload, requests, parallelism, readSnapshot, readLoad)
	started := time.Now()
	groupLatency := make([]float64, 0, requests)
	credentialLatency := make([]float64, 0, requests/50)
	latencyByGroup := map[int64][]float64{1: {}, 2: {}}
	updated := make(map[int64]string)
	publications, decoded := 0, 0
	loadAccountsRead := 0
	peakHeapAlloc := beforeMem.HeapAlloc
	// Requests within a 50-operation wave may overlap; credential events remain
	// between waves, matching the single-worker workload's event placement.
	type readResult struct {
		elapsed float64
		decoded int
		err     error
	}
	readGroup := func(i int) (result readResult) {
		bucket := buckets[i%len(buckets)]
		opStarted := time.Now()
		defer func() { result.elapsed = float64(time.Since(opStarted).Nanoseconds()) / 1e6 }()
		var group *service.Group
		if variant == "before" {
			group, result.err = groups.GetByID(ctx, bucket.GroupID)
		} else {
			group, result.err = groups.GetByIDLite(ctx, bucket.GroupID)
		}
		if result.err != nil {
			return
		}
		if group.ID != bucket.GroupID || group.Platform != bucket.Platform {
			result.err = fmt.Errorf("incorrect group configuration")
			return
		}
		if !readSnapshot {
			return
		}
		snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
		if err != nil {
			result.err = err
			return
		}
		if !hit || len(snapshot) != []int{18000, 27000}[i%2] {
			result.err = fmt.Errorf("incomplete snapshot: group=%d hit=%t count=%d", bucket.GroupID, hit, len(snapshot))
			return
		}
		result.decoded = len(snapshot)
		if readLoad {
			candidates := make([]service.AccountWithConcurrency, len(snapshot))
			for index, account := range snapshot {
				candidates[index] = service.AccountWithConcurrency{ID: account.ID, MaxConcurrency: account.Concurrency}
			}
			loads, err := concurrencyCache.GetAccountsLoadBatch(ctx, candidates)
			if err != nil {
				result.err = err
				return
			}
			if len(loads) != len(snapshot) {
				result.err = fmt.Errorf("incomplete candidate load map")
			}
		}
		return
	}
	for i := 0; i < requests; i++ {
		if workload != "credentials" && i%50 == 0 {
			results := make([]readResult, 50)
			var workers sync.WaitGroup
			for worker := 0; worker < parallelism; worker++ {
				workers.Add(1)
				go func(worker, start int) {
					defer workers.Done()
					for offset := worker; offset < len(results); offset += parallelism {
						results[offset] = readGroup(start + offset)
					}
				}(worker, i)
			}
			workers.Wait()
			for offset, result := range results {
				syntheticRequire(t, result.err)
				groupID := buckets[(i+offset)%len(buckets)].GroupID
				groupLatency = append(groupLatency, result.elapsed)
				latencyByGroup[groupID] = append(latencyByGroup[groupID], result.elapsed)
				decoded += result.decoded
				if readLoad {
					loadAccountsRead += result.decoded
				}
			}
		}
		if workload != "group" && (i+1)%50 == 0 {
			eventIndex := (i+1)/50 - 1
			bucket := buckets[eventIndex%2]
			accountID := int64(eventIndex/2 + 1)
			if bucket.GroupID == 2 {
				accountID += 18000
			}
			token := fmt.Sprintf("synthetic-never-valid-%d-%s", eventIndex, strings.Repeat("a", 768))
			opStarted := time.Now()
			credentials := map[string]any{
				"access_token": token, "refresh_token": "synthetic-never-valid-" + strings.Repeat("r", 512),
				"expires_at": 4102444800, "synthetic_only": true,
			}
			if bucket.Platform == service.PlatformGrok {
				current, err := accounts.GetByID(ctx, accountID)
				syntheticRequire(t, err)
				changed, err := accounts.UpdateGrokOAuthCredentialsIfUnchanged(ctx, accountID, current.Credentials, current.ProxyID, credentials)
				syntheticRequire(t, err)
				if !changed {
					t.Fatal("synthetic Grok credential CAS unexpectedly missed")
				}
			} else {
				syntheticRequire(t, accounts.UpdateCredentials(ctx, accountID, credentials))
			}
			// These are the handler's production repository/cache calls. The
			// current producer is common to both variants; only the consumer's
			// full-group branch is toggled, isolating the rebuild cost.
			account, err := accounts.GetByID(ctx, accountID)
			syntheticRequire(t, err)
			syntheticRequire(t, cache.SetAccount(ctx, account))
			if variant == "before" {
				syntheticPublishGroup(t, ctx, accounts, cache, bucket, []int{18000, 27000}[eventIndex%2])
				publications += 2
			}
			credentialLatency = append(credentialLatency, float64(time.Since(opStarted).Nanoseconds())/1e6)
			updated[accountID] = token
		}
		if (i+1)%10 == 0 {
			runtime.ReadMemStats(&afterMem)
			if afterMem.HeapAlloc > peakHeapAlloc {
				peakHeapAlloc = afterMem.HeapAlloc
			}
		}
	}
	elapsed := time.Since(started)
	syntheticRequire(t, syscall.Getrusage(syscall.RUSAGE_SELF, &afterCPU))
	runtime.ReadMemStats(&afterMem)
	fmt.Printf("BENCH_MEASURE_END variant=%s workload=%s\n", variant, workload)
	afterSQL := syntheticReadSQLStats(t, ctx, db)
	afterRedis := syntheticRedisInfo(t, ctx, rdb)
	// Correctness assertions occur after measurement, using the real cache.
	for id, token := range updated {
		account, err := cache.GetAccount(ctx, id)
		syntheticRequire(t, err)
		if account == nil || account.Credentials["access_token"] != token {
			t.Fatalf("updated synthetic credentials missing from cache for account %d", id)
		}
	}
	var eventCount, cacheOnlyCount int
	syntheticRequire(t, db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE payload->'cache_only'='true'::jsonb) FROM scheduler_outbox`).Scan(&eventCount, &cacheOnlyCount))
	if eventCount != len(updated) || eventCount != cacheOnlyCount {
		t.Fatalf("credential outbox invariant failed: events=%d cache_only=%d updates=%d", eventCount, cacheOnlyCount, len(updated))
	}
	for id := range updated {
		if id <= 18000 {
			continue
		}
		current, err := accounts.GetByID(ctx, id)
		syntheticRequire(t, err)
		stale := make(map[string]any, len(current.Credentials))
		for key, value := range current.Credentials {
			stale[key] = value
		}
		stale["access_token"] = "synthetic-stale-CAS-token"
		changed, err := accounts.UpdateGrokOAuthCredentialsIfUnchanged(ctx, id, stale, current.ProxyID, current.Credentials)
		syntheticRequire(t, err)
		if changed {
			t.Fatal("stale Grok credential CAS unexpectedly succeeded")
		}
		var afterMissCount int
		syntheticRequire(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduler_outbox").Scan(&afterMissCount))
		if afterMissCount != eventCount {
			t.Fatal("stale Grok credential CAS generated an outbox event")
		}
		break
	}
	result := map[string]any{
		"variant": variant, "workload": workload, "snapshot_read": readSnapshot, "load_read": readLoad,
		"parallelism": parallelism,
		"scope":       "production repository+Redis methods including all-candidate load reads when enabled; idle slots; one sparse credential event per independent poll batch; modeled consumer ignores cache_only before; excludes outbox polling, policy sorting, slot acquisition and upstream calls",
		"accounts":    45000, "groups": map[string]int{"openai": 18000, "grok": 27000},
		"wall_seconds": elapsed.Seconds(), "process_cpu_seconds": syntheticCPUSeconds(afterCPU) - syntheticCPUSeconds(beforeCPU),
		"group_latency": syntheticLatency(groupLatency), "credential_latency": syntheticLatency(credentialLatency),
		"openai_group_latency": syntheticLatency(latencyByGroup[1]), "grok_group_latency": syntheticLatency(latencyByGroup[2]),
		"snapshot_publications": publications, "snapshot_accounts_decoded": decoded,
		"load_accounts_read": loadAccountsRead,
		"go_alloc_bytes":     afterMem.TotalAlloc - beforeMem.TotalAlloc, "go_mallocs": afterMem.Mallocs - beforeMem.Mallocs,
		"go_gc_cycles": afterMem.NumGC - beforeMem.NumGC, "go_gc_pause_ns": afterMem.PauseTotalNs - beforeMem.PauseTotalNs,
		"go_heap_start_bytes": beforeMem.HeapAlloc, "go_heap_end_bytes": afterMem.HeapAlloc, "go_heap_sampled_peak_bytes": peakHeapAlloc,
		"sql":                      afterSQL.subtract(beforeSQL),
		"redis_commands":           afterRedis["total_commands_processed"] - beforeRedis["total_commands_processed"] - 1,
		"redis_cpu_seconds":        afterRedis["used_cpu_sys"] + afterRedis["used_cpu_user"] - beforeRedis["used_cpu_sys"] - beforeRedis["used_cpu_user"],
		"redis_memory_start_bytes": beforeRedis["used_memory"], "redis_memory_end_bytes": afterRedis["used_memory"],
		"correctness": "group config equivalent; full snapshot cardinalities retained; updated credentials visible; credential outbox cache_only; Grok stale CAS rejected when exercised",
	}
	encoded, err := json.Marshal(result)
	syntheticRequire(t, err)
	fmt.Printf("BENCH_RESULT %s\n", encoded)
}

func syntheticPublishGroup(t *testing.T, ctx context.Context, accounts *accountRepository, cache *schedulerCache, bucket service.SchedulerBucket, expected int) {
	t.Helper()
	forced := bucket
	forced.Mode = service.SchedulerModeForced
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	syntheticRequire(t, err)
	forcedToken, err := cache.CaptureBucketWriteToken(ctx, forced)
	syntheticRequire(t, err)
	list, err := accounts.ListSchedulableByGroupIDAndPlatform(ctx, bucket.GroupID, bucket.Platform)
	syntheticRequire(t, err)
	if len(list) != expected {
		t.Fatalf("unexpected schedulable count: group=%d got=%d want=%d", bucket.GroupID, len(list), expected)
	}
	ids, err := cache.SetSnapshotAndReturnAccountIDs(ctx, bucket, token, list)
	syntheticRequire(t, err)
	syntheticRequire(t, cache.SetSnapshotByAccountIDs(ctx, forced, forcedToken, ids))
}

func syntheticVerifyDataset(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var total, synthetic int
	syntheticRequire(t, db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE name LIKE 'synthetic-perf-%' AND credentials->>'synthetic_only'='true') FROM accounts`).Scan(&total, &synthetic))
	if total != 45000 || total != synthetic {
		t.Fatalf("refusing workload: expected exactly 45000 synthetic accounts, got total=%d synthetic=%d", total, synthetic)
	}
	var groups int
	syntheticRequire(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM groups WHERE (id=1 AND name='synthetic-perf-openai') OR (id=2 AND name='synthetic-perf-grok')`).Scan(&groups))
	if groups != 2 {
		t.Fatal("refusing workload: synthetic group identities do not match")
	}
}

func syntheticPrepare(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	// Refuse to modify any pre-existing application schema. Reuse is supported
	// through mode=run only, and run additionally validates all account records.
	var exists bool
	syntheticRequire(t, db.QueryRowContext(ctx, "SELECT to_regclass('public.accounts') IS NOT NULL").Scan(&exists))
	if exists {
		t.Fatal("prepare requires an empty isolated database; accounts already exists")
	}
	syntheticRequire(t, ApplyMigrations(ctx, db))
	queries := []string{
		`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`,
		`DELETE FROM groups`, // Remove migration 008's empty default group.
		`INSERT INTO groups (id,name,platform,status) VALUES (1,'synthetic-perf-openai','openai','active'), (2,'synthetic-perf-grok','grok','active')`,
		`INSERT INTO accounts (id,name,platform,type,credentials,extra,concurrency,priority,status,schedulable,last_used_at)
		 SELECT n,'synthetic-perf-'||n,CASE WHEN n<=18000 THEN 'openai' ELSE 'grok' END,'oauth',
		 jsonb_build_object('access_token','synthetic-never-valid-'||n||repeat('a',768),
		 'refresh_token','synthetic-never-valid-'||n||repeat('r',512),'expires_at',4102444800,'synthetic_only',true),
		 jsonb_build_object('synthetic_only',true,'privacy_mode','training_off','padding',repeat('x',384)),
		 3, 1+(n%10),'active',true,NOW()-(n%1000)*INTERVAL '1 second' FROM generate_series(1,45000) n`,
		`INSERT INTO account_groups(account_id,group_id,priority) SELECT id,CASE WHEN id<=18000 THEN 1 ELSE 2 END,priority FROM accounts`,
		`SELECT setval(pg_get_serial_sequence('accounts','id'),45000)`,
		`SELECT setval(pg_get_serial_sequence('groups','id'),2)`,
		`ANALYZE accounts`, `ANALYZE account_groups`, `ANALYZE groups`,
	}
	for _, query := range queries {
		_, err := db.ExecContext(ctx, query)
		syntheticRequire(t, err)
	}
	syntheticVerifyDataset(t, ctx, db)
}
