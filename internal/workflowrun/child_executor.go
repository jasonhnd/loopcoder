package workflowrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// ChildRoute is the immutable per-child provider pin for one WorkItem.
// Permission/WindowKind/AccountRef/ReservationID/InstallRef are first-class
// atomic route identity — not optional prose.
type ChildRoute struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Depth      string `json:"depth,omitempty"`
	Permission string `json:"permission,omitempty"`
	// TaskClass is the exact classified capability floor (luna|tera|soul) from
	// the child's RouteRequirement. Empty only for legacy resume rows.
	TaskClass  string `json:"task_class,omitempty"`
	AccountRef string `json:"account_ref,omitempty"`
	// InstallRef is the non-secret install/observation binding for the account.
	InstallRef    string `json:"install_ref,omitempty"`
	WindowKind    string `json:"window_kind,omitempty"`
	ReservationID string `json:"reservation_id,omitempty"`
	RouteReason   string `json:"route_reason,omitempty"`
	// CapabilityProbeOnly runs a fixed read-only no-tools prompt. Success is
	// fail-closed and never integrated; only typed model_unavailable may use the
	// normal generation-safe alternate path.
	CapabilityProbeOnly bool `json:"capability_probe_only,omitempty"`
}

// ProcessStart is workflow-owned spawn identity published while the provider
// process is alive (via agent.Invocation.OnProviderStart / supervisedexec OnStart).
// Not JSON-serialized on ChildExecInput; Service binds a callback per attempt.
type ProcessStart struct {
	PID                   int
	PGID                  int
	ProcessBirthIdentity  string
	ExecutableIdentity    string
	ObservedAt            time.Time
	IdentityAmbiguous     bool
	IdentityAmbiguityNote string
	// WorktreePath/LogPath set by ProductionChildExecutor before callback (authority).
	WorktreePath string
	LogPath      string
}

// ChildExecInput is one LoopCoder-owned child launch request.
type ChildExecInput struct {
	ProjectID  string
	RunID      string
	GraphID    string
	WorkItemID string
	ClaimID    string
	AttemptID  string
	Intent     string
	Route      ChildRoute
	RepoPath   string
	// BaseRef is the git ref to materialize the child worktree from (goal branch
	// HEAD after prior integrations). Empty → HEAD of RepoPath.
	BaseRef string
	// ReadOnly forces read-only provider mode (research children).
	ReadOnly bool
	Timeout  time.Duration
	// OnProcessStart is a non-serialized, workflow-owned process-start callback.
	// ProductionChildExecutor wires it to agent.Invocation.OnProviderStart so the
	// Service can persist a critical pid event while the child is still alive.
	// Returning an error fails closed through supervisedexec OnStart (kill+drain).
	// ProcessStart.WorktreePath/LogPath are filled by ProductionChildExecutor
	// before the callback so authority can be persisted with the same paths.
	OnProcessStart func(ProcessStart) error `json:"-"`
	// Guardian configures macOS supervisor-death guardian (supervisedexec).
	// Production Service fills this for real spawns; empty = disabled.
	Guardian supervisedexec.GuardianOptions `json:"-"`
	// OnWorktreeAllocated is invoked after exclusive worktree allocation succeeds
	// (real concurrent peak enter). Returning a non-nil error aborts the child
	// before further work (Service refuses a second allocation while one is active).
	// On error, the executor must synchronously release the just-created path —
	// Service still tracks only the prior lease and will not clean the new path.
	// Service owns exact leave after integrate/final terminal/error unwind.
	OnWorktreeAllocated func(path string) error `json:"-"`
}

// ChildExecResult is durable child terminal evidence. Capacity actual is only
// set when the provider reported usable usage — never fabricated.
type ChildExecResult struct {
	Terminal       workgraph.TerminalState
	OutputEvidence string // required for success close
	WorktreePath   string
	ProcessPID     int
	// Spawn-time process identity (production path fills from OnProviderStart).
	ProcessPGID           int
	ProcessBirthIdentity  string
	ExecutableIdentity    string
	ProcessObservedAt     time.Time
	IdentityAmbiguous     bool
	IdentityAmbiguityNote string
	// SpawnObserved is true when ProductionChildExecutor saw a real OnProviderStart.
	SpawnObserved bool
	ExitCode      int
	InputTokens   int64
	OutputTokens  int64
	// ActualCapacity is a fraction [0,1] when known; nil means unknown.
	ActualCapacity *float64
	// ActualSource is provider_usage|estimated|unknown for capacity fraction
	// (never invent numbers). Distinct from ActualSources for route dimensions.
	ActualSource string
	// ActualSources is per-dimension evidence class for InvokedRoute Actual*
	// (provider_stream|accepted_invocation|auth_binding|install_binding|unknown).
	// accepted_invocation is never collapsed into provider_stream.
	ActualSources struct {
		Model      string `json:"model,omitempty"`
		Effort     string `json:"effort,omitempty"`
		Permission string `json:"permission,omitempty"`
		Account    string `json:"account,omitempty"`
		Install    string `json:"install,omitempty"`
	} `json:"actual_sources,omitempty"`
	// ArgvDigest is redacted exact launched argv fingerprint when known.
	ArgvDigest   string
	FailureClass string
	Message      string
	// InvokedRoute is the actual invocation metadata the executor used.
	// Service exact-compares this to ChildExecInput.Route before success;
	// never copy route from the request into ChildOutcome without this echo.
	InvokedRoute ChildRoute
	// Deprecated fields kept for intermediate mapping — prefer InvokedRoute.
	Provider string
	Model    string
	Depth    string
	// FilesTouched are relative paths written under the child worktree.
	FilesTouched []string
}

// ChildExecutor runs one claimed WorkItem as a LoopCoder-owned transparent child.
// Production default invokes the routed provider in an exclusive worktree.
// Tests inject FakeChildExecutor; remote acceptance must use production.
type ChildExecutor interface {
	Execute(ctx context.Context, in ChildExecInput) (ChildExecResult, error)
}

// FakeChildExecutor is for focused unit tests only. It still allocates a real
// worktree and writes evidence files (not the old Claim→Close string stub),
// but never launches a live provider process.
type FakeChildExecutor struct {
	// FailIDs force TermFailed for matching work item ids.
	FailIDs map[string]bool
	// CancelAfterIDs force TermCancelled after success write for matching ids
	// (simulates forced interrupt mid-child). Prefer FailIDs for hard fails.
	CancelAfterIDs map[string]bool
	// HangIDs block until ctx is cancelled (true forced interrupt mid-flight).
	// Does NOT call OnProcessStart and does NOT use a production spawn identity.
	// ProcessPID may be left 0; tests that need durable pid/authority must use
	// the test-only package workflowrun/testspawn (own process group), never the
	// supervisor PID and never a production Fake OnProcessStart with os.Getpid.
	HangIDs map[string]bool
	// OnHangEntry is a test-only handshake invoked after the HangIDs branch has
	// entered the wait state, immediately before blocking on ctx.Done.
	OnHangEntry func(workItemID string, pid int)
	// ForceProcessPID is deprecated for production-path evidence. When true it may
	// set ProcessPID for occupancy-only pulses but never calls OnProcessStart and
	// never claims the current process as a child (no authority over the runner).
	ForceProcessPID bool
	// Calls records provider-exec invocations per work item (exactly-once tests).
	// When non-nil, each Execute increments Calls[WorkItemID].
	Calls map[string]int
	// InvocationCountPath when set appends one line per Execute (subprocess tests).
	InvocationCountPath string
	// MutateInvokedRoute when set alters echoed InvokedRoute for mismatch tests.
	MutateInvokedRoute func(ChildRoute) ChildRoute
	// ProductFiles optional per-work-item relative paths to write as product
	// content (for integrate tests). Default: notes/<id>.md + optional test.
	ProductFiles map[string][]string
	// FailModel triggers model_unavailable when Route.Model matches (case-insensitive).
	// Used for generation-safe alternate reroute tests.
	FailModel string
	// HomeDir overrides layout root (tests).
	HomeDir string
	Now     func() time.Time
}

// echoRoute returns InvokedRoute metadata for the actual invocation.
// Writes a durable invocation-binding evidence file under the worktree so
// InvokedRoute is not merely a request copy without side-effect proof.
// MutateInvokedRoute (tests) may alter a copy for identity mismatch injection.
func (f FakeChildExecutor) echoRoute(in ChildExecInput) ChildRoute {
	r := in.Route
	if f.MutateInvokedRoute != nil {
		r = f.MutateInvokedRoute(r)
	}
	return r
}

