//go:build !windows

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOpenCreatesOwnerOnlyStorageUnderPermissiveUmask(t *testing.T) {
	oldUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	homeDir := filepath.Join(t.TempDir(), ".loopcoder")
	path := filepath.Join(homeDir, "data", "loopcoder.db")
	store, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	assertExactMode(t, homeDir, 0o700)
	assertExactMode(t, filepath.Dir(path), 0o700)
	assertExactMode(t, path, 0o600)
}

func TestRepairPermissionsTightensExistingDatabaseAndSidecars(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), ".loopcoder")
	dataDir := filepath.Join(homeDir, "data")
	path := filepath.Join(dataDir, "loopcoder.db")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(file, []byte("fixture"), 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}

	report, err := RepairPermissions(path)
	if err != nil {
		t.Fatalf("RepairPermissions returned error: %v", err)
	}
	if !report.Secure || !report.Repaired {
		t.Fatalf("report = %#v, want repaired secure", report)
	}
	assertExactMode(t, homeDir, 0o700)
	assertExactMode(t, dataDir, 0o700)
	assertExactMode(t, path, 0o600)
	assertExactMode(t, path+"-wal", 0o600)
	assertExactMode(t, path+"-shm", 0o600)
}

func TestRepairPermissionsDoesNotBroadenStricterModes(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), ".loopcoder")
	dataDir := filepath.Join(homeDir, "data")
	path := filepath.Join(dataDir, "loopcoder.db")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		if err := os.Chmod(file, 0o400); err != nil {
			t.Fatalf("chmod %s: %v", file, err)
		}
	}
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatalf("chmod data dir: %v", err)
	}
	if err := os.Chmod(homeDir, 0o500); err != nil {
		t.Fatalf("chmod home dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dataDir, 0o700)
		_ = os.Chmod(homeDir, 0o700)
	})

	report, err := RepairPermissions(path)
	if err != nil {
		t.Fatalf("RepairPermissions returned error: %v", err)
	}
	if !report.Secure || report.Repaired {
		t.Fatalf("report = %#v, want secure without repair", report)
	}
	assertExactMode(t, homeDir, 0o500)
	assertExactMode(t, dataDir, 0o500)
	assertExactMode(t, path, 0o400)
	assertExactMode(t, path+"-wal", 0o400)
	assertExactMode(t, path+"-shm", 0o400)
}

func TestRepairPermissionsFailsSafelyForSymlinkAndNonRegularPaths(t *testing.T) {
	t.Run("data directory symlink", func(t *testing.T) {
		homeDir := filepath.Join(t.TempDir(), ".loopcoder")
		targetDir := filepath.Join(t.TempDir(), "target")
		if err := os.MkdirAll(homeDir, 0o700); err != nil {
			t.Fatalf("create home dir: %v", err)
		}
		if err := os.MkdirAll(targetDir, 0o700); err != nil {
			t.Fatalf("create target dir: %v", err)
		}
		link := filepath.Join(homeDir, "data")
		if err := os.Symlink(targetDir, link); err != nil {
			t.Fatalf("create symlink: %v", err)
		}

		_, err := RepairPermissions(filepath.Join(link, "loopcoder.db"))
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("RepairPermissions error = %v, want symlink failure", err)
		}
	})

	t.Run("database path is directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ".loopcoder", "data", "loopcoder.db")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create db directory fixture: %v", err)
		}

		_, err := RepairPermissions(path)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("RepairPermissions error = %v, want non-regular failure", err)
		}
	})
}

func TestOpenRejectsDatabaseSymlink(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".loopcoder", "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.db")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	path := filepath.Join(dataDir, "loopcoder.db")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	_, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Open error = %v, want symlink failure", err)
	}
}

func TestCheckHealthRejectsUnsafeSymlinkBeforeSQLiteOpen(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".loopcoder", "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.db")
	if err := os.WriteFile(target, []byte("not sqlite"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	path := filepath.Join(dataDir, "loopcoder.db")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	_, err := CheckHealth(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "unsafe storage path") || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("CheckHealth error = %v, want unsafe symlink failure", err)
	}
}

func assertExactMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %04o, want %04o", path, got, want)
	}
}
