package codexbar

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/obsplan"
)

const (
	SchemaBridge      = "loopcoder.codexbar.bridge.v1"
	SchemaObservation = "loopcoder.codexbar.observation.v1"
	SourceID          = "codexbar"
	// AuthorityRank is lower than official CLI (80) and API (90).
	AuthorityRank = 30
	SafetyRank    = 70
	MaxOutputB    = 64 << 10
)

// Status of the optional bridge surface.
type Status string

const (
	StatusAbsent      Status = "absent"
	StatusAvailable   Status = "available"
	StatusBroken      Status = "broken"
	StatusUnsupported Status = "unsupported_version"
	StatusTimeout     Status = "timeout"
	StatusMalformed   Status = "malformed"
	StatusPartial     Status = "partial"
)

var (
	ErrAbsent    = errors.New("codexbar: absent")
	ErrMalformed = errors.New("codexbar: malformed")
	ErrTimeout   = errors.New("codexbar: timeout")
	ErrTooLarge  = errors.New("codexbar: output too large")
)

// Descriptor describes the optional source for source plans.
type Descriptor struct {
	Schema    string             `json:"schema"`
	SourceID  string             `json:"source_id"`
	Kind      obsplan.SourceKind `json:"kind"`
	Authority int                `json:"authority"`
	Safety    int                `json:"safety"`
	Optional  bool               `json:"optional"`
	Version   string             `json:"version,omitempty"`
}

// DefaultDescriptor returns the reviewed optional bridge descriptor.
func DefaultDescriptor() Descriptor {
	return Descriptor{
		Schema: SchemaBridge, SourceID: SourceID, Kind: obsplan.SourceBridge,
		Authority: AuthorityRank, Safety: SafetyRank, Optional: true,
	}
}

// Observation is a redacted bridge payload (no credentials).
type Observation struct {
	Schema     string            `json:"schema"`
	SourceID   string            `json:"source_id"`
	Version    string            `json:"version,omitempty"`
	Status     Status            `json:"status"`
	Provider   string            `json:"provider,omitempty"`    // codex|claude|...
	AccountRef string            `json:"account_ref,omitempty"` // redacted
	WindowKind string            `json:"window_kind,omitempty"`
	Facts      map[string]string `json:"facts,omitempty"`
	Confidence string            `json:"confidence"`
	Freshness  string            `json:"freshness"`
	CapturedAt time.Time         `json:"captured_at"`
	Diagnostic string            `json:"diagnostic,omitempty"`
	// Strategy documents how this evidence may be used.
	Strategy string `json:"strategy"` // supplement_only
}

// RawFixture is structured public output from a CodexBar-compatible surface.
type RawFixture struct {
	Version    string            `json:"version"`
	Provider   string            `json:"provider"`
	AccountRef string            `json:"account_ref,omitempty"`
	WindowKind string            `json:"window_kind,omitempty"`
	Facts      map[string]string `json:"facts,omitempty"`
}

// ProbeInputs inject fixture probe results (no real process required).
type ProbeInputs struct {
	// Present is whether the optional binary/surface exists.
	Present bool
	// Version of the bridge surface.
	Version string
	// Timeout / Malformed force typed failures.
	Timeout   bool
	Malformed bool
	// Output is JSON raw fixture bytes when healthy.
	Output []byte
	Now    func() time.Time
}

