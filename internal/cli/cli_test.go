package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func TestRootHelpListsSubcommands(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"--help"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("Run returned exit code %d, want 0", exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	help := stdout.String()
	for _, command := range Commands() {
		if !strings.Contains(help, command.Name) {
			t.Fatalf("root help does not list %q:\n%s", command.Name, help)
		}
	}
}

func TestSubcommandHelpWorks(t *testing.T) {
	for _, command := range Commands() {
		t.Run(command.Name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			exitCode := Run([]string{command.Name, "--help"}, &stdout, &stderr)
			if exitCode != 0 {
				t.Fatalf("Run returned exit code %d, want 0", exitCode)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}

			help := stdout.String()
			if !strings.Contains(help, "loopcoder "+command.Name) {
				t.Fatalf("command help missing usage for %q:\n%s", command.Name, help)
			}
			if !strings.Contains(help, "--help") {
				t.Fatalf("command help missing --help flag:\n%s", help)
			}
		})
	}
}

func TestUnimplementedSubcommandReportsMigrationDoc(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"verify-local"}, &stdout, &stderr)
	if exitCode == 0 {
		t.Fatal("Run returned exit code 0, want non-zero")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}

	message := stderr.String()
	if !strings.Contains(message, "not yet implemented") {
		t.Fatalf("stderr missing not-yet-implemented message: %q", message)
	}
	if !strings.Contains(message, "docs/go-migration.md") {
		t.Fatalf("stderr missing migration doc reference: %q", message)
	}
}

func TestUnknownCommandReturnsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := Run([]string{"unknown"}, &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("Run returned exit code %d, want 2", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr missing unknown-command message: %q", stderr.String())
	}
}

func TestReadySetRunsWithInjectedReader(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{"ready-set", "--repo", repo, "--format", "json"}, &stdout, &stderr, Deps{
		NewGitHubReader: func(string) orchestration.GitHubReader {
			return cliFakeReader{
				issues: []gh.Issue{{Number: 93, Title: "Implement ready-set", State: "OPEN"}},
			}
		},
		ProcessAlive: func(int) bool { return false },
		Now: func() time.Time {
			return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got["repo"] != "owner/repo" {
		t.Fatalf("repo = %#v, want owner/repo", got["repo"])
	}
	summary := got["summary"].(map[string]any)
	if summary["ready_count"] != float64(1) {
		t.Fatalf("ready_count = %#v, want 1", summary["ready_count"])
	}
}

func TestReadySetRequiresRepo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"ready-set"}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--repo is required") {
		t.Fatalf("stderr missing required repo message: %q", stderr.String())
	}
}

func TestDispatchRunsWithInjectedWorker(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "101",
		"--issue-title", "Implement dispatch",
		"--issue-body", "Body",
		"--model", "gpt-5",
		"--effort", "high",
	}, &stdout, &stderr, Deps{
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			if opts.RepoPath != repo {
				t.Fatalf("RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if opts.IssueNumber != 101 || opts.IssueTitle != "Implement dispatch" || opts.IssueBody != "Body" {
				t.Fatalf("dispatch opts issue fields = %#v", opts)
			}
			if opts.BaseBranch != "main" || opts.Provider != "codex" || opts.Model != "gpt-5" || opts.Effort != "high" {
				t.Fatalf("dispatch opts defaults/pass-through = %#v", opts)
			}
			if opts.Stderr == nil {
				t.Fatal("dispatch opts Stderr is nil")
			}
			return worker.Result{
				OK:          true,
				Issue:       opts.IssueNumber,
				Branch:      "loop/issue-101",
				RunID:       "run-test",
				PR:          "https://github.com/owner/repo/pull/101",
				Summary:     "Implemented dispatch.",
				AttemptPath: "/repo/.loopcoder/runs/run-test/workers/job-101-1.attempt.json",
				Status:      "succeeded",
				ExitCode:    0,
				LogBytes:    12,
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	for _, key := range []string{"ok", "issue", "branch", "run_id", "pr", "summary", "attempt_path", "status", "exit_code", "log_bytes"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("dispatch JSON missing %q: %s", key, stdout.String())
		}
	}
	if got["ok"] != true || got["status"] != "succeeded" {
		t.Fatalf("dispatch JSON has wrong success fields: %#v", got)
	}
}

func TestDispatchRequiresIssueFields(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"dispatch"}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--repo is required") {
		t.Fatalf("stderr missing required repo message: %q", stderr.String())
	}
}

