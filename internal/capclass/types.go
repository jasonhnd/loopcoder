package capclass

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// SchemaClassification is the immutable classification envelope.
	SchemaClassification = "loopcoder.capability.classification.v1"
	// SchemaOverride is an owner override record.
	SchemaOverride = "loopcoder.capability.override.v1"
	// SchemaModelMap is the data-only model capability table.
	SchemaModelMap = "loopcoder.capability.model_map.v1"
	// PolicyVersion is the versioned rule set identity.
	PolicyVersion = "capability-class-v1"
)

// Class is a provider-neutral capability class.
type Class string

const (
	// ClassLuna is narrow routine work (docs polish, tiny pure fixes).
	ClassLuna Class = "luna"
	// ClassTera is standard bounded implementation.
	ClassTera Class = "tera"
	// ClassSoul is high-risk architecture, security, migration, or complex reasoning.
	ClassSoul Class = "soul"
	// ClassNeedsHuman stops automatic routing when evidence cannot be trusted.
	ClassNeedsHuman Class = "needs_human"
)

// Rank returns a total order for conservative max selection.
// Higher rank is more conservative / capable. Unknown rank is 0.
func (c Class) Rank() int {
	switch c {
	case ClassLuna:
		return 1
	case ClassTera:
		return 2
	case ClassSoul:
		return 3
	case ClassNeedsHuman:
		return 4
	default:
		return 0
	}
}

// Valid reports whether c is a known class token.
func (c Class) Valid() bool {
	return c.Rank() > 0
}

// EvidenceState is whether a risk input is known.
type EvidenceState string

const (
	EvidenceKnown   EvidenceState = "known"
	EvidenceUnknown EvidenceState = "unknown"
	EvidenceAbsent  EvidenceState = "absent"
)

// Risk field value tokens (when EvidenceKnown).
const (
	ChangeDocs         = "docs"
	ChangeCode         = "code"
	ChangeConfig       = "config"
	ChangeMigration    = "migration"
	ChangeArchitecture = "architecture"
	ChangeRelease      = "release"
	ChangeUnknown      = "unknown"

	TestNone        = "none"
	TestUnit        = "unit"
	TestIntegration = "integration"
	TestSystem      = "system"
	TestUnknown     = "unknown"

	RevEasy         = "easy"
	RevHard         = "hard"
	RevIrreversible = "irreversible"
	RevUnknown      = "unknown"
)

// RiskEvidence is the deterministic risk input surface for classification.
// Every field must be listed in the result, including unknown/absent.
type RiskEvidence struct {
	// ChangeType: docs|code|config|migration|architecture|release|unknown
	ChangeType      string        `json:"change_type"`
	ChangeTypeState EvidenceState `json:"change_type_state"`

	// OwnershipAffected: multi-owner / exclusive boundary impact.
	OwnershipAffected      bool          `json:"ownership_affected"`
	OwnershipAffectedState EvidenceState `json:"ownership_affected_state"`

	Migration      bool          `json:"migration"`
	MigrationState EvidenceState `json:"migration_state"`

	Security      bool          `json:"security"`
	SecurityState EvidenceState `json:"security_state"`

	Concurrency      bool          `json:"concurrency"`
	ConcurrencyState EvidenceState `json:"concurrency_state"`

	ExternalSideEffects      bool          `json:"external_side_effects"`
	ExternalSideEffectsState EvidenceState `json:"external_side_effects_state"`

	// TestBreadth: none|unit|integration|system|unknown
	TestBreadth      string        `json:"test_breadth"`
	TestBreadthState EvidenceState `json:"test_breadth_state"`

	// Reversibility: easy|hard|irreversible|unknown
	Reversibility      string        `json:"reversibility"`
	ReversibilityState EvidenceState `json:"reversibility_state"`

	Ambiguity      bool          `json:"ambiguity"`
	AmbiguityState EvidenceState `json:"ambiguity_state"`
}

// Reason is one explainable rule hit.
type Reason struct {
	Code    string `json:"code"`
	Input   string `json:"input"`
	Detail  string `json:"detail"`
	Raises  Class  `json:"raises"`
	Floor   Class  `json:"floor"`
	Unknown bool   `json:"unknown,omitempty"`
}

// Classification is an immutable required-class decision with full explain.
type Classification struct {
	Schema        string `json:"schema"`
	PolicyVersion string `json:"policy_version"`
	RequiredClass Class  `json:"required_class"`
	// RiskInputs lists every risk input name → value|state (acceptance #1).
	RiskInputs map[string]string `json:"risk_inputs"`
	Reasons    []Reason          `json:"reasons"`
	// BaseClass is the class before owner override (if any).
	BaseClass Class `json:"base_class"`
	// OverrideID is set when an owner override was applied.
	OverrideID string `json:"override_id,omitempty"`
	Digest     string `json:"digest"`
}

// ModelCapability maps one canonical model ID to a capability class.
// Adding a newly observed model changes only this data (and tests), never
// scheduler code (acceptance #5).
type ModelCapability struct {
	Provider string `json:"provider"`
	// ModelID is the canonical model identity, not a marketing alias.
	ModelID string `json:"model_id"`
	Class   Class  `json:"class"`
}

// ModelMap is a versioned, data-only capability table.
type ModelMap struct {
	Schema  string            `json:"schema"`
	Version string            `json:"version"`
	Entries []ModelCapability `json:"entries"`
}

// OverrideDirection is raise (more capable) or lower (less capable).
type OverrideDirection string

