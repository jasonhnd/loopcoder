package workflowrun_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("git", "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "README.md")
	run("git", "commit", "-m", "init")
	return dir
}

func TestChildIntegrateOntoSharedGoalBranchVisibleToNext(t *testing.T) {
	repo := initGitRepo(t)
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0,
			ProductFiles: map[string][]string{
				"a": {"notes/notes.go"},
				"b": {"notes/notes_test.go"},
				"c": {"docs/c.md"},
			},
		},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj-int", RunID: "run_int_1",
		Definition: workflowrun.ChainDefinition("g-int"),
		Actor:      "owner",
		RepoPath:   repo,
		BaseRef:    "main",
		GoalBranch: "loopcoder/goal-run_int_1",
	})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("%+v", res)
	}
	if len(res.IntegrateCommits) < 3 {
		t.Fatalf("want ≥3 integrate commits, got %+v", res.IntegrateCommits)
	}
	// Goal branch must contain product files from a and b (not receipt-only).
	show := func(path string) string {
		cmd := exec.Command("git", "show", "loopcoder/goal-run_int_1:"+path)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("show %s: %v %s", path, err, out)
		}
		return string(out)
	}
	if !strings.Contains(show("notes/notes.go"), "package notes") {
		t.Fatal("notes.go missing on goal branch")
	}
	if !strings.Contains(show("notes/notes_test.go"), "TestNotes") {
		t.Fatal("notes_test.go missing on goal branch")
	}
	// b's worktree was based on goal branch after a integrated — b should see a's file.
	var bWT string
	for _, c := range res.Children {
		if c.WorkItemID == "b" {
			bWT = c.WorktreePath
		}
	}
	if bWT == "" {
		t.Fatal("missing b worktree")
	}
	if _, err := os.Stat(filepath.Join(bWT, "notes/notes.go")); err != nil {
		t.Fatalf("b must see a's integrated notes.go: %v", err)
	}
	joined := strings.Join(res.Events, "\n")
	if !strings.Contains(joined, "integrate.ok:a") && !strings.Contains(joined, "integrate.skip:a") {
		t.Fatalf("missing integrate event: %v", res.Events)
	}
}

