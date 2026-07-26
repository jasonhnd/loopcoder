package goalrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/goalpr"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/routecontract"
	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// InventoryProvenance tracks how capacity inventory was obtained.
// Release canary emission requires LiveDiscover only.
type InventoryProvenance string

const (
	InventoryProvenanceUnspecified      InventoryProvenance = ""
	InventoryProvenanceLiveDiscover     InventoryProvenance = "live_discover"
	InventoryProvenanceCapacitySnapshot InventoryProvenance = "capacity_snapshot"
	InventoryProvenanceInjected         InventoryProvenance = "injected"
)

// Request is one product-path goal execution.
type Request struct {
	ProjectID string
	// RunID optional unique execution namespace. Empty → generated per Execute.
	// On forced restart, pass the same RunID so claim/attempt IDs stay stable.
	RunID string
	Goal  string
	Issue string
	Actor string
	Owner string
	// InventoryProvenance is structural (not string filtering of sources).
	// Canary emit fails closed unless LiveDiscover.
	InventoryProvenance InventoryProvenance
	// Provider/Model: empty or "auto" → per-child bare auto-route with durable
	// capacity rehydrate. Explicit production pin requires both; "fixture" always
	// fails closed (never a product-path bypass, even with injected Executor).
	Provider string
	Model    string
	// TaskClass is optional request-level documentation only. Production route
	// and pin bind use the exact class= token from each child RouteRequirement
	// (see TaskClassFromRoute). Never used as a silent Tera default.
	TaskClass capclass.Class
	// RepoPath for live inventory discover (optional).
	RepoPath string
	// DryRun when true only plans routes and releases capacity without executing
	// children (route preview). Default false → real child execution.
	DryRun *bool
	// Resume loads durable checkpoint for RunID and seeds PriorSucceeded so
	// already-terminal children are not re-claimed / re-executed / re-reserved.
	// When true, RunID is required (or checkpoint must resolve).
	Resume bool
	// PriorSucceeded optional explicit seed (tests). Prefer Resume+checkpoint.
	PriorSucceeded map[string]workflowrun.ChildOutcome
	// Executor injects the workflow child executor. nil → production real path.
	// Focused tests inject workflowrun.FakeChildExecutor. Executor injection never
	// authorizes fixture provider/model or inventory/reserve bypass.
	Executor workflowrun.ChildExecutor
	// Decompose injects a frozen workgraph for tests (malformed class seam).
	// nil → workgraph.DecomposeGoal production path.
	Decompose func(opts workgraph.DecomposeOptions) (workgraph.Graph, error)
	// LoadInventory injects inventory for tests.
	LoadInventory func(ctx context.Context, repo string, now time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error)
	// OpenLedger injects ledger for tests.
	OpenLedger func(now func() time.Time) (*capacityledger.Ledger, error)
	ReportOut  io.Writer
	Now        func() time.Time
	// HomeDir overrides child worktree layout (tests).
	HomeDir string
	// OpenPR when true opens a real branch/commit/push/PR on RepoPath after
	// human_gate (never auto-merges). Product path for real_pr_human_gate.
	// PR head is the shared goal branch that already holds integrated product
	// commits (not a receipt-only branch).
	OpenPR bool
	// PRBaseRef default main.
	PRBaseRef string
	// GoalBranch optional shared integrate branch (default loopcoder/goal-<runID>).
	GoalBranch string
	// IndependentVerifier provider/company for PR gate evidence.
	IndependentVerifier string
	// VerifierEvidence durable independent review ref (digest/path).
	VerifierEvidence string
	// RequiredCheckNames optional expected PR checks.
	RequiredCheckNames []string
	// GoalPR injects goalpr opener (tests). nil → goalpr.Open production.
	GoalPR func(ctx context.Context, req goalpr.Request) (goalpr.Result, error)
	// CanaryEmit when set writes loopcoder.canary_evidence.v1 via EmitCanaryEvidence
	// at goal end (exact-binary path; no hand flags).
	CanaryEmit *CanaryEmitOptions
	// WaitPRChecks when OpenPR waits for meaningful CI green then finalizes.
	WaitPRChecks bool
	// PRCheckWait max wait for checks (default 15m when WaitPRChecks).
	PRCheckWait time.Duration
	// CanaryUnavailableProbe* selects one adapter-declared but non-routable
	// model for a fixed read-only paid capability probe on wi_research. It is
	// valid only with CanaryEmit and auto-route. Success blocks; only a real
	// typed model_unavailable may reroute.
	CanaryUnavailableProbeProvider string
	CanaryUnavailableProbeModel    string
}

// ChildReport is one transparent child line for UI/JSONL.
type ChildReport struct {
	ChildID          string `json:"child_id"`
	Intent           string `json:"intent"`
	Owner            string `json:"owner"`
	RouteRequirement string `json:"route_requirement"`
	// TaskClass is the exact classified capability floor parsed from
	// RouteRequirement (class=luna|tera|soul). Never silently defaulted.
	TaskClass string `json:"task_class,omitempty"`
	// ExecutionPlanDigest is the canonical workflowdef.Normalize digest.
	ExecutionPlanDigest string `json:"execution_plan_digest,omitempty"`
	// ChildContractDigest binds plan + work_item + class/depth/permission + output_contract.
	ChildContractDigest string `json:"child_contract_digest,omitempty"`
	// Generation is positive claim generation when known (≥1).
	Generation       int      `json:"generation,omitempty"`
	Provider         string   `json:"provider,omitempty"`
	AccountRef       string   `json:"account_ref,omitempty"`
	InstallRef       string   `json:"install_ref,omitempty"`
	WindowKind       string   `json:"window_kind,omitempty"`
	Permission       string   `json:"permission,omitempty"`
	ReservationID    string   `json:"reservation_id,omitempty"`
	Model            string   `json:"model,omitempty"`
	Depth            string   `json:"depth,omitempty"`
	Stage            string   `json:"stage"`
	RouteReason      string   `json:"route_reason,omitempty"`
	CapacityBefore   *float64 `json:"capacity_before,omitempty"`
	CapacityReserved *float64 `json:"capacity_reserved,omitempty"`
	CapacityActual   *float64 `json:"capacity_actual,omitempty"`
	CapacityAfter    *float64 `json:"capacity_after,omitempty"`
	CapacityState    string   `json:"capacity_state,omitempty"`
	CapacityNote     string   `json:"capacity_note,omitempty"`
	// Structured before-window evidence at Reserve (never invented from note prose).
	CapacityBeforeSource          string    `json:"capacity_before_source,omitempty"`
	CapacityBeforeCapturedAt      time.Time `json:"capacity_before_captured_at,omitempty"`
	CapacityBeforeFreshness       string    `json:"capacity_before_freshness,omitempty"`
	CapacityBeforeConfidence      string    `json:"capacity_before_confidence,omitempty"`
	CapacityBeforeInventoryDigest string    `json:"capacity_before_inventory_digest,omitempty"`
	// CapacityResetAt is the exact reserved/observed window reset identity (UTC).
	// Required for finite/fixed windows; empty for unbounded/non-reset capacity.
	CapacityResetAt *time.Time `json:"capacity_reset_at,omitempty"`
	// Structured after evidence: observed (fresh same-window) vs derived (Before−Actual).
	// Derived never qualifies as fresh capacity-after.
	CapacityAfterSource          string    `json:"capacity_after_source,omitempty"`
	CapacityAfterObservedAt      time.Time `json:"capacity_after_observed_at,omitempty"`
	CapacityAfterFreshness       string    `json:"capacity_after_freshness,omitempty"`
	CapacityAfterConfidence      string    `json:"capacity_after_confidence,omitempty"`
	CapacityAfterState           string    `json:"capacity_after_state,omitempty"` // observed|derived
	CapacityAfterInventoryDigest string    `json:"capacity_after_inventory_digest,omitempty"`
	// CapacityActualConfidence is always estimated for group window aggregates
	// (concurrent external use cannot be excluded), even when before/after are exact.
	CapacityActualConfidence string `json:"capacity_actual_confidence,omitempty"`
	// CapacityGroupID / CapacityGroupObserveID prove one aggregate delta was split
	// across same-identity attempts (no per-child double-count of run-final After).
	CapacityGroupID        string `json:"capacity_group_id,omitempty"`
	CapacityGroupObserveID string `json:"capacity_group_observe_id,omitempty"`
	// Raw token usage (not quota-window fraction). Never reconciled as Actual.
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	TokenTotal   int64 `json:"token_total,omitempty"`
	// ActualSource is capacity fraction source (estimated_group_delta_* only for Actual).
	ActualSource string `json:"actual_source,omitempty"`
	// ActualSources is per-dimension route proof (never collapsed into ActualSource).
	ActualSources  workflowrun.ActualRouteSources `json:"actual_sources,omitempty"`
	ArgvDigest     string                         `json:"argv_digest,omitempty"`
	AttemptID      string                         `json:"attempt_id,omitempty"`
	OutputEvidence string                         `json:"output_evidence,omitempty"`
	FilesTouched   []string                       `json:"files_touched,omitempty"`
	// Production-owned Go tests validation binding (present only when applicable).
	TestValidationStatus        string `json:"test_validation_status,omitempty"`
	TestValidationEvidence      string `json:"test_validation_evidence,omitempty"`
	TestValidationCommandDigest string `json:"test_validation_command_digest,omitempty"`
	TestValidationHeadSHA       string `json:"test_validation_head_sha,omitempty"`
	TestValidationReceiptPath   string `json:"test_validation_receipt_path,omitempty"`
	WorktreePath                string `json:"worktree_path,omitempty"`
	Terminal                    string `json:"terminal,omitempty"`
	// FailureClass is the structured failure class (e.g. model_unavailable) when
	// Terminal is non-success. Retained on universal reports for MU failed routes.
	FailureClass string `json:"failure_class,omitempty"`
	// SupersedesAttemptID / RerouteEventRef bind generation-safe alternate winners.
	SupersedesAttemptID string `json:"supersedes_attempt_id,omitempty"`
	RerouteEventRef     string `json:"reroute_event_ref,omitempty"`
	// IntegrateCommitSHA is the goal-branch commit that absorbed this attempt (winner only).
	// Failed MU routes must leave this empty (no product integrate).
	IntegrateCommitSHA string `json:"integrate_commit_sha,omitempty"`
	// Authoritative lifecycle event IDs + PID from the event log (attempt-keyed).
	ClaimEventID     string `json:"claim_event_id,omitempty"`
	LaunchEventID    string `json:"launch_event_id,omitempty"`
	PIDEventID       string `json:"pid_event_id,omitempty"`
	TerminalEventID  string `json:"terminal_event_id,omitempty"`
	IntegrateEventID string `json:"integrate_event_id,omitempty"`
	ProviderPID      int    `json:"provider_pid,omitempty"`
	NextAction       string `json:"next_action,omitempty"`
	Unavailable      bool   `json:"unavailable,omitempty"`
}

// Result is parent evidence after bounded child execution.
type Result struct {
	Status  string `json:"status"`
	GraphID string `json:"graph_id"`
	// PlanDigest is the canonical ExecutionPlanDigest (workflowdef.Normalize).
	PlanDigest string `json:"plan_digest"`
	// GraphDigest is the separate workgraph.DigestGraph identity (not attempt key).
	GraphDigest  string             `json:"graph_digest,omitempty"`
	RunID        string             `json:"run_id,omitempty"`
	ProjectID    string             `json:"project_id,omitempty"`
	Children     []ChildReport      `json:"children"`
	Workflow     workflowrun.Result `json:"workflow"`
	Message      string             `json:"message"`
	HumanSummary string             `json:"human_summary"`
	// ProvidersUsed / ModelsUsed / DepthsUsed are actual successful accepted
	// provider execution only (never planned or fake). Empty on dry-run.
	ProvidersUsed       []string `json:"providers_used,omitempty"`
	ModelsUsed          []string `json:"models_used,omitempty"`
	DepthsUsed          []string `json:"depths_used,omitempty"`
	MultiProviderOK     bool     `json:"multi_provider_ok"`
	MultiModelOrDepthOK bool     `json:"multi_model_or_depth_ok"`
	// Planned* fields are structural/dry-run route diversity only — never imply
	// actual execution. Distinct JSON keys so qualification cannot confuse them.
	PlannedProvidersUsed       []string `json:"planned_providers_used,omitempty"`
	PlannedModelsUsed          []string `json:"planned_models_used,omitempty"`
	PlannedDepthsUsed          []string `json:"planned_depths_used,omitempty"`
	PlannedMultiProviderOK     bool     `json:"planned_multi_provider_ok"`
	PlannedMultiModelOrDepthOK bool     `json:"planned_multi_model_or_depth_ok"`
	// Restart / ceilings evidence (from workflow + durable checkpoint).
	ReuseCount     int    `json:"reuse_count,omitempty"`
	WorktreePeak   int    `json:"worktree_peak,omitempty"`
	ProcessPeak    int    `json:"process_peak,omitempty"`
	CheckpointPath string `json:"checkpoint_path,omitempty"`
	Resumed        bool   `json:"resumed,omitempty"`
	// PR is real GitHub PR human-gate evidence when OpenPR requested.
	PR *goalpr.Result `json:"pr,omitempty"`
	// RouteExcludes are measured hard/soft excludes from the live candidate set
	// (unavailable_retry evidence; Claimed=false for pure exclude).
	RouteExcludes []RouteExclude `json:"route_excludes,omitempty"`
	// InventoryReportDigest is the exact immutable inventory snapshot used for
	// route/reserve decisions in this run.
	InventoryReportDigest string `json:"inventory_report_digest,omitempty"`
	// ClaudeCatalogReceipts are the exact account-bound paid capability probes
	// covered by InventoryReportDigest. Static catalog rows never populate this.
	ClaudeCatalogReceipts []providerinventory.ClaudeCapabilityProbeReceipt `json:"claude_catalog_receipts,omitempty"`
	// CapacityLedgerEntries are exact read-backs for this run's attempts. Canary
	// emission serializes them as raw qualification proof; they contain no
	// credentials.
	CapacityLedgerEntries []capacityledger.Entry `json:"capacity_ledger_entries,omitempty"`
	// CanaryEvidencePath is set when CanaryEmit succeeds.
	CanaryEvidencePath string `json:"canary_evidence_path,omitempty"`
}

// capacityHold tracks a live reservation for post-execute reconcile/release.
type capacityHold struct {
	projectID, runID, attemptID string
}

// rejectForbiddenProductRoute fails closed on fixture (and fixture-model) pins
// before any decompose, approval, checkpoint recovery, inventory, ledger, claim,
// or durable product write. Injected Executor never authorizes fixture.
func rejectForbiddenProductRoute(provider, model string) error {
	p := strings.TrimSpace(provider)
	m := strings.TrimSpace(model)
	if strings.EqualFold(p, "fixture") || strings.EqualFold(m, "fixture-model") ||
		strings.EqualFold(m, "fixture") {
		return fmt.Errorf("goalrun: fixture provider/model is not a production route (fail closed before decompose/inventory/reserve/launch)")
	}
	return nil
}