const (
	OverrideRaise OverrideDirection = "raise"
	OverrideLower OverrideDirection = "lower"
)

// Override is an append-only owner correction with actor and reason.
type Override struct {
	Schema    string            `json:"schema"`
	ID        string            `json:"id"`
	Actor     string            `json:"actor"`
	Reason    string            `json:"reason"`
	Direction OverrideDirection `json:"direction"`
	// TargetClass is the owner-requested class.
	TargetClass Class `json:"target_class"`
	// AttemptID when non-empty binds this override to an attempt; once the
	// attempt is active the route cannot be mutated via a second override.
	AttemptID string    `json:"attempt_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	ErrInvalid     = errors.New("capclass: invalid")
	ErrImmutable   = errors.New("capclass: active attempt route immutable")
	ErrNotFound    = errors.New("capclass: not found")
	ErrUnsupported = errors.New("capclass: unsupported")
)

// MaxClass returns the more conservative of a and b.
func MaxClass(a, b Class) Class {
	if a.Rank() >= b.Rank() {
		return a
	}
	return b
}

// DigestOf returns a stable digest of classification content.
func DigestOf(c Classification) string {
	// Normalize for stability.
	type wire struct {
		Schema        string            `json:"schema"`
		PolicyVersion string            `json:"policy_version"`
		RequiredClass Class             `json:"required_class"`
		RiskInputs    map[string]string `json:"risk_inputs"`
		Reasons       []Reason          `json:"reasons"`
		BaseClass     Class             `json:"base_class"`
		OverrideID    string            `json:"override_id,omitempty"`
	}
	w := wire{
		Schema:        c.Schema,
		PolicyVersion: c.PolicyVersion,
		RequiredClass: c.RequiredClass,
		RiskInputs:    c.RiskInputs,
		Reasons:       c.Reasons,
		BaseClass:     c.BaseClass,
		OverrideID:    c.OverrideID,
	}
	// Sort reason codes already deterministic from Classify; encode JSON stable
	// by sorting risk input keys via encoding with sorted maps (Go json maps
	// are sorted by key).
	b, err := json.Marshal(w)
	if err != nil {
		return "sha256:error"
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

// FormatRiskInputs materializes every risk dimension for explain surfaces.
func FormatRiskInputs(e RiskEvidence) map[string]string {
	fmtBool := func(v bool, st EvidenceState) string {
		if st == "" {
			st = EvidenceAbsent
		}
		if st != EvidenceKnown {
			return string(st)
		}
		if v {
			return "true"
		}
		return "false"
	}
	fmtStr := func(v string, st EvidenceState) string {
		if st == "" {
			st = EvidenceAbsent
		}
		if st != EvidenceKnown {
			return string(st)
		}
		if v == "" {
			return "empty"
		}
		return v
	}
	return map[string]string{
		"change_type":           fmtStr(e.ChangeType, e.ChangeTypeState),
		"ownership_affected":    fmtBool(e.OwnershipAffected, e.OwnershipAffectedState),
		"migration":             fmtBool(e.Migration, e.MigrationState),
		"security":              fmtBool(e.Security, e.SecurityState),
		"concurrency":           fmtBool(e.Concurrency, e.ConcurrencyState),
		"external_side_effects": fmtBool(e.ExternalSideEffects, e.ExternalSideEffectsState),
		"test_breadth":          fmtStr(e.TestBreadth, e.TestBreadthState),
		"reversibility":         fmtStr(e.Reversibility, e.ReversibilityState),
		"ambiguity":             fmtBool(e.Ambiguity, e.AmbiguityState),
	}
}

// NormalizeModelMap validates and canonicalizes entries (provider lower, trim).
func NormalizeModelMap(m ModelMap) (ModelMap, error) {
	out := ModelMap{
		Schema:  SchemaModelMap,
		Version: strings.TrimSpace(m.Version),
		Entries: make([]ModelCapability, 0, len(m.Entries)),
	}
	if out.Version == "" {
		return ModelMap{}, fmt.Errorf("%w: model map version required", ErrInvalid)
	}
	seen := map[string]struct{}{}
	for _, e := range m.Entries {
		p := strings.ToLower(strings.TrimSpace(e.Provider))
		id := strings.TrimSpace(e.ModelID)
		if p == "" || id == "" {
			return ModelMap{}, fmt.Errorf("%w: provider and model_id required", ErrInvalid)
		}
		if !e.Class.Valid() || e.Class == ClassNeedsHuman {
			return ModelMap{}, fmt.Errorf("%w: model class must be luna|tera|soul", ErrInvalid)
		}
		key := p + "\x00" + id
		if _, ok := seen[key]; ok {
			return ModelMap{}, fmt.Errorf("%w: duplicate model %s/%s", ErrInvalid, p, id)
		}
		seen[key] = struct{}{}
		out.Entries = append(out.Entries, ModelCapability{
			Provider: p,
			ModelID:  id,
			Class:    e.Class,
		})
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		if out.Entries[i].Provider != out.Entries[j].Provider {
			return out.Entries[i].Provider < out.Entries[j].Provider
		}
		return out.Entries[i].ModelID < out.Entries[j].ModelID
	})
	return out, nil
}

// LookupModel returns the capability class for a canonical model, or false.
func LookupModel(m ModelMap, provider, modelID string) (Class, bool) {
	p := strings.ToLower(strings.TrimSpace(provider))
	id := strings.TrimSpace(modelID)
	for _, e := range m.Entries {
		if e.Provider == p && e.ModelID == id {
			return e.Class, true
		}
	}
	return "", false
}
