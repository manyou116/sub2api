package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// StandaloneState owns the persistent namespace for standalone task slots. It
// contains no OAuth credentials, response history, or refresh state.
type StandaloneState struct {
	key  string
	lock *os.File
	once sync.Once
}

// OpenStandaloneState opens a private directory and exclusively owns it until
// Close. Existing keys are validated, never replaced or regenerated. On Windows,
// Go's permission bits do not describe ACLs; the directory inherits its parent's
// ACL, while regular-file, symlink, key-content and exclusive-lock checks apply.
func OpenStandaloneState(dir string) (*StandaloneState, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("standalone task state directory is required")
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("standalone task state directory is unavailable")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || !standalonePrivateMode(info, 0700) {
		return nil, errors.New("standalone task state directory must be private (0700) and not a symlink")
	}
	lockPath := filepath.Join(dir, "owner.lock")
	lockInfo, err := os.Lstat(lockPath)
	ownerExisted := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("standalone task state owner file is unavailable")
	}
	if ownerExisted && (!lockInfo.Mode().IsRegular() || !standalonePrivateMode(lockInfo, 0600)) {
		return nil, errors.New("standalone task state owner file must be private (0600), regular, and not a symlink")
	}
	lock, err := lockStandaloneState(lockPath)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = lock.Close()
		}
	}()
	opened, err := lock.Stat()
	current, statErr := os.Lstat(lockPath)
	if err != nil || statErr != nil || !current.Mode().IsRegular() || !standalonePrivateMode(current, 0600) || !os.SameFile(opened, current) {
		return nil, errors.New("standalone task state owner file changed or is not private")
	}
	keyPath := filepath.Join(dir, "task.key")
	key, err := readStandaloneTaskKey(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		// An owner file left by an earlier installation must not silently create
		// a new namespace after its key is lost. Restore its original task.key.
		if ownerExisted {
			return nil, errors.New("standalone task key is missing from existing state; restore the original task.key")
		}
		key, err = createStandaloneTaskKey(dir, keyPath)
	}
	if err != nil {
		return nil, err
	}
	keep = true
	return &StandaloneState{key: key, lock: lock}, nil
}

// Key returns the stable task namespace key. It must not be logged or returned
// by the public HTTP API.
func (s *StandaloneState) Key() string {
	if s == nil {
		return ""
	}
	return s.key
}

// Close releases ownership without deleting the key or owner file. Call only
// after all handlers holding task-slot leases have stopped.
func (s *StandaloneState) Close() {
	if s != nil {
		s.once.Do(func() {
			if s.lock != nil {
				_ = s.lock.Close()
			}
		})
	}
}

func standalonePrivateMode(info os.FileInfo, mode os.FileMode) bool {
	return runtime.GOOS == "windows" || info.Mode().Perm() == mode
}

func readStandaloneTaskKey(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", os.ErrNotExist
	}
	if err != nil {
		return "", errors.New("standalone task key is unreadable")
	}
	if !info.Mode().IsRegular() || !standalonePrivateMode(info, 0600) {
		return "", errors.New("standalone task key must be private (0600), regular, and not a symlink")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("standalone task key is unreadable")
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("standalone task key changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65))
	current, statErr := os.Lstat(path)
	if err != nil || statErr != nil || !current.Mode().IsRegular() || !standalonePrivateMode(current, 0600) || !os.SameFile(opened, current) {
		return "", errors.New("standalone task key is unreadable or changed while reading")
	}
	if len(raw) != 64 {
		return "", errors.New("standalone task key must contain exactly 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(string(raw)); err != nil {
		return "", errors.New("standalone task key must contain exactly 64 hexadecimal characters")
	}
	return string(raw), nil
}

func createStandaloneTaskKey(dir, path string) (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", errors.New("standalone task key generation failed")
	}
	key := hex.EncodeToString(secret[:])
	f, err := os.CreateTemp(dir, ".task-key-*")
	if err != nil {
		return "", errors.New("standalone task key creation failed")
	}
	temporary := f.Name()
	defer func() { _ = os.Remove(temporary) }()
	if err = f.Chmod(0600); err == nil {
		_, err = io.WriteString(f, key)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return "", errors.New("standalone task key creation failed")
	}
	// Linking publishes a fully written key and refuses to overwrite an existing
	// name, including one created outside the cooperating owner lock.
	if err = os.Link(temporary, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readStandaloneTaskKey(path)
		}
		return "", errors.New("standalone task key publication failed")
	}
	if runtime.GOOS != "windows" {
		directory, err := os.Open(dir)
		if err != nil {
			return "", errors.New("standalone task state sync failed")
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil || closeErr != nil {
			return "", errors.New("standalone task state sync failed")
		}
	}
	return key, nil
}
