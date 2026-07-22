package capacitysnapshot

import (
	"context"
	"fmt"
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
}

// LoadRouteInventory discovers provider capacity and maps it to autoroute.Inventory.
// Fails closed when the resulting snapshot is not unattended-eligible.
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
	accounts := FromProviderInventoryReport(rep, now)
	if len(accounts) == 0 {
		return autoroute.Inventory{}, Snapshot{}, fmt.Errorf("%w: empty inventory report", ErrNoEligibleAccount)
	}
	snap, err := Build(accounts, now)
	if err != nil {
		return autoroute.Inventory{}, Snapshot{}, err
	}
	inv, err := ToRouteInventory(snap, now)
	if err != nil {
		return autoroute.Inventory{}, snap, err
	}
	return inv, snap, nil
}
