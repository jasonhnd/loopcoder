package releaseslo_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/releaseslo"
)

func fixed() time.Time {
	return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
}

func greenObs() []releaseslo.MetricObservation {
	ok := releaseslo.Bool(true)
	ids := []releaseslo.MetricID{
		releaseslo.MetricStopJoin, releaseslo.MetricRepoLocalState, releaseslo.MetricRouteSubstitution,
		releaseslo.MetricDeliveryReplay, releaseslo.MetricResources, releaseslo.MetricRedaction,
		releaseslo.MetricMigration, releaseslo.MetricArtifact,
		// real_runtime required
		releaseslo.MetricMultiDepthRouting, releaseslo.MetricUnavailableRouteExclude,
		releaseslo.MetricMultiProviderExecution, releaseslo.MetricCapacityAfterRuntime,
		releaseslo.MetricForcedRestartCeilings, releaseslo.MetricRealPRHumanGate,
	}
	var obs []releaseslo.MetricObservation
	for _, id := range ids {
		obs = append(obs, releaseslo.MetricObservation{ID: id, BoolOK: ok, EvidenceRef: "ev:" + string(id)})
	}
	obs = append(obs,
		releaseslo.MetricObservation{ID: releaseslo.MetricStartReportLatency, ObservedMs: 1000, EvidenceRef: "ev:start"},
		releaseslo.MetricObservation{ID: releaseslo.MetricReportInterval, ObservedMs: 2000, EvidenceRef: "ev:rep"},
		releaseslo.MetricObservation{ID: releaseslo.MetricRenderedAck, ObservedMs: 500, EvidenceRef: "ev:ack"},
		releaseslo.MetricObservation{ID: releaseslo.MetricStatusFreshness, ObservedMs: 800, EvidenceRef: "ev:fresh"},
		releaseslo.MetricObservation{ID: releaseslo.MetricProcessLeaks, ObservedCount: 0, EvidenceRef: "ev:leak"},
	)
	return obs
}

func TestCompileGO(t *testing.T) {
	sc := releaseslo.Compile(releaseslo.Candidate{SHA: "abc", ArchiveDigest: "def"}, greenObs(), releaseslo.DefaultThresholds(), nil, fixed())
	if !sc.GO || sc.Overall != releaseslo.VerdictPass {
		t.Fatalf("%#v", sc)
	}
	if sc.CalcVersion != releaseslo.CalcVersion {
		t.Fatal(sc.CalcVersion)
	}
}

func TestMissingMetricNotPass(t *testing.T) {
	obs := greenObs()[:3] // incomplete
	sc := releaseslo.Compile(releaseslo.Candidate{SHA: "a", ArchiveDigest: "b"}, obs, releaseslo.DefaultThresholds(), nil, fixed())
	if sc.GO {
		t.Fatal("missing must not GO")
	}
}

func TestRealRuntimeNotRunFailClosedWithoutCanary(t *testing.T) {
	// Structural-only observations: no real_runtime metrics → GO false.
	ok := releaseslo.Bool(true)
	obs := []releaseslo.MetricObservation{
		{ID: releaseslo.MetricStopJoin, BoolOK: ok, EvidenceRef: "e"},
		{ID: releaseslo.MetricRepoLocalState, BoolOK: ok, EvidenceRef: "e"},
		{ID: releaseslo.MetricRouteSubstitution, BoolOK: ok, EvidenceRef: "e"},
		{ID: releaseslo.MetricDeliveryReplay, BoolOK: ok, EvidenceRef: "e"},
		{ID: releaseslo.MetricResources, BoolOK: ok, EvidenceRef: "e"},
		{ID: releaseslo.MetricRedaction, BoolOK: ok, EvidenceRef: "e"},
		{ID: releaseslo.MetricMigration, BoolOK: ok, EvidenceRef: "e"},
		{ID: releaseslo.MetricArtifact, BoolOK: ok, EvidenceRef: "e"},
		{ID: releaseslo.MetricStartReportLatency, ObservedMs: 1000, EvidenceRef: "e"},
		{ID: releaseslo.MetricReportInterval, ObservedMs: 2000, EvidenceRef: "e"},
		{ID: releaseslo.MetricRenderedAck, ObservedMs: 500, EvidenceRef: "e"},
		{ID: releaseslo.MetricStatusFreshness, ObservedMs: 800, EvidenceRef: "e"},
		{ID: releaseslo.MetricProcessLeaks, ObservedCount: 0, EvidenceRef: "e"},
		// structural only (not required)
		{ID: releaseslo.MetricStructuralDepthPlan, BoolOK: ok, EvidenceRef: "structural"},
		{ID: releaseslo.MetricUsefulCapacityRouting, BoolOK: ok, EvidenceRef: "structural"},
		// real_runtime explicitly not_run
		{ID: releaseslo.MetricMultiDepthRouting, NotRun: true},
		{ID: releaseslo.MetricUnavailableRouteExclude, NotRun: true},
		{ID: releaseslo.MetricMultiProviderExecution, NotRun: true},
		{ID: releaseslo.MetricCapacityAfterRuntime, NotRun: true},
		{ID: releaseslo.MetricForcedRestartCeilings, NotRun: true},
		{ID: releaseslo.MetricRealPRHumanGate, NotRun: true},
	}
	sc := releaseslo.Compile(releaseslo.Candidate{SHA: "a", ArchiveDigest: "b"}, obs, releaseslo.DefaultThresholds(), nil, fixed())
	if sc.GO {
		t.Fatal("dry-run/structural-only must not scorecard_go")
	}
	found := false
	for _, r := range sc.Reasons {
		if strings.Contains(r, "multi_depth_routing=not_run") || strings.Contains(r, "real_pr_human_gate=not_run") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons=%v", sc.Reasons)
	}
}

func TestStaleAndNotRun(t *testing.T) {
	obs := greenObs()
	for i := range obs {
		if obs[i].ID == releaseslo.MetricRedaction {
			obs[i].Stale = true
		}
	}
	sc := releaseslo.Compile(releaseslo.Candidate{SHA: "a", ArchiveDigest: "b"}, obs, releaseslo.DefaultThresholds(), nil, fixed())
	if sc.GO {
		t.Fatal("stale must not GO")
	}
}

func TestWaiverApproved(t *testing.T) {
	obs := greenObs()
	for i := range obs {
		if obs[i].ID == releaseslo.MetricProcessLeaks {
			obs[i].ObservedCount = 2
		}
	}
	w := releaseslo.Waiver{
		MetricID: releaseslo.MetricProcessLeaks, Owner: "jasonhnd",
		Rationale: "known flake", Scope: "rc1", Risk: "low leak monitor",
		Expiry: fixed().Add(24 * time.Hour), Approved: true,
	}
	sc := releaseslo.Compile(releaseslo.Candidate{SHA: "a", ArchiveDigest: "b"}, obs, releaseslo.DefaultThresholds(), []releaseslo.Waiver{w}, fixed())
	if !sc.GO {
		t.Fatalf("waiver should allow GO: %#v", sc.Reasons)
	}
}

func TestInvalidWaiverIgnored(t *testing.T) {
	if err := releaseslo.ValidateWaiver(releaseslo.Waiver{}, fixed()); err == nil {
		t.Fatal()
	}
}

func TestCandidateRequired(t *testing.T) {
	sc := releaseslo.Compile(releaseslo.Candidate{}, nil, releaseslo.DefaultThresholds(), nil, fixed())
	if sc.GO {
		t.Fatal()
	}
}
