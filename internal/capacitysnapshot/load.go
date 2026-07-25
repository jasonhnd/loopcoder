package capacitysnapshot

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// LoadOptions controls production inventory loading for auto-route.
type LoadOptions struct {
	RepoPath string
	Now      time.Time
	// Discover when set is used instead of providerinventory.Discover.
	Discover func(ctx context.Context, opts providerinventory.Options) (providerinventory.Report, error)
	// LoadDurable when set loads previously refreshed inventory (providers refresh).
	// When nil, production opens the default LOOPCODER_HOME store.
	// Tests may inject a fixed durable report or a no-op loader.
	LoadDurable func(ctx context.Context) (providerinventory.Report, error)
	// SkipDefaultDurableStore when true prevents opening the real home store
	// when LoadDurable is nil (unit tests only).
	SkipDefaultDurableStore bool
}

// LoadRouteInventory discovers provider capacity, rehydrates durable quota from
// a prior providers refresh when present, and maps it to autoroute.Inventory.
// Fails closed when the resulting snapshot is not unattended-eligible.
//
// Bare `run --auto-route` must NOT require a hand-passed --capacity-snapshot.
// After `providers refresh --grant-quota-telemetry`, exact/estimated windows
// with freshness/reset persist in the local store and are rehydrated here.
func LoadRouteInventory(ctx context.Context, opts LoadOptions) (autoroute.Inventory, Snapshot, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	discover := opts.Discover
	if discover == nil {
		discover = func(ctx context.Context, o providerinventory.Options) (providerinventory.Report, error) {
			return providerinventory.Discover(ctx, o, providerinventory.DefaultDeps())
		}
	}
	rep, err := discover(ctx, providerinventory.Options{
		RepoPath: opts.RepoPath,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		return autoroute.Inventory{}, Snapshot{}, fmt.Errorf("capacitysnapshot: discover: %w", err)
	}

	// Rehydrate durable quota/auth/models from providers refresh store.
	loadDurable := opts.LoadDurable
	if loadDurable == nil && !opts.SkipDefaultDurableStore {
		loadDurable = defaultLoadDurable
	}
	if loadDurable != nil {
		if durable, derr := loadDurable(ctx); derr == nil {
			rep = providerinventory.RehydrateForAutoRoute(rep, durable, now)
		}
		// Durable load errors are non-fatal: fall back to live discover honesty.
	}

	accounts := FromProviderInventoryReport(rep, now)
	if len(accounts) == 0 {
		return autoroute.Inventory{}, Snapshot{}, fmt.Errorf("%w: empty inventory report", ErrNoEligibleAccount)
	}
	snap, err := Build(accounts, now)
	if err != nil {
		return autoroute.Inventory{}, Snapshot{}, err
	}
	for _, catalog := range rep.ModelCatalogSnapshots {
		if !providerinventory.ValidClaudeVerifiedSnapshot(catalog, now) || catalog.CapabilityProbeReceipt == nil {
			continue
		}
		receipt := *catalog.CapabilityProbeReceipt
		receipt.UsageRecordIDs = append([]string(nil), receipt.UsageRecordIDs...)
		receipt.GapReasons = append([]string(nil), receipt.GapReasons...)
		snap.ClaudeCatalogReceipts = append(snap.ClaudeCatalogReceipts, receipt)
	}
	sort.SliceStable(snap.ClaudeCatalogReceipts, func(i, j int) bool {
		left, right := snap.ClaudeCatalogReceipts[i], snap.ClaudeCatalogReceipts[j]
		if left.AccountProfileID != right.AccountProfileID {
			return left.AccountProfileID < right.AccountProfileID
		}
		if left.ActualModel != right.ActualModel {
			return left.ActualModel < right.ActualModel
		}
		return left.AcceptedEffort < right.AcceptedEffort
	})
	if len(snap.ClaudeCatalogReceipts) > 0 {
		snap.Digest, err = digestOf(snap)
		if err != nil {
			return autoroute.Inventory{}, Snapshot{}, err
		}
	}
	inv, err := ToRouteInventory(snap, now)
	if err != nil {
		return autoroute.Inventory{}, snap, err
	}
	return inv, snap, nil
}

func defaultLoadDurable(ctx context.Context) (providerinventory.Report, error) {
	store, err := providerinventory.OpenDefaultStore(ctx, providerinventory.DefaultDeps(), time.Now)
	if err != nil {
		return providerinventory.Report{}, err
	}
	defer store.Close()
	return providerinventory.Load(ctx, store)
}
