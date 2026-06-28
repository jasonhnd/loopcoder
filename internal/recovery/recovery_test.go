package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestScrubRedactsSecretPatterns(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "github ghp token",
			in:   "value ghp_abcDEF123_456",
			want: "value [REDACTED_GITHUB_TOKEN]",
		},
		{
			name: "github pat token",
			in:   "value github_pat_abcDEF123_456",
			want: "value [REDACTED_GITHUB_TOKEN]",
		},
		{
			name: "github other prefixes",
			in:   "gho_one ghu_two ghs_three ghr_four",
			want: "[REDACTED_GITHUB_TOKEN] [REDACTED_GITHUB_TOKEN] [REDACTED_GITHUB_TOKEN] [REDACTED_GITHUB_TOKEN]",
		},
		{
			name: "api key",
			in:   "key sk-12345678901234567890",
			want: "key [REDACTED_API_KEY]",
		},
		{
			name: "bearer token",
			in:   "Authorization: Bearer abc.def_ghi~jkl+/=-",
			want: "Authorization: Bearer [REDACTED_TOKEN]",
		},
		{
			name: "token assignment",
			in:   "token=abc123",
			want: "token=[REDACTED_SECRET]",
		},
		{
			name: "password assignment",
			in:   "password: hunter2",
			want: "password: [REDACTED_SECRET]",
		},
		{
			name: "secret assignment",
			in:   "secret=value",
			want: "secret=[REDACTED_SECRET]",
		},
		{
			name: "api key assignment",
			in:   "api_key: value",
			want: "api_key: [REDACTED_SECRET]",
		},
		{
			name: "api-key assignment",
			in:   "api-key=value",
			want: "api-key=[REDACTED_SECRET]",
		},
		{
			name: "normal text",
			in:   "token bucket and password policy are normal words",
			want: "token bucket and password policy are normal words",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Scrub(tt.in); got != tt.want {
				t.Fatalf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRenderBriefIncludesSectionsFencesAndScrubsSecrets(t *testing.T) {
	brief := RenderBrief(BriefInput{
		IssueNumber:    99,
		IssueTitle:     "Cross-platform recovery",
		Branch:         "loop/issue-99",
		WorktreePath:   "C:/tmp/wt",
		LogPath:        "C:/tmp/codex.log",
		SummaryPath:    "C:/tmp/summary.txt",
		AttemptNumber:  2,
		LastPhase:      "codex_started",
		Status:         "failed",
		Error:          "request failed password=hunter2",
		ChangedFiles:   " M internal/recovery/recovery.go",
		ExistingPRText: "#42 https://github.com/owner/repo/pull/42",
		LogTail:        "Authorization: Bearer abc.def\ntoken=plain",
	})

	for _, want := range []string{
		"# Recovery context for issue #99",
		"- Issue: #99",
		"- Title: Cross-platform recovery",
		"- Branch: loop/issue-99",
		"- Worktree path: C:/tmp/wt",
		"- Log path: C:/tmp/codex.log",
		"- Summary path: C:/tmp/summary.txt",
		"- Attempt: 2",
		"- Last phase: codex_started",
		"- Status: failed",
		"## Changed files",
		"```text\n M internal/recovery/recovery.go\n```",
		"## Existing PR for branch",
		"```text\n#42 https://github.com/owner/repo/pull/42\n```",
		"## Scrubbed log tail (last 50 lines)",
		"Bearer [REDACTED_TOKEN]",
		"token=[REDACTED_SECRET]",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("RenderBrief output missing %q:\n%s", want, brief)
		}
	}

	if strings.Count(brief, "```text") != 3 {
		t.Fatalf("RenderBrief emitted %d fenced code blocks, want 3:\n%s", strings.Count(brief, "```text"), brief)
	}
	for _, leaked := range []string{"hunter2", "abc.def", "token=plain"} {
		if strings.Contains(brief, leaked) {
			t.Fatalf("RenderBrief leaked %q:\n%s", leaked, brief)
		}
	}
}

func TestRecoverAdoptsExistingPRBeforeRetry(t *testing.T) {
	repo := t.TempDir()
	fakeGitHub := &recoverFakeGitHub{
		headPRs: map[string][]gh.PullRequestReference{
			"loop/issue-103-retry-2": {{Number: 55, URL: "https://github.com/owner/repo/pull/55"}},
		},
	}
	var slept bool
	var dispatched bool
	maxBudgetAttempts := 1

	result, err := Run(context.Background(), Options{
		RepoPath:       repo,
		IssueNumber:    103,
		IssueTitle:     "Implement recover",
		RunID:          "run-test",
		MaxAttempts:    3,
		BackoffSeconds: []int{0},
		Budget:         config.GuardrailBudget{MaxTotalAttempts: &maxBudgetAttempts},
	}, Deps{
		GitHub: func(string) PullRequestReader { return fakeGitHub },
		LoadAttempts: func(string, string) ([]state.Attempt, error) {
			return []state.Attempt{{
				Issue:        103,
				Attempt:      1,
				JobID:        "job-103-1",
				Status:       "failed",
				Phase:        "codex_exited",
				Branch:       "loop/issue-103",
				Path:         filepath.Join(repo, ".loopcoder", "runs", "run-test", "workers", "job-103-1.attempt.json"),
				LastWriteUTC: fixedRecoverTime(),
			}}, nil
		},
		Sleep: func(context.Context, time.Duration) error {
			slept = true
			return nil
		},
		Dispatch: func(context.Context, DispatchOptions) (DispatchResult, error) {
			dispatched = true
			return DispatchResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionAdopt {
		t.Fatalf("Action = %q, want %q", result.Action, ActionAdopt)
	}
	if slept || dispatched {
		t.Fatalf("adopt path slept=%v dispatched=%v, want both false", slept, dispatched)
	}
	for _, want := range []string{
		"ADOPT EXISTING PR; NO RETRY",
		"Issue: #103",
		"Prior attempts: 1",
		"Latest status: failed",
		"PR: #55 https://github.com/owner/repo/pull/55",
		"Head branch: loop/issue-103-retry-2",
	} {
		if !strings.Contains(result.Report, want) {
			t.Fatalf("adopt report missing %q:\n%s", want, result.Report)
		}
	}
}

func TestRecoverBudgetBlocksRetryBeforeSleepOrDispatch(t *testing.T) {
	repo := t.TempDir()
	if _, err := state.WriteAttempt(repo, "run-test", state.AttemptRecord{
		Version:        1,
		JobID:          "job-103-1",
		Issue:          103,
		Attempt:        1,
		Provider:       "codex",
		PID:            1234,
		Phase:          "codex_exited",
		Status:         "failed",
		Branch:         "loop/issue-103",
		StartedAt:      state.FormatTimestamp(fixedRecoverTime()),
		HeartbeatAt:    state.FormatTimestamp(fixedRecoverTime()),
		LastProgressAt: state.FormatTimestamp(fixedRecoverTime()),
		LogBytes:       10,
	}); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}

	maxBudgetAttempts := 1
	var slept bool
	var dispatched bool
	result, err := Run(context.Background(), Options{
		RepoPath:       repo,
		IssueNumber:    103,
		IssueTitle:     "Implement recover",
		RunID:          "run-test",
		MaxAttempts:    3,
		BackoffSeconds: []int{0},
		Budget:         config.GuardrailBudget{MaxTotalAttempts: &maxBudgetAttempts},
		Now:            fixedRecoverTime(),
	}, Deps{
		GitHub: func(string) PullRequestReader { return &recoverFakeGitHub{} },
		Sleep: func(context.Context, time.Duration) error {
			slept = true
			return nil
		},
		Dispatch: func(context.Context, DispatchOptions) (DispatchResult, error) {
			dispatched = true
			return DispatchResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionBlocked {
		t.Fatalf("Action = %q, want %q", result.Action, ActionBlocked)
	}
	if slept || dispatched {
		t.Fatalf("budget-blocked retry slept=%v dispatched=%v, want both false", slept, dispatched)
	}
	for _, want := range []string{
		"BLOCKED: guardrails budget needs-human",
		"guardrails.budget.max_total_attempts",
		"Prior attempts: 1",
		"Human decision needed:",
	} {
		if !strings.Contains(result.Report, want) {
			t.Fatalf("budget blocked report missing %q:\n%s", want, result.Report)
		}
	}
	if _, err := os.Stat(guardrails.LedgerPath(repo, "run-test", 103)); err != nil {
		t.Fatalf("budget ledger was not written: %v", err)
	}
}

func TestRecoverBlocksAfterRetryLimit(t *testing.T) {
	repo := t.TempDir()
	briefPath := filepath.Join(repo, ".loopcoder", "runs", "run-test", "recovery", "job-103-3-context.md")
	if err := os.MkdirAll(filepath.Dir(briefPath), 0o755); err != nil {
		t.Fatalf("MkdirAll brief dir: %v", err)
	}
	if err := os.WriteFile(briefPath, []byte("latest recovery brief"), 0o644); err != nil {
		t.Fatalf("WriteFile brief: %v", err)
	}

	var dispatched bool
	result, err := Run(context.Background(), Options{
		RepoPath:       repo,
		IssueNumber:    103,
		IssueTitle:     "Implement recover",
		RunID:          "run-test",
		MaxAttempts:    3,
		BackoffSeconds: []int{0},
	}, Deps{
		GitHub: func(string) PullRequestReader { return &recoverFakeGitHub{} },
		LoadAttempts: func(string, string) ([]state.Attempt, error) {
			return []state.Attempt{
				recoverAttempt(repo, 1, "job-103-1", "failed", "first error"),
				recoverAttempt(repo, 2, "job-103-2", "failed", "second error"),
				recoverAttempt(repo, 3, "job-103-3", "failed", "third error"),
			}, nil
		},
		Dispatch: func(context.Context, DispatchOptions) (DispatchResult, error) {
			dispatched = true
			return DispatchResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionBlocked {
		t.Fatalf("Action = %q, want %q", result.Action, ActionBlocked)
	}
	if dispatched {
		t.Fatal("blocked path dispatched, want no dispatch")
	}
	for _, want := range []string{
		"BLOCKED: retry limit reached",
		"Prior attempts: 3",
		"Max attempts: 3",
		"Latest recovery brief contents:",
		"latest recovery brief",
		"Attempt history:",
		"attempt 3 job job-103-3: status=failed, phase=codex_exited, error=third error",
		"Human decision needed:",
	} {
		if !strings.Contains(result.Report, want) {
			t.Fatalf("blocked report missing %q:\n%s", want, result.Report)
		}
	}
}

func TestRecoverRetriesWithBackoffAndDispatchOptions(t *testing.T) {
	repo := t.TempDir()
	briefPath := filepath.Join(repo, ".loopcoder", "runs", "run-test", "recovery", "job-103-2-context.md")
	if err := os.MkdirAll(filepath.Dir(briefPath), 0o755); err != nil {
		t.Fatalf("MkdirAll brief dir: %v", err)
	}
	if err := os.WriteFile(briefPath, []byte("latest retry context"), 0o644); err != nil {
		t.Fatalf("WriteFile brief: %v", err)
	}

	var slept time.Duration
	var gotDispatch DispatchOptions
	result, err := Run(context.Background(), Options{
		RepoPath:       repo,
		IssueNumber:    103,
		IssueTitle:     "Implement recover",
		IssueBody:      "issue body",
		RunID:          "run-test",
		BaseBranch:     "trunk",
		MaxAttempts:    4,
		BackoffSeconds: []int{10, 30, 120},
		Provider:       "codex",
		Model:          "gpt-5",
		Effort:         "high",
	}, Deps{
		GitHub: func(string) PullRequestReader { return &recoverFakeGitHub{} },
		LoadAttempts: func(string, string) ([]state.Attempt, error) {
			return []state.Attempt{
				recoverAttempt(repo, 1, "job-103-1", "failed", "first error"),
				recoverAttempt(repo, 2, "job-103-2", "failed", "second error"),
			}, nil
		},
		Sleep: func(_ context.Context, duration time.Duration) error {
			slept = duration
			return nil
		},
		Dispatch: func(_ context.Context, opts DispatchOptions) (DispatchResult, error) {
			gotDispatch = opts
			return DispatchResult{
				OK:          true,
				Issue:       opts.IssueNumber,
				Branch:      opts.Branch,
				RunID:       opts.RunID,
				PR:          "https://github.com/owner/repo/pull/103",
				Summary:     "retried",
				AttemptPath: filepath.Join(repo, ".loopcoder", "runs", opts.RunID, "workers", "job-103-3.attempt.json"),
				Status:      "succeeded",
				ExitCode:    0,
				LogBytes:    12,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionRetry {
		t.Fatalf("Action = %q, want %q", result.Action, ActionRetry)
	}
	if slept != 30*time.Second {
		t.Fatalf("slept = %s, want 30s", slept)
	}
	if gotDispatch.Attempt != 3 || gotDispatch.Branch != "loop/issue-103-retry-3" {
		t.Fatalf("dispatch attempt/branch = %d/%q", gotDispatch.Attempt, gotDispatch.Branch)
	}
	if gotDispatch.RunID != "run-test" || gotDispatch.BaseBranch != "trunk" || gotDispatch.RecoveryContext != "latest retry context" {
		t.Fatalf("dispatch run/base/recovery = %#v", gotDispatch)
	}
	if gotDispatch.Provider != "codex" || gotDispatch.Model != "gpt-5" || gotDispatch.Effort != "high" {
		t.Fatalf("dispatch provider/model/effort = %#v", gotDispatch)
	}
	for _, want := range []string{
		"RETRY: dispatching issue #103 attempt 3",
		"Retry branch: loop/issue-103-retry-3",
		"Backoff seconds: 30",
		`"ok":true`,
		`"branch":"loop/issue-103-retry-3"`,
	} {
		if !strings.Contains(result.Report, want) {
			t.Fatalf("retry report missing %q:\n%s", want, result.Report)
		}
	}
}

func recoverAttempt(repo string, attemptNumber int, jobID, status, errText string) state.Attempt {
	return state.Attempt{
		Issue:        103,
		Attempt:      attemptNumber,
		JobID:        jobID,
		Status:       status,
		Phase:        "codex_exited",
		Error:        errText,
		Branch:       fmt.Sprintf("loop/issue-103-retry-%d", attemptNumber),
		Path:         filepath.Join(repo, ".loopcoder", "runs", "run-test", "workers", jobID+".attempt.json"),
		LastWriteUTC: fixedRecoverTime().Add(time.Duration(attemptNumber) * time.Minute),
	}
}

func fixedRecoverTime() time.Time {
	return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
}

type recoverFakeGitHub struct {
	headPRs map[string][]gh.PullRequestReference
	openPRs []gh.PullRequest
}

func (f *recoverFakeGitHub) ListHeadPRs(_ context.Context, branch string) ([]gh.PullRequestReference, error) {
	if f.headPRs == nil {
		return nil, nil
	}
	return f.headPRs[branch], nil
}

func (f *recoverFakeGitHub) ListOpenPRs(context.Context) ([]gh.PullRequest, error) {
	return f.openPRs, nil
}
