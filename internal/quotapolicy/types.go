package quotapolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
)

const (
	SchemaScore   = "loopcoder.quota.policy.score.v1"
	SchemaRanking = "loopcoder.quota.policy.ranking.v1"
	PolicyVersion = "quota-policy-v1"
)

// WindowKind separates five-hour, weekly, credit, and rate-limit constraints.
type WindowKind string

const (
	WindowFiveHour  WindowKind = "five_hour"
	WindowWeekly    WindowKind = "weekly"
	WindowCredit    WindowKind = "credit"
	WindowRateLimit WindowKind = "rate_limit"
	WindowOther     WindowKind = "other"
)

// EvidenceClass distinguishes exact numbers from uncertain states.
// Never coerce unknown/stale into numeric zero.
type EvidenceClass string

const (
	EvidenceExact     EvidenceClass = "exact"
	EvidenceEstimated EvidenceClass = "estimated"
	EvidenceUnknown   EvidenceClass = "unknown"
	EvidenceStale     EvidenceClass = "stale"
	EvidenceMissing   EvidenceClass = "missing"
)

// Window is one normalized quota window feature set (provider-agnostic).
type Window struct {
	Kind WindowKind `json:"kind"`
	// RemainingFraction in [0,1] only when Evidence is exact or estimated.
	RemainingFraction *float64      `json:"remaining_fraction,omitempty"`
	Evidence          EvidenceClass `json:"evidence"`
	// TimeToReset is nil when unknown/missing.
	TimeToReset *time.Duration `json:"time_to_reset,omitempty"`
	// Exhausted is true only when remaining is known exact/estimated zero.
	Exhausted bool `json:"exhausted"`
	// RateLimited is true when this window (or sibling rate-limit window) is limited.
	RateLimited bool   `json:"rate_limited"`
	EvidenceID  string `json:"evidence_id,omitempty"`
}

// Candidate is a hard-eligible route with captured soft features.
// Hard eligibility is assumed already decided (V090-051); this package does not
// re-open install/auth/pin gates.
type Candidate struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Windows  []Window `json:"windows"`
	// Reliability in [0,1] recent success rate when known.
	Reliability         *float64      `json:"reliability,omitempty"`
	ReliabilityEvidence EvidenceClass `json:"reliability_evidence"`
	CooldownActive      bool          `json:"cooldown_active"`
	// ConcurrencyLoad in [0,1] (1 = fully loaded).
	ConcurrencyLoad float64 `json:"concurrency_load"`
}

// Policy is versioned weights and reserve floors.
type Policy struct {
	Version string `json:"version"`
	// Soft score weights (non-negative; normalized internally).
	WeightBurnUrgency float64 `json:"weight_burn_urgency"`
	WeightRemaining   float64 `json:"weight_remaining"`
	WeightReliability float64 `json:"weight_reliability"`
	WeightConcurrency float64 `json:"weight_concurrency"`
	// Reserve fractions held for higher capability classes (0-1).
	// When task class is below Soul, SoulReserveFraction of scarce capacity
	// is treated as unavailable for soft ranking of lower-class work.
	SoulReserveFraction float64 `json:"soul_reserve_fraction"`
	TeraReserveFraction float64 `json:"tera_reserve_fraction"`
	// Penalties applied to soft score for uncertain windows (0-1 scale).
	UnknownPenalty float64 `json:"unknown_penalty"`
	StalePenalty   float64 `json:"stale_penalty"`
	// NearResetHorizon: time-to-reset at or below this is "near reset".
	NearResetHorizon time.Duration `json:"near_reset_horizon"`
}

// DefaultPolicy returns reviewed default weights.
func DefaultPolicy() Policy {
	return Policy{
		Version:             PolicyVersion,
		WeightBurnUrgency:   0.40,
		WeightRemaining:     0.25,
		WeightReliability:   0.20,
		WeightConcurrency:   0.15,
		SoulReserveFraction: 0.20,
		TeraReserveFraction: 0.10,
		UnknownPenalty:      0.35,
		StalePenalty:        0.50,
		NearResetHorizon:    2 * time.Hour,
	}
}

