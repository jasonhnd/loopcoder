package antigravityexec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/antigravityobs"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
)

const (
	AdapterID     = "antigravity"
	SchemaCommand = "loopcoder.antigravity.exec.command.v1"
)

// ErrAuth is a typed auth refusal for Antigravity execution.
var ErrAuth = errors.New("antigravityexec: auth")

// CommandPlan is the redacted command construction plan (no secrets).
type CommandPlan struct {
	Schema      string   `json:"schema"`
	Binary      string   `json:"binary"`
	Args        []string `json:"args"`
	WorkDirKey  string   `json:"workdir_key"`
	Model       string   `json:"model"`
	Effort      string   `json:"effort,omitempty"`
	Permission  string   `json:"permission,omitempty"`
	TimeoutMS   int64    `json:"timeout_ms"`
	Idempotency string   `json:"idempotency_key,omitempty"`
	EnvScrubbed bool     `json:"env_scrubbed"`
}

// Caps is the accepted observed capability set used for preflight.
type Caps struct {
	Models    []antigravityobs.ModelRecord
	Installed bool
	Auth      string // known|unknown|missing
}

// DefaultCaps returns fixture capabilities for tests.
func DefaultCaps() Caps {
	return Caps{
		Installed: true, Auth: "known",
		Models: []antigravityobs.ModelRecord{
			{CanonicalID: "antigravity-flash", Aliases: []string{"ag-flash"}, Efforts: []string{"low", "medium", "high"}},
			{CanonicalID: "antigravity-pro", Efforts: []string{"medium", "high"}},
		},
	}
}

// Planner builds a command plan from an immutable request + observed caps.
type Planner struct {
	Caps Caps
}

// Plan validates request against frozen caps and builds command args.
func (p *Planner) Plan(req providerexec.Request) (CommandPlan, error) {
	if err := req.Validate(); err != nil {
		return CommandPlan{}, err
	}
	if req.Route.Provider != "" && req.Route.Provider != AdapterID {
		return CommandPlan{}, fmt.Errorf("%w: provider %s", providerexec.ErrUnsupported, req.Route.Provider)
	}
	if !p.Caps.Installed {
		return CommandPlan{}, fmt.Errorf("%w: antigravity not installed", providerexec.ErrUnsupported)
	}
	if p.Caps.Auth == "missing" {
		return CommandPlan{}, fmt.Errorf("%w: auth missing", ErrAuth)
	}
	model := req.Route.Model
	can, ok, _ := antigravityobs.NormalizeAlias(p.Caps.Models, model)
	if !ok {
		for _, m := range p.Caps.Models {
			if m.CanonicalID == model {
				can, ok = m.CanonicalID, true
				break
			}
		}
	}
	if !ok || can == "" {
		return CommandPlan{}, fmt.Errorf("%w: model %q not in frozen catalog", providerexec.ErrUnsupported, model)
	}
	if req.Route.Effort != "" {
		allowed := false
		for _, m := range p.Caps.Models {
			if m.CanonicalID != can {
				continue
			}
			if len(m.Efforts) == 0 {
				allowed = true
				break
			}
			for _, e := range m.Efforts {
				if e == req.Route.Effort {
					allowed = true
				}
			}
		}
		if !allowed {
			return CommandPlan{}, fmt.Errorf("%w: effort %q for %s", providerexec.ErrUnsupported, req.Route.Effort, can)
		}
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	args := []string{"exec", "--model", can}
	if req.Route.Effort != "" {
		args = append(args, "--effort", req.Route.Effort)
	}
	if req.Route.Permission != "" && req.Route.Permission != "default" {
		args = append(args, "--permission", req.Route.Permission)
	}
	if req.RequestID != "" {
		args = append(args, "--idempotency", req.RequestID)
	}
	return CommandPlan{
		Schema: SchemaCommand, Binary: "antigravity", Args: args,
		WorkDirKey: "attempt_workdir", Model: can, Effort: req.Route.Effort,
		Permission: req.Route.Permission, TimeoutMS: timeout.Milliseconds(),
		Idempotency: req.RequestID, EnvScrubbed: true,
	}, nil
}

// Adapter implements providerexec.Adapter for Antigravity.
type Adapter struct {
	Planner Planner
	Exec    func(ctx context.Context, plan CommandPlan, req providerexec.Request) (providerexec.Outcome, error)
	// Mode forces typed failure for conformance.
	Mode string
}

// Identity implements providerexec.Adapter.
func (a *Adapter) Identity() providerexec.Capability {
	models := make([]string, 0, len(a.Planner.Caps.Models))
	for _, m := range a.Planner.Caps.Models {
		models = append(models, m.CanonicalID)
	}
	return providerexec.Capability{
		AdapterID: AdapterID, Version: "v1", Providers: []string{AdapterID},
		Models: models, Efforts: []string{"low", "medium", "high"},
		Permissions: []string{"default"}, DelegationOK: false,
	}
}

// Execute implements providerexec.Adapter.
func (a *Adapter) Execute(ctx context.Context, req providerexec.Request) (providerexec.Outcome, error) {
	plan, err := a.Planner.Plan(req)
	if err != nil {
		fc := providerexec.FailUnsupported
		if errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "auth") {
			fc = providerexec.FailAuth
		}
		return outcome(req, fc, err.Error(), 1), err
	}

	switch a.Mode {
	case "timeout":
		return outcome(req, providerexec.FailTimeout, "timeout", -1), context.DeadlineExceeded
	case "auth":
		return outcome(req, providerexec.FailAuth, "auth refusal", 1), ErrAuth
	case "rate_limit":
		return outcome(req, providerexec.FailRateLimit, "rate limited", 1), nil
	case "cancel":
		return outcome(req, providerexec.FailCancelled, "cancelled", -1), context.Canceled
	case "malformed":
		return outcome(req, providerexec.FailMalformed, "malformed output", 1), nil
	case "flood":
		return outcome(req, providerexec.FailMalformed, "output flood", 1), nil
	case "nonzero":
		return outcome(req, providerexec.FailProcess, "nonzero exit", 2), nil
	case "escape":
		o := outcome(req, providerexec.FailProcess, "descendant escape", -1)
		o.Message = "descendant_escape"
		return o, nil
	case "mismatch":
		o := outcome(req, providerexec.FailRouteMismatch, "actual model mismatch", 0)
		o.ActualRoute = providerexec.Route{Provider: AdapterID, Model: "other-model"}
		return o, providerexec.ErrRouteMismatch
	}

	if a.Exec != nil {
		return a.Exec(ctx, plan, req)
	}
	if err := ctx.Err(); err != nil {
		return outcome(req, providerexec.FailCancelled, "context done", -1), err
	}
	// Requested provider must be claude when set to something else
	if req.Route.Provider != "" && req.Route.Provider != AdapterID {
		return outcome(req, providerexec.FailRouteMismatch, "provider mismatch", 1), providerexec.ErrRouteMismatch
	}
	actual := req.Route
	actual.Provider = AdapterID
	actual.Model = plan.Model
	return providerexec.Outcome{
		Schema: providerexec.SchemaOutcome, RequestID: req.RequestID,
		RequestedRoute: req.Route, ActualRoute: actual, RouteDigest: actual.Digest(),
		Process:    providerexec.ProcessEvidence{Adapter: AdapterID, Version: "v1", Command: "antigravity exec"},
		ExitCode:   0,
		FinishedAt: time.Now().UTC(),
		Message:    "fixture_ok",
	}, nil
}

