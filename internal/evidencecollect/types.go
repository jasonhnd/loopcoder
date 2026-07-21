package evidencecollect

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const SchemaVersion = "loopcoder.evidence_collect.v1"

// Type classifies the observation.
type Type string

const (
	TypeProcessState   Type = "process_state"
	TypeOutputMovement Type = "output_movement"
	TypeResourceSample Type = "resource_sample"
	TypeGitCommit      Type = "git_commit"
	TypeGitWorktree    Type = "git_worktree"
	TypeGitHubDelivery Type = "github_delivery"
	TypeGitHubCheck    Type = "github_check"
	TypeOperatorAction Type = "operator_action"
	// TypeProviderProse is accepted only as content evidence, never lifecycle.
	TypeProviderProse Type = "provider_prose"
)

// Source is who produced the observation.
type Source string

const (
	SourceRuntime  Source = "runtime"
	SourceGit      Source = "git"
	SourceGitHub   Source = "github"
	SourceOperator Source = "operator"
	SourceProvider Source = "provider"
	SourceResource Source = "resource"
)

// Confidence grades observation quality.
type Confidence string

const (
	ConfidenceFull    Confidence = "full"
	ConfidencePartial Confidence = "partial"
	ConfidenceNone    Confidence = "none"
)

// PrivacyClass controls redaction.
type PrivacyClass string

const (
	PrivacyPublic   PrivacyClass = "public"
	PrivacyInternal PrivacyClass = "internal"
	PrivacySecret   PrivacyClass = "secret" // must not persist body
)

// Significance separates heartbeat from concrete progress.
type Significance string

const (
	SigHeartbeat  Significance = "heartbeat"
	SigProgress   Significance = "progress"
	SigTransition Significance = "transition"
)

var (
	ErrRejected             = errors.New("evidencecollect: rejected")
	ErrProviderLifecycle    = errors.New("evidencecollect: provider prose cannot set lifecycle")
	ErrMissingFields        = errors.New("evidencecollect: missing required fields")
	ErrSecretNotPersistable = errors.New("evidencecollect: secret privacy class not persistable")
)

// Observation is a raw collector input before accept/dedup.
type Observation struct {
	Type       Type
	Source     Source
	Subject    string // attempt/project/check id — no secrets
	Confidence Confidence
	ObservedAt time.Time
	// CausalIdentity links parent evidence (optional).
	CausalIdentity string
	Privacy        PrivacyClass
	Significance   Significance
	// LifecycleAuthority is true only for trusted sources that may set process/
	// delivery/verification/terminal state.
	LifecycleAuthority bool
	// Fields are redacted key/value (no argv/secrets).
	Fields map[string]string
	// Excerpt is optional bounded content (redacted before accept).
	Excerpt string
}

// Event is an accepted, digests-stable, deduplicated observation.
type Event struct {
	SchemaVersion  string       `json:"schema_version"`
	Type           Type         `json:"type"`
	Source         Source       `json:"source"`
	Subject        string       `json:"subject"`
	Confidence     Confidence   `json:"confidence"`
	ObservedAt     time.Time    `json:"observed_at"`
	RecordedAt     time.Time    `json:"recorded_at"`
	Digest         string       `json:"digest"`
	CausalIdentity string       `json:"causal_identity,omitempty"`
	Privacy        PrivacyClass `json:"privacy"`
	Significance   Significance `json:"significance"`
	// IsProgress is true only for concrete progress (never heartbeat alone).
	IsProgress bool `json:"is_progress"`
	// IsHeartbeat is true for liveness samples.
	IsHeartbeat bool              `json:"is_heartbeat"`
	Fields      map[string]string `json:"fields,omitempty"`
	// Excerpt is redacted and truncated.
	Excerpt string `json:"excerpt,omitempty"`
}

// DigestOf computes a stable content digest for dedup.
func DigestOf(o Observation) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%v",
		o.Type, o.Source, o.Subject, o.Significance, o.Confidence, o.CausalIdentity, o.LifecycleAuthority)
	// Sorted fields
	keys := make([]string, 0, len(o.Fields))
	for k := range o.Fields {
		keys = append(keys, k)
	}
	// simple insertion sort
	for i := 1; i < len(keys); i++ {
		j := i
		for j > 0 && keys[j-1] > keys[j] {
			keys[j-1], keys[j] = keys[j], keys[j-1]
			j--
		}
	}
	for _, k := range keys {
		fmt.Fprintf(h, "|%s=%s", k, o.Fields[k])
	}
	// Excerpt not in digest for heartbeat; include for progress content movement.
	if o.Significance == SigProgress || o.Significance == SigTransition {
		fmt.Fprintf(h, "|ex=%s", o.Excerpt)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// RedactExcerpt strips secret-shaped tokens and truncates.
func RedactExcerpt(s string, max int) string {
	if max <= 0 {
		max = 256
	}
	lower := strings.ToLower(s)
	for _, bad := range []string{"sk-", "ghp_", "password=", "api_key", "-----begin", "bearer "} {
		if strings.Contains(lower, bad) {
			return "[redacted]"
		}
	}
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) > max {
		return s[:max]
	}
	return s
}