// writeInvocationBinding persists non-secret route binding for audit; returns
// the binding used as InvokedRoute (must match request for success).
func writeInvocationBinding(wt string, r ChildRoute) error {
	if wt == "" {
		return nil
	}
	dir := filepath.Join(wt, ".loopcoder")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(map[string]string{
		"provider": r.Provider, "model": r.Model, "depth": r.Depth,
		"permission": r.Permission, "account_ref": r.AccountRef,
		"install_ref": r.InstallRef, "window_kind": r.WindowKind,
		"reservation_id": r.ReservationID,
		"capability_probe_only": func() string {
			if r.CapabilityProbeOnly {
				return "true"
			}
			return ""
		}(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "invocation-binding.json"), b, 0o600)
}

func withRoute(base ChildExecResult, r ChildRoute) ChildExecResult {
	base.InvokedRoute = r
	base.Provider = r.Provider
	base.Model = r.Model
	base.Depth = r.Depth
	return base
}

// Execute implements ChildExecutor for tests.
func (f FakeChildExecutor) Execute(ctx context.Context, in ChildExecInput) (ChildExecResult, error) {
	if f.Calls != nil {
		f.Calls[in.WorkItemID]++
	}
	// Persist invocation count to durable file when configured (subprocess tests).
	if p := strings.TrimSpace(f.InvocationCountPath); p != "" {
		_ = appendInvocationCount(p, in.WorkItemID)
	}
	route := f.echoRoute(in)
	if err := ctx.Err(); err != nil {
		return withRoute(ChildExecResult{
			Terminal: workgraph.TermCancelled, FailureClass: "cancelled", Message: err.Error(),
			ActualSource: "unknown",
		}, route), err
	}
	now := f.Now
	if now == nil {
		now = time.Now
	}
	wt, err := allocateChildWorktree(f.HomeDir, in.ProjectID, in.GraphID, in.WorkItemID, in.AttemptID, in.RepoPath, in.BaseRef)
	if err != nil {
		return withRoute(ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "worktree", Message: err.Error(),
			ActualSource: "unknown",
		}, route), err
	}
	// Allocation enter only — Service owns exact leave after integrate/terminal/unwind.
	if in.OnWorktreeAllocated != nil {
		if aerr := in.OnWorktreeAllocated(wt); aerr != nil {
			// Callback rejected: release the just-created path (prior lease is Service-owned).
			cerr := releaseChildWorktree(in.RepoPath, wt)
			msg := aerr.Error()
			if cerr != nil {
				msg = msg + "; cleanup new worktree: " + cerr.Error()
			}
			return withRoute(ChildExecResult{
				Terminal: workgraph.TermFailed, FailureClass: "worktree_lease", Message: msg,
				// Path empty only if cleanup succeeded; otherwise retain for leak evidence.
				WorktreePath: worktreePathIfLeaked(wt, cerr), ActualSource: "unknown",
			}, route), fmt.Errorf("%s", msg)
		}
	}
	evPath, digest, files, err := writeChildEvidence(wt, in, "fake_executor", now().UTC())
	if err != nil {
		return withRoute(ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "evidence", Message: err.Error(),
			WorktreePath: wt, ActualSource: "unknown",
		}, route), err
	}
	prod := writeFakeProductFiles(wt, in, f.ProductFiles)
	files = append(files, prod...)
	// Durable invocation binding (non-secret) — proves which route was used.
	if err := writeInvocationBinding(wt, route); err != nil {
		return withRoute(ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "invocation_binding", Message: err.Error(),
			WorktreePath: wt, ActualSource: "unknown",
		}, route), err
	}
	if f.FailIDs != nil && f.FailIDs[in.WorkItemID] {
		return withRoute(ChildExecResult{
			Terminal: workgraph.TermFailed, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: 1, FailureClass: "injected_fail", Message: "fake fail " + in.WorkItemID,
			FilesTouched: files, ActualSource: "unknown",
		}, route), nil
	}
	if strings.TrimSpace(f.FailModel) != "" &&
		strings.EqualFold(strings.TrimSpace(in.Route.Model), strings.TrimSpace(f.FailModel)) {
		return withRoute(ChildExecResult{
			Terminal: workgraph.TermFailed, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: 1, FailureClass: "model_unavailable",
			Message:      "invalid model selection " + in.Route.Model,
			FilesTouched: files, ActualSource: "unknown",
		}, route), nil
	}
	if f.HangIDs != nil && f.HangIDs[in.WorkItemID] {
		// No OnProcessStart / no supervisor PID. For durable spawn identity tests use
		// the test-only package workflowrun/testspawn instead.
		if f.OnHangEntry != nil {
			f.OnHangEntry(in.WorkItemID, 0)
		}
		<-ctx.Done()
		return withRoute(ChildExecResult{
			Terminal: workgraph.TermCancelled, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: 130, FailureClass: "forced_interrupt",
			Message:      "forced interrupt while running " + in.WorkItemID,
			FilesTouched: files, ActualSource: "unknown",
		}, route), ctx.Err()
	}
	if f.CancelAfterIDs != nil && f.CancelAfterIDs[in.WorkItemID] {
		// Executor-local cancel — not Service forced_interrupt. Service reclassifies
		// any forced_interrupt label without Service emission to executor_cancelled.
		return withRoute(ChildExecResult{
			Terminal: workgraph.TermCancelled, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: 130, FailureClass: FailureClassExecutorCancelled, Message: "fake executor cancel " + in.WorkItemID,
			FilesTouched: files, ActualSource: "unknown",
		}, route), nil
	}
	_ = evPath
	// ForceProcessPID never attaches production authority to the current process.
	// It is ignored for durable spawn identity (no OnProcessStart, SpawnObserved=false).
	_ = f.ForceProcessPID
	return withRoute(ChildExecResult{
		Terminal: workgraph.TermSucceeded, OutputEvidence: digest, WorktreePath: wt,
		ExitCode: 0, FilesTouched: files, ActualSource: "unknown",
		Message: "fake_executor_ok",
	}, route), nil
}

func appendInvocationCount(path, workItemID string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\n", workItemID)
	return err
}

// ProductionChildExecutor is the default LoopCoder-owned child executor.
// It allocates an exclusive worktree and invokes the routed provider via
// agent.Runner with DisableDelegation (never provider-native subagents).
// Fixture routes are test-only (AllowFixture); production fails closed on
// empty/auto/fixture without explicit test mode.
type ProductionChildExecutor struct {
	HomeDir string
	// Lookup defaults to agent.Lookup.
	Lookup func(provider string) (agent.Runner, error)
	Now    func() time.Time
	// HardCap bounds each child provider call (default 10m).
	HardCap time.Duration
	// AllowFixture enables fixture_local evidence without a live process.
	// Production must leave this false.
	AllowFixture bool
}

// DefaultChildExecutor returns the production executor.
func DefaultChildExecutor() ChildExecutor {
	return ProductionChildExecutor{}
}

// stampRoute copies only InvokedRoute fields already filled on out (actuals).
// Never copies requested route model/depth/permission into success actuals.
func stampRoute(out ChildExecResult, route ChildRoute) ChildExecResult {
	if out.InvokedRoute.Provider == "" && out.Provider != "" {
		out.InvokedRoute.Provider = out.Provider
	}
	if out.InvokedRoute.Model == "" && out.Model != "" {
		out.InvokedRoute.Model = out.Model
	}
	if out.InvokedRoute.Depth == "" && out.Depth != "" {
		out.InvokedRoute.Depth = out.Depth
	}
	_ = route
	return out
}

// observedInvokedRoute binds capacity-window identity only after the runner
// affirmed the exact account and installation selected for this invocation.
// This applies to typed non-success results too: model_unavailable is durable
// route evidence, but requested window/reservation values must never be copied
// across an absent or mismatched account/install observation.
func observedInvokedRoute(requested ChildRoute, provider, model, depth, permission, account, install string) ChildRoute {
	invoked := ChildRoute{
		Provider: provider, Model: model, Depth: depth, Permission: permission,
		AccountRef: account, InstallRef: install,
		CapabilityProbeOnly: requested.CapabilityProbeOnly,
	}
	if account != "" && account == strings.TrimSpace(requested.AccountRef) &&
		install != "" && install == strings.TrimSpace(requested.InstallRef) {
		invoked.WindowKind = strings.TrimSpace(requested.WindowKind)
		invoked.ReservationID = strings.TrimSpace(requested.ReservationID)
	}
	return invoked
}

