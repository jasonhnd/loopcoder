package workflowrun_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// TestSkipIntegrate_CannotFabricateIntegratedOrProductProof: even with a real
// git repo, SkipIntegrate=true keeps succeeded children terminal-only — no
// Integrated membership, no integrate events, no product PR integrate proof.
func TestSkipIntegrate_CannotFabricateIntegratedOrProductProof(t *testing.T) {
	home := testHome(t)
	repo := initGitRepoForSkip(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-skip-int", Definition: workflowrun.ChainDefinition("g_skip"),
		Actor: "owner", RepoPath: repo, SkipIntegrate: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Integrated) != 0 {
		t.Fatalf("SkipIntegrate must not produce Integrated: %v", res.Integrated)
	}
	if len(res.IntegrateCommits) != 0 {
		t.Fatalf("SkipIntegrate must not produce IntegrateCommits: %+v", res.IntegrateCommits)
	}
	for _, c := range res.Children {
		if c.Terminal != "succeeded" {
			t.Fatalf("want terminal succeeded: %+v", c)
		}
		if c.IntegrateCommitSHA != "" {
			t.Fatalf("SkipIntegrate child must not carry integrate SHA: %+v", c)
		}
	}
	// Event log must not contain integrate-equivalent product events for attempts.
	if res.EventLogPath == "" {
		t.Fatal("event log path required")
	}
	raw, err := os.ReadFile(res.EventLogPath)
	if err != nil {
		t.Fatal(err)
	}
	// Exact project/run parse must succeed — no empty-expect retry that masks identity.
	if strings.TrimSpace(res.RunID) == "" {
		t.Fatal("result RunID required for exact event parse")
	}
	evs, err := workflowrun.ParseEventJSONLStrict(string(raw), "proj-skip-int", res.RunID)
	if err != nil {
		t.Fatalf("exact project/run event parse: %v", err)
	}
	for _, e := range evs {
		if strings.EqualFold(e.Kind, "integrate") {
			t.Fatalf("SkipIntegrate must not emit integrate events: %+v", e)
		}
	}
	// Emit lines must not claim integrate fiction.
	for _, line := range res.Events {
		if strings.HasPrefix(line, "integrate:") || strings.Contains(line, "integrate.ok") {
			t.Fatalf("SkipIntegrate must not emit integrate-equivalent event line: %q", line)
		}
	}
}

func initGitRepoForSkip(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
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
	run("init")
	run("checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("skip-int\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")
	return dir
}
