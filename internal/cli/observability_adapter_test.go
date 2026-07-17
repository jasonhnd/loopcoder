package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/audit"
	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/detachedrun"
	"github.com/jasonhnd/loopcoder/internal/observability"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func TestObservabilityJSONLCommandAdaptersEmitMachineRecordsOnly(t *testing.T) {
	clearGitSelectionEnvForFixture(t)
	t.Setenv("GH_REPO", "")
	repoState := t.TempDir()
	record := validDispatchReport()
	record.WorkID = "run-jsonl"
	result := validDispatchResult(record)
	result.AttemptPath = filepath.Join(string(os.PathSeparator), "tmp", "repo", ".loopcoder", "runs", "run-jsonl", "workers", "job-864.attempt.json")
	result.RunID = "run-jsonl"
	result.Issue = 864
	result.Status = "succeeded"
	seedObservabilityRun(t, repoState, result.RunID, []observabilityAttempt{{Issue: 864, JobID: "job-864", Status: "succeeded", Report: record}})
	dispatchRepo := t.TempDir()
	nestedRepo := t.TempDir()
	planPath := writeNestedPlanFixture(t, nestedRepo, []orchestration.ChildRunPlan{
		nestedPlanItem("alpha", 864, nil, true, "write", nil),
	}, 1)
	detached := seedDetachedRecoverObservabilityFixture(t, repoState)

	cases := []struct {
		name       string
		args       []string
		deps       Deps
		allowExit1 bool
	}{
		{
			name: "dispatch",
			args: []string{"dispatch", "--repo", dispatchRepo, "--issue-number", "864", "--issue-title", "Observability", "--run-id", result.RunID, "--format", "jsonl"},
			deps: Deps{
				Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
					return result, nil
				},
			},
		},
		{
			name: "dispatch-wave",
			args: []string{"dispatch-wave", "--repo", repoState, "--issue-numbers", "864", "--run-id", result.RunID, "--format", "jsonl", "--no-pretty"},
			deps: Deps{
				ComputeReadySet: func(context.Context, orchestration.Options) (report.ReadySetReport, error) {
					runID := result.RunID
					return report.ReadySetReport{
						Version:     1,
						Repo:        "owner/repo",
						RepoPath:    repoState,
						BaseBranch:  "main",
						RunID:       &runID,
						GeneratedAt: "2026-07-14T00:00:00Z",
						Ready:       []report.ReadyIssue{{Issue: 864, Title: "Observability", Reason: "ready"}},
						Blocked:     []report.BlockedIssue{},
					}, nil
				},
				NewGitHubReader: func(string) orchestration.GitHubReader {
					return cliFakeReader{views: map[int]gh.Issue{
						864: {Number: 864, Title: "Observability", Body: "Body", State: "OPEN"},
					}}
				},
				Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
					if opts.IssueTitle != "Observability" || opts.IssueBody != "Body" {
						return worker.Result{}, fmt.Errorf("dispatch-wave used issue fields title=%q body=%q, want injected fake issue", opts.IssueTitle, opts.IssueBody)
					}
					return result, nil
				},
			},
		},
		{
			name: "tick",
			args: []string{"tick", "--repo", repoState, "--run-id", result.RunID, "--format", "jsonl", "--no-pretty"},
			deps: Deps{
				Tick: func(context.Context, orchestration.TickOptions) (orchestration.TickReport, error) {
					return observabilityTickReport(repoState, result.RunID, result), nil
				},
			},
		},
		{
			name: "audit",
			args: []string{"audit", "--repo", t.TempDir(), "--format", "jsonl"},
			deps: Deps{
				Audit: func(context.Context, audit.Options) (audit.Result, error) {
					out := audit.NewResult("repo-audit", []string{audit.LayerSAST}, audit.SeverityMedium)
					out.Report = &record
					return audit.Finalize(out), nil
				},
			},
		},
		{
			name: "recover",
			args: []string{"recover", "--repo", t.TempDir(), "--issue-number", "864", "--issue-title", "Observability", "--run-id", result.RunID, "--format", "jsonl"},
			deps: Deps{
				Recover: func(context.Context, recovery.Options) (recovery.Result, error) {
					return recovery.Result{
						Action: recovery.ActionRetry,
						DispatchResult: &recovery.DispatchResult{
							OK:          true,
							Issue:       864,
							RunID:       result.RunID,
							AttemptPath: result.AttemptPath,
							Status:      "succeeded",
							Report:      &record,
						},
					}, nil
				},
			},
		},
		{
			name: "detached-recover",
			args: []string{
				"recover", "--repo", repoState, "--run-id", detached.runID, "--detached",
				"--supervisor-owner", detached.owner, "--supervisor-generation", "1", "--supervisor-lease", detached.lease,
				"--format", "jsonl",
			},
			deps: Deps{
				StartDetachedDispatch: func(context.Context, []string, string) (int, error) {
					return 6161, nil
				},
			},
		},
		{
			name: "resume",
			args: []string{"resume", "--repo", repoState, "--run-id", result.RunID, "--format", "jsonl"},
			deps: Deps{
				NewGitHubReader: func(string) orchestration.GitHubReader {
					return cliFakeReader{issues: []gh.Issue{{Number: 864, Title: "Observability", State: "OPEN"}}}
				},
			},
		},
		{
			name:       "nested",
			args:       []string{"nested", "run", "--repo", nestedRepo, "--plan", planPath, "--format", "jsonl"},
			allowExit1: true,
			deps: Deps{
				Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
					nestedRecord := record
					nestedRecord.WorkID = opts.RunID
					nestedRecord.Issue = opts.IssueNumber
					nestedResult := result
					nestedResult.Issue = opts.IssueNumber
					nestedResult.RunID = opts.RunID
					nestedResult.Branch = opts.Branch
					nestedResult.AttemptPath = filepath.Join(nestedRepo, ".loopcoder", "runs", opts.RunID, "workers", "job-864.attempt.json")
					nestedResult.Report = &nestedRecord
					return nestedResult, nil
				},
			},
		},
		{
			name: "status",
			args: []string{"status", "--repo", repoState, "--run", result.RunID, "--format", "jsonl"},
		},
		{
			name: "report",
			args: []string{"report", "--repo", repoState, "--run", result.RunID, "--format", "jsonl"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			tc.deps.Now = fixedCLINow
			exitCode := RunWithDeps(tc.args, &stdout, &stderr, tc.deps)
			if exitCode != 0 && !(tc.allowExit1 && exitCode == 1) {
				t.Fatalf("RunWithDeps exit=%d stderr=%q stdout=%q", exitCode, stderr.String(), stdout.String())
			}
			records := assertCanonicalJSONLOnly(t, stdout.String(), tc.name)
			assertCanonicalSemantics(t, records, tc.name)
			if strings.Contains(stdout.String(), "warning:") || strings.Contains(stdout.String(), "OBSERVABILITY ") {
				t.Fatalf("machine stdout contains human/diagnostic text:\n%s", stdout.String())
			}
			if strings.Contains(stdout.String(), repoState) || strings.Contains(stdout.String(), string(os.PathSeparator)+".loopcoder"+string(os.PathSeparator)) {
				t.Fatalf("machine stdout leaked local path:\n%s", stdout.String())
			}
		})
	}
}

