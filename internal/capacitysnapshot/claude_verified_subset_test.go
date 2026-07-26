package capacitysnapshot_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func TestClaudeMachineReadableCatalogRequiresAccountBoundInvocationReceipt(t *testing.T) {
	now := time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC)
	const (
		install = "pinst_claude_verified"
		account = "acct-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		authID  = "auth_claude_verified"
		snapID  = "mcatsnap_claude_verified"
	)
	remaining, limit := int64(92), int64(100)
	receipt := verifiedClaudeReceipt(now, install, account, authID)
	report := providerinventory.Report{
		InventoryFingerprint: "sha256:inventory-claude-verified",
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("claude", install, "sha256:claude-resolved", "sha256:claude-path"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AuthReadinessID:        authID,
			AdapterID:              "claude",
			ReadinessState:         providerinventory.ReadinessReady,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			ReadinessConfidence:    providerinventory.ConfidenceExact,
			AccountProfileID:       ptrStr(account),
			ProviderInstallationID: ptrStr(install),
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: snapID,
			AdapterID:              "claude",
			ProviderInstallationID: ptrStr(install),
			AccountProfileID:       ptrStr(account),
			AuthReadinessID:        ptrStr(authID),
			CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
			CatalogSourceReference: "claude-capability-probe#" + receipt.OutputRawSHA256,
			SourceSchemaVersion:    providerinventory.ClaudeCapabilityProbeReceiptSchema,
			EntryCount:             1,
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			CapabilityProbeReceipt: &receipt,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			ModelCatalogSnapshotID: snapID,
			AdapterID:              "claude",
			CanonicalModelID:       "claude-sonnet-5",
			DisplayName:            "claude-sonnet-5",
			AvailabilityState:      providerinventory.AvailabilityAvailable,
			LifecycleState:         providerinventory.LifecycleAvailable,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			Constraints:            []string{"supported_depth=low", "default_depth=low"},
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
				SourceReference: "claude-capability-probe#" + receipt.OutputRawSHA256,
				Confidence:      providerinventory.ConfidenceExact,
				FreshnessState:  providerinventory.FreshnessFresh,
			}},
			Source: providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID:              "claude",
			WindowKind:             providerinventory.WindowRolling,
			Unit:                   "percent",
			RemainingValue:         &remaining,
			LimitValue:             &limit,
			AccountProfileID:       ptrStr(account),
			ProviderInstallationID: ptrStr(install),
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			CapturedAt:             now.Format(time.RFC3339Nano),
		}},
	}
	forged := report.ModelCapabilities[0]
	forged.ModelCapabilityID = "mcap_forged"
	forged.CanonicalModelID = "claude-forged"
	forged.DisplayName = "claude-forged"
	report.ModelCapabilities = append(report.ModelCapabilities, forged)
	accounts := capacitysnapshot.FromProviderInventoryReport(report, now)
	inventory, err := capacitysnapshot.ToRouteInventory(mustBuild(t, accounts, now), now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range inventory.Candidates {
		if candidate.Provider == "claude" && candidate.Model == "claude-forged" {
			t.Fatalf("extra capability sharing receipt snapshot routed: %+v", candidate)
		}
		if candidate.Provider == "claude" && candidate.Model == "claude-sonnet-5" && candidate.Effort == "low" {
			found = true
			if candidate.AccountRef != account || candidate.InstallRef != install {
				t.Fatalf("candidate identity changed: %+v", candidate)
			}
		}
	}
	if !found {
		t.Fatalf("verified Claude route absent: %+v", inventory.Candidates)
	}

	opusReport := report
	opusReceipt := receipt
	opusReceipt.RequestedModel = "opus"
	opusReceipt.ActualModel = "claude-opus-4-8"
	opusReceipt.AcceptedEffort = "high"
	opusSnapshot := report.ModelCatalogSnapshots[0]
	opusSnapshot.CapabilityProbeReceipt = &opusReceipt
	opusModel := report.ModelCapabilities[0]
	opusModel.CanonicalModelID = "claude-opus-4-8"
	opusModel.DisplayName = "claude-opus-4-8"
	opusModel.Constraints = []string{"supported_depth=high", "default_depth=high"}
	opusReport.ModelCatalogSnapshots = []providerinventory.ModelCatalogSnapshot{opusSnapshot}
	opusReport.ModelCapabilities = []providerinventory.ModelCapability{opusModel}
	opusAccounts := capacitysnapshot.FromProviderInventoryReport(opusReport, now)
	opusInventory, err := capacitysnapshot.ToRouteInventory(mustBuild(t, opusAccounts, now), now)
	if err != nil {
		t.Fatal(err)
	}
	opusFound := false
	for _, candidate := range opusInventory.Candidates {
		if candidate.Provider == "claude" && candidate.Model == "claude-opus-4-8" && candidate.Effort == "high" {
			opusFound = true
			if candidate.ModelClass != capclass.ClassSoul {
				t.Fatalf("observed exact Opus class = %s, want soul: %+v", candidate.ModelClass, candidate)
			}
		}
	}
	if !opusFound {
		t.Fatalf("verified exact Opus route absent: %+v", opusInventory.Candidates)
	}

	report.ModelCatalogSnapshots[0].CapabilityProbeReceipt = nil
	unverifiedAccounts := capacitysnapshot.FromProviderInventoryReport(report, now)
	unverifiedInventory, err := capacitysnapshot.ToRouteInventory(mustBuild(t, unverifiedAccounts, now), now)
	if err == nil {
		for _, candidate := range unverifiedInventory.Candidates {
			if candidate.Provider == "claude" && candidate.Model == "claude-sonnet-5" {
				t.Fatalf("static/generic MR row routed without invocation receipt: %+v", candidate)
			}
		}
	}
}

func verifiedClaudeReceipt(now time.Time, install, account, authID string) providerinventory.ClaudeCapabilityProbeReceipt {
	return providerinventory.ClaudeCapabilityProbeReceipt{
		SchemaVersion:          providerinventory.ClaudeCapabilityProbeReceiptSchema,
		Provider:               "claude",
		RequestedModel:         "sonnet",
		ActualModel:            "claude-sonnet-5",
		AcceptedEffort:         "low",
		ProviderInstallationID: install,
		AccountProfileID:       account,
		AuthReadinessID:        authID,
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
		BudgetReservationID:    "bres_claude_verified",
		ReservedTokens:         1000,
		CommittedTokens:        31,
		ReleasedTokens:         969,
		BudgetState:            "released",
		UsageRecordIDs:         []string{"usage_claude_verified"},
		Source:                 "claude-code-stream-json",
		Confidence:             providerinventory.ConfidenceExact,
		FreshnessState:         providerinventory.FreshnessFresh,
		GapReasons:             []string{},
	}
}
