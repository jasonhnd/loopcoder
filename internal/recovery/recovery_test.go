package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/reporter"
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
			name: "aws access key",
			in:   "key " + "AKIA" + strings.Repeat("A", 16),
			want: "key [REDACTED_AWS_ACCESS_KEY]",
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

func TestRecoverAdoptsExistingPRBeforeCircuitBreaker(t *testing.T) {
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
	fakeGitHub := &recoverFakeGitHub{
		headPRs: map[string][]gh.PullRequestReference{
			"loop/issue-103": {{Number: 55, URL: "https://github.com/owner/repo/pull/55"}},
		},
	}
	maxNoProgressAttempts := 1
	var slept bool
	var dispatched bool

	result, err := Run(context.Background(), Options{
		RepoPath:       repo,
		IssueNumber:    103,
		IssueTitle:     "Implement recover",
		RunID:          "run-test",
		MaxAttempts:    3,
		BackoffSeconds: []int{0},
		CircuitBreaker: config.GuardrailCircuitBreaker{
			MaxNoProgressAttempts: &maxNoProgressAttempts,
		},
		Now: fixedRecoverTime(),
	}, Deps{
		GitHub: func(string) PullRequestReader { return fakeGitHub },
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
	data, err := os.ReadFile(guardrails.LedgerPath(repo, "run-test", 103))
	if err != nil {
		t.Fatalf("read circuit ledger: %v", err)
	}
	if !strings.Contains(string(data), `"status": "allowed"`) ||
		!strings.Contains(string(data), `"material_progress": true`) {
		t.Fatalf("adopt should record material progress without freezing:\n%s", string(data))
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

func TestRecoverRunEmitsRecoveryProgressAndTerminalBlocked(t *testing.T) {
	repo := t.TempDir()
	recorder := &recordingRecoveryProgressRecorder{}
	result, err := Run(context.Background(), Options{
		RepoPath:       repo,
		IssueNumber:    103,
		IssueTitle:     "Implement recover",
		RunID:          "run-test",
		MaxAttempts:    1,
		Now:            fixedRecoverTime(),
		SkipAdoptPR:    true,
		Progress:       recorder,
		BackoffSeconds: []int{0},
	}, Deps{
		GitHub: func(string) PullRequestReader { return &recoverFakeGitHub{} },
		LoadAttempts: func(string, string) ([]state.Attempt, error) {
			return []state.Attempt{{Issue: 103, Attempt: 1, JobID: "job-103-1", Status: "failed"}}, nil
		},
		Dispatch: func(context.Context, DispatchOptions) (DispatchResult, error) {
			t.Fatal("dispatch should not run after max-attempts block")
			return DispatchResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionBlocked {
		t.Fatalf("Action = %q, want blocked", result.Action)
	}
	if !recorder.hasKnown(progress.KnownRecoveryInProgress) || !recorder.hasTerminal(progress.KnownBlocked) {
		t.Fatalf("recovery progress observations = %#v", recorder.observations)
	}
}

func TestRecoverCircuitBreakerBlocksRetryBeforeSleepOrDispatch(t *testing.T) {
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

	maxNoProgressAttempts := 1
	var slept bool
	var dispatched bool
	result, err := Run(context.Background(), Options{
		RepoPath:       repo,
		IssueNumber:    103,
		IssueTitle:     "Implement recover",
		RunID:          "run-test",
		MaxAttempts:    3,
		BackoffSeconds: []int{0},
		CircuitBreaker: config.GuardrailCircuitBreaker{
			MaxNoProgressAttempts: &maxNoProgressAttempts,
		},
		Now: fixedRecoverTime(),
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
		t.Fatalf("circuit-blocked retry slept=%v dispatched=%v, want both false", slept, dispatched)
	}
	for _, want := range []string{
		"BLOCKED: guardrails circuit-breaker needs-human",
		"guardrails.circuit_breaker.max_no_progress_attempts",
		"No-progress attempts: 1",
		"Last material progress: unknown",
		"Attempt history:",
		"Human decision needed:",
	} {
		if !strings.Contains(result.Report, want) {
			t.Fatalf("circuit blocked report missing %q:\n%s", want, result.Report)
		}
	}
	data, err := os.ReadFile(guardrails.LedgerPath(repo, "run-test", 103))
	if err != nil {
		t.Fatalf("read circuit ledger: %v", err)
	}
	if !strings.Contains(string(data), `"status": "needs-human"`) ||
		!strings.Contains(string(data), `"no_progress_attempts": 1`) {
		t.Fatalf("circuit ledger missing frozen evidence:\n%s", string(data))
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
		MaxAttempts:    3,
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
				Report:      recoverReport(opts.IssueNumber, 321),
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
	if gotDispatch.Provider != "codex" || gotDispatch.Model != "gpt-5" || gotDispatch.Effort != "xhigh" {
		t.Fatalf("dispatch provider/model/effort = %#v", gotDispatch)
	}
	for _, want := range []string{
		"RETRY: dispatching issue #103 attempt 3",
		"Retry branch: loop/issue-103-retry-3",
		"Recovery strategy: upgraded_config",
		"Effort: xhigh",
		"Backoff seconds: 30",
		`"ok":true`,
		`"branch":"loop/issue-103-retry-3"`,
		`"report":{"role":"worker"`,
		`"model":"recover-model-103"`,
	} {
		if !strings.Contains(result.Report, want) {
			t.Fatalf("retry report missing %q:\n%s", want, result.Report)
		}
	}
	if result.DispatchResult == nil || result.DispatchResult.Report == nil {
		t.Fatalf("retry result missing report: %#v", result.DispatchResult)
	}
	if result.DispatchResult.Report.Model != "recover-model-103" ||
		result.DispatchResult.Report.Usage.TotalTokens == nil ||
		*result.DispatchResult.Report.Usage.TotalTokens != 321 {
		t.Fatalf("retry report not preserved: %#v", result.DispatchResult.Report)
	}
}

func TestRecoverReadsLegacyRecoveryBriefForRegisteredProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOOPCODER_HOME", home)
	repo := t.TempDir()
	if _, err := registry.Register(context.Background(), registry.Options{RepoPath: repo}, registry.DefaultDeps()); err != nil {
		t.Fatalf("register project: %v", err)
	}
	legacyBrief := filepath.Join(repo, ".loopcoder", "runs", "run-test", "recovery", "job-103-2-context.md")
	if err := os.MkdirAll(filepath.Dir(legacyBrief), 0o755); err != nil {
		t.Fatalf("MkdirAll legacy brief dir: %v", err)
	}
	if err := os.WriteFile(legacyBrief, []byte("legacy retry context"), 0o644); err != nil {
		t.Fatalf("WriteFile legacy brief: %v", err)
	}

	var gotDispatch DispatchOptions
	result, err := Run(context.Background(), Options{
		RepoPath:       repo,
		IssueNumber:    103,
		IssueTitle:     "Implement recover",
		IssueBody:      "issue body",
		RunID:          "run-test",
		MaxAttempts:    3,
		BackoffSeconds: []int{0},
	}, Deps{
		GitHub: func(string) PullRequestReader { return &recoverFakeGitHub{} },
		LoadAttempts: func(string, string) ([]state.Attempt, error) {
			return []state.Attempt{
				recoverAttempt(repo, 1, "job-103-1", "failed", "first error"),
				recoverAttempt(repo, 2, "job-103-2", "failed", "second error"),
			}, nil
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
				AttemptPath: state.AttemptPath(repo, opts.RunID, "job-103-3"),
				Status:      "succeeded",
				ExitCode:    0,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionRetry {
		t.Fatalf("Action = %q, want %q", result.Action, ActionRetry)
	}
	if gotDispatch.RecoveryContext != "legacy retry context" {
		t.Fatalf("dispatch recovery context = %q, want legacy retry context", gotDispatch.RecoveryContext)
	}
	if !strings.Contains(result.Report, "Latest recovery brief: "+legacyBrief) {
		t.Fatalf("report did not surface legacy brief path %s:\n%s", legacyBrief, result.Report)
	}
}

func TestRecoverLoopReviewFailRetriesSameThenUpgradedThenBlocks(t *testing.T) {
	repo := t.TempDir()
	attempts := []state.Attempt{recoverAttempt(repo, 1, "job-103-1", "failed", "first error")}
	var dispatched []DispatchOptions
	var reviewed []int

	result, err := Run(context.Background(), Options{
		RepoPath:         repo,
		IssueNumber:      103,
		IssueTitle:       "Implement recover",
		RunID:            "run-test",
		MaxAttempts:      3,
		BackoffSeconds:   []int{0},
		Provider:         "codex",
		VerifierProvider: "claude",
	}, Deps{
		GitHub: func(string) PullRequestReader { return &recoverFakeGitHub{} },
		LoadAttempts: func(string, string) ([]state.Attempt, error) {
			return append([]state.Attempt(nil), attempts...), nil
		},
		Dispatch: func(_ context.Context, opts DispatchOptions) (DispatchResult, error) {
			dispatched = append(dispatched, opts)
			attempts = append(attempts, recoverAttempt(repo, opts.Attempt, fmt.Sprintf("job-103-%d", opts.Attempt), "succeeded", ""))
			return DispatchResult{
				OK:     true,
				Issue:  opts.IssueNumber,
				Branch: opts.Branch,
				RunID:  opts.RunID,
				PR:     fmt.Sprintf("https://github.com/owner/repo/pull/%d", 100+opts.Attempt),
				Status: "succeeded",
			}, nil
		},
		Review: func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
			reviewed = append(reviewed, opts.PRNumber)
			return loopreview.Result{
				Verdict: loopreview.Verdict{
					Verdict:         loopreview.VerdictFail,
					Evidence:        fmt.Sprintf("review failed for PR #%d", opts.PRNumber),
					Findings:        []loopreview.Finding{},
					SpecConformance: loopreview.SpecConformanceFail,
				},
				ExitCode: loopreview.ExitCodeForVerdict(loopreview.VerdictFail),
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionBlocked {
		t.Fatalf("Action = %q, want %q", result.Action, ActionBlocked)
	}
	if len(dispatched) != 2 {
		t.Fatalf("dispatch calls = %#v, want two", dispatched)
	}
	if dispatched[0].Attempt != 2 || dispatched[0].Effort != "" {
		t.Fatalf("same-config dispatch = %#v", dispatched[0])
	}
	if dispatched[1].Attempt != 3 || dispatched[1].Effort != "xhigh" {
		t.Fatalf("upgraded dispatch = %#v", dispatched[1])
	}
	if !reflect.DeepEqual(reviewed, []int{102, 103}) {
		t.Fatalf("reviewed PRs = %#v", reviewed)
	}
	if len(result.RecoveryAttempts) != 2 ||
		result.RecoveryAttempts[0].Strategy != AttemptStrategySameConfig ||
		result.RecoveryAttempts[1].Strategy != AttemptStrategyUpgradedConfig {
		t.Fatalf("recovery attempts = %#v", result.RecoveryAttempts)
	}
	for _, want := range []string{
		"RETRY: dispatching issue #103 attempt 2",
		"Recovery strategy: same_config",
		"RETRY: dispatching issue #103 attempt 3",
		"Recovery strategy: upgraded_config",
		"BLOCKED: retry limit reached",
	} {
		if !strings.Contains(result.Report, want) {
			t.Fatalf("report missing %q:\n%s", want, result.Report)
		}
	}
}

func TestRecoverWithRaisedMaxAttemptsUpgradesOnlyFinalAttempt(t *testing.T) {
	repo := t.TempDir()
	attempts := []state.Attempt{recoverAttempt(repo, 1, "job-103-1", "failed", "first error")}
	var dispatched []DispatchOptions
	var reviewed []int

	result, err := Run(context.Background(), Options{
		RepoPath:         repo,
		IssueNumber:      103,
		IssueTitle:       "Implement recover",
		RunID:            "run-test",
		MaxAttempts:      5,
		BackoffSeconds:   []int{0},
		Provider:         "codex",
		VerifierProvider: "claude",
	}, Deps{
		GitHub: func(string) PullRequestReader { return &recoverFakeGitHub{} },
		LoadAttempts: func(string, string) ([]state.Attempt, error) {
			return append([]state.Attempt(nil), attempts...), nil
		},
		Dispatch: func(_ context.Context, opts DispatchOptions) (DispatchResult, error) {
			dispatched = append(dispatched, opts)
			attempts = append(attempts, recoverAttempt(repo, opts.Attempt, fmt.Sprintf("job-103-%d", opts.Attempt), "succeeded", ""))
			return DispatchResult{
				OK:     true,
				Issue:  opts.IssueNumber,
				Branch: opts.Branch,
				RunID:  opts.RunID,
				PR:     fmt.Sprintf("https://github.com/owner/repo/pull/%d", 100+opts.Attempt),
				Status: "succeeded",
			}, nil
		},
		Review: func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
			reviewed = append(reviewed, opts.PRNumber)
			return loopreview.Result{
				Verdict: loopreview.Verdict{
					Verdict:         loopreview.VerdictFail,
					Evidence:        fmt.Sprintf("review failed for PR #%d", opts.PRNumber),
					Findings:        []loopreview.Finding{},
					SpecConformance: loopreview.SpecConformanceFail,
				},
				ExitCode: loopreview.ExitCodeForVerdict(loopreview.VerdictFail),
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionBlocked {
		t.Fatalf("Action = %q, want %q", result.Action, ActionBlocked)
	}
	if len(dispatched) != 4 {
		t.Fatalf("dispatch calls = %#v, want four", dispatched)
	}
	for i, opts := range dispatched {
		wantAttempt := i + 2
		wantEffort := ""
		if wantAttempt == 5 {
			wantEffort = "xhigh"
		}
		if opts.Attempt != wantAttempt || opts.Effort != wantEffort {
			t.Fatalf("dispatch[%d] = %#v, want attempt %d effort %q", i, opts, wantAttempt, wantEffort)
		}
	}
	if !reflect.DeepEqual(reviewed, []int{102, 103, 104, 105}) {
		t.Fatalf("reviewed PRs = %#v", reviewed)
	}
	if len(result.RecoveryAttempts) != 4 ||
		result.RecoveryAttempts[0].Strategy != AttemptStrategySameConfig ||
		result.RecoveryAttempts[1].Strategy != AttemptStrategySameConfig ||
		result.RecoveryAttempts[2].Strategy != AttemptStrategySameConfig ||
		result.RecoveryAttempts[3].Strategy != AttemptStrategyUpgradedConfig {
		t.Fatalf("recovery attempts = %#v", result.RecoveryAttempts)
	}
	if strings.Count(result.Report, "Recovery strategy: same_config") != 3 ||
		strings.Count(result.Report, "Recovery strategy: upgraded_config") != 1 {
		t.Fatalf("report did not show graduated escalation:\n%s", result.Report)
	}
	for _, want := range []string{
		"RETRY: dispatching issue #103 attempt 4",
		"RETRY: dispatching issue #103 attempt 5",
		"BLOCKED: retry limit reached",
	} {
		if !strings.Contains(result.Report, want) {
			t.Fatalf("report missing %q:\n%s", want, result.Report)
		}
	}
}

func TestRecoverLoopReviewPassStopsMidLoop(t *testing.T) {
	repo := t.TempDir()
	attempts := []state.Attempt{recoverAttempt(repo, 1, "job-103-1", "failed", "first error")}
	dispatchCalls := 0
	reviewCalls := 0

	result, err := Run(context.Background(), Options{
		RepoPath:         repo,
		IssueNumber:      103,
		IssueTitle:       "Implement recover",
		RunID:            "run-test",
		MaxAttempts:      3,
		BackoffSeconds:   []int{0},
		Provider:         "codex",
		VerifierProvider: "claude",
	}, Deps{
		GitHub: func(string) PullRequestReader { return &recoverFakeGitHub{} },
		LoadAttempts: func(string, string) ([]state.Attempt, error) {
			return append([]state.Attempt(nil), attempts...), nil
		},
		Dispatch: func(_ context.Context, opts DispatchOptions) (DispatchResult, error) {
			dispatchCalls++
			attempts = append(attempts, recoverAttempt(repo, opts.Attempt, fmt.Sprintf("job-103-%d", opts.Attempt), "succeeded", ""))
			return DispatchResult{
				OK:     true,
				Issue:  opts.IssueNumber,
				Branch: opts.Branch,
				RunID:  opts.RunID,
				PR:     "https://github.com/owner/repo/pull/102",
				Status: "succeeded",
			}, nil
		},
		Review: func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
			reviewCalls++
			if opts.PRNumber != 102 || opts.Timeout <= 0 {
				t.Fatalf("review opts = %#v", opts)
			}
			return loopreview.Result{
				Verdict: loopreview.Verdict{
					Verdict:         loopreview.VerdictPass,
					Evidence:        "review passed",
					Findings:        []loopreview.Finding{},
					SpecConformance: loopreview.SpecConformancePass,
				},
				ExitCode: 0,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionSucceeded {
		t.Fatalf("Action = %q, want %q", result.Action, ActionSucceeded)
	}
	if dispatchCalls != 1 || reviewCalls != 1 {
		t.Fatalf("calls dispatch=%d review=%d, want one each", dispatchCalls, reviewCalls)
	}
	if result.DispatchResult == nil || result.DispatchResult.PR != "https://github.com/owner/repo/pull/102" {
		t.Fatalf("dispatch result = %#v", result.DispatchResult)
	}
	if result.ReviewResult == nil || result.ReviewResult.Verdict.Verdict != loopreview.VerdictPass {
		t.Fatalf("review result = %#v", result.ReviewResult)
	}
	if len(result.RecoveryAttempts) != 1 || result.RecoveryAttempts[0].Strategy != AttemptStrategySameConfig || result.RecoveryAttempts[0].Status != "succeeded" {
		t.Fatalf("recovery attempts = %#v", result.RecoveryAttempts)
	}
}

func TestRecoverBlocksNeedsHumanDispatchResultWithoutReview(t *testing.T) {
	repo := t.TempDir()
	reviewCalls := 0

	result, err := Run(context.Background(), Options{
		RepoPath:         repo,
		IssueNumber:      103,
		IssueTitle:       "Implement recover",
		RunID:            "run-test",
		MaxAttempts:      3,
		BackoffSeconds:   []int{0},
		VerifierProvider: "claude",
	}, Deps{
		GitHub: func(string) PullRequestReader { return &recoverFakeGitHub{} },
		Dispatch: func(context.Context, DispatchOptions) (DispatchResult, error) {
			return DispatchResult{
				OK:      true,
				Issue:   103,
				Branch:  "loop/issue-103-retry-1",
				PR:      "https://github.com/owner/repo/pull/103",
				Status:  guardrails.StatusNeedsHuman,
				Summary: "harvested from hung/killed worker - possibly incomplete",
				Report:  recoverReport(103, 321),
			}, nil
		},
		Review: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			reviewCalls++
			return loopreview.Result{}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionBlocked {
		t.Fatalf("Action = %q, want %q", result.Action, ActionBlocked)
	}
	if reviewCalls != 0 {
		t.Fatalf("review calls = %d, want 0 for needs-human dispatch result", reviewCalls)
	}
	if result.DispatchResult == nil || result.DispatchResult.Status != guardrails.StatusNeedsHuman {
		t.Fatalf("dispatch result = %#v", result.DispatchResult)
	}
	if len(result.RecoveryAttempts) != 1 || result.RecoveryAttempts[0].Status != guardrails.StatusNeedsHuman {
		t.Fatalf("recovery attempts = %#v, want needs-human", result.RecoveryAttempts)
	}
	if !strings.Contains(result.Report, "BLOCKED: recovery review needs-human") ||
		!strings.Contains(result.Report, "harvested from hung/killed worker") {
		t.Fatalf("report missing needs-human harvest evidence:\n%s", result.Report)
	}
}

func TestRecoverRetryErrorPreservesPartialDispatchReport(t *testing.T) {
	repo := t.TempDir()

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
			}, nil
		},
		Dispatch: func(context.Context, DispatchOptions) (DispatchResult, error) {
			return DispatchResult{
				Issue:  103,
				Status: "failed",
				Report: recoverReport(103, 654),
			}, fmt.Errorf("dispatch failed after worker report")
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionBlocked {
		t.Fatalf("Action = %q, want %q", result.Action, ActionBlocked)
	}
	if result.DispatchResult == nil || result.DispatchResult.Report == nil {
		t.Fatalf("partial dispatch result missing report: %#v", result.DispatchResult)
	}
	if result.DispatchResult.Report.Model != "recover-model-103" ||
		result.DispatchResult.Report.Usage.TotalTokens == nil ||
		*result.DispatchResult.Report.Usage.TotalTokens != 654 {
		t.Fatalf("partial dispatch report not preserved: %#v", result.DispatchResult.Report)
	}
	if len(result.RecoveryAttempts) != 2 ||
		result.RecoveryAttempts[0].Strategy != AttemptStrategySameConfig ||
		result.RecoveryAttempts[1].Strategy != AttemptStrategyUpgradedConfig {
		t.Fatalf("recovery attempts = %#v", result.RecoveryAttempts)
	}
	for _, want := range []string{
		"RETRY: dispatching issue #103 attempt 2",
		"Recovery strategy: same_config",
		"RETRY: dispatching issue #103 attempt 3",
		"Recovery strategy: upgraded_config",
		"BLOCKED: retry limit reached",
	} {
		if !strings.Contains(result.Report, want) {
			t.Fatalf("report missing %q:\n%s", want, result.Report)
		}
	}
}

func TestRecoverHungRetriesSameConfigAndExhaustsBudget(t *testing.T) {
	repo := t.TempDir()
	attempts := []state.Attempt{
		recoverAttempt(repo, 1, "job-103-1", "failed", "first error"),
		recoverAttempt(repo, 2, "job-103-2", StatusHung, "reason=hung: worker stalled"),
	}
	var dispatched []DispatchOptions
	var records []AttemptRecord

	result, err := Run(context.Background(), Options{
		RepoPath:       repo,
		IssueNumber:    103,
		IssueTitle:     "Implement recover",
		RunID:          "run-test",
		MaxAttempts:    3,
		BackoffSeconds: []int{0},
		Provider:       "codex",
		Model:          "gpt-5",
		Effort:         "high",
		UpgradedModel:  "gpt-5.5",
		UpgradedEffort: "xhigh",
		Now:            fixedRecoverTime(),
	}, Deps{
		GitHub: func(string) PullRequestReader { return &recoverFakeGitHub{} },
		LoadAttempts: func(string, string) ([]state.Attempt, error) {
			return append([]state.Attempt(nil), attempts...), nil
		},
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
		Dispatch: func(_ context.Context, opts DispatchOptions) (DispatchResult, error) {
			dispatched = append(dispatched, opts)
			return DispatchResult{
				OK:       false,
				Issue:    opts.IssueNumber,
				Branch:   opts.Branch,
				RunID:    opts.RunID,
				Status:   StatusHung,
				ExitCode: -1,
				LogBytes: 12,
			}, fmt.Errorf("reason=hung: worker stalled again")
		},
		RecordAttempt: func(_ string, _ string, record AttemptRecord) error {
			records = append(records, record)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Action != ActionBlocked {
		t.Fatalf("Action = %q, want %q", result.Action, ActionBlocked)
	}
	if len(dispatched) != 1 {
		t.Fatalf("dispatch calls = %#v, want one", dispatched)
	}
	if dispatched[0].Attempt != 3 || dispatched[0].Model != "gpt-5" || dispatched[0].Effort != "high" {
		t.Fatalf("hung retry dispatch = %#v, want same-config final attempt", dispatched[0])
	}
	if len(result.RecoveryAttempts) != 2 {
		t.Fatalf("recovery attempts = %#v, want hung attempt plus needs-human escalation", result.RecoveryAttempts)
	}
	if result.RecoveryAttempts[0].Strategy != AttemptStrategySameConfig || result.RecoveryAttempts[0].Status != StatusHung {
		t.Fatalf("hung recovery record = %#v, want same_config/hung", result.RecoveryAttempts[0])
	}
	if result.RecoveryAttempts[1].Status != guardrails.StatusNeedsHuman || !strings.Contains(result.RecoveryAttempts[1].Error, "reason=hung") {
		t.Fatalf("needs-human escalation = %#v, want reason=hung", result.RecoveryAttempts[1])
	}
	if len(records) != 2 || records[0].Status != StatusHung || records[1].Status != guardrails.StatusNeedsHuman {
		t.Fatalf("recorded attempts = %#v, want hung then needs-human", records)
	}
	for _, want := range []string{
		"RETRY: dispatching issue #103 attempt 3",
		"Recovery strategy: same_config",
		"Reason: reason=hung",
		"BLOCKED: retry limit reached",
	} {
		if !strings.Contains(result.Report, want) {
			t.Fatalf("report missing %q:\n%s", want, result.Report)
		}
	}
	if strings.Contains(result.Report, "upgraded_config") || strings.Contains(result.Report, "gpt-5.5") || strings.Contains(result.Report, "xhigh") {
		t.Fatalf("hung recovery report unexpectedly upgraded config:\n%s", result.Report)
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

type recordingRecoveryProgressRecorder struct {
	observations []progress.Observation
}

func (r *recordingRecoveryProgressRecorder) Emit(_ context.Context, observation progress.Observation) (progress.EmitResult, error) {
	r.observations = append(r.observations, observation)
	return progress.EmitResult{Emitted: true}, nil
}

func (r *recordingRecoveryProgressRecorder) Terminal(_ context.Context, observation progress.Observation) (progress.EmitResult, error) {
	observation.Terminal = true
	r.observations = append(r.observations, observation)
	return progress.EmitResult{Emitted: true}, nil
}

func (r *recordingRecoveryProgressRecorder) hasKnown(known string) bool {
	for _, observation := range r.observations {
		if observation.KnownState == known {
			return true
		}
	}
	return false
}

func (r *recordingRecoveryProgressRecorder) hasTerminal(known string) bool {
	for _, observation := range r.observations {
		if observation.Terminal && observation.KnownState == known {
			return true
		}
	}
	return false
}

func recoverReport(issue int, totalTokens int64) *reporter.Report {
	started := fixedRecoverTime().Add(time.Duration(issue) * time.Second)
	return &reporter.Report{
		Role:        reporter.RoleWorker,
		Provider:    fmt.Sprintf("recover-provider-%d", issue),
		Model:       fmt.Sprintf("recover-model-%d", issue),
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "high",
		Permission:  reporter.PermissionWrite,
		Action:      fmt.Sprintf("implement issue #%d", issue),
		ExitCode:    0,
		StartedAt:   started.Format(time.RFC3339),
		EndedAt:     started.Add(time.Second).Format(time.RFC3339),
		DurationMS:  1000,
		Usage: reporter.Usage{
			TotalTokens: &totalTokens,
		},
		Verified: true,
	}
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
