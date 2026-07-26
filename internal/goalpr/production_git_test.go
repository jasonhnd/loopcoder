package goalpr_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/goalpr"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=loopcoder-test",
		"GIT_AUTHOR_EMAIL=loopcoder-test@local",
		"GIT_COMMITTER_NAME=loopcoder-test",
		"GIT_COMMITTER_EMAIL=loopcoder-test@local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProductionGitGoalPRReceiptForceAddIsExact(t *testing.T) {
	repo := t.TempDir()
	runGitTest(t, repo, "init", "-b", "main")
	writeTestFile(t, filepath.Join(repo, ".gitignore"), ".loopcoder/\n")
	runGitTest(t, repo, "add", ".gitignore")
	runGitTest(t, repo, "commit", "-m", "initial")

	receipt := filepath.Join(".loopcoder", "goal-pr", "run_exact-receipt.json")
	writeTestFile(t, filepath.Join(repo, receipt), "{}\n")
	writeTestFile(t, filepath.Join(repo, ".loopcoder", "runtime", "state.json"), "{}\n")

	git := goalpr.ProductionGit{}
	if err := git.AddGoalPRReceipt(context.Background(), repo, receipt); err != nil {
		t.Fatal(err)
	}
	tracked := strings.Fields(runGitTest(t, repo, "ls-files", ".loopcoder"))
	if len(tracked) != 1 || filepath.ToSlash(tracked[0]) != filepath.ToSlash(receipt) {
		t.Fatalf("tracked ignored paths=%v want only %s", tracked, receipt)
	}

	for _, bad := range []string{
		".loopcoder",
		filepath.Join(".loopcoder", "runtime", "state.json"),
		filepath.Join(".loopcoder", "goal-pr", "receipt.json"),
		filepath.Join(".loopcoder", "goal-pr", "..", "runtime", "state-receipt.json"),
		filepath.Join("..", ".loopcoder", "goal-pr", "run-receipt.json"),
	} {
		if err := git.AddGoalPRReceipt(context.Background(), repo, bad); err == nil {
			t.Fatalf("AddGoalPRReceipt(%q) unexpectedly accepted", bad)
		}
	}
	tracked = strings.Fields(runGitTest(t, repo, "ls-files", ".loopcoder"))
	if len(tracked) != 1 || filepath.ToSlash(tracked[0]) != filepath.ToSlash(receipt) {
		t.Fatalf("rejected paths changed index: %v", tracked)
	}
}

func TestOpenProductionGitTracksReceiptWhenLoopCoderIsIgnored(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, remote, "init", "--bare")
	runGitTest(t, repo, "init", "-b", "main")
	writeTestFile(t, filepath.Join(repo, ".gitignore"), ".loopcoder/\n")
	writeTestFile(t, filepath.Join(repo, "README.md"), "acceptance repo\n")
	runGitTest(t, repo, "add", ".gitignore", "README.md")
	runGitTest(t, repo, "commit", "-m", "initial")
	runGitTest(t, repo, "remote", "add", "origin", remote)
	runGitTest(t, repo, "push", "-u", "origin", "main")

	const runID = "run_ignored_receipt"
	branch := "loopcoder/goal-" + runID
	runGitTest(t, repo, "checkout", "-b", branch)
	writeTestFile(t, filepath.Join(repo, "slug", "slug.go"), "package slug\n")
	writeTestFile(t, filepath.Join(repo, "verdict.md"), "verified\n")
	writeTestFile(t, filepath.Join(repo, ".loopcoder", "runtime", "state.json"), "{}\n")
	runGitTest(t, repo, "add", "slug/slug.go", "verdict.md")
	runGitTest(t, repo, "commit", "-m", "integrate product")

	off := false
	host := &fakeHost{}
	result, err := goalpr.Open(context.Background(), goalpr.Request{
		RepoPath: repo, BaseRef: "main",
		ProjectID: "proj-real", RunID: runID, GraphID: "g-real",
		PlanDigest: "sha256:plan", Actor: "owner",
		Children: []workflowrun.ChildOutcome{
			{
				WorkItemID: "wi_implement", TaskClass: "tera", Terminal: "succeeded",
				AttemptID: "att-implement", Provider: "codex",
				OutputEvidence: fullSHA256("implement"), FilesTouched: []string{"slug/slug.go"},
			},
			{
				WorkItemID: "wi_verify", TaskClass: "soul", Terminal: "succeeded",
				AttemptID: "att-verify", Provider: "claude",
				OutputEvidence: fullSHA256("verify"), FilesTouched: []string{"verdict.md"},
			},
		},
		InstallMeaningfulCI: &off,
		Git:                 goalpr.ProductionGit{},
		Host:                host,
		Now:                 t0,
	})
	if err != nil {
		t.Fatalf("Open: %v result=%+v", err, result)
	}
	if !result.OK || !result.HumanMergeGate || result.AutoMerge || !host.created {
		t.Fatalf("human gate result=%+v host=%+v", result, host)
	}
	wantReceipt := filepath.ToSlash(filepath.Join(".loopcoder", "goal-pr", runID+"-receipt.json"))
	tracked := strings.Fields(runGitTest(t, repo, "ls-files", ".loopcoder"))
	if len(tracked) != 1 || filepath.ToSlash(tracked[0]) != wantReceipt {
		t.Fatalf("tracked ignored paths=%v want only %s", tracked, wantReceipt)
	}
	if got := runGitTest(t, repo, "show", "HEAD:"+wantReceipt); !strings.Contains(got, goalpr.ReceiptSchema) {
		t.Fatalf("tracked receipt missing schema: %s", got)
	}
	if host.mergeCalled {
		t.Fatal("Open must never merge")
	}
}
