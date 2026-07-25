package workflowrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/routecontract"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	"github.com/jasonhnd/loopcoder/internal/waveschedule"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// Ensure home is wired only through ResolveDurableHome (no PID temp).

const (
	StatusHumanGate = "human_gate"
	StatusBlocked   = "workflow_blocked"
	StatusInvalid   = "workflow_invalid"
)

// Request executes one bounded workflow.
type Request struct {
	ProjectID string
	// RunID uniquely namespaces this execution (attempts, worktrees). Empty → generated.
	// Prevents stale/cross-run reuse of attempt IDs and durable worktree paths.
	// On resume, pass the same RunID so attempt IDs stay stable (exactly-once).
	RunID string
	// ExpectedPlanDigest is the canonical ExecutionPlanDigest (workflowdef.Normalize
	// digest) from the parent (goalrun). Required nonempty; must equal local
	// Normalize(def).Digest before materialize/claim. Fail closed on empty or mismatch.
	ExpectedPlanDigest string
	// ExpectedGraphDigest is the canonical post-materialize workgraph.DigestGraph
	// from the parent (goalrun). Required nonempty; must equal local materialize
	// DigestGraph before claim. Fail closed on empty or mismatch — never silently
	// prefer either side.
	ExpectedGraphDigest string
	// Definition is the frozen user graph (JSON-serializable).
	Definition workflowdef.Definition
	// Actor is the approving owner identity (required for materialize).
	Actor string
	// Provider/Model optional default child route pin when ChildRoutes lacks an item.
	Provider string
	Model    string
	// ChildRoutes optional per-work-item routes (goalrun auto-route winners).
	ChildRoutes map[string]ChildRoute
	// SameDepthAlternates are production decision-set candidates (fresh
	// machine-readable, hard-eligible) keyed by work item. Used only after typed
	// model_unavailable to claim a distinct attempt generation — never reopen the
	// closed failed attempt.
	SameDepthAlternates map[string][]AlternateCandidate
	// CapacityReroute optional hook: release failed reservation + reserve alternate
	// under the new attempt id (goalrun ledger wiring).
	CapacityReroute CapacityRerouteHook
	// RepoPath optional base repo for child worktrees and goal-branch integration.
	RepoPath string
	// BaseRef is the starting point for the shared goal branch (default main).
	BaseRef string
	// GoalBranch is the shared branch all succeeded children integrate into.
	// Empty → loopcoder/goal-<runID>. Required for product PR (not receipt-only).
	GoalBranch string
	// SkipIntegrate disables product-branch integration (tests without a git repo).
	// Production with RepoPath set always integrates on success.
	SkipIntegrate bool
	// MaxWaves hard cap (default 32).
	MaxWaves int
	// PriorSucceeded seeds already-terminal succeeded children from a prior
	// interrupted run. Same attempt_id is reused; executor is NOT re-invoked
	// (exactly-once provider call / file / capacity).
	PriorSucceeded map[string]ChildOutcome
	// PriorOutcomes is the complete validated durable attempt set from resume
	// preflight (checkpoint/partial WorkflowKids cross-bound to the event log
	// BEFORE inventory/ledger/claim/launch). Every row is re-validated in pure
	// preflight; out.Children is seeded with this set so writePartialPrior
	// persists the full history without reading on-disk partial. Only
	// terminal=succeeded rows with evidence may also appear in PriorSucceeded
	// for scheduling reuse. Non-success historical rows never re-exec.
	PriorOutcomes []ChildOutcome
	// AttemptGeneration bumps attempt IDs for aborted/in-flight children on resume
	// (completed children stay generation 0 / prior attempt). Key = work item id.
	AttemptGeneration map[string]int
	// Integrator injects branch integration (tests). nil → GitBranchIntegrator
	// when RepoPath is set and SkipIntegrate is false.
	Integrator BranchIntegrator
	// EventLogPath optional pre-opened path; empty → open under HomeDir/project/run.
	EventLogPath string
}

// AlternateCandidate is one same-depth hard-eligible route for model_unavailable
// generation-safe reroute. Effort must match the failed attempt's required depth.
// AccountRef/WindowKind bind the exact capacity identity used at reserve time.
type AlternateCandidate struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Permission   string `json:"permission,omitempty"`
	AccountRef   string `json:"account_ref,omitempty"`
	InstallRef   string `json:"install_ref,omitempty"`
	WindowKind   string `json:"window_kind,omitempty"`
	HardEligible bool   `json:"hard_eligible"`
	SoftExcluded bool   `json:"soft_excluded"`
}

// CapacityRerouteInput binds failed → alternate capacity transfer (no double hold).
// Call only after the new attempt has been claimed under NewAttemptID.
// Every prior and alternate identity dimension is required nonempty exact.
type CapacityRerouteInput struct {
	WorkItemID       string
	FailedAttemptID  string // workflow attempt id (audit)
	PriorHoldAttempt string // ledger attempt id for prior reservation
	NewAttemptID     string
	// Product execution identity for alternate reserve (same contract as failed).
	PlanDigest          string
	GraphDigest         string
	TaskClass           string
	ChildContractDigest string
	// Failed route identity (exact prior).
	FailedProvider      string
	FailedModel         string
	FailedDepth         string
	FailedPermission    string
	FailedAccountRef    string
	FailedInstallRef    string
	FailedWindowKind    string
	FailedReservationID string
	// Alternate selected route identity (exact).
	AltProvider   string
	AltModel      string
	AltDepth      string
	AltPermission string
	AltAccountRef string
	AltInstallRef string
	AltWindowKind string
	// Depth is the required invocation depth (must equal AltDepth).
	Depth       string
	Permission  string // required permission class for this child
	RouteReason string
	// PriorActual/Source: when known from failed child, reconcile; else honest release.
	PriorActual     *float64
	PriorSource     string
	SupersedesEvent string
}

// CapacityTransition is one durable ledger transition for a single attempt.
// Canary proof must use these records — never prose CapacityNote inference.
// Provider/Model/Depth/AccountRef/WindowKind bind exact selected route capacity identity.
type CapacityTransition struct {
	AttemptID  string `json:"attempt_id"`
	Role       string `json:"role"`  // prior|alternate
	State      string `json:"state"` // released|reconciled|reserved
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Depth      string `json:"depth,omitempty"`
	Permission string `json:"permission,omitempty"`
	AccountRef string `json:"account_ref,omitempty"`
	// InstallRef binds exact install observation (required on finalized MU transitions).
	InstallRef    string `json:"install_ref,omitempty"`
	WindowKind    string `json:"window_kind,omitempty"`
	ReservationID string `json:"reservation_id,omitempty"`
	// Before / Reserved / After are ledger remaining fractions for this attempt.
	Before   float64  `json:"capacity_before,omitempty"`
	Reserved float64  `json:"capacity_reserved,omitempty"`
	Actual   *float64 `json:"actual,omitempty"` // nil = honest unknown
	After    *float64 `json:"capacity_after,omitempty"`
	// Source is ActualSource (never prose). Empty when Actual is nil.
	Source string `json:"source,omitempty"`
	// Structured before-window evidence (exact|estimated|unknown + freshness + reset).
	BeforeSource     string     `json:"before_source,omitempty"`
	BeforeCapturedAt *time.Time `json:"before_captured_at,omitempty"`
	BeforeFreshness  string     `json:"before_freshness,omitempty"`
	BeforeConfidence string     `json:"before_confidence,omitempty"`
	ResetAt          *time.Time `json:"reset_at,omitempty"`
	// Structured after evidence (observed|derived); derived never greens runtime freshness.
	AfterSource      string     `json:"after_source,omitempty"`
	AfterObservedAt  *time.Time `json:"after_observed_at,omitempty"`
	AfterFreshness   string     `json:"after_freshness,omitempty"`
	AfterConfidence  string     `json:"after_confidence,omitempty"`
	AfterState       string     `json:"after_state,omitempty"`
	ActualConfidence string     `json:"actual_confidence,omitempty"`
}

// CapacityRerouteResult binds the alternate reservation (account must not cross companies)
// and durable prior/alternate transitions for canary proof.
type CapacityRerouteResult struct {
	AccountRef          string
	WindowKind          string
	PriorState          string // released|reconciled
	AlternateState      string // reserved
	ReservationID       string
	PriorTransition     CapacityTransition
	AlternateTransition CapacityTransition
}

// CapacityRerouteHook transfers capacity after a new attempt claim.
// Must not invent capacity or double-hold. Returns alternate account_ref.
// CompensateAlternateHold releases/reconciles the alternate hold on post-transfer
// pre-launch failures (event append, etc.) so no live hold remains.
type CapacityRerouteHook interface {
	OnModelUnavailableAlternate(in CapacityRerouteInput) (CapacityRerouteResult, error)
	CompensateAlternateHold(newAttemptID string) error
}

