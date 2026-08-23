package webdriver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPollLockKeyUsesWholeToken(t *testing.T) {
	const sharedPrefix = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9."
	first := pollLockKey(Auth{AccessToken: sharedPrefix + "account-one"})
	second := pollLockKey(Auth{AccessToken: sharedPrefix + "account-two"})
	if first == second {
		t.Fatal("tokens with a shared JWT prefix must not share a poll lock")
	}
	if len(first) != 64 {
		t.Fatalf("poll lock key length = %d, want SHA-256 hex length 64", len(first))
	}
}

func TestWithAccountPollLockHonorsContextCancellation(t *testing.T) {
	key := "cancel-" + t.Name()
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- withAccountPollLock(context.Background(), key, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := withAccountPollLock(ctx, key, func() error {
		t.Fatal("cancelled waiter must not acquire the poll lock")
		return nil
	})
	var driverErr *Error
	if !errors.As(err, &driverErr) || driverErr.Kind != ErrorKindTimeout {
		t.Fatalf("error = %v, want timeout driver error", err)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("lock holder returned %v", err)
	}
}

func TestWithAccountPollLockReleasesAfterCompletion(t *testing.T) {
	key := "release-" + t.Name()
	if err := withAccountPollLock(context.Background(), key, func() error { return nil }); err != nil {
		t.Fatalf("first acquisition: %v", err)
	}
	if err := withAccountPollLock(context.Background(), key, func() error { return nil }); err != nil {
		t.Fatalf("second acquisition after release: %v", err)
	}
	accountPollLocks.Lock()
	_, stillPresent := accountPollLocks.locks[key]
	accountPollLocks.Unlock()
	if stillPresent {
		t.Fatal("idle poll lock must be removed after its final user exits")
	}
}
