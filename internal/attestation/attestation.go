package attestation

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Role string

const (
	RoleWorker    Role = "worker"
	RoleVerifier  Role = "verifier"
	RoleConductor Role = "conductor"
)

type ModelSource string

const (
	ModelSourceParsed       ModelSource = "parsed"
	ModelSourceSelfReported ModelSource = "self-reported"
)

type Permission string

const (
	PermissionReadOnly    Permission = "read-only"
	PermissionWrite       Permission = "write"
	PermissionOrchestrate Permission = "orchestrate"
)

// AttestationRecord is the shared schema emitted for worker, verifier, and
// conductor invocations.
type AttestationRecord struct {
	Role        Role        `json:"role"`
	Provider    string      `json:"provider"`
	Model       string      `json:"model"`
	ModelSource ModelSource `json:"model_source"`
	Effort      string      `json:"effort"`
	Permission  Permission  `json:"permission"`
	Action      string      `json:"action"`
	ExitCode    int         `json:"exit_code"`
	StartedAt   string      `json:"started_at"`
	EndedAt     string      `json:"ended_at"`
	DurationMS  int64       `json:"duration_ms"`
	Usage       Usage       `json:"usage"`
	Verified    bool        `json:"verified"`

	presence attestationPresence
}

type attestationPresence struct {
	Known      bool
	ExitCode   bool
	DurationMS bool
	Usage      bool
	Verified   bool
}

// Usage holds token counts. Providers may report either total-only usage or
// split input/output usage.
type Usage struct {
	InputTokens  *int64 `json:"input_tokens,omitempty"`
	OutputTokens *int64 `json:"output_tokens,omitempty"`
	TotalTokens  *int64 `json:"total_tokens,omitempty"`
}

// ValidationError lists all attestation validation problems found in a record.
type ValidationError struct {
	Problems []string
}

func (e ValidationError) Error() string {
	return "invalid attestation record: " + strings.Join(e.Problems, "; ")
}

// CanonicalJSON returns the machine rendering using the schema's snake_case
// field names and stable struct field order.
func (r AttestationRecord) CanonicalJSON() ([]byte, error) {
	return json.Marshal(r)
}

func (r *AttestationRecord) UnmarshalJSON(data []byte) error {
	type attestationRecord AttestationRecord
	var parsed attestationRecord
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	*r = AttestationRecord(parsed)
	r.presence = attestationPresence{
		Known:      true,
		ExitCode:   fieldPresent(fields, "exit_code"),
		DurationMS: fieldPresent(fields, "duration_ms"),
		Usage:      fieldPresent(fields, "usage"),
		Verified:   fieldPresent(fields, "verified"),
	}
	return nil
}

func (r AttestationRecord) HasDurationMS() bool {
	return !r.presence.Known || r.presence.DurationMS
}

func (r AttestationRecord) HasUsage() bool {
	return !r.presence.Known || r.presence.Usage
}

func (r AttestationRecord) HasVerified() bool {
	return !r.presence.Known || r.presence.Verified
}

func fieldPresent(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	if !ok {
		return false
	}
	return strings.TrimSpace(string(raw)) != "null"
}

// Header renders a stable, greppable one-line attestation header for humans.
func (r AttestationRecord) Header() string {
	return fmt.Sprintf(
		"[attestation] role=%s provider=%s model=%s(%s) effort=%s perm=%s action=%s exit=%d dur=%s tokens=%s verified=%t",
		r.Role,
		r.Provider,
		r.Model,
		r.ModelSource,
		r.Effort,
		r.Permission,
		strconv.Quote(r.Action),
		r.ExitCode,
		formatDuration(r.DurationMS),
		formatUsage(r.Usage),
		r.Verified,
	)
}

func formatDuration(durationMS int64) string {
	return (time.Duration(durationMS) * time.Millisecond).String()
}

func formatUsage(usage Usage) string {
	var parts []string
	if usage.InputTokens != nil && usage.OutputTokens != nil {
		parts = append(parts, fmt.Sprintf("%d/%d", *usage.InputTokens, *usage.OutputTokens))
	}
	if usage.TotalTokens != nil {
		parts = append(parts, strconv.FormatInt(*usage.TotalTokens, 10))
	}
	if len(parts) == 0 {
		return "missing"
	}
	return strings.Join(parts, "|")
}

