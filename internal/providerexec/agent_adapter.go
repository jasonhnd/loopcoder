package providerexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
)

// AgentAdapter executes via agent.Lookup / agent.Runner — production path.
// FakeAdapter must never be used as a silent production default.
type AgentAdapter struct {
	// Lookup overrides agent.Lookup in tests; production leaves nil.
	Lookup func(provider string) (agent.Runner, error)
	// Cap is optional advertised capability; empty Providers means accept any looked-up runner.
	Cap Capability
}

// NewAgentAdapter returns the production provider execution adapter.
func NewAgentAdapter() *AgentAdapter {
	return &AgentAdapter{
		Cap: Capability{
			AdapterID: "agent-runner", Version: "1",
			// Empty model lists: support is enforced by agent.Lookup + runner.
			Permissions: []string{"default", "readonly", "bounded_write", "read_only"},
		},
	}
}

func (a *AgentAdapter) Identity() Capability {
	if a == nil {
		return Capability{AdapterID: "agent-runner", Version: "1"}
	}
	return a.Cap
}

// Execute runs the real agent.Runner for the requested provider.
// ActualRoute is built only from runner-affirmed Result fields, never a silent
// request copy. Requested vs actual are compared fail-closed when identity is
// required (account/install/provider/model/effort/permission).
func (a *AgentAdapter) Execute(ctx context.Context, req Request) (Outcome, error) {
	req, err := NewRequest(req)
	if err != nil {
		return Outcome{}, err
	}
	lookup := a.Lookup
	if lookup == nil {
		lookup = agent.Lookup
	}
	prov := strings.TrimSpace(req.Route.Provider)
	runner, lerr := lookup(prov)
	if lerr != nil {
		return outcomeFail(req, FailUnsupported, lerr.Error(), ProcessEvidence{
			Adapter: a.Identity().AdapterID, Version: a.Identity().Version,
		}), nil
	}

	workDir := strings.TrimSpace(req.WorkDir)
	if workDir == "" {
		return outcomeFail(req, FailMalformed, "work_dir required", ProcessEvidence{
			Adapter: a.Identity().AdapterID, Version: a.Identity().Version,
		}), nil
	}
	logPath := filepath.Join(workDir, ".loopcoder-provider-exec.log")
	prompt := strings.TrimSpace(req.PromptRef)
	if prompt == "" {
		prompt = "loopcoder direct provider execution attempt=" + req.AttemptID
	}
	perm := strings.TrimSpace(req.Route.Permission)
	readOnly := perm == "readonly" || perm == "read_only"
	inv := agent.Invocation{
		WorktreePath:      workDir,
		Prompt:            prompt,
		Model:             req.Route.Model,
		Effort:            req.Route.Effort,
		Permission:        perm,
		AccountRef:        req.Route.AccountRef,
		InstallRef:        req.Route.InstallRef,
		WindowKind:        req.Route.WindowKind,
		ReservationID:     req.Route.ReservationID,
		ReadOnly:          readOnly,
		BoundedWrite:      !readOnly,
		DisableDelegation: true,
		Role:              "nested-bounded-write",
		LogPath:           logPath,
		RunID:             req.AttemptID,
		ProviderKey:       req.RequestID,
	}
	if readOnly {
		inv.Role = "nested-read-only"
		inv.BoundedWrite = false
	}
	if req.Timeout > 0 {
		inv.HardCap = req.Timeout
	}
	// Wire spawn authority: publish real PID/PGID/birth to caller (directrun lease).
	if req.OnProviderStart != nil {
		cb := req.OnProviderStart
		inv.OnProviderStart = func(pp agent.ProviderProcess) error {
			return cb(ProcessStart{
				PID: pp.PID, PGID: pp.PGID,
				ProcessBirthIdentity: pp.ProcessBirthIdentity,
				ExecutableIdentity:   pp.ExecutableIdentity,
				ObservedAt:           pp.ObservedAt,
			})
		}
	}

	start := time.Now().UTC()
	res, rerr := runner.Run(ctx, inv)
	proc := ProcessEvidence{
		StartedAt: start,
		Command:   "agent://" + prov + "/" + req.Route.Model,
		Adapter:   a.Identity().AdapterID,
		Version:   firstNonEmpty(res.AdapterVersion, a.Identity().Version),
	}
	if res.ExecutableIdentity != "" {
		proc.Command = res.ExecutableIdentity
	}

	// Partial evidence only (may be retained on failure for audit). Never treat
	// as accepted success Actual until full success path below.
	partial := Route{
		Provider:   firstNonEmpty(res.ActualProvider, ""),
		Model:      firstNonEmpty(res.ActualModel, ""),
		Effort:     firstNonEmpty(res.ActualEffort, ""),
		Permission: firstNonEmpty(res.ActualPermission, ""),
		AccountRef: strings.TrimSpace(res.ActualAccountRef),
		InstallRef: strings.TrimSpace(res.ActualInstallRef),
	}
	sources := ActualSources{
		Model:      res.ActualSourceModel,
		Effort:     res.ActualSourceEffort,
		Permission: res.ActualSourcePermission,
		Account:    res.ActualSourceAccount,
		Install:    res.ActualSourceInstall,
	}
	argvDig := strings.TrimSpace(res.ArgvDigest)

	// 1) Typed runner FailureClass and errors FIRST — never rewrite model_unavailable
	//    / auth / rate-limit into route_mismatch by running identity acceptance early.
	code := res.ExitCode
	if fc := mapRunnerFailureClass(res.FailureClass); fc != FailNone {
		out := outcomeFail(req, fc, firstNonEmpty(rerrMsg(rerr), res.Summary, string(fc)), proc)
		out.ActualRoute = partial
		out.ActualSources = sources
		out.ArgvDigest = argvDig
		out.ExitCode = firstNonZero(code, 1)
		return out, nil
	}
	if rerr != nil {
		fc := FailProcess
		if ctx.Err() != nil {
			if ctx.Err() == context.DeadlineExceeded {
				fc = FailTimeout
			} else {
				fc = FailCancelled
			}
		}
		// Map common typed error substrings when FailureClass was empty.
		if mapped := mapRunnerError(rerr); mapped != FailNone {
			fc = mapped
		}
		out := outcomeFail(req, fc, rerr.Error(), proc)
		out.ActualRoute = partial
		out.ActualSources = sources
		out.ArgvDigest = argvDig
		out.ExitCode = firstNonZero(code, 1)
		return out, nil
	}
	if code != 0 {
		out := outcomeFail(req, FailProcess, firstNonEmpty(res.Summary, fmt.Sprintf("exit %d", code)), proc)
		out.ActualRoute = partial
		out.ActualSources = sources
		out.ArgvDigest = argvDig
		out.ExitCode = code
		return out, nil
	}

	// 2) Success path only: accept Actual from independently verified fields.
	actual := partial
	if actual.Provider == "" {
		actual.Provider = prov
	}
	// Fail-closed: required Actual dimensions must have allowed sources.
	if srcErr := requireActualSources(req.Route, actual, sources); srcErr != "" {
		out := outcomeFail(req, FailRouteMismatch, srcErr, proc)
		out.ActualRoute = actual
		out.ActualSources = sources
		out.ArgvDigest = argvDig
		out.ExitCode = 1
		return out, nil
	}
	if mismatch := routeIdentityMismatch(req.Route, actual); mismatch != "" {
		out := outcomeFail(req, FailRouteMismatch, mismatch, proc)
		out.ActualRoute = actual
		out.ActualSources = sources
		out.ArgvDigest = argvDig
		out.ExitCode = 1
		return out, nil
	}

	// Capacity bindings may attach after verified account/install match.
	if strings.TrimSpace(req.Route.AccountRef) != "" &&
		strings.TrimSpace(actual.AccountRef) != "" &&
		strings.EqualFold(strings.TrimSpace(req.Route.AccountRef), strings.TrimSpace(actual.AccountRef)) {
		if strings.TrimSpace(req.Route.InstallRef) == "" ||
			strings.EqualFold(strings.TrimSpace(req.Route.InstallRef), strings.TrimSpace(actual.InstallRef)) {
			actual.WindowKind = req.Route.WindowKind
			actual.ReservationID = req.Route.ReservationID
			actual.RouteReason = req.Route.RouteReason
		}
	}

	outDig := contentDigest(res.Summary)
	if outDig == "" {
		out := Outcome{
			Schema: SchemaOutcome, RequestID: req.RequestID,
			RequestedRoute: req.Route, ActualRoute: actual, ActualSources: sources,
			ArgvDigest: argvDig, RouteDigest: req.RouteDigest,
			Process: proc, ExitCode: 1, FinishedAt: time.Now().UTC(),
			Usage: usageFromAgent(res), Failure: FailProcess,
			Message: "empty provider output (no redacted summary/artifact content for OutputDigest)",
		}
		return out, fmt.Errorf("providerexec: empty output digest")
	}
	return Outcome{
		Schema: SchemaOutcome, RequestID: req.RequestID,
		RequestedRoute: req.Route, ActualRoute: actual, ActualSources: sources,
		ArgvDigest: argvDig, RouteDigest: req.RouteDigest,
		Process: proc, ExitCode: 0, FinishedAt: time.Now().UTC(),
		Usage: usageFromAgent(res), OutputDigest: outDig,
	}, nil
}

func rerrMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// mapRunnerFailureClass maps agent.Result.FailureClass to providerexec FailureClass.
func mapRunnerFailureClass(fc string) FailureClass {
	switch strings.ToLower(strings.TrimSpace(fc)) {
	case "", "none":
		return FailNone
	case "model_unavailable":
		return FailModelUnavailable
	case "auth_refusal", "auth":
		return FailAuth
	case "rate_limit":
		return FailRateLimit
	case "route_mismatch":
		return FailRouteMismatch
	case "timeout":
		return FailTimeout
	case "cancelled":
		return FailCancelled
	case "malformed_output", "malformed":
		return FailMalformed
	case "unsupported", "unsupported_capability":
		return FailUnsupported
	default:
		return FailNone // unknown class: fall through to error/exit handling
	}
}

func mapRunnerError(err error) FailureClass {
	if err == nil {
		return FailNone
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "model_unavailable") || strings.Contains(msg, "invalid model selection"):
		return FailModelUnavailable
	case strings.Contains(msg, "auth_refusal") || strings.Contains(msg, "account mismatch") || strings.Contains(msg, "openai_api_key"):
		return FailAuth
	case strings.Contains(msg, "rate limit") || strings.Contains(msg, "rate_limit"):
		return FailRateLimit
	case strings.Contains(msg, "transport") || strings.Contains(msg, "terminal result frame"):
		return FailMalformed
	default:
		return FailNone
	}
}

