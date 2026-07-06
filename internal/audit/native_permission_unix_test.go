//go:build !windows

package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativePermissionFindingFlagsUnixGroupWorldBits(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, "prompt.txt")
	if err := os.WriteFile(path, []byte("sensitive prompt\n"), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod prompt: %v", err)
	}

	finding, ok := nativePermissionFinding(repo, "prompt.txt")
	if !ok {
		t.Fatal("nativePermissionFinding did not flag group/world-readable sensitive file")
	}
	if finding.Rule != "native:file-permission" || finding.File != "prompt.txt" {
		t.Fatalf("permission finding = %#v, want native:file-permission for prompt.txt", finding)
	}
}

func TestNativePermissionFindingAllowsUnixOwnerOnlyMode(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, ".env")
	if err := os.WriteFile(path, []byte("TOKEN=value\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod .env: %v", err)
	}

	if finding, ok := nativePermissionFinding(repo, ".env"); ok {
		t.Fatalf("nativePermissionFinding flagged owner-only sensitive file: %#v", finding)
	}
}
