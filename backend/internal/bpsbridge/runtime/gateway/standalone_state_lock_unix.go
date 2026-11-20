//go:build darwin || linux || freebsd || openbsd || netbsd

package gateway

import (
	"errors"
	"os"
	"syscall"
)

func lockStandaloneState(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("standalone task state is unavailable or already in use")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("standalone task state is unavailable or already in use")
	}
	return f, nil
}
