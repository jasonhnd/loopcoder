package directrun_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/directrun"
	"github.com/jasonhnd/loopcoder/internal/eventstream"
	"github.com/jasonhnd/loopcoder/internal/execidentity"
	"github.com/jasonhnd/loopcoder/internal/preflight"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
)

func TestRequireExecutionIdentity_RejectsInvalidClassAndCCD(t *testing.T) {
	repo, sha := gitRepo(t)
	base := withDirectContract(t, directrun.Request{
		Repo: "acme/app", Issue: "1", Prompt: "title\n\nbody",
		RepoPath: repo, BaseSHA: sha, Provider: "codex", Model: "m",
		Effort: "medium", Permission: "bounded_write",
		RequiredUI: []string{"terminal"}, ProjectID: "proj-id-val",
	})
	svc := directrun.Service{Deps: directrun.Deps{
		HomeDir: ownerHome(t), Now: func() time.Time { return time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC) },
		Preflight: allowPreflight,
		Provider:  affirmingProvider(t, new(int)),
	}}
	// Uppercase hex in CCD must fail.
	badCCD := base
	badCCD.ChildContractDigest = "sha256:" + strings.ToUpper(strings.TrimPrefix(base.ChildContractDigest, "sha256:"))
	if _, err := svc.Execute(context.Background(), badCCD); err == nil {
		t.Fatal("uppercase CCD must fail")
	} else if !strings.Contains(err.Error(), "lowercase") && !strings.Contains(err.Error(), "child_contract") {
		t.Fatalf("unexpected err: %v", err)
	}
	// Truncated CCD.
	trunc := base
	trunc.ChildContractDigest = "sha256:abcd"
	if _, err := svc.Execute(context.Background(), trunc); err == nil {
		t.Fatal("truncated CCD must fail")
	}
	// needs_human class.
	nh := base
	nh.TaskClass = "needs_human"
	if _, err := svc.Execute(context.Background(), nh); err == nil {
		t.Fatal("needs_human must fail")
	} else if !strings.Contains(err.Error(), "needs_human") {
		t.Fatalf("err=%v", err)
	}
	// Invalid class token.
	bad := base
	bad.TaskClass = "mega"
	if _, err := svc.Execute(context.Background(), bad); err == nil {
		t.Fatal("invalid class must fail")
	}
}

func withDirectContract(t *testing.T, req directrun.Request) directrun.Request {
	t.Helper()
	// Tests that intentionally omit BaseSHA must hit Service fail-closed — do not invent contract.
	if strings.TrimSpace(req.BaseSHA) == "" {
		return req
	}
	title := "Implement feature X from issue body"
	body := "acceptance criteria body"
	if req.Prompt != "" {
		// Split first line as title when multi-line prompt.
		parts := strings.SplitN(req.Prompt, "\n", 2)
		title = strings.TrimSpace(parts[0])
		if len(parts) > 1 {
			body = strings.TrimSpace(parts[1])
		} else {
			body = "product path body"
		}
	}
	depth := strings.TrimSpace(req.Effort)
	if depth == "" {
		depth = "medium"
	}
	perm := strings.TrimSpace(req.Permission)
	if perm == "" || perm == "default" {
		perm = "bounded_write"
	}
	tc := strings.TrimSpace(req.TaskClass)
	if tc == "" {
		tc = "tera"
	}
	proj := strings.TrimSpace(req.ProjectID)
	if proj == "" {
		proj = "proj-dr"
	}
	dc, err := execidentity.BuildDirectContract(execidentity.DirectContractInput{
		IssueTitle: title, IssueBody: body, BaseSHA: req.BaseSHA,
		TaskClass: tc, Depth: depth, Permission: perm,
		OutputContract: execidentity.DirectRunOutputContract,
		Actor:          "owner", ProjectID: proj,
		Now: time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BuildDirectContract: %v", err)
	}
	req.PlanDigest = dc.PlanDigest
	req.GraphDigest = dc.GraphDigest
	req.TaskClass = dc.TaskClass
	req.ChildContractDigest = dc.ChildContractDigest
	return req
}

func ownerHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	if err := os.Chmod(h, 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

func gitRepo(t *testing.T) (repo, sha string) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(out))
}

func allowPreflight(ctx context.Context, in preflight.Input) (preflight.Snapshot, error) {
	return preflight.Snapshot{
		Decision: "allow", AllowLaunch: true, Provider: in.Provider, Repo: in.Repo, Digest: "pf-test",
	}, nil
}

func affirmingProvider(t *testing.T, launches *int) func(ctx context.Context, req providerexec.Request) (providerexec.Outcome, error) {
	t.Helper()
	return func(ctx context.Context, req providerexec.Request) (providerexec.Outcome, error) {
		*launches++
		if strings.TrimSpace(req.PromptRef) == "" {
			t.Fatal("provider must receive non-empty prompt")
		}
		// Spawn authority: product providers must invoke OnProviderStart before work.
		if req.OnProviderStart != nil {
			if err := req.OnProviderStart(providerexec.ProcessStart{PID: os.Getpid(), PGID: os.Getpid()}); err != nil {
				return providerexec.Outcome{}, err
			}
		}
		// Affirm requested identity as actual (controlled test runner).
		out := providerexec.Outcome{
			Schema: providerexec.SchemaOutcome, RequestID: req.RequestID,
			RequestedRoute: req.Route, ActualRoute: req.Route, RouteDigest: req.RouteDigest,
			ExitCode: 0, FinishedAt: time.Now().UTC(),
			OutputDigest: "sha256:test-content",
			Usage:        providerexec.UsageEvidence{InputTokens: 3, OutputTokens: 2},
		}
		return out, nil
	}
}

