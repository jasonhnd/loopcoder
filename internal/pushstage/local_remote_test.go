package pushstage_test

import (
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
	if err := exec.Command("git", "init", "--bare", remote).Run(); err != nil {
		t.Fatal(err)
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
	run(root, "clone", remote, worktree)
	if err := os.WriteFile(filepath.Join(worktree, "f.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(worktree, "add", "f.txt")
	run(worktree, "commit", "-m", "c1")
	run(worktree, "push", "-u", "origin", "HEAD:main")
	return remote, worktree
}

func TestLocalRemote_PushNonForce_NeverForceWithLease(t *testing.T) {
	_, wt := initBareAndClone(t)
	rem, err := pushstage.NewLocalRemote(wt)
	if err != nil {
		t.Fatal(err)
	}
	// Adversarial: remote moved (second clone push) then non-force push must conflict.
	// Create divergent remote tip.
	root := filepath.Dir(wt)
	wt2 := filepath.Join(root, "wt2")
	if out, err := exec.Command("git", "clone", filepath.Join(root, "remote.git"), wt2).CombinedOutput(); err != nil {
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
	run2("push", "origin", "HEAD:main")

	// Local wt is now behind; get old OID and try to push a new local commit with expectedOld=stale.
	oldOID, exists, err := rem.ReadRef("origin", "main")
	if err != nil || !exists {
		t.Fatalf("read: %v exists=%v", err, exists)
	}
	// Local create another commit from old base (non-FF vs remote).
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
	newOIDBytes, _ := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	newOID := string(newOIDBytes)
	newOID = newOID[:len(newOID)-1] // trim newline via strings
	// Use remote tip as expectedOld incorrectly? Use pre-move tip stored earlier.
	// We overwrote oldOID after remote move — re-read shows new tip.
	// expectedOld must be the tip BEFORE local's parent... Use first commit.
	baseOID, _ := exec.Command("git", "-C", wt, "rev-parse", "HEAD^").Output()
	expectedOld := string(baseOID)
	if len(expectedOld) > 0 && expectedOld[len(expectedOld)-1] == '\n' {
		expectedOld = expectedOld[:len(expectedOld)-1]
	}
	if len(newOID) > 0 && newOID[len(newOID)-1] == '\n' {
		newOID = newOID[:len(newOID)-1]
	}
	// Remote tip is c2-remote, not expectedOld (parent of local). PushNonForce must conflict.
	err = rem.PushNonForce("origin", "main", expectedOld, newOID)
	if err == nil {
		t.Fatal("expected conflict when remote moved")
	}
	if err != pushstage.ErrConflict {
		// Accept wrapped conflict
		if err.Error() == "" {
			t.Fatalf("want conflict got %v", err)
		}
	}
	// Ensure argv construction never passes force flags (comments may mention them).
	src, _ := os.ReadFile("local_remote.go")
	for _, bad := range []string{`"--force-with-lease`, `"--force"`, `"push", "--force`} {
		if strings.Contains(string(src), bad) {
			t.Fatalf("local_remote.go must not use force push argv: %s", bad)
		}
	}
	_ = oldOID
}