func TestRecoverRunsWithInjectedRecoverAndAliases(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{
		"recover",
		"-Repo", repo,
		"-IssueNumber", "103",
		"-IssueTitle", "Implement recover",
		"-IssueBody", "Body",
		"-RunId", "run-test",
		"-BaseBranch", "trunk",
		"-MaxAttempts", "4",
		"-BackoffSeconds", "1,2,3",
		"-Provider", "codex",
		"-Model", "gpt-5",
		"-Effort", "high",
	}, &stdout, &stderr, Deps{
		Recover: func(_ context.Context, opts recovery.Options) (recovery.Result, error) {
			if opts.RepoPath != repo {
				t.Fatalf("RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if opts.IssueNumber != 103 || opts.IssueTitle != "Implement recover" || opts.IssueBody != "Body" {
				t.Fatalf("recover opts issue fields = %#v", opts)
			}
			if opts.RunID != "run-test" || opts.BaseBranch != "trunk" || opts.MaxAttempts != 4 {
				t.Fatalf("recover opts run/base/max = %#v", opts)
			}
			if len(opts.BackoffSeconds) != 3 || opts.BackoffSeconds[0] != 1 || opts.BackoffSeconds[2] != 3 {
				t.Fatalf("BackoffSeconds = %#v, want [1 2 3]", opts.BackoffSeconds)
			}
			if opts.Provider != "codex" || opts.Model != "gpt-5" || opts.Effort != "high" {
				t.Fatalf("recover opts provider/model/effort = %#v", opts)
			}
			if opts.Stderr == nil {
				t.Fatal("recover opts Stderr is nil")
			}
			return recovery.Result{
				Action: recovery.ActionRetry,
				Report: "RETRY: dispatching issue #103 attempt 2\n",
			}, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "RETRY: dispatching issue #103 attempt 2") {
		t.Fatalf("stdout missing retry report: %q", stdout.String())
	}
}

func TestRecoverRequiresRunID(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{
		"recover",
		"--repo", t.TempDir(),
		"--issue-number", "103",
		"--issue-title", "Implement recover",
	}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--run-id is required") {
		t.Fatalf("stderr missing required run-id message: %q", stderr.String())
	}
}

func TestResumeRunsWithInjectedReaderAndDefaultConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	repo := t.TempDir()

	exitCode := RunWithDeps([]string{"resume", "--repo", repo}, &stdout, &stderr, Deps{
		NewGitHubReader: func(string) orchestration.GitHubReader {
			return cliFakeReader{
				issues: []gh.Issue{{Number: 97, Title: "Implement resume", State: "OPEN"}},
			}
		},
		ProcessAlive: func(int) bool { return false },
		Now: func() time.Time {
			return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps returned exit code %d, stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	text := stdout.String()
	for _, want := range []string{
		"RESUME REPORT",
		"Repo: owner/repo",
		"RunId: (none) (.loopcoder/runs not found)",
		"GitHub snapshot: open issues=1, open PRs=0",
		"Local state: attempts=0, events=0",
		"classification: ready",
		"resume is read-only",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stdout missing %q:\n%s", want, text)
		}
	}
}

func TestResumeRequiresRepo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	exitCode := RunWithDeps([]string{"resume"}, &stdout, &stderr, Deps{})
	if exitCode != 2 {
		t.Fatalf("RunWithDeps returned exit code %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "--repo is required") {
		t.Fatalf("stderr missing required repo message: %q", stderr.String())
	}
}

type cliFakeReader struct {
	issues []gh.Issue
}

func (f cliFakeReader) RepoName(context.Context) (string, error) {
	return "owner/repo", nil
}

func (f cliFakeReader) ListIssues(context.Context, string) ([]gh.Issue, error) {
	return f.issues, nil
}

func (f cliFakeReader) ViewIssue(context.Context, int) (gh.Issue, error) {
	return gh.Issue{}, nil
}

func (f cliFakeReader) ListOpenPRs(context.Context) ([]gh.PullRequest, error) {
	return nil, nil
}

func (f cliFakeReader) PRChecks(context.Context, int) ([]gh.Check, error) {
	return nil, nil
}
