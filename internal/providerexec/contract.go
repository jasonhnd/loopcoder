package providerexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SchemaRequest   = "loopcoder.provider.exec.request.v1"
	SchemaOutcome   = "loopcoder.provider.exec.outcome.v1"
	SchemaInventory = "loopcoder.provider.exec.inventory.v1"
)

// FailureClass is a typed refusal/failure category.
type FailureClass string

const (
	FailNone          FailureClass = ""
	FailTimeout       FailureClass = "timeout"
	FailAuth          FailureClass = "auth_refusal"
	FailUnsupported   FailureClass = "unsupported_capability"
	FailRateLimit     FailureClass = "rate_limit"
	FailMalformed     FailureClass = "malformed_output"
	FailCancelled     FailureClass = "cancelled"
	FailProcess       FailureClass = "process_failure"
	FailRouteMismatch FailureClass = "route_mismatch"
	FailIntegrity     FailureClass = "integrity"
)

// Route is the explicit provider/model/effort/permission pin.
type Route struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Effort     string `json:"effort,omitempty"`
	Permission string `json:"permission,omitempty"`
	// NativeDelegation is always false for this minimal contract unless explicitly allowed.
	NativeDelegation bool `json:"native_delegation"`
}

// Digest returns stable requested-route digest.
func (r Route) Digest() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%v", r.Provider, r.Model, r.Effort, r.Permission, r.NativeDelegation)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:24]
}

