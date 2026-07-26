package admission

import (
	"errors"
	"time"
)

const (
	// SchemaVersion tags decision and reservation payloads.
	SchemaVersion = "loopcoder.machine_admission.v1"

	// Default budgets (issue acceptance defaults).
	DefaultMaxActiveWorkers   = 1
	DefaultMaxActiveVerifiers = 0 // while any worker is active
	DefaultMaxLocalTests      = 1
	DefaultMaxChildProcesses  = 8
	DefaultMaxRSSBytes        = int64(2) << 30 // 2 GiB
	DefaultMaxCPURate         = 1.5            // 150% sustained CPU
	DefaultLeaseTTL           = 5 * time.Minute
)

// Role is the admitted work class.
type Role string

const (
	RoleWorker    Role = "worker"
	RoleVerifier  Role = "verifier"
	RoleLocalTest Role = "local_test"
)

// State is the reservation lifecycle.
type State string

const (
	StateActive            State = "active"
	StateReleased          State = "released"
	StateExpired           State = "expired"
	StateAttentionRequired State = "attention_required"
)

// Metric names for enforcement transitions.
type Metric string

const (
	MetricProcesses Metric = "process_count"
	MetricRSS       Metric = "rss_bytes"
	MetricCPU       Metric = "cpu_rate"
	MetricWorkers   Metric = "active_workers"
	MetricVerifiers Metric = "active_verifiers"
	MetricLocalTest Metric = "local_tests"
)

var (
	// ErrDenied is returned when admission policy refuses the request.
	ErrDenied = errors.New("admission: denied")
	// ErrGenerationMismatch is a generation fence violation.
	ErrGenerationMismatch = errors.New("admission: generation mismatch")
	// ErrNotFound means the reservation id is unknown.
	ErrNotFound = errors.New("admission: reservation not found")
	// ErrAttentionRequired means authority must be resolved before reuse.
	ErrAttentionRequired = errors.New("admission: attention required")
	// ErrInvalidRequest means required identity fields are missing.
	ErrInvalidRequest = errors.New("admission: invalid request")
)

// Budget is the configured machine budget.
type Budget struct {
	MaxActiveWorkers   int
	MaxActiveVerifiers int
	MaxLocalTests      int
	MaxChildProcesses  int
	MaxRSSBytes        int64
	MaxCPURate         float64
}

// DefaultBudget returns issue defaults.
func DefaultBudget() Budget {
	return Budget{
		MaxActiveWorkers:   DefaultMaxActiveWorkers,
		MaxActiveVerifiers: DefaultMaxActiveVerifiers,
		MaxLocalTests:      DefaultMaxLocalTests,
		MaxChildProcesses:  DefaultMaxChildProcesses,
		MaxRSSBytes:        DefaultMaxRSSBytes,
		MaxCPURate:         DefaultMaxCPURate,
	}
}

// ResourceView is an explainable multi-dimension resource snapshot.
// Values never include other projects' private content — only aggregates and
// the caller's own request/reservation identifiers.
type ResourceView struct {
	Workers    int     `json:"workers"`
	Verifiers  int     `json:"verifiers"`
	LocalTests int     `json:"local_tests"`
	Processes  int     `json:"processes"`
	RSSBytes   int64   `json:"rss_bytes"`
	CPURate    float64 `json:"cpu_rate"`
}

// Request is an admission claim.
type Request struct {
	ProjectID      string
	JobID          string
	AttemptID      string
	Role           Role
	Processes      int
	RSSBytes       int64
	CPURate        float64
	IdempotencyKey string
	LeaseTTL       time.Duration
}

// Decision is the explainable policy output.
type Decision struct {
	SchemaVersion     string       `json:"schema_version"`
	Admitted          bool         `json:"admitted"`
	ReservationID     string       `json:"reservation_id,omitempty"`
	Generation        int64        `json:"generation,omitempty"`
	State             State        `json:"state,omitempty"`
	Reasons           []string     `json:"reasons"`
	Requested         ResourceView `json:"requested"`
	Reserved          ResourceView `json:"reserved"`
	Available         ResourceView `json:"available"`
	Denied            ResourceView `json:"denied"`
	Unknown           ResourceView `json:"unknown"`
	AttentionRequired bool         `json:"attention_required"`
	LeaseUntil        time.Time    `json:"lease_until,omitempty"`
	Replay            bool         `json:"replay,omitempty"`
}

// Reservation is a persisted active or terminal claim.
type Reservation struct {
	ID              string
	Generation      int64
	ProjectID       string
	JobID           string
	AttemptID       string
	Role            Role
	State           State
	Processes       int
	RSSBytes        int64
	CPURate         float64
	LeaseUntil      time.Time
	IdempotencyKey  string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	AttentionReason string
}

// ObservedUse is host evidence attached to a reservation (no argv/paths).
type ObservedUse struct {
	ProcessCount int
	RSSBytes     int64
	CPURate      float64
	// Liveness is true when process evidence confirms the reservation owner is live.
	// When Unknown is true, Liveness must not be trusted and authority fails closed.
	Liveness bool
	Unknown  bool
}

// EnforcementRequest is a one-shot threshold transition for enforcement
// consumers. It does not declare attempt failure.
type EnforcementRequest struct {
	ID            string
	ReservationID string
	TransitionKey string
	Metric        Metric
	Observed      float64
	Threshold     float64
	CreatedAt     time.Time
}

// LivenessProbe inspects whether a reservation's processes are still live.
// Implementations must not kill processes.
type LivenessProbe interface {
	// Probe returns known-live, known-dead, or unknown.
	Probe(res Reservation) (live bool, unknown bool, err error)
}

// AlwaysUnknownProbe fails closed (unknown liveness).
type AlwaysUnknownProbe struct{}

func (AlwaysUnknownProbe) Probe(Reservation) (bool, bool, error) { return false, true, nil }

// StaticLivenessProbe returns fixed answers (tests).
type StaticLivenessProbe struct {
	Live    bool
	Unknown bool
}

func (p StaticLivenessProbe) Probe(Reservation) (bool, bool, error) {
	return p.Live, p.Unknown, nil
}