func TestObservabilityLegacyJSONKeepsSchemaAndEmbedsCanonicalDocument(t *testing.T) {
	repo := t.TempDir()
	dispatchRepo := t.TempDir()
	record := validDispatchReport()
	record.WorkID = "run-json"
	result := validDispatchResult(record)
	result.RunID = "run-json"
	result.Issue = 864
	result.Status = "succeeded"
	seedObservabilityRun(t, repo, result.RunID, []observabilityAttempt{{Issue: 864, JobID: "job-864", Status: "succeeded", Report: record}})

	cases := []struct {
		name         string
		args         []string
		deps         Deps
		wantSchema   string
		wantLegacy   string
		wantCommand  string
		allowExitOne bool
	}{
		{
			name:        "dispatch",
			args:        []string{"dispatch", "--repo", dispatchRepo, "--issue-number", "864", "--issue-title", "Observability", "--run-id", result.RunID, "--format", "json"},
			deps:        Deps{Dispatch: func(context.Context, worker.Options) (worker.Result, error) { return result, nil }},
			wantSchema:  "loopcoder.dispatch_result.v1",
			wantLegacy:  "run_id",
			wantCommand: "dispatch",
		},
		{
			name: "tick",
			args: []string{"tick", "--repo", repo, "--run-id", result.RunID, "--format", "json", "--no-pretty"},
			deps: Deps{Tick: func(context.Context, orchestration.TickOptions) (orchestration.TickReport, error) {
				return observabilityTickReport(repo, result.RunID, result), nil
			}},
			wantSchema:  "loopcoder.tick_result.v1",
			wantLegacy:  "run_id",
			wantCommand: "tick",
		},
		{
			name: "audit",
			args: []string{"audit", "--repo", t.TempDir(), "--format", "json"},
			deps: Deps{Audit: func(context.Context, audit.Options) (audit.Result, error) {
				out := audit.NewResult("repo-audit", []string{audit.LayerSAST}, audit.SeverityMedium)
				out.Report = &record
				return audit.Finalize(out), nil
			}},
			wantLegacy:  "verdict",
			wantCommand: "audit",
		},
		{
			name: "recover",
			args: []string{"recover", "--repo", repo, "--issue-number", "864", "--issue-title", "Observability", "--run-id", result.RunID, "--format", "json"},
			deps: Deps{Recover: func(context.Context, recovery.Options) (recovery.Result, error) {
				return recovery.Result{Action: recovery.ActionRetry, DispatchResult: &recovery.DispatchResult{OK: true, Issue: 864, RunID: result.RunID, AttemptPath: result.AttemptPath, Status: "succeeded", Report: &record}}, nil
			}},
			wantSchema:  "loopcoder.recover_result.v1",
			wantLegacy:  "action",
			wantCommand: "recover",
		},
		{
			name:        "status",
			args:        []string{"status", "--repo", repo, "--run", result.RunID, "--format", "json"},
			wantLegacy:  "rows",
			wantCommand: "status",
		},
		{
			name:        "report",
			args:        []string{"report", "--repo", repo, "--run", result.RunID, "--format", "json"},
			wantSchema:  "loopcoder.report_query.v1",
			wantLegacy:  "reports",
			wantCommand: "report",
		},
		{
			name: "resume",
			args: []string{"resume", "--repo", repo, "--run-id", result.RunID, "--format", "json"},
			deps: Deps{NewGitHubReader: func(string) orchestration.GitHubReader {
				return cliFakeReader{issues: []gh.Issue{{Number: 864, Title: "Observability", State: "OPEN"}}}
			}},
			wantLegacy:  "issues",
			wantCommand: "resume",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			tc.deps.Now = fixedCLINow
			code := RunWithDeps(tc.args, &stdout, &stderr, tc.deps)
			if code != 0 && !(tc.allowExitOne && code == 1) {
				t.Fatalf("RunWithDeps exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
			}
			var payload map[string]json.RawMessage
			assertSingleJSONValue(t, stdout.String(), &payload)
			if tc.wantSchema != "" {
				var schema string
				if err := json.Unmarshal(payload["schema_version"], &schema); err != nil || schema != tc.wantSchema {
					t.Fatalf("%s schema_version=%q err=%v payload=%s", tc.name, schema, err, stdout.String())
				}
			}
			if _, ok := payload[tc.wantLegacy]; !ok {
				t.Fatalf("%s legacy field %q missing from JSON payload: %s", tc.name, tc.wantLegacy, stdout.String())
			}
			var doc observability.Document
			if err := json.Unmarshal(payload["observability"], &doc); err != nil {
				t.Fatalf("%s observability document missing/invalid: %v\n%s", tc.name, err, stdout.String())
			}
			if doc.SchemaVersion != observability.RenderSchemaVersion || doc.Command != tc.wantCommand {
				t.Fatalf("%s observability doc = %#v", tc.name, doc)
			}
		})
	}
}

