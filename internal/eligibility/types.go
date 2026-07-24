package eligibility

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
)

const (
	SchemaDecision  = "loopcoder.route.eligibility.v1"
	SchemaCandidate = "loopcoder.route.candidate.v1"
	SchemaExclusion = "loopcoder.route.exclusion.v1"
	PolicyVersion   = "hard-eligibility-v1"
	ModeExplicitPin = "explicit_pin"
	ModeAutomatic   = "automatic"
)

// Freshness of captured evidence.
type Freshness string

const (
	FreshFresh   Freshness = "fresh"
	FreshStale   Freshness = "stale"
	FreshUnknown Freshness = "unknown"
	FreshExpired Freshness = "expired"
	FreshMissing Freshness = "missing"
)

// FactState is the truth value of a hard prerequisite in the snapshot.
type FactState string

const (
	FactTrue    FactState = "true"
	FactFalse   FactState = "false"
	FactUnknown FactState = "unknown"
)

// Fact is one captured evidence cell (never live network).
type Fact struct {
	State      FactState `json:"state"`
	EvidenceID string    `json:"evidence_id,omitempty"`
	Freshness  Freshness `json:"freshness,omitempty"`
	Note       string    `json:"note,omitempty"`
}

// KnownTrue reports whether the fact is known true and usable for hard gates.
func (f Fact) KnownTrue() bool {
	return f.State == FactTrue && f.Freshness != FreshStale && f.Freshness != FreshExpired && f.Freshness != FreshMissing
}

// KnownFalse reports whether the fact is known false.
func (f Fact) KnownFalse() bool { return f.State == FactFalse }

// IsUnknown reports unknown/missing/stale/expired evidence.
func (f Fact) IsUnknown() bool {
	if f.State == FactUnknown || f.State == "" {
		return true
	}
	switch f.Freshness {
	case FreshStale, FreshExpired, FreshMissing, FreshUnknown:
		return true
	}
	return false
}

// PinFields is an immutable explicit owner pin (provider/model/effort/permission).
type PinFields struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Effort     string `json:"effort,omitempty"`
	Permission string `json:"permission,omitempty"`
}

// Normalize trims and lowercases provider; rejects empty provider/model.
func (p PinFields) Normalize() (PinFields, error) {
	out := PinFields{
		Provider:   strings.ToLower(strings.TrimSpace(p.Provider)),
		Model:      strings.TrimSpace(p.Model),
		Effort:     strings.TrimSpace(p.Effort),
		Permission: strings.TrimSpace(p.Permission),
	}
	if out.Provider == "" || out.Model == "" {
		return PinFields{}, fmt.Errorf("%w: pin provider and model required", ErrInvalid)
	}
	return out, nil
}

// Policy is allow/deny lists over provider/model keys (empty allow = all allowed).
type Policy struct {
	// AllowProvider when non-empty restricts to listed providers.
	AllowProvider []string `json:"allow_provider,omitempty"`
	// DenyProvider always excludes.
	DenyProvider []string `json:"deny_provider,omitempty"`
	// AllowModel keys are "provider/model".
	AllowModel []string `json:"allow_model,omitempty"`
	DenyModel  []string `json:"deny_model,omitempty"`
}

// MachineAdmission is concurrency / resource fit from a captured snapshot.
type MachineAdmission struct {
	// CapacityOK true when machine can host another child.
	CapacityOK Fact `json:"capacity_ok"`
	// ConcurrentSlots remaining soft count (informational; not a soft score input here).
	ConcurrentSlots int `json:"concurrent_slots,omitempty"`
}

// Candidate is one route option with pre-captured hard evidence.
// Quota remaining is deliberately NOT a hard-eligibility input.
// AccountRef/WindowKind are first-class capacity identity (exact equality).
type Candidate struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Effort     string `json:"effort,omitempty"`
	Permission string `json:"permission,omitempty"`
	// AccountRef is the canonical redacted capacity account for this candidate.
	AccountRef string `json:"account_ref,omitempty"`
	// InstallRef is the exact provider installation identity for this candidate.
	InstallRef string `json:"install_ref,omitempty"`
	// WindowKind is the capacity window bound to this candidate.
	WindowKind string `json:"window_kind,omitempty"`
	// ModelClass is the capability class of this model (from capclass map data).
	ModelClass capclass.Class `json:"model_class"`

	Installed      Fact `json:"installed"`
	Authenticated  Fact `json:"authenticated"`
	ModelPresent   Fact `json:"model_present"`
	PermissionOK   Fact `json:"permission_ok"`
	EffortOK       Fact `json:"effort_ok"`
	Healthy        Fact `json:"healthy"`
	CooldownActive Fact `json:"cooldown_active"` // true = on cooldown = ineligible
	// ResourceFit is machine/process fit for this candidate.
	ResourceFit Fact `json:"resource_fit"`
	// QuotaRemaining is captured for explain only; MUST NOT make an incompatible
	// route eligible (acceptance #3).
	QuotaRemaining int64 `json:"quota_remaining,omitempty"`
}