func TestExecuteReachesCleanupTerminalOnce(t *testing.T) {
	home := ownerHome(t)
	repo, sha := gitRepo(t)
	launches := 0
	svc := directrun.Service{Deps: directrun.Deps{
		HomeDir:   home,
		Now:       func() time.Time { return time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC) },
		Preflight: allowPreflight,
		Provider:  affirmingProvider(t, &launches),
	}}
	var report strings.Builder
	res, err := svc.Execute(context.Background(), withDirectContract(t, directrun.Request{
		Repo: "acme/app", Issue: "42", Prompt: "Implement feature X from issue body",
		RepoPath: repo, BaseSHA: sha,
		Provider: "codex", Model: "gpt-test",
		Permission: "default", BaseBranch: "pre-prod",
		RequiredUI: []string{"terminal"}, ProjectID: "proj-dr-1",
		ReportOut: &report,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != directattempt.StateCleanupTerminal {
		t.Fatalf("state=%s res=%#v", res.State, res)
	}
	if launches != 1 || res.ProviderLaunchN != 1 {
		t.Fatalf("launches=%d resN=%d", launches, res.ProviderLaunchN)
	}
	// Worktree must be a real git worktree (has .git file/dir).
	if _, err := os.Stat(filepath.Join(res.WorktreePath, ".git")); err != nil {
		t.Fatalf("not a git worktree: %v", err)
	}
	if !strings.Contains(report.String(), "start") {
		t.Fatalf("start report missing: %q", report.String())
	}
	st, err := eventstream.OpenAt(home, "proj-dr-1", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ListSequences()) < 2 {
		t.Fatalf("seqs=%v", st.ListSequences())
	}
}

func TestNoLaunchWithoutRenderedStart(t *testing.T) {
	svc := directrun.Service{Deps: directrun.Deps{HomeDir: ownerHome(t), Preflight: allowPreflight}}
	_, err := svc.Execute(context.Background(), withDirectContract(t, directrun.Request{
		Repo: "r", Issue: "1", Provider: "codex", Model: "m", BaseBranch: "pre-prod",
		Permission: "default", ProjectID: "p",
	}))
	if err == nil {
		t.Fatal("expected required UI error")
	}
}

func TestFailClosedMissingBaseSHA(t *testing.T) {
	svc := directrun.Service{Deps: directrun.Deps{HomeDir: ownerHome(t), Preflight: allowPreflight}}
	_, err := svc.Execute(context.Background(), withDirectContract(t, directrun.Request{
		Repo: "r", Issue: "1", Prompt: "do work", RepoPath: "/tmp", Provider: "codex", Model: "m",
		Permission: "default", ProjectID: "p", RequiredUI: []string{"t"},
	}))
	if err == nil || !strings.Contains(err.Error(), "base SHA") {
		t.Fatalf("want base SHA error got %v", err)
	}
}

func TestFailClosedNonzeroExit(t *testing.T) {
	home := ownerHome(t)
	repo, sha := gitRepo(t)
	svc := directrun.Service{Deps: directrun.Deps{
		HomeDir: home, Preflight: allowPreflight,
		Provider: func(ctx context.Context, req providerexec.Request) (providerexec.Outcome, error) {
			return providerexec.Outcome{
				ExitCode: 2, Failure: providerexec.FailProcess, Message: "boom",
				RequestedRoute: req.Route, ActualRoute: req.Route, RequestID: req.RequestID,
			}, nil
		},
	}}
	_, err := svc.Execute(context.Background(), withDirectContract(t, directrun.Request{
		Repo: "acme/app", Issue: "1", Prompt: "do work", RepoPath: repo, BaseSHA: sha,
		Provider: "codex", Model: "m", Permission: "default",
		RequiredUI: []string{"terminal"}, ProjectID: "proj-fail",
	}))
	if err == nil {
		t.Fatal("expected fail closed on nonzero")
	}
}

func TestIdempotentSecondExecuteDifferentAttempt(t *testing.T) {
	home := ownerHome(t)
	repo, sha := gitRepo(t)
	launches := 0
	svc := directrun.Service{Deps: directrun.Deps{
		HomeDir: home, Preflight: allowPreflight,
		Provider: affirmingProvider(t, &launches),
	}}
	r1, err := svc.Execute(context.Background(), withDirectContract(t, directrun.Request{
		Repo: "acme/app", Issue: "1", Prompt: "do work", RepoPath: repo, BaseSHA: sha,
		Provider: "codex", Model: "m", Permission: "default",
		BaseBranch: "pre-prod", RequiredUI: []string{"terminal"}, ProjectID: "proj-idemp",
		RunID: "run-fixed-1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = svc.Execute(context.Background(), withDirectContract(t, directrun.Request{
		Repo: "acme/app", Issue: "1", Prompt: "do work", RepoPath: repo, BaseSHA: sha,
		Provider: "codex", Model: "m", Permission: "default",
		BaseBranch: "pre-prod", RequiredUI: []string{"terminal"}, ProjectID: "proj-idemp",
		RunID: "run-fixed-1",
	}))
	_ = r1
}
