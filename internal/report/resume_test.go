package report

import (
	"strings"
	"testing"
)

func TestRenderResumeTextIncludesDocumentedSections(t *testing.T) {
	runID := "run-20260626T120000Z-wave"
	text := RenderResumeText(ResumeReport{
		Version:     1,
		Repo:        "owner/repo",
		BaseBranch:  "main",
		RunID:       &runID,
		RunNote:     "requested run",
		GeneratedAt: "2026-06-26T12:00:00Z",
		GitHub: ResumeGitHubSnapshot{
			OpenIssueCount: 2,
			OpenPRCount:    1,
		},
		Local: ResumeLocalState{
			AttemptCount: 3,
			EventCount:   4,
			RunCount:     2,
		},
		Thresholds: ResumeThresholds{
			HeartbeatFreshSeconds: 30,
			StaleAfterSeconds:     120,
			HungAfterSeconds:      300,
		},
		Issues: []ResumeIssue{
			{
				Issue:          81,
				Title:          "Ready issue",
				State:          "OPEN",
				Classification: "ready",
				ActionKind:     "ready",
				Action:         "ready to dispatch; no open PR or live local attempt found",
				Evidence:       []string{"PR: none open", "attempt: none"},
			},
			{
				Issue:          82,
				Title:          "Blocked issue",
				State:          "OPEN",
				Labels:         []string{"needs-human"},
				Classification: "needs-inspection",
				ActionKind:     "blocked",
				Action:         "blocked by labels: needs-human",
				Evidence:       []string{"PR: none open", "attempt: none"},
			},
		},
		RunDecisions: []ResumeRunDecision{
			{
				RunID:          "run-child",
				ParentRunID:    "run-parent",
				Role:           "child",
				Issue:          81,
				Status:         "running",
				Classification: "interrupted-child",
				ActionKind:     "ready",
				Action:         "safe to run recover for issue #81 with run run-child; guardrails will bound retry",
				Evidence:       []string{"run state: running", "recovery: .loopcoder/runs/run-child/recovery/job-81-context.md"},
			},
		},
	})

	for _, want := range []string{
		"RESUME REPORT",
		"Repo: owner/repo",
		"RunId: run-20260626T120000Z-wave (requested run)",
		"GitHub snapshot: open issues=2, open PRs=1",
		"Local state: attempts=3, events=4, runs=2",
		"Thresholds: heartbeat fresh <= 30s, stale progress > 120s, hung progress > 300s",
		"Issues",
		"classification: ready",
		"Run recovery decisions",
		"run-child (child)",
		"safe to run recover for issue #81",
		"Next ready actions",
		"Blocked / awaiting human input",
		"Safety",
		"resume is read-only: no dispatch, no merge, no push, no GitHub mutation, and no run-state writes were attempted.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered text missing %q:\n%s", want, text)
		}
	}
}
