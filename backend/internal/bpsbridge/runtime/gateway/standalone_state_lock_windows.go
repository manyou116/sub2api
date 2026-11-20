//go:build windows

package gateway

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var standaloneLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

// lockStandaloneState keeps the first byte locked until the returned file is
// closed. LockFileEx permits a range past EOF, so owner.lock can remain empty.
func lockStandaloneState(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("standalone state owner lock cannot be opened")
	}
	if err := standaloneLockFileEx.Find(); err != nil {
		_ = f.Close()
		return nil, errors.New("standalone state owner lock is unavailable")
	}
	const lockExclusive = 0x00000002
	const lockFailImmediately = 0x00000001
	var overlapped syscall.Overlapped
	locked, _, callErr := standaloneLockFileEx.Call(
		f.Fd(), lockExclusive|lockFailImmediately, 0, 1, 0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	runtime.KeepAlive(&overlapped)
	if locked == 0 {
		_ = f.Close()
		if callErr == syscall.Errno(33) { // ERROR_LOCK_VIOLATION
			return nil, errors.New("standalone state is owned by another process")
		}
		return nil, errors.New("standalone state owner lock could not be acquired")
	}
	return f, nil
}
