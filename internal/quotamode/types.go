package quotamode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

const (
	SchemaReservation = "loopcoder.quota.soft_reservation.v1"
	SchemaAttribution = "loopcoder.quota.usage_attribution.v1"
	SchemaModeRank    = "loopcoder.quota.mode_ranking.v1"
	PolicyVersion     = "quota-mode-v1"
)

// Mode is an owner-selectable quota spending policy.
type Mode string

const (
	// ModeBalanced spreads soft preference across providers/windows.
	ModeBalanced Mode = "balanced"
	// ModeBurnBeforeReset prefers near-reset capacity more aggressively.
	ModeBurnBeforeReset Mode = "burn_before_reset"
	// ModePreservePremium holds premium (Soul-class / scarce weekly) capacity.
	ModePreservePremium Mode = "preserve_premium"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool {
	switch m {
	case ModeBalanced, ModeBurnBeforeReset, ModePreservePremium:
		return true
	}
	return false
}

// ReservationState is the lifecycle of a soft reservation.
type ReservationState string

const (
	StatePending    ReservationState = "pending"
	StateActive     ReservationState = "active"
	StateReleased   ReservationState = "released"
	StateReconciled ReservationState = "reconciled"
	StateExpired    ReservationState = "expired"
	StateRefused    ReservationState = "refused"
	StateCancelled  ReservationState = "cancelled"
)

// WindowKey identifies one normalized window for reservation accounting.
type WindowKey struct {
	Provider string                 `json:"provider"`
	Account  string                 `json:"account,omitempty"`
	Model    string                 `json:"model"`
	Window   quotapolicy.WindowKind `json:"window"`
}

func (k WindowKey) String() string {
	return strings.ToLower(strings.TrimSpace(k.Provider)) + "|" +
		strings.TrimSpace(k.Account) + "|" +
		strings.TrimSpace(k.Model) + "|" +
		string(k.Window)
}

// SnapshotRemaining is captured remaining fraction for a window (0-1) with evidence.
type SnapshotRemaining struct {
	Key               WindowKey                 `json:"key"`
	RemainingFraction float64                   `json:"remaining_fraction"`
	Evidence          quotapolicy.EvidenceClass `json:"evidence"`
	EvidenceID        string                    `json:"evidence_id,omitempty"`
}

// Reservation is a short-lived soft hold on remaining fraction.
type Reservation struct {
	Schema    string    `json:"schema"`
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	AttemptID string    `json:"attempt_id"`
	Key       WindowKey `json:"key"`
	// Fraction held in [0,1] of the window limit (not absolute tokens).
	Fraction    float64          `json:"fraction"`
	State       ReservationState `json:"state"`
	CreatedAt   time.Time        `json:"created_at"`
	ExpiresAt   time.Time        `json:"expires_at"`
	SnapshotEID string           `json:"snapshot_evidence_id,omitempty"`
	// DemandEstimate is estimated consumption fraction for headroom checks.
	DemandEstimate float64                   `json:"demand_estimate"`
	DemandEvidence quotapolicy.EvidenceClass `json:"demand_evidence"`
	// RiskAccepted records owner override of headroom rejection.
	RiskAccepted bool   `json:"risk_accepted,omitempty"`
	RiskActor    string `json:"risk_actor,omitempty"`
	RiskReason   string `json:"risk_reason,omitempty"`
	// Terminal outcome notes.
	TerminalReason string    `json:"terminal_reason,omitempty"`
	TerminalAt     time.Time `json:"terminal_at,omitempty"`
}

// Attribution records post-attempt usage without rewriting provider evidence.
type Attribution struct {
	Schema        string    `json:"schema"`
	ID            string    `json:"id"`
	ReservationID string    `json:"reservation_id"`
	AttemptID     string    `json:"attempt_id"`
	Key           WindowKey `json:"key"`
	// ObservedFraction is local estimate of consumption (not provider truth).
	ObservedFraction float64                   `json:"observed_fraction"`
	Source           string                    `json:"source"` // receipt|local_tokens|estimate
	Confidence       quotapolicy.EvidenceClass `json:"confidence"`
	// Drift is observed - reserved (can be negative).
	Drift     float64   `json:"drift"`
	CreatedAt time.Time `json:"created_at"`
	Note      string    `json:"note,omitempty"`
}

// HeadroomRejectReason distinguishes completion-headroom failures.
type HeadroomRejectReason string

const (
	HeadroomOK              HeadroomRejectReason = ""
	HeadroomExactExhausted  HeadroomRejectReason = "exact_exhaustion"
	HeadroomUnknownQuota    HeadroomRejectReason = "unknown_quota"
	HeadroomEstimatedDemand HeadroomRejectReason = "estimated_demand_exceeds"
	HeadroomOvercommit      HeadroomRejectReason = "overcommit_with_active_reservations"
	HeadroomRiskAccepted    HeadroomRejectReason = "owner_risk_accepted"
)

var (
	ErrInvalid     = errors.New("quotamode: invalid")
	ErrConflict    = errors.New("quotamode: reservation conflict")
	ErrNotFound    = errors.New("quotamode: not found")
	ErrHeadroom    = errors.New("quotamode: headroom")
	ErrImmutable   = errors.New("quotamode: terminal reservation immutable")
	ErrPinOverride = errors.New("quotamode: mode cannot substitute pin")
)

// ModeConfig tunes mode-specific ranking and reserve behavior.
type ModeConfig struct {
	Mode Mode `json:"mode"`
	// CompletionHeadroom is minimum remaining fraction that must remain after
	// demand + active reservations (0-1).
	CompletionHeadroom float64 `json:"completion_headroom"`
	// SoftTTL is default reservation lifetime.
	SoftTTL time.Duration `json:"soft_ttl"`
	// PreservePremiumFraction extra hold in preserve_premium mode.
	PreservePremiumFraction float64 `json:"preserve_premium_fraction"`
	// BurnBoost multiplies burn urgency weight in burn_before_reset.
	BurnBoost float64 `json:"burn_boost"`
}

// DefaultModeConfig returns defaults for a mode.
func DefaultModeConfig(m Mode) ModeConfig {
	cfg := ModeConfig{
		Mode:                    m,
		CompletionHeadroom:      0.05,
		SoftTTL:                 15 * time.Minute,
		PreservePremiumFraction: 0.15,
		BurnBoost:               1.0,
	}
	switch m {
	case ModeBurnBeforeReset:
		cfg.BurnBoost = 1.75
		cfg.CompletionHeadroom = 0.02
	case ModePreservePremium:
		cfg.PreservePremiumFraction = 0.25
		cfg.BurnBoost = 0.75
		cfg.CompletionHeadroom = 0.10
	case ModeBalanced:
		// defaults
	}
	return cfg
}

// Store is an in-memory machine-local soft reservation ledger (injected clock).
type Store struct {
	mu   sync.Mutex
	byID map[string]*Reservation
	// activeByKey lists active/pending reservation IDs per window key.
	activeByKey map[string][]string
	attr        map[string]*Attribution
	now         func() time.Time
	seq         int64
}

// NewStore creates a store with injected clock.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		byID:        map[string]*Reservation{},
		activeByKey: map[string][]string{},
		attr:        map[string]*Attribution{},
		now:         now,
	}
}

func digestJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "sha256:error"
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func terminal(st ReservationState) bool {
	switch st {
	case StateReleased, StateReconciled, StateExpired, StateRefused, StateCancelled:
		return true
	}
	return false
}

// ActiveFraction sums non-terminal reservations on a key.
func (s *Store) ActiveFraction(key WindowKey) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeFractionLocked(key)
}

func (s *Store) activeFractionLocked(key WindowKey) float64 {
	ids := s.activeByKey[key.String()]
	var sum float64
	for _, id := range ids {
		r := s.byID[id]
		if r == nil || terminal(r.State) {
			continue
		}
		if r.State == StateActive || r.State == StatePending {
			sum += r.Fraction
		}
	}
	return sum
}

// Reserve tries to create a soft reservation. Conflict is deterministic when
// remaining - active - demand - headroom < 0 (unless risk accepted).
func (s *Store) Reserve(req ReserveRequest) (Reservation, error) {
	if s == nil {
		return Reservation{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := req.validate(); err != nil {
		return Reservation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	// Expire stale first (visible evidence via state transition).
	s.expireLocked(now)

	active := s.activeFractionLocked(req.Key)
	avail, reject := headroomAvailable(req.Snapshot, active, req.DemandEstimate, req.DemandEvidence, req.Config.CompletionHeadroom, req.RiskAccepted)
	if reject != HeadroomOK && reject != HeadroomRiskAccepted {
		ref := Reservation{
			Schema: SchemaReservation, ProjectID: req.ProjectID, AttemptID: req.AttemptID,
			Key: req.Key, Fraction: req.DemandEstimate, State: StateRefused,
			CreatedAt: now, ExpiresAt: now,
			DemandEstimate: req.DemandEstimate, DemandEvidence: req.DemandEvidence,
			TerminalReason: string(reject), TerminalAt: now,
			SnapshotEID: req.Snapshot.EvidenceID,
		}
		s.seq++
		ref.ID = fmt.Sprintf("sres_%d", s.seq)
		s.byID[ref.ID] = &ref
		return ref, fmt.Errorf("%w: %s", ErrHeadroom, reject)
	}

	// Deterministic conflict: two concurrent full claims cannot both succeed.
	// Hold demand fraction (or explicit Fraction).
	hold := req.DemandEstimate
	if req.Fraction > 0 {
		hold = req.Fraction
	}
	hold = clamp01(hold)
	if hold <= 0 {
		return Reservation{}, fmt.Errorf("%w: hold fraction", ErrInvalid)
	}
	// avail already subtracts active reservations + completion headroom.
	if hold > avail+1e-9 && !req.RiskAccepted {
		return Reservation{}, fmt.Errorf("%w: concurrent capacity", ErrConflict)
	}

	ttl := req.Config.SoftTTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	s.seq++
	id := fmt.Sprintf("sres_%d", s.seq)
	r := &Reservation{
		Schema: SchemaReservation, ID: id,
		ProjectID: req.ProjectID, AttemptID: req.AttemptID,
		Key: req.Key, Fraction: hold, State: StateActive,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
		SnapshotEID:    req.Snapshot.EvidenceID,
		DemandEstimate: req.DemandEstimate, DemandEvidence: req.DemandEvidence,
		RiskAccepted: req.RiskAccepted, RiskActor: req.RiskActor, RiskReason: req.RiskReason,
	}
	s.byID[id] = r
	k := req.Key.String()
	s.activeByKey[k] = append(s.activeByKey[k], id)
	return *r, nil
}

// ReserveRequest is the input to Reserve.
type ReserveRequest struct {
	ProjectID      string
	AttemptID      string
	Key            WindowKey
	Snapshot       SnapshotRemaining
	DemandEstimate float64
	DemandEvidence quotapolicy.EvidenceClass
	Fraction       float64 // optional override hold size
	Config         ModeConfig
	RiskAccepted   bool
	RiskActor      string
	RiskReason     string
}

func (r ReserveRequest) validate() error {
	if strings.TrimSpace(r.ProjectID) == "" || strings.TrimSpace(r.AttemptID) == "" {
		return fmt.Errorf("%w: project and attempt required", ErrInvalid)
	}
	if strings.TrimSpace(r.Key.Provider) == "" || strings.TrimSpace(r.Key.Model) == "" {
		return fmt.Errorf("%w: window key", ErrInvalid)
	}
	if !r.Config.Mode.Valid() {
		return fmt.Errorf("%w: mode", ErrInvalid)
	}
	if r.RiskAccepted && (strings.TrimSpace(r.RiskActor) == "" || strings.TrimSpace(r.RiskReason) == "") {
		return fmt.Errorf("%w: risk actor/reason required", ErrInvalid)
	}
	return nil
}

func headroomAvailable(snap SnapshotRemaining, active, demand float64, demEv quotapolicy.EvidenceClass, headroom float64, riskOK bool) (float64, HeadroomRejectReason) {
	switch snap.Evidence {
	case quotapolicy.EvidenceUnknown, quotapolicy.EvidenceMissing, "":
		if riskOK {
			return 0, HeadroomRiskAccepted
		}
		return 0, HeadroomUnknownQuota
	case quotapolicy.EvidenceStale:
		if riskOK {
			return 0, HeadroomRiskAccepted
		}
		return 0, HeadroomUnknownQuota
	case quotapolicy.EvidenceExact, quotapolicy.EvidenceEstimated:
		// ok
	default:
		return 0, HeadroomUnknownQuota
	}
	rem := clamp01(snap.RemainingFraction)
	if rem <= 0 {
		return 0, HeadroomExactExhausted
	}
	// Available for new holds after active reservations and completion headroom.
	avail := rem - active - headroom
	if avail < 0 {
		avail = 0
	}
	need := clamp01(demand)
	if demEv == quotapolicy.EvidenceEstimated || demEv == quotapolicy.EvidenceUnknown {
		// estimated demand: reject when need > avail unless risk
		if need > avail+1e-9 {
			if riskOK {
				return avail, HeadroomRiskAccepted
			}
			if demEv == quotapolicy.EvidenceUnknown {
				return avail, HeadroomUnknownQuota
			}
			return avail, HeadroomEstimatedDemand
		}
	} else if need > avail+1e-9 {
		if active > 0 {
			return avail, HeadroomOvercommit
		}
		return avail, HeadroomExactExhausted
	}
	return avail, HeadroomOK
}

func (s *Store) expireLocked(now time.Time) {
	for id, r := range s.byID {
		if r == nil || terminal(r.State) {
			continue
		}
		if !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt) {
			r.State = StateExpired
			r.TerminalAt = now
			r.TerminalReason = "stale_ttl"
			_ = id
		}
	}
}

// ExpireStale marks expired reservations (idempotent).
func (s *Store) ExpireStale() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	before := 0
	for _, r := range s.byID {
		if r != nil && r.State == StateExpired {
			before++
		}
	}
	s.expireLocked(now)
	after := 0
	for _, r := range s.byID {
		if r != nil && r.State == StateExpired {
			after++
		}
	}
	return after - before
}

// Release terminates a reservation (cancel/timeout/start-refusal). Idempotent.
func (s *Store) Release(id, reason string) (Reservation, error) {
	return s.terminal(id, StateReleased, reason)
}

// Cancel is Release with cancelled state.
func (s *Store) Cancel(id, reason string) (Reservation, error) {
	return s.terminal(id, StateCancelled, reason)
}

func (s *Store) terminal(id string, st ReservationState, reason string) (Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[strings.TrimSpace(id)]
	if !ok {
		return Reservation{}, ErrNotFound
	}
	if terminal(r.State) {
		// idempotent: return current
		return *r, nil
	}
	r.State = st
	r.TerminalAt = s.now().UTC()
	r.TerminalReason = reason
	return *r, nil
}

// Reconcile attaches usage attribution and marks reconciled.
func (s *Store) Reconcile(reservationID string, observed float64, source string, conf quotapolicy.EvidenceClass) (Attribution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[strings.TrimSpace(reservationID)]
	if !ok {
		return Attribution{}, ErrNotFound
	}
	if r.State != StateActive && r.State != StatePending && r.State != StateReleased {
		// allow reconcile from active/released; not from expired twice as new
		if r.State == StateReconciled {
			// idempotent return existing attr if any
			for _, a := range s.attr {
				if a.ReservationID == r.ID {
					return *a, nil
				}
			}
		}
	}
	now := s.now().UTC()
	obs := clamp01(observed)
	s.seq++
	a := &Attribution{
		Schema: SchemaAttribution, ID: fmt.Sprintf("qattr_%d", s.seq),
		ReservationID: r.ID, AttemptID: r.AttemptID, Key: r.Key,
		ObservedFraction: obs, Source: source, Confidence: conf,
		Drift: obs - r.Fraction, CreatedAt: now,
		Note: "local estimate; not provider-authoritative remaining",
	}
	s.attr[a.ID] = a
	r.State = StateReconciled
	r.TerminalAt = now
	r.TerminalReason = "reconciled"
	return *a, nil
}

// Get returns a reservation by ID.
func (s *Store) Get(id string) (Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[strings.TrimSpace(id)]
	if !ok {
		return Reservation{}, ErrNotFound
	}
	return *r, nil
}
