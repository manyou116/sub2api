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

func TestPollGateSharesSpacingWithoutHoldingConversationLock(t *testing.T) {
	key := "spacing-" + t.Name()
	started := time.Now()
	if err := withAccountPollGate(context.Background(), key, func(lock *accountPollLock) error {
		lock.nextAllowed.Store(time.Now().Add(40 * time.Millisecond).UnixNano())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// First conversation is still generating, but its HTTP call already released
	// the gate. Another conversation can poll after the account-wide minimum gap.
	if err := withAccountPollGate(context.Background(), key, func(*accountPollLock) error {
		if time.Since(started) < 35*time.Millisecond {
			t.Fatal("shared poll spacing was bypassed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	accountPollLocks.Lock()
	_, retained := accountPollLocks.locks[key]
	accountPollLocks.Unlock()
	if retained {
		t.Fatal("expired idle gate should be removed")
	}
}

func TestPollGateWaitHonorsDeadline(t *testing.T) {
	key := "deadline-" + t.Name()
	if err := withAccountPollGate(context.Background(), key, func(lock *accountPollLock) error {
		lock.nextAllowed.Store(time.Now().Add(50 * time.Millisecond).UnixNano())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err := withAccountPollGate(ctx, key, func(*accountPollLock) error { t.Fatal("expired context must not make HTTP request"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestPollGateKeepsIdentityAcrossTokenRefresh(t *testing.T) {
	if pollLockKey(Auth{AccountID: "parent", AccessToken: "old"}) != pollLockKey(Auth{AccountID: "parent", AccessToken: "new"}) {
		t.Fatal("refresh must not bypass the parent's poll spacing")
	}
}
