package store

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestErrUnsupportedPlatformIsStable(t *testing.T) {
	err := requireSupportedPlatform()
	if SupportedPlatform() {
		if err != nil {
			t.Fatalf("supported platform returned error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("expected error on unsupported platform")
	}
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("error = %v, want errors.Is ErrUnsupportedPlatform", err)
	}
	if !strings.Contains(err.Error(), "darwin/arm64") {
		t.Fatalf("error %q missing stable product platform text", err)
	}
	if strings.Contains(err.Error(), string(os.PathSeparator)+"Users") ||
		strings.Contains(err.Error(), "HOME=") {
		t.Fatalf("error leaked host path or environment: %v", err)
	}
}

func TestStorePackageHasNoWindowsPermissionImplementation(t *testing.T) {
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.Contains(strings.ToLower(name), "windows") {
			t.Fatalf("v0.9 store path must not ship Windows permission file %s", name)
		}
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		// Skip this inventory test file so its own needle strings are not
		// treated as production references.
		if name == "platform_boundary_test.go" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pkgDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(data)
		windowsSysImport := "golang.org/x/sys/" + "windows"
		if strings.Contains(body, windowsSysImport) {
			t.Fatalf("%s imports %s", name, windowsSysImport)
		}
		windowsPermFile := "permissions_" + "windows"
		if strings.Contains(body, windowsPermFile) {
			t.Fatalf("%s still references %s", name, windowsPermFile)
		}
	}
}

func TestUnsupportedGOOSBuildUsesFailClosedBoundary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cross-compile boundary check runs on darwin CI hosts")
	}
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	out := filepath.Join(t.TempDir(), "store.test")
	cmd := exec.Command("go", "test", "-c", "-o", out, ".")
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH=amd64",
		"CGO_ENABLED=0",
	)
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Either compile fails closed or produces a binary that only exposes the
		// unsupported-platform stubs. A typecheck failure of leftover Windows
		// code is also acceptable; soft-fail only on unexpected tool errors.
		msg := stderr.String()
		if strings.Contains(msg, "permissions_windows") ||
			strings.Contains(msg, "golang.org/x/sys/windows") {
			t.Fatalf("unsupported build still depends on Windows store code:\n%s", msg)
		}
		// Build constraint / missing symbol failures are explicit boundaries.
		if strings.Contains(msg, "build constraints exclude all Go files") ||
			strings.Contains(msg, "undefined:") ||
			strings.Contains(msg, "unsupported") {
			return
		}
		// Successful path is preferred: binary exists with stubs.
		t.Fatalf("GOOS=linux go test -c ./internal/store failed unexpectedly: %v\n%s", err, msg)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected linux test binary at %s: %v", out, err)
	}
}

func TestUnsupportedWindowsGOOSBuildFailsClosed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cross-compile boundary check runs on darwin CI hosts")
	}
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
	err = cmd.Run()
	msg := stderr.String()
	if strings.Contains(msg, "permissions_windows") || strings.Contains(msg, "golang.org/x/sys/windows") {
		t.Fatalf("windows target still compiles Windows ACL store implementation:\n%s", msg)
	}
	// With !darwin stubs the package may compile; that is acceptable only when
	// no Windows ACL implementation remains. If it compiles, keep the binary
	// as evidence the stub path is the only available implementation.
	if err != nil {
		return
	}
	if _, statErr := os.Stat(out); statErr != nil {
		t.Fatalf("expected windows stub binary or explicit build failure; stat: %v", statErr)
	}
}
