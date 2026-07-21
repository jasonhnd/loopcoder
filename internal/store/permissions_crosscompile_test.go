//go:build !windows

package store

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestStorePackageCrossCompilesForWindows keeps the Windows owner-only ACL
// implementation in the default (macOS) CI packet: if permissions_windows.go
// fails to typecheck against golang.org/x/sys/windows, this fails closed.
func TestStorePackageCrossCompilesForWindows(t *testing.T) {
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	out := filepath.Join(t.TempDir(), "store.test.exe")
	cmd := exec.Command("go", "test", "-c", "-o", out, ".")
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(),
		"GOOS=windows",
		"GOARCH=amd64",
		"CGO_ENABLED=0",
	)
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("GOOS=windows go test -c ./internal/store failed: %v\n%s", err, stderr.String())
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected windows test binary at %s: %v", out, err)
	}
}
