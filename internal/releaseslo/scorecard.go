package releaseslo

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// SchemaScorecard id.
const SchemaScorecard = "loopcoder.releaseslo.scorecard.v1"

// CalcVersion of the scorecard algorithm.
const CalcVersion = "v090-102.1"

// MetricID is one scored dimension.
type MetricID string

const (
	MetricStartReportLatency MetricID = "run_id_start_report_latency"
	MetricReportInterval     MetricID = "report_interval"
	MetricRenderedAck        MetricID = "rendered_ack_latency"
	MetricStatusFreshness    MetricID = "status_freshness"
	MetricStopJoin           MetricID = "stop_join"
	MetricProcessLeaks       MetricID = "process_leaks"
	MetricRepoLocalState     MetricID = "repo_local_state"
	MetricRouteSubstitution  MetricID = "route_substitution"
	MetricDeliveryReplay     MetricID = "delivery_replay"
	MetricResources          MetricID = "resources"
	MetricRedaction          MetricID = "redaction"
	MetricMigration          MetricID = "migration"
	MetricArtifact           MetricID = "artifact"
)

// Verdict for one metric.
type Verdict string

const (
	VerdictPass            Verdict = "pass"
	VerdictFail            Verdict = "fail"
	VerdictNotRun          Verdict = "not_run"
	VerdictStale           Verdict = "stale"
	VerdictUnsupported     Verdict = "unsupported"
	VerdictWaiverRequested Verdict = "waiver_requested"
	VerdictWaiverApproved  Verdict = "waiver_approved"
)

// Thresholds for numeric metrics (milliseconds or counts as documented).
type Thresholds struct {
	StartReportLatencyMs int64
	ReportIntervalMs     int64
	RenderedAckLatencyMs int64
	StatusFreshnessMs    int64
	// ProcessLeaks max allowed (0 = none).
	ProcessLeaksMax int
}

// DefaultThresholds returns conservative product thresholds.
func DefaultThresholds() Thresholds {
	return Thresholds{
		StartReportLatencyMs: 30_000,
		ReportIntervalMs:     60_000,
		RenderedAckLatencyMs: 15_000,
		StatusFreshnessMs:    30_000,
		ProcessLeaksMax:      0,
	}
}

// MetricObservation is evidence for one metric.
type MetricObservation struct {
	ID MetricID `json:"id"`
	// ObservedMs for latency metrics; optional.
	ObservedMs int64 `json:"observed_ms,omitempty"`
	// ObservedCount for leak counts.
	ObservedCount int `json:"observed_count,omitempty"`
	// BoolOK for boolean metrics (redaction clean, etc.).
	BoolOK *bool `json:"bool_ok,omitempty"`
	// EvidenceRef links to run/manifest (not free prose).
	EvidenceRef string `json:"evidence_ref"`
	// Stale when evidence is older than candidate.
	Stale bool `json:"stale,omitempty"`
	// NotRun when metric was never collected.
	NotRun bool `json:"not_run,omitempty"`
	// Unsupported platform/capability.
	Unsupported bool `json:"unsupported,omitempty"`
}

// Waiver is an owner-approved exception.
type Waiver struct {
	MetricID  MetricID  `json:"metric_id"`
	Owner     string    `json:"owner"`
	Rationale string    `json:"rationale"`
	Scope     string    `json:"scope"`
	Expiry    time.Time `json:"expiry"`
	// Risk documented release risk.
	Risk string `json:"risk"`
	// Approved must be true for waiver_approved.
	Approved bool `json:"approved"`
}

// Candidate binds scorecard to release identity.
type Candidate struct {
	SHA           string `json:"sha"`
	ArchiveDigest string `json:"archive_digest"`
}

