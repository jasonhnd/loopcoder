package orchestration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestComputeResumeClassifiesGitHubPRAndLocalState(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	pid := 1234
	reader := fakeReader{
		repo: "owner/repo",
		issues: []gh.Issue{
			{Number: 1, Title: "Ready", State: "OPEN"},
			{Number: 4, Title: "Fixing", State: "OPEN"},
			{Number: 5, Title: "Gated", State: "OPEN"},
			{Number: 6, Title: "In review", State: "OPEN"},
			{Number: 7, Title: "Adopt PR", State: "OPEN"},
			{Number: 8, Title: "Running", State: "OPEN"},
			{Number: 9, Title: "Stale", State: "OPEN"},
			{Number: 10, Title: "Hung", State: "OPEN"},
			{Number: 11, Title: "Orphaned", State: "OPEN"},
			{Number: 12, Title: "Blocked label", State: "OPEN", Labels: []gh.Label{{Name: "needs-human"}}},
		},
		views: map[int]gh.Issue{
			2: {Number: 2, Title: "Closed completed", State: "CLOSED", StateReason: "COMPLETED"},
			3: {
				Number:                         3,
				Title:                          "Closed by PR ref",
				State:                          "CLOSED",
				StateReason:                    "NOT_PLANNED",
				ClosedByPullRequestsReferences: []gh.PullRequestReference{{Number: 33}},
			},
		},
		prs: []gh.PullRequest{
			{Number: 40, Title: "Fix #4", HeadRefName: "loop/issue-4"},
			{Number: 50, Title: "Fix #5", HeadRefName: "loop/issue-5"},
			{Number: 60, Title: "Fix #6", HeadRefName: "loop/issue-6"},
			{Number: 70, Title: "Fix #7", HeadRefName: "loop/issue-7"},
		},
		checks: map[int][]gh.Check{
			40: {{Name: "go", Bucket: "fail"}},
			50: {{Name: "go", Bucket: "pending"}},
			60: {{Name: "go", Bucket: "pass"}},
			70: {{Name: "go", Bucket: "pass"}},
		},
	}

	result, err := ComputeResume(context.Background(), ResumeOptions{
		Reader:     reader,
		RepoPath:   "C:/repo",
		BaseBranch: "main",
		RunID:      "run-test",
		Attempts: []state.Attempt{
			{
				JobID:   "job-2",
				Issue:   2,
				Attempt: 1,
				Status:  "succeeded",
				Phase:   "done",
				Branch:  "loop/issue-2",
				Path:    "C:/repo/.loopcoder/runs/run-test/workers/job-2.attempt.json",
			},
			{
				JobID:   "job-3",
				Issue:   3,
				Attempt: 1,
				Status:  "succeeded",
				Branch:  "loop/issue-3",
			},
			{
				JobID:   "job-7",
				Issue:   7,
				Attempt: 1,
				Status:  "running",
				Branch:  "loop/issue-7",
			},
			{
				JobID:          "job-8",
				Issue:          8,
				Attempt:        1,
				Status:         "running",
				PID:            &pid,
				HeartbeatAt:    "2026-06-26T11:59:50Z",
				LastProgressAt: "2026-06-26T11:59:50Z",
				Branch:         "loop/issue-8",
			},
			{
				JobID:          "job-9",
				Issue:          9,
				Attempt:        1,
				Status:         "running",
				HeartbeatAt:    "2026-06-26T11:59:55Z",
				LastProgressAt: "2026-06-26T11:57:00Z",
				Branch:         "loop/issue-9",
			},
			{
				JobID:          "job-10",
				Issue:          10,
				Attempt:        1,
				Status:         "running",
				LastProgressAt: "2026-06-26T11:50:00Z",
				Branch:         "loop/issue-10",
			},
			{
				JobID:       "job-11",
				Issue:       11,
				Attempt:     1,
				Status:      "running",
				HeartbeatAt: "2026-06-26T11:59:00Z",
				Branch:      "loop/issue-11",
			},
		},
		EventCount:   2,
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(got int) bool { return got == pid },
		Now:          now,
	})
	if err != nil {
		t.Fatalf("ComputeResume returned error: %v", err)
	}

	if result.Repo != "owner/repo" || result.GeneratedAt != "2026-06-26T12:00:00Z" {
		t.Fatalf("metadata mismatch: %#v", result)
	}
	if result.GitHub.OpenIssueCount != 10 || result.GitHub.OpenPRCount != 4 {
		t.Fatalf("github counts = %#v", result.GitHub)
	}
	if result.Local.AttemptCount != 7 || result.Local.EventCount != 2 {
		t.Fatalf("local counts = %#v", result.Local)
	}

	classes := map[int]string{}
	actions := map[int]string{}
	evidence := map[int]string{}
	for _, issue := range result.Issues {
		classes[issue.Issue] = issue.Classification
		actions[issue.Issue] = issue.ActionKind
		evidence[issue.Issue] = strings.Join(issue.Evidence, "\n")
	}

	wantClasses := map[int]string{
		1:  "ready",
		2:  "done",
		3:  "done",
		4:  "fixing",
		5:  "gated",
		6:  "in-review",
		7:  "adopt-PR",
		8:  "running",
		9:  "stale",
		10: "hung",
		11: "orphaned",
		12: "needs-inspection",
	}
	for issue, want := range wantClasses {
		if classes[issue] != want {
			t.Fatalf("issue #%d classification = %q, want %q; all=%#v", issue, classes[issue], want, classes)
		}
	}
	if actions[2] != "none" {
		t.Fatalf("issue #2 action kind = %q, want none", actions[2])
	}
	if actions[10] != "ready" || actions[11] != "ready" {
		t.Fatalf("hung/orphaned action kinds = %q/%q, want ready/ready", actions[10], actions[11])
	}
	if !strings.Contains(evidence[2], "attempt: job=job-2") || !strings.Contains(evidence[2], "branch: loop/issue-2") {
		t.Fatalf("done issue evidence missing attempt/branch lines:\n%s", evidence[2])
	}
	if !strings.Contains(evidence[3], "closing PRs: #33") {
		t.Fatalf("closing ref done evidence missing:\n%s", evidence[3])
	}
}

