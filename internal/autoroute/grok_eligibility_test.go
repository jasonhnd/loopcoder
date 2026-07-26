package autoroute_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

// Grok medium-only frozen inventory: model presence via catalog; only medium is
// an explicitly justified routing depth (do not invent low/high for grok-4.5).
func grokMediumOnlyInv(t *testing.T) *autoroute.Inventory {
	t.Helper()
	ok := func(id string) eligibility.Fact {
		return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	ff := func(id string) eligibility.Fact {
		return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	acct := "acct-grok-" + strings.Repeat("g", 48)
	mk := func(perm string) eligibility.Candidate {
		return eligibility.Candidate{
			Provider: "grok", Model: "grok-4.5", Effort: "medium", Permission: perm,
			ModelClass: capclass.ClassSoul,
			AccountRef: acct, InstallRef: "install-grok", WindowKind: "five_hour",
			Installed: ok("i"), Authenticated: ok("a"), ModelPresent: ok("m"),
			PermissionOK: ok("p"), EffortOK: ok("e"), Healthy: ok("h"),
			CooldownActive: ff("cd"), ResourceFit: ok("r"), QuotaRemaining: 9999,
		}
	}
	rf, ttr, rel := 0.85, time.Hour, 0.9
	return &autoroute.Inventory{
		EvidenceDigest: "test-grok-medium-only",
		Candidates:     []eligibility.Candidate{mk("bounded_write"), mk("read-only")},
		Soft: []quotapolicy.Candidate{{
			Provider: "grok", Model: "grok-4.5", AccountRef: acct, InstallRef: "install-grok", WindowKind: "five_hour",
			Windows: []quotapolicy.Window{{
				Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf,
				Evidence: quotapolicy.EvidenceExact, TimeToReset: &ttr,
			}},
			Reliability: &rel, ReliabilityEvidence: quotapolicy.EvidenceExact,
		}},
		Machine: eligibility.MachineAdmission{CapacityOK: ok("mach"), ConcurrentSlots: 4},
	}
}

func TestGrokExplicitPinAndAutoShareHardEligibility(t *testing.T) {
	now := time.Date(2026, 7, 23, 15, 0, 0, 0, time.UTC)
	inv := grokMediumOnlyInv(t)

	// Explicit pin uses BindExplicitPinWithClass (same hard path as auto eligibility).
	bound, err := autoroute.BindExplicitPinWithClass("grok", "grok-4.5", "medium", "bounded_write", capclass.ClassSoul, inv)
	if err != nil {
		t.Fatalf("pin bind: %v", err)
	}
	if bound.Provider != "grok" || bound.Model != "grok-4.5" || bound.Effort != "medium" {
		t.Fatalf("pin identity: %+v", bound)
	}
	if bound.AccountRef == "" || bound.InstallRef == "" || bound.WindowKind != "five_hour" {
		t.Fatalf("pin missing capacity identity: %+v", bound)
	}

	// Stale fact must fail pin (same hard eligibility — no unknown/stale accepted).
	bad := *inv
	bad.Candidates = append([]eligibility.Candidate(nil), inv.Candidates...)
	bad.Candidates[0].Authenticated = eligibility.Fact{
		State: eligibility.FactTrue, EvidenceID: "stale-auth", Freshness: eligibility.FreshStale,
	}
	if _, err := autoroute.BindExplicitPinWithClass("grok", "grok-4.5", "medium", "bounded_write", capclass.ClassSoul, &bad); err == nil {
		t.Fatal("stale auth must fail pin bind")
	} else if !errors.Is(err, autoroute.ErrPinFail) && !strings.Contains(err.Error(), "pin") {
		t.Fatalf("want pin fail, got %v", err)
	}

	// FreshUnknown State=true must fail on every pin-matching candidate.
	bad2 := *inv
	bad2.Candidates = append([]eligibility.Candidate(nil), inv.Candidates...)
	for i := range bad2.Candidates {
		bad2.Candidates[i].Healthy = eligibility.Fact{
			State: eligibility.FactTrue, EvidenceID: "unk-h", Freshness: eligibility.FreshUnknown,
		}
	}
	if _, err := autoroute.BindExplicitPinWithClass("grok", "grok-4.5", "medium", "bounded_write", capclass.ClassSoul, &bad2); err == nil {
		t.Fatal("FreshUnknown healthy must fail pin")
	}

	// Auto-route medium selects grok; inventing high depth fails closed (no fictional depth).
	sel, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "grok-auto-med",
		Inventory: inv, Effort: "medium", Permission: "bounded_write",
		TaskClass: capclass.ClassSoul, Now: now,
	})
	if err != nil || sel.Outcome != autoroute.OutcomeSelected {
		t.Fatalf("auto medium: %+v err=%v", sel, err)
	}
	if sel.Provider != "grok" || sel.Model != "grok-4.5" {
		t.Fatalf("auto winner: %+v", sel)
	}
	if sel.AccountRef == "" || sel.InstallRef == "" {
		t.Fatalf("auto missing capacity identity before reserve: %+v", sel)
	}

	// Unsupported high depth: no invent — fail closed.
	noHigh, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "grok-auto-high",
		Inventory: inv, Effort: "high", Permission: "bounded_write",
		TaskClass: capclass.ClassSoul, Now: now,
	})
	if err == nil && noHigh.Outcome == autoroute.OutcomeSelected {
		t.Fatalf("high depth must not invent route: %+v", noHigh)
	}

	// Explicit pin capability for high depth still fails closed when inventory has no high.
	if _, err := autoroute.BindExplicitPinWithClass("grok", "grok-4.5", "high", "bounded_write", capclass.ClassSoul, inv); err == nil {
		t.Fatal("pin high must fail without high inventory row")
	}
}