// Execute implements ChildExecutor for production.
func (p ProductionChildExecutor) Execute(ctx context.Context, in ChildExecInput) (ChildExecResult, error) {
	route := in.Route
	if err := ctx.Err(); err != nil {
		return stampRoute(ChildExecResult{
			Terminal: workgraph.TermCancelled, FailureClass: "cancelled", Message: err.Error(),
			Provider: in.Route.Provider, Model: in.Route.Model, Depth: in.Route.Depth,
			ActualSource: "unknown",
		}, route), err
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	prov := strings.TrimSpace(in.Route.Provider)
	model := strings.TrimSpace(in.Route.Model)
	depth := strings.TrimSpace(in.Route.Depth)
	if prov == "" || model == "" {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "route_incomplete",
			Message:      "provider and model required (no fixture production default)",
			ActualSource: "unknown",
		}, fmt.Errorf("workflowrun: provider/model required")
	}
	if strings.EqualFold(prov, "auto") || strings.EqualFold(model, "auto") {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "route_unresolved",
			Message:      "auto provider/model not resolved (fail closed)",
			ActualSource: "unknown",
		}, fmt.Errorf("workflowrun: auto route unresolved")
	}
	if depth == "" {
		// Depth unset is not medium invent — fail when exact depth is required later.
		// Keep empty so exactRouteMatch fails closed if parent required a depth.
	}
	in.Route.Provider, in.Route.Model, in.Route.Depth = prov, model, depth

	// Fixture is test-only.
	if strings.EqualFold(prov, "fixture") || strings.EqualFold(model, "fixture-model") {
		if !p.AllowFixture {
			return ChildExecResult{
				Terminal: workgraph.TermFailed, FailureClass: "fixture_forbidden",
				Message:  "fixture provider forbidden on production executor",
				Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
			}, fmt.Errorf("workflowrun: fixture requires AllowFixture")
		}
	}

	wt, err := allocateChildWorktree(p.HomeDir, in.ProjectID, in.GraphID, in.WorkItemID, in.AttemptID, in.RepoPath, in.BaseRef)
	if err != nil {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "worktree", Message: err.Error(),
			Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
		}, err
	}
	// Allocation enter only — Service owns exact leave after integrate/terminal/unwind.
	if in.OnWorktreeAllocated != nil {
		if aerr := in.OnWorktreeAllocated(wt); aerr != nil {
			cerr := releaseChildWorktree(in.RepoPath, wt)
			msg := aerr.Error()
			if cerr != nil {
				msg = msg + "; cleanup new worktree: " + cerr.Error()
			}
			return ChildExecResult{
				Terminal: workgraph.TermFailed, FailureClass: "worktree_lease", Message: msg,
				WorktreePath: worktreePathIfLeaked(wt, cerr), Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
			}, fmt.Errorf("%s", msg)
		}
	}
	// Snapshot parent/disposable root + durable project root before provider runs.
	// Root mutation during child execution is an isolation failure (fail closed).
	parentRoot := strings.TrimSpace(in.RepoPath)
	projectRoot := projectRootFromWorktree(wt)
	parentSnap := snapshotDirTree(parentRoot)
	projectSnap := snapshotDirTree(projectRoot)

	// Explicit test fixture path only (AllowFixture).
	if p.AllowFixture && (strings.EqualFold(prov, "fixture") || strings.EqualFold(model, "fixture-model")) {
		_, digest, files, werr := writeChildEvidence(wt, in, "fixture_local", now().UTC())
		if werr != nil {
			return ChildExecResult{
				Terminal: workgraph.TermFailed, FailureClass: "evidence", Message: werr.Error(),
				WorktreePath: wt, Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
			}, werr
		}
		// Still hash product files when present; stub-only is allowed only in AllowFixture.
		// Never ignore digest errors (symlink/non-regular product must fail closed).
		if productDig, productFiles, perr := productOutputDigest(wt); perr != nil {
			return ChildExecResult{
				Terminal: workgraph.TermFailed, FailureClass: FailureClassProductDigest, Message: perr.Error(),
				WorktreePath: wt, Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
			}, perr
		} else if productDig != "" {
			digest = productDig
			files = mergeUniquePaths(files, productFiles)
		}
		return stampRoute(ChildExecResult{
			Terminal: workgraph.TermSucceeded, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: 0, Provider: prov, Model: model, Depth: depth,
			FilesTouched: files, ActualSource: "unknown",
			Message: "fixture_local_evidence",
			InvokedRoute: ChildRoute{Provider: prov, Model: model, Depth: depth,
				Permission: in.Route.Permission, AccountRef: in.Route.AccountRef, InstallRef: in.Route.InstallRef},
		}, route), nil
	}

	lookup := p.Lookup
	if lookup == nil {
		lookup = agent.Lookup
	}
	runner, lerr := lookup(prov)
	if lerr != nil {
		// Honest failure — do not silently close as success without a provider.
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "unsupported_provider",
			Message: lerr.Error(), WorktreePath: wt,
			Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
		}, lerr
	}

	logPath, err := providerControlPlaneLogPath(p.HomeDir, in.ProjectID, in.RunID, in.AttemptID)
	if err != nil {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "control_plane_path",
			Message: err.Error(), WorktreePath: wt,
			Provider: prov, Model: model, Depth: depth, ActualSource: "unknown",
		}, err
	}
	prompt := strings.TrimSpace(in.Intent)
	if prompt == "" {
		prompt = "bounded LoopCoder child work item " + in.WorkItemID
	}
	if in.Route.CapabilityProbeOnly {
		if !in.ReadOnly || strings.TrimSpace(in.Route.Permission) != "read-only" {
			return ChildExecResult{
				Terminal: workgraph.TermFailed, FailureClass: "capability_probe_permission",
				Message:      "capability probe requires exact read-only route",
				WorktreePath: wt, Provider: prov, Model: model, Depth: depth,
				ActualSource: "unknown",
			}, fmt.Errorf("workflowrun: capability probe requires read-only")
		}
		prompt = "Reply with exactly OK. Do not use tools."
	}
	// Explicit anti-delegation: LoopCoder owns children; never provider-native subagents.
	hardCap := p.HardCap
	if hardCap <= 0 {
		hardCap = 10 * time.Minute
	}
	if in.Timeout > 0 && in.Timeout < hardCap {
		hardCap = in.Timeout
	}
	runCtx := ctx
	var cancel context.CancelFunc
	if hardCap > 0 {
		runCtx, cancel = context.WithTimeout(ctx, hardCap)
		defer cancel()
	}

	inv := agent.Invocation{
		WorktreePath:        wt,
		Prompt:              prompt,
		Model:               model,
		Effort:              depth,
		Permission:          strings.TrimSpace(in.Route.Permission),
		AccountRef:          strings.TrimSpace(in.Route.AccountRef),
		InstallRef:          strings.TrimSpace(in.Route.InstallRef),
		WindowKind:          strings.TrimSpace(in.Route.WindowKind),
		ReservationID:       strings.TrimSpace(in.Route.ReservationID),
		ReadOnly:            in.ReadOnly,
		BoundedWrite:        !in.ReadOnly,
		DisableDelegation:   true,
		CapabilityProbeOnly: in.Route.CapabilityProbeOnly,
		Role:                "nested-bounded-write",
		LogPath:             logPath,
		RunID:               in.AttemptID,
		ProviderKey:         in.AttemptID,
		HardCap:             hardCap,
		Guardian:            in.Guardian,
	}
	if in.ReadOnly {
		inv.Role = "nested-read-only"
		inv.BoundedWrite = false
	}
	// Capture real spawn identity from agent.OnProviderStart (supervisedexec authority).
	// Never invent PID after runner.Run returns.
	var spawnStart ProcessStart
	var spawnSeen bool
	inv.OnProviderStart = func(pp agent.ProviderProcess) error {
		ps := ProcessStart{
			PID:                   pp.PID,
			PGID:                  pp.PGID,
			ProcessBirthIdentity:  pp.ProcessBirthIdentity,
			ExecutableIdentity:    pp.ExecutableIdentity,
			ObservedAt:            pp.ObservedAt,
			IdentityAmbiguous:     pp.IdentityAmbiguous,
			IdentityAmbiguityNote: pp.IdentityAmbiguityNote,
			WorktreePath:          wt,
			LogPath:               logPath,
		}
		spawnStart = ps
		spawnSeen = true
		if in.OnProcessStart != nil {
			return in.OnProcessStart(ps)
		}
		return nil
	}

	res, rerr := runner.Run(runCtx, inv)
	// Typed model_unavailable is fail-closed here: do NOT silently change
	// required depth or re-run on the same claim (that would fake a route
	// success and duplicate capacity/files semantics). Scheduler/goalrun must
	// pick another HardEligible same-depth candidate with a generation-safe
	// retry attempt when that path is wired; until then surface the class.
	if in.Route.CapabilityProbeOnly {
		// Canary authority accepts only the adapter's exact typed refusal from
		// the fixed prompt. Never derive it from Summary/error prose.
		if res.FailureClass != "model_unavailable" {
			res.FailureClass = firstNonEmpty(res.FailureClass, "capability_probe_unclassified_failure")
		}
	} else if isModelUnavailableResult(res, rerr) && res.FailureClass == "" {
		res.FailureClass = "model_unavailable"
	}
	// Audit stub (not acceptance evidence). Product OutputEvidence comes from
	// actual changed product paths after execution.
	evidenceIn := in
	if in.Route.CapabilityProbeOnly {
		evidenceIn.Intent = "fixed read-only capability probe"
	}
	_, _, auditFiles, _ := writeChildEvidence(wt, evidenceIn, "provider_run", now().UTC())
	// Actual route ONLY from independently verified runner Actual* fields.
	// Never res.Model/res.Effort (request-echo) or inv request fallbacks.
	actualProv := strings.TrimSpace(res.ActualProvider)
	actualModel := strings.TrimSpace(res.ActualModel)
	actualDepth := strings.TrimSpace(res.ActualEffort)
	actualPerm := strings.TrimSpace(res.ActualPermission)
	actualAcct := strings.TrimSpace(res.ActualAccountRef)
	actualInstall := strings.TrimSpace(res.ActualInstallRef)
	if res.FailureClass == "model_unavailable" && in.Route.CapabilityProbeOnly {
		// The provider refused the exact attempted argv. Preserve attempted route
		// identity without promoting it to accepted_invocation success evidence.
		actualProv = strings.TrimSpace(in.Route.Provider)
		actualModel = strings.TrimSpace(in.Route.Model)
		actualDepth = strings.TrimSpace(in.Route.Depth)
		actualPerm = strings.TrimSpace(in.Route.Permission)
		res.ActualSourceModel = agent.ActualSourceAttemptedInvocation
		res.ActualSourceEffort = agent.ActualSourceAttemptedInvocation
		res.ActualSourcePermission = agent.ActualSourceAttemptedInvocation
	}
	// Carry honest per-dimension sources + argv digest through child evidence.
	bindSources := func(out *ChildExecResult) {
		if out == nil {
			return
		}
		out.ActualSources.Model = res.ActualSourceModel
		out.ActualSources.Effort = res.ActualSourceEffort
		out.ActualSources.Permission = res.ActualSourcePermission
		out.ActualSources.Account = res.ActualSourceAccount
		out.ActualSources.Install = res.ActualSourceInstall
		out.ArgvDigest = res.ArgvDigest
	}
	// Stamp spawn-time identity captured from OnProviderStart only.
	bindSpawn := func(out *ChildExecResult) {
		if out == nil || !spawnSeen {
			return
		}
		out.SpawnObserved = true
		out.ProcessPID = spawnStart.PID
		out.ProcessPGID = spawnStart.PGID
		out.ProcessBirthIdentity = spawnStart.ProcessBirthIdentity
		out.ExecutableIdentity = spawnStart.ExecutableIdentity
		out.ProcessObservedAt = spawnStart.ObservedAt
		out.IdentityAmbiguous = spawnStart.IdentityAmbiguous
		out.IdentityAmbiguityNote = spawnStart.IdentityAmbiguityNote
	}
	// Product evidence digest from actual worktree changes (not stub evidence file).
	// On already-failed process paths, preserve real FailureClass/route/spawn/usage:
	// digest errors only clear product evidence (do not overwrite process FC).
	digest, files, digErr := productOutputDigest(wt)
	if digErr != nil {
		digest, files = "", nil
	}
	files = mergeUniquePaths(files, auditFiles)
	if rerr != nil {
		term := workgraph.TermFailed
		fc := firstNonEmpty(res.FailureClass, "process_failure")
		if errors.Is(rerr, context.Canceled) {
			term = workgraph.TermCancelled
			fc = "cancelled"
		} else if errors.Is(rerr, context.DeadlineExceeded) {
			term = workgraph.TermFailed
			fc = "timeout"
		}
		out := ChildExecResult{
			Terminal: term, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: res.ExitCode, FailureClass: fc, Message: rerr.Error(),
			Provider: actualProv, Model: actualModel, Depth: actualDepth, FilesTouched: files,
			ActualSource: "unknown",
		}
		bindSources(&out)
		bindSpawn(&out)
		out.InvokedRoute = observedInvokedRoute(
			in.Route, actualProv, actualModel, actualDepth, actualPerm, actualAcct, actualInstall,
		)
		out = attachUsage(out, res)
		return out, rerr
	}
	if res.ExitCode != 0 {
		out := ChildExecResult{
			Terminal: workgraph.TermFailed, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: res.ExitCode, FailureClass: firstNonEmpty(res.FailureClass, "nonzero_exit"),
			Message:  firstNonEmpty(res.Summary, fmt.Sprintf("exit %d", res.ExitCode)),
			Provider: actualProv, Model: actualModel, Depth: actualDepth, FilesTouched: files,
			ActualSource: "unknown",
		}
		bindSources(&out)
		bindSpawn(&out)
		out.InvokedRoute = observedInvokedRoute(
			in.Route, actualProv, actualModel, actualDepth, actualPerm, actualAcct, actualInstall,
		)
		out = attachUsage(out, res)
		return out, nil
	}
	if in.Route.CapabilityProbeOnly {
		out := ChildExecResult{
			Terminal: workgraph.TermFailed, WorktreePath: wt,
			ExitCode: 1, FailureClass: "capability_probe_unexpected_success",
			Message:  "declared-only capability probe succeeded but is not an authoritative production route",
			Provider: actualProv, Model: actualModel, Depth: actualDepth,
			FilesTouched: files, ActualSource: "unknown",
			InvokedRoute: observedInvokedRoute(
				in.Route, actualProv, actualModel, actualDepth, actualPerm, actualAcct, actualInstall,
			),
		}
		bindSources(&out)
		bindSpawn(&out)
		out = attachUsage(out, res)
		return out, fmt.Errorf("workflowrun: capability probe unexpectedly succeeded; refusing product integration")
	}
	// Isolation first (before evidence/actual requirements) so escapes fail closed
	// even when no in-worktree product files or Actual* affirmation exist.
	if !in.ReadOnly {
		escaped := detectWorktreeEscapes(wt)
		parentMut := diffDirTree(parentSnap, snapshotDirTree(parentRoot), wt)
		projectMut := diffDirTree(projectSnap, snapshotDirTree(projectRoot), wt)
		if len(escaped) > 0 || len(parentMut) > 0 || len(projectMut) > 0 {
			cleanupIsolationViolation(escaped, parentMut, projectMut, parentSnap, projectSnap, parentRoot, projectRoot)
			all := append(append([]string{}, escaped...), parentMut...)
			all = append(all, projectMut...)
			msg := fmt.Sprintf("worktree isolation violation (fail closed, cleaned): %s",
				strings.Join(all, ", "))
			out := ChildExecResult{
				Terminal: workgraph.TermFailed, OutputEvidence: digest, WorktreePath: wt,
				ExitCode: 1, FailureClass: "isolation_violation", Message: msg,
				Provider: actualProv, Model: actualModel, Depth: actualDepth, FilesTouched: append(files, all...),
				ActualSource: "unknown",
			}
			out = attachUsage(out, res)
			return out, fmt.Errorf("workflowrun: %s", msg)
		}
	}
	// Re-hash product after isolation check; mutation must invalidate acceptance.
	// Provider logs/summaries alone are not product (see productOutputDigest).
	// Research/verify are often provider-sandbox read-only and still need durable
	// product for acceptance — materialize from independently observed provider
	// Summary after process exit (LoopCoder-owned write, not Actual* echo).
	//
	// If materialize is attempted and fails, surface the real typed failure —
	// never swallow into generic missing_evidence (RC39 Stage D diagnostics).
	// Digest errors on the success path are fail-closed (never discard).
	failProductDigest := func(perr error) (ChildExecResult, error) {
		out := ChildExecResult{
			Terminal: workgraph.TermFailed, WorktreePath: wt,
			FailureClass: FailureClassProductDigest, Message: perr.Error(),
			Provider: actualProv, Model: actualModel, Depth: actualDepth,
			ActualSource: "unknown",
			InvokedRoute: ChildRoute{
				Provider: actualProv, Model: actualModel, Depth: actualDepth,
				Permission: actualPerm, AccountRef: actualAcct, InstallRef: actualInstall,
			},
		}
		bindSources(&out)
		bindSpawn(&out)
		out = attachUsage(out, res)
		return out, perr
	}
	digest, files, digErr = productOutputDigest(wt)
	if digErr != nil {
		return failProductDigest(digErr)
	}
	if strings.TrimSpace(digest) == "" {
		role := ClassifyTaskRole(in.WorkItemID, in.Intent, "")
		var merr error
		var fc string
		switch role {
		case RoleResearch:
			fc = FailureClassResearchFindingsMaterialization
			merr = materializeResearchFindings(wt, res.Summary, in)
		case RoleVerify:
			fc = FailureClassVerifierVerdictMaterialization
			merr = materializeVerifierVerdict(wt, res.Summary, in)
		case RoleDocs:
			fc = FailureClassDocsMaterialization
			merr = materializeDocsNotes(wt, res.Summary, in)
		}
		if fc != "" {
			if merr != nil {
				out := ChildExecResult{
					Terminal: workgraph.TermFailed, WorktreePath: wt,
					FailureClass: fc, Message: merr.Error(),
					Provider: actualProv, Model: actualModel, Depth: actualDepth,
					ActualSource: "unknown",
					InvokedRoute: ChildRoute{
						Provider: actualProv, Model: actualModel, Depth: actualDepth,
						Permission: actualPerm, AccountRef: actualAcct, InstallRef: actualInstall,
					},
				}
				bindSources(&out)
				bindSpawn(&out)
				out = attachUsage(out, res)
				return out, merr
			}
			digest, files, digErr = productOutputDigest(wt)
			if digErr != nil {
				return failProductDigest(digErr)
			}
		}
	}
	if strings.TrimSpace(digest) == "" {
		out := ChildExecResult{
			Terminal: workgraph.TermFailed, WorktreePath: wt, FailureClass: "missing_evidence",
			Message: "no product artifact content after execution", ActualSource: "unknown",
		}
		bindSpawn(&out)
		return out, fmt.Errorf("workflowrun: no product artifacts")
	}
	// Exact route success requires independently affirmed identity.
	if actualProv == "" || actualModel == "" {
		out := ChildExecResult{
			Terminal: workgraph.TermFailed, WorktreePath: wt, FailureClass: "route_mismatch",
			Message: "runner did not affirm actual provider/model", ActualSource: "unknown",
		}
		bindSpawn(&out)
		return out, fmt.Errorf("workflowrun: actual provider/model unobserved")
	}
	if strings.TrimSpace(in.Route.Depth) != "" && actualDepth == "" {
		out := ChildExecResult{
			Terminal: workgraph.TermFailed, WorktreePath: wt, FailureClass: "route_mismatch",
			Message: "runner did not affirm actual depth for exact depth route", ActualSource: "unknown",
		}
		bindSpawn(&out)
		return out, fmt.Errorf("workflowrun: actual depth unobserved")
	}
	if strings.TrimSpace(in.Route.AccountRef) != "" && actualAcct == "" {
		out := ChildExecResult{
			Terminal: workgraph.TermFailed, WorktreePath: wt, FailureClass: "route_mismatch",
			Message: "runner did not affirm account_ref", Provider: actualProv, Model: actualModel, Depth: actualDepth,
			ActualSource: "unknown",
		}
		bindSpawn(&out)
		return out, fmt.Errorf("workflowrun: runner did not affirm account_ref")
	}
	if strings.TrimSpace(in.Route.AccountRef) != "" && actualAcct != "" &&
		!strings.EqualFold(strings.TrimSpace(in.Route.AccountRef), actualAcct) {
		out := ChildExecResult{
			Terminal: workgraph.TermFailed, WorktreePath: wt, FailureClass: "route_mismatch",
			Message:  fmt.Sprintf("account_ref mismatch requested=%s actual=%s", in.Route.AccountRef, actualAcct),
			Provider: actualProv, Model: actualModel, Depth: actualDepth, ActualSource: "unknown",
		}
		bindSpawn(&out)
		return out, fmt.Errorf("workflowrun: account_ref mismatch")
	}
	if strings.TrimSpace(in.Route.InstallRef) != "" {
		if actualInstall == "" {
			out := ChildExecResult{
				Terminal: workgraph.TermFailed, WorktreePath: wt, FailureClass: "route_mismatch",
				Message: "runner did not affirm install_ref", ActualSource: "unknown",
			}
			bindSpawn(&out)
			return out, fmt.Errorf("workflowrun: install_ref unobserved")
		}
		if actualInstall != strings.TrimSpace(in.Route.InstallRef) {
			out := ChildExecResult{
				Terminal: workgraph.TermFailed, WorktreePath: wt, FailureClass: "route_mismatch",
				Message:      fmt.Sprintf("install_ref mismatch requested=%s actual=%s", in.Route.InstallRef, actualInstall),
				ActualSource: "unknown",
			}
			bindSpawn(&out)
			return out, fmt.Errorf("workflowrun: install_ref mismatch")
		}
	}
	// Invoked route from runner-affirmed actuals only — never request fallbacks.
	// Window/reservation attach only after exact account+install match.
	invoked := observedInvokedRoute(
		in.Route, actualProv, actualModel, actualDepth, actualPerm, actualAcct, actualInstall,
	)
	// Production success requires a real spawn-time identity callback while the
	// process was alive — never invent PID after Run returns.
	if !spawnSeen {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: 1, FailureClass: "missing_spawn_identity",
			Message:  "production child requires OnProviderStart spawn identity while process is alive",
			Provider: actualProv, Model: actualModel, Depth: actualDepth, FilesTouched: files,
			ActualSource: "unknown", InvokedRoute: invoked,
		}, fmt.Errorf("workflowrun: missing spawn-time process identity")
	}
	if err := ValidateProcessStart(spawnStart); err != nil {
		out := ChildExecResult{
			Terminal: workgraph.TermFailed, OutputEvidence: digest, WorktreePath: wt,
			ExitCode: 1, FailureClass: "spawn_identity_invalid", Message: err.Error(),
			Provider: actualProv, Model: actualModel, Depth: actualDepth, FilesTouched: files,
			ActualSource: "unknown", InvokedRoute: invoked,
		}
		bindSpawn(&out)
		return out, err
	}
	out := ChildExecResult{
		Terminal: workgraph.TermSucceeded, OutputEvidence: digest, WorktreePath: wt,
		ExitCode: 0, Message: firstNonEmpty(res.Summary, "provider_ok"),
		Provider: invoked.Provider, Model: invoked.Model, Depth: invoked.Depth, FilesTouched: files,
	}
	bindSources(&out)
	bindSpawn(&out)
	out = attachUsage(out, res)
	if err := writeInvocationBinding(wt, invoked); err != nil {
		return ChildExecResult{
			Terminal: workgraph.TermFailed, FailureClass: "invocation_binding",
			Message: err.Error(), WorktreePath: wt, ActualSource: "unknown",
			InvokedRoute: invoked,
		}, err
	}
	out.InvokedRoute = invoked
	return out, nil
}

