package providerinventory

import (
	"testing"
	"time"
)

func TestRehydrateForAutoRouteTranslatesClaudeVerifiedCatalogReceiptWithPATHAlias(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	receipt := testClaudeCapabilityReceipt(now)
	durableInstallID := receipt.ProviderInstallationID
	const (
		liveInstallID = "pinst_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		resolvedHash  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		snapshotID    = "mcatsnap_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	snapshot := claudeVerifiedSnapshotForRehydrate(snapshotID, receipt)
	model := claudeVerifiedModelForRehydrate(snapshotID, receipt)

	live := Report{Installations: []ProviderInstallation{
		exactFreshPATHInstallForRehydrate(liveInstallID, 0, resolvedHash),
	}}
	durable := Report{
		Installations: []ProviderInstallation{
			exactFreshPATHInstallForRehydrate(durableInstallID, 1, resolvedHash),
		},
		ModelCatalogSnapshots: []ModelCatalogSnapshot{snapshot},
		ModelCapabilities:     []ModelCapability{model},
	}

	merged := RehydrateForAutoRoute(live, durable, now)
	if len(merged.ModelCatalogSnapshots) != 1 {
		t.Fatalf("verified durable catalog did not survive rehydrate: %#v", merged.ModelCatalogSnapshots)
	}
	got := merged.ModelCatalogSnapshots[0]
	if got.ProviderInstallationID == nil || *got.ProviderInstallationID != liveInstallID {
		t.Fatalf("snapshot install = %#v, want live PATH primary %q", got.ProviderInstallationID, liveInstallID)
	}
	if got.CapabilityProbeReceipt == nil || got.CapabilityProbeReceipt.ProviderInstallationID != liveInstallID {
		t.Fatalf("receipt install was not translated with snapshot: %#v", got.CapabilityProbeReceipt)
	}
	if !ValidClaudeVerifiedSnapshot(got, now) {
		t.Fatalf("translated snapshot lost verified status: %#v", got)
	}
	if len(merged.ModelCapabilities) != 1 ||
		!ValidClaudeVerifiedCapability(got, merged.ModelCapabilities[0], now) {
		t.Fatalf("verified model did not survive rehydrate: %#v", merged.ModelCapabilities)
	}

	// Rehydrate must not mutate durable evidence shared with callers/storage.
	if *durable.ModelCatalogSnapshots[0].ProviderInstallationID != durableInstallID ||
		durable.ModelCatalogSnapshots[0].CapabilityProbeReceipt.ProviderInstallationID != durableInstallID {
		t.Fatalf("durable catalog mutated in place: %#v", durable.ModelCatalogSnapshots[0])
	}
	got.CapabilityProbeReceipt.UsageRecordIDs[0] = "usage_mutated_merged_copy"
	if durable.ModelCatalogSnapshots[0].CapabilityProbeReceipt.UsageRecordIDs[0] !=
		receipt.UsageRecordIDs[0] {
		t.Fatal("merged receipt shares mutable usage evidence with durable report")
	}
}

func TestRewriteCatalogInstallIDsDoesNotRepairMismatchedClaudeReceipt(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	receipt := testClaudeCapabilityReceipt(now)
	const mismatchedInstallID = "pinst_cccccccccccccccccccccccccccccccc"
	snapshot := claudeVerifiedSnapshotForRehydrate(
		"mcatsnap_bbbbbbbbbbbbbbbbbbbbbbbbbb",
		receipt,
	)
	snapshot.CapabilityProbeReceipt.ProviderInstallationID = mismatchedInstallID
	originalSnapshotID := *snapshot.ProviderInstallationID

	rewritten := rewriteCatalogInstallIDs(
		[]ModelCatalogSnapshot{snapshot},
		map[string]string{originalSnapshotID: "pinst_dddddddddddddddddddddddddddddddd"},
	)
	if got := *rewritten[0].ProviderInstallationID; got != originalSnapshotID {
		t.Fatalf("mismatched snapshot install was repaired to %q", got)
	}
	if got := rewritten[0].CapabilityProbeReceipt.ProviderInstallationID; got != mismatchedInstallID {
		t.Fatalf("mismatched receipt install was rewritten to %q", got)
	}
	if ValidClaudeVerifiedSnapshot(rewritten[0], now) {
		t.Fatal("mismatched receipt became valid after alias rewrite")
	}
}

func claudeVerifiedSnapshotForRehydrate(snapshotID string, receipt ClaudeCapabilityProbeReceipt) ModelCatalogSnapshot {
	installID := receipt.ProviderInstallationID
	accountID := receipt.AccountProfileID
	authID := receipt.AuthReadinessID
	return ModelCatalogSnapshot{
		ModelCatalogSnapshotID: snapshotID,
		AdapterID:              "claude",
		ProviderInstallationID: &installID,
		AccountProfileID:       &accountID,
		AuthReadinessID:        &authID,
		CatalogSourceKind:      CatalogSourceProviderMachineReadable,
		CatalogSourceReference: "claude-capability-probe#" + receipt.OutputRawSHA256,
		SourceSchemaVersion:    ClaudeCapabilityProbeReceiptSchema,
		EntryCount:             1,
		Confidence:             ConfidenceExact,
		FreshnessState:         FreshnessFresh,
		CapabilityProbeReceipt: &receipt,
	}
}

func claudeVerifiedModelForRehydrate(snapshotID string, receipt ClaudeCapabilityProbeReceipt) ModelCapability {
	return ModelCapability{
		ModelCatalogSnapshotID: snapshotID,
		AdapterID:              "claude",
		CanonicalModelID:       receipt.ActualModel,
		DisplayName:            receipt.ActualModel,
		AvailabilityState:      AvailabilityAvailable,
		LifecycleState:         LifecycleAvailable,
		FreshnessState:         FreshnessFresh,
		Confidence:             ConfidenceExact,
		Constraints: []string{
			"supported_depth=" + receipt.AcceptedEffort,
			"default_depth=" + receipt.AcceptedEffort,
		},
		EntrySources: []CatalogEntrySource{{
			SourceKind:      CatalogSourceProviderMachineReadable,
			SourceReference: "claude-capability-probe#" + receipt.OutputRawSHA256,
			Confidence:      ConfidenceExact,
			FreshnessState:  FreshnessFresh,
		}},
		Source: SourceDescriptor{Kind: string(CatalogSourceProviderMachineReadable)},
	}
}

func exactFreshPATHInstallForRehydrate(id string, order int, resolvedHash string) ProviderInstallation {
	return ProviderInstallation{
		ProviderInstallationID: id,
		AdapterID:              "claude",
		ExecutableName:         "claude",
		DiscoverySource:        DiscoveryPath,
		DiscoveryOrder:         order,
		InstallationState:      InstallationInstalled,
		UsableForInvocation:    "yes",
		FreshnessState:         FreshnessFresh,
		Confidence:             ConfidenceExact,
		ExecutableIdentity: ExecutableIdentity{
			Basename:         "claude",
			ResolvedPathHash: resolvedHash,
		},
	}
}