// ChildOutcome is per-child terminal + capacity-relevant evidence for reports.
type ChildOutcome struct {
	WorkItemID string `json:"work_item_id"`
	// TaskClass is the canonical classified capability floor (luna|tera|soul).
	// Empty on legacy records — not reusable without Gate 2A-2 acceptance.
	TaskClass string `json:"task_class,omitempty"`
	// ExecutionPlanDigest is the canonical workflowdef.Normalize digest.
	ExecutionPlanDigest string `json:"execution_plan_digest,omitempty"`
	// ChildContractDigest binds plan + work_item + class/depth/permission + output_contract.
	ChildContractDigest string `json:"child_contract_digest,omitempty"`
	// Generation is the positive claim generation (≥1) for this attempt.
	Generation     int      `json:"generation,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	Model          string   `json:"model,omitempty"`
	Depth          string   `json:"depth,omitempty"`
	Permission     string   `json:"permission,omitempty"`
	AccountRef     string   `json:"account_ref,omitempty"`
	InstallRef     string   `json:"install_ref,omitempty"`
	WindowKind     string   `json:"window_kind,omitempty"`
	ReservationID  string   `json:"reservation_id,omitempty"`
	RouteReason    string   `json:"route_reason,omitempty"`
	Terminal       string   `json:"terminal,omitempty"`
	OutputEvidence string   `json:"output_evidence,omitempty"`
	WorktreePath   string   `json:"worktree_path,omitempty"`
	AttemptID      string   `json:"attempt_id,omitempty"`
	ExitCode       int      `json:"exit_code,omitempty"`
	FailureClass   string   `json:"failure_class,omitempty"`
	Message        string   `json:"message,omitempty"`
	ActualCapacity *float64 `json:"actual_capacity,omitempty"`
	// ActualSource is capacity fraction source (same_window_delta only when known).
	// Distinct from ActualSources for route dimension proof. Token estimates never
	// set ActualCapacity (preserve InputTokens/OutputTokens separately).
	ActualSource string `json:"actual_source,omitempty"`
	// Raw token usage (not quota-window fraction unit).
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// ActualSources is per-dimension route proof class:
	// provider_stream|accepted_invocation|auth_binding|install_binding|unknown.
	// accepted_invocation is never collapsed into provider_stream.
	ActualSources ActualRouteSources `json:"actual_sources,omitempty"`
	// ArgvDigest is redacted exact launched argv fingerprint when known.
	ArgvDigest   string   `json:"argv_digest,omitempty"`
	FilesTouched []string `json:"files_touched,omitempty"`
	// IntegrateCommitSHA is the goal-branch commit that absorbed this child (if any).
	IntegrateCommitSHA string `json:"integrate_commit_sha,omitempty"`
	// SupersedesAttemptID when this outcome is a generation-safe alternate.
	SupersedesAttemptID string `json:"supersedes_attempt_id,omitempty"`
	// RerouteEventRef binds durable model_unavailable → reroute event evidence.
	RerouteEventRef string `json:"reroute_event_ref,omitempty"`
}

// ActualRouteSources is the per-dimension evidence class for Actual* route fields.
type ActualRouteSources struct {
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Permission string `json:"permission,omitempty"`
	Account    string `json:"account,omitempty"`
	Install    string `json:"install,omitempty"`
}

// Result is durable parent evidence.
type Result struct {
	Status  string
	Message string
	GraphID string
	// PlanDigest is the canonical ExecutionPlanDigest (workflowdef.Normalize).
	PlanDigest string
	// GraphDigest is the separate workgraph.DigestGraph identity (not attempt key).
	GraphDigest    string
	RunID          string // execution namespace (unique per parent launch)
	GraphVersion   int
	ClaimCount     int
	LaunchCount    int // child launches (== claims on success path)
	ReuseCount     int // prior-succeeded children reused without re-exec
	Integrated     []string
	Children       []ChildOutcome
	Events         []string
	DirectRunEquiv bool
	AutoMerge      bool
	Error          string
	// Ceilings are best-effort peaks observed during this process (restart evidence).
	WorktreePeak int `json:"worktree_peak,omitempty"`
	ProcessPeak  int `json:"process_peak,omitempty"`
	// Active occupancy at return (must be 0 after every attempt completes/errors).
	WorktreeActive int `json:"worktree_active,omitempty"`
	ProcessActive  int `json:"process_active,omitempty"`
	// GoalBranch is the shared product branch (child integrations land here).
	GoalBranch string `json:"goal_branch,omitempty"`
	// IntegrateCommits are exactly-once product commits onto GoalBranch.
	IntegrateCommits []IntegrateCommit `json:"integrate_commits,omitempty"`
	// EventLogPath is the append-only raw event JSONL for interrupt evidence.
	EventLogPath string `json:"event_log_path,omitempty"`
	// CapacityTransitions are durable per-attempt ledger transitions from
	// model_unavailable capacity transfer (prior + alternate). Canary proof only.
	CapacityTransitions []CapacityTransition `json:"capacity_transitions,omitempty"`
	// Interrupted true when a forced interrupt was recorded this process.
	Interrupted bool `json:"interrupted,omitempty"`
	// AbortedAttempts maps work_item_id → aborted attempt_id (must get new gen on resume).
	AbortedAttempts map[string]string `json:"aborted_attempts,omitempty"`
}

// Service runs bounded workflows.
type Service struct {
	Now func() time.Time
	// Executor runs claimed children. nil → DefaultChildExecutor (production real path).
	// Focused tests inject FakeChildExecutor; remote acceptance must use production.
	Executor ChildExecutor
	// HomeDir is the durable LoopCoder home root for event log + claim store +
	// worktrees. Empty → ResolveDurableHome (LOOPCODER_HOME / ~/.loopcoder).
	// Tests MUST inject t.TempDir() — never rely on process PID isolation.
	HomeDir string
	// TestConfigureEventLog optional hook applied after OpenEventLog (tests only).
	// Used to inject FailAppendKind etc. without production surface.
	TestConfigureEventLog func(*EventLog)
	// TestCrashAfterTerminal when non-nil is invoked after durable terminal
	// append and before claim Close. Returning an error aborts without close
	// (simulates crash window for recovery tests). Tests only.
	TestCrashAfterTerminal func(attemptID, terminal string) error
	// TestAfterPIDEvent is invoked after a successful spawn-time pid append
	// (tests only). Used for deterministic cancel/handshake without sleeps.
	TestAfterPIDEvent func(Event)
	// TestAfterGuardianReady is invoked from Guardian.OnStart after guardian is
	// ready (tests only). Ready markers for hard-restart MUST use this, not
	// TestAfterPIDEvent (guardian starts after provider OnStart).
	TestAfterGuardianReady func(ps ProcessStart, guardianPID int, diagnosticPath string)
	// TestRecoverFailAfter injects crash-window failpoints into authoritative
	// recovery: interrupt|terminal|claim|authority (tests only).
	TestRecoverFailAfter string
	// TestReleaseWorktree optional override for child worktree release (tests only).
	// nil → releaseChildWorktree. Returning non-nil keeps the lease active and
	// surfaces cleanup failure (blocks human_gate / alternate / success).
	TestReleaseWorktree func(repoPath, wtPath string) error
}

// Execute freezes, materializes, claims, runs LoopCoder-owned children, integrates;
// never auto-merges. Children are executed via ChildExecutor (production default
// calls the routed provider in an exclusive worktree — never fake Claim→Close).
func (s Service) Execute(ctx context.Context, req Request) (Result, error) {
	now := s.Now
	if now == nil {
		now = time.Now
	}
	t0 := now().UTC()
	out := Result{AutoMerge: false}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		// Unique per parent launch — never reuse att-* across disposable canaries.
		runID = "run_" + short(fmt.Sprintf("%s|%d|%s", req.ProjectID, t0.UnixNano(), req.Actor))
	}
	out.RunID = runID
	emit := func(e string) { out.Events = append(out.Events, e) }

	if ctx.Err() != nil {
		return fail(out, StatusBlocked, "context cancelled")
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		projectID = "local-project"
	}
	// Shared goal branch name is computed here; EnsureGoalBranch runs only after
	// pure preflight succeeds (zero git/event/claim side effects until then).
	goalBranch := strings.TrimSpace(req.GoalBranch)
	if goalBranch == "" {
		goalBranch = "loopcoder/goal-" + sanitizeBranch(runID)
	}
	out.GoalBranch = goalBranch
	baseRef := strings.TrimSpace(req.BaseRef)
	if baseRef == "" {
		baseRef = "main"
	}

	// =====================================================================
	// PURE PREFLIGHT — freeze and validate the complete execution envelope
	// before any integrator/git, eventlog, claim, or provider side effect.
	// =====================================================================
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "owner"
	}
	// Production: never invent fixture defaults on the real ProductionChildExecutor
	// without AllowFixture. FakeChildExecutor is an explicit test injection and
	// may use fixture pins. Empty default is OK when ChildRoutes supplies all pins.
	defaultProvider := strings.TrimSpace(req.Provider)
	defaultModel := strings.TrimSpace(req.Model)
	hasChildRoutes := len(req.ChildRoutes) > 0
	allowFixture := false
	if pe, ok := s.Executor.(ProductionChildExecutor); ok {
		allowFixture = pe.AllowFixture
	}
	if pe, ok := s.Executor.(*ProductionChildExecutor); ok {
		allowFixture = pe.AllowFixture
	}
	// FakeChildExecutor is always an explicit test injection.
	switch s.Executor.(type) {
	case FakeChildExecutor, *FakeChildExecutor:
		allowFixture = true
		// Test convenience: empty default pin → fixture when using FakeChildExecutor.
		if defaultProvider == "" {
			defaultProvider = "fixture"
		}
		if defaultModel == "" {
			defaultModel = "fixture-model"
		}
	}
	if strings.EqualFold(defaultProvider, "auto") || strings.EqualFold(defaultModel, "auto") {
		return fail(out, StatusInvalid, "auto provider/model not resolved (fail closed; no fixture_local success)")
	}
	if !hasChildRoutes && (defaultProvider == "" || defaultModel == "") {
		return fail(out, StatusInvalid, "provider and model required when ChildRoutes empty (no fixture production default)")
	}
	if (strings.EqualFold(defaultProvider, "fixture") || strings.EqualFold(defaultModel, "fixture-model")) && !allowFixture {
		// Production executor without AllowFixture, or nil executor → fail closed.
		if s.Executor == nil {
			return fail(out, StatusInvalid, "fixture provider requires explicit test executor with AllowFixture")
		}
		if _, isProd := s.Executor.(ProductionChildExecutor); isProd {
			return fail(out, StatusInvalid, "fixture provider requires AllowFixture test mode")
		}
		if _, isProd := s.Executor.(*ProductionChildExecutor); isProd {
			return fail(out, StatusInvalid, "fixture provider requires AllowFixture test mode")
		}
	}
	maxWaves := req.MaxWaves
	if maxWaves <= 0 {
		maxWaves = 32
	}

	exec := s.Executor
	if exec == nil {
		exec = ProductionChildExecutor{HomeDir: s.HomeDir, Now: now}
	}

	def := req.Definition
	if def.SchemaVersion == 0 {
		def.SchemaVersion = 1
	}
	if strings.TrimSpace(def.Source) == "" {
		def.Source = "explicit_definition"
	}

	// --- freeze plan (no side effects) ---
	plan, err := workflowdef.Normalize(def)
	if err != nil {
		return fail(out, StatusInvalid, "normalize: "+err.Error())
	}
	// Canonical ExecutionPlanDigest: require parent expected digest and match.
	expected := strings.TrimSpace(req.ExpectedPlanDigest)
	if expected == "" {
		return fail(out, StatusInvalid, "expected_plan_digest required (canonical ExecutionPlanDigest; no silent local-only plan)")
	}
	if expected != plan.Digest {
		return fail(out, StatusInvalid, fmt.Sprintf(
			"execution plan digest mismatch: expected=%s local=%s (zero claim/reserve/launch)",
			short(expected), short(plan.Digest)))
	}
	out.PlanDigest = plan.Digest
	emit("plan.ok:" + short(plan.Digest))

	// Prevalidate every Definition item + resolved ChildRoute BEFORE materialize/claim.
	// Build assignment-time ChildContractDigest (never post-exec from InvokedRoute).
	var contracts map[string]childContract
	var cerr error
	contracts, cerr = buildChildContracts(def, plan.Digest, req.ChildRoutes)
	if cerr != nil {
		return fail(out, StatusInvalid, "child contract prevalidate: "+cerr.Error())
	}

	// --- approve + materialize (in-memory only; no durable side effects) ---
	ap, err := workflowdef.Approve(plan.Digest, actor, "bounded workflow run", t0)
	if err != nil {
		return fail(out, StatusBlocked, "approve: "+err.Error())
	}
	reg := workflowdef.NewRegistry()
	mat, err := reg.Materialize(projectID, def, ap, t0)
	if err != nil {
		// invalid/cyclic/oversized must create zero claims
		return fail(out, StatusInvalid, "materialize: "+err.Error())
	}
	g := mat.Graph
	out.GraphID = g.GraphID
	out.GraphVersion = g.Version
	// Canonical GraphDigest = approved materialized graph only (never decompose-only).
	localGraphDigest := workgraph.DigestGraph(g)
	if localGraphDigest == "" {
		return fail(out, StatusInvalid, "empty graph digest after materialize")
	}
	if stored := strings.TrimSpace(g.PlanDigest); stored != "" && stored != localGraphDigest {
		return fail(out, StatusInvalid, fmt.Sprintf(
			"materialize graph PlanDigest inconsistent with DigestGraph: stored=%s computed=%s",
			short(stored), short(localGraphDigest)))
	}
	expectedGraph := strings.TrimSpace(req.ExpectedGraphDigest)
	if expectedGraph == "" {
		return fail(out, StatusInvalid, "expected_graph_digest required (canonical post-materialize GraphDigest; no silent local-only graph)")
	}
	if expectedGraph != localGraphDigest {
		return fail(out, StatusInvalid, fmt.Sprintf(
			"graph digest mismatch: expected=%s local=%s (zero claim/reserve/launch)",
			short(expectedGraph), short(localGraphDigest)))
	}
	out.GraphDigest = localGraphDigest
	out.DirectRunEquiv = g.DirectRunEquivalent
	emit(fmt.Sprintf("materialize.ok items=%d equiv=%v graph=%s", len(g.Items), g.DirectRunEquivalent, short(localGraphDigest)))

	itemByID := map[string]workgraph.WorkItem{}
	for _, it := range g.Items {
		itemByID[it.ID] = it
	}

	// Entire PriorSucceeded + PriorOutcomes + AttemptGeneration validated in pure
	// preflight (not lazily when a wave reaches an item). Present-invalid → fail closed.
	if err := validateEntirePriorSucceededMap(req.PriorSucceeded, itemByID, contracts, out.PlanDigest, runID); err != nil {
		return fail(out, StatusBlocked, err.Error())
	}
	if err := validateEntirePriorOutcomes(req.PriorOutcomes, itemByID, contracts, out.PlanDigest, runID); err != nil {
		return fail(out, StatusBlocked, err.Error())
	}
	// PriorSucceeded must be a subset of PriorOutcomes when both present (no
	// succeeded reuse identity outside the full attempt set).
	if err := requirePriorSucceededSubsetOfOutcomes(req.PriorSucceeded, req.PriorOutcomes); err != nil {
		return fail(out, StatusBlocked, err.Error())
	}
	if err := validateEntireAttemptGenerationMap(req.AttemptGeneration, itemByID); err != nil {
		return fail(out, StatusBlocked, err.Error())
	}
	// Seed out.Children with the full validated historical attempt set BEFORE
	// any side effect so every writePartialPrior sees complete history.
	if len(req.PriorOutcomes) > 0 {
		out.Children = append([]ChildOutcome(nil), req.PriorOutcomes...)
	}

	// Item set for plan-bound child event identity at write and recovery boundaries.
	itemOK := map[string]bool{}
	for id := range itemByID {
		itemOK[id] = true
	}

	// Existing event log: read-only validate against current graph/plan/run
	// IMMEDIATELY after pure preflight and BEFORE EnsureGoalBranch or any other
	// side effect. Invalid log remains byte-for-byte unchanged (zero git/claim/exec).
	if logPath, lperr := eventLogPathIfExists(s.HomeDir, projectID, runID); lperr != nil {
		return fail(out, StatusBlocked, "event log path: "+lperr.Error())
	} else if logPath != "" {
		raw, rerr := os.ReadFile(logPath)
		if rerr != nil {
			return fail(out, StatusBlocked, "event log pre-recovery read: "+rerr.Error())
		}
		preEvents, perr := ParseEventJSONLStrict(string(raw), projectID, runID)
		if perr != nil {
			return fail(out, StatusBlocked, "event log pre-recovery parse: "+perr.Error())
		}
		if verr := ValidateExistingEventLogForPlan(preEvents, EventWriteIdentity{
			ProjectID: projectID, RunID: runID,
			PlanDigest: out.PlanDigest, GraphDigest: out.GraphDigest,
			GraphID: out.GraphID, GraphVersion: out.GraphVersion,
		}, itemOK); verr != nil {
			return fail(out, StatusBlocked, "event log pre-recovery validate: "+verr.Error())
		}
	}

	// =====================================================================
	// SIDE EFFECTS — only after pure preflight + existing-log validation
	// =====================================================================
	var integrator BranchIntegrator
	// Product integration requires a real git repository path. Fake/missing
	// paths (unit tests that only set RepoPath for inventory) skip integrate.
	doIntegrate := !req.SkipIntegrate && isGitRepo(req.RepoPath)
	if doIntegrate {
		integrator = req.Integrator
		if integrator == nil {
			// Production default: durable namespaced integrate ledger under HomeDir.
			// Never <repo>/.loopcoder — customer repo stays free of LoopCoder runtime.
			ledgerDir, lerr := DefaultIntegrateLedgerDir(s.HomeDir, projectID, runID)
			if lerr != nil {
				return fail(out, StatusBlocked, "integrate ledger dir: "+lerr.Error())
			}
			integrator = GitBranchIntegrator{Now: now, LedgerDir: ledgerDir}
		}
		if _, err := integrator.EnsureGoalBranch(ctx, req.RepoPath, baseRef, goalBranch); err != nil {
			return fail(out, StatusBlocked, "ensure goal branch: "+err.Error())
		}
		emit("goal_branch.ready:" + goalBranch)
	}

	// Append-only event log (forced interrupt / exactly-once evidence source).
	// Fail closed on open/recovery/read corruption before claim/launch.
	elog, eerr := OpenEventLog(s.HomeDir, projectID, runID)
	if eerr != nil {
		return fail(out, StatusBlocked, "event log open: "+eerr.Error())
	}
	if elog == nil {
		return fail(out, StatusBlocked, "event log unavailable")
	}
	if s.TestConfigureEventLog != nil {
		s.TestConfigureEventLog(elog)
	}
	out.EventLogPath = elog.Path()
	// Authoritative hard-kill recovery: two-phase validate-then-mutate.
	// Never invent "forced process kill recovery".
	if n, rerr := RecoverOpenLaunchInterruptsAuthoritative(elog, RecoverOptions{
		HomeDir: s.HomeDir, ProjectID: projectID, RunID: runID, Now: now,
		WaitAlive: 8 * time.Second, KillAfterVerify: true,
		FailAfter:   s.TestRecoverFailAfter,
		PlanDigest:  out.PlanDigest,
		GraphDigest: out.GraphDigest,
	}); rerr != nil {
		return fail(out, StatusBlocked, "event log recover: "+rerr.Error())
	} else if n > 0 {
		out.Interrupted = true
		emit(fmt.Sprintf("interrupt:recover_open_launches n=%d", n))
	}
	// Durable next generation after recovery (and any prior hard-kill terminals).
	// Request.AttemptGeneration nil → auto; explicit must match durable next.
	postEvents, perr2 := elog.ReadAllForRun(projectID, runID)
	if perr2 != nil {
		return fail(out, StatusBlocked, "event log post-recovery read: "+perr2.Error())
	}
	if len(postEvents) > 0 {
		if verr := ValidateExistingEventLogForPlan(postEvents, EventWriteIdentity{
			ProjectID: projectID, RunID: runID,
			PlanDigest: out.PlanDigest, GraphDigest: out.GraphDigest,
			GraphID: out.GraphID, GraphVersion: out.GraphVersion,
		}, itemOK); verr != nil {
			return fail(out, StatusBlocked, "event log post-recovery validate: "+verr.Error())
		}
	}
	durableNextGen := NextAttemptGenerationFromEvents(postEvents)
	_, aborted := InterruptedFromEvents(postEvents)
	if out.AbortedAttempts == nil {
		out.AbortedAttempts = map[string]string{}
	}
	for id, att := range aborted {
		out.AbortedAttempts[id] = att
	}
	// logEv persists and returns the durable event. Failures return error —
	// model_unavailable/reroute/claim linkage must not invent event IDs.
	// Every attempt-scoped lifecycle event is stamped at write time with the
	// authoritative runtime identity (plan/graph digests, graph id/version,
	// project/run, work item, attempt, task class, CCD, generation).
	writeID := EventWriteIdentity{
		ProjectID: projectID, RunID: runID,
		PlanDigest: out.PlanDigest, GraphDigest: out.GraphDigest,
		GraphID: out.GraphID, GraphVersion: out.GraphVersion,
	}
	logEv := func(ev Event) (Event, error) {
		if elog == nil {
			return Event{}, fmt.Errorf("workflowrun: event log unavailable")
		}
		ev.ProjectID = projectID
		ev.RunID = runID
		if IsParentOnlyEvent(ev) {
			// Parent-only: project/run only; never invent attempt lifecycle stamps.
			outEv, err := elog.Append(ev)
			if err != nil {
				return Event{}, err
			}
			if strings.TrimSpace(outEv.EventID) == "" {
				return Event{}, fmt.Errorf("workflowrun: empty persisted event_id kind=%s", ev.Kind)
			}
			return outEv, nil
		}
		// Authoritative stamp — always overwrite from current Execute identity.
		if strings.TrimSpace(writeID.PlanDigest) == "" || strings.TrimSpace(writeID.GraphDigest) == "" ||
			strings.TrimSpace(writeID.GraphID) == "" || writeID.GraphVersion <= 0 {
			return Event{}, fmt.Errorf("workflowrun: refuse persist: incomplete result identity for kind=%s", ev.Kind)
		}
		ev.ExecutionPlanDigest = writeID.PlanDigest
		ev.GraphDigest = writeID.GraphDigest
		ev.GraphID = writeID.GraphID
		ev.GraphVersion = writeID.GraphVersion
		// Assignment-time child contract + complete structured identity.
		// Never post-exec invent from InvokedRoute.
		wid := strings.TrimSpace(ev.WorkItemID)
		if wid == "" {
			return Event{}, fmt.Errorf("workflowrun: refuse persist: attempt-scoped kind=%s missing work_item_id", ev.Kind)
		}
		if contracts == nil {
			return Event{}, fmt.Errorf("workflowrun: refuse persist: child contracts unavailable for %s", wid)
		}
		c, ok := contracts[wid]
		if !ok {
			return Event{}, fmt.Errorf("workflowrun: refuse persist: no contract for work_item_id %q", wid)
		}
		ev.TaskClass = c.TaskClass
		ev.ChildContractDigest = c.Digest
		var m map[string]string
		if len(ev.Payload) > 0 {
			if err := json.Unmarshal(ev.Payload, &m); err != nil {
				return Event{}, fmt.Errorf("workflowrun: refuse persist: malformed event payload kind=%s: %w", ev.Kind, err)
			}
			if m == nil {
				return Event{}, fmt.Errorf("workflowrun: refuse persist: event payload must be a JSON object kind=%s", ev.Kind)
			}
		} else {
			m = map[string]string{}
		}
		m = stampChildIdentityPayload(m, projectID, runID, ev.GraphID, ev.GraphVersion,
			wid, ev.AttemptID, ev.Generation,
			ev.ExecutionPlanDigest, ev.GraphDigest, c.TaskClass, c.Digest)
		// Preserve caller-supplied route fields; fill missing from ChildRoute when empty.
		r := resolveChildRoute(req.ChildRoutes, wid, defaultProvider, defaultModel)
		r.TaskClass = c.TaskClass
		r.Depth = firstNonEmpty(r.Depth, c.Depth)
		r.Permission = firstNonEmpty(r.Permission, c.Permission)
		rf := childRoutePayloadFields(r)
		for _, k := range requiredRouteKeys {
			if strings.TrimSpace(m[k]) == "" && strings.TrimSpace(rf[k]) != "" {
				m[k] = rf[k]
			}
		}
		// Canonical FailureClass: top-level ↔ payload failure_class (never Message).
		if strings.TrimSpace(ev.FailureClass) == "" {
			ev.FailureClass = strings.TrimSpace(m["failure_class"])
		}
		if strings.TrimSpace(ev.FailureClass) != "" {
			m["failure_class"] = strings.TrimSpace(ev.FailureClass)
		}
		// Exact durable terminal on persisted event stamp — no EqualFold/TrimSpace.
		if ev.Terminal == string(workgraph.TermSucceeded) {
			ev.FailureClass = ""
			delete(m, "failure_class")
		}
		ev.Payload = eventJSONPayload(m)
		if err := ValidateChildEventIdentityForPlan(ev, writeID, itemOK); err != nil {
			return Event{}, fmt.Errorf("workflowrun: refuse persist: %w", err)
		}
		outEv, err := elog.Append(ev)
		if err != nil {
			return Event{}, err
		}
		if strings.TrimSpace(outEv.EventID) == "" {
			return Event{}, fmt.Errorf("workflowrun: empty persisted event_id kind=%s", ev.Kind)
		}
		return outEv, nil
	}
	if _, err := logEv(Event{Kind: "run.start", Message: "workflow execute"}); err != nil {
		return fail(out, StatusBlocked, "event log start: "+err.Error())
	}
	// --- schedule + claim + real child execute each ready item once ---
	// Durable claim store co-located with the event log (stable home, not PID).
	runDir, rerr := RunDurableDir(s.HomeDir, projectID, runID)
	if rerr != nil {
		return fail(out, StatusBlocked, "durable run dir: "+rerr.Error())
	}
	claimPath := filepath.Join(runDir, "workclaims.json")
	cs, csErr := workclaim.OpenPath(claimPath, now)
	if csErr != nil {
		return fail(out, StatusBlocked, "claim store open: "+csErr.Error())
	}
	// Reconcile open claims from exact durable terminal events (crash window
	// recovery) then fail closed on remaining impossible states.
	if recErr := reconcileClaimsWithEventLog(cs, elog, projectID, runID); recErr != nil {
		return fail(out, StatusBlocked, "claim/event reconcile: "+recErr.Error())
	}
	ev := workgraph.TerminalEvidence{}
	claimed := map[string]int{}
	launches := 0
	integrated := []string{}
	// True concurrent peaks (not launch-count accumulators).
	peaks := concurrentPeaks{}
	// Run-scoped ProviderExecutionAuthority store (same storage schema as worker).
	authStore, authOpenErr := OpenAuthorityStore(ctx, s.HomeDir, projectID, runID, now)
	if authOpenErr != nil {
		return fail(out, StatusBlocked, "authority store open: "+authOpenErr.Error())
	}
	if authStore != nil {
		defer authStore.Close()
	}
	// itemByID already built in pure preflight.

	bounds := waveschedule.DefaultBounds()
	for wave := 0; wave < maxWaves; wave++ {
		if ctx.Err() != nil {
			out.Interrupted = true
			// Parent interrupt is recovery evidence — not silent best-effort.
			if _, ierr := logEv(Event{Kind: "interrupt", Message: "cancelled mid-wave (forced process interrupt)", Generation: 0}); ierr != nil {
				return failBlockedJoin(out, s.HomeDir, projectID, runID, "parent interrupt event: "+ierr.Error()+"; cancelled mid-wave", nil)
			}
			emit("interrupt:mid_wave")
			return failBlockedJoin(out, s.HomeDir, projectID, runID, "cancelled mid-wave", nil)
		}
		ready := workgraph.EvaluateReady(g, ev)
		if !ready.Valid {
			return fail(out, StatusBlocked, "ready invalid: "+strings.Join(ready.Errors, ";"))
		}
		if len(ready.Ready) == 0 {
			// check all terminal
			if allTerminal(g, ev) {
				emit("waves.complete")
				break
			}
			return fail(out, StatusBlocked, "no ready items but graph incomplete")
		}

		// wave plan under budgets (deterministic order)
		snap := waveschedule.Snapshot{
			Graph: g, Evidence: ev, Bounds: bounds, WaveSeq: wave,
		}
		wp, err := waveschedule.PlanWave(snap)
		if err != nil {
			return fail(out, StatusBlocked, "wave plan: "+err.Error())
		}
		members := ready.Ready
		if len(wp.Members) > 0 {
			members = nil
			for _, m := range wp.Members {
				members = append(members, m.WorkItemID)
			}
		}
		emit(fmt.Sprintf("wave.%d ready=%d", wave, len(members)))

		for _, id := range members {
			if claimed[id] > 0 {
				continue
			}
			// Exactly-once resume: any present PriorSucceeded entry must fully
			// validate against the locally normalized plan + prevalidated child
			// contract, or fail closed before claim/launch. Never mutate supplied
			// evidence (including WorkItemID). Never fall through to re-exec.
			if prior, ok := req.PriorSucceeded[id]; ok {
				if err := validatePriorSucceededForReuse(prior, id, out.PlanDigest, runID, contracts[id]); err != nil {
					return fail(out, StatusBlocked, err.Error())
				}
				// Mark claimed so claim-once map stays consistent without a second claim.
				claimed[id] = 1
				out.ReuseCount++
				ev[id] = workgraph.TermSucceeded
				// PriorOutcomes may already have seeded this attempt into out.Children;
				// append only when missing (exact AttemptID). Never duplicate.
				already := false
				for _, c := range out.Children {
					// Byte-exact AttemptID compare for durable prior seed.
					if c.AttemptID == prior.AttemptID {
						already = true
						break
					}
				}
				if !already {
					out.Children = append(out.Children, prior)
				}
				// Re-list Integrated only when prior carries product integrate SHA.
				// Terminal-only prior (SkipIntegrate / no-repo) must not invent Integrated.
				if strings.TrimSpace(prior.IntegrateCommitSHA) != "" {
					integrated = append(integrated, id)
				}
				emit(fmt.Sprintf("child.reuse:%s attempt=%s evidence=%s", id, prior.AttemptID, short(prior.OutputEvidence)))
				// Reuse events must carry exact plan/class/full CCD/attempt/positive generation.
				if _, lerr := logEv(Event{
					Kind: "reuse", WorkItemID: id, AttemptID: prior.AttemptID,
					Evidence: prior.OutputEvidence, Generation: prior.Generation,
					ExecutionPlanDigest: out.PlanDigest, GraphDigest: out.GraphDigest,
					TaskClass: prior.TaskClass, ChildContractDigest: prior.ChildContractDigest,
					Terminal: string(workgraph.TermSucceeded),
				}); lerr != nil {
					return fail(out, StatusBlocked, "reuse event: "+lerr.Error())
				}
				// Prior reuse does not count as concurrent occupancy.
				continue
			}
			// Bind attempt to plan digest AND unique run ID (no cross-run reuse).
			// Generation: durable hard-recovery next (auto) unless explicit map provided.
			// Explicit must match durable next when both exist (fail closed).
			gen := 0
			if d, ok := durableNextGen[id]; ok {
				gen = d
			}
			if req.AttemptGeneration != nil {
				if explicit, ok := req.AttemptGeneration[id]; ok {
					if d, dok := durableNextGen[id]; dok && explicit != d {
						return fail(out, StatusBlocked, fmt.Sprintf(
							"AttemptGeneration[%s]=%d != durable next %d (fail closed)", id, explicit, d))
					}
					gen = explicit
				}
			}
			attemptID := AttemptID(id, out.PlanDigest, runID, gen)
			cc, ok := contracts[id]
			if !ok {
				return fail(out, StatusInvalid, fmt.Sprintf("claim %s: missing prevalidated child contract", id))
			}
			res, err := cs.Claim(workclaim.ClaimRequest{
				ProjectID: projectID, Graph: g, Evidence: ev, WorkItemID: id,
				AttemptID:  attemptID,
				ExecutorID: "workflowrun", Lease: time.Minute,
				PlanDigest:          out.PlanDigest, // required ExecutionPlanDigest — no empty fallback
				TaskClass:           cc.TaskClass,
				ChildContractDigest: cc.Digest,
			})
			if err != nil {
				return fail(out, StatusBlocked, fmt.Sprintf("claim %s: %v", id, err))
			}
			// Durable restart: closed claim for this exact attempt → reconstruct
			// outcome from claim + terminal/launch events; launch zero providers.
			if res.Code == workclaim.ResultTerminalReused && res.Claim != nil &&
				res.Claim.State == workclaim.StateClosed {
				reused, rerr := reconstructOutcomeFromDurable(res.Claim, elog, projectID, runID, id)
				if rerr != nil {
					return fail(out, StatusBlocked, fmt.Sprintf("claim %s terminal reuse: %v", id, rerr))
				}
				claimed[id] = 1
				out.ReuseCount++
				ev[id] = workgraph.TerminalState(reused.Terminal)
				out.Children = append(out.Children, reused)
				// Product Integrated only with durable integrate commit identity.
				if reused.Terminal == string(workgraph.TermSucceeded) &&
					reused.IntegrateCommitSHA != "" {
					integrated = append(integrated, id)
				}
				emit(fmt.Sprintf("child.terminal_reuse:%s attempt=%s terminal=%s", id, reused.AttemptID, reused.Terminal))
				// Durable reuse must stamp Generation matching attempt suffix+1.
				// Any suffix parse/canonical mismatch fails before persistence.
				wantG, gerr := ClaimGenerationFromAttemptID(reused.AttemptID)
				if gerr != nil {
					return fail(out, StatusBlocked, fmt.Sprintf("terminal reuse %s: %v", id, gerr))
				}
				reuseGen := reused.Generation
				if reuseGen <= 0 {
					reuseGen = wantG
				} else if reuseGen != wantG {
					return fail(out, StatusBlocked, fmt.Sprintf(
						"terminal reuse %s generation %d != attempt suffix+1 %d", id, reuseGen, wantG))
				}
				if res.Claim != nil && res.Claim.Generation > 0 {
					if int64(reuseGen) != res.Claim.Generation {
						return fail(out, StatusBlocked, fmt.Sprintf(
							"terminal reuse %s generation %d != claim %d", id, reuseGen, res.Claim.Generation))
					}
				}
				if reuseGen <= 0 {
					return fail(out, StatusBlocked, fmt.Sprintf("terminal reuse %s missing positive generation", id))
				}
				if _, lerr := logEv(Event{
					Kind: "reuse", WorkItemID: id, AttemptID: reused.AttemptID,
					Terminal: reused.Terminal, Evidence: reused.OutputEvidence,
					Generation: reuseGen,
					Message:    "durable claim+terminal restart reuse; zero launch",
				}); lerr != nil {
					return fail(out, StatusBlocked, "terminal reuse event: "+lerr.Error())
				}
				continue
			}
			if res.Code != workclaim.ResultClaimed {
				return fail(out, StatusBlocked, fmt.Sprintf("claim %s: code=%v reason=%s", id, res.Code, res.Reason))
			}
			claimed[id]++
			out.ClaimCount++
			// Event Generation must equal attempt suffix+1 (canonical), not merely
			// claim fence generation which can lag AttemptGeneration bumps.
			eventGen, egErr := ClaimGenerationFromAttemptID(attemptID)
			if egErr != nil {
				return fail(out, StatusBlocked, fmt.Sprintf("claim %s: %v", id, egErr))
			}
			if eventGen <= 0 {
				return fail(out, StatusBlocked, fmt.Sprintf("claim %s: non-positive generation %d", id, eventGen))
			}
			// Critical claim linkage: fail closed before exec if durable claim event missing.
			// On failure: terminalize via durable terminal→close path (never direct-close
			// into closed-claim-without-terminal).
			if _, lerr := logEv(Event{Kind: "claim", WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
				Payload: eventJSONPayload(map[string]string{
					"work_item_id": id,
					"attempt_id":   attemptID,
				}),
			}); lerr != nil {
				failOut := ChildOutcome{
					WorkItemID: id, AttemptID: attemptID, FailureClass: "claim_event_failed",
					Message: lerr.Error(),
				}
				// Append terminal then close — same critical path as other failures.
				termPayload := map[string]string{
					"work_item_id": id, "attempt_id": attemptID,
					"terminal": string(workgraph.TermFailed), "failure_class": "claim_event_failed",
					"output_evidence": "failed:claim_event:" + id,
				}
				if _, terr := logEv(Event{
					Kind: "terminal", WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
					Terminal: string(workgraph.TermFailed), Evidence: "failed:claim_event:" + id,
					Message: "claim_event_failed", Payload: eventJSONPayload(termPayload),
				}); terr != nil {
					// Leave claim open for reopen recovery — never closed without terminal.
					return fail(out, StatusBlocked, "claim event: "+lerr.Error()+"; terminal: "+terr.Error())
				}
				if s.TestCrashAfterTerminal != nil {
					if cerr := s.TestCrashAfterTerminal(attemptID, string(workgraph.TermFailed)); cerr != nil {
						return fail(out, StatusBlocked, "claim event: "+lerr.Error()+"; crash: "+cerr.Error())
					}
				}
				if _, cerr := cs.Close(workclaim.CloseRequest{
					ClaimID: res.Claim.ClaimID, Generation: res.Claim.Generation,
					ExecutorID: "workflowrun", AttemptID: res.Claim.AttemptID,
					Terminal: workgraph.TermFailed, OutputEvidence: "failed:claim_event:" + id,
				}); cerr != nil {
					failOut.Message = lerr.Error() + "; close: " + cerr.Error()
					out.Children = append(out.Children, failOut)
					return fail(out, StatusBlocked, "claim event: "+lerr.Error()+"; close: "+cerr.Error())
				}
				failOut.Terminal = string(workgraph.TermFailed)
				out.Children = append(out.Children, failOut)
				return fail(out, StatusBlocked, "claim event: "+lerr.Error())
			}

			route := resolveChildRoute(req.ChildRoutes, id, defaultProvider, defaultModel)
			it := itemByID[id]
			// Assignment dimensions always come from the prevalidated contract.
			// When ChildRoutes was absent, this constructs the complete route once
			// from Definition. When present, ChildRoute already matched exactly —
			// never partially merge empties from Definition at launch time.
			route.TaskClass = cc.TaskClass
			route.Depth = cc.Depth
			route.Permission = cc.Permission
			readOnly := strings.EqualFold(cc.Permission, "read-only")

			childIn := ChildExecInput{
				ProjectID: projectID, GraphID: g.GraphID, WorkItemID: id,
				ClaimID: res.Claim.ClaimID, AttemptID: attemptID,
				Intent: it.Intent, Route: route, RepoPath: req.RepoPath,
				// Materialize child from goal branch so prior integrations are visible.
				BaseRef:  firstNonEmpty(goalBranch, baseRef),
				ReadOnly: readOnly,
			}
			// Critical launch linkage with full route identity (not provider/model alone).
			emit(fmt.Sprintf("child.launch:%s route=%s/%s depth=%s gen=%d", id, route.Provider, route.Model, route.Depth, gen))
			if _, lerr := logEv(Event{Kind: "launch", WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
				Message: route.Provider + "/" + route.Model,
				Payload: eventJSONPayload(map[string]string{
					"work_item_id":   id,
					"attempt_id":     attemptID,
					"provider":       route.Provider,
					"model":          route.Model,
					"depth":          route.Depth,
					"permission":     route.Permission,
					"account_ref":    route.AccountRef,
					"install_ref":    route.InstallRef,
					"window_kind":    route.WindowKind,
					"reservation_id": route.ReservationID,
					"route_reason":   route.RouteReason,
				}),
			}); lerr != nil {
				termPayload := map[string]string{
					"work_item_id": id, "attempt_id": attemptID,
					"terminal": string(workgraph.TermFailed), "failure_class": "launch_event_failed",
					"output_evidence": "failed:launch_event:" + id,
				}
				if _, terr := logEv(Event{
					Kind: "terminal", WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
					Terminal: string(workgraph.TermFailed), Evidence: "failed:launch_event:" + id,
					Message: "launch_event_failed", Payload: eventJSONPayload(termPayload),
				}); terr != nil {
					return fail(out, StatusBlocked, "launch event: "+lerr.Error()+"; terminal: "+terr.Error())
				}
				if s.TestCrashAfterTerminal != nil {
					if cerr := s.TestCrashAfterTerminal(attemptID, string(workgraph.TermFailed)); cerr != nil {
						return fail(out, StatusBlocked, "launch event: "+lerr.Error()+"; crash: "+cerr.Error())
					}
				}
				closeMsg := ""
				if _, cerr := cs.Close(workclaim.CloseRequest{
					ClaimID: res.Claim.ClaimID, Generation: res.Claim.Generation,
					ExecutorID: "workflowrun", AttemptID: res.Claim.AttemptID,
					Terminal: workgraph.TermFailed, OutputEvidence: "failed:launch_event:" + id,
				}); cerr != nil {
					closeMsg = "; close: " + cerr.Error()
				}
				out.Children = append(out.Children, ChildOutcome{
					WorkItemID: id, AttemptID: attemptID, Terminal: string(workgraph.TermFailed),
					FailureClass: "launch_event_failed", Message: lerr.Error() + closeMsg,
					Provider: route.Provider, Model: route.Model, Depth: route.Depth,
					Permission: route.Permission, AccountRef: route.AccountRef, WindowKind: route.WindowKind,
				})
				return fail(out, StatusBlocked, "launch event: "+lerr.Error()+closeMsg)
			}
			launches++
			out.LaunchCount = launches
			// Spawn-time process identity: Service owns durable pid truth for this
			// exact work_item/attempt/generation. Production path fires while the
			// child is alive via OnProviderStart; failure kills+drains the group.
			var spawnPIDLogged bool
			var processEntered bool
			var spawnStart ProcessStart
			var spawnPIDErr error
			var spawnAuth storage.ProviderExecutionAuthority
			authOwner := ""
			if res.Claim != nil {
				authOwner = AuthorityOwnerFromClaimID(res.Claim.ClaimID)
			}
			claimGen := int64(eventGen)
			if res.Claim != nil && res.Claim.Generation > 0 {
				claimGen = int64(res.Claim.Generation)
			}
			storePath, _ := AuthorityStorePath(s.HomeDir, projectID, runID)
			diagPath, _ := GuardianDiagnosticPath(s.HomeDir, projectID, runID, attemptID)
			// Worktree lease: enter at each allocation; exact leave per enter (primary then
			// alternate). Must not use a one-shot "released" flag — alternate re-enters after
			// primary leave. Counters decrement only after path+git registration are gone.
			wtActive := false
			wtPath := ""
			var wtCleanupErr error
			childIn.OnWorktreeAllocated = func(path string) error {
				// Second allocation while prior still active: fail closed (do not continue).
				if wtActive {
					return fmt.Errorf("workflowrun: worktree still active at %s; refusing allocate %s", wtPath, path)
				}
				peaks.enterWorktree()
				wtActive = true
				wtPath = path
				out.WorktreePeak = peaks.WorktreePeak
				out.WorktreeActive = peaks.activeWorktree
				return nil
			}
			// releaseWorktreePeak deregisters the path then leaves the peak counter.
			// Cleanup failure keeps active/leak evidence truthful (does not claim active=0).
			// Blocks succeeded/human-gate when non-nil; combine with primary errors.
			releaseFn := releaseChildWorktree
			if s.TestReleaseWorktree != nil {
				releaseFn = s.TestReleaseWorktree
			}
			releaseWorktreePeak := func() error {
				if !wtActive && strings.TrimSpace(wtPath) == "" {
					return wtCleanupErr // idempotent durable result
				}
				p := strings.TrimSpace(wtPath)
				if p != "" {
					if rerr := releaseFn(req.RepoPath, p); rerr != nil {
						if wtCleanupErr == nil {
							wtCleanupErr = rerr
						}
						// Keep wtActive true / path retained so counters match leak.
						out.WorktreeActive = peaks.activeWorktree
						if out.WorktreeActive == 0 && wtActive {
							// Truthful: still active while leaked.
							out.WorktreeActive = 1
						}
						// Retain leaked path on Result for diagnostics (last child outcome may hold it).
						return wtCleanupErr
					}
					wtPath = ""
				}
				if wtActive {
					peaks.leaveWorktree()
					wtActive = false
					out.WorktreePeak = peaks.WorktreePeak
					out.WorktreeActive = peaks.activeWorktree
					out.ProcessActive = peaks.activeProcess
				}
				return wtCleanupErr
			}

			// Guardian + authority share claim-bound fence; ProviderAuthority after persist.
			childIn.Guardian = BuildChildGuardianOptions(storePath, diagPath, projectID, runID, attemptID, authOwner, claimGen, &spawnAuth)
			// Guardian.OnStart fires only after provider OnStart completed and guardian is ready.
			childIn.Guardian.OnStart = func(gp supervisedexec.GuardianProcess) error {
				if s.TestAfterGuardianReady != nil {
					s.TestAfterGuardianReady(spawnStart, gp.PID, diagPath)
				}
				return nil
			}
			childIn.OnProcessStart = func(ps ProcessStart) error {
				// Exactly-once: reject a second callback BEFORE any second append.
				if spawnPIDLogged {
					spawnPIDErr = fmt.Errorf("workflowrun: duplicate process-start for %s/%s (exactly-once pid)", id, attemptID)
					return spawnPIDErr
				}
				if err := ValidateProcessStart(ps); err != nil {
					spawnPIDErr = err
					return err
				}
				if authOwner == "" {
					spawnPIDErr = fmt.Errorf("workflowrun: claim-bound authority owner required")
					return spawnPIDErr
				}
				// Persist ProviderExecutionAuthority BEFORE pid event (same identity).
				if authStore != nil {
					persisted, aerr := PersistChildExecutionAuthority(ctx, authStore, projectID, runID, attemptID, authOwner, claimGen, ps, ps.WorktreePath, ps.LogPath, ps.ObservedAt)
					if aerr != nil {
						spawnPIDErr = aerr
						return aerr
					}
					if err := ValidateAuthorityMatchesSpawn(persisted, ps, claimGen, attemptID, authOwner); err != nil {
						spawnPIDErr = err
						return err
					}
					// SpawnPhase=authority_persisted is durable in the same row/tx as Persist
					// (see PersistChildExecutionAuthority). Crash between Persist and PID
					// append is recoverable solely from that row phase — not a later event.
					spawnAuth = persisted
				}
				pidPL := processStartPayload(ps)
				pidPL = mergePayloadStringMap(pidPL, childRoutePayloadFields(route))
				evPID := Event{
					Kind: "pid", WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
					PID: ps.PID, Payload: eventJSONPayload(pidPL),
					Message: "spawn process identity",
				}
				if err := ValidatePIDEventPayload(evPID); err != nil {
					// Mark typed pid_event_failed — never swallow transition errors.
					if authStore != nil && authOwner != "" && spawnAuth.ProviderPID > 0 {
						if terr := TransitionChildSpawnPhase(ctx, authStore, projectID, runID, attemptID, authOwner, claimGen, SpawnPhasePIDEventFailed, now()); terr != nil {
							spawnPIDErr = fmt.Errorf("%w; spawn_phase: %v", err, terr)
							return spawnPIDErr
						}
					}
					spawnPIDErr = err
					return err
				}
				outPID, perr := logEv(evPID)
				if perr != nil {
					// Authority row stays authority_persisted (or we stamp pid_event_failed).
					// Prefer pid_event_failed so recovery does not wait for a phantom PID.
					if authStore != nil && authOwner != "" && spawnAuth.ProviderPID > 0 {
						if terr := TransitionChildSpawnPhase(ctx, authStore, projectID, runID, attemptID, authOwner, claimGen, SpawnPhasePIDEventFailed, now()); terr != nil {
							spawnPIDErr = fmt.Errorf("%w; spawn_phase: %v", perr, terr)
							return spawnPIDErr
						}
					}
					spawnPIDErr = perr
					return perr
				}
				// PID durable → transition spawn_phase on the same authority store.
				if authStore != nil && authOwner != "" && spawnAuth.ProviderPID > 0 {
					if terr := TransitionChildSpawnPhase(ctx, authStore, projectID, runID, attemptID, authOwner, claimGen, SpawnPhasePIDEventPersisted, now()); terr != nil {
						spawnPIDErr = terr
						return terr
					}
					spawnAuth.SpawnPhase = SpawnPhasePIDEventPersisted
				}
				spawnPIDLogged = true
				spawnStart = ps
				peaks.enterProcess()
				processEntered = true
				out.ProcessPeak = peaks.ProcessPeak
				if s.TestAfterPIDEvent != nil {
					s.TestAfterPIDEvent(outPID)
				}
				return nil
			}
			childOut, cerr := exec.Execute(ctx, childIn)
			// Exact leave for process enter (including OnStart-then-error paths).
			if processEntered {
				peaks.leaveProcess()
				processEntered = false
				out.ProcessPeak = peaks.ProcessPeak
			} else if childOut.ProcessPID > 0 && !childOut.SpawnObserved {
				// Fake occupancy pulse only — never emit durable production PID without
				// authority row (no payload-shape recovery bypass).
				peaks.enterProcess()
				peaks.leaveProcess()
				out.ProcessPeak = peaks.ProcessPeak
			}
			// Executor error never coexists with success terminal/integration.
			if cerr != nil {
				if childOut.Terminal == workgraph.TermSucceeded || childOut.Terminal == workgraph.TermNone {
					childOut.Terminal = workgraph.TermFailed
				}
				if strings.TrimSpace(childOut.FailureClass) == "" {
					childOut.FailureClass = "executor_error"
				}
				if strings.TrimSpace(childOut.Message) == "" {
					childOut.Message = cerr.Error()
				}
				if strings.TrimSpace(childOut.OutputEvidence) == "" {
					childOut.OutputEvidence = "failed:executor_error:" + id
				}
			}
			// Authority complete only after final terminal is known — deferred to finalize paths.
			// Never invent a durable pid for Fake post-return ProcessPID.
			if spawnPIDErr != nil {
				termPayload := map[string]string{
					"work_item_id": id, "attempt_id": attemptID,
					"terminal": string(workgraph.TermFailed), "failure_class": "pid_event_failed",
					"output_evidence": "failed:pid_event:" + id,
				}
				closeMsg := ""
				if _, terr := logEv(Event{
					Kind: "terminal", WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
					Terminal: string(workgraph.TermFailed), Evidence: "failed:pid_event:" + id,
					Message: "pid_event_failed", Payload: eventJSONPayload(termPayload),
				}); terr != nil {
					// Leave claim/authority open for reconciler — never close without durable terminal.
					out.Children = append(out.Children, ChildOutcome{
						WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
						TaskClass: cc.TaskClass, ExecutionPlanDigest: out.PlanDigest, ChildContractDigest: cc.Digest,
						FailureClass: "pid_event_failed", Message: spawnPIDErr.Error() + "; terminal: " + terr.Error(),
						Provider: route.Provider, Model: route.Model, Depth: route.Depth,
					})
					ev[id] = workgraph.TermFailed
					return failBlockedJoin(out, s.HomeDir, projectID, runID, "pid event: "+spawnPIDErr.Error()+"; terminal: "+terr.Error(), releaseWorktreePeak)
				}
				if _, cerr := cs.Close(workclaim.CloseRequest{
					ClaimID: res.Claim.ClaimID, Generation: res.Claim.Generation,
					ExecutorID: "workflowrun", AttemptID: res.Claim.AttemptID,
					Terminal: workgraph.TermFailed, OutputEvidence: "failed:pid_event:" + id,
				}); cerr != nil {
					// Do not swallow: claim stays open; reconciler closes + completes authority.
					closeMsg = "; close: " + cerr.Error()
				} else if authStore != nil && authOwner != "" && spawnAuth.ProviderPID > 0 {
					// pid_event_failed / authority_persisted → typed pre-PID complete only.
					if aerr := CompleteChildExecutionAuthorityPrePIDRecovery(ctx, authStore, projectID, runID, attemptID, authOwner, claimGen, string(workgraph.TermFailed), now()); aerr != nil {
						closeMsg = "; authority complete: " + aerr.Error()
					}
				}
				out.Children = append(out.Children, ChildOutcome{
					WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
					TaskClass: cc.TaskClass, ExecutionPlanDigest: out.PlanDigest, ChildContractDigest: cc.Digest,
					FailureClass: "pid_event_failed", Message: spawnPIDErr.Error() + closeMsg, Terminal: string(workgraph.TermFailed),
					Provider: route.Provider, Model: route.Model, Depth: route.Depth,
				})
				ev[id] = workgraph.TermFailed
				return failBlockedJoin(out, s.HomeDir, projectID, runID, "pid event: "+spawnPIDErr.Error()+closeMsg, releaseWorktreePeak)
			}
			if spawnPIDLogged {
				if err := crossCheckSpawnPID(spawnStart, childOut); err != nil {
					termPayload := map[string]string{
						"work_item_id": id, "attempt_id": attemptID,
						"terminal": string(workgraph.TermFailed), "failure_class": "pid_identity_mismatch",
						"output_evidence": "failed:pid_mismatch:" + id,
					}
					closeMsg := ""
					if _, terr := logEv(Event{
						Kind: "terminal", WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
						Terminal: string(workgraph.TermFailed), Evidence: "failed:pid_mismatch:" + id,
						Message: "pid_identity_mismatch", Payload: eventJSONPayload(termPayload),
					}); terr != nil {
						out.Children = append(out.Children, ChildOutcome{
							WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
							TaskClass: cc.TaskClass, ExecutionPlanDigest: out.PlanDigest, ChildContractDigest: cc.Digest,
							FailureClass: "pid_identity_mismatch", Message: err.Error() + "; terminal: " + terr.Error(),
							Provider: route.Provider, Model: route.Model, Depth: route.Depth,
						})
						ev[id] = workgraph.TermFailed
						return failBlockedJoin(out, s.HomeDir, projectID, runID, "pid identity mismatch: "+err.Error()+"; terminal: "+terr.Error(), nil)
					}
					if _, cerr := cs.Close(workclaim.CloseRequest{
						ClaimID: res.Claim.ClaimID, Generation: res.Claim.Generation,
						ExecutorID: "workflowrun", AttemptID: res.Claim.AttemptID,
						Terminal: workgraph.TermFailed, OutputEvidence: "failed:pid_mismatch:" + id,
					}); cerr != nil {
						closeMsg = "; close: " + cerr.Error()
					} else if authStore != nil && authOwner != "" && spawnAuth.ProviderPID > 0 {
						if aerr := CompleteChildExecutionAuthority(ctx, authStore, projectID, runID, attemptID, authOwner, claimGen, string(workgraph.TermFailed), now()); aerr != nil {
							closeMsg = "; authority complete: " + aerr.Error()
						}
					}
					out.Children = append(out.Children, ChildOutcome{
						WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
						TaskClass: cc.TaskClass, ExecutionPlanDigest: out.PlanDigest, ChildContractDigest: cc.Digest,
						FailureClass: "pid_identity_mismatch", Message: err.Error() + closeMsg, Terminal: string(workgraph.TermFailed),
						Provider: route.Provider, Model: route.Model, Depth: route.Depth,
					})
					ev[id] = workgraph.TermFailed
					return failBlockedJoin(out, s.HomeDir, projectID, runID, "pid identity mismatch: "+err.Error()+closeMsg, releaseWorktreePeak)
				}
			}
			// Fake / no SpawnObserved: never emit a durable production pid event without
			// authority row (removes payload-shape recovery bypass). Occupancy already pulsed.
			// Assignment-time contract identity (pre-claim); InvokedRoute supplies actual
			// route pin evidence only — never recompute ChildContractDigest post-exec.
			inv := childOut.InvokedRoute
			outcome := ChildOutcome{
				WorkItemID: id, Provider: inv.Provider, Model: inv.Model, Depth: inv.Depth,
				Permission: inv.Permission, AccountRef: inv.AccountRef, InstallRef: inv.InstallRef,
				WindowKind: inv.WindowKind, ReservationID: inv.ReservationID,
				RouteReason: route.RouteReason,
				TaskClass:   cc.TaskClass, ExecutionPlanDigest: out.PlanDigest,
				ChildContractDigest: cc.Digest, Generation: eventGen,
				AttemptID: attemptID, WorktreePath: childOut.WorktreePath,
				OutputEvidence: childOut.OutputEvidence, ExitCode: childOut.ExitCode,
				FailureClass: childOut.FailureClass, Message: childOut.Message,
				ActualCapacity: childOut.ActualCapacity, ActualSource: childOut.ActualSource,
				InputTokens: childOut.InputTokens, OutputTokens: childOut.OutputTokens,
				ActualSources: ActualRouteSources{
					Model: childOut.ActualSources.Model, Effort: childOut.ActualSources.Effort,
					Permission: childOut.ActualSources.Permission, Account: childOut.ActualSources.Account,
					Install: childOut.ActualSources.Install,
				},
				ArgvDigest:   childOut.ArgvDigest,
				FilesTouched: childOut.FilesTouched,
			}

			term := childOut.Terminal
			if term == workgraph.TermNone {
				if cerr != nil {
					term = workgraph.TermFailed
				} else {
					term = workgraph.TermFailed
					outcome.FailureClass = "missing_terminal"
					outcome.Message = "executor returned no terminal"
				}
			}
			// Exact route affirmation rejects claimed SUCCESS when InvokedRoute is
			// missing or mismatches independently observed identity. Pre-spawn /
			// process failures already carry authentic FailureClass+Message — never
			// overwrite them with route_identity_mismatch (RC38: auth_refusal was
			// masked as "invoked model required nonempty" after empty Actual*).
			if term == workgraph.TermSucceeded {
				if invErr := exactRouteMatch(route, childOut.InvokedRoute); invErr != nil {
					term = workgraph.TermFailed
					outcome.FailureClass = "route_identity_mismatch"
					outcome.Message = invErr.Error()
					if strings.TrimSpace(outcome.OutputEvidence) == "" {
						outcome.OutputEvidence = "failed:route_identity:" + id
					}
				}
			}
			// Context cancel mid-child → exactly one typed service_forced_interrupt
			// plus matching cancelled terminal (distinct from hard_kill_recovery).
			// Does NOT select hard-recovery gN+1; AbortedAttempts drives resume gen.
			// Only THIS branch may use failure_class=forced_interrupt / service_forced_interrupt.
			serviceInterruptID := ""
			if ctx.Err() != nil && term != workgraph.TermSucceeded {
				term = workgraph.TermCancelled
				outcome.FailureClass = "forced_interrupt"
				out.Interrupted = true
				if out.AbortedAttempts == nil {
					out.AbortedAttempts = map[string]string{}
				}
				out.AbortedAttempts[id] = attemptID
				// Required child interrupt evidence — not best-effort.
				// Prefer durable spawn-time PID when spawn was logged (exact identity).
				intPID, intPIDErr := interruptPIDFromSpawn(spawnPIDLogged, spawnStart, childOut)
				if intPIDErr != nil {
					outcome.FailureClass = "interrupt_pid_mismatch"
					outcome.Message = intPIDErr.Error()
					outcome.Terminal = string(workgraph.TermFailed)
					term = workgraph.TermFailed
					emit(fmt.Sprintf("interrupt_pid_mismatch:%s: %v", id, intPIDErr))
				} else {
					serviceInterruptID = newInterruptID(attemptID, eventGen)
					intPayload := map[string]string{
						"failure_class":   "forced_interrupt",
						"interrupt_class": InterruptClassServiceForced,
						"interrupt_id":    serviceInterruptID,
						"terminal":        string(workgraph.TermCancelled),
						"work_item_id":    id,
						"attempt_id":      attemptID,
						"generation":      fmt.Sprintf("%d", eventGen),
					}
					intPayload = mergePayloadStringMap(intPayload, childRoutePayloadFields(route))
					if intPID > 0 {
						intPayload["pid"] = fmt.Sprintf("%d", intPID)
					}
					if _, ierr := logEv(Event{
						Kind: "interrupt", WorkItemID: id, AttemptID: attemptID, Generation: eventGen,
						PID: intPID, Message: "forced interrupt; attempt aborted",
						Terminal: string(workgraph.TermCancelled), FailureClass: "forced_interrupt",
						Payload: eventJSONPayload(intPayload),
					}); ierr != nil {
						outcome.FailureClass = "interrupt_event_failed"
						outcome.Message = ierr.Error()
						outcome.Terminal = string(workgraph.TermFailed)
						term = workgraph.TermFailed
						serviceInterruptID = ""
						emit(fmt.Sprintf("interrupt_event_failed:%s: %v", id, ierr))
					} else {
						emit(fmt.Sprintf("interrupt:%s attempt=%s pid=%d class=%s", id, attemptID, intPID, InterruptClassServiceForced))
					}
				}
			} else if term == workgraph.TermCancelled ||
				strings.EqualFold(outcome.FailureClass, "forced_interrupt") ||
				strings.EqualFold(outcome.FailureClass, InterruptClassServiceForced) {
				// Executor returned cancel without Service interrupt emission.
				// Never promote to service_forced_interrupt; use distinct executor class.
				// Ambiguous "forced_interrupt" label from executor without Service pair is reclassified.
				term = workgraph.TermCancelled
				outcome.FailureClass = FailureClassExecutorCancelled
				if strings.TrimSpace(outcome.Message) == "" {
					outcome.Message = "executor cancelled without service forced_interrupt"
				}
				emit(fmt.Sprintf("executor_cancelled:%s attempt=%s (not service_forced_interrupt)", id, attemptID))
			}
			outcome.Terminal = string(term)

			// Lifecycle: never Close(succeeded)+terminal until acceptance and required
			// integration succeed. Failures finalize failed claim/event exactly once.
			closeTerm := term
			closeEvidence := childOut.OutputEvidence
			if closeTerm == workgraph.TermSucceeded {
				if strings.TrimSpace(closeEvidence) == "" {
					closeTerm = workgraph.TermFailed
					outcome.Terminal = string(closeTerm)
					outcome.FailureClass = "missing_evidence"
					outcome.Message = "refusing success close without output evidence"
					closeEvidence = "failed:missing_evidence:" + id
				}
			} else if strings.TrimSpace(closeEvidence) == "" {
				closeEvidence = "failed:" + firstNonEmpty(outcome.FailureClass, string(closeTerm)) + ":" + id
			}
			outcome.Terminal = string(closeTerm)

			// Active claim for the attempt being finalized (swapped on alternate).
			activeClaim := res.Claim
			if activeClaim == nil {
				return fail(out, StatusBlocked, "claim returned nil claim object for "+id)
			}
			effectiveOut := childOut
			isAlternate := false
			var failedAttemptIDForAlt string

			// finalizeTerminal: durable terminal event FIRST, then claim close.
			// Terminal append failure must never leave a closed claim without
			// durable terminal linkage (claim stays open for restart reconciliation).
			// Preserves cancelled/forced_interrupt — never rewrite to failed.
			finalizeTerminal := func(claim *workclaim.Claim, att string, genn int, outc *ChildOutcome, term workgraph.TerminalState, evid string, supersedes string) error {
				if claim == nil {
					return fmt.Errorf("nil claim")
				}
				if term == workgraph.TermNone || term == workgraph.TermSucceeded {
					term = workgraph.TermFailed
				}
				if strings.TrimSpace(evid) == "" {
					evid = "failed:" + firstNonEmpty(outc.FailureClass, string(term)) + ":" + id
				}
				payload := map[string]string{
					"terminal": string(term), "failure_class": outc.FailureClass,
					"output_evidence": evid,
					"work_item_id":    id,
					"attempt_id":      att,
					"generation":      fmt.Sprintf("%d", genn),
				}
				payload = mergePayloadStringMap(payload, childRoutePayloadFields(route))
				// Matching typed pair for Service forced cancel (not hard_kill_recovery).
				// forced_interrupt/service_forced_interrupt is legal ONLY with the durable
				// interrupt_id from the Service-emitted interrupt branch.
				if strings.EqualFold(outc.FailureClass, "forced_interrupt") ||
					strings.EqualFold(outc.FailureClass, InterruptClassServiceForced) {
					if !strings.EqualFold(string(term), string(workgraph.TermCancelled)) {
						return fmt.Errorf("forced_interrupt requires cancelled terminal")
					}
					if strings.TrimSpace(serviceInterruptID) == "" {
						return fmt.Errorf("forced_interrupt terminal missing interrupt_id pair (Service must emit interrupt first)")
					}
					payload["interrupt_class"] = InterruptClassServiceForced
					payload["failure_class"] = "forced_interrupt"
					payload["interrupt_id"] = serviceInterruptID
				}
				if supersedes != "" {
					payload["supersedes_attempt_id"] = supersedes
					payload["retry_attempt_id"] = att
				}
				if _, lerr := logEv(Event{
					Kind: "terminal", WorkItemID: id, AttemptID: att, Generation: genn,
					Terminal: string(term), Evidence: evid, Message: outc.FailureClass,
					FailureClass: outc.FailureClass,
					Payload:      eventJSONPayload(payload),
				}); lerr != nil {
					// Do not close — claim remains open; no closed claim without durable terminal.
					return fmt.Errorf("terminal event before close: %w", lerr)
				}
				if s.TestCrashAfterTerminal != nil {
					if cerr := s.TestCrashAfterTerminal(att, string(term)); cerr != nil {
						// Simulate crash: terminal durable, claim still open.
						return fmt.Errorf("crash after terminal: %w", cerr)
					}
				}
				outc.Terminal = string(term)
				if _, cerr := cs.Close(workclaim.CloseRequest{
					ClaimID: claim.ClaimID, Generation: claim.Generation,
					ExecutorID: "workflowrun", AttemptID: claim.AttemptID,
					Terminal: term, OutputEvidence: evid,
				}); cerr != nil {
					outc.Message = firstNonEmpty(outc.Message, "") + "; close: " + cerr.Error()
					return cerr
				}
				// Authority complete only after final terminal + claim close.
				// Production spawn (SpawnObserved/authority row) must complete; load errors fail closed.
				if authStore != nil {
					owner := AuthorityOwnerFromClaimID(claim.ClaimID)
					if owner != "" {
						loaded, lerr := storage.LoadProviderExecutionAuthority(ctx, authStore, projectID, runID, att)
						if lerr != nil {
							// Fake/no-spawn: missing authority is OK. Production spawn must exist.
							if spawnAuth.ProviderPID > 0 || (spawnPIDLogged && att == attemptID) {
								return fmt.Errorf("authority load after terminal: %w", lerr)
							}
						} else if loaded.ProviderPID > 0 {
							if err := CompleteChildExecutionAuthority(ctx, authStore, projectID, runID, att, owner, int64(claim.Generation), string(term), now()); err != nil {
								return fmt.Errorf("authority complete after terminal: %w", err)
							}
						}
					}
				}
				return nil
			}
			finalizeFailed := func(claim *workclaim.Claim, att string, genn int, outc *ChildOutcome, evid string, supersedes string) error {
				return finalizeTerminal(claim, att, genn, outc, workgraph.TermFailed, evid, supersedes)
			}
			// finalizeSucceeded: durable terminal event FIRST, then claim close.
			// Terminal append failure must never leave succeeded claim without
			// durable terminal linkage.
			finalizeSucceeded := func(claim *workclaim.Claim, att string, genn int, outc *ChildOutcome, evid string, supersedes string) error {
				if claim == nil {
					return fmt.Errorf("nil claim")
				}
				payload := map[string]string{
					"terminal": string(workgraph.TermSucceeded), "failure_class": "",
					"output_evidence": evid,
				}
				payload = mergePayloadStringMap(payload, childRoutePayloadFields(route))
				if supersedes != "" {
					payload["supersedes_attempt_id"] = supersedes
					payload["retry_attempt_id"] = att
				}
				if _, lerr := logEv(Event{
					Kind: "terminal", WorkItemID: id, AttemptID: att, Generation: genn,
					Terminal: string(workgraph.TermSucceeded), Evidence: evid, Message: "",
					FailureClass: "",
					Payload:      eventJSONPayload(payload),
				}); lerr != nil {
					// Do not close as succeeded — claim remains open; fail path will terminalize.
					return fmt.Errorf("terminal event before close: %w", lerr)
				}
				if s.TestCrashAfterTerminal != nil {
					if cerr := s.TestCrashAfterTerminal(att, string(workgraph.TermSucceeded)); cerr != nil {
						return fmt.Errorf("crash after terminal: %w", cerr)
					}
				}
				outc.Terminal = string(workgraph.TermSucceeded)
				if _, cerr := cs.Close(workclaim.CloseRequest{
					ClaimID: claim.ClaimID, Generation: claim.Generation,
					ExecutorID: "workflowrun", AttemptID: claim.AttemptID,
					Terminal: workgraph.TermSucceeded, OutputEvidence: evid,
				}); cerr != nil {
					outc.Message = "close: " + cerr.Error()
					return cerr
				}
				if authStore != nil {
					owner := AuthorityOwnerFromClaimID(claim.ClaimID)
					if owner != "" {
						loaded, lerr := storage.LoadProviderExecutionAuthority(ctx, authStore, projectID, runID, att)
						if lerr != nil {
							if spawnAuth.ProviderPID > 0 || spawnPIDLogged {
								return fmt.Errorf("authority load after success terminal: %w", lerr)
							}
						} else if loaded.ProviderPID > 0 {
							if err := CompleteChildExecutionAuthority(ctx, authStore, projectID, runID, att, owner, int64(claim.Generation), string(workgraph.TermSucceeded), now()); err != nil {
								return fmt.Errorf("authority complete after success terminal: %w", err)
							}
						}
					}
				}
				return nil
			}

			// --- Failure path (incl. model_unavailable alternate) ---
			if closeTerm != workgraph.TermSucceeded {
				// Preserve exact terminal (cancelled stays cancelled).
				if ferr := finalizeTerminal(activeClaim, attemptID, eventGen, &outcome, closeTerm, closeEvidence, ""); ferr != nil {
					out.Children = append(out.Children, outcome)
					return failBlockedJoin(out, s.HomeDir, projectID, runID, "finalize terminal "+id+": "+ferr.Error(), releaseWorktreePeak)
				}
				// Generation-safe same-depth alternate after typed model_unavailable.
				// Release primary worktree occupancy before alternate launch (sequential peak).
				if strings.EqualFold(strings.TrimSpace(outcome.FailureClass), "model_unavailable") && ctx.Err() == nil {
					if cerr := releaseWorktreePeak(); cerr != nil {
						return fail(out, StatusBlocked, "worktree cleanup: "+cerr.Error())
					}
					failedAttemptID := attemptID
					failedProv, failedModel := outcome.Provider, outcome.Model
					failedDepth := firstNonEmpty(outcome.Depth, route.Depth)
					failedPerm := firstNonEmpty(outcome.Permission, route.Permission, "bounded_write")
					if readOnly {
						failedPerm = "read-only"
					}
					failedAcc := firstNonEmpty(outcome.AccountRef, route.AccountRef)
					failedWin := firstNonEmpty(outcome.WindowKind, route.WindowKind)
					failedRes := firstNonEmpty(outcome.ReservationID, route.ReservationID)
					failedActual, failedSource := outcome.ActualCapacity, outcome.ActualSource
					failedOutcome := outcome
					reqDepth := firstNonEmpty(route.Depth, outcome.Depth)
					reqPerm := firstNonEmpty(route.Permission, "bounded_write")
					if readOnly {
						reqPerm = "read-only"
					}
					alt := pickSameDepthAlternate(req.SameDepthAlternates[id], failedProv, failedModel, reqDepth, reqPerm)
					if alt.Provider != "" {
						if req.CapacityReroute == nil {
							out.Children = append(out.Children, failedOutcome)
							ev[id] = workgraph.TermFailed
							return fail(out, StatusBlocked, "model_unavailable alternate requires CapacityReroute; refusing unreserved exec for "+id)
						}
						evFail, lerr := logEv(Event{
							Kind: "model_unavailable", WorkItemID: id, AttemptID: failedAttemptID,
							Terminal: string(workgraph.TermFailed), Message: failedProv + "/" + failedModel,
							Payload: eventJSONPayload(map[string]string{
								"work_item_id": id, "attempt_id": failedAttemptID,
								"provider": failedProv, "model": failedModel, "failure_class": "model_unavailable",
							}),
							Evidence: closeEvidence, Generation: eventGen,
						})
						if lerr != nil {
							out.Children = append(out.Children, failedOutcome)
							ev[id] = workgraph.TermFailed
							return failBlockedJoin(out, s.HomeDir, projectID, runID, "model_unavailable event: "+lerr.Error(), nil)
						}
						out.Children = append(out.Children, failedOutcome)

						newGen := gen + 1
						newAttemptID := AttemptID(id, out.PlanDigest, runID, newGen)
						res2, cerr2 := cs.Claim(workclaim.ClaimRequest{
							ProjectID: projectID, Graph: g, Evidence: ev, WorkItemID: id,
							AttemptID: newAttemptID, ExecutorID: "workflowrun", Lease: time.Minute,
							SupersedesAttemptID: failedAttemptID,
							PlanDigest:          out.PlanDigest,
							TaskClass:           cc.TaskClass,
							ChildContractDigest: cc.Digest, // same assignment digest across alternates
						})
						if cerr2 != nil || res2.Code != workclaim.ResultClaimed {
							ev[id] = workgraph.TermFailed
							return failBlockedJoin(out, s.HomeDir, projectID, runID, fmt.Sprintf("alternate claim %s: %v code=%v", id, cerr2, res2.Code), nil)
						}
						claimed[id]++
						out.ClaimCount++
						altEventGen, altEgErr := ClaimGenerationFromAttemptID(newAttemptID)
						if altEgErr != nil {
							return fail(out, StatusBlocked, fmt.Sprintf("alternate claim %s: %v", id, altEgErr))
						}
						if altEventGen <= 0 {
							altEventGen = newGen + 1 // never zero
						}
						evClaim, lerr := logEv(Event{
							Kind: "claim", WorkItemID: id, AttemptID: newAttemptID, Generation: altEventGen,
							Payload: eventJSONPayload(map[string]string{
								"supersedes_attempt_id": failedAttemptID, "retry_attempt_id": newAttemptID,
								"work_item_id": id, "attempt_id": newAttemptID,
							}),
						})
						if lerr != nil {
							altFail := ChildOutcome{
								WorkItemID: id,
								TaskClass:  cc.TaskClass, ExecutionPlanDigest: out.PlanDigest,
								ChildContractDigest: cc.Digest, Generation: altEventGen, Provider: alt.Provider, Model: alt.Model, Depth: reqDepth,
								AttemptID: newAttemptID, FailureClass: "event_log",
								Message: "claim event: " + lerr.Error(), SupersedesAttemptID: failedAttemptID,
							}
							_ = finalizeTerminal(res2.Claim, newAttemptID, altEventGen, &altFail, workgraph.TermFailed,
								"failed:event_log:"+id, failedAttemptID)
							out.Children = append(out.Children, altFail)
							return failBlockedJoin(out, s.HomeDir, projectID, runID, altFail.Message, nil)
						}

						altPerm := firstNonEmpty(alt.Permission, reqPerm)
						altDepth := firstNonEmpty(alt.Effort, reqDepth)
						altRoute := ChildRoute{
							Provider: alt.Provider, Model: alt.Model, Depth: altDepth,
							Permission: altPerm, TaskClass: cc.TaskClass,
							AccountRef: strings.TrimSpace(alt.AccountRef),
							InstallRef: strings.TrimSpace(alt.InstallRef),
							WindowKind: strings.TrimSpace(alt.WindowKind),
							RouteReason: fmt.Sprintf(
								"model_unavailable_reroute from %s/%s attempt=%s; winner=%s/%s depth=%s permission=%s",
								failedProv, failedModel, failedAttemptID, alt.Provider, alt.Model, altDepth, altPerm,
							),
						}
						capIn := CapacityRerouteInput{
							WorkItemID: id, FailedAttemptID: failedAttemptID, PriorHoldAttempt: failedAttemptID,
							NewAttemptID:        newAttemptID,
							PlanDigest:          out.PlanDigest,
							GraphDigest:         out.GraphDigest,
							TaskClass:           cc.TaskClass,
							ChildContractDigest: cc.Digest,
							FailedProvider:      failedProv,
							FailedModel:         failedModel,
							FailedDepth:         failedDepth,
							FailedPermission:    failedPerm,
							FailedAccountRef:    failedAcc,
							FailedWindowKind:    failedWin,
							FailedReservationID: failedRes,
							AltProvider:         alt.Provider,
							AltModel:            alt.Model,
							AltDepth:            altDepth,
							AltPermission:       altPerm,
							AltAccountRef:       strings.TrimSpace(alt.AccountRef),
							AltInstallRef:       strings.TrimSpace(alt.InstallRef),
							AltWindowKind:       strings.TrimSpace(alt.WindowKind),
							Depth:               altDepth,
							Permission:          altPerm,
							RouteReason:         altRoute.RouteReason,
							PriorActual:         failedActual,
							PriorSource:         failedSource,
							SupersedesEvent:     "event_id=" + evFail.EventID + ";claim_event_id=" + evClaim.EventID,
						}
						capacityTransferred := false
						cr, rerr := req.CapacityReroute.OnModelUnavailableAlternate(capIn)
						if rerr != nil {
							// Always compensate after hook invocation error (may have reserved).
							compMsg := ""
							if cerr := req.CapacityReroute.CompensateAlternateHold(newAttemptID); cerr != nil {
								compMsg = "; compensate: " + cerr.Error()
							}
							altFail := ChildOutcome{
								WorkItemID: id,
								TaskClass:  cc.TaskClass, ExecutionPlanDigest: out.PlanDigest,
								ChildContractDigest: cc.Digest, Generation: altEventGen, Provider: alt.Provider, Model: alt.Model, Depth: reqDepth,
								AttemptID: newAttemptID, FailureClass: "capacity_refused",
								Message: rerr.Error() + compMsg, SupersedesAttemptID: failedAttemptID,
							}
							if ferr := finalizeTerminal(res2.Claim, newAttemptID, altEventGen, &altFail, workgraph.TermFailed,
								"failed:capacity_refused:"+id, failedAttemptID); ferr != nil {
								altFail.Message += "; finalize: " + ferr.Error()
							}
							out.Children = append(out.Children, altFail)
							ev[id] = workgraph.TermFailed
							return failBlockedJoin(out, s.HomeDir, projectID, runID, "capacity reroute "+id+": "+altFail.Message, nil)
						}
						if verr := validateCapacityRerouteResult(cr, capIn); verr != nil {
							compMsg := ""
							if cerr := req.CapacityReroute.CompensateAlternateHold(newAttemptID); cerr != nil {
								compMsg = "; compensate: " + cerr.Error()
							}
							altFail := ChildOutcome{
								WorkItemID: id,
								TaskClass:  cc.TaskClass, ExecutionPlanDigest: out.PlanDigest,
								ChildContractDigest: cc.Digest, Generation: altEventGen, Provider: alt.Provider, Model: alt.Model, Depth: reqDepth,
								AttemptID: newAttemptID, FailureClass: "capacity_refused",
								Message: verr.Error() + compMsg, SupersedesAttemptID: failedAttemptID,
							}
							if ferr := finalizeTerminal(res2.Claim, newAttemptID, altEventGen, &altFail, workgraph.TermFailed,
								"failed:capacity_contract:"+id, failedAttemptID); ferr != nil {
								altFail.Message += "; finalize: " + ferr.Error()
							}
							out.Children = append(out.Children, altFail)
							ev[id] = workgraph.TermFailed
							return failBlockedJoin(out, s.HomeDir, projectID, runID, "capacity contract "+id+": "+altFail.Message, nil)
						}
						capacityTransferred = true
						altRoute.AccountRef = strings.TrimSpace(cr.AccountRef)
						altRoute.WindowKind = strings.TrimSpace(firstNonEmpty(cr.WindowKind, cr.AlternateTransition.WindowKind))
						altRoute.ReservationID = strings.TrimSpace(cr.ReservationID)
						altRoute.Permission = altPerm
						altRoute.Depth = altDepth
						out.CapacityTransitions = append(out.CapacityTransitions, cr.PriorTransition, cr.AlternateTransition)

						compensateStrict := func() error {
							if !capacityTransferred || req.CapacityReroute == nil {
								return nil
							}
							return req.CapacityReroute.CompensateAlternateHold(newAttemptID)
						}

						evReroute, lerr := logEv(Event{
							Kind: "reroute", WorkItemID: id, AttemptID: newAttemptID, Generation: altEventGen,
							Message: altRoute.RouteReason,
							Payload: eventJSONPayload(map[string]string{
								"work_item_id": id, "supersedes_attempt_id": failedAttemptID,
								"retry_attempt_id": newAttemptID, "alt_provider": altRoute.Provider,
								"alt_model": altRoute.Model, "depth": altRoute.Depth, "permission": altRoute.Permission,
								"account_ref":                altRoute.AccountRef,
								"window_kind":                altRoute.WindowKind,
								"reservation_id":             altRoute.ReservationID,
								"model_unavailable_event_id": evFail.EventID, "claim_event_id": evClaim.EventID,
							}),
						})
						if lerr != nil {
							compMsg := ""
							if compErr := compensateStrict(); compErr != nil {
								compMsg = "; compensate: " + compErr.Error()
							}
							altFail := ChildOutcome{
								WorkItemID: id,
								TaskClass:  cc.TaskClass, ExecutionPlanDigest: out.PlanDigest,
								ChildContractDigest: cc.Digest, Generation: altEventGen, Provider: alt.Provider, Model: alt.Model, Depth: reqDepth,
								AttemptID: newAttemptID, FailureClass: "event_log",
								Message:             "reroute event: " + lerr.Error() + compMsg,
								SupersedesAttemptID: failedAttemptID,
							}
							_ = finalizeTerminal(res2.Claim, newAttemptID, altEventGen, &altFail, workgraph.TermFailed,
								"failed:reroute_event:"+id, failedAttemptID)
							out.Children = append(out.Children, altFail)
							return failBlockedJoin(out, s.HomeDir, projectID, runID, altFail.Message, nil)
						}
						emit(fmt.Sprintf("child.reroute:%s from=%s/%s to=%s/%s gen=%d", id, failedProv, failedModel, alt.Provider, alt.Model, newGen))
						evLaunch, lerr := logEv(Event{
							Kind: "launch", WorkItemID: id, AttemptID: newAttemptID, Generation: altEventGen,
							Message: altRoute.Provider + "/" + altRoute.Model,
							Payload: eventJSONPayload(map[string]string{
								"work_item_id": id, "retry_attempt_id": newAttemptID,
								"reroute_event_id": evReroute.EventID, "supersedes_attempt_id": failedAttemptID,
								"provider": altRoute.Provider, "model": altRoute.Model,
								"depth": altRoute.Depth, "permission": altRoute.Permission,
								"account_ref": altRoute.AccountRef, "install_ref": altRoute.InstallRef,
								"window_kind":    altRoute.WindowKind,
								"reservation_id": altRoute.ReservationID, "route_reason": altRoute.RouteReason,
							}),
						})
						if lerr != nil {
							compMsg := ""
							if compErr := compensateStrict(); compErr != nil {
								compMsg = "; compensate: " + compErr.Error()
							}
							altFail := ChildOutcome{
								WorkItemID: id,
								TaskClass:  cc.TaskClass, ExecutionPlanDigest: out.PlanDigest,
								ChildContractDigest: cc.Digest, Generation: altEventGen, Provider: alt.Provider, Model: alt.Model, Depth: reqDepth,
								AttemptID: newAttemptID, FailureClass: "event_log",
								Message:             "launch event: " + lerr.Error() + compMsg,
								SupersedesAttemptID: failedAttemptID,
							}
							_ = finalizeTerminal(res2.Claim, newAttemptID, altEventGen, &altFail, workgraph.TermFailed,
								"failed:launch_event:"+id, failedAttemptID)
							out.Children = append(out.Children, altFail)
							return failBlockedJoin(out, s.HomeDir, projectID, runID, altFail.Message, nil)
						}
						launches++
						out.LaunchCount = launches
						// Primary child already finished occupancy; alternate is sequential.
						// leave primary process peak already released via finishChild only at
						// end of outer child — for alternate, enter/leave within alt path.
						rerouteRef := fmt.Sprintf(
							"event_id=%s;event_id=%s;event_id=%s;event_id=%s;supersedes_attempt_id=%s;retry_attempt_id=%s",
							evFail.EventID, evClaim.EventID, evReroute.EventID, evLaunch.EventID, failedAttemptID, newAttemptID,
						)
						childIn2 := ChildExecInput{
							ProjectID: projectID, GraphID: g.GraphID, WorkItemID: id,
							ClaimID: res2.Claim.ClaimID, AttemptID: newAttemptID,
							Intent: it.Intent, Route: altRoute, RepoPath: req.RepoPath,
							BaseRef: firstNonEmpty(goalBranch, baseRef), ReadOnly: readOnly,
						}
						var altSpawnLogged bool
						var altProcessEntered bool
						var altSpawnStart ProcessStart
						var altSpawnErr error
						var altSpawnAuth storage.ProviderExecutionAuthority
						altClaimGen := int64(altEventGen)
						if res2.Claim != nil && res2.Claim.Generation > 0 {
							altClaimGen = int64(res2.Claim.Generation)
						}
						altOwner := ""
						if res2.Claim != nil {
							altOwner = AuthorityOwnerFromClaimID(res2.Claim.ClaimID)
						}
						altStorePath, _ := AuthorityStorePath(s.HomeDir, projectID, runID)
						altDiagPath, _ := GuardianDiagnosticPath(s.HomeDir, projectID, runID, newAttemptID)
						// Sequential alternate: re-enter worktree lease (primary already released).
						// Refuse allocation if primary cleanup left a lease active.
						childIn2.OnWorktreeAllocated = func(path string) error {
							if wtActive {
								return fmt.Errorf("workflowrun: primary worktree still active at %s; refuse alternate %s", wtPath, path)
							}
							peaks.enterWorktree()
							wtActive = true
							wtPath = path
							out.WorktreePeak = peaks.WorktreePeak
							out.WorktreeActive = peaks.activeWorktree
							return nil
						}
						childIn2.Guardian = BuildChildGuardianOptions(altStorePath, altDiagPath, projectID, runID, newAttemptID, altOwner, altClaimGen, &altSpawnAuth)
						childIn2.OnProcessStart = func(ps ProcessStart) error {
							if altSpawnLogged {
								altSpawnErr = fmt.Errorf("workflowrun: duplicate process-start for %s/%s (exactly-once pid)", id, newAttemptID)
								return altSpawnErr
							}
							if err := ValidateProcessStart(ps); err != nil {
								altSpawnErr = err
								return err
							}
							if altOwner == "" {
								altSpawnErr = fmt.Errorf("workflowrun: alternate claim-bound owner required")
								return altSpawnErr
							}
							if authStore != nil {
								persisted, aerr := PersistChildExecutionAuthority(ctx, authStore, projectID, runID, newAttemptID, altOwner, altClaimGen, ps, ps.WorktreePath, ps.LogPath, ps.ObservedAt)
								if aerr != nil {
									altSpawnErr = aerr
									return aerr
								}
								if err := ValidateAuthorityMatchesSpawn(persisted, ps, altClaimGen, newAttemptID, altOwner); err != nil {
									altSpawnErr = err
									return err
								}
								// SpawnPhase=authority_persisted in same Persist write (not a later event).
								altSpawnAuth = persisted
							}
							evPID := Event{
								Kind: "pid", WorkItemID: id, AttemptID: newAttemptID, Generation: altEventGen,
								PID: ps.PID, Payload: eventJSONPayload(mergePayloadStringMap(processStartPayload(ps), childRoutePayloadFields(altRoute))),
								Message: "spawn process identity (alternate)",
							}
							if err := ValidatePIDEventPayload(evPID); err != nil {
								if authStore != nil && altOwner != "" && altSpawnAuth.ProviderPID > 0 {
									if terr := TransitionChildSpawnPhase(ctx, authStore, projectID, runID, newAttemptID, altOwner, altClaimGen, SpawnPhasePIDEventFailed, now()); terr != nil {
										altSpawnErr = fmt.Errorf("%w; spawn_phase: %v", err, terr)
										return altSpawnErr
									}
								}
								altSpawnErr = err
								return err
							}
							outPID, perr := logEv(evPID)
							if perr != nil {
								if authStore != nil && altOwner != "" && altSpawnAuth.ProviderPID > 0 {
									if terr := TransitionChildSpawnPhase(ctx, authStore, projectID, runID, newAttemptID, altOwner, altClaimGen, SpawnPhasePIDEventFailed, now()); terr != nil {
										altSpawnErr = fmt.Errorf("%w; spawn_phase: %v", perr, terr)
										return altSpawnErr
									}
								}
								altSpawnErr = perr
								return perr
							}
							if authStore != nil && altOwner != "" && altSpawnAuth.ProviderPID > 0 {
								if terr := TransitionChildSpawnPhase(ctx, authStore, projectID, runID, newAttemptID, altOwner, altClaimGen, SpawnPhasePIDEventPersisted, now()); terr != nil {
									altSpawnErr = terr
									return terr
								}
								altSpawnAuth.SpawnPhase = SpawnPhasePIDEventPersisted
							}
							altSpawnLogged = true
							altSpawnStart = ps
							peaks.enterProcess()
							altProcessEntered = true
							out.ProcessPeak = peaks.ProcessPeak
							if s.TestAfterPIDEvent != nil {
								s.TestAfterPIDEvent(outPID)
							}
							return nil
						}
						childOut2, altExecErr := exec.Execute(ctx, childIn2)
						if altProcessEntered {
							peaks.leaveProcess()
							out.ProcessPeak = peaks.ProcessPeak
						} else if childOut2.ProcessPID > 0 && !childOut2.SpawnObserved {
							// Fake occupancy pulse only — no durable production PID.
							peaks.enterProcess()
							peaks.leaveProcess()
							out.ProcessPeak = peaks.ProcessPeak
						}
						// Alternate executor error never coexists with success.
						if altExecErr != nil {
							if childOut2.Terminal == workgraph.TermSucceeded || childOut2.Terminal == workgraph.TermNone {
								childOut2.Terminal = workgraph.TermFailed
							}
							if strings.TrimSpace(childOut2.FailureClass) == "" {
								childOut2.FailureClass = "executor_error"
							}
							if strings.TrimSpace(childOut2.Message) == "" {
								childOut2.Message = altExecErr.Error()
							}
							if strings.TrimSpace(childOut2.OutputEvidence) == "" {
								childOut2.OutputEvidence = "failed:executor_error:" + id
							}
						}
						if altSpawnErr != nil {
							altFail := ChildOutcome{
								WorkItemID: id, AttemptID: newAttemptID, Generation: altEventGen,
								TaskClass: cc.TaskClass, ExecutionPlanDigest: out.PlanDigest, ChildContractDigest: cc.Digest,
								Provider: alt.Provider, Model: alt.Model, Depth: reqDepth,
								FailureClass: "pid_event_failed", Message: altSpawnErr.Error(),
								SupersedesAttemptID: failedAttemptID,
							}
							_ = finalizeTerminal(res2.Claim, newAttemptID, altEventGen, &altFail, workgraph.TermFailed,
								"failed:pid_event:"+id, failedAttemptID)
							out.Children = append(out.Children, altFail)
							return failBlockedJoin(out, s.HomeDir, projectID, runID, "alternate pid event: "+altSpawnErr.Error(), releaseWorktreePeak)
						}
						if altSpawnLogged {
							if err := crossCheckSpawnPID(altSpawnStart, childOut2); err != nil {
								altFail := ChildOutcome{
									WorkItemID: id, AttemptID: newAttemptID, Generation: altEventGen,
									TaskClass: cc.TaskClass, ExecutionPlanDigest: out.PlanDigest, ChildContractDigest: cc.Digest,
									Provider: alt.Provider, Model: alt.Model, Depth: reqDepth,
									FailureClass: "pid_identity_mismatch", Message: err.Error(),
									SupersedesAttemptID: failedAttemptID,
								}
								_ = finalizeTerminal(res2.Claim, newAttemptID, altEventGen, &altFail, workgraph.TermFailed,
									"failed:pid_mismatch:"+id, failedAttemptID)
								out.Children = append(out.Children, altFail)
								return failBlockedJoin(out, s.HomeDir, projectID, runID, "alternate pid identity mismatch: "+err.Error(), releaseWorktreePeak)
							}
						}
						// Fake alternate: never emit durable production pid without authority.
						effectiveOut = childOut2
						// Same assignment contract as original; only attempt/gen/route change.
						inv2 := childOut2.InvokedRoute
						outcome = ChildOutcome{
							WorkItemID: id,
							TaskClass:  cc.TaskClass, ExecutionPlanDigest: out.PlanDigest,
							ChildContractDigest: cc.Digest, Generation: altEventGen,
							Provider:      firstNonEmpty(inv2.Provider, childOut2.Provider, altRoute.Provider),
							Model:         firstNonEmpty(inv2.Model, childOut2.Model, altRoute.Model),
							Depth:         firstNonEmpty(inv2.Depth, childOut2.Depth, altRoute.Depth),
							Permission:    firstNonEmpty(inv2.Permission, altRoute.Permission),
							AccountRef:    firstNonEmpty(inv2.AccountRef, altRoute.AccountRef),
							InstallRef:    firstNonEmpty(inv2.InstallRef, altRoute.InstallRef),
							WindowKind:    firstNonEmpty(inv2.WindowKind, altRoute.WindowKind),
							ReservationID: firstNonEmpty(inv2.ReservationID, altRoute.ReservationID),
							RouteReason:   altRoute.RouteReason,
							AttemptID:     newAttemptID, WorktreePath: childOut2.WorktreePath,
							OutputEvidence: childOut2.OutputEvidence, ExitCode: childOut2.ExitCode,
							FailureClass: childOut2.FailureClass, Message: childOut2.Message,
							ActualCapacity: childOut2.ActualCapacity, ActualSource: childOut2.ActualSource,
							InputTokens: childOut2.InputTokens, OutputTokens: childOut2.OutputTokens,
							ActualSources: ActualRouteSources{
								Model: childOut2.ActualSources.Model, Effort: childOut2.ActualSources.Effort,
								Permission: childOut2.ActualSources.Permission, Account: childOut2.ActualSources.Account,
								Install: childOut2.ActualSources.Install,
							},
							ArgvDigest:          childOut2.ArgvDigest,
							FilesTouched:        childOut2.FilesTouched,
							SupersedesAttemptID: failedAttemptID, RerouteEventRef: rerouteRef,
						}
						term2 := childOut2.Terminal
						if term2 == workgraph.TermNone {
							term2 = workgraph.TermFailed
							if outcome.FailureClass == "" {
								outcome.FailureClass = "missing_terminal"
							}
						}
						if ctx.Err() != nil && term2 != workgraph.TermSucceeded {
							term2 = workgraph.TermCancelled
							outcome.FailureClass = "forced_interrupt"
							out.Interrupted = true
							if out.AbortedAttempts == nil {
								out.AbortedAttempts = map[string]string{}
							}
							out.AbortedAttempts[id] = newAttemptID
							// Typed service_forced_interrupt (not hard_kill_recovery).
							intPID, intPIDErr := interruptPIDFromSpawn(altSpawnLogged, altSpawnStart, childOut2)
							if intPIDErr != nil {
								outcome.FailureClass = "interrupt_pid_mismatch"
								outcome.Message = intPIDErr.Error()
								outcome.Terminal = string(workgraph.TermFailed)
								term2 = workgraph.TermFailed
								serviceInterruptID = ""
								emit(fmt.Sprintf("interrupt_pid_mismatch:alt:%s: %v", id, intPIDErr))
							} else {
								serviceInterruptID = newInterruptID(newAttemptID, altEventGen)
								altIntPL := map[string]string{
									"failure_class": "forced_interrupt", "interrupt_class": InterruptClassServiceForced,
									"interrupt_id": serviceInterruptID,
									"terminal":     string(workgraph.TermCancelled),
								}
								altIntPL = mergePayloadStringMap(altIntPL, childRoutePayloadFields(altRoute))
								if intPID > 0 {
									altIntPL["pid"] = fmt.Sprintf("%d", intPID)
								}
								if _, ierr := logEv(Event{
									Kind: "interrupt", WorkItemID: id, AttemptID: newAttemptID, Generation: altEventGen,
									PID: intPID, Message: "forced interrupt; alternate attempt aborted",
									Terminal: string(workgraph.TermCancelled), FailureClass: "forced_interrupt",
									Payload: eventJSONPayload(altIntPL),
								}); ierr != nil {
									outcome.FailureClass = "interrupt_event_failed"
									outcome.Message = ierr.Error()
									outcome.Terminal = string(workgraph.TermFailed)
									term2 = workgraph.TermFailed
									serviceInterruptID = ""
									emit(fmt.Sprintf("interrupt_event_failed:alt:%s: %v", id, ierr))
								} else {
									emit(fmt.Sprintf("interrupt:alt:%s attempt=%s pid=%d", id, newAttemptID, intPID))
								}
							}
						} else if term2 == workgraph.TermCancelled ||
							strings.EqualFold(outcome.FailureClass, "forced_interrupt") ||
							strings.EqualFold(outcome.FailureClass, InterruptClassServiceForced) {
							// Executor-local cancel on alternate without Service interrupt.
							term2 = workgraph.TermCancelled
							outcome.FailureClass = FailureClassExecutorCancelled
							serviceInterruptID = ""
							if strings.TrimSpace(outcome.Message) == "" {
								outcome.Message = "executor cancelled alternate without service forced_interrupt"
							}
							emit(fmt.Sprintf("executor_cancelled:alt:%s attempt=%s", id, newAttemptID))
						}
						closeTerm = term2
						closeEvidence = childOut2.OutputEvidence
						if closeTerm == workgraph.TermSucceeded && strings.TrimSpace(closeEvidence) == "" {
							closeTerm = workgraph.TermFailed
							outcome.FailureClass = "missing_evidence"
							closeEvidence = "failed:missing_evidence:" + id
						} else if closeTerm != workgraph.TermSucceeded && strings.TrimSpace(closeEvidence) == "" {
							closeEvidence = "failed:" + firstNonEmpty(outcome.FailureClass, string(closeTerm)) + ":" + id
						}
						outcome.Terminal = string(closeTerm)
						if res2.Claim == nil {
							if cerr := releaseWorktreePeak(); cerr != nil {
								return fail(out, StatusBlocked, "worktree cleanup: "+cerr.Error())
							}
							return fail(out, StatusBlocked, "alternate claim nil for "+id)
						}
						activeClaim = res2.Claim
						attemptID = newAttemptID
						eventGen = altEventGen
						route = altRoute
						isAlternate = true
						failedAttemptIDForAlt = failedAttemptID
						if closeTerm != workgraph.TermSucceeded {
							if ferr := finalizeFailed(activeClaim, attemptID, eventGen, &outcome, closeEvidence, failedAttemptID); ferr != nil {
								out.Children = append(out.Children, outcome)
								return failBlockedJoin(out, s.HomeDir, projectID, runID, "alternate finalize failed: "+ferr.Error(), releaseWorktreePeak)
							}
							if cerr := releaseWorktreePeak(); cerr != nil {
								return fail(out, StatusBlocked, "worktree cleanup: "+cerr.Error())
							}
						}
					}
				} else {
					// Non-alternate failure: worktree no longer needed after terminal.
					if cerr := releaseWorktreePeak(); cerr != nil {
						return fail(out, StatusBlocked, "worktree cleanup: "+cerr.Error())
					}
				}
			}

			// --- Success path: accept + integrate BEFORE Close(succeeded)/terminal ---
			if closeTerm == workgraph.TermSucceeded {
				if aerr := AcceptSucceededChild(id, it.Intent, it.Owner, effectiveOut.FilesTouched, effectiveOut.WorktreePath, closeEvidence); aerr != nil {
					outcome.FailureClass = "acceptance_failed"
					outcome.Message = aerr.Error()
					closeEvidence = "failed:acceptance_failed:" + id
					supersedes := ""
					if isAlternate {
						supersedes = failedAttemptIDForAlt
					}
					if ferr := finalizeFailed(activeClaim, attemptID, eventGen, &outcome, closeEvidence, supersedes); ferr != nil {
						out.Children = append(out.Children, outcome)
						return failBlockedJoin(out, s.HomeDir, projectID, runID, "accept finalize: "+ferr.Error(), releaseWorktreePeak)
					}
					ev[id] = workgraph.TermFailed
					out.Children = append(out.Children, outcome)
					emit(fmt.Sprintf("accept.fail:%s err=%s", id, aerr.Error()))
					if it.Status == workgraph.ItemRequired {
						return failBlockedJoin(out, s.HomeDir, projectID, runID, "accept "+id+": "+aerr.Error(), releaseWorktreePeak)
					}
					// Non-required: still persist partial + cleanup occupancy.
					if _, jerr := failBlockedJoin(out, s.HomeDir, projectID, runID, "", releaseWorktreePeak); jerr != nil {
						return out, jerr
					}
					continue
				}
				emit(fmt.Sprintf("accept.ok:%s role=%s", id, ClassifyTaskRole(id, it.Intent, it.Owner)))

				if doIntegrate && integrator != nil {
					ic, ierr := integrator.IntegrateChild(ctx, IntegrateRequest{
						RepoPath: req.RepoPath, GoalBranch: goalBranch,
						WorkItemID: id, AttemptID: attemptID,
						ChildWorktree: effectiveOut.WorktreePath,
						ProductFiles:  effectiveOut.FilesTouched,
						Intent:        it.Intent,
					})
					if ierr != nil {
						outcome.FailureClass = "integrate_failed"
						outcome.Message = ierr.Error()
						closeEvidence = "failed:integrate_failed:" + id
						supersedes := ""
						if isAlternate {
							supersedes = failedAttemptIDForAlt
						}
						if ferr := finalizeFailed(activeClaim, attemptID, eventGen, &outcome, closeEvidence, supersedes); ferr != nil {
							out.Children = append(out.Children, outcome)
							return failBlockedJoin(out, s.HomeDir, projectID, runID, "integrate finalize: "+ferr.Error(), releaseWorktreePeak)
						}
						ev[id] = workgraph.TermFailed
						out.Children = append(out.Children, outcome)
						emit(fmt.Sprintf("integrate.fail:%s err=%s", id, ierr.Error()))
						if it.Status == workgraph.ItemRequired {
							return failBlockedJoin(out, s.HomeDir, projectID, runID, "integrate "+id+": "+ierr.Error(), releaseWorktreePeak)
						}
						if _, jerr := failBlockedJoin(out, s.HomeDir, projectID, runID, "", releaseWorktreePeak); jerr != nil {
							return out, jerr
						}
						continue
					}
					outcome.IntegrateCommitSHA = ic.CommitSHA
					outcome.FilesTouched = firstNonEmptySlice(ic.Files, outcome.FilesTouched)
					out.IntegrateCommits = append(out.IntegrateCommits, ic)
					integrated = append(integrated, id)

					supersedes := ""
					if isAlternate {
						supersedes = failedAttemptIDForAlt
					}
					// Required order: IntegrateChild succeeds → critical integrate event
					// persists → only then close/log succeeded. Integrate-event failure
					// must never leave succeeded claim/terminal.
					intPayload := map[string]string{
						"work_item_id": id, "attempt_id": attemptID, "commit_sha": ic.CommitSHA,
					}
					if supersedes != "" {
						intPayload["retry_attempt_id"] = attemptID
						intPayload["supersedes_attempt_id"] = supersedes
					}
					if _, lerr := logEv(Event{
						Kind: "integrate", WorkItemID: id, AttemptID: attemptID,
						CommitSHA: ic.CommitSHA, Generation: eventGen,
						Payload: eventJSONPayload(intPayload),
					}); lerr != nil {
						outcome.FailureClass = "integrate_event_failed"
						outcome.Message = lerr.Error()
						closeEvidence = "failed:integrate_event:" + id
						if ferr := finalizeTerminal(activeClaim, attemptID, eventGen, &outcome, workgraph.TermFailed, closeEvidence, supersedes); ferr != nil {
							outcome.Message += "; finalize: " + ferr.Error()
						}
						// Commit landed; preserve IntegrateCommits identity without succeeded terminal.
						out.Children = append(out.Children, outcome)
						ev[id] = workgraph.TermFailed
						return failBlockedJoin(out, s.HomeDir, projectID, runID, "integrate event: "+outcome.Message, releaseWorktreePeak)
					}
					if ferr := finalizeSucceeded(activeClaim, attemptID, eventGen, &outcome, closeEvidence, supersedes); ferr != nil {
						// Never report succeeded without durable terminal + closed claim.
						outcome.Terminal = ""
						outcome.FailureClass = firstNonEmpty(outcome.FailureClass, "terminal_event_failed")
						outcome.Message = ferr.Error()
						out.Children = append(out.Children, outcome)
						return failBlockedJoin(out, s.HomeDir, projectID, runID, "success finalize: "+ferr.Error(), releaseWorktreePeak)
					}
					if ic.Skipped {
						emit(fmt.Sprintf("integrate.skip:%s attempt=%s commit=%s", id, attemptID, short(ic.CommitSHA)))
					} else {
						emit(fmt.Sprintf("integrate.ok:%s attempt=%s commit=%s files=%d", id, attemptID, short(ic.CommitSHA), len(ic.Files)))
					}
				} else {
					// No integrate required: finalize succeeded after accept only.
					supersedes := ""
					if isAlternate {
						supersedes = failedAttemptIDForAlt
					}
					if ferr := finalizeSucceeded(activeClaim, attemptID, eventGen, &outcome, closeEvidence, supersedes); ferr != nil {
						outcome.Terminal = ""
						outcome.FailureClass = firstNonEmpty(outcome.FailureClass, "terminal_event_failed")
						outcome.Message = ferr.Error()
						out.Children = append(out.Children, outcome)
						return failBlockedJoin(out, s.HomeDir, projectID, runID, "success finalize: "+ferr.Error(), releaseWorktreePeak)
					}
				}
			}

			// Logical terminal evidence only after final attempt for this child.
			ev[id] = closeTerm
			if outcome.Terminal == "" {
				outcome.Terminal = string(closeTerm)
			}
			out.Children = append(out.Children, outcome)
			emit(fmt.Sprintf("child.terminal:%s term=%s evidence=%s", id, outcome.Terminal, short(closeEvidence)))
			// Durable partial + worktree leave: both required; join failures with terminal cause.
			// Required child failure blocks the parent (do not pretend human_gate success).
			if it.Status == workgraph.ItemRequired && closeTerm != workgraph.TermSucceeded {
				msg := fmt.Sprintf("required child %s terminal=%s", id, closeTerm)
				if outcome.Message != "" {
					msg += ": " + outcome.Message
				}
				return failBlockedJoin(out, s.HomeDir, projectID, runID, msg, releaseWorktreePeak)
			}
			// Success/continue path: still persist partial and release occupancy.
			if perr := writePartialPrior(s.HomeDir, projectID, runID, out); perr != nil {
				if cerr := releaseWorktreePeak(); cerr != nil {
					return fail(out, StatusBlocked, errors.Join(
						fmt.Errorf("partial_checkpoint: %w", perr),
						fmt.Errorf("cleanup: %w", cerr),
					).Error())
				}
				return fail(out, StatusBlocked, "partial_checkpoint: "+perr.Error())
			}
			if cerr := releaseWorktreePeak(); cerr != nil {
				return fail(out, StatusBlocked, "worktree cleanup: "+cerr.Error())
			}
			out.WorktreeActive = peaks.activeWorktree
			out.ProcessActive = peaks.activeProcess
			// Required child failure already returned above.
			if cerr != nil && it.Status == workgraph.ItemRequired {
				return fail(out, StatusBlocked, "required child "+id+": "+cerr.Error())
			}
		}
	}

	// Integrated is product-branch integrate identity only (IntegrateChild + integrate
	// event + commit SHA). When doIntegrate is false (SkipIntegrate or no git repo),
	// succeeded children remain terminal/completed and MUST NOT enter Integrated or
	// emit integrate-equivalent events. Never fabricate a legacy integrated list.
	out.Integrated = integrated

	// Claim budget per logical child: exactly one normal claim, or two when a
	// generation-safe model_unavailable alternate claimed a distinct AttemptID.
	// Never more than two (no retry storms); never zero for launched members.
	for id, n := range claimed {
		if n < 1 || n > 2 {
			return fail(out, StatusBlocked, fmt.Sprintf("item %s claimed %d times", id, n))
		}
	}
	// Launches count actual exec only; claims may exceed launches when an alternate
	// claim is closed without launch (capacity_refused / event failure after claim).
	if out.LaunchCount > out.ClaimCount {
		return fail(out, StatusBlocked, "launch/claim mismatch")
	}

	// parent cannot succeed before required children terminal
	for _, it := range g.Items {
		if it.Status == workgraph.ItemRequired {
			if term, ok := ev[it.ID]; !ok || term != workgraph.TermSucceeded {
				return fail(out, StatusBlocked, "required child not terminal: "+it.ID)
			}
		}
	}

	emit("human_gate.await_owner")
	out.Status = StatusHumanGate
	out.Message = fmt.Sprintf("bounded workflow graph=%s claims=%d launches=%d integrated=%d; auto_merge=false",
		out.GraphID, out.ClaimCount, out.LaunchCount, len(out.Integrated))
	out.AutoMerge = false
	out.WorktreePeak = peaks.WorktreePeak
	out.ProcessPeak = peaks.ProcessPeak
	out.WorktreeActive = peaks.activeWorktree
	out.ProcessActive = peaks.activeProcess
	return out, nil
}

func resolveChildRoute(routes map[string]ChildRoute, id, defProv, defModel string) ChildRoute {
	// Provider/model pin only. TaskClass/Depth/Permission are applied from the
	// prevalidated childContract after resolve (no partial Definition merge here).
	if len(routes) > 0 {
		if r, ok := routes[id]; ok {
			if strings.TrimSpace(r.Provider) == "" {
				r.Provider = defProv
			}
			if strings.TrimSpace(r.Model) == "" {
				r.Model = defModel
			}
			return r
		}
	}
	return ChildRoute{
		Provider: defProv, Model: defModel, RouteReason: "default_pin",
	}
}

func eventJSONPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// crossCheckSpawnPID ensures the executor-returned spawn identity matches the
// spawn-time pid event already persisted. Empty returned fields are allowed
// (executor may only stamp partial identity on error paths); a positive mismatch fails closed.
func crossCheckSpawnPID(logged ProcessStart, out ChildExecResult) error {
	if out.ProcessPID > 0 && out.ProcessPID != logged.PID {
		return fmt.Errorf("returned pid %d != spawn pid %d", out.ProcessPID, logged.PID)
	}
	if out.ProcessPGID > 0 && out.ProcessPGID != logged.PGID {
		return fmt.Errorf("returned pgid %d != spawn pgid %d", out.ProcessPGID, logged.PGID)
	}
	if b := strings.TrimSpace(out.ProcessBirthIdentity); b != "" && b != strings.TrimSpace(logged.ProcessBirthIdentity) {
		return fmt.Errorf("returned process_birth_identity mismatch")
	}
	if e := strings.TrimSpace(out.ExecutableIdentity); e != "" && e != strings.TrimSpace(logged.ExecutableIdentity) {
		return fmt.Errorf("returned executable_identity mismatch")
	}
	return nil
}

// interruptPIDFromSpawn returns the durable interrupt PID. When spawn was
// logged, use spawnStart.PID (exact identity); a positive returned mismatch fails closed.
// When spawn was not logged (Fake post-return path), fall back to result ProcessPID.
func interruptPIDFromSpawn(spawnLogged bool, spawn ProcessStart, out ChildExecResult) (int, error) {
	if spawnLogged {
		if spawn.PID <= 0 {
			return 0, fmt.Errorf("spawn-logged interrupt missing durable pid")
		}
		if out.ProcessPID > 0 && out.ProcessPID != spawn.PID {
			return 0, fmt.Errorf("interrupt pid mismatch: result %d != spawn %d", out.ProcessPID, spawn.PID)
		}
		return spawn.PID, nil
	}
	return out.ProcessPID, nil
}

// pickSameDepthAlternate selects a hard-eligible, non-soft-excluded candidate
// matching required depth and permission. Never rewrites candidate depth.
// Preserves AccountRef/WindowKind identity from the candidate row.
func pickSameDepthAlternate(
	cands []AlternateCandidate,
	failedProvider, failedModel, reqDepth, reqPerm string,
) AlternateCandidate {
	reqDepth = strings.ToLower(strings.TrimSpace(reqDepth))
	reqPerm = strings.ToLower(strings.TrimSpace(reqPerm))
	failedProvider = strings.TrimSpace(failedProvider)
	failedModel = strings.TrimSpace(failedModel)
	if reqDepth == "" {
		return AlternateCandidate{}
	}
	for _, cv := range cands {
		if !cv.HardEligible || cv.SoftExcluded {
			continue
		}
		if strings.TrimSpace(cv.Provider) == "" || strings.TrimSpace(cv.Model) == "" {
			continue
		}
		if strings.EqualFold(cv.Provider, failedProvider) && strings.EqualFold(cv.Model, failedModel) {
			continue
		}
		if strings.ToLower(strings.TrimSpace(cv.Effort)) != reqDepth {
			continue
		}
		if reqPerm != "" {
			p := strings.ToLower(strings.TrimSpace(cv.Permission))
			// Empty observed permission never matches a required permission.
			if p == "" || p != reqPerm {
				continue
			}
		}
		return AlternateCandidate{
			Provider: cv.Provider, Model: cv.Model, Effort: strings.ToLower(strings.TrimSpace(cv.Effort)),
			Permission: strings.ToLower(strings.TrimSpace(cv.Permission)),
			AccountRef: strings.TrimSpace(cv.AccountRef), InstallRef: strings.TrimSpace(cv.InstallRef),
			WindowKind:   strings.TrimSpace(cv.WindowKind),
			HardEligible: true,
		}
	}
	return AlternateCandidate{}
}

// validateCapacityRerouteResult enforces the strict exact contract before alternate exec.
// Every prior and alternate identity dimension is required nonempty and exact-bound
// to the input failed route and selected alternate route — no optional dimensions.
func validateCapacityRerouteResult(cr CapacityRerouteResult, in CapacityRerouteInput) error {
	prior := cr.PriorTransition
	alt := cr.AlternateTransition
	require := func(role, field, v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("capacity: %s %s required nonempty", role, field)
		}
		return nil
	}
	eq := func(role, field, got, want string) error {
		if strings.TrimSpace(got) != strings.TrimSpace(want) {
			return fmt.Errorf("capacity: %s %s %q != %q", role, field, got, want)
		}
		return nil
	}
	// Input identity dimensions must all be nonempty (caller contract).
	for _, chk := range []struct{ f, v string }{
		{"FailedProvider", in.FailedProvider}, {"FailedModel", in.FailedModel},
		{"FailedDepth", in.FailedDepth}, {"FailedPermission", in.FailedPermission},
		{"FailedAccountRef", in.FailedAccountRef}, {"FailedWindowKind", in.FailedWindowKind},
		{"FailedReservationID", in.FailedReservationID},
		{"AltProvider", in.AltProvider}, {"AltModel", in.AltModel},
		{"AltDepth", in.AltDepth}, {"AltPermission", in.AltPermission},
		{"AltAccountRef", in.AltAccountRef}, {"AltWindowKind", in.AltWindowKind},
		{"Depth", in.Depth}, {"Permission", in.Permission},
		{"FailedAttemptID", in.FailedAttemptID}, {"NewAttemptID", in.NewAttemptID},
	} {
		if err := require("input", chk.f, chk.v); err != nil {
			return err
		}
	}
	if strings.TrimSpace(in.AltDepth) != strings.TrimSpace(in.Depth) {
		return fmt.Errorf("capacity: AltDepth %q != Depth %q", in.AltDepth, in.Depth)
	}
	if strings.TrimSpace(in.AltPermission) != strings.TrimSpace(in.Permission) {
		return fmt.Errorf("capacity: AltPermission %q != Permission %q", in.AltPermission, in.Permission)
	}

	if strings.TrimSpace(prior.AttemptID) == "" || strings.TrimSpace(prior.Role) != "prior" {
		return fmt.Errorf("capacity: prior transition missing or role!=prior")
	}
	if err := eq("prior", "AttemptID", prior.AttemptID, in.FailedAttemptID); err != nil {
		return err
	}
	pst := strings.ToLower(strings.TrimSpace(prior.State))
	if pst != "released" && pst != "reconciled" {
		return fmt.Errorf("capacity: prior state %q want released|reconciled", prior.State)
	}
	for _, chk := range []struct{ f, v string }{
		{"provider", prior.Provider}, {"model", prior.Model}, {"depth", prior.Depth},
		{"permission", prior.Permission},
		{"account", prior.AccountRef}, {"window", prior.WindowKind}, {"reservation", prior.ReservationID},
	} {
		if err := require("prior", chk.f, chk.v); err != nil {
			return err
		}
	}
	// Prior binds failed route exactly (no optional).
	if err := eq("prior", "provider", prior.Provider, in.FailedProvider); err != nil {
		return err
	}
	if err := eq("prior", "model", prior.Model, in.FailedModel); err != nil {
		return err
	}
	if err := eq("prior", "depth", prior.Depth, in.FailedDepth); err != nil {
		return err
	}
	if err := eq("prior", "permission", prior.Permission, in.FailedPermission); err != nil {
		return err
	}
	if err := eq("prior", "account", prior.AccountRef, in.FailedAccountRef); err != nil {
		return err
	}
	if err := eq("prior", "window", prior.WindowKind, in.FailedWindowKind); err != nil {
		return err
	}
	if err := eq("prior", "reservation", prior.ReservationID, in.FailedReservationID); err != nil {
		return err
	}
	// Result-level state must match transitions exactly (no untrusted duplicate).
	if strings.TrimSpace(cr.PriorState) != "" {
		if err := eq("result", "PriorState", cr.PriorState, prior.State); err != nil {
			return err
		}
	}

	if strings.TrimSpace(alt.AttemptID) == "" || strings.TrimSpace(alt.Role) != "alternate" {
		return fmt.Errorf("capacity: alternate transition missing or role!=alternate")
	}
	if err := eq("alternate", "AttemptID", alt.AttemptID, in.NewAttemptID); err != nil {
		return err
	}
	if strings.ToLower(strings.TrimSpace(alt.State)) != "reserved" {
		return fmt.Errorf("capacity: alternate state %q want reserved", alt.State)
	}
	for _, chk := range []struct{ f, v string }{
		{"provider", alt.Provider}, {"model", alt.Model}, {"depth", alt.Depth},
		{"permission", alt.Permission},
		{"account", alt.AccountRef}, {"window", alt.WindowKind}, {"reservation", alt.ReservationID},
	} {
		if err := require("alternate", chk.f, chk.v); err != nil {
			return err
		}
	}
	if strings.TrimSpace(prior.ReservationID) == strings.TrimSpace(alt.ReservationID) {
		return fmt.Errorf("capacity: prior and alternate ReservationID must be distinct")
	}
	// Result-level bind exact to alternate transition (always required).
	if err := eq("result", "AccountRef", cr.AccountRef, alt.AccountRef); err != nil {
		return err
	}
	if err := eq("result", "WindowKind", cr.WindowKind, alt.WindowKind); err != nil {
		return err
	}
	if err := eq("result", "ReservationID", cr.ReservationID, alt.ReservationID); err != nil {
		return err
	}
	if strings.TrimSpace(cr.AlternateState) != "" {
		if err := eq("result", "AlternateState", cr.AlternateState, alt.State); err != nil {
			return err
		}
	}
	// Alternate binds selected route exactly (always).
	if err := eq("alternate", "provider", alt.Provider, in.AltProvider); err != nil {
		return err
	}
	if err := eq("alternate", "model", alt.Model, in.AltModel); err != nil {
		return err
	}
	if err := eq("alternate", "depth", alt.Depth, in.AltDepth); err != nil {
		return err
	}
	if err := eq("alternate", "permission", alt.Permission, in.AltPermission); err != nil {
		return err
	}
	if err := eq("alternate", "account", alt.AccountRef, in.AltAccountRef); err != nil {
		return err
	}
	if err := eq("alternate", "window", alt.WindowKind, in.AltWindowKind); err != nil {
		return err
	}
	return nil
}

// exactRouteMatch requires every identity dimension equal between request and
// actual InvokedRoute. Capacity-backed routes (any of account/window/reservation/
// install) require ALL fields nonempty on both sides — empty==empty is not a match.
// InstallRef requires exact canonical equality (no substring).
func exactRouteMatch(want, got ChildRoute) error {
	// Provider/model always required nonempty and equal.
	for _, f := range []struct{ n, a, b string }{
		{"provider", want.Provider, got.Provider},
		{"model", want.Model, got.Model},
	} {
		if strings.TrimSpace(f.a) == "" {
			return fmt.Errorf("route %s required nonempty", f.n)
		}
		if strings.TrimSpace(f.b) == "" {
			return fmt.Errorf("invoked %s required nonempty", f.n)
		}
		if strings.TrimSpace(f.a) != strings.TrimSpace(f.b) {
			return fmt.Errorf("invoked %s %q != route %q", f.n, f.b, f.a)
		}
	}
	// Depth/permission: when requested nonempty, actual must match exactly.
	// Empty request depth is allowed only for non-capacity fixture tests.
	for _, f := range []struct{ n, a, b string }{
		{"depth", want.Depth, got.Depth},
		{"permission", want.Permission, got.Permission},
	} {
		if strings.TrimSpace(f.a) == "" {
			continue
		}
		if strings.TrimSpace(f.b) == "" {
			return fmt.Errorf("invoked %s required nonempty", f.n)
		}
		if strings.TrimSpace(f.a) != strings.TrimSpace(f.b) {
			return fmt.Errorf("invoked %s %q != route %q", f.n, f.b, f.a)
		}
	}
	capacityBound := strings.TrimSpace(want.AccountRef) != "" ||
		strings.TrimSpace(want.WindowKind) != "" ||
		strings.TrimSpace(want.ReservationID) != "" ||
		strings.TrimSpace(want.InstallRef) != ""
	if capacityBound {
		// Exact depth required for capacity-backed routes.
		if strings.TrimSpace(want.Depth) == "" {
			return fmt.Errorf("route depth required nonempty for capacity route")
		}
		if strings.TrimSpace(got.Depth) == "" {
			return fmt.Errorf("invoked depth required nonempty for capacity route")
		}
		if strings.TrimSpace(want.Depth) != strings.TrimSpace(got.Depth) {
			return fmt.Errorf("invoked depth %q != route %q", got.Depth, want.Depth)
		}
	}
	capFields := []struct{ n, a, b string }{
		{"account_ref", want.AccountRef, got.AccountRef},
		{"install_ref", want.InstallRef, got.InstallRef},
		{"window_kind", want.WindowKind, got.WindowKind},
		{"reservation_id", want.ReservationID, got.ReservationID},
	}
	for _, f := range capFields {
		if capacityBound {
			if strings.TrimSpace(f.a) == "" {
				return fmt.Errorf("route %s required nonempty for capacity route", f.n)
			}
			if strings.TrimSpace(f.b) == "" {
				return fmt.Errorf("invoked %s required nonempty for capacity route", f.n)
			}
		}
		// Exact equality only — never substring install matching.
		if strings.TrimSpace(f.a) != strings.TrimSpace(f.b) {
			return fmt.Errorf("invoked %s %q != route %q", f.n, f.b, f.a)
		}
	}
	return nil
}

// reconstructOutcomeFromDurable builds ChildOutcome from a closed claim plus
// exact matching terminal and launch events. Used for process restart without
// caller-supplied PriorSucceeded.
func reconstructOutcomeFromDurable(c *workclaim.Claim, elog *EventLog, projectID, runID, workItemID string) (ChildOutcome, error) {
	if c == nil {
		return ChildOutcome{}, fmt.Errorf("nil claim")
	}
	if c.State != workclaim.StateClosed {
		return ChildOutcome{}, fmt.Errorf("claim not closed")
	}
	if !validTerminalEnum(string(c.Terminal)) {
		return ChildOutcome{}, fmt.Errorf("claim terminal enum invalid %q", c.Terminal)
	}
	events, err := elog.ReadAllForRun(projectID, runID)
	if err != nil {
		return ChildOutcome{}, err
	}
	att := strings.TrimSpace(c.AttemptID)
	var termEv *Event
	var launchEv *Event
	for i := range events {
		ev := &events[i]
		if strings.TrimSpace(ev.AttemptID) != att {
			continue
		}
		if strings.TrimSpace(ev.WorkItemID) != "" && strings.TrimSpace(ev.WorkItemID) != workItemID {
			return ChildOutcome{}, fmt.Errorf("event work_item_id mismatch for attempt %q", att)
		}
		switch strings.ToLower(strings.TrimSpace(ev.Kind)) {
		case "terminal":
			if !validTerminalEnum(ev.Terminal) {
				return ChildOutcome{}, fmt.Errorf("invalid terminal enum %q", ev.Terminal)
			}
			if termEv != nil {
				if !terminalEventsExactEqual(*termEv, *ev) {
					return ChildOutcome{}, fmt.Errorf("duplicate divergent terminal for attempt %q", att)
				}
				continue
			}
			// Exact positive generation equal to claim — generation 0 is never a wildcard.
			if ev.Generation <= 0 {
				return ChildOutcome{}, fmt.Errorf("terminal generation must be positive got %d", ev.Generation)
			}
			if int64(ev.Generation) != c.Generation {
				return ChildOutcome{}, fmt.Errorf("terminal generation %d != claim %d", ev.Generation, c.Generation)
			}
			if strings.TrimSpace(ev.ProjectID) != "" && strings.TrimSpace(ev.ProjectID) != projectID {
				return ChildOutcome{}, fmt.Errorf("terminal project_id mismatch")
			}
			if strings.TrimSpace(ev.RunID) != "" && strings.TrimSpace(ev.RunID) != runID {
				return ChildOutcome{}, fmt.Errorf("terminal run_id mismatch")
			}
			termEv = ev
		case "launch":
			if launchEv != nil {
				// No duplicate launch, even byte-different that share a subset.
				if !launchEventsExactEqual(*launchEv, *ev) {
					return ChildOutcome{}, fmt.Errorf("duplicate divergent launch for attempt %q", att)
				}
				return ChildOutcome{}, fmt.Errorf("duplicate launch for attempt %q", att)
			}
			if ev.Generation <= 0 {
				return ChildOutcome{}, fmt.Errorf("launch generation must be positive got %d", ev.Generation)
			}
			if int64(ev.Generation) != c.Generation {
				return ChildOutcome{}, fmt.Errorf("launch generation %d != claim %d", ev.Generation, c.Generation)
			}
			if strings.TrimSpace(ev.ProjectID) != "" && strings.TrimSpace(ev.ProjectID) != projectID {
				return ChildOutcome{}, fmt.Errorf("launch project_id mismatch")
			}
			if strings.TrimSpace(ev.RunID) != "" && strings.TrimSpace(ev.RunID) != runID {
				return ChildOutcome{}, fmt.Errorf("launch run_id mismatch")
			}
			launchEv = ev
		}
	}
	if termEv == nil {
		return ChildOutcome{}, fmt.Errorf("no terminal event for closed claim attempt %q", att)
	}
	if strings.TrimSpace(termEv.Terminal) != string(c.Terminal) {
		return ChildOutcome{}, fmt.Errorf("claim terminal %q != event %q", c.Terminal, termEv.Terminal)
	}
	if c.Terminal == workgraph.TermSucceeded {
		if strings.TrimSpace(c.OutputEvidence) == "" || strings.TrimSpace(termEv.Evidence) == "" {
			return ChildOutcome{}, fmt.Errorf("succeeded reuse missing evidence")
		}
		if strings.TrimSpace(c.OutputEvidence) != strings.TrimSpace(termEv.Evidence) {
			return ChildOutcome{}, fmt.Errorf("claim evidence != terminal evidence")
		}
		if launchEv == nil {
			return ChildOutcome{}, fmt.Errorf("succeeded reuse requires exactly one launch event")
		}
	}
	out := ChildOutcome{
		WorkItemID: workItemID, AttemptID: att,
		// Assignment-time identity from claim (never re-parse InvokedRoute post-exec).
		TaskClass: c.TaskClass, ExecutionPlanDigest: c.PlanDigest,
		ChildContractDigest: c.ChildContractDigest, Generation: int(c.Generation),
		Terminal: string(c.Terminal), OutputEvidence: firstNonEmpty(c.OutputEvidence, termEv.Evidence),
	}
	if launchEv != nil {
		if len(launchEv.Payload) == 0 {
			return ChildOutcome{}, fmt.Errorf("launch event missing payload identity")
		}
		var m map[string]string
		if json.Unmarshal(launchEv.Payload, &m) != nil {
			return ChildOutcome{}, fmt.Errorf("launch payload malformed")
		}
		out.Provider = m["provider"]
		out.Model = m["model"]
		out.Depth = m["depth"]
		out.Permission = m["permission"]
		out.AccountRef = m["account_ref"]
		out.WindowKind = m["window_kind"]
		out.ReservationID = m["reservation_id"]
		// Prefer claim/event assignment digests; payload may reaffirm them.
		if tc := strings.TrimSpace(m["task_class"]); tc != "" && strings.TrimSpace(out.TaskClass) == "" {
			out.TaskClass = tc
		}
		if ccd := strings.TrimSpace(m["child_contract_digest"]); ccd != "" && strings.TrimSpace(out.ChildContractDigest) == "" {
			out.ChildContractDigest = ccd
		}
		if c.Terminal == workgraph.TermSucceeded {
			// Require full binding: provider/model/depth/permission/account/install/window/reservation/route_reason.
			for _, f := range []struct{ n, v string }{
				{"provider", out.Provider}, {"model", out.Model},
				{"depth", out.Depth}, {"permission", out.Permission},
				{"account_ref", out.AccountRef}, {"install_ref", m["install_ref"]},
				{"window_kind", out.WindowKind}, {"reservation_id", out.ReservationID},
				{"route_reason", m["route_reason"]},
			} {
				if strings.TrimSpace(f.v) == "" {
					return ChildOutcome{}, fmt.Errorf("succeeded reuse launch missing %s", f.n)
				}
			}
		}
	}
	return out, nil
}

func validTerminalEnum(s string) bool {
	switch strings.TrimSpace(s) {
	case string(workgraph.TermSucceeded), string(workgraph.TermFailed), string(workgraph.TermCancelled):
		return true
	}
	return false
}

func terminalEventsExactEqual(a, b Event) bool {
	// Exact identity: project/run/workitem/attempt/generation (no wildcards).
	if strings.TrimSpace(a.ProjectID) != strings.TrimSpace(b.ProjectID) {
		return false
	}
	if strings.TrimSpace(a.RunID) != strings.TrimSpace(b.RunID) {
		return false
	}
	if strings.TrimSpace(a.AttemptID) != strings.TrimSpace(b.AttemptID) {
		return false
	}
	if strings.TrimSpace(a.WorkItemID) != strings.TrimSpace(b.WorkItemID) {
		return false
	}
	if a.Generation != b.Generation {
		return false
	}
	if a.Generation <= 0 {
		return false
	}
	if strings.TrimSpace(a.Terminal) != strings.TrimSpace(b.Terminal) {
		return false
	}
	if strings.TrimSpace(a.Evidence) != strings.TrimSpace(b.Evidence) {
		return false
	}
	// Strict payload decode — malformed JSON is not equal (not ignored).
	var ma, mb map[string]string
	if len(a.Payload) > 0 {
		if json.Unmarshal(a.Payload, &ma) != nil {
			return false
		}
	}
	if len(b.Payload) > 0 {
		if json.Unmarshal(b.Payload, &mb) != nil {
			return false
		}
	}
	// Canonical full binding equality on all payload keys present in either.
	keys := map[string]bool{}
	for k := range ma {
		keys[k] = true
	}
	for k := range mb {
		keys[k] = true
	}
	for k := range keys {
		if ma[k] != mb[k] {
			return false
		}
	}
	return true
}

func launchEventsExactEqual(a, b Event) bool {
	if strings.TrimSpace(a.ProjectID) != strings.TrimSpace(b.ProjectID) {
		return false
	}
	if strings.TrimSpace(a.RunID) != strings.TrimSpace(b.RunID) {
		return false
	}
	if strings.TrimSpace(a.AttemptID) != strings.TrimSpace(b.AttemptID) {
		return false
	}
	if strings.TrimSpace(a.WorkItemID) != strings.TrimSpace(b.WorkItemID) {
		return false
	}
	if a.Generation != b.Generation || a.Generation <= 0 {
		return false
	}
	var ma, mb map[string]string
	if len(a.Payload) == 0 || json.Unmarshal(a.Payload, &ma) != nil {
		return false
	}
	if len(b.Payload) == 0 || json.Unmarshal(b.Payload, &mb) != nil {
		return false
	}
	// Full binding equality including message/work_item/route_reason.
	for _, k := range []string{
		"provider", "model", "depth", "permission", "account_ref", "install_ref",
		"window_kind", "reservation_id", "route_reason", "work_item_id", "message",
	} {
		if ma[k] != mb[k] {
			return false
		}
	}
	// Any extra keys must also match (byte-different duplicates fail).
	keys := map[string]bool{}
	for k := range ma {
		keys[k] = true
	}
	for k := range mb {
		keys[k] = true
	}
	for k := range keys {
		if ma[k] != mb[k] {
			return false
		}
	}
	return true
}

// reconcileClaimsWithEventLog couples durable claim store and event ledger.
// Crash window recovery: open claims with an exact durable terminal event are
// closed idempotently from that terminal (succeeded/failed/cancelled). Remaining
// impossible states (closed without terminal, divergent terminal) fail closed.
func reconcileClaimsWithEventLog(cs *workclaim.Store, elog *EventLog, projectID, runID string) error {
	if cs == nil || elog == nil {
		return fmt.Errorf("claim store and event log required for reconcile")
	}
	events, err := elog.ReadAllForRun(projectID, runID)
	if err != nil {
		return err
	}
	termByAttempt := map[string]Event{}
	for _, ev := range events {
		if !strings.EqualFold(strings.TrimSpace(ev.Kind), "terminal") {
			continue
		}
		att := strings.TrimSpace(ev.AttemptID)
		if att == "" {
			return fmt.Errorf("terminal event %q missing attempt_id", ev.EventID)
		}
		if !validTerminalEnum(ev.Terminal) {
			return fmt.Errorf("terminal event %q invalid terminal enum %q", ev.EventID, ev.Terminal)
		}
		if prev, ok := termByAttempt[att]; ok {
			if !terminalEventsExactEqual(prev, ev) {
				return fmt.Errorf("divergent terminal events for attempt %q (evidence/workitem/generation/payload)", att)
			}
			continue
		}
		termByAttempt[att] = ev
	}
	// Index claims by attempt.
	claimByAttempt := map[string]workclaim.Claim{}
	for _, c := range cs.AllClaims() {
		att := strings.TrimSpace(c.AttemptID)
		if att == "" {
			return fmt.Errorf("claim %q missing attempt_id", c.ClaimID)
		}
		if _, ok := claimByAttempt[att]; ok {
			return fmt.Errorf("duplicate claims for attempt %q", att)
		}
		claimByAttempt[att] = c
	}
	// Terminal without matching claim is corruption (claim persists before terminal).
	for att, tev := range termByAttempt {
		c, ok := claimByAttempt[att]
		if !ok {
			return fmt.Errorf("terminal event for attempt %q has no claim (corruption)", att)
		}
		if strings.TrimSpace(tev.WorkItemID) != "" && strings.TrimSpace(tev.WorkItemID) != strings.TrimSpace(c.WorkItemID) {
			return fmt.Errorf("terminal work_item_id %q != claim %q", tev.WorkItemID, c.WorkItemID)
		}
		if tev.Generation <= 0 {
			return fmt.Errorf("terminal generation must be positive for attempt %q got %d", att, tev.Generation)
		}
		if int64(tev.Generation) != c.Generation {
			return fmt.Errorf("terminal generation %d != claim generation %d for attempt %q",
				tev.Generation, c.Generation, att)
		}
	}
	// Auto-recover: open claim + exact terminal → Close from durable terminal.
	for att, c := range claimByAttempt {
		tev, hasTerm := termByAttempt[att]
		switch c.State {
		case workclaim.StateClaimed, workclaim.StateRunning:
			if !hasTerm {
				continue
			}
			term := workgraph.TerminalState(strings.TrimSpace(tev.Terminal))
			if term == workgraph.TermNone {
				return fmt.Errorf("terminal event for attempt %q has empty terminal", att)
			}
			evid := strings.TrimSpace(tev.Evidence)
			if evid == "" && len(tev.Payload) > 0 {
				var m map[string]string
				if json.Unmarshal(tev.Payload, &m) == nil {
					evid = strings.TrimSpace(m["output_evidence"])
				}
			}
			if term == workgraph.TermSucceeded && evid == "" {
				return fmt.Errorf("succeeded terminal for attempt %q missing output evidence", att)
			}
			if evid == "" {
				evid = "reconcile:" + string(term) + ":" + att
			}
			if _, cerr := cs.Close(workclaim.CloseRequest{
				ClaimID: c.ClaimID, Generation: c.Generation,
				ExecutorID: c.ExecutorID, AttemptID: c.AttemptID,
				Terminal: term, OutputEvidence: evid,
			}); cerr != nil {
				return fmt.Errorf("reconcile close claim %q attempt %q: %w", c.ClaimID, att, cerr)
			}
		case workclaim.StateClosed:
			if !hasTerm {
				return fmt.Errorf("closed claim %q attempt %q has no durable terminal event", c.ClaimID, att)
			}
			if strings.TrimSpace(tev.Terminal) != string(c.Terminal) {
				return fmt.Errorf("claim %q terminal %q != event terminal %q", c.ClaimID, c.Terminal, tev.Terminal)
			}
			if c.Terminal == workgraph.TermSucceeded && strings.TrimSpace(c.OutputEvidence) == "" {
				return fmt.Errorf("succeeded claim %q missing output evidence", c.ClaimID)
			}
			// Evidence exact bind when both present.
			if tev.Evidence != "" && c.OutputEvidence != "" &&
				strings.TrimSpace(tev.Evidence) != strings.TrimSpace(c.OutputEvidence) {
				return fmt.Errorf("claim %q evidence != terminal evidence", c.ClaimID)
			}
		}
	}
	return nil
}

// OneNodeDefinition builds a direct-run-equivalent single-item definition.
func OneNodeDefinition(graphID, intent string) workflowdef.Definition {
	if graphID == "" {
		graphID = "g-one"
	}
	if intent == "" {
		intent = "single direct-equivalent work item"
	}
	return workflowdef.Definition{
		SchemaVersion: 1, GraphID: graphID, Source: "explicit_definition",
		Items: []workflowdef.DefItem{
			{
				ID: "only", Intent: intent, Status: "required", IntegrationOrder: 1,
				RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
				OutputContract:   "branch+diff",
			},
		},
	}
}

// ChainDefinition builds a linear required chain a→b→c.
func ChainDefinition(graphID string) workflowdef.Definition {
	if graphID == "" {
		graphID = "g-chain"
	}
	return workflowdef.Definition{
		SchemaVersion: 1, GraphID: graphID, Source: "explicit_definition",
		Items: []workflowdef.DefItem{
			{
				ID: "a", Intent: "A", Status: "required", IntegrationOrder: 1,
				RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
				OutputContract:   "branch+diff",
			},
			{
				ID: "b", Intent: "B", Status: "required", IntegrationOrder: 2,
				RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
				OutputContract:   "branch+diff",
			},
			{
				ID: "c", Intent: "C", Status: "required", IntegrationOrder: 3,
				RouteRequirement: "class=soul,depth=high,permission=read-only",
				OutputContract:   "review_report",
			},
		},
		Deps: []workflowdef.DefDep{
			{From: "a", To: "b", Kind: "finish_to_start"},
			{From: "b", To: "c", Kind: "finish_to_start"},
		},
	}
}

// childContract is the prevalidated assignment-time execution contract for one item.
type childContract struct {
	TaskClass      string
	Depth          string
	Permission     string
	OutputContract string
	Digest         string
}

// buildChildContracts prevalidates each Definition item RouteRequirement and any
// resolved ChildRoute. Digests are built before materialize/claim — never after
// execution from InvokedRoute.
//
// Rules:
//   - When ChildRoutes is entirely absent (nil/empty), contract dimensions come
//     solely from the strict Definition RouteRequirement (default pin mode).
//   - When ChildRoutes is present, every Definition item must have a complete
//     entry: TaskClass, Depth, and Permission all explicit and exactly equal to
//     the parsed Definition contract. No partial ChildRoute merge/fill from
//     Definition.
func buildChildContracts(def workflowdef.Definition, planDigest string, routes map[string]ChildRoute) (map[string]childContract, error) {
	out := make(map[string]childContract, len(def.Items))
	routesPresent := len(routes) > 0
	for _, it := range def.Items {
		id := strings.TrimSpace(it.ID)
		if id == "" {
			return nil, fmt.Errorf("definition item missing id")
		}
		pr, err := routecontract.ParseRouteRequirement(it.RouteRequirement)
		if err != nil {
			return nil, fmt.Errorf("item %s route_requirement: %w", id, err)
		}
		outc := strings.TrimSpace(it.OutputContract)
		if outc == "" {
			return nil, fmt.Errorf("item %s: output_contract required for child contract", id)
		}
		taskClass := string(pr.Class)
		depth := pr.Depth
		perm := pr.Permission
		if routesPresent {
			r, ok := routes[id]
			if !ok {
				return nil, fmt.Errorf("item %s: ChildRoutes present but missing entry (no partial map)", id)
			}
			// All three assignment dimensions must be explicit on a present entry.
			if strings.TrimSpace(r.TaskClass) == "" {
				return nil, fmt.Errorf("item %s ChildRoute: task_class required (no Definition fill)", id)
			}
			if strings.TrimSpace(r.Depth) == "" {
				return nil, fmt.Errorf("item %s ChildRoute: depth required (no Definition fill)", id)
			}
			if strings.TrimSpace(r.Permission) == "" {
				return nil, fmt.Errorf("item %s ChildRoute: permission required (no Definition fill)", id)
			}
			if err := routecontract.ValidateRouteMatchesParsed(pr, r.TaskClass, r.Depth, r.Permission); err != nil {
				return nil, fmt.Errorf("item %s ChildRoute vs Definition: %w", id, err)
			}
			// Use the explicit ChildRoute values (already proven equal to Definition).
			taskClass = strings.ToLower(strings.TrimSpace(r.TaskClass))
			depth = strings.ToLower(strings.TrimSpace(r.Depth))
			perm = strings.TrimSpace(r.Permission)
		}
		dig, err := routecontract.ChildContractDigest(routecontract.ChildAssignment{
			ExecutionPlanDigest: planDigest,
			WorkItemID:          id,
			TaskClass:           taskClass,
			Depth:               depth,
			Permission:          perm,
			OutputContract:      outc,
		})
		if err != nil {
			return nil, fmt.Errorf("item %s contract digest: %w", id, err)
		}
		out[id] = childContract{
			TaskClass: taskClass, Depth: depth, Permission: perm,
			OutputContract: outc, Digest: dig,
		}
	}
	return out, nil
}

// ResultJSON encodes result for CLI.
func ResultJSON(r Result) []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return append(b, '\n')
}

func allTerminal(g workgraph.Graph, ev workgraph.TerminalEvidence) bool {
	for _, it := range g.Items {
		if _, ok := ev[it.ID]; !ok {
			return false
		}
	}
	return true
}

// failBlockedJoin writes partial checkpoint, runs cleanup, and returns blocked
// with ALL of primary + partial + cleanup errors joined (never replace original cause).
func failBlockedJoin(out Result, homeDir, projectID, runID, primary string, cleanup func() error) (Result, error) {
	var errs []error
	if strings.TrimSpace(primary) != "" {
		errs = append(errs, fmt.Errorf("%s", primary))
	}
	if perr := writePartialPrior(homeDir, projectID, runID, out); perr != nil {
		errs = append(errs, fmt.Errorf("partial_checkpoint: %w", perr))
	}
	if cleanup != nil {
		if cerr := cleanup(); cerr != nil {
			errs = append(errs, fmt.Errorf("cleanup: %w", cerr))
		}
	}
	if len(errs) == 0 {
		return fail(out, StatusBlocked, primary)
	}
	return fail(out, StatusBlocked, errors.Join(errs...).Error())
}

func fail(out Result, status, msg string) (Result, error) {
	out.Status = status
	out.Message = msg
	out.Error = msg
	return out, fmt.Errorf("workflowrun: %s", msg)
}

func isGitRepo(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && (st.IsDir() || st.Mode().IsRegular())
}

func firstNonEmptySlice(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

func sanitizeBranch(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "run"
	}
	if len(out) > 80 {
		return out[:80]
	}
	return out
}

// AttemptID builds the durable workflow attempt identifier used for claim,
// launch, capacity reservation, and event binding:
//
//	att-<workItemID>-<short(executionPlanDigest|"|"runID)>-g<generation>
//
// planDigest MUST be the canonical ExecutionPlanDigest (workflowdef.Normalize),
// never workgraph.DigestGraph. goalrun must Reserve under this exact ID.
// generation here is the 0-indexed attempt suffix; claim Generation is 1-indexed
// (claim gen G ↔ AttemptID(..., G-1)).
func AttemptID(workItemID, planDigest, runID string, generation int) string {
	workItemID = strings.TrimSpace(workItemID)
	if workItemID == "" {
		workItemID = "item"
	}
	if generation < 0 {
		generation = 0
	}
	return fmt.Sprintf("att-%s-%s-g%d", workItemID, short(planDigest+"|"+runID), generation)
}

// validateEntirePriorOutcomes validates every PriorOutcomes row in pure preflight
// as defense-in-depth: exact canonical terminal, class/CCD/depth/permission,
// plan, coherent provider/model/capacity when capacity-bearing. Conflicts fail.
func validateEntirePriorOutcomes(
	outcomes []ChildOutcome,
	itemByID map[string]workgraph.WorkItem,
	contracts map[string]childContract,
	planDigest, runID string,
) error {
	if len(outcomes) == 0 {
		return nil
	}
	seenAtt := map[string]bool{}
	for i, o := range outcomes {
		wi := o.WorkItemID
		att := o.AttemptID
		if wi == "" || att == "" {
			return fmt.Errorf("workflowrun: PriorOutcomes[%d] missing work_item/attempt (fail closed before side effects)", i)
		}
		if _, ok := itemByID[wi]; !ok {
			return fmt.Errorf("workflowrun: PriorOutcomes ghost work_item %q not in current graph (fail closed before side effects)", wi)
		}
		if seenAtt[att] {
			return fmt.Errorf("workflowrun: PriorOutcomes duplicate AttemptID %q (fail closed before side effects)", att)
		}
		seenAtt[att] = true
		switch o.Terminal {
		case string(workgraph.TermSucceeded), string(workgraph.TermFailed),
			string(workgraph.TermCancelled), string(workgraph.TermSkipped):
		default:
			return fmt.Errorf("workflowrun: PriorOutcomes attempt %s invalid terminal %q (exact succeeded|failed|cancelled|skipped)", att, o.Terminal)
		}
		if o.Generation < 1 {
			return fmt.Errorf("workflowrun: PriorOutcomes attempt %s generation %d < 1", att, o.Generation)
		}
		wantAtt := AttemptID(wi, planDigest, runID, o.Generation-1)
		if att != wantAtt {
			return fmt.Errorf("workflowrun: PriorOutcomes attempt %q != canonical %q", att, wantAtt)
		}
		if o.ExecutionPlanDigest != planDigest {
			return fmt.Errorf("workflowrun: PriorOutcomes attempt %s plan_digest mismatch", att)
		}
		cc, ok := contracts[wi]
		if !ok {
			return fmt.Errorf("workflowrun: PriorOutcomes %q missing prevalidated child contract", wi)
		}
		if o.TaskClass == "" || o.TaskClass != cc.TaskClass {
			return fmt.Errorf("workflowrun: PriorOutcomes attempt %s task_class %q != contract %q", att, o.TaskClass, cc.TaskClass)
		}
		if o.ChildContractDigest == "" || o.ChildContractDigest != cc.Digest {
			return fmt.Errorf("workflowrun: PriorOutcomes attempt %s child_contract_digest mismatch", att)
		}
		// Depth/permission must be nonempty (contract binding includes them via CCD).
		if o.Depth == "" {
			return fmt.Errorf("workflowrun: PriorOutcomes attempt %s depth empty", att)
		}
		if o.Permission == "" {
			return fmt.Errorf("workflowrun: PriorOutcomes attempt %s permission empty", att)
		}
		if o.Terminal == string(workgraph.TermSucceeded) {
			if strings.TrimSpace(o.OutputEvidence) == "" {
				return fmt.Errorf("workflowrun: PriorOutcomes succeeded attempt %s missing output_evidence", att)
			}
		}
		// Coherent capacity/route identity when any capacity field present.
		if o.AccountRef != "" || o.InstallRef != "" || o.WindowKind != "" || o.ReservationID != "" ||
			o.Provider != "" || o.Model != "" {
			if o.Provider == "" || o.Model == "" || o.Depth == "" || o.Permission == "" ||
				o.AccountRef == "" || o.InstallRef == "" || o.WindowKind == "" {
				return fmt.Errorf("workflowrun: PriorOutcomes attempt %s capacity/route-bearing incomplete", att)
			}
		}
	}
	return nil
}

// requirePriorSucceededSubsetOfOutcomes ensures every PriorSucceeded row is
// full-struct equal to a PriorOutcomes row when the full set is provided.
func requirePriorSucceededSubsetOfOutcomes(priors map[string]ChildOutcome, outcomes []ChildOutcome) error {
	if len(priors) == 0 {
		return nil
	}
	if len(outcomes) == 0 {
		// PriorSucceeded alone remains valid for legacy single-seed resumes
		// without a multi-attempt WorkflowKids set.
		return nil
	}
	byAtt := map[string]ChildOutcome{}
	for _, o := range outcomes {
		// Byte-exact AttemptID keys (no TrimSpace normalize of durable identity).
		byAtt[o.AttemptID] = o
	}
	for id, p := range priors {
		att := p.AttemptID
		o, ok := byAtt[att]
		if !ok {
			return fmt.Errorf("workflowrun: PriorSucceeded[%s] attempt %s not in PriorOutcomes (fail closed before side effects)", id, att)
		}
		if !reflect.DeepEqual(p, o) {
			return fmt.Errorf("workflowrun: PriorSucceeded[%s] full-row mismatch vs PriorOutcomes attempt %s (fail closed before side effects)", id, att)
		}
		if o.Terminal != string(workgraph.TermSucceeded) {
			return fmt.Errorf("workflowrun: PriorSucceeded[%s] maps to non-succeeded PriorOutcomes row (want exact succeeded, got %q)", id, o.Terminal)
		}
	}
	return nil
}

// validateEntirePriorSucceededMap validates every PriorSucceeded entry during
// pure preflight. Every key must be a current graph item; key must equal
// ChildOutcome.WorkItemID; no ghost entries; full identity must match local
// plan/class/CCD and canonical AttemptID. Present-invalid fails closed.
func validateEntirePriorSucceededMap(
	priors map[string]ChildOutcome,
	itemByID map[string]workgraph.WorkItem,
	contracts map[string]childContract,
	planDigest, runID string,
) error {
	if len(priors) == 0 {
		return nil
	}
	for id, prior := range priors {
		if _, ok := itemByID[id]; !ok {
			return fmt.Errorf("workflowrun: PriorSucceeded ghost key %q not in current graph (fail closed before side effects)", id)
		}
		if prior.WorkItemID != id {
			return fmt.Errorf("workflowrun: PriorSucceeded key %q != WorkItemID %q (fail closed)", id, prior.WorkItemID)
		}
		cc, ok := contracts[id]
		if !ok {
			return fmt.Errorf("workflowrun: PriorSucceeded %q missing prevalidated child contract", id)
		}
		if err := validatePriorSucceededForReuse(prior, id, planDigest, runID, cc); err != nil {
			return err
		}
	}
	return nil
}

// validateEntireAttemptGenerationMap validates every AttemptGeneration entry
// during pure preflight: key is a current graph item and value >= 0.
func validateEntireAttemptGenerationMap(gens map[string]int, itemByID map[string]workgraph.WorkItem) error {
	if len(gens) == 0 {
		return nil
	}
	for id, g := range gens {
		if _, ok := itemByID[id]; !ok {
			return fmt.Errorf("workflowrun: AttemptGeneration ghost key %q not in current graph (fail closed before side effects)", id)
		}
		if g < 0 {
			return fmt.Errorf("workflowrun: AttemptGeneration[%s]=%d is negative (fail closed before side effects)", id, g)
		}
	}
	return nil
}

// validatePriorSucceededForReuse independently validates a PriorSucceeded seed
// against the locally normalized ExecutionPlanDigest and prevalidated child
// contract before zero-spend reuse. Never mutates prior. Fail closed on any
// missing/mismatched identity — do not fall through to re-exec.
func validatePriorSucceededForReuse(prior ChildOutcome, workItemID, planDigest, runID string, cc childContract) error {
	if prior.WorkItemID != workItemID {
		return fmt.Errorf("workflowrun: prior %s work_item_id %q != graph item %q (refuse mutate)", workItemID, prior.WorkItemID, workItemID)
	}
	// Exact durable terminal identity — no EqualFold/TrimSpace normalize.
	if prior.Terminal != string(workgraph.TermSucceeded) {
		return fmt.Errorf("workflowrun: prior %s terminal %q != succeeded (fail closed; no re-exec)", workItemID, prior.Terminal)
	}
	if prior.AttemptID == "" || prior.AttemptID != strings.TrimSpace(prior.AttemptID) {
		return fmt.Errorf("workflowrun: prior %s missing or whitespace-padded attempt_id %q (fail closed)", workItemID, prior.AttemptID)
	}
	if prior.OutputEvidence == "" {
		return fmt.Errorf("workflowrun: prior %s missing output_evidence (fail closed)", workItemID)
	}
	if prior.Generation < 1 {
		return fmt.Errorf("workflowrun: prior %s generation %d < 1", workItemID, prior.Generation)
	}
	wantAtt := AttemptID(workItemID, planDigest, runID, prior.Generation-1)
	if prior.AttemptID != wantAtt {
		return fmt.Errorf("workflowrun: prior %s attempt_id %q != canonical %q (generation=%d)",
			workItemID, prior.AttemptID, wantAtt, prior.Generation)
	}
	if prior.ExecutionPlanDigest != planDigest {
		return fmt.Errorf("workflowrun: prior %s execution_plan_digest %q != local plan %q",
			workItemID, prior.ExecutionPlanDigest, planDigest)
	}
	if strings.TrimSpace(prior.TaskClass) == "" || prior.TaskClass != cc.TaskClass {
		return fmt.Errorf("workflowrun: prior %s task_class %q != contract %q",
			workItemID, prior.TaskClass, cc.TaskClass)
	}
	if strings.TrimSpace(prior.ChildContractDigest) == "" || prior.ChildContractDigest != cc.Digest {
		return fmt.Errorf("workflowrun: prior %s child_contract_digest %q != contract %q",
			workItemID, prior.ChildContractDigest, cc.Digest)
	}
	// CCD must be full lowercase canonical (no padding/case fold).
	if prior.ChildContractDigest != strings.ToLower(prior.ChildContractDigest) ||
		prior.ChildContractDigest != strings.TrimSpace(prior.ChildContractDigest) {
		return fmt.Errorf("workflowrun: prior %s child_contract_digest not lowercase canonical", workItemID)
	}
	return nil
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 12 {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// Err helpers for tests.
var (
	ErrInvalid = errors.New("workflowrun: invalid")
)
