package testspawn_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
)

// TestScrubFailedWorktreeProduct_RealGitBoundary proves physical cleanup:
// staged+untracked product/meta residue is removed and full porcelain is empty;
// dirty unrelated residue after partial scrub is not the concern of a successful
// scrub of listed product paths + meta — but any residual after Scrub fails closed.
func TestScrubFailedWorktreeProduct_RealGitBoundary(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("git", "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "README.md")
	run("git", "commit", "-m", "init")

	// Staged product + untracked product + meta stubs (as Fake would leave).
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes", "notes.go"), []byte("package notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra.md"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".loopcoder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".loopcoder", "invocation-binding.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "notes/notes.go")

	// Invalid path entries fail closed (no continue).
	for _, bad := range []string{"", ".", "..", "../escape", "/abs"} {
		if err := testspawn.ScrubFailedWorktreeProduct(dir, []string{bad}); err == nil {
			t.Fatalf("want fail on invalid path %q", bad)
		}
	}

	// Scrub listed product paths; meta is also removed by scrub.
	if err := testspawn.ScrubFailedWorktreeProduct(dir, []string{"notes/notes.go", "extra.md"}); err != nil {
		t.Fatalf("scrub: %v", err)
	}
	// Full porcelain empty.
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--untracked-files=all").CombinedOutput()
	if err != nil {
		t.Fatalf("status after scrub: %v %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("want clean porcelain after scrub, got:\n%s", out)
	}
	// Paths gone on disk.
	for _, rel := range []string{"notes/notes.go", "extra.md", ".loopcoder/invocation-binding.json"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Fatalf("path still present %s err=%v", rel, err)
		}
	}

	// Dirty unrelated residue after a clean scrub: re-dirty and require fail-closed
	// when Scrub is asked to clean an empty product list against a dirty tree.
	if err := os.WriteFile(filepath.Join(dir, "dirt.md"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testspawn.ScrubFailedWorktreeProduct(dir, nil); err == nil {
		t.Fatal("want fail-closed when full porcelain dirty and product list empty")
	}
	// Non-git directory fails closed (never tolerate not-a-git-repository).
	nogit := t.TempDir()
	if err := testspawn.ScrubFailedWorktreeProduct(nogit, []string{"a.go"}); err == nil {
		t.Fatal("want fail on non-git worktree")
	}
}
