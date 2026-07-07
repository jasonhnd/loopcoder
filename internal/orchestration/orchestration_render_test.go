package orchestration

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	reportpkg "github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/reporter"
)

func TestRenderTickTextGoldenBytes(t *testing.T) {
	report := TickReport{
		Version:       TickReportVersion,
		Repo:          "owner/repo",
		RepoPath:      "/repo",
		BaseBranch:    "main",
		PreProdBranch: "pre-prod",
		RunID:         "run-render-golden",
		Status:        TickStatusNoReadyWork,
		StopReason:    TickStopNoReadyWork,
		StartedAt:     "2026-07-05T00:00:00Z",
		FinishedAt:    "2026-07-05T00:00:01Z",
		Compile: compiler.Report{
			PlanApprovalRequired: true,
			Summary: compiler.Summary{
				CreatedCount:   1,
				UpdatedCount:   2,
				UnchangedCount: 3,
				ClosedCount:    4,
			},
		},
		ReadySet: reportpkg.ReadySetReport{
			Ready:   []reportpkg.ReadyIssue{{Issue: 7}},
			Blocked: []reportpkg.BlockedIssue{{Issue: 8}},
		},
	}

	const want = "TICK\n" +
		"Repo: owner/repo\n" +
		"Base branch: main\n" +
		"Pre-prod branch: pre-prod\n" +
		"RunId: run-render-golden\n" +
		"Status: no-ready-work\n" +
		"Stop reason: no-ready-work\n" +
		"Started at: 2026-07-05T00:00:00Z\n" +
		"Finished at: 2026-07-05T00:00:01Z\n" +
		"\n" +
		"Compile\n" +
		"- created=1 updated=2 unchanged=3 closed=4 plan_approval_required=yes\n" +
		"\n" +
		"Ready set\n" +
		"- ready=1 blocked=1\n" +
		"\n" +
		"Pending promotion\n" +
		"- none\n" +
		"\n" +
		"Dispatch\n" +
		"- none\n" +
		"\n" +
		"Recoveries\n" +
		"- none\n" +
		"\n" +
		"Reviews\n" +
		"- none\n" +
		"\n" +
		"Risk gates\n" +
		"- none\n" +
		"\n" +
		"Pre-prod merges\n" +
		"- none\n" +
		"\n" +
		"Pre-prod health\n" +
		"- none\n" +
		"\n" +
		"Pre-prod reverts\n" +
		"- none\n" +
		"\n" +
		"Needs human\n" +
		"- none\n" +
		"\n" +
		"Failures\n" +
		"- none\n" +
		"\n" +
		"State\n" +
		"- not pushed\n" +
		"\n" +
		"Next\n" +
		"- No ready issues were dispatched in this pass.\n"
	if got := RenderTickText(report); got != want {
		t.Fatalf("RenderTickText drifted:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderPromoteTextGoldenBytes(t *testing.T) {
	report := promoteGoldenReport()

	const want = "PROMOTE\n" +
		"Repo path: /repo\n" +
		"Pre-prod branch: pre-prod\n" +
		"Main branch: main\n" +
		"Gate: auto\n" +
		"RunId: run-promote-golden\n" +
		"Status: succeeded\n" +
		"Started at: 2026-07-05T00:00:00Z\n" +
		"Finished at: 2026-07-05T00:00:02Z\n" +
		"\n" +
		"Kicked back\n" +
		"- none\n" +
		"\n" +
		"Needs human\n" +
		"- none\n" +
		"\n" +
		"Toggle inventory\n" +
		"- flip_on: none\n" +
		"- leave_dark: none\n" +
		"\n" +
		"Promoted\n" +
		"- pre-prod -> main succeeded\n" +
		"  sha: abc123\n" +
		"  url: https://example.com/promotion\n" +
		"\n" +
		"Pre-prod sync\n" +
		"- main -> pre-prod succeeded\n" +
		"\n" +
		"State\n" +
		"- branch=state/pre-prod remote=origin committed=true pushed=true files=0\n"
	if got := RenderPromoteText(report); got != want {
		t.Fatalf("RenderPromoteText drifted:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestMarshalPromoteJSONGoldenBytes(t *testing.T) {
	got, err := MarshalPromoteJSON(promoteGoldenReport())
	if err != nil {
		t.Fatalf("MarshalPromoteJSON returned error: %v", err)
	}

	const want = `{
  "version": 1,
  "repo_path": "/repo",
  "run_id": "run-promote-golden",
  "pre_prod_branch": "pre-prod",
  "main_branch": "main",
  "gate": "auto",
  "status": "succeeded",
  "started_at": "2026-07-05T00:00:00Z",
  "finished_at": "2026-07-05T00:00:02Z",
  "kicked_back": [],
  "needs_human": [],
  "promoted": {
    "pre_prod_branch": "pre-prod",
    "main_branch": "main",
    "sha": "abc123",
    "url": "https://example.com/promotion",
    "status": "succeeded"
  },
  "sync": {
    "pre_prod_branch": "pre-prod",
    "main_branch": "main",
    "status": "succeeded"
  },
  "state_push": {
    "branch": "state/pre-prod",
    "remote": "origin",
    "committed": true,
    "pushed": true,
    "files": []
  },
  "summary": {
    "kicked_back_count": 0,
    "promoted_count": 0,
    "needs_human_count": 0,
    "failure_count": 0
  }
}
`
	if string(got) != want {
		t.Fatalf("MarshalPromoteJSON drifted:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderDispatchWaveTextGoldenBytes(t *testing.T) {
	report := dispatchWaveGoldenReport()

	const want = "DISPATCH WAVE\n" +
		"Repo: owner/repo\n" +
		"Base branch: main\n" +
		"RunId: run-wave-golden\n" +
		"Issues requested: #7, #8\n" +
		"Issues dispatched: 1\n" +
		"Issues skipped: 0\n" +
		"Issues needs-human: 1\n" +
		"Started at: 2026-07-05T00:00:00Z\n" +
		"Finished at: 2026-07-05T00:00:03Z\n" +
		"\n" +
		"Results\n" +
		"- #7 succeeded\n" +
		"  branch: loop/issue-7\n" +
		"  pr: https://github.com/owner/repo/pull/7\n" +
		"  attestation: provider=codex model=gpt-5(parsed) effort=high permission=write duration=1.5s tokens input=10 output=20 total=30 verified=true\n" +
		"  attempt: .loopcoder/runs/run-wave-golden/workers/job-7.attempt.json\n" +
		"  recovery: .loopcoder/runs/run-wave-golden/recovery/job-7-context.md\n" +
		"- #8 needs-human\n" +
		"  error: guardrail frozen\n" +
		"\n" +
		"Next\n" +
		"- Verify successful PRs before calling them merge-eligible.\n" +
		"- Recover failed attempts before retrying the issue.\n" +
		"- Run resume after human review, merge, or interruption.\n"
	if got := RenderDispatchWaveText(report); got != want {
		t.Fatalf("RenderDispatchWaveText drifted:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderDispatchWaveIssueCompletionGoldenBytes(t *testing.T) {
	result := dispatchWaveGoldenReport().Results[0]
	const pretty = "worker attestation\n  provider codex\n"

	const want = "DISPATCH WAVE WORKER #7 succeeded\n" +
		"branch: loop/issue-7\n" +
		"pr: https://github.com/owner/repo/pull/7\n" +
		"attempt: .loopcoder/runs/run-wave-golden/workers/job-7.attempt.json\n" +
		"recovery: .loopcoder/runs/run-wave-golden/recovery/job-7-context.md\n" +
		"worker attestation\n" +
		"  provider codex\n" +
		"\n"
	if got := RenderDispatchWaveIssueCompletion(result, pretty); got != want {
		t.Fatalf("RenderDispatchWaveIssueCompletion drifted:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestStateFilesDoNotCallPresentationHelpers(t *testing.T) {
	forbidden := map[string]bool{
		"MarshalTickJSON":                   true,
		"RenderTickText":                    true,
		"renderTickConfiguredEvidence":      true,
		"renderTickRenderedArtifacts":       true,
		"renderTickIssueSection":            true,
		"formatTickEvidenceValue":           true,
		"formatTickPendingPromotionTarget":  true,
		"tickYesNo":                         true,
		"MarshalPromoteJSON":                true,
		"RenderPromoteText":                 true,
		"renderPromoteGoNoGoPanel":          true,
		"RenderDispatchWaveText":            true,
		"RenderDispatchWaveIssueCompletion": true,
		"formatDispatchWaveReport":          true,
		"formatDispatchWaveDuration":        true,
		"formatDispatchWaveToken":           true,
		"dispatchWaveCounts":                true,
		"formatIssueList":                   true,
	}
	for _, path := range []string{"tick.go", "promote.go", "dispatch_wave.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name := callName(call.Fun); forbidden[name] {
				t.Fatalf("%s state path calls presentation helper %s", path, name)
			}
			return true
		})
	}
}

func promoteGoldenReport() PromoteReport {
	return PromoteReport{
		Version:       PromoteReportVersion,
		RepoPath:      "/repo",
		RunID:         "run-promote-golden",
		PreProdBranch: "pre-prod",
		MainBranch:    "main",
		Gate:          GateAuto,
		Status:        PromoteStatusSucceeded,
		StartedAt:     "2026-07-05T00:00:00Z",
		FinishedAt:    "2026-07-05T00:00:02Z",
		Promoted: PromoteMainResult{
			PreProdBranch: "pre-prod",
			MainBranch:    "main",
			SHA:           "abc123",
			URL:           "https://example.com/promotion",
			Status:        PromoteStatusSucceeded,
		},
		Sync: PromoteSyncResult{
			PreProdBranch: "pre-prod",
			MainBranch:    "main",
			Status:        PromoteStatusSucceeded,
		},
		StatePush: &PromoteStatePush{
			Branch:    "state/pre-prod",
			Remote:    "origin",
			Committed: true,
			Pushed:    true,
		},
	}
}

func dispatchWaveGoldenReport() DispatchWaveReport {
	inputTokens := int64(10)
	outputTokens := int64(20)
	totalTokens := int64(30)
	return DispatchWaveReport{
		Repo:            "owner/repo",
		RepoPath:        "/repo",
		BaseBranch:      "main",
		RunID:           "run-wave-golden",
		IssuesRequested: []int{7, 8},
		StartedAt:       "2026-07-05T00:00:00Z",
		FinishedAt:      "2026-07-05T00:00:03Z",
		Results: []DispatchWaveIssueResult{
			{
				Issue:               7,
				Status:              DispatchWaveStatusSucceeded,
				Branch:              "loop/issue-7",
				PR:                  "https://github.com/owner/repo/pull/7",
				AttemptPath:         ".loopcoder/runs/run-wave-golden/workers/job-7.attempt.json",
				RecoveryContextPath: ".loopcoder/runs/run-wave-golden/recovery/job-7-context.md",
				Report: &reporter.Report{
					Provider:    "codex",
					Model:       "gpt-5",
					ModelSource: reporter.ModelSourceParsed,
					Effort:      "high",
					Permission:  reporter.PermissionWrite,
					DurationMS:  1500,
					Usage: reporter.Usage{
						InputTokens:  &inputTokens,
						OutputTokens: &outputTokens,
						TotalTokens:  &totalTokens,
					},
					Verified: true,
				},
			},
			{
				Issue:  8,
				Status: DispatchWaveStatusNeedsHuman,
				Error:  "guardrail frozen",
			},
		},
	}
}

func callName(expr ast.Expr) string {
	switch fun := expr.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		parts := []string{}
		for current := ast.Expr(fun); current != nil; {
			switch item := current.(type) {
			case *ast.SelectorExpr:
				parts = append([]string{item.Sel.Name}, parts...)
				current = item.X
			case *ast.Ident:
				parts = append([]string{item.Name}, parts...)
				current = nil
			default:
				current = nil
			}
		}
		return strings.Join(parts, ".")
	default:
		return ""
	}
}
