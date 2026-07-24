package goalrun_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// TestExecute_DiversifyUsedProvider_AtomicAlternateIdentity calls goalrun.Execute
// (product path) with the same multi-provider inventory style as capacity accounting
// tests. Asserts multi-provider children and that no child retains a mismatched
// foreign account for its provider after diversification.
func TestExecute_DiversifyUsedProvider_AtomicAlternateIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOOPCODER_HOME", home)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex-div-raw", InstallRef: "install-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 20, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 80, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTimeDiv(now.Add(45 * time.Minute)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	// Second company must be account-affirmable (grok). Gemini cannot affirm
	// AccountRef/depth and is hard-excluded from capacity-bound product routes.
	grok := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "grok", AccountRef: "acct-grok-div-raw", InstallRef: "install-grok",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTimeDiv(now.Add(30 * time.Minute)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "grok-4.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex, grok}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(t.TempDir(), "capacity-ledger.json")
	canonCodex := capacityledger.CanonicalAccountRef("acct-codex-div-raw")
	canonGrok := capacityledger.CanonicalAccountRef("acct-grok-div-raw")

	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj-div-atom", Goal: "implement multi-provider capacity-aware routing with tests",
		Issue: "1397", Actor: "owner",
		HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now },
		},
		Now: func() time.Time { return now },
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
	})
	if err != nil {
		t.Fatalf("Execute product path: %v status=%s msg=%s children=%+v", err, res.Status, res.Message, res.Children)
	}
	if len(res.Children) < 2 {
		t.Fatalf("need multi-child plan, got %d", len(res.Children))
	}

	// Product path must produce provider diversity when inventory has two companies.
	if len(res.ProvidersUsed) < 2 {
		t.Fatalf("want multi-provider diversification (usedProviders), got providers=%v children=%+v",
			res.ProvidersUsed, res.Children)
	}

	// Atomic identity: provider-bound account must not cross-wire to the other company.
	for _, c := range res.Children {
		if c.Unavailable {
			t.Fatalf("unexpected unavailable child: %+v", c)
		}
		if c.Provider == "" || c.Model == "" || c.Depth == "" {
			t.Fatalf("incomplete route: %+v", c)
		}
		switch strings.ToLower(c.Provider) {
		case "codex":
			if c.AccountRef == canonGrok {
				t.Fatalf("codex child has grok account (cross-wire): %+v", c)
			}
		case "grok":
			if c.AccountRef == canonCodex {
				t.Fatalf("grok child has codex account (cross-wire): %+v", c)
			}
		}
	}
	for _, c := range res.Workflow.Children {
		if c.Provider == "codex" && c.AccountRef == canonGrok {
			t.Fatalf("workflow outcome cross-wire: %+v", c)
		}
		if c.Provider == "grok" && c.AccountRef == canonCodex {
			t.Fatalf("workflow outcome cross-wire: %+v", c)
		}
	}
}

func ptrTimeDiv(t time.Time) *time.Time { return &t }
