package gateway

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func standaloneTestDirectory(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "openbsd", "netbsd", "windows":
	default:
		t.Skip("standalone state locking is unsupported on this platform")
	}
	dir := filepath.Join(t.TempDir(), "task-state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStandaloneStateReopenPreservesKeyAndFiles(t *testing.T) {
	dir := filepath.Join(standaloneTestDirectory(t), "nested", "state")
	first, err := OpenStandaloneState(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	key := first.Key()
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != 32 || len(key) != 64 {
		t.Fatal("expected a 32-byte hexadecimal key")
	}
	for name, mode := range map[string]os.FileMode{"": 0700, "task.key": 0600, "owner.lock": 0600} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !standalonePrivateMode(info, mode) {
			t.Fatalf("unexpected file permissions for %q", name)
		}
	}
	first.Close()
	first.Close()
	second, err := OpenStandaloneState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Key() != key {
		t.Fatal("reopening replaced the task key")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "task.key"))
	if err != nil || string(raw) != key {
		t.Fatal("key file differs from the loaded task namespace")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatal("expected only task.key and owner.lock, with no temporary files")
	}
}

func TestStandaloneStateExclusiveOwnership(t *testing.T) {
	dir := standaloneTestDirectory(t)
	owner, err := OpenStandaloneState(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if other, err := OpenStandaloneState(dir); err == nil {
		other.Close()
		t.Fatal("second owner acquired a live task namespace")
	}
	owner.Close()
	again, err := OpenStandaloneState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Key() != owner.Key() {
		t.Fatal("lock contention changed the persistent task key")
	}
}

func TestStandaloneStateRejectsUnsafeDirectory(t *testing.T) {
	if _, err := OpenStandaloneState(""); err == nil {
		t.Fatal("empty state path accepted")
	}
	t.Run("regular-file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "state")
		if err := os.WriteFile(file, []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenStandaloneState(file); err == nil {
			t.Fatal("regular file accepted as a directory")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := standaloneTestDirectory(t)
		link := filepath.Join(t.TempDir(), "state-link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := OpenStandaloneState(link + string(os.PathSeparator)); err == nil {
			t.Fatal("symlink directory accepted")
		}
		entries, err := os.ReadDir(target)
		if err != nil || len(entries) != 0 {
			t.Fatal("symlink target was modified")
		}
	})
	t.Run("permissions", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows modes do not describe ACL permissions")
		}
		dir := standaloneTestDirectory(t)
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenStandaloneState(dir); err == nil {
			t.Fatal("public state directory accepted")
		}
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatal("existing directory permissions were changed")
		}
	})
}

func TestStandaloneStateRejectsInvalidKeyWithoutOverwriting(t *testing.T) {
	cases := map[string]string{
		"empty": "", "short": "private-sentinel-invalid-key", "not-hex": strings.Repeat("x", 64),
		"newline": strings.Repeat("a", 64) + "\n", "oversized": strings.Repeat("a", 65),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			dir := standaloneTestDirectory(t)
			path := filepath.Join(dir, "task.key")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenStandaloneState(dir); err == nil {
				t.Fatal("invalid key accepted")
			} else if strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("error exposed key contents")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != raw {
				t.Fatal("invalid key was overwritten")
			}
			// A failed open must release ownership so explicit repair can work.
			if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)), 0600); err != nil {
				t.Fatal(err)
			}
			repaired, err := OpenStandaloneState(dir)
			if err != nil {
				t.Fatal(err)
			}
			repaired.Close()
		})
	}
}

func TestStandaloneStateRejectsUnsafeKeyAndOwnerFiles(t *testing.T) {
	for _, filename := range []string{"task.key", "owner.lock"} {
		t.Run(filename, func(t *testing.T) {
			t.Run("symlink", func(t *testing.T) {
				dir := standaloneTestDirectory(t)
				target := filepath.Join(t.TempDir(), "target")
				original := strings.Repeat("a", 64)
				if err := os.WriteFile(target, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, filename)); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				if _, err := OpenStandaloneState(dir); err == nil {
					t.Fatal("symlink file accepted")
				}
				after, err := os.ReadFile(target)
				if err != nil || string(after) != original {
					t.Fatal("symlink target was modified")
				}
			})
			t.Run("directory", func(t *testing.T) {
				dir := standaloneTestDirectory(t)
				if err := os.Mkdir(filepath.Join(dir, filename), 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := OpenStandaloneState(dir); err == nil {
					t.Fatal("non-regular file accepted")
				}
			})
			t.Run("permissions", func(t *testing.T) {
				if runtime.GOOS == "windows" {
					t.Skip("Windows modes do not describe ACL permissions")
				}
				dir := standaloneTestDirectory(t)
				path := filepath.Join(dir, filename)
				if err := os.WriteFile(path, []byte(strings.Repeat("b", 64)), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
				if _, err := OpenStandaloneState(dir); err == nil {
					t.Fatal("public file accepted")
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0644 {
					t.Fatal("existing file permissions were changed")
				}
			})
		})
	}
}

func TestStandaloneStateMissingPriorKeyDoesNotRegenerate(t *testing.T) {
	dir := standaloneTestDirectory(t)
	state, err := OpenStandaloneState(dir)
	if err != nil {
		t.Fatal(err)
	}
	state.Close()
	path := filepath.Join(dir, "task.key")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStandaloneState(dir); err == nil {
		t.Fatal("missing original key was regenerated")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("missing key was recreated")
	}
}
