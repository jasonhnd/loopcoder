package home_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
	"github.com/jasonhnd/loopcoder/internal/home"
)

func fixedNow() time.Time {
	return time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
}

func TestEnsureMinimumLayoutAndOpenStores(t *testing.T) {
	if !storePlatformOK(t) {
		return
	}
	homeDir := filepath.Join(t.TempDir(), "loopcoder-home")
	repo := filepath.Join(t.TempDir(), "customer-repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	layout, err := home.EnsureMinimumLayout(homeDir, "proj_abc")
	if err != nil {
		t.Fatalf("EnsureMinimumLayout: %v", err)
	}
	if err := layout.AssertNotUnderRepo(repo); err != nil {
		t.Fatal(err)
	}
	// Idempotent second ensure
	if _, err := home.EnsureMinimumLayout(homeDir, "proj_abc"); err != nil {
		t.Fatalf("idempotent ensure: %v", err)
	}

	ctx := context.Background()
	ms, err := layout.OpenMachine(ctx, fixedNow)
	if err != nil {
		t.Fatalf("OpenMachine: %v", err)
	}
	defer ms.Close()
	if ms.Role() != authoritystore.RoleMachine {
		t.Fatalf("role = %s", ms.Role())
	}
	ps, err := layout.OpenProject(ctx, "proj_abc", fixedNow)
	if err != nil {
		t.Fatalf("OpenProject: %v", err)
	}
	defer ps.Close()

	// All paths under home.
	for _, p := range []string{ms.Path(), ps.Path()} {
		if !layout.ContainsPath(p) {
			t.Fatalf("path %s not under home", filepath.Base(p))
		}
		if filepath.IsAbs(p) && (filepath.Dir(p) == repo || hasPrefixPath(p, repo)) {
			t.Fatalf("store path under repo")
		}
	}

	if err := home.ScanRepoForRuntimeState(repo); err != nil {
		t.Fatalf("repo scan: %v", err)
	}
}

func TestProjectIDValidation(t *testing.T) {
	cases := []string{"", ".", "..", "/abs", "a/b", `a\b`, "../x", "bad id", "id$"}
	for _, id := range cases {
		if _, err := home.ValidateProjectID(id); err == nil {
			t.Fatalf("expected error for %q", id)
		}
	}
	if _, err := home.ValidateProjectID("proj_OK-1.2"); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinkHomeComponentRejected(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// EnsureBase on a path whose data component is a symlink fails when validating
	// after creation — create home then replace data with symlink.
	homeDir := filepath.Join(root, "home")
	layout, err := home.NewV09(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureBase(); err != nil {
		t.Fatal(err)
	}
	data := layout.DataDir()
	if err := os.Remove(data); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, data); err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureBase(); err == nil || !errors.Is(err, home.ErrUnsafePath) {
		t.Fatalf("error = %v, want ErrUnsafePath", err)
	}
}

func TestScanRepoDetectsDotLoopcoder(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".loopcoder", "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := home.ScanRepoForRuntimeState(repo); err == nil || !errors.Is(err, home.ErrRepoRuntimeState) {
		t.Fatalf("error = %v, want ErrRepoRuntimeState", err)
	}
}

func TestInsecureModeRejected(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "home")
	layout, err := home.NewV09(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureBase(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(layout.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := layout.EnsureBase(); err == nil || !errors.Is(err, home.ErrUnsafePath) {
		t.Fatalf("error = %v, want ErrUnsafePath", err)
	}
}

func storePlatformOK(t *testing.T) bool {
	t.Helper()
	// authoritystore/store require darwin/arm64
	ms, err := authoritystore.OpenMachine(context.Background(), authoritystore.OpenOptions{
		Path: filepath.Join(t.TempDir(), "probe.db"),
		Now:  fixedNow,
	})
	if err != nil {
		t.Skipf("store platform unavailable: %v", err)
		return false
	}
	_ = ms.Close()
	return true
}

func hasPrefixPath(path, prefix string) bool {
	return path == prefix || len(path) > len(prefix) && path[:len(prefix)+1] == prefix+string(os.PathSeparator)
}