// Scorecard is the human/JSON release decision input.
type Scorecard struct {
	Schema      string         `json:"schema"`
	CalcVersion string         `json:"calc_version"`
	Candidate   Candidate      `json:"candidate"`
	Metrics     []MetricResult `json:"metrics"`
	// Overall GO only when all metrics pass or waiver_approved and none fail/not_run/stale.
	Overall       Verdict  `json:"overall"`
	GO            bool     `json:"go"`
	Reasons       []string `json:"reasons,omitempty"`
	EvidenceLinks []string `json:"evidence_links,omitempty"`
}

// MetricResult is one scored row.
type MetricResult struct {
	ID          MetricID `json:"id"`
	Verdict     Verdict  `json:"verdict"`
	Threshold   string   `json:"threshold,omitempty"`
	Observed    string   `json:"observed,omitempty"`
	EvidenceRef string   `json:"evidence_ref,omitempty"`
}

// Compile builds a scorecard from observations and optional waivers.
func Compile(cand Candidate, obs []MetricObservation, th Thresholds, waivers []Waiver, now time.Time) Scorecard {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	sc := Scorecard{
		Schema: SchemaScorecard, CalcVersion: CalcVersion, Candidate: cand,
	}
	if cand.SHA == "" || cand.ArchiveDigest == "" {
		sc.Overall = VerdictFail
		sc.GO = false
		sc.Reasons = []string{"candidate sha and archive digest required"}
		return sc
	}

	byID := map[MetricID]MetricObservation{}
	for _, o := range obs {
		byID[o.ID] = o
	}
	waiverBy := map[MetricID]Waiver{}
	for _, w := range waivers {
		if err := ValidateWaiver(w, now); err != nil {
			continue // invalid waiver ignored (does not grant pass)
		}
		if w.Approved {
			waiverBy[w.MetricID] = w
		}
	}

	required := []MetricID{
		MetricStartReportLatency, MetricReportInterval, MetricRenderedAck, MetricStatusFreshness,
		MetricStopJoin, MetricProcessLeaks, MetricRepoLocalState, MetricRouteSubstitution,
		MetricDeliveryReplay, MetricResources, MetricRedaction, MetricMigration, MetricArtifact,
	}

	allPass := true
	for _, id := range required {
		o, has := byID[id]
		mr := scoreOne(id, o, has, th, waiverBy[id])
		sc.Metrics = append(sc.Metrics, mr)
		if mr.EvidenceRef != "" {
			sc.EvidenceLinks = append(sc.EvidenceLinks, mr.EvidenceRef)
		}
		switch mr.Verdict {
		case VerdictPass, VerdictWaiverApproved, VerdictUnsupported:
			// ok
		default:
			allPass = false
			sc.Reasons = append(sc.Reasons, fmt.Sprintf("%s=%s", id, mr.Verdict))
		}
	}
	sort.Slice(sc.Metrics, func(i, j int) bool { return sc.Metrics[i].ID < sc.Metrics[j].ID })
	sc.EvidenceLinks = uniqueSorted(sc.EvidenceLinks)
	if allPass {
		sc.Overall = VerdictPass
		sc.GO = true
		sc.Reasons = append(sc.Reasons, "all metrics pass or waiver-approved/unsupported")
	} else {
		sc.Overall = VerdictFail
		sc.GO = false
	}
	return sc
}

