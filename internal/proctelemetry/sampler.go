package proctelemetry

import (
	"sort"
	"time"

	"github.com/jasonhnd/loopcoder/internal/processtree"
)

// Sampler aggregates resources for an owned tree identity.
type Sampler struct {
	// MaxPIDs bounds how many owned PIDs are read (default DefaultMaxPIDs).
	MaxPIDs int
	// MinInterval skips productive re-reads when called too soon (returns stale).
	MinInterval time.Duration
	Thresholds  Thresholds
	Reader      ResourceReader
	Now         func() time.Time

	lastAt      time.Time
	lastCPU     float64
	haveLast    bool
	lastQuality Quality

	// edge state for one-shot crossings
	aboveCPU  bool
	aboveRSS  bool
	aboveProc bool
	// initialized tracks whether above* mirrors have been set from a real sample
	edgesInit bool
}

// SampleFromAssessment samples owned PIDs from a processtree assessment.
// Reused roots (pid_reuse) contribute no metrics and force unavailable/attention.
func (s *Sampler) SampleFromAssessment(a processtree.Assessment) Sample {
	now := s.now()
	if a.Liveness == processtree.LivenessNotStarted {
		return Sample{
			ObservedAt: now, Quality: QualityUnavailable,
			Reasons: []string{"not_started"},
		}
	}
	for _, r := range a.Reasons {
		if r == "pid_reuse" {
			return Sample{
				ObservedAt: now, Quality: QualityUnavailable,
				Reasons: []string{"pid_reuse_excluded"},
			}
		}
	}
	if a.Liveness == processtree.LivenessUnknown && len(a.Snapshot.Nodes) == 0 {
		return Sample{
			ObservedAt: now, Quality: QualityUnavailable,
			Reasons: []string{"tree_unknown"},
		}
	}

	var pids []int
	for _, n := range a.Snapshot.Nodes {
		if !n.Owned || n.Zombie {
			continue
		}
		pids = append(pids, n.PID)
	}
	sort.Ints(pids)
	return s.SamplePIDs(pids, a.Snapshot.Truncated, now)
}

// SamplePIDs samples a known owned PID set (tests and low-level callers).
func (s *Sampler) SamplePIDs(pids []int, alreadyTruncated bool, at time.Time) Sample {
	if at.IsZero() {
		at = s.now()
	}
	max := s.MaxPIDs
	if max <= 0 {
		max = DefaultMaxPIDs
	}
	minI := s.MinInterval
	if minI <= 0 {
		minI = DefaultMinInterval
	}

	out := Sample{ObservedAt: at}
	if alreadyTruncated {
		out.Truncated = true
		out.Reasons = append(out.Reasons, "tree_truncated")
	}
	if len(pids) > max {
		pids = pids[:max]
		out.Truncated = true
		out.Reasons = append(out.Reasons, "sample_truncated")
	}

	// Interval gate: return stale copy of quality signal without pretending zeros.
	if s.haveLast && !s.lastAt.IsZero() && at.Sub(s.lastAt) < minI {
		out.Quality = QualityStale
		out.Reasons = append(out.Reasons, "min_interval")
		// Preserve last counters as non-authoritative display? Spec: stale explicit.
		// Do not fill zeros as "use" — leave metrics zero and HasCPURate false.
		return out
	}

	if len(pids) == 0 {
		out.Quality = QualityFull
		out.ProcessCount = 0
		out.Reasons = append(out.Reasons, "empty_owned_set")
		// Empty owned set after exit is legitimate zero process count.
		s.recordSuccess(at, 0, out)
		out.Crossings = s.crossings(0, 0, 0)
		return out
	}

	reader := s.Reader
	if reader == nil {
		reader = DarwinReader{}
	}
	got, err := reader.Read(pids)
	if err != nil {
		out.Quality = QualityUnavailable
		out.Reasons = append(out.Reasons, "read_failed")
		out.MissedPIDs = len(pids)
		// Never report zero use on failure.
		return out
	}

	var cpu float64
	var rss int64
	sampled := 0
	for _, pid := range pids {
		r, ok := got[pid]
		if !ok || !r.OK {
			out.MissedPIDs++
			continue
		}
		sampled++
		cpu += r.CPUTimeSecs
		rss += r.RSSBytes
	}
	out.SampledPIDs = sampled
	out.ProcessCount = sampled
	out.CPUTimeSecs = cpu
	out.RSSBytes = rss

	switch {
	case sampled == 0:
		out.Quality = QualityUnavailable
		out.Reasons = append(out.Reasons, "none_sampled")
		// Clear numeric fields so callers cannot treat as idle.
		out.ProcessCount = 0
		out.CPUTimeSecs = 0
		out.RSSBytes = 0
		return out
	case sampled < len(pids):
		out.Quality = QualityPartial
		out.Reasons = append(out.Reasons, "partial_pids")
	default:
		out.Quality = QualityFull
	}

	if s.haveLast && s.lastQuality != QualityUnavailable && s.lastQuality != QualityStale {
		dt := at.Sub(s.lastAt).Seconds()
		if dt > 0 {
			dcpu := cpu - s.lastCPU
			if dcpu < 0 {
				// Counter reset (PID recycle); do not invent negative rate.
				out.Reasons = append(out.Reasons, "cpu_counter_reset")
			} else {
				out.CPURate = dcpu / dt
				out.HasCPURate = true
			}
		}
	}

	out.Crossings = s.crossings(out.CPURate, float64(rss), float64(sampled))
	s.recordSuccess(at, cpu, out)
	return out
}