// Request is an immutable execution request.
type Request struct {
	Schema      string            `json:"schema"`
	RequestID   string            `json:"request_id"`
	ProjectID   string            `json:"project_id"`
	AttemptID   string            `json:"attempt_id"`
	WorkDir     string            `json:"work_dir,omitempty"`
	PromptRef   string            `json:"prompt_ref,omitempty"` // never raw secrets
	Route       Route             `json:"route"`
	Timeout     time.Duration     `json:"timeout"`
	EnvAllow    []string          `json:"env_allow,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	AcceptedAt  time.Time         `json:"accepted_at"`
	RouteDigest string            `json:"route_digest"`
}

// Clone returns a deep copy (immutability helper).
func (r Request) Clone() Request {
	cp := r
	cp.EnvAllow = append([]string(nil), r.EnvAllow...)
	if r.Labels != nil {
		cp.Labels = map[string]string{}
		for k, v := range r.Labels {
			cp.Labels[k] = v
		}
	}
	return cp
}

// Validate checks required fields and forbids native delegation by default.
func (r Request) Validate() error {
	if r.RequestID == "" || r.ProjectID == "" || r.AttemptID == "" {
		return fmt.Errorf("%w: missing identity", ErrInvalid)
	}
	if r.Route.Provider == "" || r.Route.Model == "" {
		return fmt.Errorf("%w: route incomplete", ErrInvalid)
	}
	if r.Route.NativeDelegation {
		return fmt.Errorf("%w: native delegation not supported on minimal contract", ErrUnsupported)
	}
	if r.RouteDigest != "" && r.RouteDigest != r.Route.Digest() {
		return fmt.Errorf("%w: route digest mismatch", ErrRouteMismatch)
	}
	return nil
}

// ProcessEvidence is process-start evidence (no credentials).
type ProcessEvidence struct {
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Command   string    `json:"command_redacted,omitempty"`
	Adapter   string    `json:"adapter"`
	Version   string    `json:"adapter_version"`
}

// UsageEvidence is optional token/cost usage (never secrets).
type UsageEvidence struct {
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
}

// Outcome is the normalized execution result envelope.
type Outcome struct {
	Schema         string          `json:"schema"`
	RequestID      string          `json:"request_id"`
	RequestedRoute Route           `json:"requested_route"`
	ActualRoute    Route           `json:"actual_route"`
	RouteDigest    string          `json:"route_digest"`
	Process        ProcessEvidence `json:"process"`
	Usage          UsageEvidence   `json:"usage"`
	ExitCode       int             `json:"exit_code"`
	Failure        FailureClass    `json:"failure_class,omitempty"`
	Message        string          `json:"message,omitempty"`
	FinishedAt     time.Time       `json:"finished_at"`
	// OutputDigest is a redacted content fingerprint, not body.
	OutputDigest string `json:"output_digest,omitempty"`
}

// Capability declares what an adapter supports.
type Capability struct {
	AdapterID    string   `json:"adapter_id"`
	Version      string   `json:"version"`
	Providers    []string `json:"providers"`
	Models       []string `json:"models,omitempty"`
	Efforts      []string `json:"efforts,omitempty"`
	Permissions  []string `json:"permissions,omitempty"`
	DelegationOK bool     `json:"native_delegation"`
}

// Adapter is the minimal execution port.
type Adapter interface {
	Identity() Capability
	Execute(ctx context.Context, req Request) (Outcome, error)
}

var (
	ErrInvalid        = errors.New("providerexec: invalid request")
	ErrUnsupported    = errors.New("providerexec: unsupported")
	ErrRouteMismatch  = errors.New("providerexec: route mismatch")
	ErrNotImplemented = errors.New("providerexec: not implemented")
)

// NewRequest builds a validated immutable request with digest filled.
func NewRequest(req Request) (Request, error) {
	req.Schema = SchemaRequest
	if req.AcceptedAt.IsZero() {
		req.AcceptedAt = time.Now().UTC()
	}
	req.RouteDigest = req.Route.Digest()
	if err := req.Validate(); err != nil {
		return Request{}, err
	}
	return req.Clone(), nil
}

// FakeAdapter is a deterministic adapter for conformance.
type FakeAdapter struct {
	Cap Capability
	// Behavior overrides for tests.
	Behavior FailureClass
	Delay    time.Duration
	ExitCode int
	mu       sync.Mutex
	last     Request
}

// NewFake creates a fake adapter advertising fixture provider.
func NewFake() *FakeAdapter {
	return &FakeAdapter{
		Cap: Capability{
			AdapterID: "fake", Version: "1",
			Providers: []string{"fixture"}, Models: []string{"m0"},
			Efforts: []string{"low", "medium", "high"}, Permissions: []string{"default"},
		},
	}
}

func (f *FakeAdapter) Identity() Capability { return f.Cap }

func (f *FakeAdapter) Execute(ctx context.Context, req Request) (Outcome, error) {
	req, err := NewRequest(req)
	if err != nil {
		return Outcome{}, err
	}
	if err := supportCheck(f.Cap, req.Route); err != nil {
		return outcomeFail(req, FailUnsupported, err.Error(), ProcessEvidence{
			Adapter: f.Cap.AdapterID, Version: f.Cap.Version,
		}), nil
	}
	f.mu.Lock()
	f.last = req.Clone()
	f.mu.Unlock()

	start := time.Now().UTC()
	proc := ProcessEvidence{
		PID: 1, StartedAt: start,
		Command: "fake://" + req.Route.Provider + "/" + req.Route.Model,
		Adapter: f.Cap.AdapterID, Version: f.Cap.Version,
	}

	if f.Delay > 0 {
		select {
		case <-ctx.Done():
			return outcomeFail(req, FailCancelled, "cancelled", proc), nil
		case <-time.After(f.Delay):
		}
	}
	if err := ctx.Err(); err != nil {
		return outcomeFail(req, FailCancelled, err.Error(), proc), nil
	}
	switch f.Behavior {
	case FailTimeout:
		return outcomeFail(req, FailTimeout, "timeout", proc), nil
	case FailAuth:
		return outcomeFail(req, FailAuth, "auth refused", proc), nil
	case FailRateLimit:
		return outcomeFail(req, FailRateLimit, "rate limited", proc), nil
	case FailMalformed:
		return outcomeFail(req, FailMalformed, "malformed", proc), nil
	case FailProcess:
		return outcomeFail(req, FailProcess, "process failed", proc), nil
	}
	// Actual must match requested exactly.
	actual := req.Route
	if actual.Digest() != req.RouteDigest {
		return outcomeFail(req, FailRouteMismatch, "actual route digest mismatch", proc), nil
	}
	code := f.ExitCode
	out := Outcome{
		Schema: SchemaOutcome, RequestID: req.RequestID,
		RequestedRoute: req.Route, ActualRoute: actual, RouteDigest: req.RouteDigest,
		Process: proc, ExitCode: code, FinishedAt: time.Now().UTC(),
		OutputDigest: "sha256:fake-ok",
		Usage:        UsageEvidence{InputTokens: 1, OutputTokens: 1},
	}
	return out, nil
}

// LastRequest returns the last accepted request (tests).
func (f *FakeAdapter) LastRequest() Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last.Clone()
}

// ReferenceAdapter wraps a command builder selected from existing agent runners
// without launching real providers in unit tests — it validates route and
// constructs redacted process evidence using the same identity as agent.Lookup.
type ReferenceAdapter struct {
	Cap Capability
	// BuildCmd returns a redacted command line for evidence (no secrets).
	BuildCmd func(req Request) (string, error)
	// Launch is optional; if nil, Execute succeeds without a process (dry evidence).
	Launch func(ctx context.Context, req Request, cmd string) (ProcessEvidence, int, error)
}

// NewReferenceFixture builds a reference adapter for the fixture provider.
func NewReferenceFixture() *ReferenceAdapter {
	return &ReferenceAdapter{
		Cap: Capability{
			AdapterID: "agent-fixture-ref", Version: "1",
			Providers: []string{"fixture"}, Models: []string{"m0", "m1"},
			Efforts: []string{"low", "medium", "high"}, Permissions: []string{"default", "readonly"},
		},
		BuildCmd: func(req Request) (string, error) {
			return "fixture-run --model " + req.Route.Model + " --effort " + req.Route.Effort, nil
		},
	}
}

func (r *ReferenceAdapter) Identity() Capability { return r.Cap }

func (r *ReferenceAdapter) Execute(ctx context.Context, req Request) (Outcome, error) {
	req, err := NewRequest(req)
	if err != nil {
		return Outcome{}, err
	}
	if err := supportCheck(r.Cap, req.Route); err != nil {
		return outcomeFail(req, FailUnsupported, err.Error(), ProcessEvidence{
			Adapter: r.Cap.AdapterID, Version: r.Cap.Version,
		}), nil
	}
	cmd, err := r.BuildCmd(req)
	if err != nil {
		return outcomeFail(req, FailProcess, err.Error(), ProcessEvidence{
			Adapter: r.Cap.AdapterID, Version: r.Cap.Version,
		}), nil
	}
	proc := ProcessEvidence{
		StartedAt: time.Now().UTC(), Command: cmd,
		Adapter: r.Cap.AdapterID, Version: r.Cap.Version,
	}
	code := 0
	if r.Launch != nil {
		p, c, err := r.Launch(ctx, req, cmd)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				fc := FailCancelled
				if errors.Is(err, context.DeadlineExceeded) {
					fc = FailTimeout
				}
				return outcomeFail(req, fc, err.Error(), p), nil
			}
			return outcomeFail(req, FailProcess, err.Error(), p), nil
		}
		proc = p
		code = c
	} else {
		proc.PID = 0 // dry-run evidence only
	}
	// integrity: actual equals requested
	if req.Route.Digest() != req.RouteDigest {
		return outcomeFail(req, FailIntegrity, "route integrity", proc), nil
	}
	return Outcome{
		Schema: SchemaOutcome, RequestID: req.RequestID,
		RequestedRoute: req.Route, ActualRoute: req.Route, RouteDigest: req.RouteDigest,
		Process: proc, ExitCode: code, FinishedAt: time.Now().UTC(),
		OutputDigest: "sha256:ref-" + req.RequestID,
	}, nil
}

func supportCheck(cap Capability, route Route) error {
	if !contains(cap.Providers, route.Provider) {
		return fmt.Errorf("%w: provider %s", ErrUnsupported, route.Provider)
	}
	if len(cap.Models) > 0 && !contains(cap.Models, route.Model) {
		return fmt.Errorf("%w: model %s", ErrUnsupported, route.Model)
	}
	if route.Effort != "" && len(cap.Efforts) > 0 && !contains(cap.Efforts, route.Effort) {
		return fmt.Errorf("%w: effort %s", ErrUnsupported, route.Effort)
	}
	if route.Permission != "" && len(cap.Permissions) > 0 && !contains(cap.Permissions, route.Permission) {
		return fmt.Errorf("%w: permission %s", ErrUnsupported, route.Permission)
	}
	if route.NativeDelegation && !cap.DelegationOK {
		return fmt.Errorf("%w: delegation", ErrUnsupported)
	}
	// reject alias-like model names with spaces or uppercase provider tricks
	if strings.Contains(route.Model, " ") {
		return fmt.Errorf("%w: model alias", ErrUnsupported)
	}
	return nil
}

func outcomeFail(req Request, fc FailureClass, msg string, proc ProcessEvidence) Outcome {
	return Outcome{
		Schema: SchemaOutcome, RequestID: req.RequestID,
		RequestedRoute: req.Route, ActualRoute: Route{}, RouteDigest: req.RouteDigest,
		Process: proc, ExitCode: 1, Failure: fc, Message: msg, FinishedAt: time.Now().UTC(),
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// InventoryClass classifies existing agent runners for migration planning.
type InventoryClass string

const (
	ClassReferenceReady   InventoryClass = "reference_ready"
	ClassLaterP4Migration InventoryClass = "later_p4_migration"
	ClassCompatibility    InventoryClass = "compatibility_only"
)

// RunnerInventoryEntry is a non-duplicating inventory of existing agent runners.
type RunnerInventoryEntry struct {
	Provider string         `json:"provider"`
	Class    InventoryClass `json:"class"`
	Note     string         `json:"note"`
}

// ExistingAgentInventory lists current agent runners without duplicating invocation.
func ExistingAgentInventory() []RunnerInventoryEntry {
	entries := []RunnerInventoryEntry{
		{Provider: "codex", Class: ClassReferenceReady, Note: "primary direct-path reference candidate"},
		{Provider: "claude", Class: ClassReferenceReady, Note: "supported; wrap via agent.ClaudeRunner"},
		{Provider: "gemini", Class: ClassLaterP4Migration, Note: "needs catalog/quota consolidation in P4"},
		{Provider: "antigravity", Class: ClassLaterP4Migration, Note: "needs discovery adapter in P4"},
		{Provider: "grok", Class: ClassLaterP4Migration, Note: "needs catalog consolidation in P4"},
		{Provider: "fixture", Class: ClassReferenceReady, Note: "test/fake execution port"},
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Provider < entries[j].Provider })
	return entries
}

// InventoryDoc is the published inventory envelope.
type InventoryDoc struct {
	Schema  string                 `json:"schema"`
	Entries []RunnerInventoryEntry `json:"entries"`
}

// PublishInventory returns the inventory document.
func PublishInventory() InventoryDoc {
	return InventoryDoc{Schema: SchemaInventory, Entries: ExistingAgentInventory()}
}