func TestRecoverJSONRedactsLegacyLocalPathsWithoutMutatingResult(t *testing.T) {
	clearGitSelectionEnvForFixture(t)
	t.Setenv("GH_REPO", "")
	repo := t.TempDir()
	runID := "run-recover-json-redaction"
	topAttemptPath := filepath.Join(repo, ".loopcoder", "runs", runID, "workers", "job-864-top.attempt.json")
	firstContextPath := filepath.Join(repo, ".loopcoder", "runs", runID, "recovery", "job-864-2-context.md")
	secondContextPath := filepath.Join(repo, ".loopcoder", "runs", runID, "recovery", "job-864-3-context.md")
	firstNestedAttemptPath := filepath.Join(repo, ".loopcoder", "runs", runID, "workers", "job-864-retry-2.attempt.json")
	secondNestedAttemptPath := filepath.Join(repo, ".loopcoder", "runs", runID, "workers", "job-864-retry-3.attempt.json")
	record := validDispatchReport()
	record.WorkID = runID
	record.Issue = 864
	original := recovery.Result{
		Action: recovery.ActionRetry,
		DispatchResult: &recovery.DispatchResult{
			OK:          true,
			Issue:       864,
			Branch:      "loop/issue-864",
			RunID:       runID,
			PR:          "https://github.com/owner/repo/pull/864",
			Summary:     "Recovered issue.",
			AttemptPath: topAttemptPath,
			Status:      "succeeded",
			ExitCode:    0,
			LogBytes:    64,
			Reason:      "retry succeeded",
			NextAction:  "loopreview",
			Report:      &record,
		},
		RecoveryAttempts: []recovery.AttemptRecord{
			{
				Version:             1,
				Issue:               864,
				RunID:               runID,
				Attempt:             2,
				Strategy:            recovery.AttemptStrategySameConfig,
				Status:              "failed",
				Branch:              "loop/issue-864",
				RecoveryContextPath: firstContextPath,
				Model:               "gpt-5.5",
				Effort:              "xhigh",
				Error:               "worker failed",
				DispatchResult: &recovery.DispatchResult{
					OK:          false,
					Issue:       864,
					Branch:      "loop/issue-864",
					RunID:       runID,
					AttemptPath: firstNestedAttemptPath,
					Status:      "failed",
					ExitCode:    1,
					LogBytes:    32,
				},
			},
			{
				Version:             1,
				Issue:               864,
				RunID:               runID,
				Attempt:             3,
				Strategy:            recovery.AttemptStrategyUpgradedConfig,
				Status:              "succeeded",
				Branch:              "loop/issue-864",
				PR:                  "https://github.com/owner/repo/pull/864",
				RecoveryContextPath: secondContextPath,
				Model:               "gpt-5.5",
				Effort:              "xhigh",
				DispatchResult: &recovery.DispatchResult{
					OK:          true,
					Issue:       864,
					Branch:      "loop/issue-864",
					RunID:       runID,
					PR:          "https://github.com/owner/repo/pull/864",
					AttemptPath: secondNestedAttemptPath,
					Status:      "succeeded",
					ExitCode:    0,
					LogBytes:    48,
				},
			},
		},
	}
	var captured recovery.Options
	var stdout, stderr bytes.Buffer
	code := RunWithDeps([]string{"recover", "--repo", repo, "--issue-number", "864", "--issue-title", "Observability", "--run-id", runID, "--format", "json"}, &stdout, &stderr, Deps{
		Recover: func(_ context.Context, opts recovery.Options) (recovery.Result, error) {
			captured = opts
			return original, nil
		},
	})
	if code != 0 {
		t.Fatalf("RunWithDeps exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr diagnostics for machine JSON = %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "warning:") || strings.Contains(stdout.String(), "OBSERVABILITY ") {
		t.Fatalf("stdout contains diagnostic/human text:\n%s", stdout.String())
	}

	type legacyDispatch struct {
		OK          bool             `json:"ok"`
		Issue       int              `json:"issue"`
		Branch      string           `json:"branch"`
		RunID       string           `json:"run_id"`
		PR          string           `json:"pr"`
		Summary     string           `json:"summary"`
		AttemptPath string           `json:"attempt_path"`
		Status      string           `json:"status"`
		ExitCode    int              `json:"exit_code"`
		LogBytes    int64            `json:"log_bytes"`
		Reason      string           `json:"reason,omitempty"`
		NextAction  string           `json:"next_action,omitempty"`
		Report      *reporter.Report `json:"report,omitempty"`
	}
	type legacyAttempt struct {
		Version             int             `json:"version"`
		Issue               int             `json:"issue"`
		RunID               string          `json:"run_id"`
		Attempt             int             `json:"attempt"`
		Strategy            string          `json:"strategy"`
		Status              string          `json:"status"`
		Branch              string          `json:"branch,omitempty"`
		PR                  string          `json:"pr,omitempty"`
		RecoveryContextPath string          `json:"recovery_context_path,omitempty"`
		Model               string          `json:"model,omitempty"`
		Effort              string          `json:"effort,omitempty"`
		Error               string          `json:"error,omitempty"`
		DispatchResult      *legacyDispatch `json:"dispatch_result,omitempty"`
	}
	var payload struct {
		SchemaVersion string                 `json:"schema_version"`
		Observability observability.Document `json:"observability"`
		Action        string                 `json:"action"`
		Dispatch      *legacyDispatch        `json:"dispatch_result,omitempty"`
		Attempts      []legacyAttempt        `json:"recovery_attempts,omitempty"`
	}
	assertSingleJSONValue(t, stdout.String(), &payload)
	if payload.SchemaVersion != "loopcoder.recover_result.v1" || payload.Action != string(recovery.ActionRetry) {
		t.Fatalf("recover payload schema/action = %q/%q", payload.SchemaVersion, payload.Action)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw payload: %v", err)
	}
	for _, key := range []string{"schema_version", "observability", "action", "dispatch_result", "recovery_attempts"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("recover JSON missing legacy key %q: %s", key, stdout.String())
		}
	}
	var dispatchKeys map[string]json.RawMessage
	if err := json.Unmarshal(raw["dispatch_result"], &dispatchKeys); err != nil {
		t.Fatalf("unmarshal dispatch_result: %v", err)
	}
	for _, key := range []string{"ok", "issue", "branch", "run_id", "pr", "summary", "attempt_path", "status", "exit_code", "log_bytes", "reason", "next_action", "report"} {
		if _, ok := dispatchKeys[key]; !ok {
			t.Fatalf("dispatch_result missing key %q: %s", key, string(raw["dispatch_result"]))
		}
	}
	var attemptKeys []map[string]json.RawMessage
	if err := json.Unmarshal(raw["recovery_attempts"], &attemptKeys); err != nil {
		t.Fatalf("unmarshal recovery_attempts keys: %v", err)
	}
	if len(attemptKeys) != 2 || len(payload.Attempts) != 2 {
		t.Fatalf("recovery_attempts length = keys:%d payload:%d", len(attemptKeys), len(payload.Attempts))
	}
	for i, keys := range attemptKeys {
		for _, key := range []string{"version", "issue", "run_id", "attempt", "strategy", "status", "branch", "recovery_context_path", "model", "effort", "dispatch_result"} {
			if _, ok := keys[key]; !ok {
				t.Fatalf("recovery_attempts[%d] missing key %q: %s", i, key, string(raw["recovery_attempts"]))
			}
		}
	}

	assertStableLegacyRecoverID(t, "dispatch_result.attempt_path", payload.Dispatch.AttemptPath, topAttemptPath, repo, ".attempt.json")
	assertStableLegacyRecoverID(t, "recovery_attempts[0].recovery_context_path", payload.Attempts[0].RecoveryContextPath, firstContextPath, repo, ".md")
	assertStableLegacyRecoverID(t, "recovery_attempts[1].recovery_context_path", payload.Attempts[1].RecoveryContextPath, secondContextPath, repo, ".md")
	assertStableLegacyRecoverID(t, "recovery_attempts[0].dispatch_result.attempt_path", payload.Attempts[0].DispatchResult.AttemptPath, firstNestedAttemptPath, repo, ".attempt.json")
	assertStableLegacyRecoverID(t, "recovery_attempts[1].dispatch_result.attempt_path", payload.Attempts[1].DispatchResult.AttemptPath, secondNestedAttemptPath, repo, ".attempt.json")
	if payload.Dispatch.Issue != 864 || payload.Dispatch.RunID != runID || payload.Dispatch.Status != "succeeded" || payload.Dispatch.LogBytes != 64 {
		t.Fatalf("dispatch_result non-path fields changed: %#v", payload.Dispatch)
	}
	if payload.Attempts[0].Strategy != recovery.AttemptStrategySameConfig || payload.Attempts[1].Strategy != recovery.AttemptStrategyUpgradedConfig {
		t.Fatalf("attempt non-path fields changed: %#v", payload.Attempts)
	}

	if original.DispatchResult.AttemptPath != topAttemptPath ||
		original.RecoveryAttempts[0].RecoveryContextPath != firstContextPath ||
		original.RecoveryAttempts[1].RecoveryContextPath != secondContextPath ||
		original.RecoveryAttempts[0].DispatchResult.AttemptPath != firstNestedAttemptPath ||
		original.RecoveryAttempts[1].DispatchResult.AttemptPath != secondNestedAttemptPath {
		t.Fatalf("recover output projection mutated original result: %#v", original)
	}
	var gotDoc, wantDoc bytes.Buffer
	if err := observability.RenderJSON(&gotDoc, payload.Observability); err != nil {
		t.Fatalf("render got observability: %v", err)
	}
	if err := observability.RenderJSON(&wantDoc, recoveryObservability(captured, original)); err != nil {
		t.Fatalf("render want observability: %v", err)
	}
	if gotDoc.String() != wantDoc.String() {
		t.Fatalf("observability document changed\nwant:\n%s\ngot:\n%s", wantDoc.String(), gotDoc.String())
	}
}