// Reset clears rate/edge state (e.g. new attempt).
func (s *Sampler) Reset() {
	s.lastAt = time.Time{}
	s.lastCPU = 0
	s.haveLast = false
	s.lastQuality = ""
	s.aboveCPU = false
	s.aboveRSS = false
	s.aboveProc = false
	s.edgesInit = false
}

func (s *Sampler) recordSuccess(at time.Time, cpu float64, out Sample) {
	if out.Quality == QualityUnavailable || out.Quality == QualityStale {
		return
	}
	s.lastAt = at
	s.lastCPU = cpu
	s.haveLast = true
	s.lastQuality = out.Quality
}

func (s *Sampler) crossings(cpuRate, rss, proc float64) []Crossing {
	th := s.Thresholds
	var out []Crossing

	// First successful sample only initializes edge state (no flood of crossings).
	if !s.edgesInit {
		s.aboveCPU = th.CPURate > 0 && cpuRate >= th.CPURate
		s.aboveRSS = th.RSSBytes > 0 && int64(rss) >= th.RSSBytes
		s.aboveProc = th.ProcessCount > 0 && int(proc) >= th.ProcessCount
		s.edgesInit = true
		return nil
	}

	if th.CPURate > 0 {
		above := cpuRate >= th.CPURate
		if above != s.aboveCPU {
			dir := CrossingUp
			if !above {
				dir = CrossingDown
			}
			out = append(out, Crossing{Metric: MetricCPURate, Direction: dir, Value: cpuRate, Threshold: th.CPURate})
			s.aboveCPU = above
		}
	}
	if th.RSSBytes > 0 {
		above := int64(rss) >= th.RSSBytes
		if above != s.aboveRSS {
			dir := CrossingUp
			if !above {
				dir = CrossingDown
			}
			out = append(out, Crossing{Metric: MetricRSSBytes, Direction: dir, Value: rss, Threshold: float64(th.RSSBytes)})
			s.aboveRSS = above
		}
	}
	if th.ProcessCount > 0 {
		above := int(proc) >= th.ProcessCount
		if above != s.aboveProc {
			dir := CrossingUp
			if !above {
				dir = CrossingDown
			}
			out = append(out, Crossing{Metric: MetricProcessCount, Direction: dir, Value: proc, Threshold: float64(th.ProcessCount)})
			s.aboveProc = above
		}
	}
	return out
}

func (s *Sampler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// OwnedPIDs extracts owned non-zombie PIDs from a snapshot (helper for callers).
func OwnedPIDs(snap processtree.Snapshot, max int) []int {
	if max <= 0 {
		max = DefaultMaxPIDs
	}
	var pids []int
	for _, n := range snap.Nodes {
		if !n.Owned || n.Zombie {
			continue
		}
		pids = append(pids, n.PID)
		if len(pids) >= max {
			break
		}
	}
	sort.Ints(pids)
	return pids
}