// ValidateProcessStart enforces the supervised start identity contract for
// workflow-owned pid events: PID/PGID > 0, non-empty birth + executable,
// non-zero ObservedAt (freshness from the real start callback — never invented),
// and not ambiguous.
func ValidateProcessStart(ps ProcessStart) error {
	if ps.PID <= 0 {
		return fmt.Errorf("workflowrun: process start PID must be > 0")
	}
	if ps.PGID <= 0 {
		return fmt.Errorf("workflowrun: process start PGID required (pid=%d)", ps.PID)
	}
	if strings.TrimSpace(ps.ProcessBirthIdentity) == "" {
		return fmt.Errorf("workflowrun: process_birth_identity required (pid=%d)", ps.PID)
	}
	if strings.TrimSpace(ps.ExecutableIdentity) == "" {
		return fmt.Errorf("workflowrun: executable_identity required (pid=%d)", ps.PID)
	}
	if ps.ObservedAt.IsZero() {
		return fmt.Errorf("workflowrun: process start observed_at required (pid=%d)", ps.PID)
	}
	if ps.IdentityAmbiguous {
		note := strings.TrimSpace(ps.IdentityAmbiguityNote)
		if note == "" {
			note = "ambiguous"
		}
		return fmt.Errorf("workflowrun: process identity ambiguous (pid=%d): %s", ps.PID, note)
	}
	return nil
}

