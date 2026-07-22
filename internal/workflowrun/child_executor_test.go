package workflowrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
)

func TestDetectWorktreeEscapesFindsProjectRootFiles(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "runs", "wf_abc", "worktree")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "NOTES.md")
	if err := os.WriteFile(escape, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "ok.go"), []byte("package ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := detectWorktreeEscapes(wt)
	if len(got) == 0 {
		t.Fatal("expected escape detection")
	}
	found := false
	for _, p := range got {
		if filepath.Base(p) == "NOTES.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("NOTES.md not in escapes: %v", got)
	}
}

func TestRootMutationFailsClosedAndCleansUp(t *testing.T) {
	home := t.TempDir()
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "README.md"), []byte("# p"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(home, "projects", "disp-iso")
	wt := filepath.Join(projectRoot, "runs", "wf_x", "worktree")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	parentSnap := snapshotDirTree(parent)
	projectSnap := snapshotDirTree(projectRoot)
	escape := filepath.Join(projectRoot, "NOTES.md")
	if err := os.WriteFile(escape, []byte("escaped"), 0o600); err != nil {
		t.Fatal(err)
	}
	parentEscape := filepath.Join(parent, "LEAK.md")
	if err := os.WriteFile(parentEscape, []byte("leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	escaped := detectWorktreeEscapes(wt)
	parentMut := diffDirTree(parentSnap, snapshotDirTree(parent), wt)
	projectMut := diffDirTree(projectSnap, snapshotDirTree(projectRoot), wt)
	if len(escaped) == 0 && len(projectMut) == 0 {
		t.Fatal("expected project root mutation")
	}
	if len(parentMut) == 0 {
		t.Fatal("expected parent mutation")
	}
	cleanupIsolationViolation(escaped, parentMut, projectMut, parentSnap, projectSnap, parent, projectRoot)
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatalf("escape should be cleaned: %v", err)
	}
	if _, err := os.Stat(parentEscape); !os.IsNotExist(err) {
		t.Fatalf("parent leak should be cleaned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "NOTES.md")); err == nil {
		t.Fatal("must not integrate escapes into worktree")
	}
}

func TestConcurrentChildWorktreesAreIsolated(t *testing.T) {
	home := t.TempDir()
	parent := t.TempDir()
	mustRun(t, parent, "git", "init")
	mustWrite(t, filepath.Join(parent, "README.md"), "# r")
	mustRun(t, parent, "git", "add", "README.md")
	mustRun(t, parent, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "i")

	wt1, err := allocateChildWorktree(home, "disp-a", "g1", "wi_implement", "att-1", parent)
	if err != nil {
		t.Fatal(err)
	}
	wt2, err := allocateChildWorktree(home, "disp-a", "g1", "wi_tests", "att-2", parent)
	if err != nil {
		t.Fatal(err)
	}
	if wt1 == wt2 {
		t.Fatal("worktrees must be distinct")
	}
	mustWrite(t, filepath.Join(wt1, "ONLY1.md"), "one")
	if _, err := os.Stat(filepath.Join(wt2, "ONLY1.md")); err == nil {
		t.Fatal("child2 must not see child1 files")
	}
	if _, err := os.Stat(filepath.Join(parent, "ONLY1.md")); err == nil {
		t.Fatal("parent must not receive child product files")
	}
	if _, err := os.Stat(filepath.Join(wt1, "ONLY1.md")); err != nil {
		t.Fatal(err)
	}
}

// escapeRunner writes a product file to the durable project root (parent of runs/)
// then returns success — Production must fail isolation and cleanup.
type escapeRunner struct{}

func (escapeRunner) Run(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
	// worktree = .../projects/<id>/runs/wf_*/worktree → project root three Dir()s up.
	pr := filepath.Dir(filepath.Dir(filepath.Dir(inv.WorktreePath)))
	_ = os.WriteFile(filepath.Join(pr, "NOTES.md"), []byte("escaped from provider"), 0o600)
	return agent.Result{ExitCode: 0, Summary: "wrote NOTES.md outside worktree", Model: inv.Model, Effort: inv.Effort}, nil
}

func TestProductionFailClosedOnRootEscapeNoRelocate(t *testing.T) {
	home := t.TempDir()
	parent := t.TempDir()
	mustRun(t, parent, "git", "init")
	mustWrite(t, filepath.Join(parent, "README.md"), "# r")
	mustRun(t, parent, "git", "add", "README.md")
	mustRun(t, parent, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "i")

	exec := ProductionChildExecutor{
		HomeDir: home,
		Now:     func() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) },
		Lookup: func(provider string) (agent.Runner, error) {
			return escapeRunner{}, nil
		},
	}
	res, err := exec.Execute(context.Background(), ChildExecInput{
		ProjectID: "disp-iso", GraphID: "g1", WorkItemID: "wi_implement",
		ClaimID: "c1", AttemptID: "att-1", Intent: "write notes",
		Route:    ChildRoute{Provider: "antigravity", Model: "m", Depth: "medium"},
		RepoPath: parent, ReadOnly: false,
	})
	if err == nil {
		t.Fatal("expected isolation failure")
	}
	if res.Terminal != "failed" && res.FailureClass != "isolation_violation" {
		// Terminal is workgraph.TerminalState
		if res.FailureClass != "isolation_violation" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	}
	if res.FailureClass != "isolation_violation" {
		t.Fatalf("FailureClass=%q want isolation_violation", res.FailureClass)
	}
	// Parent unchanged
	if _, err := os.Stat(filepath.Join(parent, "NOTES.md")); err == nil {
		t.Fatal("parent disposable must not keep NOTES.md")
	}
	// Project root cleaned (no success-via-relocate into worktree)
	// worktree should not have NOTES.md from relocate
	if res.WorktreePath != "" {
		if _, err := os.Stat(filepath.Join(res.WorktreePath, "NOTES.md")); err == nil {
			t.Fatal("must not relocate escape into worktree on success path")
		}
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
