//go:build darwin

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenReadOnlyRejectsBroadPermissionsWithoutRepair(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	writable, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open writable seed: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("Close writable seed: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Broaden fixture mode: %v", err)
	}

	readOnly, err := OpenReadOnly(ctx, Options{Path: path, Now: fixedNow})
	if readOnly != nil {
		_ = readOnly.Close()
		t.Fatal("OpenReadOnly accepted insecure database permissions")
	}
	if err == nil || !strings.Contains(err.Error(), "insecure database file") {
		t.Fatalf("OpenReadOnly error = %v, want insecure permission failure", err)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		t.Fatalf("Stat database after rejected open: %v", statErr)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("database mode after rejected open = %04o, want unchanged 0644", got)
	}
}
