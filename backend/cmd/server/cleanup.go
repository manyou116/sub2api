package main

import (
	"context"
	"log"
	"sync"
)

// CleanupStep represents a single shutdown task.
type CleanupStep struct {
	Name string
	Fn   func() error
}

// RunCleanupParallel launches all steps concurrently and waits until either
// every step finishes or ctx is canceled. If ctx expires, the goroutines for
// hung steps are abandoned (they keep running until the process exits) so that
// the shutdown sequence can make forward progress.
func RunCleanupParallel(ctx context.Context, steps []CleanupStep) {
	if len(steps) == 0 {
		return
	}
	var wg sync.WaitGroup
	for i := range steps {
		step := steps[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := step.Fn(); err != nil {
				log.Printf("[Cleanup] %s failed: %v", step.Name, err)
				return
			}
			log.Printf("[Cleanup] %s succeeded", step.Name)
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("[Cleanup] runParallel exceeded deadline; abandoning hung steps")
	}
}

// RunCleanupSequential executes steps one-by-one. Before running a step it
// checks ctx; if a step blocks past the deadline the remaining steps are
// skipped.
func RunCleanupSequential(ctx context.Context, steps []CleanupStep) {
	for i := range steps {
		select {
		case <-ctx.Done():
			log.Printf("[Cleanup] runSequential aborted before %s: deadline exceeded", steps[i].Name)
			return
		default:
		}
		step := steps[i]
		stepDone := make(chan error, 1)
		go func() {
			stepDone <- step.Fn()
		}()
		select {
		case err := <-stepDone:
			if err != nil {
				log.Printf("[Cleanup] %s failed: %v", step.Name, err)
				continue
			}
			log.Printf("[Cleanup] %s succeeded", step.Name)
		case <-ctx.Done():
			log.Printf("[Cleanup] %s timed out; abandoning", step.Name)
			return
		}
	}
}