// Execute decomposes the goal, routes each LoopCoder-owned child independently
// via bare auto-route + durable capacity rehydrate when not pinned, reserves
// capacity, runs real (or injected) child executors, reconciles actual usage
// when known (never fabricates), and emits transparent reports.
// Provider-native subagents are never used.
func Execute(ctx context.Context, req Request) (Result, error) {
	// Earliest request validation: forbidden product routes create zero durable state.
	if err := rejectForbiddenProductRoute(req.Provider, req.Model); err != nil {
		return Result{}, err
	}
	if err := validateCanaryUnavailableProbeRequest(req); err != nil {
		return Result{}, err
	}

	nowFn := req.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	// Production: honor LOOPCODER_HOME for event log / partial / child worktrees.
	if strings.TrimSpace(req.HomeDir) == "" {
		if h := strings.TrimSpace(os.Getenv("LOOPCODER_HOME")); h != "" {
			req.HomeDir = h
		}
	}
	now := nowFn().UTC()
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "owner"
	}
	decompOpts := workgraph.DecomposeOptions{
		Goal: req.Goal, Issue: req.Issue, Actor: actor, Owner: req.Owner, Now: now,
	}
	var g workgraph.Graph
	var err error
	if req.Decompose != nil {
		g, err = req.Decompose(decompOpts)
	} else {
		g, err = workgraph.DecomposeGoal(decompOpts)
	}
	if err != nil {
		return Result{}, err
	}

	// Architecture contract: parse every child route requirement immediately after
	// Decompose and BEFORE workflow approval/Materialize, checkpoint recovery,
	// inventory, ledger, resume, or execution. Invalid graph → blocked evidence
	// with zero durable execution state.
	routeByID := map[string]ParsedRouteRequirement{}
	var preChildren []ChildReport
	graphInvalid := false
	for _, it := range g.Items {
		pr, perr := ParseRouteRequirement(it.RouteRequirement)
		cr := ChildReport{
			ChildID: it.ID, Intent: it.Intent, Owner: it.Owner,
			RouteRequirement: it.RouteRequirement,
			Stage:            "routing", NextAction: "route_child",
		}
		if perr != nil {
			graphInvalid = true
			cr.Stage = "unavailable"
			cr.Unavailable = true
			cr.Terminal = "route_requirement_invalid"
			cr.CapacityNote = "no_reserve_invalid_route_requirement"
			cr.RouteReason = perr.Error()
			cr.NextAction = "fix_route_requirement"
			// Do not resume/reuse prior success under an invalid contract.
		} else {
			routeByID[it.ID] = pr
			cr.TaskClass = string(pr.Class)
			cr.Depth = pr.Depth
			cr.Permission = pr.Permission
		}
		preChildren = append(preChildren, cr)
	}
	if graphInvalid {
		// Mark any syntactically valid siblings blocked so the graph never
		// partially spends when the decomposed plan is contract-invalid.
		for i := range preChildren {
			if preChildren[i].Terminal == "" {
				preChildren[i].Stage = "unavailable"
				preChildren[i].Unavailable = true
				preChildren[i].Terminal = "graph_route_requirement_invalid"
				preChildren[i].CapacityNote = "no_reserve_sibling_invalid"
				preChildren[i].RouteReason = "graph has invalid route_requirement (fail closed)"
				preChildren[i].NextAction = "fix_route_requirement"
			}
			emitChild(req.ReportOut, preChildren[i])
		}
		// Pre-Normalize: PlanDigest/ExecutionPlanDigest stay empty. GraphDigest
		// alone carries workgraph.DigestGraph — never put the graph digest into
		// the canonical PlanDigest field on early errors.
		return Result{
			GraphID: g.GraphID, PlanDigest: "", GraphDigest: workgraph.DigestGraph(g),
			Children: preChildren,
			Status:   "blocked", Message: "route_requirement invalid: fail closed before materialize/inventory/ledger",
		}, fmt.Errorf("goalrun: route_requirement invalid (fail closed before materialize)")
	}

	def, err := workflowdef.FromGraph(g)
	if err != nil {
		return Result{}, err
	}
	plan, err := workflowdef.Normalize(def)
	if err != nil {
		return Result{}, err
	}
	// Canonical ExecutionPlanDigest (immutable approved workflowdef plan).
	// Distinct from workgraph.DigestGraph stored as GraphDigest.
	execPlanDigest := plan.Digest
	if execPlanDigest == "" {
		return Result{}, fmt.Errorf("goalrun: empty execution plan digest after normalize")
	}
	approval, err := workflowdef.Approve(plan.Digest, actor, "goalrun transparent children", now)
	if err != nil {
		return Result{}, err
	}
	reg := workflowdef.NewRegistry()
	if err := reg.RecordApproval(approval); err != nil {
		return Result{}, err
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" || projectID == "local-project" {
		// Never share the global local-project namespace across disposable canaries.
		projectID = UniqueProjectID(req.RepoPath, nowFn)
	}
	// Canonical GraphDigest for an executable run is the approved materialized
	// graph digest (not pre-materialize decomposition). Capture first Materialize
	// and use that value for routing-era errors, ledger, checkpoint, report, and
	// workflow ExpectedGraphDigest.
	mat, merr := reg.Materialize(projectID, def, approval, now)
	if merr != nil {
		return Result{}, fmt.Errorf("goalrun: materialize for graph digest: %w", merr)
	}
	g = mat.Graph
	graphDigest := workgraph.DigestGraph(g)
	if graphDigest == "" {
		return Result{}, fmt.Errorf("goalrun: empty graph digest after materialize DigestGraph")
	}
	if stored := strings.TrimSpace(g.PlanDigest); stored != "" && stored != graphDigest {
		return Result{}, fmt.Errorf("goalrun: materialize PlanDigest %q != DigestGraph %q", stored, graphDigest)
	}
	// Prefer verified PlanDigest when present (equals DigestGraph); else computed.
	if pd := strings.TrimSpace(g.PlanDigest); pd != "" {
		graphDigest = pd
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		runID = "run_" + shortID(fmt.Sprintf("%s|%d", projectID, now.UnixNano()))
	}
	// Forced restart / exactly-once: envelope validation and seed merge happen
	// BEFORE any eventlog open, claim recovery, reserve, or launch. GraphID
	// mismatch is never allowed (deleted soft-allow).
	if req.Resume && strings.TrimSpace(runID) == "" {
		return Result{}, fmt.Errorf("goalrun: resume requires run_id")
	}
	priorSucceeded, attemptGen, priorOutcomes, loadedCP, hasCP, loadedPart, hasPart, seedErr := loadAndValidateResumeSeeds(
		req.HomeDir, projectID, runID, g.GraphID, execPlanDigest, graphDigest, g.Version,
		req.Resume, req.PriorSucceeded,
	)
	if seedErr != nil {
		return Result{}, seedErr
	}
	_ = hasPart
	_ = loadedPart
	// Parent-boundary preflight: entire PriorSucceeded + AttemptGeneration against
	// current graph + routeByID BEFORE OpenEventLog / inventory / ledger / route / reserve.
	// Ghost keys or key/value mismatches must not create eventlog or capacity spend.
	currentCCD := map[string]string{}
	for _, it := range g.Items {
		pr := routeByID[it.ID]
		ccd, ccdErr := routecontract.ChildContractDigest(routecontract.ChildAssignment{
			ExecutionPlanDigest: execPlanDigest,
			WorkItemID:          it.ID,
			TaskClass:           string(pr.Class),
			Depth:               pr.Depth,
			Permission:          pr.Permission,
			OutputContract:      it.OutputContract,
		})
		if ccdErr != nil {
			return Result{}, fmt.Errorf("goalrun: parent preflight child %s contract digest: %w", it.ID, ccdErr)
		}
		currentCCD[it.ID] = ccd
	}
	if err := validateParentResumeAgainstGraph(g.Items, routeByID, priorSucceeded, attemptGen, execPlanDigest, runID, currentCCD); err != nil {
		return Result{}, err
	}
	// PriorOutcomes vs current graph/route CCD/class/depth/permission — BEFORE inventory.
	routes := map[string]priorRouteSnap{}
	for id, pr := range routeByID {
		routes[id] = priorRouteSnap{Class: string(pr.Class), Depth: pr.Depth, Permission: pr.Permission}
	}
	if err := validatePriorOutcomesAgainstGraphMaps(priorOutcomes, g.Items, execPlanDigest, runID, currentCCD, routes); err != nil {
		return Result{}, err
	}
	resumed := req.Resume || len(priorSucceeded) > 0 || len(priorOutcomes) > 0
	_ = hasCP
	// Event log is a third durable resume source. Strict open/read/recover with
	// full error propagation; validate before any recovery append; merge next
	// gens as parsed g+1 (never hardcode 1); re-run parent validation before
	// inventory/ledger so ghost/event-derived keys cannot spend capacity.
	if req.Resume || len(priorSucceeded) > 0 || len(attemptGen) > 0 || len(priorOutcomes) > 0 {
		var eventResumed bool
		var eerr error
		attemptGen, eventResumed, eerr = applyEventLogResumeSource(
			req.HomeDir, projectID, runID, execPlanDigest, graphDigest, g.GraphID, g.Version,
			g.Items, attemptGen, priorSucceeded,
		)
		if eerr != nil {
			return Result{}, eerr
		}
		if eventResumed {
			resumed = true
		}
		// Complete parent resume validation again after event-derived merges,
		// BEFORE LoadInventory / OpenLedger / route / reserve.
		if err := validateParentResumeAgainstGraph(g.Items, routeByID, priorSucceeded, attemptGen, execPlanDigest, runID, currentCCD); err != nil {
			return Result{}, err
		}
	}
	_ = loadedCP
	// First Materialize already captured GraphDigest; re-Materialize is idempotent
	// on the same registry and must not change the canonical graph identity.
	if mat2, err := reg.Materialize(projectID, def, approval, now); err != nil {
		return Result{}, err
	} else if d2 := workgraph.DigestGraph(mat2.Graph); d2 != graphDigest {
		return Result{}, fmt.Errorf("goalrun: rematerialize graph digest drift: first=%s second=%s", graphDigest, d2)
	}

	pinProv := strings.TrimSpace(req.Provider)
	pinModel := strings.TrimSpace(req.Model)
	wantAuto := pinProv == "" || pinModel == "" ||
		strings.EqualFold(pinProv, "auto") || strings.EqualFold(pinModel, "auto")

	// routeByID already validated immediately after Decompose (all items valid).
	loadInv := req.LoadInventory
	if loadInv == nil {
		loadInv = func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return capacitysnapshot.LoadRouteInventory(ctx, capacitysnapshot.LoadOptions{
				RepoPath: repo, Now: at,
			})
		}
	}
	openLed := req.OpenLedger
	if openLed == nil {
		openLed = capacityledger.Open
	}

	var inv *autoroute.Inventory
	var snap *capacitysnapshot.Snapshot
	var ledger *capacityledger.Ledger
	var inventoryReportDigest string
	// Product path (auto-route OR explicit pin): load inventory + ledger.
	needInvLedger := wantAuto || (pinProv != "" && pinModel != "")
	if needInvLedger {
		i, s, lerr := loadInv(ctx, req.RepoPath, now)
		if lerr != nil {
			out := Result{
				GraphID: g.GraphID, PlanDigest: execPlanDigest, GraphDigest: graphDigest,
				Status: "blocked", Message: "inventory load failed: " + lerr.Error(),
			}
			for _, it := range g.Items {
				cr := ChildReport{
					ChildID: it.ID, Intent: it.Intent, Owner: it.Owner,
					RouteRequirement: it.RouteRequirement,
					Stage:            "unavailable", Unavailable: true,
					CapacityNote: "inventory_unavailable",
					RouteReason:  lerr.Error(),
					NextAction:   "providers_refresh_or_auth",
				}
				out.Children = append(out.Children, cr)
				emitChild(req.ReportOut, cr)
			}
			return out, lerr
		}
		inv, snap = &i, &s
		inventoryReportDigest = s.Digest
		// Ledger Open is mandatory for product spend (auto and explicit pin).
		led, oerr := openLed(nowFn)
		if oerr != nil {
			out := Result{
				GraphID: g.GraphID, PlanDigest: execPlanDigest, GraphDigest: graphDigest,
				Status: "blocked", Message: "capacity ledger open failed: " + oerr.Error(),
			}
			for _, it := range g.Items {
				cr := ChildReport{
					ChildID: it.ID, Intent: it.Intent, Owner: it.Owner,
					RouteRequirement: it.RouteRequirement,
					Stage:            "unavailable", Unavailable: true,
					CapacityNote: "ledger_open_failed",
					RouteReason:  oerr.Error(),
					NextAction:   "capacity_ledger_recover",
				}
				out.Children = append(out.Children, cr)
				emitChild(req.ReportOut, cr)
			}
			return out, fmt.Errorf("goalrun: capacity ledger open: %w", oerr)
		}
		ledger = led
	}

	// Resume: non-succeeded children whose prior reservation is durably
	// released/aborted must use the next canonical generation before reserve.
	// Binds only to ledger + checkpoint AttemptIDs + event-derived attemptGen
	// (never prose). Prior-succeeded children are never bumped.
	if resumed && ledger != nil {
		var cpKids []ChildReport
		if hasCP {
			cpKids = loadedCP.Children
		}
		var rerr error
		attemptGen, rerr = applyReleasedReservationResumeGenerations(
			ledger, projectID, runID, execPlanDigest,
			g.Items, priorSucceeded, attemptGen, cpKids,
		)
		if rerr != nil {
			return Result{}, rerr
		}
		// Parent boundary again after ledger-derived gen bumps (ghost keys).
		if err := validateParentResumeAgainstGraph(g.Items, routeByID, priorSucceeded, attemptGen, execPlanDigest, runID, currentCCD); err != nil {
			return Result{}, err
		}
	}

	dryRun := false
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}

	// Capacity ledger + workflow share unique runID (not graph-only / not global local-project).
	usedProviders := map[string]bool{}
	children := make([]ChildReport, 0, len(g.Items))
	childRoutes := map[string]workflowrun.ChildRoute{}
	// Track which children hold live reservations for post-execute reconcile.
	holds := map[string]capacityHold{}
	// Per-child decision-set candidates for model_unavailable same-depth alternate.
	sameDepthAlts := map[string][]workflowrun.AlternateCandidate{}
	var routeExcludes []RouteExclude
	recordExclude := func(childID, provider, reason, msg string, hard, soft, claimed bool) {
		if strings.TrimSpace(provider) == "" && strings.TrimSpace(reason) == "" {
			return
		}
		routeExcludes = append(routeExcludes, RouteExclude{
			ChildID: childID, Provider: provider, Reason: reason,
			HardEligible: hard, SoftExcluded: soft, Claimed: claimed, Message: msg,
		})
	}

	for _, it := range g.Items {
		// Structured requirement from post-Decompose prevalidation (exact; no invent).
		pr := routeByID[it.ID]
		childClass := pr.Class
		depth := pr.Depth
		perm := pr.Permission
		ccd, ccdErr := routecontract.ChildContractDigest(routecontract.ChildAssignment{
			ExecutionPlanDigest: execPlanDigest,
			WorkItemID:          it.ID,
			TaskClass:           string(childClass),
			Depth:               depth,
			Permission:          perm,
			OutputContract:      it.OutputContract,
		})
		if ccdErr != nil {
			// Should not happen after strict parse; fail closed if output_contract empty.
			return Result{}, fmt.Errorf("goalrun: child %s contract digest: %w", it.ID, ccdErr)
		}
		cr := ChildReport{
			ChildID: it.ID, Intent: it.Intent, Owner: it.Owner,
			RouteRequirement:    it.RouteRequirement,
			TaskClass:           string(childClass),
			ExecutionPlanDigest: execPlanDigest,
			ChildContractDigest: ccd,
			Depth:               depth,
			Permission:          perm,
			Stage:               "routing",
			NextAction:          "route_child",
		}

		// Exactly-once: any present priorSucceeded entry must fully validate or
		// fail closed before route/reserve/execute. Never treat present-but-invalid
		// (stale/legacy empty attempt/evidence/terminal) as absent and re-spend.
		if prior, ok := priorSucceeded[it.ID]; ok {
			// Byte-exact durable terminal/attempt/evidence — never EqualFold/TrimSpace normalize.
			if err := requireExactSucceededTerminal("resume prior "+it.ID, prior.Terminal); err != nil {
				return Result{}, fmt.Errorf("%w (no re-route)", err)
			}
			if err := requireExactDurableToken("resume prior "+it.ID+" attempt_id", prior.AttemptID); err != nil {
				return Result{}, fmt.Errorf("%w (no re-route)", err)
			}
			if err := requireExactDurableToken("resume prior "+it.ID+" output_evidence", prior.OutputEvidence); err != nil {
				return Result{}, fmt.Errorf("%w (no re-route)", err)
			}
			// Exact equality against the current materialized child contract.
			// Never rewrite historical prior TaskClass/plan/CCD/WorkItemID to match
			// the current plan — stale success must not impersonate a new approval.
			if err := requirePriorMatchesCurrentContract(it.ID, prior, string(childClass), execPlanDigest, ccd, runID); err != nil {
				return Result{}, err
			}
			cr.Provider = prior.Provider
			cr.Model = prior.Model
			cr.Depth = firstNonEmpty(prior.Depth, depth)
			cr.Permission = firstNonEmpty(prior.Permission, perm)
			cr.AccountRef = prior.AccountRef
			cr.InstallRef = prior.InstallRef
			cr.WindowKind = prior.WindowKind
			cr.ReservationID = prior.ReservationID
			cr.SupersedesAttemptID = prior.SupersedesAttemptID
			cr.RerouteEventRef = prior.RerouteEventRef
			cr.IntegrateCommitSHA = prior.IntegrateCommitSHA
			cr.RouteReason = firstNonEmpty(prior.RouteReason, "resume_prior_succeeded")
			cr.AttemptID = prior.AttemptID
			cr.OutputEvidence = prior.OutputEvidence
			cr.WorktreePath = prior.WorktreePath
			cr.Terminal = "succeeded"
			cr.Stage = "resumed"
			cr.NextAction = "reuse_no_reexec"
			cr.CapacityNote = "resume_no_re_reserve"
			if prior.ActualCapacity != nil {
				cr.CapacityActual = prior.ActualCapacity
			}
			cr.ActualSource = prior.ActualSource
			// Carry capacity fields from prior checkpoint report for THIS attempt only
			// (never first ChildID match — dual MU rows share ChildID).
			if loadedCP.Children != nil {
				for _, pc := range loadedCP.Children {
					if !idExact(pc.AttemptID, prior.AttemptID) {
						continue
					}
					cr.CapacityBefore = pc.CapacityBefore
					cr.CapacityReserved = pc.CapacityReserved
					if pc.CapacityActual != nil {
						cr.CapacityActual = pc.CapacityActual
					}
					cr.CapacityAfter = pc.CapacityAfter
					cr.CapacityState = firstNonEmpty(pc.CapacityState, "reconciled_or_released")
					cr.WindowKind = firstNonEmpty(cr.WindowKind, pc.WindowKind)
					cr.InstallRef = firstNonEmpty(cr.InstallRef, pc.InstallRef)
					cr.ReservationID = firstNonEmpty(cr.ReservationID, pc.ReservationID)
					cr.ClaimEventID = firstNonEmpty(cr.ClaimEventID, pc.ClaimEventID)
					cr.LaunchEventID = firstNonEmpty(cr.LaunchEventID, pc.LaunchEventID)
					cr.PIDEventID = firstNonEmpty(cr.PIDEventID, pc.PIDEventID)
					cr.TerminalEventID = firstNonEmpty(cr.TerminalEventID, pc.TerminalEventID)
					cr.IntegrateEventID = firstNonEmpty(cr.IntegrateEventID, pc.IntegrateEventID)
					if pc.ProviderPID > 0 {
						cr.ProviderPID = pc.ProviderPID
					}
					if pc.CapacityNote != "" {
						cr.CapacityNote = pc.CapacityNote + "; resume_reuse"
					}
					break
				}
			}
			// Resume capacity: reload exact ledger entry by attempt ID only.
			// Never fabricate Before=1.0 / Reserved=0.05 / After / ActualSource=estimated
			// when checkpoint or ledger facts are absent (fail closed for qualification).
			if ledger != nil && strings.TrimSpace(cr.AttemptID) != "" {
				if ent, ok := ledger.Get(projectID, runID, cr.AttemptID); ok {
					applyCapacityBeforeFromEntry(&cr, ent)
					applyCapacityAfterFromEntry(&cr, ent)
					cr.CapacityActual = ent.Actual
					cr.CapacityState = ent.State
					cr.ActualSource = ent.ActualSource
					cr.CapacityActualConfidence = string(ent.ActualConfidence)
					cr.AccountRef = firstNonEmpty(cr.AccountRef, ent.AccountRef)
					cr.InstallRef = firstNonEmpty(cr.InstallRef, ent.InstallRef)
					cr.WindowKind = firstNonEmpty(cr.WindowKind, ent.WindowKind)
					cr.ReservationID = firstNonEmpty(cr.ReservationID, ent.ReservationID)
					cr.Provider = firstNonEmpty(cr.Provider, ent.Provider)
					cr.Model = firstNonEmpty(cr.Model, ent.Model)
					cr.Depth = firstNonEmpty(cr.Depth, ent.Depth)
					if ent.ReleaseReason != "" {
						cr.CapacityNote = firstNonEmpty(cr.CapacityNote, "resume") +
							"; release_reason=" + ent.ReleaseReason
					} else {
						cr.CapacityNote = firstNonEmpty(cr.CapacityNote, "resume_ledger_reload")
					}
					// A forced interrupt can prevent the post-run provider
					// observation after this child has already succeeded. Keep
					// that exact reservation live across the durable resume so
					// the resumed process can observe and reconcile it. Never
					// reopen released/reconciled entries.
					if ent.State == "reserved" || ent.State == "observed" {
						holds[cr.AttemptID] = capacityHold{
							projectID: projectID, runID: runID, attemptID: cr.AttemptID,
						}
					}
				} else {
					// Missing durable entry: leave capacity fields unknown; note fail-closed.
					cr.CapacityNote = firstNonEmpty(cr.CapacityNote, "resume") +
						"; capacity_ledger_entry_missing"
				}
			} else if cr.CapacityBefore == nil && cr.CapacityReserved == nil {
				cr.CapacityNote = firstNonEmpty(cr.CapacityNote, "resume") +
					"; capacity_facts_unknown"
			}
			if strings.TrimSpace(cr.WindowKind) == "" || strings.TrimSpace(cr.InstallRef) == "" ||
				strings.TrimSpace(cr.ReservationID) == "" {
				return Result{}, fmt.Errorf("goalrun: resume child %s attempt %s missing window/install/reservation (fail closed)", it.ID, cr.AttemptID)
			}
			if cr.Provider != "" {
				usedProviders[cr.Provider] = true
			}
			if cr.Provider == "" || cr.Model == "" {
				return Result{}, fmt.Errorf("goalrun: resume child %s missing provider/model (no fixture invent)", it.ID)
			}
			// Resume must not reintroduce fixture as a product route identity.
			if strings.EqualFold(cr.Provider, "fixture") || strings.EqualFold(cr.Model, "fixture-model") {
				return Result{}, fmt.Errorf("goalrun: resume child %s has fixture route identity (fail closed)", it.ID)
			}
			// Keep priorSucceeded entry immutable; only surface identity on ChildReport.
			cr.Generation = prior.Generation
			cr.TaskClass = prior.TaskClass
			cr.ExecutionPlanDigest = prior.ExecutionPlanDigest
			cr.ChildContractDigest = prior.ChildContractDigest
			childRoutes[it.ID] = workflowrun.ChildRoute{
				Provider: cr.Provider, Model: cr.Model,
				Depth: cr.Depth, Permission: cr.Permission, TaskClass: cr.TaskClass,
				AccountRef: cr.AccountRef, InstallRef: cr.InstallRef,
				WindowKind: cr.WindowKind, ReservationID: cr.ReservationID,
				RouteReason: cr.RouteReason,
			}
			children = append(children, cr)
			emitChild(req.ReportOut, cr)
			continue
		}

		if !wantAuto {
			if pinProv == "" || pinModel == "" {
				return Result{}, fmt.Errorf("goalrun: explicit pin requires provider+model (no fixture invent)")
			}
			// Production explicit pin: preserve provider/model, bind exact inventory
			// identity, reserve capacity — never bypass ledger or launch unreserved.
			// Per-child classified TaskClass (BindExplicitPinWithClass; no Tera wrapper).
			if inv == nil || snap == nil || ledger == nil {
				return Result{}, fmt.Errorf("goalrun: explicit pin requires fresh inventory+ledger (fail closed)")
			}
			bound, berr := autoroute.BindExplicitPinWithClass(pinProv, pinModel, depth, perm, childClass, inv)
			if berr != nil {
				cr.Stage = "unavailable"
				cr.Unavailable = true
				cr.Terminal = "pin_fail"
				cr.CapacityNote = "pin_bind_failed"
				cr.RouteReason = berr.Error()
				cr.NextAction = "providers_refresh_or_auth"
				children = append(children, cr)
				emitChild(req.ReportOut, cr)
				// Pin fail-closed for this child: no reserve/spend; continue so other
				// children still surface class evidence (do not fall back provider).
				continue
			}
			cr.Provider = bound.Provider
			cr.Model = bound.Model
			cr.Depth = bound.Effort
			cr.Permission = bound.Permission
			cr.AccountRef = bound.AccountRef
			cr.InstallRef = bound.InstallRef
			cr.WindowKind = bound.WindowKind
			cr.Stage = "planned"
			cr.RouteReason = "explicit_pin_bound"
			cr.NextAction = "await_wave"
			// Capacity reserve before spend (same durable path as auto-route).
			gen := attemptGen[it.ID]
			attID := workflowrun.AttemptID(it.ID, execPlanDigest, runID, gen)
			cr.AttemptID = attID
			e, rerr := ledger.Reserve(capacityledger.ReserveInput{
				ProjectID: projectID, RunID: runID, AttemptID: attID,
				PlanDigest: execPlanDigest, GraphDigest: graphDigest,
				TaskClass: cr.TaskClass, ChildContractDigest: ccd,
				Provider: cr.Provider, Model: cr.Model, Depth: cr.Depth,
				AccountRef: cr.AccountRef, InstallRef: cr.InstallRef, WindowKind: cr.WindowKind,
				Snapshot: snap, RouteReason: cr.RouteReason,
			})
			if rerr != nil {
				cr.Stage = "unavailable"
				cr.Unavailable = true
				cr.Terminal = "reserve_failed"
				cr.CapacityNote = "reserve_failed"
				cr.RouteReason = rerr.Error()
				cr.NextAction = "capacity_refresh"
				children = append(children, cr)
				emitChild(req.ReportOut, cr)
				return Result{
					GraphID: g.GraphID, PlanDigest: execPlanDigest, GraphDigest: graphDigest, Children: children,
					Status: "blocked", Message: "explicit pin reserve failed: " + rerr.Error(),
				}, rerr
			}
			cr.ReservationID = e.ReservationID
			cr.CapacityState = e.State
			if e.Before > 0 {
				b := e.Before
				cr.CapacityBefore = &b
			}
			if e.Reserved > 0 {
				r := e.Reserved
				cr.CapacityReserved = &r
			}
			cr.CapacityNote = "pin_reserved"
			holds[attID] = capacityHold{projectID: projectID, runID: runID, attemptID: attID}
			usedProviders[cr.Provider] = true
			childRoutes[it.ID] = workflowrun.ChildRoute{
				Provider: cr.Provider, Model: cr.Model, Depth: cr.Depth,
				Permission: cr.Permission, TaskClass: cr.TaskClass,
				AccountRef: cr.AccountRef, InstallRef: cr.InstallRef,
				WindowKind: cr.WindowKind, ReservationID: cr.ReservationID,
				RouteReason: cr.RouteReason,
			}
			children = append(children, cr)
			emitChild(req.ReportOut, cr)
			continue
		}

		// Independent per-child auto-route (shared inventory; durable rehydrate).
		// TaskClass/depth/permission from strict ParseRouteRequirement — never invent.
		routeIn := autoroute.Input{
			AutoRoute:   true,
			Permission:  perm,
			ProjectID:   projectID,
			DecisionKey: "goalrun|" + g.GraphID + "|" + it.ID,
			Inventory:   inv,
			Effort:      depth,
			TaskClass:   childClass,
			Now:         now,
		}
		res, rerr := autoroute.Resolve(routeIn)
		if rerr != nil || res.Outcome != autoroute.OutcomeSelected || res.Provider == "" {
			cr.Stage = "unavailable"
			cr.Unavailable = true
			cr.Terminal = "no_route"
			cr.CapacityNote = "route_unavailable"
			if rerr != nil {
				cr.RouteReason = rerr.Error()
			} else {
				cr.RouteReason = string(res.Outcome) + ": " + res.Message
			}
			// Measure candidate hard-excludes from decision set (no claim).
			if res.Decision != nil {
				for _, cv := range res.Decision.Candidates {
					if cv.Provider == "" {
						continue
					}
					if !cv.HardEligible || cv.SoftExcluded {
						recordExclude(it.ID, cv.Provider, ClassifyExcludeReason(cr.CapacityNote, cr.Terminal, cv.Provider),
							cr.RouteReason, cv.HardEligible, cv.SoftExcluded, false)
					}
				}
			}
			recordExclude(it.ID, firstNonEmpty(res.Provider, "none"), ClassifyExcludeReason(cr.CapacityNote, cr.Terminal, cr.RouteReason),
				cr.RouteReason, false, true, false)
			cr.NextAction = "retry_or_refresh"
			children = append(children, cr)
			emitChild(req.ReportOut, cr)
			continue
		}

		// Required depth from route_requirement is authoritative for invocation.
		// Never let winner inventory default (often medium) override low/high.
		reqDepth := normalizeDepth(depth)
		if reqDepth == "" {
			reqDepth = "medium"
		}
		// Atomic selected route: every dimension moves together on diversification.
		type selectedRoute struct {
			Provider, Model, Depth, Permission, AccountRef, InstallRef, WindowKind string
		}
		sel := selectedRoute{
			Provider: res.Provider, Model: res.Model,
			Depth:      firstNonEmpty(normalizeDepth(res.Effort), reqDepth),
			Permission: normalizePerm(perm),
			AccountRef: strings.TrimSpace(res.AccountRef),
			InstallRef: strings.TrimSpace(res.InstallRef),
			WindowKind: strings.TrimSpace(res.WindowKind),
		}
		probeOnly := false
		var probeAlternate selectedRoute
		if it.ID == "wi_research" &&
			req.CanaryUnavailableProbeProvider != "" &&
			req.CanaryUnavailableProbeModel != "" {
			if sel.Provider != req.CanaryUnavailableProbeProvider {
				return Result{}, fmt.Errorf("goalrun: canary unavailable probe provider %q does not match live selected provider %q", req.CanaryUnavailableProbeProvider, sel.Provider)
			}
			if decisionHasHardEligibleRoute(res.Decision, sel.Provider, req.CanaryUnavailableProbeModel, reqDepth, normalizePerm(perm)) {
				return Result{}, fmt.Errorf("goalrun: canary unavailable probe model %q is already hard-eligible; refusing to demote a live route into a probe", req.CanaryUnavailableProbeModel)
			}
			if !declaredModelSupports(req.CanaryUnavailableProbeProvider, req.CanaryUnavailableProbeModel, reqDepth) {
				return Result{}, fmt.Errorf("goalrun: canary unavailable probe is not an adapter-declared model/depth")
			}
			probeAlternate = sel
			sel.Model = req.CanaryUnavailableProbeModel
			probeOnly = true
		}
		// Prefer alternate provider if already used and decision lists others.
		// Diversification must update provider/model/depth/permission/account/window
		// atomically — never leave original account/window on a new provider.
		diversifiedFrom := ""
		if usedProviders[sel.Provider] && res.Decision != nil {
			for _, cv := range res.Decision.Candidates {
				if cv.Provider == "" || usedProviders[cv.Provider] || !cv.HardEligible || cv.SoftExcluded {
					continue
				}
				if isReadOnlyPerm(perm) && !providerSupportsReadOnly(cv.Provider) {
					continue
				}
				obsDepth := normalizeDepth(cv.Effort)
				obsPerm := normalizePerm(cv.Permission)
				if obsDepth == "" || obsDepth != reqDepth {
					continue
				}
				if obsPerm == "" || obsPerm != normalizePerm(perm) {
					continue
				}
				diversifiedFrom = sel.Provider
				sel = selectedRoute{
					Provider: cv.Provider, Model: cv.Model,
					Depth: obsDepth, Permission: obsPerm,
					AccountRef: strings.TrimSpace(cv.AccountRef),
					InstallRef: strings.TrimSpace(cv.InstallRef),
					WindowKind: strings.TrimSpace(cv.WindowKind),
				}
				break
			}
		}
		prov, model := sel.Provider, sel.Model
		// Guard: never accept Antigravity (or any non-RO provider) for read-only.
		if isReadOnlyPerm(perm) && !providerSupportsReadOnly(prov) {
			cr.Stage = "unavailable"
			cr.Unavailable = true
			cr.Terminal = "permission_denied"
			cr.Provider = prov
			cr.Model = model
			cr.RouteReason = "provider " + prov + " does not support read-only for child " + it.ID
			cr.CapacityNote = "permission_ineligible"
			cr.NextAction = "retry_other_route"
			children = append(children, cr)
			emitChild(req.ReportOut, cr)
			continue
		}
		// Fail closed if selected route cannot bind required depth.
		// Use atomic selectedRoute depth (post-diversification), not original res.Effort.
		selDepth := normalizeDepth(firstNonEmpty(sel.Depth, reqDepth))
		if selDepth != reqDepth {
			cr.Stage = "unavailable"
			cr.Unavailable = true
			cr.Terminal = "depth_mismatch"
			cr.Provider = prov
			cr.Model = model
			cr.Depth = reqDepth
			cr.RouteReason = fmt.Sprintf(
				"depth requirement=%s selection=%s refused (no silent substitution)",
				reqDepth, selDepth,
			)
			cr.CapacityNote = "depth_ineligible"
			cr.NextAction = "retry_other_route"
			children = append(children, cr)
			emitChild(req.ReportOut, cr)
			continue
		}
		// Invocation depth == required depth (bound into ChildRoute.Depth → agent Effort).
		effort := reqDepth
		cr.Provider = prov
		cr.Model = model
		cr.Depth = effort
		// Route reason must name the *actual* selected provider/model. Multi-provider
		// diversification rewrites prov/model after Resolve; never leave the original
		// winner line (e.g. antigravity) on a Grok child.
		winnerLine := res.Message
		if res.Explain != nil && res.Explain.WinnerLine != "" {
			winnerLine = res.Explain.WinnerLine
		}
		if diversifiedFrom != "" && !strings.EqualFold(diversifiedFrom, prov) {
			winnerLine = fmt.Sprintf(
				"Winner: %s/%s depth=%s (multi-provider diversification from %s)",
				prov, model, effort, diversifiedFrom,
			)
		} else if !strings.Contains(strings.ToLower(winnerLine), strings.ToLower(prov)) {
			// Resolve message omitted provider or pointed at a different pin — restate truth.
			winnerLine = fmt.Sprintf("Winner: %s/%s depth=%s", prov, model, effort)
		}
		if probeOnly {
			winnerLine = fmt.Sprintf(
				"Canary paid capability probe: %s/%s depth=%s; verified alternate=%s/%s",
				prov, model, effort, probeAlternate.Provider, probeAlternate.Model,
			)
		}
		cr.RouteReason = fmt.Sprintf(
			"%s; permission=%s; depth requirement=%s selection=%s invocation=%s",
			winnerLine, perm, reqDepth, selDepth, effort,
		)

		// After a successful route, record hard-eligible non-winner candidates
		// from the same decision set (Claimed=false). Soft-excluded keep reason
		// soft_excluded; other hard-eligible non-winners use eligible_not_chosen.
		// Real measured exclusion evidence for canary unavailable_retry.
		// Stash same-depth alternates only when CandidateView carries observed
		// Effort/Permission that exactly match the requirement — never synthesize.
		if res.Decision != nil {
			var softCands []SoftExcludedCandidate
			var alts []workflowrun.AlternateCandidate
			for _, cv := range res.Decision.Candidates {
				softCands = append(softCands, SoftExcludedCandidate{
					Provider: cv.Provider, Model: cv.Model,
					HardEligible: cv.HardEligible, SoftExcluded: cv.SoftExcluded,
				})
				if !cv.HardEligible || cv.SoftExcluded {
					continue
				}
				if strings.TrimSpace(cv.Provider) == "" || strings.TrimSpace(cv.Model) == "" {
					continue
				}
				obsDepth := normalizeDepth(cv.Effort)
				obsPerm := normalizePerm(cv.Permission)
				if obsDepth == "" || obsDepth != reqDepth {
					continue
				}
				if obsPerm == "" || obsPerm != normalizePerm(perm) {
					continue
				}
				// Capacity identity must come from the candidate itself (first-class),
				// not a post-hoc accountWindowForProvider guess.
				accRef := strings.TrimSpace(cv.AccountRef)
				winKind := strings.TrimSpace(cv.WindowKind)
				alts = append(alts, workflowrun.AlternateCandidate{
					Provider: cv.Provider, Model: cv.Model,
					Effort: obsDepth, Permission: obsPerm,
					AccountRef: accRef, InstallRef: strings.TrimSpace(cv.InstallRef),
					WindowKind:   winKind,
					HardEligible: true, SoftExcluded: false,
				})
			}
			if probeOnly {
				alts = prependAlternateUnique(alts, workflowrun.AlternateCandidate{
					Provider: probeAlternate.Provider, Model: probeAlternate.Model,
					Effort: probeAlternate.Depth, Permission: probeAlternate.Permission,
					AccountRef: probeAlternate.AccountRef, InstallRef: probeAlternate.InstallRef,
					WindowKind:   probeAlternate.WindowKind,
					HardEligible: true,
				})
			}
			if len(alts) > 0 {
				sameDepthAlts[it.ID] = alts
			}
			for _, ex := range HardEligibleNonWinnerExcludes(it.ID, prov, softCands) {
				recordExclude(ex.ChildID, ex.Provider, ex.Reason, ex.Message, ex.HardEligible, ex.SoftExcluded, ex.Claimed)
			}
		}

		if ledger != nil && snap != nil {
			// Exact same deterministic workflow attempt ID as workflowrun claim/launch
			// so prior CapacityTransition binds the failed attempt (never WorkItemID alone).
			gen := attemptGen[it.ID]
			attemptID := workflowrun.AttemptID(it.ID, execPlanDigest, runID, gen)
			cr.AttemptID = attemptID
			// Bind exact selected route account/window (post-diversification).
			entry, rerr := ledger.Reserve(capacityledger.ReserveInput{
				ProjectID: projectID, RunID: runID, AttemptID: attemptID,
				PlanDigest: execPlanDigest, GraphDigest: graphDigest,
				TaskClass: cr.TaskClass, ChildContractDigest: ccd,
				Provider: prov, Model: model, Depth: effort,
				AccountRef: sel.AccountRef, InstallRef: sel.InstallRef, WindowKind: sel.WindowKind,
				Snapshot: snap, RouteReason: cr.RouteReason,
			})
			// Executable attempt must be state=reserved (not refused/reconciled/released).
			if rerr != nil || strings.TrimSpace(entry.State) != "reserved" {
				cr.Unavailable = true
				cr.Stage = "unavailable"
				cr.Terminal = "capacity_refused"
				cr.CapacityNote = "capacity_refused"
				if rerr != nil {
					cr.RouteReason = rerr.Error()
				} else {
					cr.RouteReason = "reserve state=" + entry.State + " (want reserved)"
				}
				// Capacity refuse before claim: exclude provider, Claimed=false.
				recordExclude(it.ID, prov, "exhausted", cr.RouteReason, true, false, false)
				cr.NextAction = "retry_other_route"
				children = append(children, cr)
				emitChild(req.ReportOut, cr)
				continue
			}
			b, r := entry.Before, entry.Reserved
			cr.AccountRef = entry.AccountRef
			cr.InstallRef = sel.InstallRef
			cr.WindowKind = entry.WindowKind
			cr.ReservationID = entry.ReservationID
			cr.Permission = sel.Permission
			cr.CapacityBefore = &b
			cr.CapacityReserved = &r
			cr.CapacityActual = entry.Actual
			cr.CapacityAfter = entry.After
			cr.CapacityState = entry.State
			cr.CapacityNote = "policy=" + string(entry.Policy)
			// Structured before-window evidence from Reserve (no prose defaults).
			cr.CapacityBeforeSource = entry.BeforeSource
			cr.CapacityBeforeFreshness = entry.Freshness
			cr.CapacityBeforeConfidence = string(entry.Confidence)
			if entry.BeforeCapturedAt != nil {
				cr.CapacityBeforeCapturedAt = entry.BeforeCapturedAt.UTC()
			}
			applyCapacityAfterFromEntry(&cr, entry)
			if dryRun {
				// Route preview only: release without execution.
				if rel, lerr := ledger.Release(projectID, runID, attemptID, "goalrun_dry_run_preview"); lerr == nil {
					cr.CapacityState = rel.State
					cr.CapacityNote += "; released=dry_run_preview"
					cr.RouteReason = rel.RouteReason
				}
			} else {
				// Attempt-keyed holds only — never ChildID (MU failed+winner share ChildID).
				holds[attemptID] = capacityHold{projectID: projectID, runID: runID, attemptID: attemptID}
			}
		} else if wantAuto {
			// Auto path without ledger+snap is unreachable after fail-closed open;
			// still refuse unreserved execution if wiring regresses.
			cr.Unavailable = true
			cr.Stage = "unavailable"
			cr.Terminal = "capacity_refused"
			cr.CapacityNote = "ledger_or_snapshot_unavailable"
			cr.NextAction = "capacity_ledger_recover"
			recordExclude(it.ID, prov, "capacity_refused", cr.CapacityNote, true, false, false)
			children = append(children, cr)
			emitChild(req.ReportOut, cr)
			continue
		} else {
			cr.CapacityNote = "ledger_unavailable"
		}

		usedProviders[prov] = true
		cr.Stage = "planned"
		cr.NextAction = "await_wave"
		// Full atomic route after Reserve: permission + reservation required for
		// capacity-backed auto routes (exactRouteMatch cannot pass on empties).
		cr.Permission = sel.Permission
		childRoutes[it.ID] = workflowrun.ChildRoute{
			Provider: prov, Model: model, Depth: effort,
			Permission:          sel.Permission,
			TaskClass:           cr.TaskClass,
			AccountRef:          cr.AccountRef,
			InstallRef:          firstNonEmpty(cr.InstallRef, sel.InstallRef),
			WindowKind:          firstNonEmpty(cr.WindowKind, sel.WindowKind),
			ReservationID:       cr.ReservationID,
			RouteReason:         cr.RouteReason,
			CapabilityProbeOnly: probeOnly,
		}
		children = append(children, cr)
		emitChild(req.ReportOut, cr)
	}

	// Dry-run route preview: do not launch children.
	if dryRun {
		out := Result{
			GraphID: g.GraphID, PlanDigest: execPlanDigest, GraphDigest: graphDigest, Children: children,
			Status: "planned", Message: "dry-run route preview; no child execution",
		}
		// Actual execution metrics stay empty/false. Planned* is structural only.
		out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed = nil, nil, nil
		out.MultiProviderOK = false
		out.MultiModelOrDepthOK = false
		out.PlannedProvidersUsed, out.PlannedModelsUsed, out.PlannedDepthsUsed = collectPlannedRouteUsage(children)
		out.PlannedMultiProviderOK = len(out.PlannedProvidersUsed) >= 2
		out.PlannedMultiModelOrDepthOK = len(out.PlannedModelsUsed) >= 2 || len(out.PlannedDepthsUsed) >= 2
		out.HumanSummary = fmt.Sprintf(
			"goal graph %s dry-run children=%d planned_providers=%v planned_models=%v planned_depths=%v multi_provider=false multi_model_or_depth=false",
			g.GraphID, len(children), out.PlannedProvidersUsed, out.PlannedModelsUsed, out.PlannedDepthsUsed,
		)
		return out, nil
	}

	// Filter definition to only available (routed) required children when auto-route
	// left some unavailable — workflowrun still needs a valid graph. Keep full def;
	// unavailable children are not in childRoutes and will use pin defaults only if
	// they remain in the graph. Remove unavailable required items by blocking early
	// when any required child is unavailable.
	for _, c := range children {
		if c.Unavailable {
			// Skip workflow launch for graphs with route/capacity unavailable required kids.
			// Mark blocked honestly.
			out := Result{
				GraphID: g.GraphID, PlanDigest: execPlanDigest, GraphDigest: graphDigest, Children: children,
				Status: "blocked", Message: "one or more children unavailable for route/capacity",
			}
			out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed = collectUsage(children)
			out.MultiProviderOK = len(out.ProvidersUsed) >= 2
			out.RouteExcludes = routeExcludes // keep measured excludes for canary unavailable_retry
			// Release any holds we already took.
			if ledger != nil {
				for id, h := range holds {
					if _, err := ledger.Release(h.projectID, h.runID, h.attemptID, "sibling_unavailable"); err == nil {
						for i := range out.Children {
							if out.Children[i].ChildID == id {
								out.Children[i].CapacityState = "released"
								out.Children[i].CapacityNote += "; released=sibling_unavailable"
							}
						}
					}
				}
			}
			return out, fmt.Errorf("goalrun: child unavailable")
		}
	}

	wfProv, wfModel := pinProv, pinModel
	for _, c := range children {
		if c.Provider != "" && !c.Unavailable {
			wfProv, wfModel = c.Provider, c.Model
			break
		}
	}
	exec := req.Executor
	if exec == nil {
		exec = workflowrun.ProductionChildExecutor{HomeDir: req.HomeDir, Now: nowFn}
	}
	svc := workflowrun.Service{Now: nowFn, Executor: exec, HomeDir: req.HomeDir}
	goalBranch := strings.TrimSpace(req.GoalBranch)
	if goalBranch == "" {
		goalBranch = "loopcoder/goal-" + runID
	}
	// Capacity bridge: on model_unavailable alternate, release prior reservation
	// and reserve the new attempt (distinct durable attempt id). Never double-hold.
	var capHook workflowrun.CapacityRerouteHook
	if ledger != nil && snap != nil {
		capHook = &goalCapacityReroute{
			ledger: ledger, snap: snap, projectID: projectID, runID: runID, holds: holds,
		}
	}
	wres, werr := svc.Execute(ctx, workflowrun.Request{
		ProjectID:           projectID,
		RunID:               runID,
		ExpectedPlanDigest:  execPlanDigest,
		ExpectedGraphDigest: graphDigest,
		Definition:          def,
		Actor:               actor,
		Provider:            firstNonEmpty(wfProv, "auto"),
		Model:               firstNonEmpty(wfModel, "auto"),
		ChildRoutes:         childRoutes,
		SameDepthAlternates: sameDepthAlts,
		CapacityReroute:     capHook,
		RepoPath:            req.RepoPath,
		BaseRef:             firstNonEmpty(req.PRBaseRef, "main"),
		GoalBranch:          goalBranch,
		PriorSucceeded:      priorSucceeded,
		// Full validated attempt set from resume preflight (before inventory/spend).
		// Service seeds out.Children so writePartialPrior persists complete history
		// without reading on-disk partial.
		PriorOutcomes:     priorOutcomes,
		AttemptGeneration: attemptGen,
	})
	out := Result{
		GraphID: g.GraphID, PlanDigest: execPlanDigest, GraphDigest: graphDigest,
		RunID: runID, ProjectID: projectID,
		Children: children, Workflow: wres, Resumed: resumed,
		InventoryReportDigest: inventoryReportDigest,
		ClaudeCatalogReceipts: append([]providerinventory.ClaudeCapabilityProbeReceipt(nil), snap.ClaudeCatalogReceipts...),
	}
	out.ReuseCount = wres.ReuseCount
	out.WorktreePeak = wres.WorktreePeak
	out.ProcessPeak = wres.ProcessPeak
	// Durable model_unavailable → reroute event refs into route excludes
	// (current-pass outcomes only; resume merge rebinds below).
	_ = recordModelUnavailableExcludes(wres, recordExclude)
	out.RouteExcludes = routeExcludes

	// Reconcile capacity from real child outcomes — never fabricate actual.
	// Then attach post-run remaining from a fresh capacity observation when possible
	// so after is never left n/a solely because token actual is unknown.
	var postSnap *capacitysnapshot.Snapshot
	if wantAuto && loadInv != nil {
		if _, s, lerr := loadInv(ctx, req.RepoPath, nowFn().UTC()); lerr == nil {
			postSnap = &s
		}
	}
	// Lifecycle/capacity bind uses current runtime plan/graph identity only
	// (execPlanDigest, graphDigest, g.GraphID, g.Version) — never git/archive SHA.
	// On resume, merge every validated durable WorkflowKid (goal-checkpoint and/or
	// workflow-partial) by AttemptID before projection so older aborted/failed/MU
	// rows survive multi-restart — including partial-only (no goal-checkpoint).
	var cpForMerge *Checkpoint
	if resumed && hasCP {
		cpForMerge = &loadedCP
	}
	var partForMerge *workflowrun.PartialCheckpoint
	if resumed && hasPart {
		partForMerge = &loadedPart
	}
	merged, wresMerged, merr := applyChildOutcomes(
		out.Children, wres, ledger, holds, postSnap,
		wres.Interrupted && wantAuto,
		projectID, runID, execPlanDigest, graphDigest, g.GraphID, g.Version,
		cpForMerge, partForMerge,
	)
	if merr != nil {
		return out, fmt.Errorf("goalrun: apply child outcomes: %w", merr)
	}
	out.Children = merged
	// Production contract: Workflow.Children is the complete attempt set (current
	// pass + validated historical kids + AbortedAttempts projection).
	out.Workflow = wresMerged
	wres = wresMerged
	// Usage diversity is measured only after outcomes are applied (integrated /
	// terminal stages, ActualSources, argv). Planned-only or pre-spawn fail
	// rows must not count as multi-provider execution.
	// Failed MU rows never contribute (Terminal != succeeded).
	out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed = collectUsage(out.Children)
	out.MultiProviderOK = len(out.ProvidersUsed) >= 2
	out.MultiModelOrDepthOK = len(out.ModelsUsed) >= 2 || len(out.DepthsUsed) >= 2

	// Capacity transition seed: exact Role+AttemptID only. Prefer current-pass
	// wres transitions; on resume with empty wres, use envelope-validated
	// checkpoint seed. Always finalize from live ledger — never treat
	// checkpoint as final capacity truth.
	capacitySeed := identityCapacitySeed(wres.CapacityTransitions)
	if len(capacitySeed) < 2 && resumed && hasCP {
		capacitySeed = identityCapacitySeed(loadedCP.CapacityTransitions)
	}

	// After apply/release: finalize capacity from ledger for the exact seed IDs.
	// Mid-workflow "reserved" alternate is not final qualification truth.
	refreshLedgerCapacityTransitions := func(seed []workflowrun.CapacityTransition) error {
		if len(seed) < 2 {
			// No MU pair this pass — leave empty (do not invent).
			if len(out.Workflow.CapacityTransitions) > 0 && len(identityCapacitySeed(out.Workflow.CapacityTransitions)) < 2 {
				out.Workflow.CapacityTransitions = nil
			}
			return nil
		}
		// Prefer Role+AttemptID only so finalize re-reads ledger.
		seed = identityCapacitySeed(seed)
		fin := finalizeCapacityTransitions(ledger, projectID, runID, seed)
		if len(fin) != 2 {
			return fmt.Errorf("goalrun: capacity finalize failed for seed prior/alternate (want 2, got %d)", len(fin))
		}
		if err := validateLedgerMUCapacity(fin); err != nil {
			return err
		}
		out.Workflow.CapacityTransitions = fin
		return nil
	}
	if rerr := refreshLedgerCapacityTransitions(capacitySeed); rerr != nil {
		return out, rerr
	}

	// Resume: strict durable merge of MU failed+alternate from envelope-validated
	// checkpoint, cross-bound to JSONL event log. Fail closed on any mismatch.
	// Re-finalizes capacity from live ledger using the validated pair seed.
	if resumed && hasCP {
		if merr := mergeDurableModelUnavailableOnResume(
			&out, loadedCP, ledger, projectID, runID, execPlanDigest, graphDigest, g.GraphID, g.Version,
		); merr != nil {
			return out, merr
		}
		// Re-bind claimed excludes from merged workflow children only when not
		// already recorded for this pass (avoid duplicate claimed MU rows).
		hasClaimedMU := false
		for _, ex := range routeExcludes {
			if ex.Claimed && ex.Reason == "model_unavailable" {
				hasClaimedMU = true
				break
			}
		}
		if !hasClaimedMU {
			_ = recordModelUnavailableExcludes(out.Workflow, recordExclude)
		}
		out.RouteExcludes = routeExcludes
		// Universal ChildReport surface: retain MU failed rows after durable merge
		// (applyChildOutcomes ran before merge and only saw the winner).
		byItem := map[string][]workflowrun.ChildOutcome{}
		for _, c := range out.Workflow.Children {
			byItem[c.WorkItemID] = append(byItem[c.WorkItemID], c)
		}
		chosen := map[string]workflowrun.ChildOutcome{}
		for id, outs := range byItem {
			latest, err := selectLatestValidatedOutcome(id, outs)
			if err != nil {
				return out, fmt.Errorf("goalrun: resume durable outcome select %s: %w", id, err)
			}
			chosen[id] = latest
		}
		out.Children = appendMUFailedChildReports(out.Children, byItem, chosen)
		byAttempt := map[string]workflowrun.ChildOutcome{}
		for _, c := range out.Workflow.Children {
			// Byte-exact AttemptID keys — padded IDs never alias into the map.
			if c.AttemptID == "" || c.AttemptID != strings.TrimSpace(c.AttemptID) {
				if c.AttemptID != "" {
					return out, fmt.Errorf("goalrun: durable ChildOutcome attempt_id has whitespace padding %q (fail closed)", c.AttemptID)
				}
				continue
			}
			if prev, ok := byAttempt[c.AttemptID]; ok {
				if !reflect.DeepEqual(prev, c) {
					return out, fmt.Errorf("goalrun: conflicting durable ChildOutcome for AttemptID %s (fail closed)", c.AttemptID)
				}
				continue
			}
			byAttempt[c.AttemptID] = c
		}
		lifeID := lifecycleBindIdentity{
			ProjectID: projectID, RunID: runID,
			PlanDigest: execPlanDigest, GraphDigest: graphDigest,
			GraphID: g.GraphID, GraphVersion: g.Version,
		}
		var projErr error
		out.Children, out.Workflow, byAttempt, projErr = projectMissingAttemptRows(out.Children, out.Workflow, byAttempt, lifeID)
		if projErr != nil {
			return out, projErr
		}
		// Bind attempt-keyed ledger capacity + event lifecycle for dual MU rows
		// using explicit project/run and current plan/graph identity (never git SHA).
		if ledger != nil {
			if perr := populateCapacityFromLedgerByAttempt(out.Children, ledger, projectID, runID, execPlanDigest, graphDigest); perr != nil {
				return out, perr
			}
		}
		intItems := map[string]bool{}
		for _, id := range out.Workflow.Integrated {
			// Byte-exact work_item membership — padded IDs never alias.
			if id != "" && id != strings.TrimSpace(id) {
				return out, fmt.Errorf("goalrun: Integrated work_item_id has whitespace padding %q (fail closed)", id)
			}
			if id != "" {
				intItems[id] = true
			}
		}
		if berr := bindAttemptLifecycleEvidence(out.Children, out.Workflow, byAttempt, intItems, lifeID); berr != nil {
			return out, berr
		}
		for i := range out.Children {
			promoteIntegratedStage(&out.Children[i], intItems)
		}
		sortChildReportsByAttempt(out.Children)
		if uerr := requireUniqueAttemptIDs(out.Children); uerr != nil {
			return out, uerr
		}
		// Capacity already finalized inside merge when a pair exists; if merge
		// found no pair, keep existing seed finalize from above.
	}
	out.CapacityLedgerEntries = capacityLedgerEvidenceForResult(
		ledger, projectID, runID, out.Children, out.Workflow.Children,
	)

	// Always persist durable checkpoint (partial or complete) for forced restart.
	// Fail closed: no restart proof or success claim without a durable checkpoint.
	cpPath, cperr := saveRunCheckpoint(req.HomeDir, projectID, runID, req.Goal, req.Issue, actor, g.GraphID, execPlanDigest, graphDigest, out, wres, nowFn().UTC())
	if cperr != nil {
		return out, fmt.Errorf("goalrun: durable checkpoint save failed (no restart proof): %w", cperr)
	}
	if strings.TrimSpace(cpPath) == "" {
		return out, fmt.Errorf("goalrun: durable checkpoint path empty after save")
	}
	out.CheckpointPath = cpPath

	if werr != nil {
		out.Status = "blocked"
		out.Message = werr.Error()
		if wres.Error != "" {
			out.Message = wres.Error
		}
		// Failure path: release remaining holds (AttemptID-keyed only).
		if ledger != nil {
			var releaseErrs []string
			for att, h := range holds {
				// Skip already reconciled/released in applyChildOutcomes.
				if e, ok := ledger.Get(h.projectID, h.runID, h.attemptID); ok {
					if e.State == "reconciled" || e.State == "released" {
						continue
					}
				}
				// A succeeded child with an exact service-forced interrupt
				// elsewhere in the same run must retain its reservation for
				// resume-time after observation. Releasing here would erase
				// the only arithmetic bridge between before and after.
				if wres.Interrupted && wantAuto && succeededAttempt(out.Children, att) {
					continue
				}
				rel, err := ledger.Release(h.projectID, h.runID, h.attemptID, "child_failed_or_cancelled")
				if err != nil {
					releaseErrs = append(releaseErrs, att+":"+err.Error())
					continue
				}
				for i := range out.Children {
					if idExact(out.Children[i].AttemptID, att) && out.Children[i].CapacityState != "reconciled" {
						out.Children[i].CapacityState = rel.State
						out.Children[i].CapacityNote += "; released=child_failed_or_cancelled"
					}
				}
			}
			if len(releaseErrs) > 0 {
				out.Message += "; capacity_release_failed=" + strings.Join(releaseErrs, ",")
				for _, cr := range out.Children {
					emitChild(req.ReportOut, cr)
				}
				return out, fmt.Errorf("%w; capacity release failed: %s", werr, strings.Join(releaseErrs, "; "))
			}
		}
		// Re-finalize after releases using preserved identity seed (not empty wres).
		seedAfter := capacitySeed
		if len(seedAfter) < 2 {
			seedAfter = identityCapacitySeed(out.Workflow.CapacityTransitions)
		}
		if rerr := refreshLedgerCapacityTransitions(seedAfter); rerr != nil {
			// Prefer original interrupt/block error; surface capacity as blocked note.
			out.Message += "; capacity_finalize=" + rerr.Error()
			for _, cr := range out.Children {
				emitChild(req.ReportOut, cr)
			}
			return out, fmt.Errorf("%w; %v", werr, rerr)
		}
		// Re-save checkpoint after release notes + final ledger transitions (fail closed).
		p, perr := saveRunCheckpoint(req.HomeDir, projectID, runID, req.Goal, req.Issue, actor, g.GraphID, execPlanDigest, graphDigest, out, wres, nowFn().UTC())
		if perr != nil {
			return out, fmt.Errorf("%w; checkpoint re-save after release: %v", werr, perr)
		}
		if strings.TrimSpace(p) == "" {
			return out, fmt.Errorf("%w; checkpoint re-save path empty", werr)
		}
		out.CheckpointPath = p
		for _, cr := range out.Children {
			emitChild(req.ReportOut, cr)
		}
		return out, werr
	}
	out.Status = wres.Status
	out.Message = wres.Message
	out.HumanSummary = fmt.Sprintf(
		"goal graph %s digest=%s children=%d status=%s providers=%v models=%v depths=%v multi_provider=%v reuse=%d wt_peak=%d proc_peak=%d resumed=%v",
		g.GraphID, execPlanDigest, len(g.Items), wres.Status, out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed, out.MultiProviderOK,
		out.ReuseCount, out.WorktreePeak, out.ProcessPeak, out.Resumed,
	)

	// Real PR human merge gate: LoopCoder creates branch/commit/push/PR itself.
	if req.OpenPR && wres.Status == workflowrun.StatusHumanGate {
		// Structured independent verifier only — ignore Request pins/prose entirely.
		bindingEvents, bindEventsErr := loadOpenPRBindingEvents(wres.EventLogPath, projectID, runID)
		if bindEventsErr != nil {
			out.Status = "blocked"
			out.Message = firstNonEmpty(out.Message, "") + "; open_pr_event_binding_invalid"
			for _, cr := range out.Children {
				emitChild(req.ReportOut, cr)
			}
			return out, fmt.Errorf("goalrun: open pr: raw event binding: %w", bindEventsErr)
		}
		ind, verEv, bindOK := bindOpenPRVerifierFromChildren(wres.Children, bindingEvents)
		if !bindOK {
			out.Status = "blocked"
			out.Message = firstNonEmpty(out.Message, "") + "; open_pr_verifier_binding_invalid"
			for _, cr := range out.Children {
				emitChild(req.ReportOut, cr)
			}
			return out, fmt.Errorf("goalrun: open pr: structured wi_verify binding required")
		}
		prOpen := req.GoalPR
		if prOpen == nil {
			prOpen = goalpr.Open
		}
		issueN := 0
		if n, err := parseIssueNumber(req.Issue); err == nil {
			issueN = n
		}
		inst := true
		wait := req.WaitPRChecks
		checkWait := req.PRCheckWait
		if wait && checkWait <= 0 {
			checkWait = 15 * time.Minute
		}
		prRes, perr := prOpen(ctx, goalpr.Request{
			RepoPath: req.RepoPath, BaseRef: firstNonEmpty(req.PRBaseRef, "main"),
			// Head is the integrate goal branch (product commits + receipt).
			Branch:    firstNonEmpty(wres.GoalBranch, goalBranch),
			ProjectID: projectID, RunID: runID, GraphID: g.GraphID, PlanDigest: execPlanDigest,
			SourceIssue: issueN, Actor: actor, Children: productOnlyChildOutcomes(wres.Children),
			IndependentVerifier: ind, VerifierEvidence: verEv,
			RequiredCheckNames:  req.RequiredCheckNames,
			InstallMeaningfulCI: &inst,
			WaitForChecks:       wait,
			CheckWait:           checkWait,
			FinalizeAfterOpen:   wait,
			Now:                 nowFn,
		})
		out.PR = &prRes
		if perr != nil {
			out.Message = firstNonEmpty(out.Message, "") + "; pr_open_error=" + perr.Error()
			out.Status = "blocked"
			for _, cr := range out.Children {
				emitChild(req.ReportOut, cr)
			}
			// Still try canary emit for partial evidence when configured.
			if req.CanaryEmit != nil {
				opts := *req.CanaryEmit
				if opts.HomeDir == "" {
					opts.HomeDir = req.HomeDir
				}
				if strings.TrimSpace(opts.RepoPath) == "" {
					opts.RepoPath = req.RepoPath
				}
				if _, eerr := EmitCanaryFromResult(out, opts); eerr == nil {
					out.CanaryEvidencePath = opts.OutPath
				}
			}
			return out, fmt.Errorf("goalrun: open pr: %w", perr)
		}
		out.HumanSummary += fmt.Sprintf(" pr=%s human_gate=true auto_merge=false checks_green=%v", prRes.URL, prRes.RequiredChecksGreen)
	}

	// Exact-binary canary evidence emission (derived from events/PR/children).
	if req.CanaryEmit != nil {
		out.CapacityLedgerEntries = capacityLedgerEvidenceForResult(
			ledger, projectID, runID, out.Children, out.Workflow.Children,
		)
		opts := *req.CanaryEmit
		if opts.HomeDir == "" {
			opts.HomeDir = req.HomeDir
		}
		// Wire repo path for .loopcoder measurement (never leave empty when known).
		if strings.TrimSpace(opts.RepoPath) == "" {
			opts.RepoPath = req.RepoPath
		}
		if opts.InventoryProvenance == "" {
			opts.InventoryProvenance = req.InventoryProvenance
		}
		// Exact-artifact canary emission always requires the production live
		// discovery path. Unspecified is not evidence of live discovery.
		opts.RequireLiveInventory = true
		if req.InventoryProvenance != InventoryProvenanceLiveDiscover {
			return out, fmt.Errorf("goalrun: canary emit rejects inventory provenance %q", req.InventoryProvenance)
		}
		if _, eerr := EmitCanaryFromResult(out, opts); eerr != nil {
			out.Message += "; canary_emit_error=" + eerr.Error()
			// Fail closed when canary emit explicitly requested.
			for _, cr := range out.Children {
				emitChild(req.ReportOut, cr)
			}
			return out, fmt.Errorf("goalrun: canary emit: %w", eerr)
		}
		out.CanaryEvidencePath = opts.OutPath
		out.HumanSummary += " canary_evidence=" + opts.OutPath
	}

	for _, cr := range out.Children {
		emitChild(req.ReportOut, cr)
	}
	return out, nil
}

