package providerinventory

import (
	"testing"
	"time"
)

func TestValidateClaudeCapabilityProbeReceiptRejectsTampering(t *testing.T) {
	now := time.Date(2026, 7, 26, 8, 0, 0, 0, time.UTC)
	valid := testClaudeCapabilityReceipt(now)
	if err := ValidateClaudeCapabilityProbeReceipt(valid, now); err != nil {
		t.Fatalf("valid receipt: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*ClaudeCapabilityProbeReceipt)
	}{
		{name: "unknown-candidate", mutate: func(r *ClaudeCapabilityProbeReceipt) { r.RequestedModel = "invented" }},
		{name: "normalized-candidate-forbidden", mutate: func(r *ClaudeCapabilityProbeReceipt) { r.RequestedModel = " sonnet " }},
		{name: "unsafe-actual-model", mutate: func(r *ClaudeCapabilityProbeReceipt) { r.ActualModel = "claude/model" }},
		{name: "nonhex-account", mutate: func(r *ClaudeCapabilityProbeReceipt) {
			r.AccountProfileID = "acct-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
		}},
		{name: "invalid-effort", mutate: func(r *ClaudeCapabilityProbeReceipt) { r.AcceptedEffort = "HIGH" }},
		{name: "invalid-output-hash", mutate: func(r *ClaudeCapabilityProbeReceipt) { r.OutputRawSHA256 = "sha256:short" }},
		{name: "component-total-mismatch", mutate: func(r *ClaudeCapabilityProbeReceipt) { r.TotalTokens++ }},
		{name: "budget-arithmetic-mismatch", mutate: func(r *ClaudeCapabilityProbeReceipt) { r.ReleasedTokens-- }},
		{name: "state-arithmetic-mismatch", mutate: func(r *ClaudeCapabilityProbeReceipt) { r.BudgetState = "committed" }},
		{name: "duplicate-usage", mutate: func(r *ClaudeCapabilityProbeReceipt) {
			r.UsageRecordIDs = append(r.UsageRecordIDs, r.UsageRecordIDs[0])
		}},
		{name: "exact-with-gap", mutate: func(r *ClaudeCapabilityProbeReceipt) { r.GapReasons = []string{"not-exact"} }},
		{name: "expired", mutate: func(r *ClaudeCapabilityProbeReceipt) { r.ExpiresAt = now.Format(time.RFC3339Nano) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			receipt := valid
			receipt.UsageRecordIDs = append([]string(nil), valid.UsageRecordIDs...)
			tc.mutate(&receipt)
			if err := ValidateClaudeCapabilityProbeReceipt(receipt, now); err == nil {
				t.Fatalf("tampered receipt accepted: %#v", receipt)
			}
		})
	}
}

func TestValidateClaudeCapabilityProbeReceiptAcceptsFullyCommittedReservation(t *testing.T) {
	now := time.Date(2026, 7, 26, 8, 0, 0, 0, time.UTC)
	receipt := testClaudeCapabilityReceipt(now)
	receipt.ReservedTokens = receipt.TotalTokens
	receipt.ReleasedTokens = 0
	receipt.BudgetState = "committed"
	if err := ValidateClaudeCapabilityProbeReceipt(receipt, now); err != nil {
		t.Fatalf("fully committed receipt: %v", err)
	}
}

func TestValidateClaudeCapabilityProbeReceiptAcceptsDeclaredBracketedOpusID(t *testing.T) {
	now := time.Date(2026, 7, 26, 8, 0, 0, 0, time.UTC)
	receipt := testClaudeCapabilityReceipt(now)
	receipt.RequestedModel = "opus"
	receipt.ActualModel = "claude-opus-4-8[1m]"
	if _, ok := ClaudeCatalogCandidate(receipt.RequestedModel); !ok {
		t.Fatal("test requires adapter-declared opus alias")
	}
	if err := ValidateClaudeCapabilityProbeReceipt(receipt, now); err != nil {
		t.Fatalf("declared exact Opus receipt: %v", err)
	}
}

func testClaudeCapabilityReceipt(now time.Time) ClaudeCapabilityProbeReceipt {
	return ClaudeCapabilityProbeReceipt{
		SchemaVersion:          ClaudeCapabilityProbeReceiptSchema,
		Provider:               "claude",
		RequestedModel:         "sonnet",
		ActualModel:            "claude-sonnet-5",
		AcceptedEffort:         "low",
		ProviderInstallationID: "pinst_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AccountProfileID:       "acct-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AuthReadinessID:        "auth_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		AuthObservedAt:         now.Add(-time.Minute).Format(time.RFC3339Nano),
		ExecutedAt:             now.Add(-time.Minute).Format(time.RFC3339Nano),
		ExpiresAt:              now.Add(29 * time.Minute).Format(time.RFC3339Nano),
		AuthRawSHA256:          "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		OutputRawSHA256:        "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ArgvDigest:             "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		InputTokens:            2,
		OutputTokens:           4,
		CacheReadInputTokens:   20,
		CacheCreateInputTokens: 5,
		TotalTokens:            31,
		CostUSDMicros:          123,
		BudgetReservationID:    "bres_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		ReservedTokens:         1000,
		CommittedTokens:        31,
		ReleasedTokens:         969,
		BudgetState:            "released",
		UsageRecordIDs:         []string{"usage_aaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Source:                 "claude-code-stream-json",
		Confidence:             ConfidenceExact,
		FreshnessState:         FreshnessFresh,
		GapReasons:             []string{},
	}
}
