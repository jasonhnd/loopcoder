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
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	root := t.TempDir()
	path := filepath.Join(root, ".loopcoder", "data", "loopcoder.db")
	store, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	assertModeNoBroaderThan(t, filepath.Join(root, ".loopcoder"), 0o700)
	assertModeNoBroaderThan(t, filepath.Join(root, ".loopcoder", "data"), 0o700)
	assertModeNoBroaderThan(t, path, 0o600)
}

func TestOpenTightensExistingDatabaseAndSidecars(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), ".loopcoder")
	dataDir := filepath.Join(homeDir, "data")
	path := filepath.Join(dataDir, "loopcoder.db")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(file, []byte{}, 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}

	store, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	assertExactMode(t, homeDir, 0o700)
	assertExactMode(t, dataDir, 0o700)
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		assertExactMode(t, file, 0o600)
	}
}

func TestRepairPermissionsDoesNotBroadenStricterModes(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), ".loopcoder")
	dataDir := filepath.Join(homeDir, "data")
	path := filepath.Join(dataDir, "loopcoder.db")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chmod(homeDir, 0o500); err != nil {
		t.Fatalf("chmod home: %v", err)
	}
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatalf("chmod data: %v", err)
	}
	if err := os.WriteFile(path, []byte{}, 0o400); err != nil {
		t.Fatalf("write db: %v", err)
	}

	report, err := RepairPermissions(path)
	if err != nil {
		t.Fatalf("RepairPermissions returned error: %v", err)
	}
	if !report.OK || report.Changed {
		t.Fatalf("report = %#v, want ok unchanged", report)
	}
	assertExactMode(t, homeDir, 0o500)
	assertExactMode(t, dataDir, 0o500)
	assertExactMode(t, path, 0o400)
}

func TestCheckAndRepairPermissionsForExistingInsecureInstall(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), ".loopcoder")
	dataDir := filepath.Join(homeDir, "data")
	path := filepath.Join(dataDir, "loopcoder.db")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("write db: %v", err)
	}

	report, err := CheckPermissions(path)
	if err != nil {
		t.Fatalf("CheckPermissions returned error: %v", err)
	}
	if report.OK || !report.Repairable || len(report.Issues) != 3 {
		t.Fatalf("report = %#v, want three repairable issues", report)
	}

	repaired, err := RepairPermissions(path)
	if err != nil {
		t.Fatalf("RepairPermissions returned error: %v", err)
	}
	if !repaired.OK || !repaired.Changed || len(repaired.Repairs) != 3 {
		t.Fatalf("repaired = %#v, want changed ok with three repairs", repaired)
	}
	assertExactMode(t, homeDir, 0o700)
	assertExactMode(t, dataDir, 0o700)
	assertExactMode(t, path, 0o600)
}

func TestOpenFailsSafelyForSymlinkDatabase(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	if err := os.WriteFile(target, []byte{}, 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "loopcoder.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := Open(context.Background(), Options{Path: link, Now: fixedNow})
	if err == nil {
		t.Fatal("Open returned nil error, want symlink failure")
	}
	if !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("error = %q, want symlink message", err.Error())
	}
	assertExactMode(t, target, 0o644)
}

func TestOpenFailsSafelyForNonRegularDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loopcoder.db")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir db path: %v", err)
	}

	_, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err == nil {
		t.Fatal("Open returned nil error, want non-regular failure")
	}
	if !strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("error = %q, want non-regular message", err.Error())
	}
}

func TestCheckHealthRejectsSymlinkDatabase(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	if err := os.WriteFile(target, []byte{}, 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "loopcoder.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := CheckHealth(context.Background(), link)
	if err == nil {
		t.Fatal("CheckHealth returned nil error, want symlink failure")
	}
	if !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("error = %q, want symlink message", err.Error())
	}
}

func TestCheckHealthRejectsUnsafeSidecarPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loopcoder.db")
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	target := filepath.Join(dir, "target.wal")
	if err := os.WriteFile(target, []byte{}, 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, path+"-wal"); err != nil {
		t.Fatalf("symlink wal: %v", err)
	}

	_, err := CheckHealth(context.Background(), path)
	if err == nil {
		t.Fatal("CheckHealth returned nil error, want sidecar symlink failure")
	}
	if !strings.Contains(err.Error(), "SQLite WAL sidecar") || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("error = %q, want WAL sidecar symlink message", err.Error())
	}
	assertExactMode(t, target, 0o644)
}

func assertModeNoBroaderThan(t *testing.T, path string, max os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm()&^max != 0 {
		t.Fatalf("%s mode = %04o, want no broader than %04o", path, info.Mode().Perm(), max)
	}
}

func assertExactMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