func capacityLedgerEvidenceForResult(
	ledger *capacityledger.Ledger,
	projectID, runID string,
	reports []ChildReport,
	outcomes []workflowrun.ChildOutcome,
) []capacityledger.Entry {
	if ledger == nil {
		return nil
	}
	seen := map[string]bool{}
	var attempts []string
	add := func(att string) {
		if att == "" || att != strings.TrimSpace(att) || seen[att] {
			return
		}
		seen[att] = true
		attempts = append(attempts, att)
	}
	for _, c := range reports {
		add(c.AttemptID)
	}
	for _, c := range outcomes {
		add(c.AttemptID)
	}
	sort.Strings(attempts)
	out := make([]capacityledger.Entry, 0, len(attempts))
	for _, att := range attempts {
		if entry, ok := ledger.Get(projectID, runID, att); ok {
			out = append(out, entry)
		}
	}
	return out
}

func parseIssueNumber(s string) (int, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func saveRunCheckpoint(homeDir, projectID, runID, goal, issue, actor, graphID, planDigest, graphDigest string, out Result, wres workflowrun.Result, at time.Time) (string, error) {
	// Prefer finalized workflow on out (CapacityTransitions refreshed from ledger).
	// Fall back to local wres only for fields not merged onto out.Workflow.
	wf := out.Workflow
	if wf.RunID == "" && wres.RunID != "" {
		wf = wres
		// Preserve any CapacityTransitions already written onto out.Workflow.
		if len(out.Workflow.CapacityTransitions) > 0 {
			wf.CapacityTransitions = out.Workflow.CapacityTransitions
		}
	}
	// Always take final capacity transitions from out.Workflow when present.
	if len(out.Workflow.CapacityTransitions) > 0 {
		wf.CapacityTransitions = out.Workflow.CapacityTransitions
	}
	// Prefer out.Workflow children when populated (includes alternate outcomes).
	if len(out.Workflow.Children) > 0 {
		wf.Children = out.Workflow.Children
	} else if len(wres.Children) > 0 {
		wf.Children = wres.Children
	}
	if wf.EventLogPath == "" {
		wf.EventLogPath = wres.EventLogPath
	}
	if !wf.Interrupted {
		wf.Interrupted = wres.Interrupted
	}
	if wf.AbortedAttempts == nil {
		wf.AbortedAttempts = wres.AbortedAttempts
	}
	gens := map[string]int{}
	aborted := wf.AbortedAttempts
	if aborted == nil {
		aborted = wres.AbortedAttempts
	}
	for id := range aborted {
		// Persist generation so next resume can bump again if re-interrupted.
		gens[id] = 0
		// Parse generation from aborted attempt id ...-gN
		if att := aborted[id]; strings.Contains(att, "-g") {
			if i := strings.LastIndex(att, "-g"); i >= 0 {
				var n int
				if _, err := fmt.Sscanf(att[i+2:], "%d", &n); err == nil {
					gens[id] = n
				}
			}
		}
	}
	claimCount := wf.ClaimCount
	if claimCount == 0 {
		claimCount = wres.ClaimCount
	}
	launchCount := wf.LaunchCount
	if launchCount == 0 {
		launchCount = wres.LaunchCount
	}
	cp := Checkpoint{
		Schema: CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDigest, GraphDigest: graphDigest,
		Goal: goal, Issue: issue, Actor: actor,
		Status:   firstNonEmpty(out.Status, wf.Status, wres.Status),
		Message:  firstNonEmpty(out.Message, wf.Message, wres.Message),
		Children: out.Children, WorkflowKids: wf.Children,
		WorktreePeak: firstNonZero(out.WorktreePeak, wf.WorktreePeak, wres.WorktreePeak),
		ProcessPeak:  firstNonZero(out.ProcessPeak, wf.ProcessPeak, wres.ProcessPeak),
		ReuseCount:   firstNonZero(out.ReuseCount, wf.ReuseCount, wres.ReuseCount),
		ClaimCount:   claimCount, LaunchCount: launchCount,
		SavedAt: at, Interrupted: wf.Interrupted, AbortedAttempts: aborted,
		AttemptGeneration: gens, EventLogPath: wf.EventLogPath,
		// Final ledger-backed transitions (not mid-workflow reserved snapshot).
		CapacityTransitions: append([]workflowrun.CapacityTransition(nil), wf.CapacityTransitions...),
	}
	return SaveCheckpoint(homeDir, cp)
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func osIsNotExist(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) || os.IsNotExist(err))
}