func TestComputeResumeMarksGuardrailFrozenIssueBlocked(t *testing.T) {
	repo := t.TempDir()
	if _, err := guardrails.RecordDecision(repo, guardrails.Decision{
		Guardrail:       guardrails.GuardrailCircuitBreaker,
		Enabled:         true,
		Allowed:         false,
		Status:          guardrails.StatusNeedsHuman,
		Reason:          "guardrails.circuit_breaker.max_no_progress_attempts",
		DeliveryScopeID: "main:7",
		BaseBranch:      "main",
		RunID:           "run-test",
		Issue:           7,
		Issues:          []int{7},
		Observed: guardrails.Observed{
			NoProgressAttempts: 2,
		},
		LastMaterialProgressAt: "2026-06-27T12:00:00Z",
		DecisionAt:             time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("RecordDecision frozen ledger: %v", err)
	}

	result, err := ComputeResume(context.Background(), ResumeOptions{
		Reader: fakeReader{
			repo:   "owner/repo",
			issues: []gh.Issue{{Number: 7, Title: "Frozen", State: "OPEN"}},
		},
		RepoPath:     repo,
		BaseBranch:   "main",
		RunID:        "run-test",
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(int) bool { return false },
		Now:          time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ComputeResume returned error: %v", err)
	}
	if len(result.Issues) != 1 {
		t.Fatalf("issues = %#v, want one", result.Issues)
	}
	issue := result.Issues[0]
	if issue.Classification != "guardrail-frozen" || issue.ActionKind != "blocked" {
		t.Fatalf("resume issue = %#v, want guardrail-frozen blocked", issue)
	}
	if !strings.Contains(issue.Action, "guardrails.circuit_breaker.max_no_progress_attempts") {
		t.Fatalf("action missing circuit reason:\n%s", issue.Action)
	}
	evidence := strings.Join(issue.Evidence, "\n")
	for _, want := range []string{"guardrail: guardrails.circuit_breaker.max_no_progress_attempts", "no-progress waves=0, attempts=2", "last material progress=2026-06-27T12:00:00Z"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("evidence missing %q:\n%s", want, evidence)
		}
	}
}

func TestComputeResumeClassifiesInterruptedChildRunRecoveryDecision(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	result, err := ComputeResume(context.Background(), ResumeOptions{
		Reader: fakeReader{
			repo:   "owner/repo",
			issues: []gh.Issue{{Number: 650, Title: "Nested recovery", State: "OPEN"}},
		},
		RepoPath:   "C:/repo",
		BaseBranch: "main",
		RunID:      "run-parent",
		RunTree: []report.ResumeRun{
			{RunID: "run-parent", ChildRunIDs: []string{"run-child"}, Status: "interrupted", EventCount: 1},
			{RunID: "run-child", ParentRunID: "run-parent", Status: "abandoned", EventCount: 2},
		},
		Attempts: []state.Attempt{{
			JobID:               "job-child-1",
			Issue:               650,
			Attempt:             1,
			RunID:               "run-child",
			ParentRunID:         "run-parent",
			Status:              "running",
			Phase:               "codex_exec",
			Branch:              "loop/issue-650",
			RecoveryContextPath: ".loopcoder/runs/run-child/recovery/job-child-1-context.md",
			HeartbeatAt:         "2026-07-09T11:50:00Z",
			LastProgressAt:      "2026-07-09T11:50:00Z",
		}},
		EventCount:   3,
		Thresholds:   config.Default().Resilience.Worker,
		ProcessAlive: func(int) bool { return false },
		Now:          now,
	})
	if err != nil {
		t.Fatalf("ComputeResume returned error: %v", err)
	}
	if len(result.RunTree) != 2 {
		t.Fatalf("RunTree = %#v, want parent and child", result.RunTree)
	}
	if len(result.Issues) != 1 {
		t.Fatalf("Issues = %#v, want one", result.Issues)
	}
	issue := result.Issues[0]
	if issue.RunID != "run-child" || issue.ParentRunID != "run-parent" {
		t.Fatalf("issue run relationship = %#v", issue)
	}
	if issue.Classification != "orphaned" || issue.ActionKind != "ready" {
		t.Fatalf("classification/action = %q/%q, want orphaned/ready", issue.Classification, issue.ActionKind)
	}
	if !issue.RecoveryDecision.SafeToResume || issue.RecoveryDecision.NeedsHuman {
		t.Fatalf("recovery decision = %#v, want safe without needs-human", issue.RecoveryDecision)
	}
	if issue.RecoveryDecision.RecoveryContextPath != ".loopcoder/runs/run-child/recovery/job-child-1-context.md" ||
		issue.RecoveryDecision.Branch != "loop/issue-650" {
		t.Fatalf("recovery decision missing context/branch: %#v", issue.RecoveryDecision)
	}
	if !strings.Contains(strings.Join(issue.Evidence, "\n"), "run: run-child (parent run-parent)") {
		t.Fatalf("evidence missing parent/child run line: %#v", issue.Evidence)
	}
}