// processStartPayload is the non-secret structured pid event payload.
// observed_at is always RFC3339Nano from the real ProcessStart (caller must
// ValidateProcessStart first — zero ObservedAt is not filled here).
// worktree_path and log_path are required for recovery identity.
func processStartPayload(ps ProcessStart) map[string]string {
	return map[string]string{
		"pid":                     fmt.Sprintf("%d", ps.PID),
		"pgid":                    fmt.Sprintf("%d", ps.PGID),
		"process_birth_identity":  strings.TrimSpace(ps.ProcessBirthIdentity),
		"executable_identity":     strings.TrimSpace(ps.ExecutableIdentity),
		"observed_at":             ps.ObservedAt.UTC().Format(time.RFC3339Nano),
		"worktree_path":           strings.TrimSpace(ps.WorktreePath),
		"log_path":                strings.TrimSpace(ps.LogPath),
		"identity_ambiguous":      fmt.Sprintf("%v", ps.IdentityAmbiguous),
		"identity_ambiguity_note": strings.TrimSpace(ps.IdentityAmbiguityNote),
	}
}

// ValidatePIDEventPayload checks durable pid event Event.PID and payload agree,
// observed_at is a parseable non-zero RFC3339Nano, and worktree_path/log_path are nonempty.
func ValidatePIDEventPayload(ev Event) error {
	if strings.TrimSpace(ev.Kind) != "pid" {
		return fmt.Errorf("workflowrun: ValidatePIDEventPayload requires kind=pid")
	}
	if ev.PID <= 0 {
		return fmt.Errorf("workflowrun: pid event Event.PID must be > 0")
	}
	if len(ev.Payload) == 0 {
		return fmt.Errorf("workflowrun: pid event payload required")
	}
	var m map[string]string
	if err := json.Unmarshal(ev.Payload, &m); err != nil {
		return fmt.Errorf("workflowrun: pid event payload: %w", err)
	}
	payloadPID := strings.TrimSpace(m["pid"])
	if payloadPID == "" {
		return fmt.Errorf("workflowrun: pid event payload missing pid")
	}
	if payloadPID != fmt.Sprintf("%d", ev.PID) {
		return fmt.Errorf("workflowrun: pid event Event.PID=%d != payload pid=%s", ev.PID, payloadPID)
	}
	if strings.TrimSpace(m["pgid"]) == "" {
		return fmt.Errorf("workflowrun: pid event payload missing pgid")
	}
	if strings.TrimSpace(m["process_birth_identity"]) == "" {
		return fmt.Errorf("workflowrun: pid event payload missing process_birth_identity")
	}
	if strings.TrimSpace(m["executable_identity"]) == "" {
		return fmt.Errorf("workflowrun: pid event payload missing executable_identity")
	}
	obs := strings.TrimSpace(m["observed_at"])
	if obs == "" {
		return fmt.Errorf("workflowrun: pid event payload missing observed_at")
	}
	ts, err := time.Parse(time.RFC3339Nano, obs)
	if err != nil {
		// Also accept RFC3339 without nano fraction.
		ts, err = time.Parse(time.RFC3339, obs)
		if err != nil {
			return fmt.Errorf("workflowrun: pid event observed_at not RFC3339Nano: %w", err)
		}
	}
	if ts.IsZero() {
		return fmt.Errorf("workflowrun: pid event observed_at is zero")
	}
	if strings.TrimSpace(m["worktree_path"]) == "" {
		return fmt.Errorf("workflowrun: pid event payload missing worktree_path")
	}
	if strings.TrimSpace(m["log_path"]) == "" {
		return fmt.Errorf("workflowrun: pid event payload missing log_path")
	}
	return nil
}

// childRoutePayloadFields returns non-secret required route keys from a ChildRoute.
func childRoutePayloadFields(r ChildRoute) map[string]string {
	return map[string]string{
		"provider":       strings.TrimSpace(r.Provider),
		"model":          strings.TrimSpace(r.Model),
		"depth":          strings.TrimSpace(r.Depth),
		"permission":     strings.TrimSpace(r.Permission),
		"account_ref":    strings.TrimSpace(r.AccountRef),
		"install_ref":    strings.TrimSpace(r.InstallRef),
		"window_kind":    strings.TrimSpace(r.WindowKind),
		"reservation_id": strings.TrimSpace(r.ReservationID),
		"route_reason":   strings.TrimSpace(r.RouteReason),
		"capability_probe_only": func() string {
			if r.CapabilityProbeOnly {
				return "true"
			}
			return ""
		}(),
	}
}

// mergePayloadStringMap copies src into dst (dst may be nil).
func mergePayloadStringMap(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		if strings.TrimSpace(v) != "" {
			dst[k] = strings.TrimSpace(v)
		}
	}
	return dst
}

