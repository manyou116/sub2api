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
	if got := atomic.LoadInt32(&done); got != 3 {
		t.Fatalf("expected all 3 steps to run, got %d", got)
	}
}

func TestRunCleanupParallel_AbandonsHungStepsAfterDeadline(t *testing.T) {
	var finished int32
	hang := make(chan struct{})
	defer close(hang)

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
	if got := atomic.LoadInt32(&finished); got != 1 {
		t.Fatalf("expected only fast step to finish before deadline, got %d", got)
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
		t.Fatalf("empty parallel cleanup took %v", elapsed)
	}
}