// Validate checks all required fields and returns every missing or invalid
// field in one error.
func (r AttestationRecord) Validate() error {
	var problems []string

	if strings.TrimSpace(string(r.Role)) == "" {
		problems = append(problems, "role is required")
	} else if !validRole(r.Role) {
		problems = append(problems, fmt.Sprintf("role must be one of worker, verifier, conductor (got %q)", r.Role))
	}
	if strings.TrimSpace(r.Provider) == "" {
		problems = append(problems, "provider is required")
	}
	if strings.TrimSpace(r.Model) == "" {
		problems = append(problems, "model is required")
	}
	if strings.TrimSpace(string(r.ModelSource)) == "" {
		problems = append(problems, "model_source is required")
	} else if !validModelSource(r.ModelSource) {
		problems = append(problems, fmt.Sprintf("model_source must be one of parsed, self-reported (got %q)", r.ModelSource))
	}
	if strings.TrimSpace(string(r.Permission)) == "" {
		problems = append(problems, "permission is required")
	} else if !validPermission(r.Permission) {
		problems = append(problems, fmt.Sprintf("permission must be one of read-only, write, orchestrate (got %q)", r.Permission))
	}
	if strings.TrimSpace(r.Action) == "" {
		problems = append(problems, "action is required")
	}
	if r.presence.Known && !r.presence.ExitCode {
		problems = append(problems, "exit_code is required")
	} else if r.ExitCode < 0 {
		problems = append(problems, "exit_code must be non-negative")
	}
	if strings.TrimSpace(r.StartedAt) == "" {
		problems = append(problems, "started_at is required")
	} else if err := validateTimestamp("started_at", r.StartedAt); err != nil {
		problems = append(problems, err.Error())
	}
	if strings.TrimSpace(r.EndedAt) == "" {
		problems = append(problems, "ended_at is required")
	} else if err := validateTimestamp("ended_at", r.EndedAt); err != nil {
		problems = append(problems, err.Error())
	}
	if r.presence.Known && !r.presence.DurationMS {
		problems = append(problems, "duration_ms is required")
	} else if r.DurationMS < 0 {
		problems = append(problems, "duration_ms must be non-negative")
	}
	if r.presence.Known && !r.presence.Usage {
		problems = append(problems, "usage is required")
	} else {
		problems = append(problems, validateUsage(r.Usage)...)
	}
	if r.presence.Known && !r.presence.Verified {
		problems = append(problems, "verified is required")
	}

	if len(problems) > 0 {
		return ValidationError{Problems: problems}
	}
	return nil
}

func validateTimestamp(field, value string) error {
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return fmt.Errorf("%s must be an RFC3339 timestamp", field)
	}
	return nil
}

func validateUsage(usage Usage) []string {
	var problems []string
	if usage.InputTokens != nil && *usage.InputTokens < 0 {
		problems = append(problems, "usage.input_tokens must be non-negative")
	}
	if usage.OutputTokens != nil && *usage.OutputTokens < 0 {
		problems = append(problems, "usage.output_tokens must be non-negative")
	}
	if usage.TotalTokens != nil && *usage.TotalTokens < 0 {
		problems = append(problems, "usage.total_tokens must be non-negative")
	}

	hasTotal := usage.TotalTokens != nil
	hasInputOutput := usage.InputTokens != nil && usage.OutputTokens != nil
	if !hasTotal && !hasInputOutput {
		problems = append(problems, "usage requires total_tokens or both input_tokens and output_tokens")
	}
	return problems
}

func validRole(role Role) bool {
	switch role {
	case RoleWorker, RoleVerifier, RoleConductor:
		return true
	default:
		return false
	}
}

func validModelSource(source ModelSource) bool {
	switch source {
	case ModelSourceParsed, ModelSourceSelfReported:
		return true
	default:
		return false
	}
}

func validPermission(permission Permission) bool {
	switch permission {
	case PermissionReadOnly, PermissionWrite, PermissionOrchestrate:
		return true
	default:
		return false
	}
}
