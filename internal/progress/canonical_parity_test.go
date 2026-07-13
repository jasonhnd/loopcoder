package progress_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/progress"
)

func TestProgressSemanticFingerprintMatchesDeliveryCanonicalContract(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 5, 0, time.UTC)
	receipt, err := progress.NormalizeReceipt(progress.ProgressReceipt{
		ProjectID:           "proj_progress",
		DeliveryRunID:       "run_progress",
		RunID:               "run_progress",
		TaskID:              "task_progress",
		AttemptID:           "attempt_progress",
		AttemptOrdinal:      7,
		CorrelationID:       "corr_progress",
		CorrelationSequence: 3,
		Phase:               "verifying",
		Status:              "waiting",
		TaskCounts:          progress.TaskCounts{Total: 2, Running: 1, Blocked: 1},
		Provider: progress.ProviderIdentity{
			ProviderID:         "codex",
			ModelID:            "model<&>",
			ProviderConfidence: "exact",
		},
		Heartbeat: progress.AgeEvidence{State: "exact", ObservedAt: "2026-07-13T11:59:00Z"},
		Progress:  progress.AgeEvidence{State: "exact", ObservedAt: "2026-07-13T11:55:00Z"},
		Evidence: []progress.EvidenceRef{{
			RecordKind:     "ci-check",
			RecordID:       "check-1",
			Summary:        "html-sensitive <>& text and NFC e\u0301",
			Classification: "local-diagnostic",
			Confidence:     "exact",
		}},
		QuotaBudget: progress.QuotaBudgetState{
			State:             progress.Unknown,
			Confidence:        progress.Unknown,
			RemainingQuantity: -1,
			GapReasons:        []string{"not-collected", "future-field-compatible"},
		},
		Blocker:     progress.ActionState{State: "waiting", Summary: "waiting for CI"},
		NextAction:  progress.ActionState{State: "wait", Summary: "continue observing supervisor-owned checks"},
		GapReasons:  []string{progress.KnownWaitingCI, progress.ReasonStateChange},
		OccurredAt:  "2026-07-13T12:00:00Z",
		PersistedAt: "2026-07-13T12:00:05Z",
	}, now)
	if err != nil {
		t.Fatalf("NormalizeReceipt: %v", err)
	}

	want, _, err := delivery.DigestCanonicalJSON(progressSemanticMap(receipt))
	if err != nil {
		t.Fatalf("delivery canonical semantic digest: %v", err)
	}
	if receipt.SemanticFingerprint != want {
		t.Fatalf("semantic fingerprint = %s, want delivery canonical digest %s", receipt.SemanticFingerprint, want)
	}
}

func TestProgressDecodeIgnoresFutureFieldsAndRejectsInvalidFloatNumbers(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 5, 0, time.UTC)
	base := `{
		"schema_version":"loopcoder.progress_receipt.v1",
		"record_version":1,
		"project_id":"proj_progress",
		"delivery_run_id":"run_progress",
		"run_id":"run_progress",
		"task_id":"task_progress",
		"attempt_id":"attempt_progress",
		"attempt_ordinal":1,
		"correlation_id":"corr_progress",
		"correlation_sequence":1,
		"phase":"running",
		"status":"running",
		"task_counts":{"total":1,"running":1},
		"heartbeat":{"state":"exact","observed_at":"2026-07-13T12:00:00Z","age_millis":0},
		"progress":{"state":"exact","observed_at":"2026-07-13T12:00:00Z","age_millis":0},
		"occurred_at":"2026-07-13T12:00:00Z",
		"persisted_at":"2026-07-13T12:00:05Z"
	}`
	withFuture := strings.Replace(base, `"persisted_at":"2026-07-13T12:00:05Z"`, `"persisted_at":"2026-07-13T12:00:05Z","future_field":{"array":[1,2,3],"html":"<>&"}`, 1)
	decodedBase, err := progress.DecodeReceiptJSON([]byte(base), now)
	if err != nil {
		t.Fatalf("DecodeReceiptJSON base: %v", err)
	}
	decodedFuture, err := progress.DecodeReceiptJSON([]byte(withFuture), now)
	if err != nil {
		t.Fatalf("DecodeReceiptJSON future: %v", err)
	}
	if decodedFuture.SemanticFingerprint != decodedBase.SemanticFingerprint {
		t.Fatalf("future field changed semantic fingerprint: %s != %s", decodedFuture.SemanticFingerprint, decodedBase.SemanticFingerprint)
	}
	invalidFloat := strings.Replace(base, `"age_millis":0`, `"age_millis":0.5`, 1)
	if _, err := progress.DecodeReceiptJSON([]byte(invalidFloat), now); err == nil {
		t.Fatal("DecodeReceiptJSON accepted a float in an integer receipt field")
	}
}

func progressSemanticMap(receipt progress.ProgressReceipt) map[string]any {
	return map[string]any{
		"schema_version":       receipt.SchemaVersion,
		"project_id":           receipt.ProjectID,
		"delivery_run_id":      receipt.DeliveryRunID,
		"run_id":               receipt.RunID,
		"task_id":              receipt.TaskID,
		"attempt_id":           receipt.AttemptID,
		"attempt_ordinal":      receipt.AttemptOrdinal,
		"correlation_id":       receipt.CorrelationID,
		"correlation_sequence": receipt.CorrelationSequence,
		"phase":                receipt.Phase,
		"status":               receipt.Status,
		"task_counts":          receipt.TaskCounts,
		"provider":             receipt.Provider,
		"heartbeat":            receipt.Heartbeat,
		"progress":             receipt.Progress,
		"evidence":             receipt.Evidence,
		"quota_budget":         receipt.QuotaBudget,
		"blocker":              receipt.Blocker,
		"next_action":          receipt.NextAction,
		"gap_reasons":          receipt.GapReasons,
		"redaction_markers":    receipt.Redaction.Markers,
		"redacted":             receipt.Redaction.Redacted,
		"truncated":            receipt.Redaction.Truncated,
		"secret_finding_count": receipt.Redaction.SecretFindingCount,
	}
}
