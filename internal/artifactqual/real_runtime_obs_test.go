package artifactqual

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/releaseslo"
)

// Present=true, Valid=false, all flags=true must still yield NotRun for all six
// required real_runtime metrics (never partial true → scorecard GO).
func TestRealRuntimeObs_InvalidCanaryAllNotRun(t *testing.T) {
	cv := &CanaryValidation{
		Present: true, Valid: false,
		MultiDepthOK: true, MultiProviderOK: true, CapacityAfterOK: true,
		UnavailableRetryOK: true, RestartOK: true, RealPROK: true,
		EvidencePath: "/tmp/canary.json",
	}
	obs := realRuntimeObs(cv)
	if len(obs) != 6 {
		t.Fatalf("want 6 metrics, got %d", len(obs))
	}
	for _, o := range obs {
		if !o.NotRun {
			t.Fatalf("metric %s must be NotRun when Valid=false (got BoolOK=%v)", o.ID, o.BoolOK)
		}
		if o.BoolOK != nil {
			t.Fatalf("metric %s must not carry BoolOK when NotRun", o.ID)
		}
	}
	// Merge with structural latency OK so only real_runtime NotRun blocks GO.
	trueV := true
	all := append([]releaseslo.MetricObservation{}, obs...)
	for _, id := range []releaseslo.MetricID{
		releaseslo.MetricStartReportLatency, releaseslo.MetricReportInterval,
		releaseslo.MetricRenderedAck, releaseslo.MetricStatusFreshness,
		releaseslo.MetricStopJoin, releaseslo.MetricProcessLeaks,
		releaseslo.MetricRepoLocalState, releaseslo.MetricRouteSubstitution,
	} {
		all = append(all, releaseslo.MetricObservation{ID: id, BoolOK: &trueV, ObservedMs: 1, EvidenceRef: "t"})
	}
	sc := releaseslo.Compile(releaseslo.Candidate{SHA: "deadbeef", ArchiveDigest: "aa"}, all, releaseslo.DefaultThresholds(), nil, time.Now().UTC())
	if sc.GO {
		t.Fatalf("scorecard must not GO with invalid canary NotRun metrics; reasons=%v", sc.Reasons)
	}
}

func TestRealRuntimeObs_NilAndAbsentNotRun(t *testing.T) {
	for _, cv := range []*CanaryValidation{nil, {Present: false}} {
		obs := realRuntimeObs(cv)
		for _, o := range obs {
			if !o.NotRun {
				t.Fatalf("want NotRun for nil/absent, got %+v", o)
			}
		}
	}
}