// productOutputDigest hashes actual changed product paths/content under the
// worktree (excluding .loopcoder audit stubs, ownership markers, and provider
// runtime logs/summaries). Empty when no useful product change exists — cannot
// become successful evidence.
//
// Security: never os.Stat/os.ReadFile (those follow symlinks). Every leaf is
// hashed via the secure regular product chain (non-symlink root + parents,
// pre-Lstat / Open / SameFile / stream / post-Lstat). Any invalid product
// returns a non-nil error — callers must not discard it on success paths.
// Align exclusions with filterProductFiles so success digests cannot be built
// from files acceptance will discard (RC39 research).
func productOutputDigest(wt string) (digest string, files []string, err error) {
	if strings.TrimSpace(wt) == "" {
		return "", nil, nil
	}
	wtAbs, aerr := filepath.Abs(wt)
	if aerr != nil {
		return "", nil, fmt.Errorf("workflowrun: product digest worktree: %w", aerr)
	}
	if rerr := requireNonSymlinkDir(wtAbs); rerr != nil {
		return "", nil, fmt.Errorf("workflowrun: product digest worktree root: %w", rerr)
	}
	discovered, derr := discoverProductFiles(wtAbs)
	if derr != nil {
		return "", nil, derr
	}
	var product []string
	for _, rel := range discovered {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" || rel == ".loopcoder-owned-worktree" {
			continue
		}
		if runtimeOnlyProductPath(rel) {
			continue
		}
		if strings.HasPrefix(rel, ".loopcoder/") {
			continue
		}
		if strings.HasPrefix(rel, "child-output-") {
			continue
		}
		base := filepath.Base(rel)
		// Provider runtime artifacts are not product (same list as filterProductFiles).
		if strings.HasSuffix(base, ".log") || base == "prompt.txt" || base == "summary.txt" ||
			strings.HasPrefix(base, ".loopcoder-child") || base == "loopcoder-child-provider.log" {
			continue
		}
		// Reject unclean paths early with typed error (no silent skip).
		cleaned, cerr := cleanWorktreeRelPath(rel)
		if cerr != nil {
			return "", nil, fmt.Errorf("workflowrun: product path %s: %w", rel, cerr)
		}
		product = append(product, filepath.ToSlash(cleaned))
	}
	if len(product) == 0 {
		return "", nil, nil
	}
	h := sha256.New()
	for _, rel := range product {
		// Canonical digest input: slash-rel + NUL + secure stream + NUL.
		if _, werr := h.Write([]byte(rel)); werr != nil {
			return "", nil, werr
		}
		if _, werr := h.Write([]byte{0}); werr != nil {
			return "", nil, werr
		}
		if _, herr := streamSecureRegularProduct(wtAbs, rel, h, maxProductHashBytes); herr != nil {
			return "", nil, fmt.Errorf("workflowrun: product path %s: %w", rel, herr)
		}
		if _, werr := h.Write([]byte{0}); werr != nil {
			return "", nil, werr
		}
		files = append(files, rel)
	}
	if len(files) == 0 {
		return "", nil, nil
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), files, nil
}

// materializeResearchFindings writes findings.md from the provider Summary after a
// successful research run when the sandbox left no product files.
func materializeResearchFindings(wt, summary string, in ChildExecInput) error {
	summary = strings.TrimSpace(summary)
	if wt == "" || len(summary) < 80 {
		return fmt.Errorf("research summary too short to materialize findings")
	}
	if isExplicitClarificationOnly(summary) {
		return fmt.Errorf("research summary looks like clarification-only")
	}
	body := "# Research findings\n\n" +
		"Work item: " + strings.TrimSpace(in.WorkItemID) + "\n\n" +
		"Intent: " + strings.TrimSpace(in.Intent) + "\n\n" +
		"## Provider survey\n\n" + summary + "\n"
	if isExplicitClarificationOnly(body) {
		return fmt.Errorf("research summary looks like clarification-only")
	}
	return writeProductFileSecurely(wt, "findings.md", body, "research findings")
}

// materializeVerifierVerdict writes verdict.md from the provider Summary after a
// successful verify run when the sandbox left no product files (read-only soul).
func materializeVerifierVerdict(wt, summary string, in ChildExecInput) error {
	summary = strings.TrimSpace(summary)
	if wt == "" || len(summary) < 80 {
		return fmt.Errorf("verifier summary too short to materialize verdict")
	}
	if isExplicitClarificationOnly(summary) {
		return fmt.Errorf("verifier summary looks like clarification-only")
	}
	body := "# Verification verdict\n\n" +
		"Work item: " + strings.TrimSpace(in.WorkItemID) + "\n\n" +
		"Intent: " + strings.TrimSpace(in.Intent) + "\n\n" +
		"## Adversarial review\n\n" + summary + "\n"
	// Refuse if headers + body would still be clarification-only.
	if isExplicitClarificationOnly(body) {
		return fmt.Errorf("verifier summary looks like clarification-only")
	}
	return writeProductFileSecurely(wt, "verdict.md", body, "verifier verdict")
}

// materializeDocsNotes writes docs-notes.md from the provider Summary when docs
// child left no product files (summary-only path).
func materializeDocsNotes(wt, summary string, in ChildExecInput) error {
	summary = strings.TrimSpace(summary)
	if wt == "" || len(summary) < 80 {
		return fmt.Errorf("docs summary too short to materialize notes")
	}
	if isExplicitClarificationOnly(summary) {
		return fmt.Errorf("docs summary looks like clarification-only")
	}
	body := "# Documentation notes\n\n" +
		"Work item: " + strings.TrimSpace(in.WorkItemID) + "\n\n" +
		"Intent: " + strings.TrimSpace(in.Intent) + "\n\n" +
		"## Documentation\n\n" + summary + "\n"
	if isExplicitClarificationOnly(body) {
		return fmt.Errorf("docs summary looks like clarification-only")
	}
	return writeProductFileSecurely(wt, "docs-notes.md", body, "docs notes")
}

// writeProductFileSecurely writes leafName under worktree via 0600 temp + Rename
// (replaces symlink node, never follows). git add errors are returned.
func writeProductFileSecurely(wt, leafName, body, label string) error {
	leafName = filepath.Base(strings.TrimSpace(leafName))
	if leafName == "" || leafName == "." || leafName == ".." {
		return fmt.Errorf("%s: invalid leaf name", label)
	}
	wtAbs, err := filepath.Abs(wt)
	if err != nil {
		return fmt.Errorf("%s worktree abs: %w", label, err)
	}
	if st, err := os.Lstat(wtAbs); err != nil {
		return fmt.Errorf("%s worktree: %w", label, err)
	} else if !st.IsDir() {
		return fmt.Errorf("%s worktree is not a directory", label)
	}
	dest := filepath.Join(wtAbs, leafName)
	if err := requirePathUnderRoot(wtAbs, dest); err != nil {
		return fmt.Errorf("%s dest: %w", label, err)
	}
	if st, err := os.Lstat(dest); err == nil {
		if st.Mode()&os.ModeSymlink == 0 && !st.Mode().IsRegular() {
			return fmt.Errorf("%s dest is not a regular file or symlink (mode=%v)", label, st.Mode())
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("%s dest lstat: %w", label, err)
	}
	tmp, err := os.CreateTemp(wtAbs, "."+leafName+".*.tmp")
	if err != nil {
		return fmt.Errorf("%s temp create: %w", label, err)
	}
	tmpName := tmp.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if err := requirePathUnderRoot(wtAbs, tmpName); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%s temp path: %w", label, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%s temp chmod: %w", label, err)
	}
	if _, err := tmp.Write([]byte(body)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%s temp write: %w", label, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%s temp sync: %w", label, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%s temp close: %w", label, err)
	}
	if err := requireRegularNonSymlinkFile(tmpName); err != nil {
		return fmt.Errorf("%s temp: %w", label, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("%s rename: %w", label, err)
	}
	cleanupTmp = false
	if err := requirePathUnderRoot(wtAbs, dest); err != nil {
		return fmt.Errorf("%s post path: %w", label, err)
	}
	if err := requireRegularNonSymlinkFile(dest); err != nil {
		return fmt.Errorf("%s post: %w", label, err)
	}
	cmd := exec.Command("git", "-C", wtAbs, "add", "--", leafName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s git add: %w: %s", label, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// requirePathUnderRoot ensures candidate resolves under root (no escape via ..).
func requirePathUnderRoot(root, candidate string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	candAbs, err := filepath.Abs(candidate)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, candAbs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes worktree root %q", candAbs, rootAbs)
	}
	return nil
}

// requireRegularNonSymlinkFile uses Lstat only — never follows symlinks.
func requireRegularNonSymlinkFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink (refused)", path)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (mode=%v)", path, st.Mode())
	}
	return nil
}

func mergeUniquePaths(base, extra []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(base)+len(extra))
	for _, p := range append(append([]string{}, base...), extra...) {
		p = filepath.ToSlash(filepath.Clean(p))
		if p == "" || p == "." || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// detectWorktreeEscapes finds product files created under the durable project root
// but outside the assigned child worktree (common failure mode: provider writes to
// project root). Meta under runs/*/worktree is allowed.
func detectWorktreeEscapes(worktree string) []string {
	worktree = filepath.Clean(worktree)
	projectRoot := projectRootFromWorktree(worktree)
	if projectRoot == "" {
		return nil
	}
	var escaped []string
	_ = filepath.WalkDir(projectRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(worktree, path)
		if rerr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil // under worktree
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, ".") {
			return nil
		}
		if strings.Contains(path, string(filepath.Separator)+"runs"+string(filepath.Separator)) {
			return nil
		}
		// Skip durable meta dirs at project root.
		if base == "logs" || base == "tmp" || base == "recovery" {
			return nil
		}
		escaped = append(escaped, path)
		if len(escaped) >= 32 {
			return errors.New("stop")
		}
		return nil
	})
	return escaped
}