// applyChildOutcomes merges workflow child terminal/evidence into reports and
// reconciles capacity when actual is known; otherwise releases with honest unknown.
// Fail closed on contradictory assignment identity (TaskClass/plan/contract/generation).
// Universal MU dual rows are attempt-keyed: holds map is AttemptID→hold; capacity
// and lifecycle events never fall back to ChildID.
//
// Returns the updated ChildReport list AND the workflow Result with the complete
// AttemptID set (current pass + validated checkpoint WorkflowKids + AbortedAttempts
// projection). Callers MUST assign both onto out.Children and out.Workflow.
//
// planDigest/graphDigest/graphID/graphVersion are the current Execute runtime
// workgraph identities (never git/archive/pre-prod SHA).
// cp / part, when non-nil on resume, supply authoritative historical WorkflowKids
// (goal-checkpoint and workflow-partial are equal-rank durable attempt authorities).
func applyChildOutcomes(
	children []ChildReport,
	wres workflowrun.Result,
	ledger *capacityledger.Ledger,
	holds map[string]capacityHold,
	postSnap *capacitysnapshot.Snapshot,
	preserveSucceededUnknown bool,
	projectID, runID, planDigest, graphDigest, graphID string,
	graphVersion int,
	cp *Checkpoint,
	part *workflowrun.PartialCheckpoint,
) ([]ChildReport, workflowrun.Result, error) {
	id := lifecycleBindIdentity{
		ProjectID: projectID, RunID: runID,
		PlanDigest: planDigest, GraphDigest: graphDigest,
		GraphID: graphID, GraphVersion: graphVersion,
	}
	// Resume: merge every validated durable WorkflowKid by AttemptID first so
	// multi-restart history (g0 cancelled, g1 cancelled, …) is preserved before
	// selectLatest / projection. No event-alone invent. Checkpoint then partial
	// (exact-equal dedupe; conflicts fail closed).
	if cp != nil {
		var merr error
		wres, merr = mergeValidatedCheckpointWorkflowKids(wres, *cp, id)
		if merr != nil {
			return children, wres, merr
		}
	}
	if part != nil {
		var merr error
		wres, merr = mergeValidatedPartialWorkflowKids(wres, *part, id)
		if merr != nil {
			return children, wres, merr
		}
	}
	// Group outcomes by work item; validate full assignment identity family before pick.
	// Byte-exact AttemptID keys — padded IDs never alias.
	byItem := map[string][]workflowrun.ChildOutcome{}
	byAttempt := map[string]workflowrun.ChildOutcome{}
	for _, c := range wres.Children {
		byItem[c.WorkItemID] = append(byItem[c.WorkItemID], c)
		if c.AttemptID == "" {
			continue
		}
		if c.AttemptID != strings.TrimSpace(c.AttemptID) {
			return children, wres, fmt.Errorf("goalrun: ChildOutcome attempt_id has whitespace padding %q (fail closed)", c.AttemptID)
		}
		if prev, ok := byAttempt[c.AttemptID]; ok && !reflect.DeepEqual(prev, c) {
			return children, wres, fmt.Errorf("goalrun: conflicting ChildOutcome for AttemptID %s (fail closed)", c.AttemptID)
		}
		byAttempt[c.AttemptID] = c
	}
	byID := map[string]workflowrun.ChildOutcome{}
	for itemID, outs := range byItem {
		chosen, err := selectLatestValidatedOutcome(itemID, outs)
		if err != nil {
			return children, wres, err
		}
		byID[itemID] = chosen
	}
	// Authoritative work-item integrate membership (workflow Integrated list only).
	// Terminal success alone never implies Stage=integrated.
	integratedItems := map[string]bool{}
	for _, id := range wres.Integrated {
		if id != "" && id != strings.TrimSpace(id) {
			return children, wres, fmt.Errorf("goalrun: Integrated work_item_id has whitespace padding %q (fail closed)", id)
		}
		if id != "" {
			integratedItems[id] = true
		}
	}
	for i := range children {
		if children[i].Unavailable {
			continue
		}
		co, ok := byID[children[i].ChildID]
		if ok {
			// Assignment identity: fail closed on mismatch; never silent overwrite of contradictions.
			if err := mergeAssignmentIdentity(&children[i], co); err != nil {
				return children, wres, err
			}
			applyOutcomeRouteFields(&children[i], co)
		}
		// Stage assignment (pre-lifecycle): never set integrated from Terminal alone.
		// Final Stage=integrated is promoted only after integrate event/SHA binding below.
		if children[i].Terminal == string(workgraph.TermSucceeded) {
			// Pre-integrate success remains terminal until integrate binding confirms.
			if children[i].Stage == "" || children[i].Stage == "planned" || children[i].Stage == "routing" ||
				children[i].Stage == "integrated" {
				children[i].Stage = "terminal"
			}
			children[i].NextAction = "parent_continue"
		} else if wres.Status == workflowrun.StatusHumanGate &&
			!isExactMUFailureClass(children[i].FailureClass) &&
			children[i].Terminal == "" {
			children[i].Stage = "human_gate"
			children[i].NextAction = "owner_merge"
		} else if children[i].Terminal != "" {
			children[i].Stage = "terminal"
			if isExactMUFailureClass(children[i].FailureClass) {
				children[i].NextAction = "inspect_model_unavailable"
			} else {
				children[i].NextAction = "inspect_failure"
			}
		}
		// Resumed prior reuse: Stage may already be "resumed" — leave it.
	}

	// Universal report retention: keep typed model_unavailable failed routes as
	// separate ChildReport rows alongside the winning alternate (latest success).
	// Capacity for each row is applied AFTER append, keyed by AttemptID only.
	children = appendMUFailedChildReports(children, byItem, byID)

	// Assertion-only: every AbortedAttempts entry must already have a persisted
	// ChildOutcome (workflowrun interrupt boundary). Project report rows only.
	var perr error
	children, wres, _, perr = projectMissingAttemptRows(children, wres, byAttempt, id)
	if perr != nil {
		return children, wres, perr
	}
	// Rebuild byItem/byAttempt after projection; ensure every outcome has a report.
	byItem = map[string][]workflowrun.ChildOutcome{}
	byAttempt = map[string]workflowrun.ChildOutcome{}
	for _, c := range wres.Children {
		byItem[c.WorkItemID] = append(byItem[c.WorkItemID], c)
		if c.AttemptID != "" {
			byAttempt[c.AttemptID] = c
		}
	}
	children = ensureChildReportsForOutcomes(children, wres.Children)
	// MU dual rows again in case merge brought failed MU outcomes without reports.
	for itemID, outs := range byItem {
		latest, lerr := selectLatestValidatedOutcome(itemID, outs)
		if lerr != nil {
			return children, wres, lerr
		}
		byID[itemID] = latest
	}
	children = appendMUFailedChildReports(children, byItem, byID)

	// Capacity: group-reconcile only live holds (AttemptID keys). Released MU prior
	// holds are absent; their capacity is loaded from the ledger by AttemptID next.
	if ledger != nil && len(holds) > 0 {
		reconcileCapacityGroups(children, ledger, holds, postSnap, preserveSucceededUnknown)
	}
	// Authoritative per-attempt ledger population (explicit project/run; no hold inference).
	// Historical aborted rows may lack capacity holds — only bind capacity-bearing reports.
	if ledger != nil {
		if err := populateCapacityFromLedgerByAttempt(children, ledger, projectID, runID, planDigest, graphDigest); err != nil {
			return children, wres, err
		}
	}
	// Bind lifecycle event IDs + PID + integrate SHA from authoritative event log
	// cross-bound to current plan/graph identity + ChildOutcome stamps.
	if err := bindAttemptLifecycleEvidence(children, wres, byAttempt, integratedItems, id); err != nil {
		return children, wres, err
	}
	// Promote Stage=integrated only with Integrated membership + exact integrate binding.
	// Terminal success without integrate remains terminal/succeeded.
	for i := range children {
		if promoteIntegratedStage(&children[i], integratedItems) {
			continue
		}
		// human_gate status on parent does not rewrite failed MU or non-integrated success.
		if wres.Status == workflowrun.StatusHumanGate &&
			children[i].Terminal == string(workgraph.TermSucceeded) &&
			children[i].Stage != "integrated" && children[i].Stage != "resumed" {
			// Parent at human_gate; child succeeded but not product-integrated stays terminal.
			children[i].Stage = "terminal"
			children[i].NextAction = "parent_continue"
		}
	}
	// Deterministic universal order: ChildID then AttemptID (unique per row).
	sortChildReportsByAttempt(children)
	if err := requireUniqueAttemptIDs(children); err != nil {
		return children, wres, err
	}
	return children, wres, nil
}

// promoteIntegratedStage sets Stage=integrated only when the work item is on the
// authoritative Integrated list AND this attempt has exact integrate event + commit SHA.
// Returns true when promoted. MU failed / non-succeeded never promote.
func promoteIntegratedStage(cr *ChildReport, integratedItems map[string]bool) bool {
	if cr == nil {
		return false
	}
	if isExactMUFailureClass(cr.FailureClass) {
		// Failed MU route must never claim product integrate stage.
		if cr.Stage == "integrated" {
			cr.Stage = "terminal"
		}
		return false
	}
	if cr.Terminal != string(workgraph.TermSucceeded) {
		return false
	}
	if cr.ChildID == "" || cr.ChildID != strings.TrimSpace(cr.ChildID) {
		return false
	}
	if !integratedItems[cr.ChildID] {
		return false
	}
	if cr.IntegrateEventID == "" || cr.IntegrateCommitSHA == "" {
		return false
	}
	if cr.IntegrateEventID != strings.TrimSpace(cr.IntegrateEventID) ||
		cr.IntegrateCommitSHA != strings.TrimSpace(cr.IntegrateCommitSHA) {
		return false
	}
	cr.Stage = "integrated"
	cr.NextAction = "parent_continue"
	return true
}

// applyOutcomeRouteFields copies authoritative route/terminal identity from a
// workflow ChildOutcome onto a ChildReport (attempt-scoped; no capacity invent).
func applyOutcomeRouteFields(cr *ChildReport, co workflowrun.ChildOutcome) {
	if cr == nil {
		return
	}
	if co.Provider != "" {
		cr.Provider = co.Provider
	}
	if co.Model != "" {
		cr.Model = co.Model
	}
	if co.Depth != "" {
		cr.Depth = co.Depth
	}
	if co.AccountRef != "" {
		cr.AccountRef = co.AccountRef
	}
	if co.RouteReason != "" {
		cr.RouteReason = co.RouteReason
	}
	cr.Terminal = co.Terminal
	cr.AttemptID = co.AttemptID
	cr.OutputEvidence = co.OutputEvidence
	cr.FilesTouched = workflowrun.ProductFilesOnly(co.FilesTouched)
	cr.WorktreePath = co.WorktreePath
	cr.ActualSource = co.ActualSource
	cr.ActualSources = co.ActualSources
	cr.ArgvDigest = co.ArgvDigest
	cr.TestValidationStatus = co.TestValidationStatus
	cr.TestValidationEvidence = co.TestValidationEvidence
	cr.TestValidationCommandDigest = co.TestValidationCommandDigest
	cr.TestValidationHeadSHA = co.TestValidationHeadSHA
	cr.TestValidationReceiptPath = co.TestValidationReceiptPath
	if co.InstallRef != "" {
		cr.InstallRef = co.InstallRef
	}
	if co.Permission != "" {
		cr.Permission = co.Permission
	}
	if co.WindowKind != "" {
		cr.WindowKind = co.WindowKind
	}
	if co.ReservationID != "" {
		cr.ReservationID = co.ReservationID
	}
	if co.FailureClass != "" {
		cr.FailureClass = co.FailureClass
	}
	if co.SupersedesAttemptID != "" {
		cr.SupersedesAttemptID = co.SupersedesAttemptID
	}
	if co.RerouteEventRef != "" {
		cr.RerouteEventRef = co.RerouteEventRef
	}
	if co.IntegrateCommitSHA != "" {
		cr.IntegrateCommitSHA = co.IntegrateCommitSHA
	}
	// Never copy co.ActualCapacity — it is a token soft-window proxy, not
	// quota-window fraction. Tokens go to dedicated fields only.
	if co.InputTokens > 0 {
		cr.InputTokens = co.InputTokens
	}
	if co.OutputTokens > 0 {
		cr.OutputTokens = co.OutputTokens
	}
	if cr.InputTokens > 0 || cr.OutputTokens > 0 {
		cr.TokenTotal = cr.InputTokens + cr.OutputTokens
	}
}

// appendMUFailedChildReports adds one ChildReport per model_unavailable outcome
// that is not the chosen latest for its work item. Template intent/owner come from
// the primary report when present; route/capacity identity come only from the
// failed outcome (never copied from the winner). Capacity fields are left empty
// until populateCapacityFromLedgerByAttempt binds the prior AttemptID.
func appendMUFailedChildReports(
	children []ChildReport,
	byItem map[string][]workflowrun.ChildOutcome,
	chosen map[string]workflowrun.ChildOutcome,
) []ChildReport {
	// Index existing attempt IDs to avoid duplicate appends on re-entry.
	seenAtt := map[string]bool{}
	template := map[string]ChildReport{}
	for _, cr := range children {
		if a := cr.AttemptID; a != "" && a == strings.TrimSpace(a) {
			seenAtt[a] = true
		}
		// Prefer succeeded/winner row as template for intent/owner only.
		if prev, ok := template[cr.ChildID]; !ok || (cr.Terminal == string(workgraph.TermSucceeded) && prev.Terminal != string(workgraph.TermSucceeded)) {
			template[cr.ChildID] = cr
		}
	}
	for id, outs := range byItem {
		win, hasWin := chosen[id]
		for _, co := range outs {
			if !isExactMUFailureClass(co.FailureClass) {
				continue
			}
			att := co.AttemptID
			if att == "" || att != strings.TrimSpace(att) || seenAtt[att] {
				continue
			}
			if hasWin && idExact(win.AttemptID, att) {
				continue // chosen row already represents this attempt
			}
			base := template[id]
			cr := ChildReport{
				ChildID: id, Intent: base.Intent, Owner: base.Owner,
				RouteRequirement: base.RouteRequirement,
				TaskClass:        co.TaskClass, ExecutionPlanDigest: co.ExecutionPlanDigest,
				ChildContractDigest: co.ChildContractDigest, Generation: co.Generation,
				Provider: co.Provider, Model: co.Model, Depth: co.Depth,
				Permission: co.Permission, AccountRef: co.AccountRef, InstallRef: co.InstallRef,
				WindowKind: co.WindowKind, ReservationID: co.ReservationID,
				RouteReason: co.RouteReason, Terminal: co.Terminal, AttemptID: att,
				OutputEvidence: co.OutputEvidence, WorktreePath: co.WorktreePath,
				FilesTouched: workflowrun.ProductFilesOnly(co.FilesTouched),
				FailureClass: co.FailureClass, Stage: "terminal", NextAction: "inspect_model_unavailable",
				ActualSource: co.ActualSource, ActualSources: co.ActualSources,
				ArgvDigest: co.ArgvDigest,
				// Failed MU must not carry winner integrate SHA or supersedes.
				IntegrateCommitSHA: "",
			}
			children = append(children, cr)
			seenAtt[att] = true
		}
	}
	return children
}

// populateCapacityFromLedgerByAttempt loads exact ledger identity + before/reserved/
// actual/after metadata for every capacity-bearing ChildReport. Requires explicit
// projectID/runID and current plan/graph digests (never inferred from holds).
// Empty ledger identity fields are fatal. Report permission is preserved and
// must be nonempty for capacity-bearing rows (ledger has no permission field).
func populateCapacityFromLedgerByAttempt(
	children []ChildReport,
	ledger *capacityledger.Ledger,
	projectID, runID, planDigest, graphDigest string,
) error {
	if ledger == nil {
		return nil
	}
	// Runtime identity must already be exact (caller materializes digests); reject padding.
	if err := requireExactDurableToken("project_id", projectID); err != nil {
		return fmt.Errorf("goalrun: capacity populate: %w", err)
	}
	if err := requireExactDurableToken("run_id", runID); err != nil {
		return fmt.Errorf("goalrun: capacity populate: %w", err)
	}
	if err := requireExactDurableToken("plan_digest", planDigest); err != nil {
		return fmt.Errorf("goalrun: capacity populate: %w", err)
	}
	if err := requireExactDurableToken("graph_digest", graphDigest); err != nil {
		return fmt.Errorf("goalrun: capacity populate: %w", err)
	}
	for i := range children {
		if !reportNeedsLedgerCapacity(children[i]) {
			continue
		}
		att := children[i].AttemptID
		if err := requireExactDurableToken("attempt_id", att); err != nil {
			return fmt.Errorf("goalrun: capacity-bearing child %s: %w", children[i].ChildID, err)
		}
		// Permission is route-contract identity, not on ledger Entry — require it
		// on the report and never invent from empty.
		if err := requireExactDurableToken("permission", children[i].Permission); err != nil {
			return fmt.Errorf("goalrun: attempt %s: %w", att, err)
		}
		ent, ok := ledger.Get(projectID, runID, att)
		if !ok {
			// Cancelled historical projections may lack capacity; other capacity-bearing
			// rows must have a durable entry.
			term := children[i].Terminal
			if term == string(workgraph.TermCancelled) {
				continue
			}
			return fmt.Errorf("goalrun: ledger missing attempt %s for %s/%s", att, projectID, runID)
		}
		if err := requireCompleteLedgerEntry(ent, projectID, runID, att, planDigest, graphDigest); err != nil {
			return err
		}
		if err := exactRouteIdentityMatch(&children[i], ent); err != nil {
			return err
		}
		// Cross-bind assignment identity from ledger to report when report set.
		if err := exactStringMatch("task_class", children[i].TaskClass, ent.TaskClass, att); err != nil {
			return err
		}
		if err := exactStringMatch("execution_plan_digest", children[i].ExecutionPlanDigest, ent.PlanDigest, att); err != nil {
			return err
		}
		if err := exactStringMatch("child_contract_digest", children[i].ChildContractDigest, ent.ChildContractDigest, att); err != nil {
			return err
		}
		// Populate only from this exact AttemptID ledger row (permission preserved).
		perm := children[i].Permission
		children[i].Provider = ent.Provider
		children[i].Model = ent.Model
		children[i].Depth = ent.Depth
		children[i].AccountRef = ent.AccountRef
		children[i].InstallRef = ent.InstallRef
		children[i].WindowKind = ent.WindowKind
		children[i].ReservationID = ent.ReservationID
		children[i].Permission = perm
		if ent.TaskClass != "" {
			children[i].TaskClass = ent.TaskClass
		}
		if ent.PlanDigest != "" {
			children[i].ExecutionPlanDigest = ent.PlanDigest
		}
		if ent.ChildContractDigest != "" {
			children[i].ChildContractDigest = ent.ChildContractDigest
		}
		applyCapacityBeforeFromEntry(&children[i], ent)
		applyCapacityAfterFromEntry(&children[i], ent)
		children[i].CapacityActual = ent.Actual
		children[i].ActualSource = ent.ActualSource
		children[i].CapacityActualConfidence = string(ent.ActualConfidence)
		children[i].CapacityState = ent.State
		if ent.ReleaseReason != "" && !strings.Contains(children[i].CapacityNote, "release_reason=") {
			children[i].CapacityNote = firstNonEmpty(children[i].CapacityNote, "ledger") +
				"; release_reason=" + ent.ReleaseReason
		}
		if children[i].CapacityBefore == nil || children[i].CapacityReserved == nil {
			return fmt.Errorf("goalrun: attempt %s missing structured before/reserved from ledger", att)
		}
		if children[i].CapacityBeforeSource == "" ||
			children[i].CapacityBeforeFreshness == "" ||
			children[i].CapacityBeforeConfidence == "" {
			return fmt.Errorf("goalrun: attempt %s missing before metadata (source/freshness/confidence)", att)
		}
	}
	return nil
}

