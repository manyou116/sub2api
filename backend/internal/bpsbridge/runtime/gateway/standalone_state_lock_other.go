//go:build !windows && !darwin && !linux && !freebsd && !openbsd && !netbsd

package gateway

import (
	"errors"
	"os"
)

func lockStandaloneState(path string) (*os.File, error) {
	return nil, errors.New("standalone task state requires a supported file lock")
}
