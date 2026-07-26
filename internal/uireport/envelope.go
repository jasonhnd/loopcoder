package uireport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	SchemaEnvelope = "loopcoder.ui.report.v1"
	SchemaHuman    = "loopcoder.ui.human.v1"
	MaxEnvelope    = 12 << 10
)

// Kind matches the protocol table.
type Kind string

const (
	KindStart       Kind = "start"
	KindStateChange Kind = "state_change"
	KindPeriodic    Kind = "periodic"
	KindAttention   Kind = "attention"
	KindBlocker     Kind = "blocker"
	KindTerminal    Kind = "terminal"
)

// Route is a redacted route snapshot including account/install/window binding.
type Route struct {
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	Effort        string `json:"effort,omitempty"`
	Permission    string `json:"permission,omitempty"`
	AccountRef    string `json:"account_ref,omitempty"`
	InstallRef    string `json:"install_ref,omitempty"`
	WindowKind    string `json:"window_kind,omitempty"`
	ReservationID string `json:"reservation_id,omitempty"`
	RouteReason   string `json:"route_reason,omitempty"`
	// ActualSources is per-dimension proof class (provider_stream|accepted_invocation|
	// auth_binding|install_binding|unknown). Never collapsed; reports must expose it.
	ActualSources *RouteActualSources `json:"actual_sources,omitempty"`
	// ArgvDigest is redacted exact launched argv fingerprint when known.
	ArgvDigest string `json:"argv_digest,omitempty"`
}

// RouteActualSources is honest evidence class per Actual* dimension.
type RouteActualSources struct {
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Permission string `json:"permission,omitempty"`
	Account    string `json:"account,omitempty"`
	Install    string `json:"install,omitempty"`
}

// ResourceState is compact resource view (never secrets).
type ResourceState struct {
	State     string  `json:"state"` // ok|unknown|stale|breach
	CPURate   float64 `json:"cpu_rate,omitempty"`
	RSSBytes  int64   `json:"rss_bytes,omitempty"`
	Processes int     `json:"processes,omitempty"`
}

// NextAction is operator-facing next step.
type NextAction struct {
	Action   string    `json:"action"`
	Deadline time.Time `json:"deadline,omitempty"`
}

// Envelope is the public machine JSON report.
type Envelope struct {
	Schema               string            `json:"schema"`
	EventID              string            `json:"event_id"`
	ProjectID            string            `json:"project_id"`
	RunID                string            `json:"run_id,omitempty"`
	AttemptID            string            `json:"attempt_id"`
	Sequence             int64             `json:"sequence"`
	ReportKind           Kind              `json:"report_kind"`
	Stage                string            `json:"stage"`
	Status               string            `json:"status"`
	ElapsedMS            int64             `json:"elapsed_ms"`
	Liveness             string            `json:"liveness"`
	SemanticProgress     bool              `json:"semantic_progress"`
	DeliveryStage        string            `json:"delivery_stage"`
	LastConcreteEvidence map[string]string `json:"last_concrete_evidence"`
	RequestedRoute       Route             `json:"requested_route"`
	ActualRoute          Route             `json:"actual_route"`
	ResourceState        ResourceState     `json:"resource_state"`
	Attention            []string          `json:"attention"`
	Blocker              *string           `json:"blocker"`
	NextAction           NextAction        `json:"next_action"`
	NextReportAt         time.Time         `json:"next_report_at,omitempty"`
	RecordedAt           time.Time         `json:"recorded_at"`
	PrivacyClass         string            `json:"privacy_class"`
	Redaction            map[string]string `json:"redaction"`
	ContentDigest        string            `json:"content_digest"`
}

// HumanView is the compact human view model (same semantics as envelope).
type HumanView struct {
	Schema           string    `json:"schema"`
	Stage            string    `json:"stage"`
	Elapsed          string    `json:"elapsed"`
	ActualRoute      string    `json:"actual_route"`
	ConcreteEvidence string    `json:"concrete_evidence"`
	Liveness         string    `json:"liveness"`
	Resources        string    `json:"resources"`
	Blocker          string    `json:"blocker"`
	Attention        string    `json:"attention"`
	NextAction       string    `json:"next_action"`
	NextReportAt     time.Time `json:"next_report_at,omitempty"`
	ContentDigest    string    `json:"content_digest"`
}

// Input projects into an envelope.
type Input struct {
	Kind             Kind
	ProjectID        string
	RunID            string
	AttemptID        string
	Sequence         int64
	Stage            string
	Status           string
	Elapsed          time.Duration
	Liveness         string
	SemanticProgress bool
	DeliveryStage    string
	Evidence         map[string]string
	Requested        Route
	Actual           Route
	Resources        ResourceState
	Attention        []string
	Blocker          string
	Next             NextAction
	NextReportAt     time.Time
	RecordedAt       time.Time
}

