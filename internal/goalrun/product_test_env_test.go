package goalrun_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// productEnv freezes a production-eligible inventory + snapshot + ledger path
// for goalrun product-path tests. Never uses fixture.
//
// Depth policy for synthetic rows:
//   - codex (gpt-5.5): frozen injected low+medium+high (unit-test synthetic only).
//   - grok (grok-4.5): medium only — local `grok models` establishes presence;
//     routing catalog only justifies the explicitly proven medium depth. Do not
//     invent unsupported Grok depths.
type productEnv struct {
	Now        time.Time
	Home       string
	LedgerPath string
	Inv        autoroute.Inventory
	Snap       capacitysnapshot.Snapshot
}

func newProductEnv(t *testing.T, now time.Time, providers ...string) productEnv {
	t.Helper()
	if now.IsZero() {
		now = time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC)
	}
	if len(providers) == 0 {
		providers = []string{"codex"}
	}
	home := testHome(t)
	ledgerPath := filepath.Join(t.TempDir(), "capacity-ledger.json")
	var accounts []capacitysnapshot.AccountObservation
	var cands []eligibility.Candidate
	var softs []quotapolicy.Candidate
	for _, p := range providers {
		p = strings.ToLower(strings.TrimSpace(p))
		var model string
		var cl capclass.Class
		var depths []string
		switch p {
		case "grok":
			// Proven depth only (medium). Model presence ≠ multi-depth support.
			model, cl = "grok-4.5", capclass.ClassSoul
			depths = []string{"medium"}
		case "codex":
			// Synthetic unit rows for low/medium/high on a non-Grok production identity.
			model, cl = "gpt-5.5", capclass.ClassSoul
			depths = []string{"low", "medium", "high"}
		default:
			t.Fatalf("unsupported product test provider %q", p)
		}
		acct := "acct-" + p + "-" + strings.Repeat("a", 48)
		inst := "install-" + p
		acc := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: p, AccountRef: acct, InstallRef: inst,
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
				Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
				ResetAt: ptrTime(now.Add(2 * time.Hour)), CapturedAt: now, Source: "test-machine-observed",
			}},
			Models: []capacitysnapshot.ModelSpec{{
				ModelID: model, SupportedDepths: append([]string(nil), depths...), DefaultDepth: "medium", Present: true,
			}},
			Source: "test-machine-observed", CapturedAt: now,
		})
		accounts = append(accounts, acc)
		ok := func(id string) eligibility.Fact {
			return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
		}
		ff := func(id string) eligibility.Fact {
			return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
		}
		for _, effort := range depths {
			for _, perm := range []string{"read-only", "bounded_write"} {
				cands = append(cands, eligibility.Candidate{
					Provider: p, Model: model, Effort: effort, Permission: perm,
					ModelClass: cl, AccountRef: acct, InstallRef: inst, WindowKind: "five_hour",
					Installed: ok(p + "-i"), Authenticated: ok(p + "-a"), ModelPresent: ok(p + "-m"),
					PermissionOK: ok(p + "-p"), EffortOK: ok(p + "-e"), Healthy: ok(p + "-h"),
					CooldownActive: ff(p + "-cd"), ResourceFit: ok(p + "-r"), QuotaRemaining: 9999,
				})
			}
		}
		rf, ttr, rel := 0.9, 2*time.Hour, 0.95
		softs = append(softs, quotapolicy.Candidate{
			Provider: p, Model: model, AccountRef: acct, InstallRef: inst, WindowKind: "five_hour",
			Windows: []quotapolicy.Window{{
				Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf,
				Evidence: quotapolicy.EvidenceExact, TimeToReset: &ttr,
			}},
			Reliability: &rel, ReliabilityEvidence: quotapolicy.EvidenceExact,
		})
	}
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	inv := autoroute.Inventory{
		EvidenceDigest: "test-product-" + strings.Join(providers, "-"),
		Candidates:     cands,
		Soft:           softs,
		Machine: eligibility.MachineAdmission{
			CapacityOK:      eligibility.Fact{State: eligibility.FactTrue, EvidenceID: "mach", Freshness: eligibility.FreshFresh},
			ConcurrentSlots: 4,
		},
	}
	return productEnv{Now: now, Home: home, LedgerPath: ledgerPath, Inv: inv, Snap: snap}
}

func (e productEnv) loadInv() func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
	return func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
		return e.Inv, e.Snap, nil
	}
}

func (e productEnv) openLed() func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
	return func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
		return capacityledger.OpenPath(e.LedgerPath, nowFn)
	}
}

func (e productEnv) pinRequest(goal, issue string) goalrun.Request {
	// Default production pin: codex/gpt-5.5 when present; else grok-4.5.
	prov, model := "codex", "gpt-5.5"
	for _, c := range e.Inv.Candidates {
		if strings.EqualFold(c.Provider, "codex") {
			prov, model = c.Provider, c.Model
			break
		}
		if strings.EqualFold(c.Provider, "grok") {
			prov, model = c.Provider, c.Model
		}
	}
	return goalrun.Request{
		ProjectID: "proj-product", Goal: goal, Issue: issue, Actor: "owner", Owner: "worker",
		Provider: prov, Model: model,
		HomeDir: e.Home, Now: func() time.Time { return e.Now },
		LoadInventory: e.loadInv(),
		OpenLedger:    e.openLed(),
		Executor:      workflowrun.FakeChildExecutor{HomeDir: e.Home, Now: func() time.Time { return e.Now }},
	}
}

func (e productEnv) autoRequest(goal, issue string) goalrun.Request {
	return goalrun.Request{
		ProjectID: "proj-product-auto", Goal: goal, Issue: issue, Actor: "owner", Owner: "worker",
		HomeDir: e.Home, Now: func() time.Time { return e.Now },
		LoadInventory: e.loadInv(),
		OpenLedger:    e.openLed(),
		Executor:      workflowrun.FakeChildExecutor{HomeDir: e.Home, Now: func() time.Time { return e.Now }},
	}
}

// ledgerFileEntries reopens the durable ledger JSON and returns raw entries.
func ledgerFileEntries(t *testing.T, path string) []capacityledger.Entry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var doc struct {
		Entries []capacityledger.Entry `json:"entries"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Entries
}

func countLiveReserved(entries []capacityledger.Entry) int {
	n := 0
	for _, e := range entries {
		if e.State == "reserved" {
			n++
		}
	}
	return n
}
