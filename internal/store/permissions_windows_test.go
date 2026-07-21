//go:build windows

package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOpenCreatesOwnerOnlyACLPaths(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dataDir, "loopcoder-store.db")

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	userSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	for _, target := range []string{dataDir, path} {
		if err := requireCurrentUserOwner(target, userSID); err != nil {
			t.Fatalf("owner check %s: %v", target, err)
		}
		secure, detail, err := ownerOnlyACLState(target, userSID)
		if err != nil {
			t.Fatalf("ownerOnlyACLState %s: %v", target, err)
		}
		if !secure {
			t.Fatalf("path %s not owner-only: %s", target, detail)
		}
	}
	if err := store.CheckIntegrity(ctx); err != nil {
		t.Fatalf("CheckIntegrity: %v", err)
	}
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
	if !strings.Contains(report.Message, "owner-only") {
		t.Fatalf("report.Message = %q, want owner-only", report.Message)
	}
}

func TestOpenRejectsAncestorSymlink(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	path := filepath.Join(linkDir, "loopcoder-store.db")
	_, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Open error = %v, want ancestor symlink failure", err)
	}
}

func TestIntegrityFailsClosedWhenOwnerOnlyACLBroadened(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "loopcoder-store.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// Replace the protected owner-only DACL with a NULL DACL (fully permissive).
	if err := windowsSetEmptyDACL(path); err != nil {
		t.Fatalf("broaden ACL: %v", err)
	}
	err = store.CheckIntegrity(ctx)
	if err == nil {
		t.Fatal("CheckIntegrity error = nil, want insecure ACL failure")
	}
	if !strings.Contains(err.Error(), "insecure") &&
		!strings.Contains(err.Error(), "owner-only") &&
		!strings.Contains(err.Error(), "DACL") &&
		!strings.Contains(err.Error(), "permissive") {
		t.Fatalf("CheckIntegrity error = %v, want insecure/owner-only/DACL failure", err)
	}
}

func windowsSetEmptyDACL(path string) error {
	// A NULL DACL is fully permissive on Windows. Clearing PROTECTED lets
	// inherited grants apply and models an operator-visible insecure path.
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		nil,
		nil,
	)
}
