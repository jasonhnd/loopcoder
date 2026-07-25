package artifactqual_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func TestCanaryClaudeChildRequiresMatchingAccountBoundCatalogReceipt(t *testing.T) {
	now := time.Date(2026, 7, 26, 7, 0, 0, 0, time.UTC)
	account := "acct-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	install := "pinst_claude_receipt"
	ev := artifactqual.CanaryEvidence{
		Schema:                artifactqual.SchemaCanaryEvidence,
		ArchiveDigest:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PreProdSHA:            "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		BinaryVersion:         "0.9.0-rc.44",
		BinaryCommit:          "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ProjectID:             "disp-claude-receipt",
		RunID:                 "run-claude-receipt",
		InventoryProvenance:   "live_discover",
		InventoryReportDigest: "sha256:inventory-claude-receipt",
		ProducedAt:            now,
		Children: []artifactqual.CanaryChild{{
			ChildID: "wi_claude", AttemptID: "att_claude", Provider: "claude",
			Model: "claude-sonnet-5", DepthRequired: "low", DepthSelected: "low", DepthInvocation: "low",
			AccountRef: account, InstallRef: install, WindowKind: "rolling",
			Terminal: "succeeded", RealProviderExecuted: true,
			ActualSources: &artifactqual.CanaryRouteSources{
				Model: "provider_stream", Effort: "accepted_invocation", Permission: "accepted_invocation",
				Account: "auth_binding", Install: "install_binding",
			},
			ArgvDigest: "sha256:child-argv",
		}},
	}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	without := artifactqual.ValidateCanaryEvidence(ev, ev.ArchiveDigest, ev.PreProdSHA, now)
	if !hasCanaryReason(without.Reasons, "claude_catalog_receipt_missing_or_mismatched:wi_claude") {
		t.Fatalf("missing receipt reason absent: %v", without.Reasons)
	}

	receipt := providerinventory.ClaudeCapabilityProbeReceipt{
		SchemaVersion:          providerinventory.ClaudeCapabilityProbeReceiptSchema,
		Provider:               "claude",
		RequestedModel:         "sonnet",
		ActualModel:            "claude-sonnet-5",
		AcceptedEffort:         "low",
		ProviderInstallationID: install,
		AccountProfileID:       account,
		AuthReadinessID:        "auth_claude_receipt",
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
		BudgetReservationID:    "bres_claude_receipt",
		ReservedTokens:         1000,
		CommittedTokens:        31,
		ReleasedTokens:         969,
		BudgetState:            "released",
		UsageRecordIDs:         []string{"usage_claude_receipt"},
		Source:                 "claude-code-stream-json",
		Confidence:             providerinventory.ConfidenceExact,
		FreshnessState:         providerinventory.FreshnessFresh,
		GapReasons:             []string{},
	}
	ev.ClaudeCatalogReceipts = []artifactqual.CanaryClaudeCatalogReceipt{{
		InventoryReportDigest: ev.InventoryReportDigest,
		Receipt:               receipt,
	}}
	ev.ContentDigest = artifactqual.DigestCanaryBody(ev)
	with := artifactqual.ValidateCanaryEvidence(ev, ev.ArchiveDigest, ev.PreProdSHA, now)
	for _, reason := range with.Reasons {
		if strings.HasPrefix(reason, "claude_catalog_receipt_") {
			t.Fatalf("matching receipt rejected: %v", with.Reasons)
		}
	}
}

func hasCanaryReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