func scoreOne(id MetricID, o MetricObservation, has bool, th Thresholds, w Waiver) MetricResult {
	mr := MetricResult{ID: id}
	if !has || o.NotRun {
		mr.Verdict = VerdictNotRun
		return mr
	}
	if o.Stale {
		mr.Verdict = VerdictStale
		mr.EvidenceRef = o.EvidenceRef
		return mr
	}
	if o.Unsupported {
		mr.Verdict = VerdictUnsupported
		mr.EvidenceRef = o.EvidenceRef
		return mr
	}
	mr.EvidenceRef = o.EvidenceRef

	// Boolean metrics
	switch id {
	case MetricStopJoin, MetricRepoLocalState, MetricRouteSubstitution, MetricDeliveryReplay,
		MetricResources, MetricRedaction, MetricMigration, MetricArtifact:
		mr.Threshold = "ok=true"
		if o.BoolOK != nil && *o.BoolOK {
			mr.Verdict = VerdictPass
			mr.Observed = "true"
		} else if o.BoolOK != nil {
			mr.Verdict = VerdictFail
			mr.Observed = "false"
		} else {
			mr.Verdict = VerdictNotRun
		}
	case MetricProcessLeaks:
		mr.Threshold = fmt.Sprintf("count<=%d", th.ProcessLeaksMax)
		mr.Observed = fmt.Sprintf("%d", o.ObservedCount)
		if o.ObservedCount <= th.ProcessLeaksMax {
			mr.Verdict = VerdictPass
		} else {
			mr.Verdict = VerdictFail
		}
	case MetricStartReportLatency:
		mr.Threshold = fmt.Sprintf("ms<=%d", th.StartReportLatencyMs)
		mr.Observed = fmt.Sprintf("%d", o.ObservedMs)
		if o.ObservedMs > 0 && o.ObservedMs <= th.StartReportLatencyMs {
			mr.Verdict = VerdictPass
		} else if o.ObservedMs == 0 {
			mr.Verdict = VerdictNotRun
		} else {
			mr.Verdict = VerdictFail
		}
	case MetricReportInterval:
		mr.Threshold = fmt.Sprintf("ms<=%d", th.ReportIntervalMs)
		mr.Observed = fmt.Sprintf("%d", o.ObservedMs)
		if o.ObservedMs > 0 && o.ObservedMs <= th.ReportIntervalMs {
			mr.Verdict = VerdictPass
		} else if o.ObservedMs == 0 {
			mr.Verdict = VerdictNotRun
		} else {
			mr.Verdict = VerdictFail
		}
	case MetricRenderedAck:
		mr.Threshold = fmt.Sprintf("ms<=%d", th.RenderedAckLatencyMs)
		mr.Observed = fmt.Sprintf("%d", o.ObservedMs)
		if o.ObservedMs > 0 && o.ObservedMs <= th.RenderedAckLatencyMs {
			mr.Verdict = VerdictPass
		} else if o.ObservedMs == 0 {
			mr.Verdict = VerdictNotRun
		} else {
			mr.Verdict = VerdictFail
		}
	case MetricStatusFreshness:
		mr.Threshold = fmt.Sprintf("ms<=%d", th.StatusFreshnessMs)
		mr.Observed = fmt.Sprintf("%d", o.ObservedMs)
		if o.ObservedMs > 0 && o.ObservedMs <= th.StatusFreshnessMs {
			mr.Verdict = VerdictPass
		} else if o.ObservedMs == 0 {
			mr.Verdict = VerdictNotRun
		} else {
			mr.Verdict = VerdictFail
		}
	default:
		mr.Verdict = VerdictNotRun
	}

	// Apply approved waiver only on fail.
	if mr.Verdict == VerdictFail && w.Approved && w.MetricID == id {
		mr.Verdict = VerdictWaiverApproved
	}
	return mr
}

// ValidateWaiver checks required fields and expiry.
func ValidateWaiver(w Waiver, now time.Time) error {
	if strings.TrimSpace(w.Owner) == "" {
		return fmt.Errorf("waiver owner required")
	}
	if strings.TrimSpace(w.Rationale) == "" {
		return fmt.Errorf("waiver rationale required")
	}
	if strings.TrimSpace(w.Scope) == "" {
		return fmt.Errorf("waiver scope required")
	}
	if strings.TrimSpace(w.Risk) == "" {
		return fmt.Errorf("waiver risk required")
	}
	if w.Expiry.IsZero() || !w.Expiry.After(now) {
		return fmt.Errorf("waiver expiry must be in the future")
	}
	if w.MetricID == "" {
		return fmt.Errorf("waiver metric required")
	}
	return nil
}

func uniqueSorted(in []string) []string {
	m := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || m[s] {
			continue
		}
		m[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Bool returns a *bool helper.
func Bool(v bool) *bool { return &v }
