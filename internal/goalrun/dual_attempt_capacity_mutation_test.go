package goalrun

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// TestDualAttempt_CapacityMisattributeMutations proves finalize and attempt-keyed
// capacity binding refuse swap/missing-install/duplicate AttemptID green paths.
func TestDualAttempt_CapacityMisattributeMutations(t *testing.T) {
	now := time.Date(2026, 7, 25, 20, 0, 0, 0, time.UTC)
	led, err := capacityledger.OpenPath(filepath.Join(t.TempDir(), "cap.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	acct := capacityledger.CanonicalAccountRef("mut-acct")
	mk := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: acct, InstallRef: "install-mut",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: func() *time.Time { t := now.Add(2 * time.Hour); return &t }(), CapturedAt: now, Source: "test-machine-observed",
		}},
		Models: []capacitysnapshot.ModelSpec{{ModelID: "gpt-5.5", SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true}},
		Source: "test-machine-observed", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{mk}, now)
	if err != nil {
		t.Fatal(err)
	}
	priorAtt, altAtt := "att-mut-g0", "att-mut-g1"
	for _, att := range []string{priorAtt, altAtt} {
		if _, rerr := led.Reserve(capacityledger.ReserveInput{
			ProjectID: "p-mut", RunID: "r-mut", AttemptID: att,
			PlanDigest: "sha256:plan", GraphDigest: "sha256:graph", TaskClass: "tera",
			ChildContractDigest: "sha256:ccd", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
			AccountRef: acct, InstallRef: "install-mut", WindowKind: "five_hour", Snapshot: &snap,
		}); rerr != nil {
			t.Fatal(rerr)
		}
	}
	if _, err := led.Release("p-mut", "r-mut", priorAtt, "model_unavailable_supersede"); err != nil {
		t.Fatal(err)
	}
	if _, err := led.Reconcile("p-mut", "r-mut", altAtt, 0.01, "provider_usage"); err != nil {
		t.Fatal(err)
	}

	seed := []workflowrun.CapacityTransition{
		{Role: "prior", AttemptID: priorAtt, Permission: "bounded_write"},
		{Role: "alternate", AttemptID: altAtt, Permission: "bounded_write"},
	}
	fin := finalizeCapacityTransitions(led, "p-mut", "r-mut", seed)
	if len(fin) != 2 {
		t.Fatalf("want full finalize, got %+v", fin)
	}
	if fin[0].InstallRef == "" || fin[0].Permission != "bounded_write" {
		t.Fatalf("prior missing install/permission: %+v", fin[0])
	}
	if fin[0].ResetAt == nil {
		t.Fatalf("prior missing reset_at: %+v", fin[0])
	}
	if fin[0].BeforeSource == "" || fin[0].BeforeFreshness == "" || fin[0].BeforeConfidence == "" {
		t.Fatalf("prior missing before metadata: %+v", fin[0])
	}
	if fin[1].InstallRef == "" || fin[1].Permission != "bounded_write" {
		t.Fatalf("alt missing install/permission: %+v", fin[1])
	}
	// Distinct reservations.
	if fin[0].ReservationID == "" || fin[0].ReservationID == fin[1].ReservationID {
		t.Fatalf("want distinct reservation ids: %+v", fin)
	}

	// Mutation: same AttemptID on prior+alternate → nil (cannot green).
	if got := finalizeCapacityTransitions(led, "p-mut", "r-mut", []workflowrun.CapacityTransition{
		{Role: "prior", AttemptID: priorAtt},
		{Role: "alternate", AttemptID: priorAtt},
	}); got != nil {
		t.Fatalf("duplicate attempt seed must not finalize: %+v", got)
	}
	// Mutation: incomplete role set → nil.
	if got := finalizeCapacityTransitions(led, "p-mut", "r-mut", []workflowrun.CapacityTransition{
		{Role: "prior", AttemptID: priorAtt},
	}); got != nil {
		t.Fatalf("single transition must not finalize: %+v", got)
	}
	// Explicit project/run only — never hold-map namespace inference.
	// Full route identity + permission (ledger has no permission field).
	mkRep := func(att, term, fc string) ChildReport {
		return ChildReport{
			ChildID: "wi", AttemptID: att, Provider: "codex", Model: "gpt-5.5", Depth: "medium",
			Permission: "bounded_write", TaskClass: "tera",
			ExecutionPlanDigest: "sha256:plan", ChildContractDigest: "sha256:ccd",
			Terminal: term, FailureClass: fc, CapacityState: "reserved",
		}
	}
	children := []ChildReport{
		mkRep(priorAtt, "failed", "model_unavailable"),
		mkRep(altAtt, "succeeded", ""),
	}
	if err := populateCapacityFromLedgerByAttempt(children, led, "p-mut", "r-mut", "sha256:plan", "sha256:graph"); err != nil {
		t.Fatal(err)
	}
	if children[0].ReservationID == "" || children[1].ReservationID == "" {
		t.Fatalf("ledger populate missing reservations: %+v", children)
	}
	if children[0].ReservationID == children[1].ReservationID {
		t.Fatalf("failed inherited winner reservation (misattribute): %s", children[0].ReservationID)
	}
	if children[0].WindowKind == "" || children[0].InstallRef == "" {
		t.Fatalf("failed missing window/install: %+v", children[0])
	}

	// WorkItemID-keyed hold cannot reconcile/affect either MU attempt.
	holdsBad := map[string]capacityHold{
		"wi": {projectID: "p-mut", runID: "r-mut", attemptID: altAtt},
	}
	kids2 := []ChildReport{
		mkRep(priorAtt, "failed", "model_unavailable"),
		mkRep(altAtt, "succeeded", ""),
	}
	if err := populateCapacityFromLedgerByAttempt(kids2, led, "p-mut", "r-mut", "sha256:plan", "sha256:graph"); err != nil {
		t.Fatal(err)
	}
	priorRes, altRes := kids2[0].ReservationID, kids2[1].ReservationID
	reconcileCapacityGroups(kids2, led, holdsBad, &snap, false)
	if kids2[0].ReservationID != priorRes || kids2[1].ReservationID != altRes {
		t.Fatalf("WorkItemID-keyed hold mutated reservations: prior %s→%s alt %s→%s",
			priorRes, kids2[0].ReservationID, altRes, kids2[1].ReservationID)
	}
	if strings.Contains(kids2[0].CapacityNote, "reconciled=") || strings.Contains(kids2[1].CapacityNote, "reconciled=") {
		t.Fatalf("WorkItemID-keyed hold must not reconcile: %q %q", kids2[0].CapacityNote, kids2[1].CapacityNote)
	}

	// Route identity conflict fails closed.
	forged := mkRep(priorAtt, "failed", "model_unavailable")
	forged.Provider = "forged-provider"
	if err := populateCapacityFromLedgerByAttempt([]ChildReport{forged}, led, "p-mut", "r-mut", "sha256:plan", "sha256:graph"); err == nil {
		t.Fatal("provider conflict must fail closed")
	}
	// Empty ledger field mutation: plan mismatch.
	if err := populateCapacityFromLedgerByAttempt(children, led, "p-mut", "r-mut", "sha256:wrong-plan", "sha256:graph"); err == nil {
		t.Fatal("plan digest mismatch must fail closed")
	}
	if err := populateCapacityFromLedgerByAttempt(children, led, "", "r-mut", "sha256:plan", "sha256:graph"); err == nil {
		t.Fatal("empty project_id must fail")
	}
	// Missing permission is fatal.
	noPerm := mkRep(priorAtt, "failed", "model_unavailable")
	noPerm.Permission = ""
	if err := populateCapacityFromLedgerByAttempt([]ChildReport{noPerm}, led, "p-mut", "r-mut", "sha256:plan", "sha256:graph"); err == nil {
		t.Fatal("missing permission must fail closed")
	}
	if err := requireUniqueAttemptIDs([]ChildReport{
		{ChildID: "wi", Terminal: "succeeded", Provider: "codex"},
	}); err == nil {
		t.Fatal("empty AttemptID on terminal must fail")
	}
	if err := requireUniqueAttemptIDs(children); err != nil {
		t.Fatal(err)
	}
	if err := requireUniqueAttemptIDs([]ChildReport{
		{ChildID: "wi", AttemptID: altAtt, Terminal: "succeeded"},
		{ChildID: "wi2", AttemptID: altAtt, Terminal: "succeeded"},
	}); err == nil {
		t.Fatal("duplicate AttemptID must fail")
	}
}
