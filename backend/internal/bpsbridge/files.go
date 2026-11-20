package bpsbridge

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type BoundGrant struct {
	Grant
	Identity  Identity  `json:"identity"`
	ExpiresAt time.Time `json:"expires_at"`
}

func ReadGrantFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		return nil, ErrInvalidGrant
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrInvalidGrant
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, MaxGrantBytes+1))
	if err != nil || len(raw) > MaxGrantBytes {
		return nil, ErrInvalidGrant
	}
	return raw, nil
}

func Save(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".bps-")
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = os.Remove(f.Name()) }()
	err = json.NewEncoder(f).Encode(value)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil || os.Rename(f.Name(), path) != nil {
		return ErrUnavailable
	}
	return nil
}
