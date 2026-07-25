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
		inst   = "pinst_codex_load_rehydrate"
		acc    = "acct-codex-load-rehydrate"
		catSnap = "mcatsnap_codex_load"
	)
	live := providerinventory.Report{
		InventoryFingerprint: "live-fp",
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("codex", inst, "sha256:codex-load-resolved", "sha256:codex-load-path"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
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
			FreshnessState:         providerinventory.FreshnessFresh,
			CapturedAt:             now.Format(time.RFC3339),
			StaleAfter:             now.Add(24 * time.Hour).Format(time.RFC3339),
			ResetAt:                now.Add(48 * time.Hour).Format(time.RFC3339),
			ProviderQuantityName:   "primary_used_percent",
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