// Probe discovers and optionally parses the bridge surface.
func Probe(in ProbeInputs) (Observation, error) {
	now := time.Now().UTC()
	if in.Now != nil {
		now = in.Now().UTC()
	}
	base := Observation{
		Schema: SchemaObservation, SourceID: SourceID,
		CapturedAt: now, Strategy: "supplement_only",
		Confidence: "low", Freshness: "unknown",
	}
	if !in.Present {
		base.Status = StatusAbsent
		base.Diagnostic = "bridge_not_installed"
		// Absence is not an error for eligibility.
		return base, nil
	}
	if in.Timeout {
		base.Status = StatusTimeout
		base.Diagnostic = "probe_timeout"
		return base, ErrTimeout
	}
	if in.Malformed || len(in.Output) == 0 {
		base.Status = StatusMalformed
		base.Diagnostic = "empty_or_malformed"
		return base, ErrMalformed
	}
	if len(in.Output) > MaxOutputB {
		base.Status = StatusBroken
		base.Diagnostic = "output_too_large"
		return base, ErrTooLarge
	}
	if in.Version != "" && !supportedVersion(in.Version) {
		base.Status = StatusUnsupported
		base.Version = in.Version
		base.Diagnostic = "unsupported_version"
		return base, nil
	}
	var raw RawFixture
	if err := json.Unmarshal(in.Output, &raw); err != nil {
		base.Status = StatusMalformed
		base.Diagnostic = "json_parse"
		return base, ErrMalformed
	}
	if raw.Version != "" && !supportedVersion(raw.Version) {
		base.Status = StatusUnsupported
		base.Version = raw.Version
		base.Diagnostic = "unsupported_version"
		return base, nil
	}
	base.Version = firstNonEmpty(raw.Version, in.Version)
	base.Provider = strings.ToLower(strings.TrimSpace(raw.Provider))
	base.AccountRef = redact(raw.AccountRef)
	base.WindowKind = raw.WindowKind
	base.Facts = scrubFacts(raw.Facts)
	base.Confidence = "medium"
	base.Freshness = "fresh"
	if len(base.Facts) == 0 {
		base.Status = StatusPartial
		base.Diagnostic = "no_facts"
	} else {
		base.Status = StatusAvailable
	}
	return base, nil
}

// MergeWithOfficial applies deterministic conflict rules:
// official fresher/higher-authority facts win; bridge never silently overrides.
// Returns merged facts and conflict notes.
func MergeWithOfficial(official map[string]string, officialFresh bool, bridge Observation) (merged map[string]string, conflicts []string) {
	merged = map[string]string{}
	for k, v := range official {
		merged[k] = v
	}
	if bridge.Status != StatusAvailable && bridge.Status != StatusPartial {
		return merged, nil
	}
	for k, v := range bridge.Facts {
		if ov, ok := official[k]; ok {
			if officialFresh {
				conflicts = append(conflicts, fmt.Sprintf("bridge_suppressed:%s official=%s bridge=%s", k, ov, v))
				continue
			}
			// official stale: take bridge but flag
			conflicts = append(conflicts, fmt.Sprintf("bridge_fills_stale:%s", k))
			merged[k] = v
			continue
		}
		// official missing: supplement
		merged[k] = v
	}
	return merged, conflicts
}

// AsSourceStep builds an optional obsplan step for plans.
func AsSourceStep() obsplan.SourceStep {
	return obsplan.SourceStep{
		Name: SourceID, Kind: obsplan.SourceBridge,
		Authority: AuthorityRank, Safety: SafetyRank,
		Bounds: obsplan.Bounds{
			Timeout: 3 * time.Second, MaxOutputB: MaxOutputB,
			AllowNetwork: false, AllowRedirects: false,
		},
		Optional: true,
	}
}

// ScrubEnv removes secrets for any future process probe.
func ScrubEnv(env []string) []string {
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		uk := strings.ToUpper(k)
		if strings.Contains(uk, "TOKEN") || strings.Contains(uk, "SECRET") ||
			strings.Contains(uk, "PASSWORD") || strings.Contains(uk, "CREDENTIAL") ||
			strings.Contains(uk, "AUTH") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func supportedVersion(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	// accept v1.x fixture family only
	return strings.HasPrefix(v, "1.") || v == "1" || strings.HasPrefix(v, "v1")
}

func scrubFacts(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range m {
		if looksSecret(k) || looksSecret(v) {
			continue
		}
		out[k] = v
	}
	return out
}

func redact(s string) string {
	if looksSecret(s) || strings.Contains(s, "@") {
		return "redacted"
	}
	return s
}

func looksSecret(s string) bool {
	ls := strings.ToLower(s)
	return strings.HasPrefix(ls, "ghp_") || strings.HasPrefix(ls, "sk-") ||
		strings.Contains(ls, "token=") || strings.Contains(ls, "password=")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
