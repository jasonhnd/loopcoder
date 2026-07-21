//go:build darwin

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDarwinArm64ConformanceBaseline exercises the v0.9 store foundation on the
// supported product platform: open, reopen, integrity, migration ledger, and
// idempotent close without host-identifying diagnostics.
func TestDarwinArm64ConformanceBaseline(t *testing.T) {
	if !SupportedPlatform() {
		t.Skipf("conformance baseline requires darwin/arm64; got %s", PlatformTuple())
	}

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "conformance.db")

	first, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.CheckIntegrity(ctx); err != nil {
		_ = first.Close()
		t.Fatalf("CheckIntegrity after open: %v", err)
	}
	meta, err := first.Metadata(ctx)
	if err != nil {
		_ = first.Close()
		t.Fatalf("Metadata: %v", err)
	}
	if meta.StoreID == "" || meta.SchemaVersion != CurrentSchemaVersion {
		_ = first.Close()
		t.Fatalf("unexpected metadata: %#v", meta)
	}
	var ledgerCount int
	if err := first.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&ledgerCount); err != nil {
		_ = first.Close()
		t.Fatalf("ledger count: %v", err)
	}
	if ledgerCount != 1 {
		_ = first.Close()
		t.Fatalf("ledger count = %d, want 1", ledgerCount)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}

	second, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if err := second.CheckIntegrity(ctx); err != nil {
		t.Fatalf("CheckIntegrity after reopen: %v", err)
	}
	meta2, err := second.Metadata(ctx)
	if err != nil {
		t.Fatalf("Metadata after reopen: %v", err)
	}
	if meta2.StoreID != meta.StoreID {
		t.Fatalf("store id changed on reopen: %q -> %q", meta.StoreID, meta2.StoreID)
	}
	if err := second.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&ledgerCount); err != nil {
		t.Fatalf("ledger count after reopen: %v", err)
	}
	if ledgerCount != 1 {
		t.Fatalf("ledger count after reopen = %d, want 1", ledgerCount)
	}

	// Owner-only modes on the supported path.
	assertExactMode(t, filepath.Dir(path), 0o700)
	assertExactMode(t, path, 0o600)
}

func TestOpenRejectsNonArm64DarwinAtRuntimeContract(t *testing.T) {
	// Document the runtime contract: only arm64 is accepted even when GOOS is
	// darwin. On the product CI host this is a no-op assertion of the helper.
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if err := requireSupportedPlatform(); err != nil {
			t.Fatalf("supported host rejected: %v", err)
		}
		return
	}
	if err := requireSupportedPlatform(); err == nil {
		t.Fatal("expected unsupported platform error")
	} else if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("error = %v, want ErrUnsupportedPlatform", err)
	}
}

func TestOpenFailsClosedOnUnsafeSymlinkPaths(t *testing.T) {
	if !SupportedPlatform() {
		t.Skipf("requires darwin/arm64; got %s", PlatformTuple())
	}
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := Open(context.Background(), Options{Path: filepath.Join(linkDir, "store.db"), Now: fixedNow})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Open error = %v, want ancestor symlink failure", err)
	}
}
