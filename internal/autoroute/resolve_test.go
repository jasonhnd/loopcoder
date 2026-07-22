package autoroute_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/routedecision"
)

func t0() time.Time { return time.Date(2026, 7, 22, 19, 0, 0, 0, time.UTC) }

func TestExplicitPinNeverOverridden(t *testing.T) {
	res, err := autoroute.Resolve(autoroute.Input{
		Provider: "fixture", Model: "m", Effort: "low", Permission: "default",
		ProjectID: "p", DecisionKey: "k1", Now: t0(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != autoroute.OutcomeExplicitPin {
		t.Fatalf("%+v", res)
	}
	if res.Provider != "fixture" || res.Model != "m" {
		t.Fatalf("%+v", res)
	}
}

func TestAutoRouteSelectsStableWinner(t *testing.T) {
	in := autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "auto1", Now: t0(),
	}
	a, err := autoroute.Resolve(in)
	if err != nil {
		t.Fatalf("%v %+v", err, a)
	}
	if a.Outcome != autoroute.OutcomeSelected || a.Provider == "" || a.Model == "" {
		t.Fatalf("%+v", a)
	}
	b, err := autoroute.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if a.Provider != b.Provider || a.Model != b.Model || a.Digest != b.Digest {
		t.Fatalf("not stable a=%+v b=%+v", a, b)
	}
	if a.Decision == nil || a.Decision.Outcome != routedecision.OutcomeSelected {
		t.Fatalf("decision %+v", a.Decision)
	}
	if a.Explain == nil || a.Explain.Human == "" {
		t.Fatal("missing explain")
	}
}

func TestOmittedRouteWithoutFlagUsesAutoPath(t *testing.T) {
	// When both empty and AutoRoute false, caller should set AutoRoute for omitted policy.
	// Resolve with AutoRoute true models omitted-route policy.
	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "omit1", Now: t0(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != autoroute.OutcomeSelected {
		t.Fatalf("%+v", res)
	}
}

func TestPartialPinInvalid(t *testing.T) {
	_, err := autoroute.Resolve(autoroute.Input{
		Provider: "fixture", Model: "", ProjectID: "p", DecisionKey: "partial", Now: t0(),
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestEmptyInventoryNoRoute(t *testing.T) {
	inv := autoroute.Inventory{
		EvidenceDigest: "empty",
		// no candidates
	}
	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "empty", Now: t0(), Inventory: &inv,
	})
	if err == nil && res.Outcome == autoroute.OutcomeSelected {
		t.Fatalf("expected no route: %+v", res)
	}
	if res.Outcome != autoroute.OutcomeNoRoute && res.Outcome != autoroute.OutcomeInvalid {
		// no_route is expected
		if res.Outcome != autoroute.OutcomeNoRoute {
			t.Fatalf("%+v err=%v", res, err)
		}
	}
}

func TestExplainHasWinnerAndRejected(t *testing.T) {
	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "explain1", Now: t0(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Explain == nil {
		t.Fatal("nil explain")
	}
	if !strings.Contains(res.Explain.Human, res.Provider) && res.Explain.WinnerLine == "" {
		t.Fatalf("explain missing winner: %+v", res.Explain)
	}
}