// Snapshot freezes all inputs for pure evaluation.
type Snapshot struct {
	// TaskRequiredClass comes from capclass.Classify (or override).
	TaskRequiredClass capclass.Class `json:"task_required_class"`
	// ExplicitPin when set selects ModeExplicitPin.
	ExplicitPin *PinFields       `json:"explicit_pin,omitempty"`
	Policy      Policy           `json:"policy"`
	Candidates  []Candidate      `json:"candidates"`
	Machine     MachineAdmission `json:"machine"`
	// CapturedAt is part of explain; digest excludes wall-clock variability by
	// using the provided value as frozen input.
	CapturedAt time.Time `json:"captured_at"`
}

// Decision is the immutable eligibility result.
type Decision struct {
	Schema        string `json:"schema"`
	PolicyVersion string `json:"policy_version"`
	Mode          string `json:"mode"`
	// PinSelected is set when an eligible explicit pin wins unchanged.
	PinSelected *CandidateView `json:"pin_selected,omitempty"`
	// FailClosed is true when explicit pin is ineligible (no fallback).
	FailClosed bool `json:"fail_closed"`
	// Eligible candidates in stable provider/model order (empty when fail-closed pin).
	Eligible []CandidateView `json:"eligible"`
	// Excluded every rejected candidate with ordered reason codes.
	Excluded []Exclusion `json:"excluded"`
	// Reasons top-level decision reasons.
	Reasons []string `json:"reasons"`
	Digest  string   `json:"digest"`
}

// CandidateView is a normalized eligible/excluded row.
type CandidateView struct {
	Schema     string          `json:"schema"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	Effort     string          `json:"effort,omitempty"`
	Permission string          `json:"permission,omitempty"`
	AccountRef string          `json:"account_ref,omitempty"`
	InstallRef string          `json:"install_ref,omitempty"`
	WindowKind string          `json:"window_kind,omitempty"`
	ModelClass capclass.Class  `json:"model_class"`
	Eligible   bool            `json:"eligible"`
	Reasons    []string        `json:"reasons"`
	Evidence   map[string]Fact `json:"evidence"`
}

// Exclusion is one excluded candidate with ordered reason codes.
type Exclusion struct {
	Schema     string   `json:"schema"`
	Provider   string   `json:"provider"`
	Model      string   `json:"model"`
	Reasons    []string `json:"reasons"`
	EvidenceID []string `json:"evidence_ids,omitempty"`
}

var (
	ErrInvalid = errors.New("eligibility: invalid")
)

// Reason codes (stable, ordered in evaluation).
const (
	ReasonPinMatch            = "pin.match"
	ReasonPinMismatch         = "pin.mismatch"
	ReasonPinIneligible       = "pin.ineligible_fail_closed"
	ReasonPolicyDenyProvider  = "policy.deny_provider"
	ReasonPolicyDenyModel     = "policy.deny_model"
	ReasonPolicyNotAllowProv  = "policy.not_in_allow_provider"
	ReasonPolicyNotAllowModel = "policy.not_in_allow_model"
	ReasonNotInstalled        = "install.missing"
	ReasonInstallUnknown      = "install.unknown"
	ReasonAuthMissing         = "auth.missing"
	ReasonAuthUnknown         = "auth.unknown"
	ReasonModelMissing        = "model.missing"
	ReasonModelUnknown        = "model.unknown"
	ReasonPermissionDenied    = "permission.denied"
	ReasonPermissionUnknown   = "permission.unknown"
	ReasonEffortUnsupported   = "effort.unsupported"
	ReasonEffortUnknown       = "effort.unknown"
	ReasonTaskClass           = "task.class_unmet"
	ReasonTaskClassNeedsHuman = "task.needs_human"
	ReasonUnhealthy           = "health.unhealthy"
	ReasonHealthUnknown       = "health.unknown"
	ReasonCooldown            = "health.cooldown"
	ReasonCooldownUnknown     = "health.cooldown_unknown"
	ReasonResourceUnfit       = "resource.unfit"
	ReasonResourceUnknown     = "resource.unknown"
	ReasonMachineCapacity     = "machine.capacity"
	ReasonMachineUnknown      = "machine.capacity_unknown"
	ReasonStaleEvidence       = "evidence.stale_or_expired"
	ReasonQuotaIgnored        = "quota.ignored_for_hard_eligibility"
	ReasonEligible            = "eligible"
)

// DigestOf returns a stable digest of the decision (replay identity).
func DigestOf(d Decision) string {
	// Clear digest field for hashing.
	cp := d
	cp.Digest = ""
	b, err := json.Marshal(cp)
	if err != nil {
		return "sha256:error"
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

func normKey(provider, model string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "/" + strings.TrimSpace(model)
}

func inList(list []string, v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, x := range list {
		if strings.ToLower(strings.TrimSpace(x)) == v {
			return true
		}
	}
	return false
}

func collectEvidenceIDs(c Candidate) []string {
	facts := []Fact{c.Installed, c.Authenticated, c.ModelPresent, c.PermissionOK, c.EffortOK, c.Healthy, c.CooldownActive, c.ResourceFit}
	var ids []string
	seen := map[string]struct{}{}
	for _, f := range facts {
		id := strings.TrimSpace(f.EvidenceID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func evidenceMap(c Candidate) map[string]Fact {
	return map[string]Fact{
		"installed":       c.Installed,
		"authenticated":   c.Authenticated,
		"model_present":   c.ModelPresent,
		"permission_ok":   c.PermissionOK,
		"effort_ok":       c.EffortOK,
		"healthy":         c.Healthy,
		"cooldown_active": c.CooldownActive,
		"resource_fit":    c.ResourceFit,
	}
}
