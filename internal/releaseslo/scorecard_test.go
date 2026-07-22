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
		releaseslo.MetricUsefulCapacityRouting, releaseslo.MetricWorkgraphDecompose, releaseslo.MetricCapacityAccounting,
		releaseslo.MetricMultiDepthRouting, releaseslo.MetricUnavailableRouteExclude,
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

func TestMultiDepthAndUnavailableRequiredNotRunFailClosed(t *testing.T) {
	// Drop the two new #1343 core metrics → scorecard must not GO (mechanical false green forbidden).
	var obs []releaseslo.MetricObservation
	for _, o := range greenObs() {
		if o.ID == releaseslo.MetricMultiDepthRouting || o.ID == releaseslo.MetricUnavailableRouteExclude {
			continue
		}
		obs = append(obs, o)
	}
	sc := releaseslo.Compile(releaseslo.Candidate{SHA: "a", ArchiveDigest: "b"}, obs, releaseslo.DefaultThresholds(), nil, fixed())
	if sc.GO {
		t.Fatal("missing multi_depth/unavailable metrics must not scorecard_go")
	}
	found := false
	for _, r := range sc.Reasons {
		if strings.Contains(r, "multi_depth_routing=not_run") || strings.Contains(r, "unavailable_route_exclude=not_run") {
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
	// force fail process leaks
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
