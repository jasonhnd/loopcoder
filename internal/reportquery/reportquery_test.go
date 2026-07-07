package reportquery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		"REPORTS",
		"work_id: loopreview-99",
		"model: claude-opus-4-8[1m] (max)",
		"issue: #575",
		"round: 2",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("RenderText missing %q:\n%s", want, text)
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
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("JSON output invalid: %v", err)
	}
	if len(payload.Reports) != 2 {
		t.Fatalf("JSON reports = %d, want 2", len(payload.Reports))
	}
	if _, err := filepath.Rel(repo, records[1].Path); err != nil {
		t.Fatalf("worker source path is not under repo: %v", err)
	}
}

func TestListReadsLegacyAttestationEnvelopeAsReport(t *testing.T) {
	repo := t.TempDir()
	runID := "run-legacy"
	legacyReport := testReport(reporter.RoleVerifier, "claude", "claude-opus-4-8[1m]", "max", "review PR #579", "2026-07-07T00:02:00Z")
	path := filepath.Join(repo, ".loopcoder", "runs", runID, "verifiers", "pr-579.loopreview.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll verifier dir: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"verdict":     "pass",
		"attestation": legacyReport,
	})
	if err != nil {
		t.Fatalf("Marshal legacy payload: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile legacy payload: %v", err)
	}

	records, err := List(Options{RepoPath: repo})
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("List returned %d records, want one legacy report: %#v", len(records), records)
	}
	if records[0].Report.Role != reporter.RoleVerifier || records[0].Report.Action != "review PR #579" {
		t.Fatalf("legacy report = %#v, want verifier PR report", records[0].Report)
	}
	output, err := MarshalJSON(records)
	if err != nil {
		t.Fatalf("MarshalJSON returned error: %v", err)
	}
	if strings.Contains(string(output), "attestation") {
		t.Fatalf("legacy input leaked old envelope name to JSON output: %s", string(output))
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
