package pushstage_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/pushstage"
)

func initBareAndClone(t *testing.T) (remote, worktree string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	// Explicit main: do not depend on init.defaultBranch / bare HEAD (CI may be master).
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", remote).CombinedOutput(); err != nil {
		// Older git without --initial-branch: bare init + symbolic-ref.
		if err2 := exec.Command("git", "init", "--bare", remote).Run(); err2 != nil {
			t.Fatalf("git init bare: %v %s", err, out)
		}
	}
	// Pin bare HEAD before any commits so later clones default to main.
	if out, err := exec.Command("git", "-C", remote, "symbolic-ref", "HEAD", "refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("symbolic-ref HEAD main: %v %s", err, out)
	}
	worktree = filepath.Join(root, "wt")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	// Empty bare: clone without --branch, then force branch name main for the seed.
	run(root, "clone", remote, worktree)
	run(worktree, "checkout", "-B", "main")
	if err := os.WriteFile(filepath.Join(worktree, "f.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(worktree, "add", "f.txt")
	run(worktree, "commit", "-m", "c1")
	run(worktree, "push", "-u", "origin", "main")
	// Re-pin bare HEAD after first push (some git versions leave HEAD unset until first ref).
	if out, err := exec.Command("git", "-C", remote, "symbolic-ref", "HEAD", "refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("pin bare HEAD after push: %v %s", err, out)
	}
	return remote, worktree
}

func TestLocalRemote_PushNonForce_NeverForceWithLease(t *testing.T) {
	_, wt := initBareAndClone(t)
	rem, err := pushstage.NewLocalRemote(wt)
	if err != nil {
		t.Fatal(err)
	}
	// Adversarial topology (independent of init.defaultBranch):
	//   c1 on main → remote and both clones share c1.
	//   wt2 advances to c2-remote and pushes main.
	//   wt independently advances to c2-local from c1 (non-FF vs remote).
	//   PushNonForce(expectedOld=c1, newOID=c2-local) → ErrConflict, never force/lease.
	root := filepath.Dir(wt)
	// Capture c1 tip BEFORE remote moves (stale expectedOld for PushNonForce).
	c1Bytes, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	expectedOld := strings.TrimSpace(string(c1Bytes))

	wt2 := filepath.Join(root, "wt2")
	if out, err := exec.Command("git", "clone", "--branch", "main", filepath.Join(root, "remote.git"), wt2).CombinedOutput(); err != nil {
		t.Fatalf("clone2: %v %s", err, out)
	}
	run2 := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = wt2
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(wt2, "f.txt"), []byte("v2-remote\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run2("add", "f.txt")
	run2("commit", "-m", "c2-remote")
	run2("push", "origin", "main")

	// Local wt still at c1; create independent c2-local from c1 (non-FF vs remote).
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("v2-local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "f.txt")
	cmd.Dir = wt
	_ = cmd.Run()
	cmd = exec.Command("git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "c2-local")
	cmd.Dir = wt
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit local: %v %s", err, out)
	}
	newOIDBytes, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	newOID := strings.TrimSpace(string(newOIDBytes))
	// Remote tip is c2-remote; expectedOld is c1. PushNonForce must conflict.
	err = rem.PushNonForce("origin", "main", expectedOld, newOID)
	if err == nil {
		t.Fatal("expected conflict when remote moved")
	}
	if !errors.Is(err, pushstage.ErrConflict) {
		t.Fatalf("want errors.Is(err, pushstage.ErrConflict), got %v", err)
	}
	// Ensure argv construction never passes force flags (comments may mention them).
	src, _ := os.ReadFile("local_remote.go")
	for _, bad := range []string{`"--force-with-lease`, `"--force"`, `"push", "--force`} {
		if strings.Contains(string(src), bad) {
			t.Fatalf("local_remote.go must not use force push argv: %s", bad)
		}
	}
}