// requireCompleteLedgerEntry fails closed when any required identity field is empty
// or does not match the current runtime project/run/attempt/plan/graph.
func requireCompleteLedgerEntry(ent capacityledger.Entry, projectID, runID, attemptID, planDigest, graphDigest string) error {
	// Persisted ledger identity is byte-exact vs runtime (no TrimSpace normalize).
	if ent.ProjectID == "" || ent.ProjectID != projectID {
		return fmt.Errorf("goalrun: ledger entry project_id %q != runtime %q", ent.ProjectID, projectID)
	}
	if ent.RunID == "" || ent.RunID != runID {
		return fmt.Errorf("goalrun: ledger entry run_id %q != runtime %q", ent.RunID, runID)
	}
	if ent.AttemptID == "" || ent.AttemptID != attemptID {
		return fmt.Errorf("goalrun: ledger entry attempt_id %q != report %q", ent.AttemptID, attemptID)
	}
	if ent.PlanDigest == "" || ent.PlanDigest != planDigest {
		return fmt.Errorf("goalrun: ledger entry plan_digest mismatch attempt=%s", attemptID)
	}
	if ent.GraphDigest == "" || ent.GraphDigest != graphDigest {
		return fmt.Errorf("goalrun: ledger entry graph_digest mismatch attempt=%s", attemptID)
	}
	for _, pair := range []struct{ name, v string }{
		{"task_class", ent.TaskClass},
		{"child_contract_digest", ent.ChildContractDigest},
		{"provider", ent.Provider},
		{"model", ent.Model},
		{"depth", ent.Depth},
		{"account_ref", ent.AccountRef},
		{"install_ref", ent.InstallRef},
		{"window_kind", ent.WindowKind},
		{"reservation_id", ent.ReservationID},
		{"state", ent.State},
		{"before_source", ent.BeforeSource},
		{"freshness", ent.Freshness},
		{"confidence", string(ent.Confidence)},
	} {
		if pair.v == "" {
			return fmt.Errorf("goalrun: ledger entry attempt %s missing %s", attemptID, pair.name)
		}
		if pair.v != strings.TrimSpace(pair.v) {
			return fmt.Errorf("goalrun: ledger entry attempt %s %s has whitespace padding", attemptID, pair.name)
		}
	}
	if ent.BeforeCapturedAt == nil || ent.BeforeCapturedAt.IsZero() {
		return fmt.Errorf("goalrun: ledger entry attempt %s missing before_captured_at", attemptID)
	}
	// Finite windows require reset identity; unbounded may omit.
	if ent.ResetAt == nil && ent.Freshness != "not_applicable" {
		wk := ent.WindowKind
		if wk != "" && wk != "unlimited" && wk != "unknown" && wk != "none" {
			return fmt.Errorf("goalrun: ledger entry attempt %s missing reset_at for window %s", attemptID, ent.WindowKind)
		}
	}
	return nil
}

func exactStringMatch(field, have, want, att string) error {
	return exactDurableFieldMatch(field, have, want, att)
}

// reportNeedsLedgerCapacity: historical aborted projections without capacity
// identity skip ledger. Launched/reserved attempts (including MU failed that
// reserved then failed) always bind when a ledger entry exists — see populate.
func reportNeedsLedgerCapacity(c ChildReport) bool {
	if c.AttemptID == "" || c.AttemptID != strings.TrimSpace(c.AttemptID) {
		return false
	}
	// Explicit capacity facts always require ledger.
	if c.ReservationID != "" || c.CapacityBefore != nil || c.CapacityState != "" {
		return true
	}
	// Projected historical interrupt/abort without reserve: skip ledger.
	if c.Terminal == string(workgraph.TermCancelled) {
		if c.ReservationID == "" && c.CapacityBefore == nil {
			return false
		}
	}
	if isExactMUFailureClass(c.FailureClass) {
		return true
	}
	if c.Terminal == string(workgraph.TermSucceeded) {
		return true
	}
	return false
}

// exactRouteIdentityMatch requires complete ledger identity (already checked) and
// fails when report has a nonempty field that conflicts with the ledger row.
// Route/depth/model/install/window/reservation are byte-exact. Account uses the
// single explicit CanonicalAccountRef association contract only.
func exactRouteIdentityMatch(cr *ChildReport, ent capacityledger.Entry) error {
	if cr == nil {
		return fmt.Errorf("goalrun: nil child report")
	}
	att := cr.AttemptID
	if err := requireExactDurableToken("attempt_id", att); err != nil {
		return err
	}
	check := func(field, have, want string) error {
		return exactDurableFieldMatch(field, have, want, att)
	}
	if err := check("provider", cr.Provider, ent.Provider); err != nil {
		return err
	}
	if err := check("model", cr.Model, ent.Model); err != nil {
		return err
	}
	if err := check("depth", cr.Depth, ent.Depth); err != nil {
		return err
	}
	if err := check("install_ref", cr.InstallRef, ent.InstallRef); err != nil {
		return err
	}
	if err := check("window_kind", cr.WindowKind, ent.WindowKind); err != nil {
		return err
	}
	if err := check("reservation_id", cr.ReservationID, ent.ReservationID); err != nil {
		return err
	}
	// Account: ledger must be nonempty exact; report when set must match via
	// explicit CanonicalAccountRef contract (not generic EqualFold/TrimSpace).
	wantAcc := ent.AccountRef
	if wantAcc == "" || wantAcc != strings.TrimSpace(wantAcc) {
		return fmt.Errorf("goalrun: attempt %s ledger account_ref empty or padded %q", att, wantAcc)
	}
	if a := cr.AccountRef; a != "" {
		if a != strings.TrimSpace(a) {
			return fmt.Errorf("goalrun: attempt %s report account_ref has whitespace padding", att)
		}
		if capacityledger.CanonicalAccountRef(a) != capacityledger.CanonicalAccountRef(wantAcc) && a != wantAcc {
			return fmt.Errorf("goalrun: attempt %s account_ref conflict report=%q ledger=%q", att, a, wantAcc)
		}
	}
	return nil
}

// lifecycleBindIdentity is the current Execute runtime workgraph identity.
// PlanDigest/GraphDigest/GraphID/GraphVersion are never git/archive/pre-prod SHAs.
type lifecycleBindIdentity struct {
	ProjectID    string
	RunID        string
	PlanDigest   string // execPlanDigest from workflowdef.Normalize
	GraphDigest  string // workgraph.DigestGraph
	GraphID      string
	GraphVersion int
}

// projectMissingAttemptRows is assertion-only: every AbortedAttempts AttemptID
// must already have an exact persisted ChildOutcome produced by workflowrun
// (interrupt boundary + writePartialPrior) before goalrun projection. goalrun may
// exact-assert and project a ChildReport from that already-validated row only —
// never invent or refresh a ChildOutcome from event lines.
//
// Overlapping byAttempt/wres.Children rows for the same AttemptID must be
// reflect.DeepEqual; duplicate conflicts inside wres.Children fail closed.
// Persisted rows must pass complete runtime identity (plan/graph/wi/att/gen/
// class/CCD/terminal/failure) before any report projection.
func projectMissingAttemptRows(
	children []ChildReport,
	wres workflowrun.Result,
	byAttempt map[string]workflowrun.ChildOutcome,
	id lifecycleBindIdentity,
) ([]ChildReport, workflowrun.Result, map[string]workflowrun.ChildOutcome, error) {
	if byAttempt == nil {
		byAttempt = map[string]workflowrun.ChildOutcome{}
	}
	// Validate all byAttempt entries for padding + DeepEqual against wres.Children.
	for att, co := range byAttempt {
		if att == "" || att != strings.TrimSpace(att) {
			return children, wres, byAttempt, fmt.Errorf("goalrun: project aborted attempts: byAttempt key padding %q", att)
		}
		if co.AttemptID != att {
			return children, wres, byAttempt, fmt.Errorf("goalrun: project aborted attempts: byAttempt key %q != outcome AttemptID %q", att, co.AttemptID)
		}
	}
	// Index wres.Children with DeepEqual conflict detection (no first-wins).
	for _, co := range wres.Children {
		if co.AttemptID == "" {
			continue
		}
		if co.AttemptID != strings.TrimSpace(co.AttemptID) {
			return children, wres, byAttempt, fmt.Errorf("goalrun: project aborted attempts: Children attempt_id padding %q", co.AttemptID)
		}
		if prev, ok := byAttempt[co.AttemptID]; ok {
			if !reflect.DeepEqual(prev, co) {
				return children, wres, byAttempt, fmt.Errorf(
					"goalrun: project aborted attempts: conflicting ChildOutcome for AttemptID %s (byAttempt vs Children; never invent)",
					co.AttemptID)
			}
			continue
		}
		byAttempt[co.AttemptID] = co
	}
	// Detect internal wres.Children duplicates with different structs.
	seenKids := map[string]workflowrun.ChildOutcome{}
	for _, co := range wres.Children {
		if co.AttemptID == "" {
			continue
		}
		if prev, ok := seenKids[co.AttemptID]; ok {
			if !reflect.DeepEqual(prev, co) {
				return children, wres, byAttempt, fmt.Errorf(
					"goalrun: project aborted attempts: duplicate conflicting Children for AttemptID %s",
					co.AttemptID)
			}
			continue
		}
		seenKids[co.AttemptID] = co
	}
	aborted := wres.AbortedAttempts
	if len(aborted) == 0 {
		return children, wres, byAttempt, nil
	}
	seenReport := map[string]bool{}
	for _, cr := range children {
		if cr.AttemptID == "" {
			continue
		}
		if cr.AttemptID != strings.TrimSpace(cr.AttemptID) {
			return children, wres, byAttempt, fmt.Errorf("goalrun: project aborted attempts: report attempt_id padding %q", cr.AttemptID)
		}
		seenReport[cr.AttemptID] = true
	}
	for wi, att := range aborted {
		if wi == "" || att == "" {
			return children, wres, byAttempt, fmt.Errorf("goalrun: project aborted attempts: empty AbortedAttempts entry wi=%q att=%q", wi, att)
		}
		if wi != strings.TrimSpace(wi) || att != strings.TrimSpace(att) {
			return children, wres, byAttempt, fmt.Errorf("goalrun: project aborted attempts: AbortedAttempts whitespace padding wi=%q att=%q (fail closed; never invent)", wi, att)
		}
		co, hasCO := byAttempt[att]
		if !hasCO {
			return children, wres, byAttempt, fmt.Errorf(
				"goalrun: AbortedAttempts[%s]=%s missing exact persisted ChildOutcome (workflowrun must write complete aborted row before partial/return; goalrun never invents from events)",
				wi, att)
		}
		if err := requirePersistedOutcomeRuntimeIdentity(co, id, wi, att); err != nil {
			return children, wres, byAttempt, fmt.Errorf("goalrun: project aborted attempts: %w", err)
		}
		// Report projection only — copy from already-validated outcome.
		if !seenReport[att] {
			cr := childReportFromOutcome(co)
			for _, existing := range children {
				if existing.ChildID == cr.ChildID {
					if cr.Intent == "" {
						cr.Intent = existing.Intent
					}
					if cr.Owner == "" {
						cr.Owner = existing.Owner
					}
					if cr.RouteRequirement == "" {
						cr.RouteRequirement = existing.RouteRequirement
					}
					break
				}
			}
			children = append(children, cr)
			seenReport[att] = true
		}
	}
	return children, wres, byAttempt, nil
}

func childReportFromOutcome(co workflowrun.ChildOutcome) ChildReport {
	stage := "terminal"
	next := "inspect_failure"
	if isExactMUFailureClass(co.FailureClass) {
		next = "inspect_model_unavailable"
	}
	if exactTypedAbortClass(co.FailureClass) ||
		co.Terminal == string(workgraph.TermCancelled) {
		next = "resume_or_inspect_interrupt"
	}
	return ChildReport{
		ChildID: co.WorkItemID, AttemptID: co.AttemptID, Generation: co.Generation,
		TaskClass: co.TaskClass, ExecutionPlanDigest: co.ExecutionPlanDigest,
		ChildContractDigest: co.ChildContractDigest,
		Provider:            co.Provider, Model: co.Model, Depth: co.Depth, Permission: co.Permission,
		AccountRef: co.AccountRef, InstallRef: co.InstallRef, WindowKind: co.WindowKind,
		ReservationID: co.ReservationID, RouteReason: co.RouteReason,
		Terminal: co.Terminal, FailureClass: co.FailureClass,
		OutputEvidence: co.OutputEvidence, FilesTouched: workflowrun.ProductFilesOnly(co.FilesTouched),
		TestValidationStatus:        co.TestValidationStatus,
		TestValidationEvidence:      co.TestValidationEvidence,
		TestValidationCommandDigest: co.TestValidationCommandDigest,
		TestValidationHeadSHA:       co.TestValidationHeadSHA,
		TestValidationReceiptPath:   co.TestValidationReceiptPath,
		WorktreePath:                co.WorktreePath,
		Stage:                       stage, NextAction: next,
		// Never integrate historical non-success projection.
		IntegrateCommitSHA: "",
	}
}