func TestIntegrateExactlyOnceSameAttempt(t *testing.T) {
	repo := initGitRepo(t)
	integ := workflowrun.GitBranchIntegrator{Now: func() time.Time { return t0() }}
	if _, err := integ.EnsureGoalBranch(context.Background(), repo, "main", "loopcoder/goal-once"); err != nil {
		t.Fatal(err)
	}
	// Prepare child worktree with product file.
	child := t.TempDir()
	// init child as git repo with file
	must := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = child
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	must("git", "init")
	_ = os.MkdirAll(filepath.Join(child, "pkg"), 0o700)
	_ = os.WriteFile(filepath.Join(child, "pkg/x.go"), []byte("package pkg\n"), 0o600)
	must("git", "add", "pkg/x.go")

	c1, err := integ.IntegrateChild(context.Background(), workflowrun.IntegrateRequest{
		RepoPath: repo, GoalBranch: "loopcoder/goal-once",
		WorkItemID: "w1", AttemptID: "att-w1-1", ChildWorktree: child,
		ProductFiles: []string{"pkg/x.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c1.Skipped || c1.CommitSHA == "" {
		t.Fatalf("%+v", c1)
	}
	c2, err := integ.IntegrateChild(context.Background(), workflowrun.IntegrateRequest{
		RepoPath: repo, GoalBranch: "loopcoder/goal-once",
		WorkItemID: "w1", AttemptID: "att-w1-1", ChildWorktree: child,
		ProductFiles: []string{"pkg/x.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Skipped || c2.CommitSHA != c1.CommitSHA {
		t.Fatalf("exactly-once: first=%+v second=%+v", c1, c2)
	}
}

func TestIntegrateSequentialWorkItemsMayRefineSharedPath(t *testing.T) {
	repo := initGitRepo(t)
	integ := workflowrun.GitBranchIntegrator{}
	if _, err := integ.EnsureGoalBranch(context.Background(), repo, "main", "loopcoder/goal-conflict"); err != nil {
		t.Fatal(err)
	}
	mkChild := func(body string) string {
		d := t.TempDir()
		cmd := exec.Command("git", "init")
		cmd.Dir = d
		_ = cmd.Run()
		_ = os.MkdirAll(filepath.Join(d, "notes"), 0o700)
		_ = os.WriteFile(filepath.Join(d, "notes/clash.go"), []byte(body), 0o600)
		return d
	}
	c1 := mkChild("package notes\nfunc A() {}\n")
	if _, err := integ.IntegrateChild(context.Background(), workflowrun.IntegrateRequest{
		RepoPath: repo, GoalBranch: "loopcoder/goal-conflict",
		WorkItemID: "wi_implement", AttemptID: "att-a", ChildWorktree: c1,
		ProductFiles: []string{"notes/clash.go"},
	}); err != nil {
		t.Fatal(err)
	}
	// Sequential tests child may refine the same product path.
	c2 := mkChild("package notes\nfunc B() {}\n")
	if _, err := integ.IntegrateChild(context.Background(), workflowrun.IntegrateRequest{
		RepoPath: repo, GoalBranch: "loopcoder/goal-conflict",
		WorkItemID: "wi_tests", AttemptID: "att-b", ChildWorktree: c2,
		ProductFiles: []string{"notes/clash.go"},
	}); err != nil {
		t.Fatalf("sequential refine should be allowed: %v", err)
	}
}

func TestIntegrateSameWorkItemDifferentAttemptConflicts(t *testing.T) {
	repo := initGitRepo(t)
	integ := workflowrun.GitBranchIntegrator{}
	if _, err := integ.EnsureGoalBranch(context.Background(), repo, "main", "loopcoder/goal-same"); err != nil {
		t.Fatal(err)
	}
	mkChild := func(body string) string {
		d := t.TempDir()
		cmd := exec.Command("git", "init")
		cmd.Dir = d
		_ = cmd.Run()
		_ = os.MkdirAll(filepath.Join(d, "notes"), 0o700)
		_ = os.WriteFile(filepath.Join(d, "notes/clash.go"), []byte(body), 0o600)
		return d
	}
	c1 := mkChild("package notes\nfunc A() {}\n")
	if _, err := integ.IntegrateChild(context.Background(), workflowrun.IntegrateRequest{
		RepoPath: repo, GoalBranch: "loopcoder/goal-same",
		WorkItemID: "wi_implement", AttemptID: "att-a", ChildWorktree: c1,
		ProductFiles: []string{"notes/clash.go"},
	}); err != nil {
		t.Fatal(err)
	}
	c2 := mkChild("package notes\nfunc B() {}\n")
	_, err := integ.IntegrateChild(context.Background(), workflowrun.IntegrateRequest{
		RepoPath: repo, GoalBranch: "loopcoder/goal-same",
		WorkItemID: "wi_implement", AttemptID: "att-b", ChildWorktree: c2,
		ProductFiles: []string{"notes/clash.go"},
	})
	if err == nil {
		t.Fatal("expected same-work-item path conflict")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestEmptyProductFailsIntegrate(t *testing.T) {
	repo := initGitRepo(t)
	integ := workflowrun.GitBranchIntegrator{}
	if _, err := integ.EnsureGoalBranch(context.Background(), repo, "main", "loopcoder/goal-empty"); err != nil {
		t.Fatal(err)
	}
	child := t.TempDir()
	_ = exec.Command("git", "init").Run()
	// only meta
	_ = os.MkdirAll(filepath.Join(child, ".loopcoder"), 0o700)
	_ = os.WriteFile(filepath.Join(child, ".loopcoder/x.json"), []byte(`{}`), 0o600)
	_, err := integ.IntegrateChild(context.Background(), workflowrun.IntegrateRequest{
		RepoPath: repo, GoalBranch: "loopcoder/goal-empty",
		WorkItemID: "z", AttemptID: "att-z", ChildWorktree: child,
		ProductFiles: []string{".loopcoder/x.json"},
	})
	if err == nil {
		t.Fatal("meta-only must not integrate")
	}
}
