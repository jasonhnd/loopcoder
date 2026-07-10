package report

import (
	"encoding/json"
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
	})

	for _, want := range []string{
		"RESUME REPORT",
		"Repo: owner/repo",
		"RunId: run-20260626T120000Z-wave (requested run)",
		"GitHub snapshot: open issues=2, open PRs=1",
		"Local state: attempts=3, events=4",
		"Thresholds: heartbeat fresh <= 30s, stale progress > 120s, hung progress > 300s",
		"Issues",
		"classification: ready",
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

func TestMarshalResumeJSONIncludesRunTreeAndRecoveryDecision(t *testing.T) {
	runID := "run-20260709T000000Z-wave"
	data, err := MarshalResumeJSON(ResumeReport{
		Version:     1,
		Repo:        "owner/repo",
		BaseBranch:  "main",
		RunID:       &runID,
		GeneratedAt: "2026-07-09T00:00:00Z",
		RunTree: ResumeRunTree{
			RootRunID: runID,
			Nodes: []ResumeRunTreeNode{{
				RunID:       runID,
				State:       "running",
				Interrupted: true,
				RecoveryDecision: &ResumeRecoveryDecision{
					Outcome:      "resume",
					Action:       "inspect active run state and resume safe unfinished work",
					SafeToResume: true,
				},
			}},
		},
		Issues: []ResumeIssue{{
			Issue:          650,
			Title:          "Recovery",
			State:          "OPEN",
			Classification: "guardrail-frozen",
			ActionKind:     "blocked",
			Action:         "needs human",
			RecoveryDecision: &ResumeRecoveryDecision{
				Outcome:             "needs-human",
				Action:              "needs human",
				Reason:              "guardrail-frozen",
				NeedsHuman:          true,
				RecoveryContextPath: ".loopcoder/runs/run/recovery/job-context.md",
			},
		}},
	})
	if err != nil {
		t.Fatalf("MarshalResumeJSON returned error: %v", err)
	}

	var got struct {
		RunTree struct {
			Summary struct {
				InterruptedRuns int `json:"interrupted_runs"`
				NeedsHumanRuns  int `json:"needs_human_runs"`
			} `json:"summary"`
			Nodes []struct {
				RecoveryDecision struct {
					SafeToResume bool `json:"safe_to_resume"`
				} `json:"recovery_decision"`
			} `json:"nodes"`
		} `json:"run_tree"`
		Issues []struct {
			RecoveryDecision struct {
				NeedsHuman bool   `json:"needs_human"`
				Outcome    string `json:"outcome"`
			} `json:"recovery_decision"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json unmarshal failed: %v\n%s", err, string(data))
	}
	if got.RunTree.Summary.InterruptedRuns != 1 || got.RunTree.Summary.NeedsHumanRuns != 0 {
		t.Fatalf("run tree summary = %#v", got.RunTree.Summary)
	}
	if len(got.RunTree.Nodes) != 1 || !got.RunTree.Nodes[0].RecoveryDecision.SafeToResume {
		t.Fatalf("run tree nodes = %#v", got.RunTree.Nodes)
	}
	if len(got.Issues) != 1 || !got.Issues[0].RecoveryDecision.NeedsHuman || got.Issues[0].RecoveryDecision.Outcome != "needs-human" {
		t.Fatalf("issue recovery decision = %#v", got.Issues)
	}
}