// Project builds a redacted bounded envelope.
func Project(in Input) (Envelope, error) {
	if in.Kind == "" || in.ProjectID == "" || in.AttemptID == "" {
		return Envelope{}, fmt.Errorf("uireport: missing identity")
	}
	if in.RecordedAt.IsZero() {
		in.RecordedAt = time.Now().UTC()
	}
	if in.Liveness == "" {
		in.Liveness = "unknown"
	}
	if in.Resources.State == "" {
		in.Resources.State = "unknown"
	}
	if in.DeliveryStage == "" {
		in.DeliveryStage = "unknown"
	}
	ev := map[string]string{}
	for k, v := range in.Evidence {
		if forbidden(k) || forbidden(v) {
			continue
		}
		ev[k] = v
	}
	var blocker *string
	if in.Blocker != "" && !forbidden(in.Blocker) {
		b := in.Blocker
		blocker = &b
	}
	env := Envelope{
		Schema:               SchemaEnvelope,
		EventID:              fmt.Sprintf("event_%s_%d", in.AttemptID, in.Sequence),
		ProjectID:            in.ProjectID,
		RunID:                in.RunID,
		AttemptID:            in.AttemptID,
		Sequence:             in.Sequence,
		ReportKind:           in.Kind,
		Stage:                in.Stage,
		Status:               in.Status,
		ElapsedMS:            in.Elapsed.Milliseconds(),
		Liveness:             in.Liveness,
		SemanticProgress:     in.SemanticProgress,
		DeliveryStage:        in.DeliveryStage,
		LastConcreteEvidence: ev,
		RequestedRoute:       in.Requested,
		ActualRoute:          in.Actual,
		ResourceState:        in.Resources,
		Attention:            append([]string(nil), in.Attention...),
		Blocker:              blocker,
		NextAction:           in.Next,
		NextReportAt:         in.NextReportAt,
		RecordedAt:           in.RecordedAt.UTC(),
		PrivacyClass:         "operator",
		Redaction:            map[string]string{"policy": "default_no_secrets_no_paths"},
	}
	env.ContentDigest = digestEnvelope(env)
	b, err := json.Marshal(env)
	if err != nil {
		return Envelope{}, err
	}
	if len(b) > MaxEnvelope {
		return Envelope{}, fmt.Errorf("uireport: envelope exceeds bound")
	}
	return env, nil
}

// Human projects the same semantics into a compact view.
func Human(env Envelope) HumanView {
	route := strings.TrimSpace(env.ActualRoute.Provider + "/" + env.ActualRoute.Model)
	if route == "/" {
		route = "(none)"
	}
	ev := ""
	for k, v := range env.LastConcreteEvidence {
		if ev != "" {
			ev += "; "
		}
		ev += k + "=" + v
	}
	if ev == "" {
		ev = "(none)"
	}
	blocker := ""
	if env.Blocker != nil {
		blocker = *env.Blocker
	}
	att := strings.Join(env.Attention, ", ")
	return HumanView{
		Schema:           SchemaHuman,
		Stage:            env.Stage,
		Elapsed:          fmt.Sprintf("%dms", env.ElapsedMS),
		ActualRoute:      route,
		ConcreteEvidence: ev,
		Liveness:         env.Liveness,
		Resources:        env.ResourceState.State,
		Blocker:          blocker,
		Attention:        att,
		NextAction:       env.NextAction.Action,
		NextReportAt:     env.NextReportAt,
		ContentDigest:    env.ContentDigest,
	}
}

// PrettyText is non-authoritative rendering for terminals.
func PrettyText(h HumanView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "stage=%s elapsed=%s route=%s liveness=%s progress_evidence=%s resources=%s",
		h.Stage, h.Elapsed, h.ActualRoute, h.Liveness, h.ConcreteEvidence, h.Resources)
	if h.Blocker != "" {
		fmt.Fprintf(&b, " blocker=%s", h.Blocker)
	}
	if h.Attention != "" {
		fmt.Fprintf(&b, " attention=%s", h.Attention)
	}
	fmt.Fprintf(&b, " next=%s", h.NextAction)
	if !h.NextReportAt.IsZero() {
		fmt.Fprintf(&b, " next_report_at=%s", h.NextReportAt.UTC().Format(time.RFC3339))
	}
	return b.String()
}

func digestEnvelope(env Envelope) string {
	// Digest semantic fields only (not pretty text).
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%s|%s|%s|%v|%s|%s|%s|%v",
		env.ReportKind, env.Stage, env.Status, env.ElapsedMS, env.Liveness,
		env.DeliveryStage, env.ActualRoute.Model, env.SemanticProgress,
		env.ResourceState.State, env.NextAction.Action, env.AttemptID, env.Sequence)
	if env.Blocker != nil {
		fmt.Fprintf(h, "|b=%s", *env.Blocker)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:32]
}

func forbidden(s string) bool {
	lower := strings.ToLower(s)
	for _, bad := range []string{"sk-", "ghp_", "password", "api_key", "-----begin", "/users/", "bearer "} {
		if strings.Contains(lower, bad) {
			return true
		}
	}
	// Absolute path heuristic
	if strings.HasPrefix(s, "/") && strings.Count(s, "/") >= 2 {
		return true
	}
	return false
}
