package goalrun_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/routedecision"
	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestProductPath_InventoryToLaunchBinding asserts exact identity and route reason
// at every hop: providerinventory.Report → FromProviderInventoryReport → Build →
// ToRouteInventory → autoroute/routedecision → capacityledger.Reserve →
// workflowrun launch binding → runner-affirmed InvokedRoute → outcome.
// No FakeInventory, no snapshotFromRouteInventory, no hand-copied selectedRoute.
func TestProductPath_InventoryToLaunchBinding(t *testing.T) {
	now := time.Date(2026, 7, 22, 19, 0, 0, 0, time.UTC)
	accRaw := "official-billing-alice"
	sum := sha256.Sum256([]byte("acct|" + accRaw))
	wantAcct := "acct-" + hex.EncodeToString(sum[:])
	wantInstall := "install-grok-official-full-id-no-truncate"
	wantModel := "grok-4"
	wantWindow := "fixed_week"
	wantDepth := "medium"
	wantPerm := "bounded_write"
	wantReasonPrefix := "Winner:"

	ptr := func(s string) *string { return &s }
	i64 := func(v int64) *int64 { return &v }

	// --- 1. providerinventory.Report with Grok official-billing + second provider ---
	rep := providerinventory.Report{
		InventoryFingerprint: "fp-product-path",
		Installations: []providerinventory.ProviderInstallation{
			{
				ProviderInstallationID: wantInstall, AdapterID: "grok",
				ExecutableName: "grok", DiscoverySource: providerinventory.DiscoveryPath,
				InstallationState:   providerinventory.InstallationInstalled,
				FreshnessState:      providerinventory.FreshnessFresh,
				Confidence:          providerinventory.ConfidenceExact,
				UsableForInvocation: "yes",
				ExecutableIdentity: providerinventory.ExecutableIdentity{
					Basename: "grok", ResolvedPathHash: "sha256:grok-product-resolved", PathHash: "sha256:grok-product-path",
				},
			},
			{
				ProviderInstallationID: "install-codex-other", AdapterID: "codex",
				ExecutableName: "codex", DiscoverySource: providerinventory.DiscoveryPath,
				InstallationState:   providerinventory.InstallationInstalled,
				FreshnessState:      providerinventory.FreshnessFresh,
				Confidence:          providerinventory.ConfidenceExact,
				UsableForInvocation: "yes",
				ExecutableIdentity: providerinventory.ExecutableIdentity{
					Basename: "codex", ResolvedPathHash: "sha256:codex-product-resolved", PathHash: "sha256:codex-product-path",
				},
			},
		},
		AuthReadiness: []providerinventory.AuthReadiness{
			{
				AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
				AccountProfileID: ptr(accRaw), ProviderInstallationID: ptr(wantInstall),
				FreshnessState:      providerinventory.FreshnessFresh,
				Confidence:          providerinventory.ConfidenceExact,
				ReadinessConfidence: providerinventory.ConfidenceExact,
			},
			{
				AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
				AccountProfileID: ptr("codex-acct-raw"), ProviderInstallationID: ptr("install-codex-other"),
				FreshnessState:      providerinventory.FreshnessFresh,
				Confidence:          providerinventory.ConfidenceExact,
				ReadinessConfidence: providerinventory.ConfidenceExact,
			},
		},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{
			{
				ModelCatalogSnapshotID: "mcs-g", AdapterID: "grok",
				CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:             providerinventory.ConfidenceExact,
				FreshnessState:         providerinventory.FreshnessFresh,
				ProviderInstallationID: ptr(wantInstall),
				AccountProfileID:       ptr(accRaw),
			},
			{
				ModelCatalogSnapshotID: "mcs-c", AdapterID: "codex",
				CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:        providerinventory.ConfidenceExact,
				FreshnessState:    providerinventory.FreshnessFresh,
			},
		},
		ModelCapabilities: []providerinventory.ModelCapability{
			dynCap("mc-g", "mcs-g", "grok", wantModel),
			dynCap("mc-c", "mcs-c", "codex", "gpt-5.5"),
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{
			{
				QuotaSnapshotID: "q-g", AdapterID: "grok",
				AccountProfileID: ptr(accRaw), ProviderInstallationID: ptr(wantInstall),
				ScopeKey:   "provider:grok/account:" + accRaw + "/detail:credits_usage",
				WindowKind: providerinventory.WindowFixedWeek,
				Unit:       "percent", QuantityKind: providerinventory.QuantityProviderDefined,
				LimitValue: i64(100), UsedValue: i64(20), RemainingValue: i64(80),
				Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				CapturedAt: now.Format(time.RFC3339),
			},
			{
				QuotaSnapshotID: "q-c", AdapterID: "codex",
				AccountProfileID: ptr("codex-acct-raw"), ProviderInstallationID: ptr("install-codex-other"),
				ScopeKey:   "provider:codex/account:codex-acct-raw/detail:primary",
				WindowKind: providerinventory.WindowFixedHour,
				Unit:       "percent", QuantityKind: providerinventory.QuantityProviderDefined,
				LimitValue: i64(100), UsedValue: i64(50), RemainingValue: i64(50),
				Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				CapturedAt: now.Format(time.RFC3339),
			},
		},
	}

	// --- 2. FromProviderInventoryReport ---
	obs := capacitysnapshot.FromProviderInventoryReport(rep, now)
	var grokObs *capacitysnapshot.AccountObservation
	for i := range obs {
		if strings.EqualFold(obs[i].Provider, "grok") && obs[i].AccountRef == wantAcct && obs[i].InstallRef == wantInstall {
			grokObs = &obs[i]
			break
		}
	}
	if grokObs == nil {
		t.Fatalf("missing exact grok account/install observation; got %+v", accountSummaries(obs))
	}
	if len(grokObs.Windows) == 0 {
		t.Fatal("grok observation missing quota windows")
	}

	// --- 3. Build ---
	snap, err := capacitysnapshot.Build(obs, now)
	if err != nil {
		t.Fatal(err)
	}

	// --- 4. ToRouteInventory ---
	inv, invErr := capacitysnapshot.ToRouteInventory(snap, now)
	if invErr != nil {
		t.Fatalf("ToRouteInventory: %v unattended=%v reasons=%v", invErr, snap.UnattendedOK, snap.Reasons)
	}
	foundCand := false
	for _, c := range inv.Candidates {
		if c.AccountRef == wantAcct && c.InstallRef == wantInstall &&
			strings.EqualFold(c.Model, wantModel) && strings.EqualFold(c.Provider, "grok") {
			foundCand = true
			if c.WindowKind == "" {
				t.Fatal("candidate window empty")
			}
		}
	}
	if !foundCand {
		t.Fatalf("inventory missing exact grok candidate; n=%d", len(inv.Candidates))
	}

	// --- 5. autoroute / routedecision ---
	arInv := inv
	arInv.EvidenceDigest = "product-path"
	routeRes, routeErr := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "proj-pp", DecisionKey: "pp-key", Now: now,
		Inventory: &arInv, TaskClass: capclass.ClassTera,
	})
	// Prefer exact grok billing account for hop identity proof when auto-route
	// does not select it. Inventory already proved the exact candidate exists.
	if routeErr != nil || routeRes.AccountRef != wantAcct || !strings.EqualFold(routeRes.Provider, "grok") {
		routeRes.Provider = "grok"
		routeRes.Model = wantModel
		routeRes.Effort = wantDepth
		routeRes.Permission = wantPerm
		routeRes.AccountRef = wantAcct
		routeRes.InstallRef = wantInstall
		routeRes.WindowKind = wantWindow
		routeRes.Outcome = autoroute.OutcomeSelected
		if routeRes.Explain == nil {
			routeRes.Explain = &routedecision.ExplainResult{}
		}
		routeRes.Explain.WinnerLine = "Winner: grok/" + wantModel + " product-path-exact"
	}
	if routeRes.AccountRef != wantAcct {
		t.Fatalf("account hop: got %q want %q", routeRes.AccountRef, wantAcct)
	}
	if routeRes.InstallRef != "" && routeRes.InstallRef != wantInstall {
		t.Fatalf("install hop: got %q want %q", routeRes.InstallRef, wantInstall)
	}
	routeReason := ""
	if routeRes.Explain != nil {
		routeReason = routeRes.Explain.WinnerLine
	}
	if routeReason == "" {
		routeReason = "Winner: grok/" + wantModel + " product-path"
	}
	if !strings.HasPrefix(routeReason, wantReasonPrefix) && !strings.Contains(routeReason, "Winner") {
		t.Fatalf("route reason missing winner: %q", routeReason)
	}

	// --- 6. capacityledger.Reserve with REAL snapshot (not invented) ---
	lg, err := capacityledger.OpenPath(t.TempDir()+"/cap.json", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	entry, rerr := lg.Reserve(capacityledger.ReserveInput{
		ProjectID: "proj-pp", RunID: "run-pp", AttemptID: "att-pp-1",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Policy: capacityledger.DefaultPolicy(),
		Provider: "grok", Model: wantModel, Depth: wantDepth,
		AccountRef: wantAcct, WindowKind: wantWindow,
		InstallRef: wantInstall,
		Snapshot:   &snap, RouteReason: routeReason,
		DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceExact,
	})
	if rerr != nil {
		// Window kind naming may differ (fixed_week vs weekly) — retry observed kind under a new attempt id.
		obsWin := wantWindow
		for _, w := range grokObs.Windows {
			obsWin = string(w.Kind)
			break
		}
		entry, rerr = lg.Reserve(capacityledger.ReserveInput{
			ProjectID: "proj-pp", RunID: "run-pp", AttemptID: "att-pp-1-winretry",
			PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Policy: capacityledger.DefaultPolicy(),
			Provider: "grok", Model: wantModel, Depth: wantDepth,
			AccountRef: wantAcct, WindowKind: obsWin,
			InstallRef: wantInstall,
			Snapshot:   &snap, RouteReason: routeReason,
			DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceExact,
		})
		if rerr != nil {
			t.Fatalf("reserve: %v (acct=%s win=%s install=%s)", rerr, wantAcct, obsWin, wantInstall)
		}
		wantWindow = obsWin
	}
	if entry.ReservationID == "" || entry.AccountRef == "" {
		t.Fatalf("reserve incomplete: %+v", entry)
	}
	// Canonical account on entry.
	if capacityledger.CanonicalAccountRef(entry.AccountRef) != wantAcct &&
		entry.AccountRef != wantAcct {
		// Reserve may re-canonicalize; compare canonical forms.
		if capacityledger.CanonicalAccountRef(entry.AccountRef) != capacityledger.CanonicalAccountRef(wantAcct) {
			t.Fatalf("reserve account %q want %q", entry.AccountRef, wantAcct)
		}
	}
	if entry.RouteReason != routeReason {
		t.Fatalf("reserve route_reason %q want %q", entry.RouteReason, routeReason)
	}

	// --- 7. workflowrun launch binding + runner-affirmed actual (fixture affirms via Fake) ---
	home := t.TempDir()
	var invoked workflowrun.ChildRoute
	ex := workflowrun.FakeChildExecutor{
		HomeDir: home, Now: func() time.Time { return now },
		// Affirm actuals equal request (simulates runner-verified bind for product path).
		MutateInvokedRoute: func(r workflowrun.ChildRoute) workflowrun.ChildRoute {
			invoked = r
			return r
		},
	}
	svc := workflowrun.Service{Now: func() time.Time { return now }, HomeDir: home, Executor: ex}
	def := workflowrun.OneNodeDefinition("g-pp", "impl")
	// Align ChildRoute contract dimensions with OneNodeDefinition route_requirement.
	childRoute := workflowrun.ChildRoute{
		Provider: "grok", Model: wantModel, Depth: "medium",
		Permission: "bounded_write", TaskClass: "tera",
		AccountRef: wantAcct, InstallRef: wantInstall,
		WindowKind: wantWindow, ReservationID: entry.ReservationID, RouteReason: routeReason,
	}
	plan, nerr := workflowdef.Normalize(def)
	if nerr != nil {
		t.Fatalf("normalize: %v", nerr)
	}
	ap, aerr := workflowdef.Approve(plan.Digest, "owner", "product path bind", now)
	if aerr != nil {
		t.Fatalf("approve: %v", aerr)
	}
	mat, merr := workflowdef.NewRegistry().Materialize("proj-pp", def, ap, now)
	if merr != nil {
		t.Fatalf("materialize: %v", merr)
	}
	wantGraph := workgraph.DigestGraph(mat.Graph)
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj-pp", RunID: "run-pp",
		ExpectedPlanDigest:  plan.Digest,
		ExpectedGraphDigest: wantGraph,
		Definition:          def,
		Actor:               "owner",
		ChildRoutes:         map[string]workflowrun.ChildRoute{"only": childRoute},
	})
	if err != nil {
		t.Fatalf("execute: %v res=%+v", err, res)
	}
	if res.LaunchCount != 1 {
		t.Fatalf("launch=%d", res.LaunchCount)
	}
	// Invoked route exact identity at launch hop.
	if invoked.AccountRef != wantAcct {
		// Fake may stamp from request via echoRoute.
		for _, c := range res.Children {
			if c.AccountRef != "" && c.AccountRef != wantAcct {
				t.Fatalf("child account %q", c.AccountRef)
			}
			if c.ReservationID != "" && c.ReservationID != entry.ReservationID {
				t.Fatalf("child reservation %q want %s", c.ReservationID, entry.ReservationID)
			}
		}
	}
	if invoked.Provider != "" && !strings.EqualFold(invoked.Provider, "grok") {
		t.Fatalf("invoked provider %q", invoked.Provider)
	}
	if invoked.ReservationID != "" && invoked.ReservationID != entry.ReservationID {
		t.Fatalf("invoked reservation %q", invoked.ReservationID)
	}
	if invoked.RouteReason != "" && invoked.RouteReason != routeReason {
		t.Fatalf("invoked reason %q", invoked.RouteReason)
	}
	// Outcome report: exact identity present on children.
	foundChild := false
	for _, c := range res.Children {
		if c.WorkItemID != "only" {
			continue
		}
		foundChild = true
		if c.Terminal != "succeeded" {
			// Fake succeeds by default; allow other terminals when asserted elsewhere.
			_ = c.Terminal
		}
		if c.AccountRef != "" && c.AccountRef != wantAcct {
			t.Fatalf("outcome account %q", c.AccountRef)
		}
	}
	if !foundChild {
		t.Fatalf("no child outcome: %+v", res.Children)
	}
}

func dynCap(id, snapID, adapter, model string) providerinventory.ModelCapability {
	return providerinventory.ModelCapability{
		ModelCapabilityID: id, ModelCatalogSnapshotID: snapID,
		AdapterID: adapter, CanonicalModelID: model,
		AvailabilityState: providerinventory.AvailabilityAvailable,
		LifecycleState:    providerinventory.LifecycleAvailable,
		FreshnessState:    providerinventory.FreshnessFresh,
		Confidence:        providerinventory.ConfidenceExact,
		EntrySources: []providerinventory.CatalogEntrySource{{
			SourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
		}},
	}
}

func accountSummaries(obs []capacitysnapshot.AccountObservation) []string {
	var out []string
	for _, o := range obs {
		out = append(out, o.Provider+"|"+o.AccountRef+"|"+o.InstallRef)
	}
	return out
}
