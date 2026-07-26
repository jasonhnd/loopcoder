package capacitysnapshot_test

import (
	"context"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func TestLoadRouteInventoryRehydratesDurableQuotaWithoutSnapshotFlag(t *testing.T) {
	now := time.Date(2026, 7, 22, 16, 30, 0, 0, time.UTC)
	rem := int64(97)

	// Live discover path: PATH install + auth ready + models, but quota not granted.
	const (
		inst    = "pinst_codex_load_rehydrate"
		acc     = "acct-codex-load-rehydrate"
		catSnap = "mcatsnap_codex_load"
	)
	live := providerinventory.Report{
		InventoryFingerprint: "live-fp",
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("codex", inst, "sha256:codex-load-resolved", "sha256:codex-load-path"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			ReadinessConfidence:    providerinventory.ConfidenceExact,
			AccountProfileID:       ptrStr(acc),
			ProviderInstallationID: ptrStr(inst),
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: catSnap, AdapterID: "codex",
			CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(inst),
			AccountProfileID:       ptrStr(acc),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			ModelCatalogSnapshotID: catSnap,
			AvailabilityState:      providerinventory.AvailabilityAvailable,
			LifecycleState:         providerinventory.LifecycleAvailable,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind: providerinventory.CatalogSourceProviderMachineReadable,
				Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				SourceReference: "provider-machine-readable:codex:test",
			}},
			Source: providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", QuotaSnapshotID: "live-denied",
			AccountProfileID:       ptrStr(acc),
			ProviderInstallationID: ptrStr(inst),
			Confidence:             providerinventory.ConfidenceUnavailable,
			FreshnessState:         providerinventory.FreshnessNotApplicable,
			GapReasons:             []string{"quota-collection-not-granted"},
			CapturedAt:             now.Format(time.RFC3339),
		}},
	}
	durable := providerinventory.Report{
		QuotaTelemetrySources: []providerinventory.QuotaTelemetrySource{{
			AdapterID: "codex", QuotaSourceID: "src-d",
			SourceKind: providerinventory.QuotaSourceOfficialCLICommand,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", QuotaSnapshotID: "dur-ok", QuotaSourceID: "src-d",
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			AccountProfileID:       ptrStr(acc),
			ProviderInstallationID: ptrStr(inst),
			RemainingValue:         &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState:       providerinventory.FreshnessFresh,
			CapturedAt:           now.Format(time.RFC3339),
			StaleAfter:           now.Add(24 * time.Hour).Format(time.RFC3339),
			ResetAt:              now.Add(48 * time.Hour).Format(time.RFC3339),
			ProviderQuantityName: "primary_used_percent",
		}},
	}

	inv, snap, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now:                     now,
		SkipDefaultDurableStore: true,
		Discover: func(ctx context.Context, opts providerinventory.Options) (providerinventory.Report, error) {
			return live, nil
		},
		LoadDurable: func(ctx context.Context) (providerinventory.Report, error) {
			return durable, nil
		},
	})
	if err != nil {
		t.Fatalf("LoadRouteInventory: %v", err)
	}
	if !snap.UnattendedOK {
		t.Fatalf("expected unattended after durable rehydrate; reasons=%v", snap.Reasons)
	}
	if len(inv.Candidates) == 0 {
		t.Fatal("expected route candidates from rehydrated capacity")
	}
	if inv.Candidates[0].Provider != "codex" {
		t.Fatalf("provider=%s", inv.Candidates[0].Provider)
	}
}

