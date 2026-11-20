//go:build windows

package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStandaloneWindowsOwnerLockExclusiveAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	first, err := lockStandaloneState(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := lockStandaloneState(path)
	if second != nil {
		_ = second.Close()
		t.Fatal("a second owner obtained the same state lock")
	}
	if err == nil {
		t.Fatal("lock contention did not return an error")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := lockStandaloneState(path)
	if err != nil {
		t.Fatalf("closed owner lock was not released: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("closing the lock must retain its file", err)
	}
}

func TestStandaloneWindowsOwnerLockOpenError(t *testing.T) {
	f, err := lockStandaloneState(filepath.Join(t.TempDir(), "missing", "owner.lock"))
	if f != nil {
		_ = f.Close()
		t.Fatal("invalid owner path returned an open file")
	}
	if err == nil || err.Error() != "standalone state owner lock cannot be opened" {
		t.Fatalf("expected a safe open error, got %v", err)
	}
}