func projectRootFromWorktree(worktree string) string {
	runsDir := filepath.Dir(filepath.Dir(filepath.Clean(worktree))) // .../runs
	if filepath.Base(runsDir) != "runs" {
		return ""
	}
	return filepath.Dir(runsDir)
}

// dirSnap is a relative-path → content hash map for isolation snapshots.
type dirSnap map[string]string

func snapshotDirTree(root string) dirSnap {
	out := dirSnap{}
	root = strings.TrimSpace(root)
	if root == "" {
		return out
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return out
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		base := filepath.Base(path)
		// Never treat git internals as product mutations (shared objects/index
		// updates from child worktrees false-failed wi_tests on RC.17).
		if d.IsDir() {
			if base == ".git" || base == "runs" || base == "logs" || base == "tmp" || base == "recovery" {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip other child worktrees under runs/ so concurrent children don't
		// look like parent mutations.
		if strings.Contains(path, string(filepath.Separator)+"runs"+string(filepath.Separator)) {
			return nil
		}
		if base == "logs" || strings.HasPrefix(base, ".") {
			// Skip meta/dotfiles at snapshot roots (not child worktree product).
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			return nil
		}
		if rel == ".git" || strings.HasPrefix(filepath.ToSlash(rel), ".git/") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:8])
		return nil
	})
	return out
}

// diffDirTree returns new/changed paths in after not present (or different) in
// before, excluding anything under excludeRoot (the assigned worktree).
func diffDirTree(before, after dirSnap, excludeRoot string) []string {
	var mut []string
	excludeRoot = filepath.Clean(excludeRoot)
	for rel, h := range after {
		if before[rel] == h {
			continue
		}
		// Ignore git internals entirely.
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			continue
		}
		// If excludeRoot is under the snap root we can't map absolute paths here;
		// caller snapshots only parent/project roots, not the worktree tree.
		_ = excludeRoot
		mut = append(mut, rel)
	}
	return mut
}

// cleanupIsolationViolation removes escaped product files and restores parent
// / project roots to the pre-run snapshot (delete new files; never invent).
func cleanupIsolationViolation(escaped, parentMut, projectMut []string, parentSnap, projectSnap dirSnap, parentRoot, projectRoot string) {
	for _, p := range escaped {
		_ = os.RemoveAll(p)
	}
	// Delete newly created relative paths under parent/project roots.
	for _, rel := range parentMut {
		if parentSnap[rel] == "" {
			_ = os.RemoveAll(filepath.Join(parentRoot, filepath.FromSlash(rel)))
		}
	}
	for _, rel := range projectMut {
		if projectSnap[rel] == "" {
			_ = os.RemoveAll(filepath.Join(projectRoot, filepath.FromSlash(rel)))
		}
	}
}

func attachUsage(out ChildExecResult, res agent.Result) ChildExecResult {
	// Preserve raw token usage only. Token counts are NOT the same unit as
	// provider quota remaining fraction (Grok credits, weekly %, etc.).
	// Never reconcile/subtract tokens as capacityledger Actual.
	if res.Usage.InputTokens != nil {
		out.InputTokens = *res.Usage.InputTokens
	}
	if res.Usage.OutputTokens != nil {
		out.OutputTokens = *res.Usage.OutputTokens
	}
	// Explicit unknown for quota-window Actual — goalrun derives same-window
	// Before−After delta after ObserveAfter when identity matches.
	out.ActualSource = "unknown"
	out.ActualCapacity = nil
	return out
}

// worktreePathIfLeaked returns wt when cleanup failed (leak evidence), else "".
func worktreePathIfLeaked(wt string, cleanupErr error) string {
	if cleanupErr != nil {
		return wt
	}
	return ""
}

// releaseChildWorktree deregisters a child git worktree from the parent repo
// (when linked) and removes the worktree directory. Preserves underlying git/
// RemoveAll errors and verifies filesystem + git-registration absence.
// Exactly-once safe if path empty (nil error).
func releaseChildWorktree(repoPath, wtPath string) error {
	wtPath = strings.TrimSpace(wtPath)
	if wtPath == "" {
		return nil
	}
	var errs []string
	if rerr := releaseIntegrateWorktree(repoPath, wtPath); rerr != nil {
		errs = append(errs, rerr.Error())
	}
	// Filesystem agreement: path must be gone after release.
	if _, err := os.Stat(wtPath); err == nil {
		if rerr := os.RemoveAll(wtPath); rerr != nil {
			errs = append(errs, fmt.Sprintf("retry RemoveAll: %v", rerr))
		}
		if _, err2 := os.Stat(wtPath); err2 == nil {
			errs = append(errs, fmt.Sprintf("worktree path still present: %s", wtPath))
		}
	} else if !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("stat: %v", err))
	}
	// Git registration agreement when parent repo path exists. Absent parent is
	// plain-child only (not a registered worktree parent). Existing parent that
	// cannot be inspected fails closed.
	repoPath = strings.TrimSpace(repoPath)
	if repoPath != "" {
		if _, serr := os.Stat(repoPath); os.IsNotExist(serr) {
			// Parent path absent — nothing to list.
		} else if serr != nil {
			errs = append(errs, fmt.Sprintf("stat repo for list: %v", serr))
		} else if listed, lerr := gitWorktreeListContains(repoPath, wtPath); lerr != nil {
			errs = append(errs, fmt.Sprintf("worktree list: %v", lerr))
		} else if listed {
			errs = append(errs, fmt.Sprintf("worktree still registered: %s", wtPath))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("workflowrun: releaseChildWorktree: %s", strings.Join(errs, "; "))
}

// gitWorktreeListContains reports whether path appears in `git worktree list --porcelain`.
func gitWorktreeListContains(repoPath, wtPath string) (bool, error) {
	cmd := exec.Command("git", "-C", repoPath, "worktree", "list", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Caller ensures parent path exists. "Not a git repository" means path is
		// not a worktree parent (plain dir) — treat as not registered. Other errors
		// fail closed so a real registered parent cannot be silently skipped.
		msg := strings.ToLower(string(out) + err.Error())
		if strings.Contains(msg, "not a git repository") {
			return false, nil
		}
		return false, fmt.Errorf("%v: %s", err, out)
	}
	absWT, _ := filepath.Abs(wtPath)
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		absP, _ := filepath.Abs(p)
		if absP == absWT || p == wtPath {
			return true, nil
		}
	}
	return false, nil
}

func allocateChildWorktree(homeDir, projectID, graphID, workItemID, attemptID, repoPath, baseRef string) (string, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = "local-project"
	}
	var layout home.V09Layout
	var err error
	if homeDir != "" {
		// Tests/production may pass an explicit root; ensure owner-only base+project.
		if err := os.MkdirAll(homeDir, 0o700); err != nil {
			return "", err
		}
		_ = os.Chmod(homeDir, 0o700)
		layout, err = home.EnsureMinimumLayout(homeDir, projectID)
	} else {
		layout, err = home.ResolveV09(home.Deps{})
		if err == nil {
			err = layout.EnsureProject(projectID)
		}
	}
	if err != nil {
		return "", err
	}
	pdir, err := layout.ProjectDir(projectID)
	if err != nil {
		return "", err
	}
	runKey := short(graphID + "|" + workItemID + "|" + attemptID)
	root := filepath.Join(pdir, "runs", "wf_"+runKey, "worktree")
	// Isolated provider workspace: prefer a real git worktree/clone of the
	// parent repo so AG/Codex project context is the child path only.
	// baseRef is the shared goal branch after prior integrations (or HEAD).
	if err := materializeIsolatedGitWorkspace(root, repoPath, baseRef); err != nil {
		return "", err
	}
	_ = os.Chmod(root, 0o700)
	marker := fmt.Sprintf("graph=%s\nwork_item=%s\nattempt=%s\nrepo=%s\nbase=%s\n",
		graphID, workItemID, attemptID, strings.TrimSpace(repoPath), strings.TrimSpace(baseRef))
	if err := os.WriteFile(filepath.Join(root, ".loopcoder-owned-worktree"), []byte(marker), 0o600); err != nil {
		return "", err
	}
	return root, nil
}

// providerControlPlaneLogPath keeps provider runtime artifacts outside the
// provider-writable product worktree. Codex and other adapters derive their
// prompt, summary, and schema paths from this log directory, so the provider
// cannot delete or spoof its own authoritative metadata with ordinary
// workspace-write access.
func providerControlPlaneLogPath(homeDir, projectID, runID, attemptID string) (string, error) {
	projectID = strings.TrimSpace(projectID)
	runID = strings.TrimSpace(runID)
	attemptID = strings.TrimSpace(attemptID)
	if projectID == "" || runID == "" || attemptID == "" {
		return "", fmt.Errorf("workflowrun: project_id, run_id, and attempt_id required for provider control plane")
	}
	root, err := ResolveDurableHome(homeDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("workflowrun: create durable home: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workflowrun: resolve durable home: %w", err)
	}
	controlRoot := filepath.Join(root, "provider-control")
	dir := filepath.Join(controlRoot,
		controlPlaneSegment(projectID),
		controlPlaneSegment(runID),
		controlPlaneSegment(attemptID))
	if err := requirePathUnderRoot(controlRoot, dir); err != nil {
		return "", fmt.Errorf("workflowrun: provider control plane path: %w", err)
	}
	current := root
	for _, segment := range []string{
		"provider-control",
		controlPlaneSegment(projectID),
		controlPlaneSegment(runID),
		controlPlaneSegment(attemptID),
	} {
		current = filepath.Join(current, segment)
		if err := os.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
			return "", fmt.Errorf("workflowrun: create provider control plane: %w", err)
		}
		st, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("workflowrun: inspect provider control plane: %w", err)
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return "", fmt.Errorf("workflowrun: provider control plane component is not a non-symlink directory")
		}
		if err := os.Chmod(current, 0o700); err != nil {
			return "", fmt.Errorf("workflowrun: secure provider control plane: %w", err)
		}
	}
	return filepath.Join(dir, "provider.log"), nil
}