// bindAttemptLifecycleEvidence attaches claim/launch/pid/terminal/integrate event
// IDs and PID from the durable event log, fail-closed. Cross-binds every event to
// exact project/run + plan/graph identity + matching ChildOutcome work item /
// task class / CCD / generation. Never parses with empty expected project/run.
// integratedItems is the authoritative workflow Integrated set (work item IDs).
func bindAttemptLifecycleEvidence(
	children []ChildReport,
	wres workflowrun.Result,
	byAttempt map[string]workflowrun.ChildOutcome,
	integratedItems map[string]bool,
	id lifecycleBindIdentity,
) error {
	// Runtime identity must be exact durable tokens (reject padding; never normalize).
	if err := requireExactDurableToken("project_id", id.ProjectID); err != nil {
		return fmt.Errorf("goalrun: lifecycle bind: %w", err)
	}
	if err := requireExactDurableToken("run_id", id.RunID); err != nil {
		return fmt.Errorf("goalrun: lifecycle bind: %w", err)
	}
	if err := requireExactDurableToken("plan_digest", id.PlanDigest); err != nil {
		return fmt.Errorf("goalrun: lifecycle bind: %w", err)
	}
	if err := requireExactDurableToken("graph_digest", id.GraphDigest); err != nil {
		return fmt.Errorf("goalrun: lifecycle bind: %w", err)
	}
	if err := requireExactDurableToken("graph_id", id.GraphID); err != nil {
		return fmt.Errorf("goalrun: lifecycle bind: %w", err)
	}
	if id.GraphVersion <= 0 {
		return fmt.Errorf("goalrun: lifecycle bind requires graph_version>0; never git SHA")
	}

	// Align report integrate SHA from outcomes; MU failed must stay empty.
	for i := range children {
		att := children[i].AttemptID
		if att == "" {
			continue
		}
		if att != strings.TrimSpace(att) {
			return fmt.Errorf("goalrun: lifecycle bind report attempt_id padding %q", att)
		}
		if co, ok := byAttempt[att]; ok {
			if err := conflictIfSet(children[i].IntegrateCommitSHA, co.IntegrateCommitSHA, att, "integrate_commit_sha"); err != nil {
				return err
			}
			if co.IntegrateCommitSHA != "" {
				children[i].IntegrateCommitSHA = co.IntegrateCommitSHA
			}
			if co.RerouteEventRef != "" {
				if err := conflictIfSet(children[i].RerouteEventRef, co.RerouteEventRef, att, "reroute_event_ref"); err != nil {
					return err
				}
				children[i].RerouteEventRef = co.RerouteEventRef
			}
		}
		if isExactMUFailureClass(children[i].FailureClass) {
			if children[i].IntegrateCommitSHA != "" {
				return fmt.Errorf("goalrun: MU failed attempt %s must not carry integrate_commit_sha", att)
			}
			children[i].IntegrateCommitSHA = ""
		}
	}

	needsLifecycle := false
	for _, c := range children {
		if reportNeedsLifecycleEvidence(c) {
			needsLifecycle = true
			break
		}
	}
	if !needsLifecycle {
		return nil
	}

	path := wres.EventLogPath
	if err := requireExactEventLogPathStamp("lifecycle bind", path); err != nil {
		return fmt.Errorf("goalrun: EventLogPath required for launched/terminal lifecycle evidence: %w", err)
	}
	st, serr := os.Stat(path)
	if serr != nil {
		return fmt.Errorf("goalrun: event log stat: %w", serr)
	}
	if st.IsDir() || st.Size() == 0 {
		return fmt.Errorf("goalrun: event log path invalid or empty: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("goalrun: event log read: %w", err)
	}
	// Strict parse with exact expected project/run — never empty expect.
	evs, err := workflowrun.ParseEventJSONLStrict(string(raw), id.ProjectID, id.RunID)
	if err != nil {
		return fmt.Errorf("goalrun: event log parse: %w", err)
	}
	if len(evs) == 0 {
		return fmt.Errorf("goalrun: event log empty after strict parse")
	}

	// Index reports by AttemptID — every attempt-scoped event must bind here.
	// Byte-exact keys; padded AttemptIDs fail closed (never alias).
	byReportAtt := map[string]*ChildReport{}
	for i := range children {
		att := children[i].AttemptID
		if att == "" {
			continue
		}
		if att != strings.TrimSpace(att) {
			return fmt.Errorf("goalrun: ChildReport attempt_id has whitespace padding %q (fail closed)", att)
		}
		if _, dup := byReportAtt[att]; dup {
			return fmt.Errorf("goalrun: duplicate ChildReport AttemptID %q", att)
		}
		byReportAtt[att] = &children[i]
	}

	type life struct {
		claim, launch, pidEv, terminal, integrate, interrupt []string
		pids                                                 []int
		commits                                              []string
		muEvents, rerouteEvents                              []string
		workItem                                             string
		generation                                           int
		taskClass, ccd                                       string
	}
	byAtt := map[string]*life{}
	lifecycleKinds := map[string]bool{
		"claim": true, "launch": true, "pid": true, "terminal": true,
		"integrate": true, "model_unavailable": true, "reroute": true,
		"interrupt": true, "reuse": true, "accept": true,
	}
	for _, e := range evs {
		// Parent-only events never enter attempt binding.
		if workflowrun.IsParentOnlyEvent(e) {
			continue
		}
		att := e.AttemptID
		if att != "" && att != strings.TrimSpace(att) {
			return fmt.Errorf("goalrun: event attempt_id has whitespace padding %q (fail closed)", att)
		}
		// Exact kind at resume authority — no ToLower/TrimSpace normalize.
		kind := e.Kind
		if kind != "" && (kind != strings.TrimSpace(kind) || kind != strings.ToLower(kind)) {
			return fmt.Errorf("goalrun: noncanonical event kind %q (byte-exact lowercase required)", e.Kind)
		}
		if !lifecycleKinds[kind] {
			// Non-lifecycle attempt-scoped? still require known binding if attempt set.
			if att != "" {
				return fmt.Errorf("goalrun: unknown attempt-scoped event kind %q attempt %s", e.Kind, att)
			}
			continue
		}
		if att == "" {
			return fmt.Errorf("goalrun: lifecycle kind %q missing attempt_id (not parent-only)", e.Kind)
		}
		eid := e.EventID
		if eid == "" || eid != strings.TrimSpace(eid) {
			return fmt.Errorf("goalrun: event missing or padded event_id %q (kind=%s attempt=%s)", e.EventID, e.Kind, att)
		}
		// Every attempt-scoped lifecycle event must bind to known outcome + report.
		co, hasCO := byAttempt[att]
		if !hasCO {
			return fmt.Errorf("goalrun: event %s attempt %s has no matching ChildOutcome", eid, att)
		}
		cr, hasCR := byReportAtt[att]
		if !hasCR {
			return fmt.Errorf("goalrun: event %s attempt %s has no matching ChildReport", eid, att)
		}
		if err := eventMatchesRuntimeIdentity(e, id); err != nil {
			return fmt.Errorf("goalrun: event %s: %w", eid, err)
		}
		// Exact cross-bind WorkItemID/AttemptID/Generation/TaskClass/CCD to outcome+report.
		if err := exactEventOutcomeReportBind(e, co, cr); err != nil {
			return fmt.Errorf("goalrun: event %s: %w", eid, err)
		}
		L, ok := byAtt[att]
		if !ok {
			L = &life{
				workItem:   e.WorkItemID,
				generation: e.Generation,
				taskClass:  e.TaskClass,
				ccd:        e.ChildContractDigest,
			}
			byAtt[att] = L
		}
		if wi := e.WorkItemID; wi != "" {
			if wi != strings.TrimSpace(wi) {
				return fmt.Errorf("goalrun: attempt %s event work_item_id padding %q", att, wi)
			}
			if L.workItem != "" && L.workItem != wi {
				return fmt.Errorf("goalrun: attempt %s work_item conflict events %q vs %q", att, L.workItem, wi)
			}
			L.workItem = wi
		}
		switch kind {
		case "claim":
			L.claim = append(L.claim, eid)
		case "launch":
			L.launch = append(L.launch, eid)
		case "pid":
			L.pidEv = append(L.pidEv, eid)
			if e.PID > 0 {
				L.pids = append(L.pids, e.PID)
			}
		case "terminal":
			L.terminal = append(L.terminal, eid)
		case "integrate":
			L.integrate = append(L.integrate, eid)
			if sha := e.CommitSHA; sha != "" {
				if sha != strings.TrimSpace(sha) {
					return fmt.Errorf("goalrun: attempt %s integrate commit_sha padding", att)
				}
				L.commits = append(L.commits, sha)
			}
		case "model_unavailable":
			L.muEvents = append(L.muEvents, eid)
		case "reroute":
			L.rerouteEvents = append(L.rerouteEvents, eid)
		case "interrupt":
			L.interrupt = append(L.interrupt, eid)
		}
	}

	for i := range children {
		if !reportNeedsLifecycleEvidence(children[i]) {
			continue
		}
		att := children[i].AttemptID
		if att == "" || att != strings.TrimSpace(att) {
			return fmt.Errorf("goalrun: lifecycle child %s missing or padded AttemptID %q", children[i].ChildID, att)
		}
		L, ok := byAtt[att]
		if !ok {
			// Resumed reuse may only have reuse; still need prior claim/launch/pid/terminal from same log.
			return fmt.Errorf("goalrun: no lifecycle events for attempt %s", att)
		}
		// Work-item cross-bind (byte-exact).
		if L.workItem != "" && children[i].ChildID != "" && L.workItem != children[i].ChildID {
			return fmt.Errorf("goalrun: attempt %s work_item event=%q report=%q", att, L.workItem, children[i].ChildID)
		}
		co, hasCO := byAttempt[att]
		if hasCO {
			if err := outcomeEventIdentityMatch(co, children[i], id); err != nil {
				return err
			}
		}

		finalTerm := isFinalTerminalState(children[i].Terminal)
		muFailed := isExactMUFailureClass(children[i].FailureClass)
		cancelled := children[i].Terminal == string(workgraph.TermCancelled)
		// Real provider execution (accepted invocation / argv / stream) requires PID.
		// Structural FakeChildExecutor rows without that proof never green multi-provider.
		requiresPID := claimsRealProviderExecution(children[i], co, hasCO)
		// Product integrate required only when workflow Integrated contains this work
		// item AND this attempt is the succeeded winner (not MU failed).
		childID := children[i].ChildID
		if childID != "" && childID != strings.TrimSpace(childID) {
			return fmt.Errorf("goalrun: attempt %s ChildID padding %q", att, childID)
		}
		wantIntegrate := !muFailed &&
			children[i].Terminal == string(workgraph.TermSucceeded) &&
			integratedItems[childID]
		wantSHA := children[i].IntegrateCommitSHA
		if wantSHA != "" && wantSHA != strings.TrimSpace(wantSHA) {
			return fmt.Errorf("goalrun: attempt %s IntegrateCommitSHA padding", att)
		}
		if hasCO && co.IntegrateCommitSHA != "" {
			if co.IntegrateCommitSHA != strings.TrimSpace(co.IntegrateCommitSHA) {
				return fmt.Errorf("goalrun: attempt %s outcome IntegrateCommitSHA padding", att)
			}
			wantSHA = co.IntegrateCommitSHA
		}
		// Resumed prior reuse may already carry Stage=resumed with prior integrate.
		if children[i].Stage == "resumed" && wantSHA != "" {
			wantIntegrate = true
		}

		if finalTerm {
			// Cancelled/interrupted historical attempts: require launch + (terminal or interrupt).
			// Success/MU failed: exact claim/launch/terminal (claim optional for soft recovery).
			if cancelled {
				if len(L.launch) != 1 {
					return fmt.Errorf("goalrun: cancelled attempt %s wants exactly 1 launch got %d", att, len(L.launch))
				}
				if len(L.terminal) == 0 && len(L.interrupt) == 0 {
					return fmt.Errorf("goalrun: cancelled attempt %s missing terminal and interrupt", att)
				}
				if len(L.terminal) > 1 || len(L.interrupt) > 1 {
					return fmt.Errorf("goalrun: cancelled attempt %s duplicate terminal/interrupt", att)
				}
				if len(L.claim) > 1 {
					return fmt.Errorf("goalrun: cancelled attempt %s duplicate claim", att)
				}
				if len(L.claim) == 1 {
					if err := setEventFieldConflict(&children[i].ClaimEventID, L.claim[0], att, "claim_event_id"); err != nil {
						return err
					}
				}
				if err := setEventFieldConflict(&children[i].LaunchEventID, L.launch[0], att, "launch_event_id"); err != nil {
					return err
				}
				if len(L.terminal) == 1 {
					if err := setEventFieldConflict(&children[i].TerminalEventID, L.terminal[0], att, "terminal_event_id"); err != nil {
						return err
					}
				}
				// No integrate on cancelled historical rows.
				if len(L.integrate) != 0 {
					return fmt.Errorf("goalrun: cancelled attempt %s must have zero integrate", att)
				}
				// Skip success-path claim/launch/terminal exact-1 triple below.
				continue
			}
			// Exactly one claim, launch, terminal for final terminal proof (success/MU).
			if len(L.claim) != 1 || len(L.launch) != 1 || len(L.terminal) != 1 {
				return fmt.Errorf("goalrun: attempt %s final terminal wants exactly 1 claim/launch/terminal got claim=%d launch=%d terminal=%d",
					att, len(L.claim), len(L.launch), len(L.terminal))
			}
			if err := setEventFieldConflict(&children[i].ClaimEventID, L.claim[0], att, "claim_event_id"); err != nil {
				return err
			}
			if err := setEventFieldConflict(&children[i].LaunchEventID, L.launch[0], att, "launch_event_id"); err != nil {
				return err
			}
			if err := setEventFieldConflict(&children[i].TerminalEventID, L.terminal[0], att, "terminal_event_id"); err != nil {
				return err
			}
			// PID: real provider execution requires exactly one positive PID event.
			// Structural non-real rows must omit PID; ProviderPID>0 without event is fatal.
			if len(L.pidEv) > 1 {
				return fmt.Errorf("goalrun: attempt %s duplicate pid events (%d)", att, len(L.pidEv))
			}
			if requiresPID {
				if len(L.pidEv) != 1 || len(L.pids) != 1 || L.pids[0] <= 0 {
					return fmt.Errorf("goalrun: real provider attempt %s requires exactly one positive PID event (got pid_events=%d pids=%v)",
						att, len(L.pidEv), L.pids)
				}
				if err := setEventFieldConflict(&children[i].PIDEventID, L.pidEv[0], att, "pid_event_id"); err != nil {
					return err
				}
				if children[i].ProviderPID > 0 && children[i].ProviderPID != L.pids[0] {
					return fmt.Errorf("goalrun: attempt %s provider_pid conflict report=%d event=%d", att, children[i].ProviderPID, L.pids[0])
				}
				children[i].ProviderPID = L.pids[0]
			} else {
				if len(L.pidEv) == 1 {
					// Structural row must not carry durable pid if not claiming real execution;
					// if present, still bind but does not alone green multi-provider.
					if len(L.pids) == 1 && L.pids[0] > 0 {
						if err := setEventFieldConflict(&children[i].PIDEventID, L.pidEv[0], att, "pid_event_id"); err != nil {
							return err
						}
						children[i].ProviderPID = L.pids[0]
					}
				} else if children[i].ProviderPID > 0 {
					return fmt.Errorf("goalrun: attempt %s ProviderPID=%d without durable pid event", att, children[i].ProviderPID)
				}
			}

			if muFailed {
				if len(L.integrate) != 0 || len(L.commits) != 0 {
					return fmt.Errorf("goalrun: MU failed attempt %s must have ZERO integrate events/commits", att)
				}
				if children[i].IntegrateEventID != "" || children[i].IntegrateCommitSHA != "" {
					return fmt.Errorf("goalrun: MU failed attempt %s must not carry integrate fields", att)
				}
				if len(L.muEvents) != 1 {
					return fmt.Errorf("goalrun: MU failed attempt %s wants exactly 1 model_unavailable event got %d", att, len(L.muEvents))
				}
			} else if wantIntegrate {
				// Authoritative Integrated membership requires exact-once integrate binding.
				if len(L.integrate) != 1 {
					return fmt.Errorf("goalrun: Integrated work item attempt %s wants exactly 1 integrate event got %d", att, len(L.integrate))
				}
				if len(L.commits) != 1 {
					return fmt.Errorf("goalrun: Integrated work item attempt %s wants exactly 1 integrate commit got %d", att, len(L.commits))
				}
				if wantSHA != "" && wantSHA != L.commits[0] {
					return fmt.Errorf("goalrun: attempt %s integrate SHA mismatch outcome/report=%q event=%q", att, wantSHA, L.commits[0])
				}
				if err := setEventFieldConflict(&children[i].IntegrateEventID, L.integrate[0], att, "integrate_event_id"); err != nil {
					return err
				}
				if err := setEventFieldConflict(&children[i].IntegrateCommitSHA, L.commits[0], att, "integrate_commit_sha"); err != nil {
					return err
				}
			} else {
				// Terminal success without Integrated membership: no integrate green.
				// Structural fake/succeeded without integrate must not acquire integrate fields.
				if len(L.integrate) > 0 && wantSHA == "" && !requiresPID {
					// Tolerate structural path that somehow logged integrate without Integrated list
					// only when outcome SHA binds; otherwise refuse silent green.
					if !integratedItems[children[i].ChildID] {
						// Do not set Stage=integrated later; leave integrate unbound.
					}
				}
			}
		} else {
			// Partial interrupt / in-flight: may have claim/launch/pid without terminal.
			// Must never green final evidence (no integrate success).
			if len(L.integrate) > 0 {
				return fmt.Errorf("goalrun: non-terminal attempt %s must not green integrate events", att)
			}
			// If claim/launch present, still set without requiring exact counts of 1 terminal.
			if len(L.claim) > 1 || len(L.launch) > 1 {
				return fmt.Errorf("goalrun: attempt %s duplicate claim/launch in partial state", att)
			}
			if len(L.claim) == 1 {
				if err := setEventFieldConflict(&children[i].ClaimEventID, L.claim[0], att, "claim_event_id"); err != nil {
					return err
				}
			}
			if len(L.launch) == 1 {
				if err := setEventFieldConflict(&children[i].LaunchEventID, L.launch[0], att, "launch_event_id"); err != nil {
					return err
				}
			}
			if len(L.pidEv) == 1 {
				if err := setEventFieldConflict(&children[i].PIDEventID, L.pidEv[0], att, "pid_event_id"); err != nil {
					return err
				}
			}
			if len(L.pids) == 1 {
				children[i].ProviderPID = L.pids[0]
			}
		}
	}
	return nil
}

func reportNeedsLifecycleEvidence(c ChildReport) bool {
	if c.AttemptID == "" || c.AttemptID != strings.TrimSpace(c.AttemptID) {
		return false
	}
	if isFinalTerminalState(c.Terminal) {
		return true
	}
	if isExactMUFailureClass(c.FailureClass) {
		return true
	}
	// Launched stages (exact stage tokens).
	switch c.Stage {
	case "integrated", "terminal", "human_gate", "resumed":
		return true
	}
	// ProviderPID or reservation with non-unavailable terminal path.
	if c.ProviderPID > 0 || (c.ReservationID != "" && !c.Unavailable && c.Provider != "") {
		switch c.Stage {
		case "", "routing", "planned", "unavailable":
			return false
		default:
			return true
		}
	}
	return false
}

func isFinalTerminalState(term string) bool {
	// Exact durable terminals only — no TrimSpace/ToLower and no canceled/aborted aliases.
	return isExactFinalTerminal(term)
}

func eventMatchesRuntimeIdentity(e workflowrun.Event, id lifecycleBindIdentity) error {
	if e.ProjectID != id.ProjectID {
		return fmt.Errorf("project_id mismatch event=%q want=%q", e.ProjectID, id.ProjectID)
	}
	if e.RunID != id.RunID {
		return fmt.Errorf("run_id mismatch event=%q want=%q", e.RunID, id.RunID)
	}
	// Plan/graph digests and graph id/version are runtime workgraph stamps — not git SHA.
	if e.ExecutionPlanDigest == "" || e.ExecutionPlanDigest != id.PlanDigest {
		return fmt.Errorf("execution_plan_digest mismatch event=%q want=%q", e.ExecutionPlanDigest, id.PlanDigest)
	}
	if e.GraphDigest == "" || e.GraphDigest != id.GraphDigest {
		return fmt.Errorf("graph_digest mismatch event=%q want=%q", e.GraphDigest, id.GraphDigest)
	}
	if e.GraphID == "" || e.GraphID != id.GraphID {
		return fmt.Errorf("graph_id mismatch event=%q want=%q", e.GraphID, id.GraphID)
	}
	if e.GraphVersion != id.GraphVersion {
		return fmt.Errorf("graph_version mismatch event=%d want=%d", e.GraphVersion, id.GraphVersion)
	}
	return nil
}

func outcomeEventIdentityMatch(co workflowrun.ChildOutcome, cr ChildReport, id lifecycleBindIdentity) error {
	att := cr.AttemptID
	if att == "" || att != strings.TrimSpace(att) {
		return fmt.Errorf("goalrun: report attempt_id empty or padded %q", cr.AttemptID)
	}
	if co.ExecutionPlanDigest == "" || co.ExecutionPlanDigest != id.PlanDigest {
		return fmt.Errorf("goalrun: attempt %s outcome plan digest %q != runtime %q", att, co.ExecutionPlanDigest, id.PlanDigest)
	}
	if cr.ExecutionPlanDigest != "" && cr.ExecutionPlanDigest != id.PlanDigest {
		return fmt.Errorf("goalrun: attempt %s report plan digest %q != runtime %q", att, cr.ExecutionPlanDigest, id.PlanDigest)
	}
	if co.WorkItemID == "" || cr.ChildID == "" || co.WorkItemID != cr.ChildID {
		return fmt.Errorf("goalrun: attempt %s work_item outcome=%q report=%q", att, co.WorkItemID, cr.ChildID)
	}
	if co.TaskClass == "" || cr.TaskClass == "" || co.TaskClass != cr.TaskClass {
		return fmt.Errorf("goalrun: attempt %s task_class conflict outcome=%q report=%q", att, co.TaskClass, cr.TaskClass)
	}
	if co.ChildContractDigest == "" || cr.ChildContractDigest == "" || co.ChildContractDigest != cr.ChildContractDigest {
		return fmt.Errorf("goalrun: attempt %s CCD conflict", att)
	}
	if co.Generation <= 0 || cr.Generation <= 0 || co.Generation != cr.Generation {
		return fmt.Errorf("goalrun: attempt %s generation conflict outcome=%d report=%d", att, co.Generation, cr.Generation)
	}
	if co.AttemptID != att || cr.AttemptID != att {
		return fmt.Errorf("goalrun: attempt %s AttemptID mismatch outcome/report", att)
	}
	for _, f := range []struct {
		name       string
		outcomeVal string
		reportVal  string
	}{
		{"test_validation_status", co.TestValidationStatus, cr.TestValidationStatus},
		{"test_validation_evidence", co.TestValidationEvidence, cr.TestValidationEvidence},
		{"test_validation_command_digest", co.TestValidationCommandDigest, cr.TestValidationCommandDigest},
		{"test_validation_head_sha", co.TestValidationHeadSHA, cr.TestValidationHeadSHA},
		{"test_validation_receipt_path", co.TestValidationReceiptPath, cr.TestValidationReceiptPath},
	} {
		if f.outcomeVal != f.reportVal {
			return fmt.Errorf("goalrun: attempt %s %s conflict outcome=%q report=%q", att, f.name, f.outcomeVal, f.reportVal)
		}
	}
	return nil
}

// exactEventOutcomeReportBind requires exact WorkItemID/AttemptID/Generation/
// TaskClass/CCD equality across event, ChildOutcome, and ChildReport (byte-exact).
func exactEventOutcomeReportBind(e workflowrun.Event, co workflowrun.ChildOutcome, cr *ChildReport) error {
	if cr == nil {
		return fmt.Errorf("nil ChildReport")
	}
	att := e.AttemptID
	if att == "" || att != co.AttemptID || att != cr.AttemptID {
		return fmt.Errorf("attempt_id event=%q outcome=%q report=%q", e.AttemptID, co.AttemptID, cr.AttemptID)
	}
	if att != strings.TrimSpace(att) {
		return fmt.Errorf("attempt_id padding %q", att)
	}
	wi := e.WorkItemID
	if wi == "" || wi != co.WorkItemID || wi != cr.ChildID {
		return fmt.Errorf("work_item_id event=%q outcome=%q report=%q", e.WorkItemID, co.WorkItemID, cr.ChildID)
	}
	if e.Generation <= 0 || e.Generation != co.Generation || e.Generation != cr.Generation {
		return fmt.Errorf("generation event=%d outcome=%d report=%d", e.Generation, co.Generation, cr.Generation)
	}
	tc := e.TaskClass
	if tc == "" || tc != co.TaskClass || tc != cr.TaskClass {
		return fmt.Errorf("task_class event=%q outcome=%q report=%q", e.TaskClass, co.TaskClass, cr.TaskClass)
	}
	ccd := e.ChildContractDigest
	if ccd == "" || ccd != co.ChildContractDigest || ccd != cr.ChildContractDigest {
		return fmt.Errorf("child_contract_digest mismatch for attempt %s", att)
	}
	return nil
}

func conflictIfSet(have, want, att, field string) error {
	// Byte-exact when both set; reject padding; never TrimSpace-normalize.
	if have == "" || want == "" {
		return nil
	}
	if have != strings.TrimSpace(have) || want != strings.TrimSpace(want) {
		return fmt.Errorf("goalrun: attempt %s %s whitespace padding report=%q outcome=%q", att, field, have, want)
	}
	if have != want {
		return fmt.Errorf("goalrun: attempt %s %s conflict report=%q outcome=%q", att, field, have, want)
	}
	return nil
}

func setEventFieldConflict(dst *string, want, att, field string) error {
	if dst == nil {
		return fmt.Errorf("goalrun: nil dest for %s", field)
	}
	if want != "" && want != strings.TrimSpace(want) {
		return fmt.Errorf("goalrun: attempt %s %s event value has whitespace padding", att, field)
	}
	have := *dst
	if have != "" {
		if have != strings.TrimSpace(have) {
			return fmt.Errorf("goalrun: attempt %s %s report value has whitespace padding", att, field)
		}
		if have != want {
			return fmt.Errorf("goalrun: attempt %s %s conflict report=%q event=%q", att, field, have, want)
		}
	}
	*dst = want
	return nil
}

func sortChildReportsByAttempt(children []ChildReport) {
	sort.SliceStable(children, func(i, j int) bool {
		if children[i].ChildID != children[j].ChildID {
			return children[i].ChildID < children[j].ChildID
		}
		return children[i].AttemptID < children[j].AttemptID
	})
}

// requireUniqueAttemptIDs fails on duplicate AttemptIDs and on empty AttemptID for
// any actually routed/launched/terminal/capacity-bearing ChildReport.
func requireUniqueAttemptIDs(children []ChildReport) error {
	seen := map[string]string{}
	for _, c := range children {
		att := c.AttemptID
		if att == "" {
			if reportRequiresAttemptID(c) {
				return fmt.Errorf("goalrun: child %s stage=%s terminal=%s requires nonempty AttemptID",
					c.ChildID, c.Stage, c.Terminal)
			}
			continue
		}
		if att != strings.TrimSpace(att) {
			return fmt.Errorf("goalrun: child %s AttemptID has whitespace padding %q", c.ChildID, att)
		}
		if prev, ok := seen[att]; ok {
			return fmt.Errorf("goalrun: duplicate AttemptID %q on children %s and %s", att, prev, c.ChildID)
		}
		seen[att] = c.ChildID
	}
	return nil
}

func reportRequiresAttemptID(c ChildReport) bool {
	if isFinalTerminalState(c.Terminal) {
		return true
	}
	if isExactMUFailureClass(c.FailureClass) {
		return true
	}
	if c.ReservationID != "" || c.CapacityBefore != nil || c.CapacityState != "" {
		return true
	}
	if c.Provider != "" && !c.Unavailable {
		switch strings.TrimSpace(c.Stage) {
		case "routing", "planned", "unavailable", "":
			return false
		default:
			return true
		}
	}
	return false
}

// capacityGroupKey binds attempts that share the same quota window observation.
type capacityGroupKey struct {
	provider, account, install, window string
	beforeSrc                          string
	beforeCapUnix                      int64
}

type capacityGroupMember struct {
	idx      int
	childID  string
	hold     capacityHold
	entry    capacityledger.Entry
	tokens   int64
	reserved float64
	launched bool // actually executed (has terminal and was not merely planned)
	termOK   bool
}

// reconcileCapacityGroups observes one After per exact identity group and
// allocates the single aggregate Before−After delta across members.
func reconcileCapacityGroups(children []ChildReport, ledger *capacityledger.Ledger, holds map[string]capacityHold, postSnap *capacitysnapshot.Snapshot, preserveSucceededUnknown bool) {
	groups := map[capacityGroupKey][]capacityGroupMember{}
	var groupOrder []capacityGroupKey
	for i := range children {
		// Holds are AttemptID-keyed only. No WorkItemID/ChildID key lookup or fallback.
		att := strings.TrimSpace(children[i].AttemptID)
		if att == "" {
			continue
		}
		h, hasHold := holds[att]
		if !hasHold {
			continue
		}
		// Guard: hold attempt must equal map key and report attempt (never misattribute).
		if !idExact(h.attemptID, att) {
			children[i].CapacityNote += "; capacity_group=hold_attempt_mismatch"
			continue
		}
		ent, ok := ledger.Get(h.projectID, h.runID, h.attemptID)
		if !ok {
			children[i].CapacityNote += "; capacity_group=no_ledger_entry"
			continue
		}
		// Preserve before structured fields from reservation.
		applyCapacityBeforeFromEntry(&children[i], ent)
		beforeCapUnix := int64(0)
		if ent.BeforeCapturedAt != nil {
			beforeCapUnix = ent.BeforeCapturedAt.UTC().UnixNano()
		}
		gk := capacityGroupKey{
			provider:      strings.ToLower(strings.TrimSpace(children[i].Provider)),
			account:       capacityledger.CanonicalAccountRef(ent.AccountRef),
			install:       strings.TrimSpace(ent.InstallRef),
			window:        strings.TrimSpace(ent.WindowKind),
			beforeSrc:     strings.TrimSpace(ent.BeforeSource),
			beforeCapUnix: beforeCapUnix,
		}
		if gk.provider == "" || gk.account == "" || gk.install == "" || gk.window == "" {
			children[i].CapacityNote += "; capacity_group=incomplete_identity"
			// Fail closed: release without inventing actual.
			releaseCapacityUnknown(&children[i], ledger, h, "incomplete_capacity_identity")
			continue
		}
		if _, seen := groups[gk]; !seen {
			groupOrder = append(groupOrder, gk)
		}
		term := strings.TrimSpace(children[i].Terminal)
		launched := term != "" && !children[i].Unavailable
		groups[gk] = append(groups[gk], capacityGroupMember{
			idx: i, childID: children[i].ChildID, hold: h, entry: ent,
			tokens: children[i].TokenTotal, reserved: ent.Reserved,
			launched: launched, termOK: strings.EqualFold(term, "succeeded"),
		})
	}

	for _, gk := range groupOrder {
		members := groups[gk]
		if len(members) == 0 {
			continue
		}
		// Deterministic member order for auditable allocation (AttemptID primary).
		sort.SliceStable(members, func(a, b int) bool {
			if members[a].hold.attemptID != members[b].hold.attemptID {
				return members[a].hold.attemptID < members[b].hold.attemptID
			}
			return members[a].childID < members[b].childID
		})
		// One observe per group — use first member's reservation identity.
		rep := members[0].entry
		acc, win, install := rep.AccountRef, rep.WindowKind, rep.InstallRef
		prov := children[members[0].idx].Provider
		groupID := "cg|" + gk.provider + "|" + gk.account + "|" + gk.install + "|" + gk.window + "|" + gk.beforeSrc + "|" + fmt.Sprintf("%d", gk.beforeCapUnix)
		// Observe once.
		rem, src, fr, conf, capAt, resetEv, okMatch := remainingForProviderWindow(postSnap, prov, acc, install, win)
		if !okMatch || rem == nil || strings.TrimSpace(src) == "" || capAt.IsZero() ||
			!strings.EqualFold(fr, "fresh") {
			for _, m := range members {
				children[m.idx].CapacityGroupID = groupID
				if preserveSucceededUnknown && m.launched && m.termOK {
					children[m.idx].CapacityNote += "; after_observation=deferred_for_forced_resume"
					continue
				}
				children[m.idx].CapacityNote += "; after_observation=unavailable_or_window_mismatch"
				releaseCapacityUnknown(&children[m.idx], ledger, m.hold, "executed_usage_unknown")
			}
			continue
		}
		// Chronology: after observation must not precede before capture.
		if rep.BeforeCapturedAt != nil && capAt.Before(rep.BeforeCapturedAt.UTC()) {
			for _, m := range members {
				children[m.idx].CapacityGroupID = groupID
				children[m.idx].CapacityNote += "; after_before_before_timestamp"
				releaseCapacityUnknown(&children[m.idx], ledger, m.hold, "after_chronology_invalid")
			}
			continue
		}
		// Reset crossing: if after rose without reset evidence, leave unknown.
		if *rem > rep.Before+0.001 && resetEv == "" {
			for _, m := range members {
				children[m.idx].CapacityGroupID = groupID
				children[m.idx].CapacityNote += "; after_reset_unproven"
				releaseCapacityUnknown(&children[m.idx], ledger, m.hold, "after_reset_unproven")
			}
			continue
		}
		// Attach the same observed after to every member via ObserveAfterBound
		// (same After value; identity-bound). Then reconcile allocated fractions.
		obsID := "obs|" + src + "|" + capAt.UTC().Format(time.RFC3339Nano) + "|" + install + "|" + win
		opts := capacityledger.ObserveAfterOpts{
			AccountRef: acc, WindowKind: win, InstallRef: install,
			ObservedAt: capAt, Confidence: conf, ObservationID: obsID,
			InventoryDigest: postSnap.Digest,
		}
		if resetEv != "" {
			opts.ResetObserved = true
			opts.ResetEvidence = resetEv
		}
		// Observe on each attempt (same after value) so durable ledger carries after.
		observeOK := true
		for _, m := range members {
			entry, err := ledger.ObserveAfterBound(m.hold.projectID, m.hold.runID, m.hold.attemptID, *rem, src, fr, opts)
			if err != nil {
				children[m.idx].CapacityNote += "; after_rejected=" + err.Error()
				observeOK = false
				continue
			}
			applyCapacityAfterFromEntry(&children[m.idx], entry)
			children[m.idx].CapacityGroupID = groupID
			children[m.idx].CapacityGroupObserveID = obsID
			children[m.idx].CapacityNote += "; after_observed source=" + src + " freshness=" + fr +
				" window=" + win + " install=" + install + " group=" + groupID
		}
		if !observeOK {
			for _, m := range members {
				if children[m.idx].CapacityActual == nil {
					releaseCapacityUnknown(&children[m.idx], ledger, m.hold, "after_observe_partial_fail")
				}
			}
			continue
		}
		// Single aggregate delta for the group (never ×N members).
		// Window aggregate is always estimated confidence.
		aggregate := rep.Before - *rem
		if aggregate < 0 {
			aggregate = 0
		}
		// Only launched attempts receive allocation; others release unknown.
		var launched []capacityGroupMember
		for _, m := range members {
			if m.launched {
				launched = append(launched, m)
			} else {
				releaseCapacityUnknown(&children[m.idx], ledger, m.hold, "not_launched")
			}
		}
		if len(launched) == 0 {
			continue
		}
		shares, method := allocateGroupDelta(launched, aggregate)
		srcTag := method + ":" + obsID
		var sum float64
		for i, m := range launched {
			share := shares[i]
			sum += share
			// Always estimated for group window aggregate.
			rec, rerr := ledger.ReconcileWithConfidence(
				m.hold.projectID, m.hold.runID, m.hold.attemptID,
				share, srcTag, quotapolicy.EvidenceEstimated,
			)
			if rerr != nil {
				children[m.idx].CapacityNote += "; reconcile_failed=" + rerr.Error()
				releaseCapacityUnknown(&children[m.idx], ledger, m.hold, "reconcile_failed")
				continue
			}
			children[m.idx].CapacityState = rec.State
			children[m.idx].CapacityActual = rec.Actual
			children[m.idx].ActualSource = rec.ActualSource
			children[m.idx].CapacityActualConfidence = string(quotapolicy.EvidenceEstimated)
			children[m.idx].CapacityGroupID = groupID
			children[m.idx].CapacityGroupObserveID = obsID
			children[m.idx].CapacityNote += "; reconciled=" + srcTag +
				fmt.Sprintf("; group_delta=%.6f share=%.6f method=%s", aggregate, share, method)
		}
		// Strict sum tolerance.
		if diff := sum - aggregate; diff > 1e-9 || diff < -1e-9 {
			// Fail closed: mark notes; do not invent correction spend.
			for _, m := range launched {
				children[m.idx].CapacityNote += fmt.Sprintf("; group_alloc_sum_mismatch sum=%.9f want=%.9f", sum, aggregate)
			}
		}
	}
}

func succeededAttempt(children []ChildReport, attemptID string) bool {
	for _, child := range children {
		if child.AttemptID == attemptID && child.Terminal == string(workgraph.TermSucceeded) {
			return true
		}
	}
	return false
}

// allocateGroupDelta splits totalDelta across launched members.
// Prefer proportional to raw token totals when any tokens present; else reservations.
// Returns shares summing to totalDelta within 1e-12 and the method tag prefix.
func allocateGroupDelta(members []capacityGroupMember, totalDelta float64) (shares []float64, method string) {
	n := len(members)
	shares = make([]float64, n)
	if n == 0 {
		return shares, "estimated_group_delta_empty"
	}
	if totalDelta <= 0 {
		return shares, "estimated_group_delta_zero"
	}
	var tokenSum, resSum float64
	for _, m := range members {
		tokenSum += float64(m.tokens)
		if m.reserved > 0 {
			resSum += m.reserved
		}
	}
	weights := make([]float64, n)
	if tokenSum > 0 {
		method = "estimated_group_delta_token_weighted"
		for i, m := range members {
			weights[i] = float64(m.tokens)
		}
	} else {
		method = "estimated_group_delta_reservation_weighted"
		for i, m := range members {
			if m.reserved > 0 {
				weights[i] = m.reserved
			} else {
				weights[i] = 1 // equal if no reservation recorded
			}
		}
	}
	var wsum float64
	for _, w := range weights {
		wsum += w
	}
	if wsum <= 0 {
		// Equal split fallback.
		each := totalDelta / float64(n)
		for i := range shares {
			shares[i] = each
		}
		// Fix last for exact sum.
		var s float64
		for i := 0; i < n-1; i++ {
			s += shares[i]
		}
		shares[n-1] = totalDelta - s
		return shares, method
	}
	var s float64
	for i := 0; i < n-1; i++ {
		shares[i] = totalDelta * (weights[i] / wsum)
		s += shares[i]
	}
	shares[n-1] = totalDelta - s // exact remainder
	return shares, method
}

func releaseCapacityUnknown(cr *ChildReport, ledger *capacityledger.Ledger, h capacityHold, reason string) {
	if cr == nil || ledger == nil {
		return
	}
	if entry, err := ledger.Release(h.projectID, h.runID, h.attemptID, reason); err == nil {
		cr.CapacityState = entry.State
		cr.CapacityNote += "; released=" + reason
		// Do not invent Actual — leave nil if not already reconciled.
		if cr.CapacityActual == nil {
			cr.ActualSource = ""
		}
	}
}

func applyCapacityBeforeFromEntry(cr *ChildReport, entry capacityledger.Entry) {
	if cr == nil {
		return
	}
	if entry.BeforeSource != "" {
		cr.CapacityBeforeSource = entry.BeforeSource
	}
	if entry.BeforeCapturedAt != nil {
		cr.CapacityBeforeCapturedAt = entry.BeforeCapturedAt.UTC()
	}
	if entry.Freshness != "" {
		cr.CapacityBeforeFreshness = entry.Freshness
	}
	if entry.Confidence != "" {
		cr.CapacityBeforeConfidence = string(entry.Confidence)
	}
	cr.CapacityBeforeInventoryDigest = entry.BeforeInventoryDigest
	// Exact window reset identity from ledger — never prose parse.
	if entry.ResetAt != nil {
		t := entry.ResetAt.UTC()
		cr.CapacityResetAt = &t
	} else {
		cr.CapacityResetAt = nil
	}
	b := entry.Before
	cr.CapacityBefore = &b
	r := entry.Reserved
	cr.CapacityReserved = &r
}

// applyCapacityAfterFromEntry copies structured after evidence from a ledger entry.
func applyCapacityAfterFromEntry(cr *ChildReport, entry capacityledger.Entry) {
	if cr == nil {
		return
	}
	cr.CapacityAfter = entry.After
	cr.CapacityAfterSource = entry.AfterSource
	cr.CapacityAfterFreshness = entry.AfterFreshness
	cr.CapacityAfterConfidence = string(entry.AfterConfidence)
	cr.CapacityAfterState = entry.AfterState
	cr.CapacityAfterInventoryDigest = entry.AfterInventoryDigest
	if entry.AfterObservedAt != nil {
		cr.CapacityAfterObservedAt = entry.AfterObservedAt.UTC()
	} else {
		cr.CapacityAfterObservedAt = time.Time{}
	}
	// Before fields when present on entry.
	if entry.BeforeSource != "" {
		cr.CapacityBeforeSource = entry.BeforeSource
	}
	if entry.BeforeCapturedAt != nil {
		cr.CapacityBeforeCapturedAt = entry.BeforeCapturedAt.UTC()
	}
	if entry.Freshness != "" && cr.CapacityBeforeFreshness == "" {
		cr.CapacityBeforeFreshness = entry.Freshness
	}
	if entry.Confidence != "" && cr.CapacityBeforeConfidence == "" {
		cr.CapacityBeforeConfidence = string(entry.Confidence)
	}
	if cr.CapacityBeforeInventoryDigest == "" {
		cr.CapacityBeforeInventoryDigest = entry.BeforeInventoryDigest
	}
	// Keep CapacityResetAt identical to reserved window identity (UTC).
	if entry.ResetAt != nil {
		t := entry.ResetAt.UTC()
		cr.CapacityResetAt = &t
	}
}

// requirePriorMatchesCurrentContract fails closed unless prior durable identity
// exactly equals the current materialized child contract. Never rewrites prior.
// wantRunID is required for canonical AttemptID (Generation-1 → 0-indexed suffix).
func requirePriorMatchesCurrentContract(workItemID string, prior workflowrun.ChildOutcome, wantClass, wantPlan, wantCCD, wantRunID string) error {
	if strings.TrimSpace(prior.WorkItemID) != strings.TrimSpace(workItemID) {
		return fmt.Errorf("goalrun: resume prior work_item_id %q != current %q", prior.WorkItemID, workItemID)
	}
	if prior.Generation < 1 {
		return fmt.Errorf("goalrun: resume prior %s missing positive generation", workItemID)
	}
	if strings.TrimSpace(prior.AttemptID) == "" {
		return fmt.Errorf("goalrun: resume prior %s missing attempt_id", workItemID)
	}
	if strings.TrimSpace(prior.TaskClass) == "" {
		return fmt.Errorf("goalrun: resume prior %s missing task_class (legacy refuse)", workItemID)
	}
	if strings.TrimSpace(prior.ExecutionPlanDigest) == "" {
		return fmt.Errorf("goalrun: resume prior %s missing execution_plan_digest (legacy refuse)", workItemID)
	}
	if strings.TrimSpace(prior.ChildContractDigest) == "" {
		return fmt.Errorf("goalrun: resume prior %s missing child_contract_digest (legacy refuse)", workItemID)
	}
	if prior.TaskClass != wantClass {
		return fmt.Errorf("goalrun: resume prior %s task_class %q != current %q", workItemID, prior.TaskClass, wantClass)
	}
	if prior.ExecutionPlanDigest != wantPlan {
		return fmt.Errorf("goalrun: resume prior %s execution_plan_digest %q != current %q", workItemID, prior.ExecutionPlanDigest, wantPlan)
	}
	if prior.ChildContractDigest != wantCCD {
		return fmt.Errorf("goalrun: resume prior %s child_contract_digest %q != current %q", workItemID, prior.ChildContractDigest, wantCCD)
	}
	if err := requireCanonicalPriorAttempt(prior, workItemID, wantPlan, wantRunID); err != nil {
		return err
	}
	return nil
}

// selectLatestValidatedOutcome requires every terminal/reuse outcome for a work
// item to carry full assignment identity (class/plan/full CCD/attempt/positive gen).
// All outcomes must share the same class/plan/CCD, distinct attempt IDs, and
// strictly increasing positive generations. Returns the highest-generation outcome.
func selectLatestValidatedOutcome(workItemID string, outs []workflowrun.ChildOutcome) (workflowrun.ChildOutcome, error) {
	if len(outs) == 0 {
		return workflowrun.ChildOutcome{}, fmt.Errorf("goalrun: child %s no outcomes", workItemID)
	}
	// Sort by generation ascending for increasing check.
	sorted := append([]workflowrun.ChildOutcome(nil), outs...)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].Generation < sorted[i].Generation {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	var class, plan, ccd string
	seenAtt := map[string]bool{}
	prevGen := 0
	for i, o := range sorted {
		if err := requireTerminalOutcomeIdentity(workItemID, o); err != nil {
			return workflowrun.ChildOutcome{}, err
		}
		if i == 0 {
			class, plan, ccd = o.TaskClass, o.ExecutionPlanDigest, o.ChildContractDigest
		} else {
			if o.TaskClass != class || o.ExecutionPlanDigest != plan || o.ChildContractDigest != ccd {
				return workflowrun.ChildOutcome{}, fmt.Errorf(
					"goalrun: child %s assignment identity diverges across outcomes (class/plan/ccd)", workItemID)
			}
			if o.Generation <= prevGen {
				return workflowrun.ChildOutcome{}, fmt.Errorf(
					"goalrun: child %s generations not strictly increasing (%d then %d)", workItemID, prevGen, o.Generation)
			}
		}
		if seenAtt[o.AttemptID] {
			return workflowrun.ChildOutcome{}, fmt.Errorf(
				"goalrun: child %s duplicate attempt_id %q", workItemID, o.AttemptID)
		}
		seenAtt[o.AttemptID] = true
		prevGen = o.Generation
	}
	// Highest generation is last after sort.
	return sorted[len(sorted)-1], nil
}

// requireTerminalOutcomeIdentity enforces nonempty class/plan/full CCD/attempt
// and positive generation for every terminal or reuse outcome. No legacy empty path.
func requireTerminalOutcomeIdentity(workItemID string, o workflowrun.ChildOutcome) error {
	// Exact authority: no TrimSpace/EqualFold acceptance on terminal or IDs.
	if !exactCanonicalTerminal(o.Terminal) {
		return fmt.Errorf("goalrun: child %s outcome invalid terminal %q (exact succeeded|failed|cancelled|skipped)", workItemID, o.Terminal)
	}
	if o.TaskClass == "" {
		return fmt.Errorf("goalrun: child %s outcome missing task_class", workItemID)
	}
	if o.ExecutionPlanDigest == "" {
		return fmt.Errorf("goalrun: child %s outcome missing execution_plan_digest", workItemID)
	}
	ccd := o.ChildContractDigest
	if ccd == "" || !strings.HasPrefix(ccd, "sha256:") || len(strings.TrimPrefix(ccd, "sha256:")) != 64 {
		return fmt.Errorf("goalrun: child %s outcome child_contract_digest must be full sha256, got %q", workItemID, o.ChildContractDigest)
	}
	if o.AttemptID == "" {
		return fmt.Errorf("goalrun: child %s outcome missing attempt_id", workItemID)
	}
	if o.Generation < 1 {
		return fmt.Errorf("goalrun: child %s outcome generation must be positive, got %d", workItemID, o.Generation)
	}
	return nil
}

// mergeAssignmentIdentity copies/validates TaskClass, ExecutionPlanDigest,
// ChildContractDigest, AttemptID, and positive Generation from ChildOutcome into
// ChildReport. Contradictions fail closed — never silent overwrite. Every
// terminal/reuse outcome must already satisfy requireTerminalOutcomeIdentity.
func mergeAssignmentIdentity(cr *ChildReport, co workflowrun.ChildOutcome) error {
	if cr == nil {
		return fmt.Errorf("goalrun: nil child report")
	}
	if err := requireTerminalOutcomeIdentity(cr.ChildID, co); err != nil {
		return err
	}
	if prev := strings.TrimSpace(cr.TaskClass); prev != "" && prev != co.TaskClass {
		return fmt.Errorf("goalrun: child %s task_class mismatch report=%q outcome=%q", cr.ChildID, prev, co.TaskClass)
	}
	cr.TaskClass = co.TaskClass
	if prev := strings.TrimSpace(cr.ExecutionPlanDigest); prev != "" && prev != co.ExecutionPlanDigest {
		return fmt.Errorf("goalrun: child %s execution_plan_digest mismatch report=%q outcome=%q", cr.ChildID, prev, co.ExecutionPlanDigest)
	}
	cr.ExecutionPlanDigest = co.ExecutionPlanDigest
	if prev := strings.TrimSpace(cr.ChildContractDigest); prev != "" && prev != co.ChildContractDigest {
		return fmt.Errorf("goalrun: child %s child_contract_digest mismatch report=%q outcome=%q", cr.ChildID, prev, co.ChildContractDigest)
	}
	cr.ChildContractDigest = co.ChildContractDigest
	if cr.Generation > 0 && cr.Generation != co.Generation {
		return fmt.Errorf("goalrun: child %s generation mismatch report=%d outcome=%d", cr.ChildID, cr.Generation, co.Generation)
	}
	cr.Generation = co.Generation
	return nil
}

// remainingForProviderWindow requires exact provider+account+install+window.
// ok=false when any identity is empty or no matching row is available.
// Returns real window Source/Freshness/Confidence/CapturedAt — never invents
// capacity_snapshot or time.Now. Never attaches another install's observation.
// resetEvidence is non-empty when the observation indicates a reset boundary.
func remainingForProviderWindow(snap *capacitysnapshot.Snapshot, provider, accountRef, installRef, windowKind string) (
	rem *float64, source, freshness string, conf quotapolicy.EvidenceClass, capturedAt time.Time, resetEvidence string, ok bool,
) {
	if snap == nil {
		return nil, "", "", "", time.Time{}, "", false
	}
	provider = strings.TrimSpace(provider)
	accountRef = strings.TrimSpace(accountRef)
	installRef = strings.TrimSpace(installRef)
	windowKind = strings.TrimSpace(windowKind)
	// Exact nonempty identity required — refuse wildcard that could cross-wire installs.
	if provider == "" || accountRef == "" || installRef == "" || windowKind == "" {
		return nil, "", "", "", time.Time{}, "", false
	}
	wantAcc := capacityledger.CanonicalAccountRef(accountRef)
	for _, a := range snap.Accounts {
		if !strings.EqualFold(strings.TrimSpace(a.Provider), provider) {
			continue
		}
		if capacityledger.CanonicalAccountRef(a.AccountRef) != wantAcc {
			continue
		}
		// Exact install_ref on the observation row — never first-match across installs.
		if strings.TrimSpace(a.InstallRef) != installRef {
			continue
		}
		for _, w := range a.Windows {
			if !windowKindExact(windowKind, string(w.Kind)) {
				continue
			}
			if f := capacitysnapshot.RemainingFraction(w); f != nil {
				src := strings.TrimSpace(w.Source)
				// Do not invent source — empty stays empty (fail closed at observe/validate).
				fr := string(w.Freshness)
				c := quotapolicy.EvidenceEstimated
				switch w.Confidence {
				case capacitysnapshot.ConfidenceExact:
					c = quotapolicy.EvidenceExact
				case capacitysnapshot.ConfidenceEstimated:
					c = quotapolicy.EvidenceEstimated
				default:
					c = quotapolicy.EvidenceUnknown
				}
				resetEv := ""
				blob := strings.ToLower(src + " " + fr)
				if strings.Contains(blob, "reset") {
					resetEv = src
				}
				return f, src, fr, c, w.CapturedAt, resetEv, true
			}
		}
	}
	return nil, "", "", "", time.Time{}, "", false
}

// windowKindExact requires exact window identity (no provider-defined↔five_hour alias,
// no empty-as-compatible). Unknown/provider-defined is not a known reservable fixed window
// even when both sides are identical.
func windowKindExact(want, have string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	have = strings.ToLower(strings.TrimSpace(have))
	if want == "" || have == "" {
		return false
	}
	// Normalize only pure spelling aliases of the same known fixed window.
	norm := func(s string) string {
		switch s {
		case "five-hour", "5h", "fixed_hour", "fixed-hour", "primary_5h":
			return "five_hour"
		case "fixed-week", "fixed_week":
			return "weekly"
		default:
			return s
		}
	}
	wn, hn := norm(want), norm(have)
	// Unknown / provider-defined may be reported but cannot match as reservable.
	for _, x := range []string{wn, hn} {
		if x == "provider-defined" || x == "unknown" || x == "provider_defined" {
			return false
		}
	}
	return wn == hn
}

// windowKindCompatible is retained for callers; exact-only semantics.
func windowKindCompatible(want, have string) bool {
	return windowKindExact(want, have)
}

// collectPlannedRouteUsage counts planned route diversity for dry-run previews
// only. Must not be used as post-execution multi-provider evidence.
func collectPlannedRouteUsage(children []ChildReport) (providers, models, depths []string) {
	ps, ms, ds := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range children {
		if c.Unavailable {
			continue
		}
		if c.Provider != "" {
			ps[c.Provider] = true
		}
		if c.Model != "" {
			ms[c.Model] = true
		}
		if c.Depth != "" {
			ds[c.Depth] = true
		}
	}
	return mapKeys(ps), mapKeys(ms), mapKeys(ds)
}

// collectUsage reports providers/models/depths that actually completed a
// successful accepted provider invocation with durable proof. Planned pins,
// pre-spawn fails, and fake integrated rows without ArgvDigest/route sources
// never count.
func collectUsage(children []ChildReport) (providers, models, depths []string) {
	ps, ms, ds := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range children {
		if !childActuallyExecutedProvider(c) {
			continue
		}
		if c.Provider != "" {
			ps[c.Provider] = true
		}
		// Model/depth diversity requires truthful model/effort source only.
		if truthfulActualSource(c.ActualSources.Model) && c.Model != "" {
			ms[c.Model] = true
		}
		if truthfulActualSource(c.ActualSources.Effort) && c.Depth != "" {
			ds[c.Depth] = true
		}
	}
	return mapKeys(ps), mapKeys(ms), mapKeys(ds)
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// truthfulActualSource is provider_stream or accepted_invocation only —
// never auth_binding/install_binding/request-copy emptiness.
func truthfulActualSource(s string) bool {
	switch strings.TrimSpace(s) {
	case "provider_stream", "accepted_invocation":
		return true
	default:
		return false
	}
}

// childActuallyExecutedProvider is true only for terminal success with durable
// full-success accepted-invocation proof:
//   - Terminal == succeeded
//   - nonempty ArgvDigest (redacted argv after real launch)
//   - at least one truthful ActualSources dimension (model/effort/permission)
//
// Planned/unavailable never count. Failed rows never count (including failed
// rows that somehow have capacity actual or auth/install binding alone).
// integrated+succeeded without ArgvDigest (FakeChildExecutor) never counts.
func childActuallyExecutedProvider(c ChildReport) bool {
	if c.Unavailable {
		return false
	}
	// Exact durable terminal only — aliases (Succeeded, padding) never count.
	if c.Terminal != "succeeded" {
		return false
	}
	return hasRealProviderLaunchProof(c.ArgvDigest, c.ActualSources)
}

// claimsRealProviderExecution is the lifecycle PID gate: same structured launch
// proof as childActuallyExecutedProvider (argv + accepted_invocation/provider_stream),
// including failed/MU attempts that actually spawned. Structural FakeChildExecutor
// rows without that proof must not require PID and must not green multi-provider.
func claimsRealProviderExecution(c ChildReport, co workflowrun.ChildOutcome, hasCO bool) bool {
	if childActuallyExecutedProvider(c) {
		return true
	}
	argv := strings.TrimSpace(c.ArgvDigest)
	src := c.ActualSources
	if hasCO {
		if strings.TrimSpace(co.ArgvDigest) != "" {
			argv = strings.TrimSpace(co.ArgvDigest)
		}
		if co.ActualSources.Model != "" || co.ActualSources.Effort != "" || co.ActualSources.Permission != "" {
			src = co.ActualSources
		}
	}
	if hasRealProviderLaunchProof(argv, src) {
		return true
	}
	// Explicit ProviderPID claim without launch proof is still treated as real for PID binding.
	if c.ProviderPID > 0 {
		return true
	}
	return false
}

func hasRealProviderLaunchProof(argvDigest string, src workflowrun.ActualRouteSources) bool {
	if strings.TrimSpace(argvDigest) == "" {
		return false
	}
	return truthfulActualSource(src.Model) ||
		truthfulActualSource(src.Effort) ||
		truthfulActualSource(src.Permission)
}

func emitChild(w io.Writer, cr ChildReport) {
	if w == nil {
		return
	}
	b, _ := json.Marshal(cr)
	fmt.Fprintf(w, "%s\n", string(b))
}

func firstNonEmpty(vals ...string) string {
	for _, a := range vals {
		if strings.TrimSpace(a) != "" {
			return a
		}
	}
	return ""
}

func shortID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 16 {
		return s
	}
	return s[:16]
}

// normalizePerm maps inventory/candidate permission tokens for comparison only.
// Never invents a default for missing route_requirement (ParseRouteRequirement does that gate).
func normalizePerm(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "readonly", "read_only", "ro", "read-only":
		return "read-only"
	case "bounded-write", "write", "workspace-write", "bounded_write":
		return "bounded_write"
	default:
		return p
	}
}

