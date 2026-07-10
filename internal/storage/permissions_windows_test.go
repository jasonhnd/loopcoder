//go:build windows

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPermissionsReportDocumentsACLCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "loopcoder.db")
	report, err := CheckPermissions(path)
	if err != nil {
		t.Fatalf("CheckPermissions returned error: %v", err)
	}
	if !report.Unsupported || report.OK {
		t.Fatalf("report = %#v, want unsupported non-ok", report)
	}
	if !strings.Contains(report.Message, "owner-only ACL hardening is not implemented on Windows") {
		t.Fatalf("message = %q", report.Message)
	}

	repaired, err := RepairPermissions(path)
	if err != nil {
		t.Fatalf("RepairPermissions returned error: %v", err)
	}
	if !repaired.Unsupported || repaired.Changed {
		t.Fatalf("repaired = %#v, want unsupported unchanged", repaired)
	}
}

func TestWindowsOpenCreatesDatabaseAndRejectsNonRegularPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "loopcoder.db")
	store, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("database info = %#v err=%v, want regular file", info, err)
	}

	dirPath := filepath.Join(t.TempDir(), "loopcoder.db")
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatalf("mkdir db path: %v", err)
	}
	_, err = Open(context.Background(), Options{Path: dirPath, Now: fixedNow})
	if err == nil {
		t.Fatal("Open returned nil error, want non-regular path failure")
	}
	if !strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("error = %q, want non-regular message", err.Error())
	}
}

func TestWindowsCheckHealthRejectsNonRegularSidecarPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loopcoder.db")
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	if err := os.Mkdir(path+"-wal", 0o755); err != nil {
		t.Fatalf("mkdir wal sidecar: %v", err)
	}

	_, err := CheckHealth(context.Background(), path)
	if err == nil {
		t.Fatal("CheckHealth returned nil error, want sidecar failure")
	}
	if !strings.Contains(err.Error(), "SQLite WAL sidecar") || !strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("error = %q, want WAL sidecar non-regular message", err.Error())
	}
}