func controlPlaneSegment(value string) string {
	return sanitizeBranch(value) + "-" + short("provider-control|"+value)
}

// materializeIsolatedGitWorkspace creates an exclusive git checkout at dest
// from repoPath@baseRef (git worktree when possible, else clone, else empty git init).
// baseRef empty → HEAD. The provider must treat dest as its sole project context.
func materializeIsolatedGitWorkspace(dest, repoPath, baseRef string) error {
	if err := os.RemoveAll(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	repoPath = strings.TrimSpace(repoPath)
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" {
		baseRef = "HEAD"
	}
	if repoPath != "" {
		if abs, aerr := filepath.Abs(repoPath); aerr == nil {
			repoPath = abs
		}
		if st, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil && (st.IsDir() || st.Mode().IsRegular()) {
			// Prefer linked worktree at goal-branch / baseRef so prior integrations are visible.
			cmd := exec.Command("git", "-C", repoPath, "worktree", "add", "--detach", dest, baseRef)
			if out, err := cmd.CombinedOutput(); err == nil {
				return nil
			} else {
				// Fall back to local clone + checkout baseRef.
				_ = out
				cmd2 := exec.Command("git", "clone", "--local", "--no-hardlinks", repoPath, dest)
				if out2, err2 := cmd2.CombinedOutput(); err2 == nil {
					co := exec.Command("git", "-C", dest, "checkout", baseRef)
					if _, err3 := co.CombinedOutput(); err3 != nil {
						// keep clone HEAD if baseRef missing
						_ = err3
					}
					return nil
				} else {
					_ = out2
				}
			}
		}
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	// Empty isolated git project so providers still have a repo identity.
	init := exec.Command("git", "init")
	init.Dir = dest
	_, _ = init.CombinedOutput()
	return nil
}

// writeFakeProductFiles writes integrateable product content for tests.
func writeFakeProductFiles(wt string, in ChildExecInput, override map[string][]string) []string {
	var paths []string
	if override != nil {
		if list, ok := override[in.WorkItemID]; ok {
			paths = list
		}
	}
	role := ClassifyTaskRole(in.WorkItemID, in.Intent, "")
	if len(paths) == 0 {
		// Default product path per child — enough for integrate + acceptance tests.
		switch role {
		case RoleTests:
			paths = []string{"notes/notes_test.go", "notes/notes.go"}
		case RoleImplement:
			paths = []string{"notes/notes.go"}
		case RoleDocs:
			paths = []string{"docs-notes.md"}
		case RoleVerify:
			paths = []string{"verdict.md"}
		case RoleResearch:
			paths = []string{"findings.md"}
		default:
			if strings.Contains(strings.ToLower(in.WorkItemID), "test") || strings.Contains(strings.ToLower(in.Intent), "test") {
				paths = []string{"notes/notes_test.go", "notes/notes.go"}
			} else if strings.Contains(strings.ToLower(in.WorkItemID), "implement") || strings.Contains(strings.ToLower(in.Intent), "implement") {
				paths = []string{"notes/notes.go"}
			} else if strings.Contains(strings.ToLower(in.WorkItemID), "doc") {
				paths = []string{"docs-notes.md"}
			} else {
				paths = []string{"notes/" + in.WorkItemID + ".md"}
			}
		}
	}
	written := make([]string, 0, len(paths))
	for _, rel := range paths {
		rel = filepath.Clean(rel)
		abs := filepath.Join(wt, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			continue
		}
		body := fmt.Sprintf("// product file for %s attempt=%s\npackage notes\n\n// Intent: %s\n",
			in.WorkItemID, in.AttemptID, in.Intent)
		if strings.HasSuffix(rel, "_test.go") {
			body = fmt.Sprintf("package notes\n\nimport \"testing\"\n\nfunc TestNotes_%s(t *testing.T) {\n\t// generated for attempt %s\n}\n",
				strings.ReplaceAll(in.WorkItemID, "-", "_"), in.AttemptID)
		} else if filepath.Base(rel) == "findings.md" || (role == RoleResearch && strings.HasSuffix(rel, ".md")) {
			// Substantial survey body so AcceptSucceededChild RoleResearch passes.
			body = fmt.Sprintf("# Research findings\n\nWork item: %s\n\n## Provider survey\n\n"+
				"Survey scope and constraints for attempt %s.\n"+
				"Intent: %s\n"+
				"Fake fixture survey covers multi-provider notes package layout, capacity routing, and test plan.\n"+
				"Residual risks: fixture-only path; production uses provider Summary materialization.\n",
				in.WorkItemID, in.AttemptID, in.Intent)
		} else if filepath.Base(rel) == "verdict.md" || (role == RoleVerify && strings.HasSuffix(rel, ".md")) {
			body = fmt.Sprintf("# Verification verdict\n\nWork item: %s\n\n## Adversarial review\n\n"+
				"Independent review of attempt %s for intent: %s\n"+
				"Findings: fixture product is consistent with goal graph contracts; residual risk low for tests.\n"+
				"Recommendation: proceed with integration under human gate.\n",
				in.WorkItemID, in.AttemptID, in.Intent)
		} else if filepath.Base(rel) == "docs-notes.md" || (role == RoleDocs && strings.HasSuffix(rel, ".md")) {
			body = fmt.Sprintf("# Documentation notes\n\nWork item: %s\n\n## Documentation\n\n"+
				"User-facing notes for attempt %s.\nIntent: %s\n"+
				"Describe package API, configuration, and multi-provider selection for operators.\n",
				in.WorkItemID, in.AttemptID, in.Intent)
		} else if strings.HasSuffix(rel, ".md") {
			body = fmt.Sprintf("# %s\n\nAttempt: %s\nIntent: %s\n\n"+
				"Substantial markdown product for fixture acceptance and integrate tests.\n"+
				"Scope notes, constraints, and residual documentation risks are captured here.\n",
				in.WorkItemID, in.AttemptID, in.Intent)
		} else if strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") {
			body = fmt.Sprintf("package notes\n\n// WorkItem %s attempt %s\nfunc Notes() string { return %q }\n",
				in.WorkItemID, in.AttemptID, in.Intent)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			continue
		}
		// Stage in child worktree so discoverProductFiles via porcelain sees it.
		_ = exec.Command("git", "-C", wt, "add", "--", rel).Run()
		written = append(written, rel)
	}
	return written
}

func writeChildEvidence(wt string, in ChildExecInput, kind string, at time.Time) (path, digest string, files []string, err error) {
	evDir := filepath.Join(wt, ".loopcoder", "child-evidence")
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		return "", "", nil, err
	}
	payload := map[string]any{
		"schema":       "loopcoder.child.evidence.v1",
		"kind":         kind,
		"project_id":   in.ProjectID,
		"graph_id":     in.GraphID,
		"work_item_id": in.WorkItemID,
		"claim_id":     in.ClaimID,
		"attempt_id":   in.AttemptID,
		"intent":       in.Intent,
		"provider":     in.Route.Provider,
		"model":        in.Route.Model,
		"depth":        in.Route.Depth,
		"account_ref":  in.Route.AccountRef,
		"route_reason": in.Route.RouteReason,
		"recorded_at":  at.UTC().Format(time.RFC3339Nano),
		// Explicit: LoopCoder-owned; not a provider-native subagent.
		"ownership":          "loopcoder_work_item",
		"native_delegation":  false,
		"disable_delegation": true,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", "", nil, err
	}
	path = filepath.Join(evDir, in.WorkItemID+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", "", nil, err
	}
	// Also drop a human-readable findings stub for research/integration paths.
	stubName := "child-output-" + in.WorkItemID + ".md"
	stub := fmt.Sprintf("# Child %s\n\nIntent: %s\nRoute: %s/%s depth=%s\nKind: %s\n",
		in.WorkItemID, in.Intent, in.Route.Provider, in.Route.Model, in.Route.Depth, kind)
	stubPath := filepath.Join(wt, stubName)
	if err := os.WriteFile(stubPath, []byte(stub), 0o600); err != nil {
		return "", "", nil, err
	}
	// Stage stub so git porcelain discovery sees it (not base tree walk).
	_ = exec.Command("git", "-C", wt, "add", "--", stubName).Run()
	sum := sha256.Sum256(raw)
	digest = "sha256:" + hex.EncodeToString(sum[:])
	files = []string{
		filepath.Join(".loopcoder", "child-evidence", in.WorkItemID+".json"),
		stubName,
		".loopcoder-owned-worktree",
	}
	return path, digest, files, nil
}

func firstNonEmpty(vals ...string) string {
	for _, a := range vals {
		if strings.TrimSpace(a) != "" {
			return a
		}
	}
	return ""
}

func isModelUnavailableResult(res agent.Result, err error) bool {
	if strings.EqualFold(strings.TrimSpace(res.FailureClass), "model_unavailable") {
		return true
	}
	blob := strings.ToLower(res.Summary)
	if err != nil {
		blob += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(blob, "invalid model selection") ||
		strings.Contains(blob, "model_unavailable") ||
		strings.Contains(blob, "not recognized as a known model")
}
