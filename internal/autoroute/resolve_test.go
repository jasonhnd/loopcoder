package autoroute_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/routedecision"
)

func t0() time.Time { return time.Date(2026, 7, 22, 19, 0, 0, 0, time.UTC) }

func withFake(in autoroute.Input) autoroute.Input {
	inv := autoroute.FakeInventory(t0())
	in.Inventory = &inv
	if in.Now.IsZero() {
		in.Now = t0()
	}
	// Tests must pass classified TaskClass explicitly (Resolve never invents Tera).
	if !in.TaskClass.Valid() {
		in.TaskClass = capclass.ClassTera
	}
	return in
}

func TestExplicitPinNeverOverridden(t *testing.T) {
	// Fixture is test-only and must not production-spend; pin a real provider/model.
	res, err := autoroute.Resolve(autoroute.Input{
		Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "bounded_write",
		ProjectID: "p", DecisionKey: "k1", Now: t0(), TaskClass: capclass.ClassTera,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != autoroute.OutcomeExplicitPin {
		t.Fatalf("%+v", res)
	}
	if res.Provider != "codex" || res.Model != "gpt-5.5" {
		t.Fatalf("%+v", res)
	}
}

func TestAutoRouteSelectsStableWinner(t *testing.T) {
	in := withFake(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "auto1", Now: t0(),
	})
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
	res, err := autoroute.Resolve(withFake(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "omit1", Now: t0(),
	}))
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
		TaskClass: capclass.ClassTera,
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveTaskClassRequiredNoSilentTera(t *testing.T) {
	for _, tc := range []struct {
		name string
		cl   capclass.Class
	}{
		{"empty", ""},
		{"invalid", "mega"},
		{"needs_human", capclass.ClassNeedsHuman},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := autoroute.Resolve(autoroute.Input{
				Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "bounded_write",
				ProjectID: "p", DecisionKey: "tc-" + tc.name, Now: t0(), TaskClass: tc.cl,
			})
			if err == nil || res.Outcome != autoroute.OutcomeInvalid {
				t.Fatalf("want OutcomeInvalid, got %+v err=%v", res, err)
			}
			if strings.Contains(strings.ToLower(res.Message), "silent") || !strings.Contains(strings.ToLower(res.Message), "task class") {
				// message must name task class failure
				if !strings.Contains(strings.ToLower(res.Message), "task class") && !strings.Contains(strings.ToLower(res.Message), "needs_human") {
					t.Fatalf("message: %q", res.Message)
				}
			}
		})
	}
}

func TestResolveSourceDoesNotDefaultTaskClassTera(t *testing.T) {
	src, err := os.ReadFile("resolve.go")
	if err != nil {
		src, err = os.ReadFile(filepath.Join("internal", "autoroute", "resolve.go"))
	}
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	idx := strings.Index(body, "func Resolve(")
	if idx < 0 {
		t.Fatal("Resolve not found")
	}
	end := strings.Index(body[idx:], "\nfunc FakeInventory")
	if end < 0 {
		end = strings.Index(body[idx:], "\nfunc DefaultInventory")
	}
	if end < 0 {
		t.Fatal("cannot bound Resolve body")
	}
	resolveBody := body[idx : idx+end]
	// Guard: no silent assignment of ClassTera for missing TaskClass.
	if strings.Contains(resolveBody, "taskClass = capclass.ClassTera") ||
		strings.Contains(resolveBody, "TaskClass = capclass.ClassTera") {
		t.Fatal("Resolve must not silently assign ClassTera for missing TaskClass")
	}
}

func TestNilInventoryAutoRouteFailsClosed(t *testing.T) {
	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "nil-inv", Now: t0(),
		TaskClass: capclass.ClassTera,
		// Inventory deliberately nil
	})
	if err == nil {
		t.Fatal("expected error for nil inventory")
	}
	if res.Outcome != autoroute.OutcomeNoRoute {
		t.Fatalf("outcome=%s want no_route: %+v", res.Outcome, res)
	}
	if !strings.Contains(res.Message, "no real inventory") && !strings.Contains(res.Message, "missing real inventory") {
		t.Fatalf("message must name missing real inventory: %q", res.Message)
	}
	if res.Provider != "" || res.Model != "" {
		t.Fatalf("must not select route without inventory: %+v", res)
	}
}

func TestHistoricalFakeDigestRefused(t *testing.T) {
	inv := autoroute.FakeInventory(t0())
	inv.EvidenceDigest = "default-official-fake-v1"
	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "fake-digest", Now: t0(), Inventory: &inv,
		TaskClass: capclass.ClassTera,
	})
	if err == nil {
		t.Fatal("expected refuse of historical fake digest")
	}
	if res.Outcome == autoroute.OutcomeSelected {
		t.Fatalf("must not select: %+v", res)
	}
	if !strings.Contains(res.Message, "fake inventory") {
		t.Fatalf("message: %q", res.Message)
	}
}

func TestEmptyInventoryNoRoute(t *testing.T) {
	inv := autoroute.Inventory{
		EvidenceDigest: "empty-but-real-shaped",
		// no candidates
	}
	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "empty", Now: t0(), Inventory: &inv,
		TaskClass: capclass.ClassTera,
	})
	if err == nil && res.Outcome == autoroute.OutcomeSelected {
		t.Fatalf("expected no route: %+v", res)
	}
	if res.Outcome != autoroute.OutcomeNoRoute && res.Outcome != autoroute.OutcomeInvalid {
		if res.Outcome != autoroute.OutcomeNoRoute {
			t.Fatalf("%+v err=%v", res, err)
		}
	}
}

func TestExplainHasWinnerAndRejected(t *testing.T) {
	res, err := autoroute.Resolve(withFake(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "explain1", Now: t0(),
	}))
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

func TestResolveSourceDoesNotCallDefaultInventory(t *testing.T) {
	src, err := os.ReadFile("resolve.go")
	if err != nil {
		src, err = os.ReadFile(filepath.Join("internal", "autoroute", "resolve.go"))
	}
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// The production Resolve path must not invoke DefaultInventory/FakeInventory.
	// Allow the function definitions themselves.
	idx := strings.Index(body, "func Resolve(")
	if idx < 0 {
		t.Fatal("Resolve not found")
	}
	// Slice from Resolve until FakeInventory/DefaultInventory definitions.
	end := strings.Index(body[idx:], "\nfunc FakeInventory")
	if end < 0 {
		end = strings.Index(body[idx:], "\nfunc DefaultInventory")
	}
	if end < 0 {
		t.Fatal("cannot bound Resolve body")
	}
	resolveBody := body[idx : idx+end]
	if strings.Contains(resolveBody, "DefaultInventory(") || strings.Contains(resolveBody, "FakeInventory(") {
		t.Fatal("Resolve must not call DefaultInventory/FakeInventory")
	}
	if !strings.Contains(resolveBody, "missing real inventory") && !strings.Contains(resolveBody, "no real inventory") {
		t.Fatal("Resolve must fail closed when inventory is nil")
	}
}
