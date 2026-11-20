package webdriver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Share spacing across conversations without locking their entire poll lifetimes.
// This is process-local; ordinary account concurrency remains in the shared store.
var accountPollLocks = struct {
	sync.Mutex
	locks map[string]*accountPollLock
}{locks: make(map[string]*accountPollLock)}

type accountPollLock struct {
	semaphore   chan struct{}
	references  int
	nextAllowed atomic.Int64
}

func pollLockKey(auth Auth) string {
	key := strings.TrimSpace(auth.AccountID)
	if key == "" {
		key = strings.TrimSpace(auth.AccessToken)
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func withAccountPollLock(ctx context.Context, key string, fn func() error) error {
	return withAccountPollGate(ctx, key, func(*accountPollLock) error { return fn() })
}

func withAccountPollGate(ctx context.Context, key string, fn func(*accountPollLock) error) error {
	accountPollLocks.Lock()
	lock := accountPollLocks.locks[key]
	if lock == nil {
		lock = &accountPollLock{semaphore: make(chan struct{}, 1)}
		accountPollLocks.locks[key] = lock
	}
	lock.references++
	accountPollLocks.Unlock()
	defer func() {
		accountPollLocks.Lock()
		lock.references--
		if lock.references == 0 {
			if wait := time.Until(time.Unix(0, lock.nextAllowed.Load())); wait > 0 {
				time.AfterFunc(wait, func() {
					accountPollLocks.Lock()
					defer accountPollLocks.Unlock()
					if lock.references == 0 && time.Now().UnixNano() >= lock.nextAllowed.Load() && accountPollLocks.locks[key] == lock {
						delete(accountPollLocks.locks, key)
					}
				})
			} else {
				delete(accountPollLocks.locks, key)
			}
		}
		accountPollLocks.Unlock()
	}()
	select {
	case lock.semaphore <- struct{}{}:
		defer func() { <-lock.semaphore }()
		if err := waitForRetry(ctx, max(0, time.Until(time.Unix(0, lock.nextAllowed.Load())))); err != nil {
			return transportError("poll", err)
		}
		if err := ctx.Err(); err != nil {
			return transportError("poll", err)
		}
		return fn(lock)
	case <-ctx.Done():
		err := NewError(ErrorKindTimeout, "poll", ctx.Err().Error(), 0, true)
		err.Cause = ctx.Err()
		return err
	}
}