// normalizeDepth maps route depth tokens onto the canonical ladder.
func normalizeDepth(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	switch d {
	case "low", "minimal", "light":
		return "low"
	case "medium", "mid", "standard", "default":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max", "deep", "thinking":
		return "xhigh"
	default:
		return d
	}
}

// UniqueProjectID builds a durable project namespace for one disposable canary/repo.
// Never returns the shared "local-project" default used by stale multi-run pollution.
// IDs are alphanumeric-safe for home.ValidateProjectID (no path separators).
func UniqueProjectID(repoPath string, nowFn func() time.Time) string {
	if nowFn == nil {
		nowFn = time.Now
	}
	abs := strings.TrimSpace(repoPath)
	if abs == "" {
		abs = "."
	}
	// Hash path + nano time so two canaries on the same goal/issue never share state.
	seed := fmt.Sprintf("%s|%d", abs, nowFn().UTC().UnixNano())
	sum := sha256.Sum256([]byte(seed))
	return "disp-" + hex.EncodeToString(sum[:8])
}

func isReadOnlyPerm(p string) bool {
	return normalizePerm(p) == "read-only"
}

func providerSupportsReadOnly(provider string) bool {
	// Authoritative runtimecap contract: Antigravity is write-only.
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "antigravity":
		return false
	case "codex", "claude", "gemini", "grok":
		return true
	default:
		// Unknown providers are not assumed read-only-safe for product path.
		return false
	}
}

// goalCapacityReroute implements workflowrun.CapacityRerouteHook against the
// capacity ledger. After the new attempt is claimed: reconcile known actual on
// the prior reservation (or honest release when unknown), then reserve the
// alternate under NewAttemptID. Never double-holds; never invents actual.
// Emits durable CapacityTransition records keyed by attempt/account/window/reservation.
type goalCapacityReroute struct {
	ledger    *capacityledger.Ledger
	snap      *capacitysnapshot.Snapshot
	projectID string
	runID     string
	holds     map[string]capacityHold
}

