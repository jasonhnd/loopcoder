package reportquery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/migration"
	"github.com/jasonhnd/loopcoder/internal/relaygate"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
)

func TestListReadsAttemptsAndPendingRelayReports(t *testing.T) {
	repo := t.TempDir()
	workerReport := testReport(reporter.RoleWorker, "codex", "gpt-5.5", "high", "implement issue #575", "2026-07-07T00:00:00Z")
	if _, err := state.WriteAttempt(repo, "run-test", state.AttemptRecord{
		Version:        1,
		JobID:          "job-575-1",
		Issue:          575,
		Attempt:        2,
		Provider:       "codex",
		PID:            123,
		Phase:          "cleanup",
		Status:         "succeeded",
		Branch:         "loop/issue-575",
		StartedAt:      "2026-07-07T00:00:00Z",
		HeartbeatAt:    "2026-07-07T00:00:01Z",
		LastProgressAt: "2026-07-07T00:00:01Z",
		Report:         &workerReport,
	}); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}

	verifierReport := testReport(reporter.RoleVerifier, "claude", "claude-opus-4-8[1m]", "max", "review PR #99", "2026-07-07T00:01:00Z")
	verifierReport.WorkID = "loopreview-99"
	if _, err := relaygate.Write(relaygate.WriteOptions{
		RepoPath: repo,
		RunID:    "loopreview-pr-99",
		Role:     string(reporter.RoleVerifier),
		PRNumber: 99,
		Block:    verifierReport.Pretty(reporter.PrettyOptions{Mode: reporter.PrettyModePlain}),
		Report:   &verifierReport,
	}); err != nil {
		t.Fatalf("relaygate.Write: %v", err)
	}

	records, err := List(Options{RepoPath: repo})
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("List returned %d records, want 2: %#v", len(records), records)
	}
	if records[0].Report.Role != reporter.RoleVerifier || records[1].Report.Role != reporter.RoleWorker {
		t.Fatalf("records not sorted newest first: %#v", records)
	}
	if records[0].Source != "relay-pending" || records[0].RunID != "loopreview-pr-99" || records[0].Path == "" {
		t.Fatalf("verifier source context = %#v, want pending relay context", records[0])
	}
	if records[1].Source != "attempt" || records[1].RunID != "run-test" || records[1].Path == "" {
		t.Fatalf("worker source context = %#v, want attempt context", records[1])
	}
	worker := records[1].Report
	if worker.WorkID != "run-test" || worker.Issue != 575 || worker.Branch != "loop/issue-575" || worker.Round != 2 {
		t.Fatalf("worker context = %#v", worker)
	}

	filtered, err := List(Options{RepoPath: repo, WorkID: "run-test", Role: reporter.RoleWorker})
	if err != nil {
		t.Fatalf("filtered List returned error: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Report.WorkID != "run-test" {
		t.Fatalf("filtered records = %#v, want run-test worker", filtered)
	}

	text := RenderText(records)
	for _, want := range []string{
		"loopcoder report: verifier success",
		"Target",
		"- PR: #99",
		"Verdict",
		"- status: success",
		"Run",
		"- run id: loopreview-pr-99",
		"- verifier: claude / claude-opus-4-8[1m] (max) / write",
		"loopcoder report: worker success",
		"- issue: #575",
		"- branch: loop/issue-575",
		"- worker: codex / gpt-5.5 (high) / write",
		"Next",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("RenderText missing %q:\n%s", want, text)
		}
	}

	verbose := RenderVerboseText(records)
	for _, want := range []string{
		"REPORTS",
		"work_id: loopreview-99",
		"source: relay-pending",
		"run_id: loopreview-pr-99",
		"source: attempt",
		"run_id: run-test",
		"model: claude-opus-4-8[1m] (max)",
		"issue: #575",
		"round: 2",
	} {
		if !strings.Contains(verbose, want) {
			t.Fatalf("RenderVerboseText missing %q:\n%s", want, verbose)
		}
	}

	data, err := MarshalJSON(records)
	if err != nil {
		t.Fatalf("MarshalJSON returned error: %v", err)
	}
	if strings.Contains(string(data), "attestation") {
		t.Fatalf("JSON output used legacy terminology: %s", string(data))
	}
	var payload struct {
		Reports []reporter.Report `json:"reports"`
		Records []struct {
			Report reporter.Report `json:"report"`
			Source string          `json:"source"`
			RunID  string          `json:"run_id"`
			Path   string          `json:"path"`
		} `json:"records"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("JSON output invalid: %v", err)
	}
	if len(payload.Reports) != 2 {
		t.Fatalf("JSON reports = %d, want 2", len(payload.Reports))
	}
	if len(payload.Records) != 2 {
		t.Fatalf("JSON records = %d, want 2", len(payload.Records))
	}
	if payload.Records[1].Report.WorkID != "run-test" || payload.Records[1].Source != "attempt" || payload.Records[1].RunID != "run-test" || payload.Records[1].Path == "" {
		t.Fatalf("JSON attempt record = %#v, want report plus source context", payload.Records[1])
	}
	if _, err := filepath.Rel(repo, records[1].Path); err != nil {
		t.Fatalf("worker source path is not under repo: %v", err)
	}
}

func TestRenderTextReceiptsForVerifierNeedsHumanAndTerminalWorkerStatuses(t *testing.T) {
	verifier := testReport(reporter.RoleVerifier, "claude", "claude-opus-4-8[1m]", "max", "review PR #670", "2026-07-07T00:01:00Z")
	verifier.WorkID = "loopreview-670"
	verifier.Issue = 670
	verifier.Permission = reporter.PermissionReadOnly

	records := []Record{
		{
			Report:          verifier,
			Source:          "run-json",
			RunID:           "loopreview-pr-670",
			Verdict:         "needs-human",
			SpecConformance: "not-applicable",
			Evidence:        "merged design/spec evidence was not found",
			Findings: []loopreview.Finding{
				{Severity: "warning", Note: "missing spec"},
				{Severity: "low", Note: "non-blocking smoke gap"},
				{Severity: "low", Note: "manual acceptance gap"},
				{Severity: "info", Note: "context note"},
			},
		},
		workerStatusRecord("run-fail", "failed", "worker failed", 1),
		workerStatusRecord("run-timeout", "timed_out", "", 0),
		workerStatusRecord("run-cancel", "cancelled", "", 0),
		workerStatusRecord("run-child", "abandoned", "", 0),
	}

	text := RenderText(records)
	for _, want := range []string{
		"loopcoder report: verifier needs-human",
		"- status: needs-human",
		"- blocking: needs human",
		"- reason: merged design/spec evidence was not found",
		"- findings: 1 warning, 2 low, 1 info",
		"- human should decide whether the unresolved evidence is acceptable",
		"loopcoder report: worker fail",
		"- reason: worker failed",
		"loopcoder report: worker timeout",
		"- reason: run timed out before completion",
		"loopcoder report: worker cancelled",
		"- reason: run was cancelled before completion",
		"loopcoder report: worker partial-child-failure",
		"- reason: one or more child runs did not complete successfully",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("RenderText missing %q:\n%s", want, text)
		}
	}
}

func TestCollectReportRecordsEnrichesReviewContext(t *testing.T) {
	report := testReport(reporter.RoleVerifier, "claude", "opus", "max", "review PR #701", "2026-07-07T00:01:00Z")
	payload := map[string]any{
		"verdict":          "needs-human",
		"spec_conformance": "not-applicable",
		"evidence":         "merged spec unavailable",
		"pr_number":        701,
		"findings": []map[string]any{{
			"severity": "warning",
			"file":     "docs/specs/0701.md",
			"note":     "missing merged spec",
		}},
		"report": report,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	records := collectReportRecords(data, "run-json", "run-review", "review.json", testModTime())
	if len(records) == 0 {
		t.Fatal("collectReportRecords returned no records")
	}
	record := dedupe(records)[0]
	if record.Verdict != "needs-human" || record.PRNumber != 701 || len(record.Findings) != 1 {
		t.Fatalf("record context = %#v", record)
	}
}

func TestListAcceptsLegacyReportInputs(t *testing.T) {
	repo := t.TempDir()
	runID := "run-legacy"
	runPath := state.RunPath(repo, runID)
	if err := os.MkdirAll(runPath, 0o755); err != nil {
		t.Fatalf("create run path: %v", err)
	}

	jsonReport := testReport(reporter.RoleWorker, "codex", "gpt-5.5", "high", "implement issue #606", "2026-07-07T00:00:00Z")
	data, err := json.Marshal(map[string]reporter.Report{
		migration.LegacyReportStateKey: jsonReport,
	})
	if err != nil {
		t.Fatalf("marshal legacy report: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runPath, "legacy-report.json"), data, 0o644); err != nil {
		t.Fatalf("write legacy report json: %v", err)
	}

	ledgerReport := testReport(reporter.RoleVerifier, "claude", "claude-opus-4-8[1m]", "max", "review PR #606", "2026-07-07T00:01:00Z")
	header := strings.Replace(ledgerReport.Header(), "["+migration.ReporterHeaderToken+"]", "["+migration.LegacyReporterHeaderToken+"]", 1)
	relayDir := filepath.Join(repo, ".loopcoder", "relay", runID)
	if err := os.MkdirAll(relayDir, 0o755); err != nil {
		t.Fatalf("create relay dir: %v", err)
	}
	ledgerPath := filepath.Join(relayDir, "legacy.attest")
	ledger := "# run_id=" + runID + "\n" + header + "\n"
	if err := os.WriteFile(ledgerPath, []byte(ledger), 0o644); err != nil {
		t.Fatalf("write legacy ledger: %v", err)
	}

	records, err := List(Options{RepoPath: repo, Limit: 10})
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	bySource := map[string]Record{}
	for _, record := range records {
		bySource[record.Source] = record
	}
	if got := bySource["run-json"]; got.Report.Role != reporter.RoleWorker || got.RunID != runID || got.Path == "" {
		t.Fatalf("legacy JSON record = %#v, want run-json worker context", got)
	}
	if got := bySource["relay-ledger"]; got.Report.Role != reporter.RoleVerifier || got.RunID != runID || got.Report.WorkID != runID || got.Path != ledgerPath {
		t.Fatalf("legacy header record = %#v, want relay-ledger verifier context", got)
	}
}

func testReport(role reporter.Role, provider, model, effort, action, ended string) reporter.Report {
	total := int64(42)
	return reporter.Report{
		Role:        role,
		Provider:    provider,
		Model:       model,
		ModelSource: reporter.ModelSourceForProvider(provider),
		Effort:      effort,
		Permission:  reporter.PermissionWrite,
		Action:      action,
		ExitCode:    0,
		StartedAt:   "2026-07-07T00:00:00Z",
		EndedAt:     ended,
		DurationMS:  1000,
		Usage: reporter.Usage{
			TotalTokens: &total,
		},
		Verified: true,
	}
}

func workerStatusRecord(runID, status, errorText string, exitCode int) Record {
	record := testReport(reporter.RoleWorker, "codex", "gpt-5.5", "high", "implement issue #670", "2026-07-07T00:00:00Z")
	record.WorkID = runID
	record.Issue = 670
	record.Branch = "loop/issue-670"
	record.ExitCode = exitCode
	return Record{
		Report: record,
		Source: "attempt",
		RunID:  runID,
		Status: status,
		Error:  errorText,
	}
}

func testModTime() time.Time {
	return time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
}