func TestLoadRouteInventoryRehydratesClaudeVerifiedReceiptAcrossPATHAlias(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	const (
		liveInstall    = "pinst_claude_live_primary"
		durableInstall = "pinst_claude_durable_alias"
		account        = "acct-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		authID         = "auth_claude_verified_alias"
		snapshotID     = "mcatsnap_claude_verified_alias"
		resolvedHash   = "sha256:claude-same-resolved-binary"
	)
	remaining, limit := int64(96), int64(100)
	receipt := verifiedClaudeReceipt(now, durableInstall, account, authID)
	liveInst := exactFreshInstall("claude", liveInstall, resolvedHash, "sha256:live-path")
	liveInst.DiscoveryOrder = 0
	durableInst := exactFreshInstall("claude", durableInstall, resolvedHash, "sha256:durable-path")
	durableInst.DiscoveryOrder = 1

	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{liveInst},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AuthReadinessID:        authID,
			AdapterID:              "claude",
			ReadinessState:         providerinventory.ReadinessReady,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			ReadinessConfidence:    providerinventory.ConfidenceExact,
			AccountProfileID:       ptrStr(account),
			ProviderInstallationID: ptrStr(liveInstall),
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "claude", QuotaSnapshotID: "live-unavailable",
			Confidence:     providerinventory.ConfidenceUnavailable,
			FreshnessState: providerinventory.FreshnessNotApplicable,
			CapturedAt:     now.Format(time.RFC3339Nano),
		}},
	}
	durable := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{durableInst},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: snapshotID,
			AdapterID:              "claude",
			ProviderInstallationID: ptrStr(durableInstall),
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
			ModelCatalogSnapshotID: snapshotID,
			AdapterID:              "claude",
			CanonicalModelID:       receipt.ActualModel,
			DisplayName:            receipt.ActualModel,
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
			AdapterID: "claude", QuotaSnapshotID: "durable-claude-quota",
			WindowKind: providerinventory.WindowRolling, Unit: "percent",
			RemainingValue: &remaining, LimitValue: &limit,
			AccountProfileID:       ptrStr(account),
			ProviderInstallationID: ptrStr(durableInstall),
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			CapturedAt:             now.Format(time.RFC3339Nano),
			StaleAfter:             now.Add(time.Hour).Format(time.RFC3339Nano),
			ResetAt:                now.Add(5 * time.Hour).Format(time.RFC3339Nano),
		}},
	}

	inventory, snapshot, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now:                     now,
		SkipDefaultDurableStore: true,
		Discover: func(context.Context, providerinventory.Options) (providerinventory.Report, error) {
			return live, nil
		},
		LoadDurable: func(context.Context) (providerinventory.Report, error) {
			return durable, nil
		},
	})
	if err != nil {
		t.Fatalf("LoadRouteInventory: %v", err)
	}
	if len(snapshot.ClaudeCatalogReceipts) != 1 {
		t.Fatalf("Claude receipts = %#v, want exactly one", snapshot.ClaudeCatalogReceipts)
	}
	gotReceipt := snapshot.ClaudeCatalogReceipts[0]
	if gotReceipt.ProviderInstallationID != liveInstall ||
		gotReceipt.AccountProfileID != account ||
		gotReceipt.OutputRawSHA256 != receipt.OutputRawSHA256 {
		t.Fatalf("receipt binding/provenance changed incorrectly: %#v", gotReceipt)
	}
	found := false
	for _, candidate := range inventory.Candidates {
		if candidate.Provider == "claude" && candidate.Model == receipt.ActualModel &&
			candidate.Effort == "low" && candidate.Permission == "read-only" {
			found = true
			if candidate.AccountRef != account || candidate.InstallRef != liveInstall {
				t.Fatalf("candidate identity = %+v", candidate)
			}
		}
	}
	if !found {
		t.Fatalf("verified Claude candidate absent after production load: %+v", inventory.Candidates)
	}
}

func TestLoadRouteInventoryFailClosedWithoutDurableOrUsableQuota(t *testing.T) {
	now := time.Date(2026, 7, 22, 16, 30, 0, 0, time.UTC)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind: providerinventory.CatalogSourceProviderMachineReadable,
				Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				SourceReference: "provider-machine-readable:codex:test",
			}},
			Source: providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", Confidence: providerinventory.ConfidenceUnavailable,
			FreshnessState: providerinventory.FreshnessNotApplicable,
			GapReasons:     []string{"quota-collection-not-granted"},
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	_, snap, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now:                     now,
		SkipDefaultDurableStore: true,
		Discover: func(ctx context.Context, opts providerinventory.Options) (providerinventory.Report, error) {
			return live, nil
		},
		LoadDurable: func(ctx context.Context) (providerinventory.Report, error) {
			return providerinventory.Report{}, nil
		},
	})
	// Build succeeds but unattended false; ToRouteInventory fails closed.
	if err == nil {
		t.Fatal("expected fail-closed without usable capacity")
	}
	if snap.UnattendedOK {
		t.Fatal("must not be unattended without durable quota")
	}
}