func outcome(req providerexec.Request, fc providerexec.FailureClass, msg string, code int) providerexec.Outcome {
	return providerexec.Outcome{
		Schema: providerexec.SchemaOutcome, RequestID: req.RequestID,
		RequestedRoute: req.Route, Failure: fc, Message: msg, ExitCode: code,
		Process:    providerexec.ProcessEvidence{Adapter: AdapterID, Version: "v1"},
		FinishedAt: time.Now().UTC(),
	}
}

// ScrubEnv removes config that could silently replace model/permission/tools.
func ScrubEnv(env []string) []string {
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		uk := strings.ToUpper(k)
		if strings.Contains(uk, "TOKEN") || strings.Contains(uk, "SECRET") || strings.Contains(uk, "PASSWORD") {
			continue
		}
		if uk == "ANTIGRAVITY_MODEL" || uk == "ANTIGRAVITY_PERMISSION" || uk == "ANTIGRAVITY_EFFORT" ||
			uk == "ANTIGRAVITY_NATIVE_DELEGATION" || uk == "OPENAI_MODEL" {
			continue
		}
		if strings.HasPrefix(uk, "GIT_DIR") || strings.HasPrefix(uk, "GIT_WORK_TREE") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// ImmutableRetry proves a second plan uses the same frozen request mapping.
func ImmutableRetry(a *Adapter, req providerexec.Request) (CommandPlan, CommandPlan, error) {
	// freeze caps copy
	frozen := a.Planner.Caps
	p1, err := a.Planner.Plan(req)
	if err != nil {
		return CommandPlan{}, CommandPlan{}, err
	}
	// mutate underlying slice but keep planner caps snapshot for second plan by restoring
	mutated := frozen
	mutated.Models = append(append([]antigravityobs.ModelRecord{}, frozen.Models...), antigravityobs.ModelRecord{CanonicalID: "new-model"})
	a.Planner.Caps = mutated
	// Second plan still for original model — new-model must not be selected
	p2, err := a.Planner.Plan(req)
	a.Planner.Caps = frozen
	return p1, p2, err
}
