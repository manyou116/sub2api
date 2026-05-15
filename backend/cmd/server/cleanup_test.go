package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunCleanupParallel_CompletesWhenAllStepsFinish(t *testing.T) {
	var done int32
	steps := []CleanupStep{
		{Name: "a", Fn: func() error { atomic.AddInt32(&done, 1); return nil }},
		{Name: "b", Fn: func() error { atomic.AddInt32(&done, 1); return nil }},
		{Name: "c", Fn: func() error { atomic.AddInt32(&done, 1); return errors.New("boom") }},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	RunCleanupParallel(ctx, steps)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("RunCleanupParallel took too long: %v", elapsed)
	}
	if atomic.LoadInt32(&done) != 3 {
		t.Fatalf("expected all 3 steps to run, got %d", done)
	}
}

func TestRunCleanupParallel_AbandonsHungStepsAfterDeadline(t *testing.T) {
	var finished int32
	hang := make(chan struct{})
	defer close(hang) // release goroutine after test

	steps := []CleanupStep{
		{Name: "fast", Fn: func() error { atomic.AddInt32(&finished, 1); return nil }},
		{Name: "hung", Fn: func() error {
			<-hang
			atomic.AddInt32(&finished, 1)
			return nil
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	RunCleanupParallel(ctx, steps)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("RunCleanupParallel did not honor ctx; took %v", elapsed)
	}
	if atomic.LoadInt32(&finished) != 1 {
		t.Fatalf("expected only fast step to finish before deadline, got %d", finished)
	}
}

func TestRunCleanupSequential_RunsAllStepsInOrder(t *testing.T) {
	var order []string
	steps := []CleanupStep{
		{Name: "one", Fn: func() error { order = append(order, "one"); return nil }},
		{Name: "two", Fn: func() error { order = append(order, "two"); return errors.New("x") }},
		{Name: "three", Fn: func() error { order = append(order, "three"); return nil }},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	RunCleanupSequential(ctx, steps)
	if len(order) != 3 || order[0] != "one" || order[1] != "two" || order[2] != "three" {
		t.Fatalf("unexpected step order: %v", order)
	}
}

func TestRunCleanupSequential_SkipsRemainingStepsAfterDeadline(t *testing.T) {
	var ran int32
	hang := make(chan struct{})
	defer close(hang)

	steps := []CleanupStep{
		{Name: "ok", Fn: func() error { atomic.AddInt32(&ran, 1); return nil }},
		{Name: "hung", Fn: func() error {
			<-hang
			atomic.AddInt32(&ran, 1)
			return nil
		}},
		{Name: "never", Fn: func() error {
			atomic.AddInt32(&ran, 1)
			return nil
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	RunCleanupSequential(ctx, steps)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("RunCleanupSequential did not honor ctx; took %v", elapsed)
	}
	if got := atomic.LoadInt32(&ran); got != 1 {
		t.Fatalf("expected only first step to complete, got %d", got)
	}
}

func TestRunCleanupParallel_EmptyStepsReturnsImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	RunCleanupParallel(ctx, nil)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("empty parallel took %v", elapsed)
	}
}

// TestCleanupFlow_RealisticHangScenario 模拟 production 里 provideCleanup 的真实
// 调用路径：先并行跑一批应用层 service.Stop()，再顺序关 Redis/Ent。我们故意让
// 其中一个 service.Stop() 永久阻塞（模拟 channel 写入永远没人读 / WaitGroup 永远
// 等不到的 bug），验证：
//  1. 整体 cleanup 不会超过 ctx 时限（修复前会 hang forever）
//  2. 没阻塞的 step 都跑完 + 顺序阶段不会因为前面 ctx 超时而完全跳过（这里我们
//     传新的 ctx 给顺序阶段，模拟 main.go 给 Shutdown 单独 ctx 的设计）
//
// 真实部署超时是 10s；测试里用 300ms 让 CI 跑得快。
func TestCleanupFlow_RealisticHangScenario(t *testing.T) {
	hang := make(chan struct{})
	defer close(hang)

	var parallelDone, sequentialDone int32

	parallelSteps := []CleanupStep{
		{Name: "EmailQueue.Stop", Fn: func() error {
			atomic.AddInt32(&parallelDone, 1)
			return nil
		}},
		{Name: "PricingService.Stop", Fn: func() error {
			atomic.AddInt32(&parallelDone, 1)
			return nil
		}},
		{Name: "BogusService.Stop", Fn: func() error {
			<-hang // simulate forever-blocked stop
			atomic.AddInt32(&parallelDone, 1)
			return nil
		}},
		{Name: "BackupService.Stop", Fn: func() error {
			atomic.AddInt32(&parallelDone, 1)
			return nil
		}},
	}

	infraSteps := []CleanupStep{
		{Name: "Redis.Close", Fn: func() error {
			atomic.AddInt32(&sequentialDone, 1)
			return nil
		}},
		{Name: "Ent.Close", Fn: func() error {
			atomic.AddInt32(&sequentialDone, 1)
			return nil
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	RunCleanupParallel(ctx, parallelSteps)
	// 顺序阶段在生产里共用同一个 ctx；如果 ctx 已过期，infraSteps 应整体跳过。
	RunCleanupSequential(ctx, infraSteps)
	elapsed := time.Since(start)

	// 修复前：会无限挂；修复后必须在 ctx deadline 附近返回。给 500ms 余量。
	if elapsed > 800*time.Millisecond {
		t.Fatalf("cleanup did not honor deadline; took %v", elapsed)
	}
	// 3 个非阻塞 step 应该都跑完
	if got := atomic.LoadInt32(&parallelDone); got != 3 {
		t.Fatalf("expected 3 non-blocking parallel steps to finish, got %d", got)
	}
	// ctx 已过期，顺序阶段一上来就 abort，infra step 一个都不会跑
	if got := atomic.LoadInt32(&sequentialDone); got != 0 {
		t.Fatalf("expected sequential steps to be skipped after parallel deadline, got %d", got)
	}
}