// requireActualSources fail-closes when a requested dimension is affirmed without
// an allowed source (provider_stream | accepted_invocation | auth_binding | install_binding).
func requireActualSources(requested, actual Route, src ActualSources) string {
	allowed := func(s string) bool {
		switch strings.TrimSpace(s) {
		case "provider_stream", "accepted_invocation", "auth_binding", "install_binding":
			return true
		default:
			return false
		}
	}
	check := func(name, want, got, source string) string {
		if strings.TrimSpace(want) == "" {
			return ""
		}
		if strings.TrimSpace(got) == "" {
			return name + ": requested " + want + " but no actual (source=" + source + ")"
		}
		if !allowed(source) {
			return name + ": actual present but source " + firstNonEmpty(source, "empty") + " not allowed"
		}
		return ""
	}
	if m := check("model", requested.Model, actual.Model, src.Model); m != "" {
		return m
	}
	if m := check("effort", requested.Effort, actual.Effort, src.Effort); m != "" {
		return m
	}
	if m := check("permission", requested.Permission, actual.Permission, src.Permission); m != "" {
		return m
	}
	if m := check("account_ref", requested.AccountRef, actual.AccountRef, src.Account); m != "" {
		return m
	}
	if m := check("install_ref", requested.InstallRef, actual.InstallRef, src.Install); m != "" {
		return m
	}
	return ""
}

// FailInvalidWorkDir keeps failure class typed without expanding FailureClass enum.
func FailInvalidWorkDir(req Request) FailureClass {
	_ = req
	return FailMalformed
}

func routeIdentityMismatch(requested, actual Route) string {
	check := func(name, want, got string) string {
		want = strings.TrimSpace(want)
		got = strings.TrimSpace(got)
		if want == "" {
			return ""
		}
		if got == "" {
			return name + ": requested " + want + " but runner did not affirm"
		}
		if !strings.EqualFold(want, got) {
			return name + ": requested " + want + " actual " + got
		}
		return ""
	}
	if m := check("provider", requested.Provider, actual.Provider); m != "" {
		return m
	}
	if m := check("model", requested.Model, actual.Model); m != "" {
		return m
	}
	if m := check("effort", requested.Effort, actual.Effort); m != "" {
		return m
	}
	if m := check("permission", requested.Permission, actual.Permission); m != "" {
		return m
	}
	if m := check("account_ref", requested.AccountRef, actual.AccountRef); m != "" {
		return m
	}
	if m := check("install_ref", requested.InstallRef, actual.InstallRef); m != "" {
		return m
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstNonZero(v, def int) int {
	if v != 0 {
		return v
	}
	return def
}

func usageFromAgent(res agent.Result) UsageEvidence {
	var u UsageEvidence
	if res.Usage.InputTokens != nil {
		u.InputTokens = *res.Usage.InputTokens
	}
	if res.Usage.OutputTokens != nil {
		u.OutputTokens = *res.Usage.OutputTokens
	}
	return u
}

func contentDigest(parts ...string) string {
	h := sha256.New()
	empty := true
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		empty = false
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	if empty {
		return ""
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Ensure production adapter cannot accidentally share FakeAdapter defaults.
var _ Adapter = (*AgentAdapter)(nil)

// ProductionDefaultProvider returns the production Execute function.
// Tests that need Fake must inject providerexec.NewFake().Execute explicitly.
func ProductionDefaultProvider() func(ctx context.Context, req Request) (Outcome, error) {
	a := NewAgentAdapter()
	return a.Execute
}

// WorkDirExists is a small helper used by callers that want a pre-check.
func WorkDirExists(dir string) bool {
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}