func (g *goalCapacityReroute) OnModelUnavailableAlternate(in workflowrun.CapacityRerouteInput) (workflowrun.CapacityRerouteResult, error) {
	var zero workflowrun.CapacityRerouteResult
	if g == nil || g.ledger == nil || g.snap == nil {
		return zero, fmt.Errorf("goalrun: capacity reroute requires ledger and snapshot")
	}
	// Prior hold: exact workflow attempt ID only (PriorHoldAttempt / FailedAttemptID /
	// holds[AttemptID]). Never fall back to WorkItemID — dual rows share ChildID.
	priorAttempt := strings.TrimSpace(in.PriorHoldAttempt)
	if priorAttempt == "" {
		priorAttempt = strings.TrimSpace(in.FailedAttemptID)
	}
	if priorAttempt != "" {
		if h, ok := g.holds[priorAttempt]; ok && strings.TrimSpace(h.attemptID) != "" {
			priorAttempt = strings.TrimSpace(h.attemptID)
		}
	}
	if priorAttempt == "" {
		return zero, fmt.Errorf("goalrun: prior capacity attempt id required (refusing WorkItemID fallback)")
	}
	// Proof require: prior.AttemptID == failed.AttemptID when both provided.
	if failed := strings.TrimSpace(in.FailedAttemptID); failed != "" && priorAttempt != failed {
		return zero, fmt.Errorf("goalrun: prior hold %q != failed attempt %q", priorAttempt, failed)
	}
	var priorTrans workflowrun.CapacityTransition
	priorState := ""
	// Capacity truth: reconcile known actual/source; only honest release when unknown.
	if in.PriorActual != nil && strings.TrimSpace(in.PriorSource) != "" &&
		!strings.EqualFold(strings.TrimSpace(in.PriorSource), "unknown") {
		ent, err := g.ledger.Reconcile(g.projectID, g.runID, priorAttempt, *in.PriorActual, in.PriorSource)
		if err != nil {
			return zero, fmt.Errorf("reconcile prior reservation: %w", err)
		}
		priorState = ent.State
		priorTrans = capacityTransitionFromEntry(ent, "prior", firstNonEmpty(in.FailedPermission), in)
	} else {
		ent, err := g.ledger.Release(g.projectID, g.runID, priorAttempt, "model_unavailable_supersede")
		if err != nil {
			return zero, fmt.Errorf("release prior reservation: %w", err)
		}
		priorState = ent.State
		// Actual nil + ActualSource empty is honest unknown (never invent "unknown").
		priorTrans = capacityTransitionFromEntry(ent, "prior", firstNonEmpty(in.FailedPermission), in)
		// Release path: force honest unknown actual even if entry carried residual.
		if priorTrans.Actual != nil && strings.TrimSpace(priorTrans.Source) == "" {
			priorTrans.Actual = nil
		}
	}
	// Drop prior live hold by AttemptID only — never WorkItemID/ChildID keys.
	delete(g.holds, priorAttempt)
	newAttempt := strings.TrimSpace(in.NewAttemptID)
	if newAttempt == "" {
		return zero, fmt.Errorf("goalrun: alternate NewAttemptID required")
	}
	entry, err := g.ledger.Reserve(capacityledger.ReserveInput{
		ProjectID: g.projectID, RunID: g.runID, AttemptID: newAttempt,
		PlanDigest: in.PlanDigest, GraphDigest: in.GraphDigest,
		TaskClass: in.TaskClass, ChildContractDigest: in.ChildContractDigest,
		Provider: in.AltProvider, Model: in.AltModel, Depth: in.Depth,
		AccountRef: in.AltAccountRef, InstallRef: in.AltInstallRef, WindowKind: in.AltWindowKind,
		Snapshot: g.snap, RouteReason: in.RouteReason,
	})
	if err != nil {
		return zero, err
	}
	// New executable alternate MUST be reserved (never relaunch spent keys).
	if strings.TrimSpace(entry.State) != "reserved" {
		return zero, fmt.Errorf("capacity alternate state=%s want reserved for %s/%s", entry.State, in.AltProvider, in.AltModel)
	}
	// Exact canonical account equality only.
	if want := strings.TrimSpace(in.AltAccountRef); want != "" {
		got := strings.TrimSpace(entry.AccountRef)
		wantC := capacityledger.CanonicalAccountRef(want)
		if got == "" || got != wantC && got != want {
			_, _ = g.ledger.Release(g.projectID, g.runID, newAttempt, "account_mismatch_compensate")
			return zero, fmt.Errorf("capacity alternate AccountRef %q does not bind route %q", got, want)
		}
	}
	// Live hold for alternate only — AttemptID key (never WorkItemID).
	g.holds[newAttempt] = capacityHold{
		projectID: g.projectID, runID: g.runID, attemptID: newAttempt,
	}
	altTrans := capacityTransitionFromEntry(entry, "alternate", firstNonEmpty(in.AltPermission, in.Permission), in)
	// Mid-transfer alternate has no actual yet.
	altTrans.Actual = nil
	altTrans.Source = ""
	return workflowrun.CapacityRerouteResult{
		AccountRef:          entry.AccountRef,
		WindowKind:          entry.WindowKind,
		PriorState:          priorState,
		AlternateState:      entry.State,
		ReservationID:       entry.ReservationID,
		PriorTransition:     priorTrans,
		AlternateTransition: altTrans,
	}, nil
}

// capacityTransitionFromEntry builds a full CapacityTransition from a ledger Entry
// plus route permission (ledger has no permission field).
func capacityTransitionFromEntry(ent capacityledger.Entry, role, permission string, in workflowrun.CapacityRerouteInput) workflowrun.CapacityTransition {
	tr := workflowrun.CapacityTransition{
		AttemptID: ent.AttemptID, Role: role, State: ent.State,
		Provider:         firstNonEmpty(ent.Provider, pickTransitionProvider(role, in)),
		Model:            firstNonEmpty(ent.Model, pickTransitionModel(role, in)),
		Depth:            firstNonEmpty(ent.Depth, pickTransitionDepth(role, in)),
		Permission:       permission,
		AccountRef:       ent.AccountRef,
		InstallRef:       firstNonEmpty(ent.InstallRef, pickTransitionInstall(role, in)),
		WindowKind:       firstNonEmpty(ent.WindowKind, pickTransitionWindow(role, in)),
		ReservationID:    ent.ReservationID,
		Before:           ent.Before,
		Reserved:         ent.Reserved,
		Actual:           ent.Actual,
		After:            ent.After,
		Source:           strings.TrimSpace(ent.ActualSource),
		BeforeSource:     ent.BeforeSource,
		BeforeFreshness:  ent.Freshness,
		BeforeConfidence: string(ent.Confidence),
		AfterSource:      ent.AfterSource,
		AfterFreshness:   ent.AfterFreshness,
		AfterConfidence:  string(ent.AfterConfidence),
		AfterState:       ent.AfterState,
		ActualConfidence: string(ent.ActualConfidence),
	}
	if ent.BeforeCapturedAt != nil {
		t := ent.BeforeCapturedAt.UTC()
		tr.BeforeCapturedAt = &t
	}
	if ent.ResetAt != nil {
		t := ent.ResetAt.UTC()
		tr.ResetAt = &t
	}
	if ent.AfterObservedAt != nil {
		t := ent.AfterObservedAt.UTC()
		tr.AfterObservedAt = &t
	}
	return tr
}

func pickTransitionProvider(role string, in workflowrun.CapacityRerouteInput) string {
	if role == "prior" {
		return in.FailedProvider
	}
	return in.AltProvider
}
func pickTransitionModel(role string, in workflowrun.CapacityRerouteInput) string {
	if role == "prior" {
		return in.FailedModel
	}
	return in.AltModel
}
func pickTransitionDepth(role string, in workflowrun.CapacityRerouteInput) string {
	if role == "prior" {
		return firstNonEmpty(in.FailedDepth, in.Depth)
	}
	return firstNonEmpty(in.AltDepth, in.Depth)
}
func pickTransitionInstall(role string, in workflowrun.CapacityRerouteInput) string {
	if role == "prior" {
		return in.FailedInstallRef
	}
	return in.AltInstallRef
}
func pickTransitionWindow(role string, in workflowrun.CapacityRerouteInput) string {
	if role == "prior" {
		return in.FailedWindowKind
	}
	return in.AltWindowKind
}

// finalizeCapacityTransitions is ledger-only using ONLY exact prior/alternate
// attempt IDs from mid (wres.CapacityTransitions). No holds map / ChildReport -g1
// fallback. Unknown role, extra transition, duplicate role, missing role => nil.
// Ledger readback remains authoritative for final state/actual/source.
func finalizeCapacityTransitions(
	ledger *capacityledger.Ledger,
	projectID, runID string,
	mid []workflowrun.CapacityTransition,
) []workflowrun.CapacityTransition {
	if ledger == nil {
		return nil
	}
	if len(mid) == 0 {
		return nil
	}
	roleCount := map[string]int{}
	byRole := map[string]workflowrun.CapacityTransition{}
	for _, tr := range mid {
		role := strings.TrimSpace(tr.Role)
		att := strings.TrimSpace(tr.AttemptID)
		if role == "" || att == "" {
			return nil // incomplete transition
		}
		if role != "prior" && role != "alternate" {
			return nil // unknown role
		}
		roleCount[role]++
		if roleCount[role] > 1 {
			return nil // duplicate role
		}
		byRole[role] = tr
	}
	// Exactly one prior + one alternate; no extras (len(mid) must be 2).
	if len(mid) != 2 || roleCount["prior"] != 1 || roleCount["alternate"] != 1 {
		return nil
	}
	failedAtt := strings.TrimSpace(byRole["prior"].AttemptID)
	retryAtt := strings.TrimSpace(byRole["alternate"].AttemptID)
	if failedAtt == "" || retryAtt == "" || failedAtt == retryAtt {
		return nil
	}

	var out []workflowrun.CapacityTransition
	for _, role := range []string{"prior", "alternate"} {
		seed := byRole[role]
		att := strings.TrimSpace(seed.AttemptID)
		ent, ok := ledger.Get(projectID, runID, att)
		if !ok {
			return nil
		}
		// Ledger is capacity truth; seed retains Permission (not on ledger Entry).
		final := workflowrun.CapacityTransition{
			AttemptID:             ent.AttemptID,
			Role:                  role,
			State:                 ent.State,
			Provider:              ent.Provider,
			Model:                 ent.Model,
			Depth:                 ent.Depth,
			Permission:            firstNonEmpty(seed.Permission, ""),
			AccountRef:            ent.AccountRef,
			InstallRef:            ent.InstallRef,
			WindowKind:            ent.WindowKind,
			ReservationID:         ent.ReservationID,
			Before:                ent.Before,
			Reserved:              ent.Reserved,
			Actual:                ent.Actual,
			After:                 ent.After,
			Source:                strings.TrimSpace(ent.ActualSource),
			BeforeSource:          ent.BeforeSource,
			BeforeFreshness:       ent.Freshness,
			BeforeConfidence:      string(ent.Confidence),
			BeforeInventoryDigest: ent.BeforeInventoryDigest,
			AfterSource:           ent.AfterSource,
			AfterFreshness:        ent.AfterFreshness,
			AfterConfidence:       string(ent.AfterConfidence),
			AfterState:            ent.AfterState,
			AfterInventoryDigest:  ent.AfterInventoryDigest,
			ActualConfidence:      string(ent.ActualConfidence),
		}
		if ent.BeforeCapturedAt != nil {
			t := ent.BeforeCapturedAt.UTC()
			final.BeforeCapturedAt = &t
		}
		if ent.ResetAt != nil {
			t := ent.ResetAt.UTC()
			final.ResetAt = &t
		}
		if ent.AfterObservedAt != nil {
			t := ent.AfterObservedAt.UTC()
			final.AfterObservedAt = &t
		}
		// Fail closed: incomplete identity (incl. Permission) cannot green MU capacity proof.
		if strings.TrimSpace(final.InstallRef) == "" || strings.TrimSpace(final.WindowKind) == "" ||
			strings.TrimSpace(final.ReservationID) == "" || strings.TrimSpace(final.Provider) == "" ||
			strings.TrimSpace(final.Model) == "" || strings.TrimSpace(final.Depth) == "" ||
			strings.TrimSpace(final.AccountRef) == "" || strings.TrimSpace(final.Permission) == "" {
			return nil
		}
		out = append(out, final)
	}
	if len(out) != 2 {
		return nil
	}
	return out
}

// CompensateAlternateHold releases the alternate reservation after a post-transfer
// pre-launch failure so no live hold remains.
func (g *goalCapacityReroute) CompensateAlternateHold(newAttemptID string) error {
	if g == nil || g.ledger == nil {
		return fmt.Errorf("goalrun: capacity compensate requires ledger")
	}
	newAttemptID = strings.TrimSpace(newAttemptID)
	if newAttemptID == "" {
		return fmt.Errorf("goalrun: compensate requires new attempt id")
	}
	// Clear hold map entries pointing at this attempt (AttemptID keys).
	for k, h := range g.holds {
		if idExact(h.attemptID, newAttemptID) || idExact(k, newAttemptID) {
			delete(g.holds, k)
		}
	}
	ent, err := g.ledger.Release(g.projectID, g.runID, newAttemptID, "compensate_post_transfer_failure")
	if err != nil {
		return err
	}
	if ent.State != "released" && ent.State != "reconciled" && ent.State != "cancelled" {
		return fmt.Errorf("goalrun: compensate left state=%s", ent.State)
	}
	return nil
}

// identityCapacitySeed reduces transitions to Role+AttemptID (+ Permission when
// known). Ledger re-read is final capacity truth for fractions/state/install/window.
// Requires len(trs)==2 with exactly one prior and one alternate; any other
// length/duplicate/unknown role yields empty seed.
func identityCapacitySeed(trs []workflowrun.CapacityTransition) []workflowrun.CapacityTransition {
	if len(trs) != 2 {
		return nil
	}
	var prior, alt workflowrun.CapacityTransition
	for _, tr := range trs {
		role := strings.TrimSpace(tr.Role)
		att := strings.TrimSpace(tr.AttemptID)
		if role == "" || att == "" {
			return nil
		}
		switch role {
		case "prior":
			if prior.AttemptID != "" {
				return nil
			}
			prior = workflowrun.CapacityTransition{Role: "prior", AttemptID: att, Permission: strings.TrimSpace(tr.Permission)}
		case "alternate":
			if alt.AttemptID != "" {
				return nil
			}
			alt = workflowrun.CapacityTransition{Role: "alternate", AttemptID: att, Permission: strings.TrimSpace(tr.Permission)}
		default:
			return nil
		}
	}
	if prior.AttemptID == "" || alt.AttemptID == "" || prior.AttemptID == alt.AttemptID {
		return nil
	}
	return []workflowrun.CapacityTransition{prior, alt}
}

// recordModelUnavailableExcludes binds durable typed failure → reroute evidence
// from workflow child outcomes (Claimed=true on the failed attempt; new attempt
// is the retry id). Success alternate outcomes carry SupersedesAttemptID +
// RerouteEventRef; the failed attempt row carries FailureClass=model_unavailable.
func recordModelUnavailableExcludes(wres workflowrun.Result, record func(childID, provider, reason, msg string, hard, soft, claimed bool)) string {
	retryID := ""
	eventRef := ""
	var failedProv, failedChild string
	for _, co := range wres.Children {
		// Exact FailureClass literal — no EqualFold alias into durable MU authority.
		if co.FailureClass == "model_unavailable" {
			failedProv = co.Provider
			failedChild = co.WorkItemID
		}
		if strings.TrimSpace(co.SupersedesAttemptID) != "" && strings.TrimSpace(co.AttemptID) != "" {
			retryID = co.AttemptID
			eventRef = firstNonEmpty(co.RerouteEventRef, eventRef)
		}
	}
	if failedProv == "" || failedChild == "" {
		return retryID
	}
	msg := firstNonEmpty(eventRef, "model_unavailable claimed failure")
	if retryID != "" && eventRef != "" {
		msg = eventRef + ";retry=" + retryID
	}
	record(failedChild, failedProv, "model_unavailable", msg, true, false, true)
	return retryID
}

// bindOpenPRVerifierFromChildren derives independent verifier identity from
// structured workflow children only. Request pins/prose are never consulted.
// Requires exactly one succeeded wi_verify (soul) and one succeeded wi_implement
// (tera) with distinct providers; OutputEvidence must be sha256:+64 hex.
// WorkItemID and TaskClass must be exact literals (no TrimSpace acceptance).
// Duplicate exact required IDs or invalid exact required children fail closed,
// except one raw-event-proven forced-interrupt implement predecessor followed
// by its immediate same-route successful generation.
func bindOpenPRVerifierFromChildren(children []workflowrun.ChildOutcome, events []workflowrun.Event) (provider, evidence string, ok bool) {
	var verifyKids, implementKids, testKids []workflowrun.ChildOutcome
	for _, c := range children {
		// Exact WorkItemID literals only — whitespace-altered IDs never match.
		switch c.WorkItemID {
		case "wi_verify":
			// Exact class + exact terminal literal — no EqualFold alias.
			if c.TaskClass != "soul" || c.Terminal != "succeeded" {
				verifyKids = append(verifyKids, c) // mark slot; rejected below
				continue
			}
			verifyKids = append(verifyKids, c)
		case "wi_implement":
			if c.TaskClass != "tera" || c.Terminal != "succeeded" {
				implementKids = append(implementKids, c)
				continue
			}
			implementKids = append(implementKids, c)
		case "wi_tests":
			testKids = append(testKids, c)
		}
	}
	// Exactly one verifier. Implement may have either one direct success or the
	// one exact forced-interrupt predecessor + its immediate successful retry.
	if len(verifyKids) != 1 || len(testKids) != 1 ||
		(len(implementKids) != 1 && len(implementKids) != 2) {
		return "", "", false
	}
	v := verifyKids[0]
	// Re-check exact success + class (invalid exact-ID rows already length-gated
	// when mixed with valid ones via count!=1; single invalid still fails here).
	if v.TaskClass != "soul" || v.Terminal != "succeeded" {
		return "", "", false
	}
	tests := testKids[0]
	if tests.TaskClass != "tera" || tests.Terminal != "succeeded" ||
		tests.IntegrateCommitSHA == "" ||
		v.VerifierDecision != workflowrun.VerifierDecisionPass ||
		!openPRIsExactSHA256Digest(v.VerifierVerdictDigest) ||
		!openPRIsExactGitOID(v.VerifierReviewedHeadSHA) ||
		v.VerifierReviewedHeadSHA != tests.IntegrateCommitSHA ||
		!exactVerifierVerdictEventBinding(v, events) {
		return "", "", false
	}
	var imp workflowrun.ChildOutcome
	switch len(implementKids) {
	case 1:
		imp = implementKids[0]
		if imp.TaskClass != "tera" || imp.Terminal != "succeeded" {
			return "", "", false
		}
	case 2:
		var failed workflowrun.ChildOutcome
		successes := 0
		failures := 0
		for _, c := range implementKids {
			switch {
			case c.TaskClass == "tera" && c.Terminal == "succeeded" && c.FailureClass == "":
				imp = c
				successes++
			case c.TaskClass == "tera" && c.Terminal == "cancelled" && c.FailureClass == "forced_interrupt":
				failed = c
				failures++
			default:
				return "", "", false
			}
		}
		if successes != 1 || failures != 1 || !exactForcedInterruptRetryBinding(failed, imp, events) {
			return "", "", false
		}
	default:
		return "", "", false
	}
	if v.Provider == "" || v.AttemptID == "" {
		return "", "", false
	}
	if imp.Provider == "" {
		return "", "", false
	}
	if !openPRIsExactSHA256Digest(v.OutputEvidence) {
		return "", "", false
	}
	// Distinct providers: case-insensitive equality fails closed (same account/provider
	// under casing aliases must not green dual-provider). This is not EqualFold
	// acceptance of durable success terminals.
	if strings.EqualFold(v.Provider, imp.Provider) {
		return "", "", false
	}
	return v.Provider, v.OutputEvidence, true
}

func exactVerifierVerdictEventBinding(verifier workflowrun.ChildOutcome, events []workflowrun.Event) bool {
	terminalCount := 0
	integrateCount := 0
	for _, ev := range events {
		if ev.WorkItemID != verifier.WorkItemID || ev.AttemptID != verifier.AttemptID {
			continue
		}
		switch ev.Kind {
		case "terminal":
			terminalCount++
			if ev.Terminal != "succeeded" || ev.Evidence != verifier.OutputEvidence ||
				ev.FailureClass != "" {
				return false
			}
			var payload map[string]string
			if json.Unmarshal(ev.Payload, &payload) != nil ||
				payload["verifier_decision"] != verifier.VerifierDecision ||
				payload["verifier_verdict_digest"] != verifier.VerifierVerdictDigest ||
				payload["verifier_reviewed_head_sha"] != verifier.VerifierReviewedHeadSHA {
				return false
			}
		case "integrate":
			integrateCount++
			if ev.CommitSHA != verifier.IntegrateCommitSHA {
				return false
			}
		}
	}
	return terminalCount == 1 && integrateCount == 1
}

func openPRIsExactGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func productOnlyChildOutcomes(children []workflowrun.ChildOutcome) []workflowrun.ChildOutcome {
	out := make([]workflowrun.ChildOutcome, len(children))
	copy(out, children)
	for i := range out {
		out[i].FilesTouched = workflowrun.ProductFilesOnly(out[i].FilesTouched)
	}
	return out
}

func loadOpenPRBindingEvents(path, projectID, runID string) ([]workflowrun.Event, error) {
	if err := requireExactEventLogPathStamp("open pr binding", path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	events, err := workflowrun.ParseEventJSONLStrict(string(raw), projectID, runID)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("empty exact event log")
	}
	for _, ev := range events {
		if workflowrun.IsParentOnlyEvent(ev) {
			continue
		}
		if err := workflowrun.ValidateChildEventIdentity(ev); err != nil {
			return nil, err
		}
	}
	if err := workflowrun.ValidateEventStreamInvariants(events); err != nil {
		return nil, err
	}
	return events, nil
}

func exactForcedInterruptRetryBinding(failed, retry workflowrun.ChildOutcome, events []workflowrun.Event) bool {
	if failed.WorkItemID != "wi_implement" || retry.WorkItemID != "wi_implement" ||
		failed.TaskClass != "tera" || retry.TaskClass != "tera" ||
		failed.Terminal != "cancelled" || failed.FailureClass != "forced_interrupt" ||
		retry.Terminal != "succeeded" || retry.FailureClass != "" ||
		failed.IntegrateCommitSHA != "" || retry.IntegrateCommitSHA == "" ||
		!openPRIsExactSHA256Digest(retry.OutputEvidence) {
		return false
	}
	if failed.Generation <= 0 || retry.Generation != failed.Generation+1 {
		return false
	}
	failedGen, err := workflowrun.ClaimGenerationFromAttemptID(failed.AttemptID)
	if err != nil || failedGen != failed.Generation {
		return false
	}
	retryGen, err := workflowrun.ClaimGenerationFromAttemptID(retry.AttemptID)
	if err != nil || retryGen != retry.Generation {
		return false
	}
	if !sameForcedRetryRoute(failed, retry) {
		return false
	}
	if err := workflowrun.ValidateEventStreamInvariants(events); err != nil {
		return false
	}

	type counts struct {
		claim, launch, pid, interrupt, terminal, integrate, reuse int
	}
	failedCounts := counts{}
	retryCounts := counts{}
	interruptID := ""
	for _, ev := range events {
		if ev.WorkItemID != "wi_implement" {
			continue
		}
		var child workflowrun.ChildOutcome
		var c *counts
		switch ev.AttemptID {
		case failed.AttemptID:
			child, c = failed, &failedCounts
		case retry.AttemptID:
			child, c = retry, &retryCounts
		default:
			return false
		}
		if err := workflowrun.ValidateChildEventIdentity(ev); err != nil {
			return false
		}
		if ev.Generation != child.Generation || ev.TaskClass != child.TaskClass ||
			ev.ExecutionPlanDigest != child.ExecutionPlanDigest ||
			ev.ChildContractDigest != child.ChildContractDigest {
			return false
		}
		payload, ok := exactStringPayload(ev.Payload)
		if !ok || payload["work_item_id"] != child.WorkItemID ||
			payload["attempt_id"] != child.AttemptID ||
			payload["generation"] != fmt.Sprintf("%d", child.Generation) ||
			payload["task_class"] != child.TaskClass ||
			payload["execution_plan_digest"] != child.ExecutionPlanDigest ||
			payload["child_contract_digest"] != child.ChildContractDigest ||
			payload["provider"] != child.Provider ||
			payload["model"] != child.Model ||
			payload["depth"] != child.Depth ||
			payload["permission"] != child.Permission ||
			payload["account_ref"] != child.AccountRef ||
			payload["install_ref"] != child.InstallRef {
			return false
		}
		switch ev.Kind {
		case "claim":
			c.claim++
		case "launch":
			c.launch++
		case "pid":
			c.pid++
		case "interrupt":
			c.interrupt++
			if ev.Terminal != "cancelled" || ev.FailureClass != "forced_interrupt" ||
				payload["terminal"] != "cancelled" ||
				payload["failure_class"] != "forced_interrupt" ||
				payload["interrupt_class"] != workflowrun.InterruptClassServiceForced ||
				payload["interrupt_id"] == "" {
				return false
			}
			interruptID = payload["interrupt_id"]
		case "terminal":
			c.terminal++
			if child.AttemptID == failed.AttemptID {
				if ev.Terminal != "cancelled" || ev.FailureClass != "forced_interrupt" ||
					payload["terminal"] != "cancelled" ||
					payload["failure_class"] != "forced_interrupt" ||
					payload["interrupt_class"] != workflowrun.InterruptClassServiceForced ||
					payload["interrupt_id"] == "" || payload["interrupt_id"] != interruptID {
					return false
				}
			} else if ev.Terminal != "succeeded" || ev.FailureClass != "" ||
				ev.Evidence != retry.OutputEvidence ||
				payload["terminal"] != "succeeded" ||
				payload["output_evidence"] != retry.OutputEvidence {
				return false
			}
		case "integrate":
			c.integrate++
			if child.AttemptID != retry.AttemptID ||
				ev.CommitSHA != retry.IntegrateCommitSHA ||
				payload["commit_sha"] != retry.IntegrateCommitSHA {
				return false
			}
		case "reuse":
			c.reuse++
			// A later zero-launch resume may append any number of exact reuse
			// observations for the successful retry. They do not replace the
			// original claim/launch/terminal/integrate chain. Fail closed on a
			// reuse of the interrupted predecessor or on altered terminal /
			// evidence identity.
			if child.AttemptID != retry.AttemptID ||
				ev.Terminal != "succeeded" || ev.FailureClass != "" ||
				ev.Evidence != retry.OutputEvidence || ev.CommitSHA != "" {
				return false
			}
		}
	}
	return failedCounts == (counts{claim: 1, launch: 1, pid: 1, interrupt: 1, terminal: 1}) &&
		retryCounts.claim == 1 && retryCounts.launch == 1 &&
		retryCounts.pid == 1 && retryCounts.terminal == 1 &&
		retryCounts.integrate == 1 &&
		interruptID != ""
}

func sameForcedRetryRoute(a, b workflowrun.ChildOutcome) bool {
	if a.Provider == "" || a.Model == "" || a.Depth == "" || a.Permission == "" ||
		a.AccountRef == "" || a.InstallRef == "" ||
		a.ExecutionPlanDigest == "" || a.ChildContractDigest == "" {
		return false
	}
	return a.Provider == b.Provider &&
		a.Model == b.Model &&
		a.Depth == b.Depth &&
		a.Permission == b.Permission &&
		a.AccountRef == b.AccountRef &&
		a.InstallRef == b.InstallRef &&
		a.ExecutionPlanDigest == b.ExecutionPlanDigest &&
		a.ChildContractDigest == b.ChildContractDigest
}

func exactStringPayload(raw json.RawMessage) (map[string]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false
	}
	return payload, true
}

// openPRIsExactSHA256Digest requires exact "sha256:" + 64 hex (no surrounding trim).
func openPRIsExactSHA256Digest(s string) bool {
	const p = "sha256:"
	if len(s) != len(p)+64 || !strings.HasPrefix(s, p) {
		return false
	}
	hexPart := s[len(p):]
	for i := 0; i < len(hexPart); i++ {
		c := hexPart[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