// Normalize validates and fills defaults.
func (p Policy) Normalize() (Policy, error) {
	out := p
	if out.Version == "" {
		out.Version = PolicyVersion
	}
	if out.WeightBurnUrgency < 0 || out.WeightRemaining < 0 || out.WeightReliability < 0 || out.WeightConcurrency < 0 {
		return Policy{}, fmt.Errorf("%w: negative weight", ErrInvalid)
	}
	sum := out.WeightBurnUrgency + out.WeightRemaining + out.WeightReliability + out.WeightConcurrency
	if sum <= 0 {
		return Policy{}, fmt.Errorf("%w: weights sum to zero", ErrInvalid)
	}
	// Normalize weights to sum 1.
	out.WeightBurnUrgency /= sum
	out.WeightRemaining /= sum
	out.WeightReliability /= sum
	out.WeightConcurrency /= sum
	if out.SoulReserveFraction < 0 || out.SoulReserveFraction > 1 || out.TeraReserveFraction < 0 || out.TeraReserveFraction > 1 {
		return Policy{}, fmt.Errorf("%w: reserve fraction", ErrInvalid)
	}
	if out.NearResetHorizon <= 0 {
		out.NearResetHorizon = 2 * time.Hour
	}
	return out, nil
}

// Input freezes ranking inputs (injected clock).
type Input struct {
	Policy     Policy         `json:"policy"`
	TaskClass  capclass.Class `json:"task_class"`
	Candidates []Candidate    `json:"candidates"`
	Now        time.Time      `json:"now"`
	// ExplicitPinActive when true: soft ranking must not reorder past a pin;
	// caller should not invoke ranking for pin mode. If invoked, we still score
	// but mark pin_mode_soft_only.
	ExplicitPinActive bool `json:"explicit_pin_active,omitempty"`
}

// Component is one named soft-score contribution.
type Component struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Weight float64 `json:"weight"`
	// Weighted is value * weight (pre-penalty).
	Weighted float64 `json:"weighted"`
	Note     string  `json:"note,omitempty"`
}

// Score is the ordered breakdown for one candidate.
type Score struct {
	Schema          string      `json:"schema"`
	Provider        string      `json:"provider"`
	Model           string      `json:"model"`
	SoftScore       float64     `json:"soft_score"`
	Components      []Component `json:"components"`
	BindingWindow   WindowKind  `json:"binding_window,omitempty"`
	SoftExcluded    bool        `json:"soft_excluded"`
	ExcludeReasons  []string    `json:"exclude_reasons,omitempty"`
	Reasons         []string    `json:"reasons"`
	BurnUrgency     float64     `json:"burn_urgency"`
	RemainingUsable float64     `json:"remaining_usable"`
}

// Ranking is the ordered soft result for V090-053.
type Ranking struct {
	Schema        string `json:"schema"`
	PolicyVersion string `json:"policy_version"`
	TaskClass     string `json:"task_class"`
	// Ordered highest soft score first among non-excluded; excluded follow.
	Scores    []Score   `json:"scores"`
	Reasons   []string  `json:"reasons"`
	Digest    string    `json:"digest"`
	Evaluated time.Time `json:"evaluated_at"`
}

var (
	ErrInvalid = errors.New("quotapolicy: invalid")
)

// Reason codes.
const (
	ReasonNearResetPrefer = "burn.near_reset_prefer"
	ReasonAbundant        = "burn.abundant"
	ReasonScarceWeekly    = "window.weekly_scarce"
	ReasonExhausted       = "window.exhausted"
	ReasonRateLimited     = "window.rate_limited"
	ReasonReserveBreach   = "reserve.breach"
	ReasonUnknownEvidence = "evidence.unknown"
	ReasonStaleEvidence   = "evidence.stale"
	ReasonNoTelemetry     = "evidence.no_telemetry"
	ReasonReliabilityLow  = "reliability.low"
	ReasonCooldown        = "reliability.cooldown"
	ReasonConcurrency     = "concurrency.load"
	ReasonTieBroken       = "rank.tie_provider_model"
	ReasonSoftExcluded    = "rank.soft_excluded"
	ReasonPinMode         = "pin.mode_soft_only"
)

func DigestOf(r Ranking) string {
	cp := r
	cp.Digest = ""
	b, err := json.Marshal(cp)
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

func keyOf(c Candidate) string {
	return strings.ToLower(strings.TrimSpace(c.Provider)) + "/" + strings.TrimSpace(c.Model)
}

func sortScores(scores []Score) {
	sort.SliceStable(scores, func(i, j int) bool {
		// non-excluded first
		if scores[i].SoftExcluded != scores[j].SoftExcluded {
			return !scores[i].SoftExcluded && scores[j].SoftExcluded
		}
		if scores[i].SoftScore != scores[j].SoftScore {
			return scores[i].SoftScore > scores[j].SoftScore
		}
		// deterministic tie-break: provider then model
		if scores[i].Provider != scores[j].Provider {
			return scores[i].Provider < scores[j].Provider
		}
		return scores[i].Model < scores[j].Model
	})
}
