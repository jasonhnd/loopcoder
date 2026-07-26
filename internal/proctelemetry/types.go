package proctelemetry

import (
	"errors"
	"time"
)

// DefaultMaxPIDs matches processtree.DefaultMaxNodes sampling bound.
const DefaultMaxPIDs = 64

// DefaultMinInterval is the minimum wall time between productive samples.
const DefaultMinInterval = 200 * time.Millisecond

// Quality grades a sample. Zero metrics with QualityFull would be a bug —
// unavailable/partial never look like "idle zero use".
type Quality string

const (
	QualityFull        Quality = "full"
	QualityPartial     Quality = "partial"
	QualityUnavailable Quality = "unavailable"
	QualityStale       Quality = "stale"
)

// MetricName identifies a threshold dimension.
type MetricName string

const (
	MetricCPURate      MetricName = "cpu_rate"
	MetricRSSBytes     MetricName = "rss_bytes"
	MetricProcessCount MetricName = "process_count"
)

// CrossingDirection is rising or falling through a threshold.
type CrossingDirection string

const (
	CrossingUp   CrossingDirection = "up"
	CrossingDown CrossingDirection = "down"
)

// Thresholds define optional high watermarks for one-shot transition evidence.
// Zero means disabled for that dimension.
type Thresholds struct {
	CPURate      float64 // aggregate CPU seconds per wall second
	RSSBytes     int64
	ProcessCount int
}

// Crossing is emitted at most once per transition edge (not every sample).
type Crossing struct {
	Metric    MetricName
	Direction CrossingDirection
	Value     float64
	Threshold float64
}

// ProcResources is per-PID host evidence (no argv/paths).
type ProcResources struct {
	PID         int
	CPUTimeSecs float64 // cumulative user+sys seconds
	RSSBytes    int64
	// OK is false when this PID could not be observed.
	OK bool
}

// ResourceReader reads resources for a bounded PID list only.
type ResourceReader interface {
	Read(pids []int) (map[int]ProcResources, error)
}

// Sample is one aggregate observation for V090-016 consumers.
type Sample struct {
	ObservedAt   time.Time
	ProcessCount int
	// CPUTimeSecs is aggregate cumulative CPU over owned, non-reused PIDs.
	CPUTimeSecs float64
	// CPURate is ΔCPU / Δwall since previous successful sample; 0 when unknown.
	CPURate float64
	// HasCPURate is false on first sample or after reset (do not treat rate as zero use).
	HasCPURate  bool
	RSSBytes    int64
	Quality     Quality
	SampledPIDs int
	MissedPIDs  int
	// Truncated is true when owned PID list exceeded MaxPIDs.
	Truncated bool
	// Reasons are stable tokens (no secrets).
	Reasons []string
	// Crossings are edge-triggered threshold events for this sample only.
	Crossings []Crossing
}

// ErrStaleInterval means MinInterval has not elapsed (caller may keep last sample).
var ErrStaleInterval = errors.New("proctelemetry: sample interval not elapsed")