func assertStableLegacyRecoverID(t *testing.T, name, got, original string, disallowed ...string) {
	t.Helper()
	want := observability.StableRecordID(original)
	if got != want {
		t.Fatalf("%s = %q, want stable id %q from %q", name, got, want, original)
	}
	if strings.ContainsAny(got, `/\`) {
		t.Fatalf("%s = %q contains a path separator", name, got)
	}
	for _, value := range disallowed {
		if value != "" && strings.Contains(got, value) {
			t.Fatalf("%s = %q contains disallowed path fragment %q", name, got, value)
		}
	}
}

func TestObservabilityHumanAdapterUsesInjectedTerminalCapabilities(t *testing.T) {
	clearPrettyEnv(t)
	var stdout, stderr bytes.Buffer
	record := validDispatchReport()
	result := validDispatchResult(record)
	exitCode := RunWithDeps([]string{"dispatch", "--repo", t.TempDir(), "--issue-number", "864", "--issue-title", "Observability"}, &stdout, &stderr, Deps{
		IsTerminal:    func(io.Writer) bool { return true },
		TerminalWidth: func(io.Writer) int { return 48 },
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return result, nil
		},
	})
	if exitCode != 0 {
		t.Fatalf("RunWithDeps exit=%d stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "\x1b[32m") || !strings.Contains(stderr.String(), "OBSERVABILITY dispatch") {
		t.Fatalf("terminal capabilities did not reach canonical renderer:\n%s", stderr.String())
	}
}

func TestObservabilityHostAndDirectProfilesRenderSameCanonicalFacts(t *testing.T) {
	repo := t.TempDir()
	runID := "run-host-direct"
	record := validDispatchReport()
	record.WorkID = runID
	seedObservabilityRun(t, repo, runID, []observabilityAttempt{{Issue: 864, JobID: "job-864", Status: "succeeded", Report: record}})

	direct := runStatusJSONLForHostProfile(t, repo, runID, nil)
	codex := runStatusJSONLForHostProfile(t, repo, runID, map[string]string{"LOOPCODER_HOST": "codex"})
	claude := runStatusJSONLForHostProfile(t, repo, runID, map[string]string{"LOOPCODER_HOST": "claude-code"})
	paseo := runStatusJSONLForHostProfile(t, repo, runID, map[string]string{"LOOPCODER_HOST": "paseo"})
	for name, got := range map[string][]canonicalObservabilityLine{"codex": codex, "claude": claude, "paseo": paseo} {
		if !canonicalLineSetsEqual(direct, got) {
			t.Fatalf("%s host profile changed durable canonical facts\ndirect=%#v\nhost=%#v", name, direct, got)
		}
	}
}

func TestObservabilityGoldenMatrix(t *testing.T) {
	clearPrettyEnv(t)
	clearGitSelectionEnvForFixture(t)
	t.Setenv("GH_REPO", "")
	repo := t.TempDir()
	runID := "run-golden"
	record := validDispatchReport()
	record.WorkID = runID
	secret := strings.Join([]string{"gh", "p_"}, "") + strings.Repeat("A", 36)
	redactedRecord := record
	redactedResult := validDispatchResult(redactedRecord)
	redactedResult.Issue = 864
	redactedResult.RunID = runID
	redactedResult.Reason = "operator token " + secret + " should be hidden before truncation"
	redactedResult.NextAction = "retry with scrubbed context"
	seedObservabilityRun(t, repo, runID, []observabilityAttempt{
		{Issue: 864, JobID: "job-864", Status: "succeeded", Report: record},
		{Issue: 865, JobID: "job-865", Status: "succeeded", Report: secondObservabilityReport(record, 865)},
	})

	cases := []struct {
		name string
		args []string
		deps Deps
		out  string
	}{
		{
			name: "status_narrow_ascii_redirected.txt",
			args: []string{"status", "--repo", repo, "--run", runID, "--format", "text"},
			deps: Deps{IsTerminal: func(io.Writer) bool { return false }, TerminalWidth: func(io.Writer) int { return 44 }},
			out:  "stdout",
		},
		{
			name: "status_json.golden",
			args: []string{"status", "--repo", repo, "--run", runID, "--format", "json"},
			out:  "stdout",
		},
		{
			name: "status_jsonl.golden",
			args: []string{"status", "--repo", repo, "--run", runID, "--format", "jsonl"},
			out:  "stdout",
		},
		{
			name: "dispatch_redaction_jsonl.golden",
			args: []string{"dispatch", "--repo", t.TempDir(), "--issue-number", "864", "--issue-title", "Observability", "--run-id", runID, "--format", "jsonl"},
			deps: Deps{Dispatch: func(context.Context, worker.Options) (worker.Result, error) { return redactedResult, nil }},
			out:  "stdout",
		},
		{
			name: "dispatch_wave_nested_order_jsonl.golden",
			args: []string{"dispatch-wave", "--repo", t.TempDir(), "--issue-numbers", "865,864", "--run-id", runID, "--format", "jsonl", "--no-pretty"},
			deps: Deps{
				ComputeReadySet: func(context.Context, orchestration.Options) (report.ReadySetReport, error) {
					return report.ReadySetReport{Ready: []report.ReadyIssue{{Issue: 864, Title: "A", Reason: "ready"}, {Issue: 865, Title: "B", Reason: "ready"}}, Blocked: []report.BlockedIssue{}}, nil
				},
				NewGitHubReader: func(string) orchestration.GitHubReader {
					return cliFakeReader{views: map[int]gh.Issue{
						864: {Number: 864, Title: "A", Body: "Body A", State: "OPEN"},
						865: {Number: 865, Title: "B", Body: "Body B", State: "OPEN"},
					}}
				},
				Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
					wantTitle := map[int]string{864: "A", 865: "B"}[opts.IssueNumber]
					wantBody := map[int]string{864: "Body A", 865: "Body B"}[opts.IssueNumber]
					if opts.IssueTitle != wantTitle || opts.IssueBody != wantBody {
						return worker.Result{}, fmt.Errorf("dispatch-wave used issue #%d fields title=%q body=%q, want injected fake issue", opts.IssueNumber, opts.IssueTitle, opts.IssueBody)
					}
					r := secondObservabilityReport(record, opts.IssueNumber)
					out := validDispatchResult(r)
					out.Issue = opts.IssueNumber
					out.RunID = runID
					out.AttemptPath = filepath.Join(repo, ".loopcoder", "runs", runID, "workers", "job-"+string(rune('0'+opts.IssueNumber-860))+".attempt.json")
					out.Report = &r
					return out, nil
				},
			},
			out: "stdout",
		},
		{
			name: "dispatch_cancelled_stderr.golden",
			args: []string{"dispatch", "--repo", t.TempDir(), "--issue-number", "864", "--issue-title", "Observability", "--run-id", runID, "--format", "jsonl"},
			deps: Deps{Dispatch: func(context.Context, worker.Options) (worker.Result, error) { return worker.Result{}, context.Canceled }},
			out:  "stderr",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			tc.deps.Now = fixedCLINow
			code := RunWithDeps(tc.args, &stdout, &stderr, tc.deps)
			if tc.name == "dispatch_cancelled_stderr.golden" {
				if code == 0 || stdout.Len() != 0 {
					t.Fatalf("cancelled dispatch code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
			} else if code != 0 {
				t.Fatalf("RunWithDeps exit=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
			}
			got := stdout.String()
			if tc.out == "stderr" {
				got = stderr.String()
			}
			got = normalizeObservabilityGolden(got, repo)
			assertGolden(t, filepath.Join("testdata", "observability", tc.name), got)
			if strings.Contains(got, secret) || strings.Contains(got, repo) {
				t.Fatalf("%s leaked secret or path after normalization:\n%s", tc.name, got)
			}
		})
	}
}

func TestObservabilityStreamFailuresKeepMachineOutputParseable(t *testing.T) {
	repo := t.TempDir()
	runID := "run-stream"
	record := validDispatchReport()
	record.WorkID = runID
	seedObservabilityRun(t, repo, runID, []observabilityAttempt{
		{Issue: 864, JobID: "job-864", Status: "succeeded", Report: record},
		{Issue: 865, JobID: "job-865", Status: "succeeded", Report: secondObservabilityReport(record, 865)},
	})
	stdout := &partialFailingWriter{failOnWrite: 2, partialBytes: 0, err: errors.New("short write after first record")}
	var stderr bytes.Buffer
	code := RunWithDeps([]string{"report", "--repo", repo, "--format", "jsonl"}, stdout, &stderr, Deps{Now: fixedCLINow})
	if code == 0 {
		t.Fatalf("report with failing stdout returned success")
	}
	records := assertCanonicalJSONLOnly(t, stdout.String(), "report partial write")
	if len(records) != 1 {
		t.Fatalf("partial write preserved %d complete records, want 1; stdout=%q", len(records), stdout.String())
	}
	if !strings.Contains(stderr.String(), "write output") || strings.Contains(stdout.String(), "write output") {
		t.Fatalf("diagnostic separation failed stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	var cancelOut, cancelErr bytes.Buffer
	cancelCode := RunWithDeps([]string{"dispatch", "--repo", repo, "--issue-number", "864", "--issue-title", "Observability", "--format", "jsonl"}, &cancelOut, &cancelErr, Deps{
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return worker.Result{}, context.Canceled
		},
	})
	if cancelCode == 0 || cancelOut.Len() != 0 || !strings.Contains(cancelErr.String(), "context canceled") {
		t.Fatalf("cancelled dispatch code=%d stdout=%q stderr=%q", cancelCode, cancelOut.String(), cancelErr.String())
	}
}

type canonicalObservabilityLine struct {
	SchemaVersion string                    `json:"schema_version"`
	Command       string                    `json:"command"`
	Correlation   observability.Correlation `json:"correlation"`
	Item          observability.RenderItem  `json:"item"`
}

func assertCanonicalJSONLOnly(t *testing.T, output, command string) []canonicalObservabilityLine {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || strings.TrimSpace(output) == "" {
		t.Fatalf("%s emitted no JSONL", command)
	}
	records := make([]canonicalObservabilityLine, 0, len(lines))
	for _, line := range lines {
		var payload canonicalObservabilityLine
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("%s JSONL line did not parse: %v\n%s", command, err, output)
		}
		if payload.SchemaVersion != observability.RenderSchemaVersion || payload.Command == "" {
			t.Fatalf("%s JSONL line is not canonical observability: %#v\n%s", command, payload, output)
		}
		records = append(records, payload)
	}
	return records
}

func assertCanonicalSemantics(t *testing.T, records []canonicalObservabilityLine, command string) {
	t.Helper()
	found := false
	for _, record := range records {
		if record.Item.ID == "" || record.Item.Kind == "" || record.Item.Status == "" || len(record.Item.SourceRefs) == 0 {
			t.Fatalf("%s canonical record missing identity/status/source refs: %#v", command, record)
		}
		if record.Item.Provider == "codex" && record.Item.Model == "gpt-5.5" && record.Item.Usage.TotalTokens != nil && *record.Item.Usage.TotalTokens == 154 {
			found = true
		}
	}
	if !found && command != "resume" && command != "detached-recover" && command != "status" && command != "nested" {
		t.Fatalf("%s canonical records did not preserve provider/model/usage: %#v", command, records)
	}
}

type observabilityAttempt struct {
	Issue  int
	JobID  string
	Status string
	Report reporter.Report
}

func seedObservabilityRun(t *testing.T, repo, runID string, attempts []observabilityAttempt) {
	t.Helper()
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{
		Timestamp: "2026-07-14T00:00:00Z",
		RunID:     runID,
		State:     state.StatePlanned,
	}); err != nil {
		t.Fatalf("append lifecycle: %v", err)
	}
	for _, attempt := range attempts {
		report := attempt.Report
		report.WorkID = runID
		report.Issue = attempt.Issue
		report.Round = 1
		if _, err := state.WriteAttempt(repo, runID, state.AttemptRecord{
			Version:        1,
			JobID:          attempt.JobID,
			Issue:          attempt.Issue,
			Attempt:        1,
			Provider:       report.Provider,
			Phase:          "codex_exited",
			Status:         attempt.Status,
			Branch:         "loop/issue-" + strings.TrimPrefix(observability.StableRecordID(attempt.JobID), "job-"),
			StartedAt:      "2026-07-14T00:00:00Z",
			HeartbeatAt:    "2026-07-14T00:00:01Z",
			LastProgressAt: "2026-07-14T00:00:01Z",
			LogBytes:       42,
			Report:         &report,
		}); err != nil {
			t.Fatalf("write attempt: %v", err)
		}
		if err := state.AppendEvent(repo, runID, state.Event{
			Timestamp: "2026-07-14T00:00:42Z",
			RunID:     runID,
			JobID:     attempt.JobID,
			Issue:     attempt.Issue,
			Phase:     "pr_created",
			Status:    attempt.Status,
		}); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}
}

func secondObservabilityReport(record reporter.Report, issue int) reporter.Report {
	record.Issue = issue
	record.Action = "implement issue #" + observability.StableRecordID(record.Provider) + "-" + strings.TrimPrefix(observability.StableRecordID(record.WorkID), "run-")
	record.Usage = reporter.Usage{InputTokens: int64TestPtr(220), OutputTokens: int64TestPtr(44), TotalTokens: int64TestPtr(264)}
	return record
}

func observabilityTickReport(repo, runID string, result worker.Result) orchestration.TickReport {
	wave := orchestration.DispatchWaveReport{
		Repo:            "owner/repo",
		RepoPath:        repo,
		BaseBranch:      "main",
		RunID:           runID,
		IssuesRequested: []int{result.Issue},
		StartedAt:       "2026-07-14T00:00:00Z",
		FinishedAt:      "2026-07-14T00:00:42Z",
		Results: []orchestration.DispatchWaveIssueResult{{
			Issue:       result.Issue,
			Status:      result.Status,
			Branch:      result.Branch,
			PR:          result.PR,
			AttemptPath: result.AttemptPath,
			Reason:      result.Reason,
			NextAction:  result.NextAction,
			Report:      result.Report,
		}},
	}
	return orchestration.TickReport{
		Version:       orchestration.TickReportVersion,
		Repo:          "owner/repo",
		RepoPath:      repo,
		BaseBranch:    "main",
		PreProdBranch: "pre-prod",
		RunID:         runID,
		Status:        "succeeded",
		StopReason:    "complete",
		StartedAt:     "2026-07-14T00:00:00Z",
		FinishedAt:    "2026-07-14T00:00:42Z",
		Compile:       compiler.Report{},
		DispatchWave:  &wave,
		Reviews:       []orchestration.TickReviewResult{},
		RiskGates:     []orchestration.TickRiskGateResult{},
		Summary:       orchestration.TickSummary{ReadyCount: 1},
	}
}

type detachedObservabilityFixture struct {
	runID string
	owner string
	lease string
}

func seedDetachedRecoverObservabilityFixture(t *testing.T, repo string) detachedObservabilityFixture {
	t.Helper()
	t.Setenv("LOOPCODER_HOME", t.TempDir())
	now := time.Date(2026, 7, 14, 5, 0, 0, 0, time.UTC)
	ctx := context.Background()
	registered, err := registry.Register(ctx, registry.Options{RepoPath: repo, Now: func() time.Time { return now }}, registry.DefaultDeps())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	store, _, err := openDetachedStore(ctx, repo, Deps{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("open detached store: %v", err)
	}
	defer store.Close()
	claim, err := detachedrun.Claim(ctx, store, detachedrun.ClaimRequest{
		ProjectID:      registered.Project.ProjectID,
		RunID:          "run-detached-observability",
		Owner:          "owner-detached-observability",
		LeaseExpiresAt: now.Add(time.Minute),
		IssueNumber:    864,
		Attempt:        1,
		BaseBranch:     "pre-prod",
		Provider:       "codex",
		Payload:        map[string]any{"issue_title": "Detached observability"},
		Now:            now,
	})
	if err != nil {
		t.Fatalf("detached claim: %v", err)
	}
	return detachedObservabilityFixture{runID: claim.RunID, owner: claim.Owner, lease: claim.LeaseExpiresAt}
}

func runStatusJSONLForHostProfile(t *testing.T, repo, runID string, env map[string]string) []canonicalObservabilityLine {
	t.Helper()
	clearPrettyEnv(t)
	old := map[string]*string{}
	for _, name := range []string{"LOOPCODER_HOST", "CODEX_THREAD_ID", "CODEX_CLI", "CLAUDE_CODE_SESSION_ID", "PASEO_AGENT_ID", "PASEO_HOST"} {
		if value, ok := os.LookupEnv(name); ok {
			copy := value
			old[name] = &copy
		} else {
			old[name] = nil
		}
		_ = os.Unsetenv(name)
	}
	for key, value := range env {
		if err := os.Setenv(key, value); err != nil {
			t.Fatalf("set env %s: %v", key, err)
		}
	}
	t.Cleanup(func() {
		for name, value := range old {
			if value == nil {
				_ = os.Unsetenv(name)
			} else {
				_ = os.Setenv(name, *value)
			}
		}
	})
	var stdout, stderr bytes.Buffer
	code := RunWithDeps([]string{"status", "--repo", repo, "--run", runID, "--format", "jsonl"}, &stdout, &stderr, Deps{Now: fixedCLINow})
	if code != 0 {
		t.Fatalf("status host profile exit=%d stderr=%q", code, stderr.String())
	}
	return assertCanonicalJSONLOnly(t, stdout.String(), "host/direct status")
}

func canonicalLineSetsEqual(a, b []canonicalObservabilityLine) bool {
	if len(a) != len(b) {
		return false
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func assertGolden(t *testing.T, relPath, got string) {
	t.Helper()
	path := filepath.Join(".", relPath)
	if os.Getenv("UPDATE_OBSERVABILITY_GOLDENS") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden %s: %v", relPath, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", relPath, err)
	}
	if string(want) != got {
		t.Fatalf("golden %s mismatch\nwant:\n%s\ngot:\n%s", relPath, string(want), got)
	}
}

func normalizeObservabilityGolden(got, repo string) string {
	got = strings.ReplaceAll(got, repo, "$REPO")
	replacements := []struct {
		re   *regexp.Regexp
		with string
	}{
		{regexp.MustCompile(`proj_[a-f0-9]{8,16}`), "proj_TEST"},
		{regexp.MustCompile(`usage_[a-z0-9]{20,}`), "usage_TEST"},
		{regexp.MustCompile(`"display_name": "[0-9]+"`), `"display_name": "repo"`},
		{regexp.MustCompile(`"local_path": "[^"]+"`), `"local_path": "$REPO"`},
		{regexp.MustCompile(`Project: proj_TEST \([^)]+\)`), `Project: proj_TEST (repo)`},
		{regexp.MustCompile(`/[^"]*/TestObservabilityGoldenMatrix[^"]*`), `$TMP`},
	}
	for _, replacement := range replacements {
		got = replacement.re.ReplaceAllString(got, replacement.with)
	}
	return got
}
