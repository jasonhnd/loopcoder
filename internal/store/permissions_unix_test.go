//go:build !windows

package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOpenCreatesOwnerOnlyPathsUnderPermissiveUmask(t *testing.T) {
	oldUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	dataDir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dataDir, "loopcoder-store.db")
	store, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	assertExactMode(t, dataDir, 0o700)
	assertExactMode(t, path, 0o600)
}

func TestOpenRejectsDatabaseSymlink(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.db")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	path := filepath.Join(dataDir, "loopcoder-store.db")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Open error = %v, want symlink failure", err)
	}
}

func TestIntegrityFailsClosedOnInsecurePermissions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "loopcoder-store.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod database: %v", err)
	}
	err = store.CheckIntegrity(ctx)
	if err == nil || !strings.Contains(err.Error(), "insecure") {
		t.Fatalf("CheckIntegrity error = %v, want insecure permission failure", err)
	}
}

func TestCheckPermissionsReportsOwnerOnlyAfterOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "loopcoder-store.db")
	store, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	report, err := CheckPermissions(path)
	if err != nil {
		t.Fatalf("CheckPermissions: %v", err)
	}
	if !report.Supported || !report.Secure {
		t.Fatalf("report = %#v, want supported secure", report)
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
