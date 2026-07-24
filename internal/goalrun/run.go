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
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/goalpr"
	"github.com/jasonhnd/loopcoder/internal/routecontract"
	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
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
	// ActualSource is capacity fraction source (provider_usage|estimated|unknown).
	ActualSource string `json:"actual_source,omitempty"`
	// ActualSources is per-dimension route proof (never collapsed into ActualSource).
	ActualSources  workflowrun.ActualRouteSources `json:"actual_sources,omitempty"`
	ArgvDigest     string                         `json:"argv_digest,omitempty"`
	AttemptID      string                         `json:"attempt_id,omitempty"`
	OutputEvidence string                         `json:"output_evidence,omitempty"`
	WorktreePath   string                         `json:"worktree_path,omitempty"`
	Terminal       string                         `json:"terminal,omitempty"`
	NextAction     string                         `json:"next_action,omitempty"`
	Unavailable    bool                           `json:"unavailable,omitempty"`
}

// Result is parent evidence after bounded child execution.
type Result struct {
	Status  string `json:"status"`
	GraphID string `json:"graph_id"`
	// PlanDigest is the canonical ExecutionPlanDigest (workflowdef.Normalize).
	PlanDigest string `json:"plan_digest"`
	// GraphDigest is the separate workgraph.DigestGraph identity (not attempt key).
	GraphDigest         string             `json:"graph_digest,omitempty"`
	RunID               string             `json:"run_id,omitempty"`
	ProjectID           string             `json:"project_id,omitempty"`
	Children            []ChildReport      `json:"children"`
	Workflow            workflowrun.Result `json:"workflow"`
	Message             string             `json:"message"`
	HumanSummary        string             `json:"human_summary"`
	ProvidersUsed       []string           `json:"providers_used,omitempty"`
	ModelsUsed          []string           `json:"models_used,omitempty"`
	DepthsUsed          []string           `json:"depths_used,omitempty"`
	MultiProviderOK     bool               `json:"multi_provider_ok"`
	MultiModelOrDepthOK bool               `json:"multi_model_or_depth_ok"`
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
	priorSucceeded, attemptGen, loadedCP, hasCP, seedErr := loadAndValidateResumeSeeds(
		req.HomeDir, projectID, runID, g.GraphID, execPlanDigest, graphDigest,
		req.Resume, req.PriorSucceeded,
	)
	if seedErr != nil {
		return Result{}, seedErr
	}
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
	resumed := req.Resume || len(priorSucceeded) > 0
	_ = hasCP
	// Event log is a third durable resume source. Strict open/read/recover with
	// full error propagation; validate before any recovery append; merge next
	// gens as parsed g+1 (never hardcode 1); re-run parent validation before
	// inventory/ledger so ghost/event-derived keys cannot spend capacity.
	if req.Resume || len(priorSucceeded) > 0 || len(attemptGen) > 0 {
		var eventResumed bool
		var eerr error
		attemptGen, eventResumed, eerr = applyEventLogResumeSource(
			req.HomeDir, projectID, runID, execPlanDigest,
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
			if !strings.EqualFold(strings.TrimSpace(prior.Terminal), "succeeded") {
				return Result{}, fmt.Errorf("goalrun: resume prior %s terminal %q != succeeded (fail closed; no re-route)", it.ID, prior.Terminal)
			}
			if strings.TrimSpace(prior.AttemptID) == "" {
				return Result{}, fmt.Errorf("goalrun: resume prior %s missing attempt_id (fail closed; no re-route)", it.ID)
			}
			if strings.TrimSpace(prior.OutputEvidence) == "" {
				return Result{}, fmt.Errorf("goalrun: resume prior %s missing output_evidence (fail closed; no re-route)", it.ID)
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
			// Carry capacity fields from prior checkpoint child report when present.
			if loadedCP.Children != nil {
				for _, pc := range loadedCP.Children {
					if pc.ChildID == it.ID {
						cr.CapacityBefore = pc.CapacityBefore
						cr.CapacityReserved = pc.CapacityReserved
						if pc.CapacityActual != nil {
							cr.CapacityActual = pc.CapacityActual
						}
						cr.CapacityAfter = pc.CapacityAfter
						cr.CapacityState = firstNonEmpty(pc.CapacityState, "reconciled_or_released")
						if pc.CapacityNote != "" {
							cr.CapacityNote = pc.CapacityNote + "; resume_reuse"
						}
						break
					}
				}
			}
			// Resume capacity: reload exact ledger entry by attempt ID only.
			// Never fabricate Before=1.0 / Reserved=0.05 / After / ActualSource=estimated
			// when checkpoint or ledger facts are absent (fail closed for qualification).
			if ledger != nil && strings.TrimSpace(cr.AttemptID) != "" {
				if ent, ok := ledger.Get(projectID, runID, cr.AttemptID); ok {
					b, r := ent.Before, ent.Reserved
					cr.CapacityBefore = &b
					cr.CapacityReserved = &r
					cr.CapacityActual = ent.Actual
					cr.CapacityAfter = ent.After
					cr.CapacityState = ent.State
					cr.ActualSource = ent.ActualSource
					cr.AccountRef = firstNonEmpty(cr.AccountRef, ent.AccountRef)
					if ent.ReleaseReason != "" {
						cr.CapacityNote = firstNonEmpty(cr.CapacityNote, "resume") +
							"; release_reason=" + ent.ReleaseReason
					} else {
						cr.CapacityNote = firstNonEmpty(cr.CapacityNote, "resume_ledger_reload")
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
			holds[it.ID] = capacityHold{projectID: projectID, runID: runID, attemptID: attID}
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
			if dryRun {
				// Route preview only: release without execution.
				if rel, lerr := ledger.Release(projectID, runID, attemptID, "goalrun_dry_run_preview"); lerr == nil {
					cr.CapacityState = rel.State
					cr.CapacityNote += "; released=dry_run_preview"
					cr.RouteReason = rel.RouteReason
				}
			} else {
				holds[it.ID] = capacityHold{projectID: projectID, runID: runID, attemptID: attemptID}
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
			Permission:    sel.Permission,
			TaskClass:     cr.TaskClass,
			AccountRef:    cr.AccountRef,
			InstallRef:    firstNonEmpty(cr.InstallRef, sel.InstallRef),
			WindowKind:    firstNonEmpty(cr.WindowKind, sel.WindowKind),
			ReservationID: cr.ReservationID,
			RouteReason:   cr.RouteReason,
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
		// Dry-run reports planned route diversity only (not actual execution).
		out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed = collectPlannedRouteUsage(children)
		out.MultiProviderOK = len(out.ProvidersUsed) >= 2
		out.MultiModelOrDepthOK = len(out.ModelsUsed) >= 2 || len(out.DepthsUsed) >= 2
		out.HumanSummary = fmt.Sprintf(
			"goal graph %s dry-run children=%d providers=%v models=%v depths=%v",
			g.GraphID, len(children), out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed,
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
		AttemptGeneration:   attemptGen,
	})
	out := Result{
		GraphID: g.GraphID, PlanDigest: execPlanDigest, GraphDigest: graphDigest,
		RunID: runID, ProjectID: projectID,
		Children: children, Workflow: wres, Resumed: resumed,
	}
	out.ReuseCount = wres.ReuseCount
	out.WorktreePeak = wres.WorktreePeak
	out.ProcessPeak = wres.ProcessPeak
	// Durable model_unavailable → reroute event refs into route excludes.
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
	merged, merr := applyChildOutcomes(out.Children, wres, ledger, holds, postSnap)
	if merr != nil {
		return out, fmt.Errorf("goalrun: apply child outcomes: %w", merr)
	}
	out.Children = merged
	// Usage diversity is measured only after outcomes are applied (integrated /
	// terminal stages, ActualSources, argv). Planned-only or pre-spawn fail
	// rows must not count as multi-provider execution.
	out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed = collectUsage(out.Children)
	out.MultiProviderOK = len(out.ProvidersUsed) >= 2
	out.MultiModelOrDepthOK = len(out.ModelsUsed) >= 2 || len(out.DepthsUsed) >= 2

	// After applyChildOutcomes (and any failure-path release below), reload exact
	// ledger entries by attempt ID into Workflow.CapacityTransitions for canary.
	// Mid-workflow "reserved" alternate is not final qualification truth.
	refreshLedgerCapacityTransitions := func() {
		out.Workflow.CapacityTransitions = finalizeCapacityTransitions(
			ledger, projectID, runID, wres.CapacityTransitions,
		)
	}
	refreshLedgerCapacityTransitions()

	// Always persist durable checkpoint (partial or complete) for forced restart.
	cpPath, _ := saveRunCheckpoint(req.HomeDir, projectID, runID, req.Goal, req.Issue, actor, g.GraphID, execPlanDigest, graphDigest, out, wres, nowFn().UTC())
	out.CheckpointPath = cpPath

	if werr != nil {
		out.Status = "blocked"
		out.Message = werr.Error()
		if wres.Error != "" {
			out.Message = wres.Error
		}
		// Failure path: release remaining holds.
		if ledger != nil {
			for id, h := range holds {
				// Skip already reconciled/released in applyChildOutcomes.
				if e, ok := ledger.Get(h.projectID, h.runID, h.attemptID); ok {
					if e.State == "reconciled" || e.State == "released" {
						continue
					}
				}
				if rel, err := ledger.Release(h.projectID, h.runID, h.attemptID, "child_failed_or_cancelled"); err == nil {
					for i := range out.Children {
						if out.Children[i].ChildID == id && out.Children[i].CapacityState != "reconciled" {
							out.Children[i].CapacityState = rel.State
							out.Children[i].CapacityNote += "; released=child_failed_or_cancelled"
						}
					}
				}
			}
		}
		refreshLedgerCapacityTransitions()
		// Re-save checkpoint after release notes + final ledger transitions.
		if p, err := saveRunCheckpoint(req.HomeDir, projectID, runID, req.Goal, req.Issue, actor, g.GraphID, execPlanDigest, graphDigest, out, wres, nowFn().UTC()); err == nil {
			out.CheckpointPath = p
		}
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
		prOpen := req.GoalPR
		if prOpen == nil {
			prOpen = goalpr.Open
		}
		// Independent verifier must be a real verify-child digest (no pending-live).
		ind := strings.TrimSpace(req.IndependentVerifier)
		verEv := strings.TrimSpace(req.VerifierEvidence)
		if strings.Contains(strings.ToLower(verEv), "pending") {
			verEv = ""
		}
		for _, c := range wres.Children {
			if strings.Contains(strings.ToLower(c.WorkItemID), "verify") &&
				strings.EqualFold(c.Terminal, "succeeded") &&
				strings.HasPrefix(c.OutputEvidence, "sha256:") {
				ind = firstNonEmpty(c.Provider, ind)
				verEv = c.OutputEvidence
				break
			}
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
			SourceIssue: issueN, Actor: actor, Children: wres.Children,
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
				if _, eerr := EmitCanaryFromResult(out, *req.CanaryEmit); eerr == nil {
					out.CanaryEvidencePath = req.CanaryEmit.OutPath
				}
			}
			return out, fmt.Errorf("goalrun: open pr: %w", perr)
		}
		out.HumanSummary += fmt.Sprintf(" pr=%s human_gate=true auto_merge=false checks_green=%v", prRes.URL, prRes.RequiredChecksGreen)
	}

	// Exact-binary canary evidence emission (derived from events/PR/children).
	if req.CanaryEmit != nil {
		opts := *req.CanaryEmit
		if opts.HomeDir == "" {
			opts.HomeDir = req.HomeDir
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
func applyChildOutcomes(children []ChildReport, wres workflowrun.Result, ledger *capacityledger.Ledger, holds map[string]capacityHold, postSnap *capacitysnapshot.Snapshot) ([]ChildReport, error) {
	// Group outcomes by work item; validate full assignment identity family before pick.
	byItem := map[string][]workflowrun.ChildOutcome{}
	for _, c := range wres.Children {
		byItem[c.WorkItemID] = append(byItem[c.WorkItemID], c)
	}
	byID := map[string]workflowrun.ChildOutcome{}
	for id, outs := range byItem {
		chosen, err := selectLatestValidatedOutcome(id, outs)
		if err != nil {
			return children, err
		}
		byID[id] = chosen
	}
	done := map[string]bool{}
	for _, id := range wres.Integrated {
		done[id] = true
	}
	for i := range children {
		if children[i].Unavailable {
			continue
		}
		co, ok := byID[children[i].ChildID]
		if ok {
			// Assignment identity: fail closed on mismatch; never silent overwrite of contradictions.
			if err := mergeAssignmentIdentity(&children[i], co); err != nil {
				return children, err
			}
			if co.Provider != "" {
				children[i].Provider = co.Provider
			}
			if co.Model != "" {
				children[i].Model = co.Model
			}
			if co.Depth != "" {
				children[i].Depth = co.Depth
			}
			if co.AccountRef != "" {
				children[i].AccountRef = co.AccountRef
			}
			if co.RouteReason != "" {
				children[i].RouteReason = co.RouteReason
			}
			children[i].Terminal = co.Terminal
			children[i].AttemptID = co.AttemptID
			children[i].OutputEvidence = co.OutputEvidence
			children[i].WorktreePath = co.WorktreePath
			children[i].ActualSource = co.ActualSource
			children[i].ActualSources = co.ActualSources
			children[i].ArgvDigest = co.ArgvDigest
			if co.InstallRef != "" {
				children[i].InstallRef = co.InstallRef
			}
			if co.Permission != "" {
				children[i].Permission = co.Permission
			}
			if co.ActualCapacity != nil {
				children[i].CapacityActual = co.ActualCapacity
			}
		}
		if done[children[i].ChildID] || children[i].Terminal == "succeeded" {
			children[i].Stage = "integrated"
			if children[i].Terminal == "" {
				children[i].Terminal = "succeeded"
			}
			children[i].NextAction = "parent_continue"
		} else if wres.Status == workflowrun.StatusHumanGate {
			children[i].Stage = "human_gate"
			children[i].NextAction = "owner_merge"
		} else if children[i].Terminal != "" {
			// Single assignment of terminal stage (no duplicate set later).
			children[i].Stage = "terminal"
			children[i].NextAction = "inspect_failure"
		}

		// Capacity ledger: reserve → execute → reconcile|release.
		h, hasHold := holds[children[i].ChildID]
		if !hasHold || ledger == nil {
			continue
		}
		termOK := children[i].Terminal == "succeeded"
		if termOK && ok && co.ActualCapacity != nil && co.ActualSource != "" && co.ActualSource != "unknown" {
			entry, err := ledger.Reconcile(h.projectID, h.runID, h.attemptID, *co.ActualCapacity, co.ActualSource)
			if err == nil {
				children[i].CapacityState = entry.State
				children[i].CapacityActual = entry.Actual
				children[i].CapacityAfter = entry.After
				children[i].CapacityNote += "; reconciled=" + co.ActualSource
				children[i].ActualSource = co.ActualSource
				continue
			}
		}
		// Unknown actual or failure: honest release — never invent actual.
		reason := "executed_usage_unknown"
		if !termOK {
			reason = "child_" + firstNonEmpty(children[i].Terminal, "failed")
		}
		if entry, err := ledger.Release(h.projectID, h.runID, h.attemptID, reason); err == nil {
			children[i].CapacityState = entry.State
			children[i].CapacityNote += "; released=" + reason
			// Actual nil + ActualSource empty is honest unknown (do not invent "unknown").
			children[i].ActualSource = strings.TrimSpace(entry.ActualSource)
			children[i].CapacityActual = entry.Actual
		}
		// Always try fresh post-run remaining → after (source-tagged).
		// Required by #1343: after must not be n/a when observation exists.
		if children[i].CapacityAfter == nil {
			// Bind after to the same account+window as the reservation (fail closed).
			var reserved *capacityledger.Entry
			if e, ok := ledger.Get(h.projectID, h.runID, h.attemptID); ok {
				reserved = &e
			}
			acc, win, install := "", "", ""
			if reserved != nil {
				acc, win, install = reserved.AccountRef, reserved.WindowKind, reserved.InstallRef
			}
			if rem, src, fr, resetEv, okMatch := remainingForProviderWindow(postSnap, children[i].Provider, acc, win); rem != nil && okMatch {
				// Exact nonempty account/install/window required — no wildcard.
				opts := capacityledger.ObserveAfterOpts{AccountRef: acc, WindowKind: win, InstallRef: install}
				if resetEv != "" {
					opts.ResetObserved = true
					opts.ResetEvidence = resetEv
				}
				if entry, err := ledger.ObserveAfterBound(h.projectID, h.runID, h.attemptID, *rem, src, fr, opts); err == nil {
					children[i].CapacityAfter = entry.After
					children[i].CapacityNote += "; after_source=" + src + "; after_freshness=" + fr + "; after_window=" + win
				} else {
					children[i].CapacityNote += "; after_rejected=" + err.Error()
				}
			} else {
				children[i].CapacityNote += "; after_observation=unavailable_or_window_mismatch"
			}
		}
	}
	return children, nil
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
	if strings.TrimSpace(o.Terminal) == "" {
		return fmt.Errorf("goalrun: child %s outcome missing terminal", workItemID)
	}
	if strings.TrimSpace(o.TaskClass) == "" {
		return fmt.Errorf("goalrun: child %s outcome missing task_class", workItemID)
	}
	if strings.TrimSpace(o.ExecutionPlanDigest) == "" {
		return fmt.Errorf("goalrun: child %s outcome missing execution_plan_digest", workItemID)
	}
	ccd := strings.TrimSpace(o.ChildContractDigest)
	if ccd == "" || !strings.HasPrefix(ccd, "sha256:") || len(strings.TrimPrefix(ccd, "sha256:")) != 64 {
		return fmt.Errorf("goalrun: child %s outcome child_contract_digest must be full sha256, got %q", workItemID, o.ChildContractDigest)
	}
	if strings.TrimSpace(o.AttemptID) == "" {
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

// remainingForProviderWindow prefers the same account_ref + window_kind as the
// reservation. ok=false when no matching window is available.
// resetEvidence is non-empty when the observation indicates a reset boundary.
func remainingForProviderWindow(snap *capacitysnapshot.Snapshot, provider, accountRef, windowKind string) (rem *float64, source, freshness, resetEvidence string, ok bool) {
	if snap == nil || strings.TrimSpace(provider) == "" {
		return nil, "", "", "", false
	}
	accountRef = strings.TrimSpace(accountRef)
	windowKind = strings.TrimSpace(windowKind)
	wantAcc := ""
	if accountRef != "" {
		wantAcc = capacityledger.CanonicalAccountRef(accountRef)
	}
	for _, a := range snap.Accounts {
		if strings.TrimSpace(a.Provider) != strings.TrimSpace(provider) {
			continue
		}
		if wantAcc != "" {
			// Exact canonical account identity only.
			ar := capacityledger.CanonicalAccountRef(a.AccountRef)
			if ar != wantAcc {
				continue
			}
		}
		// Exact account + exact observed window only. No fallback to a different window.
		// Unknown/provider-defined may be reported but is not a known reservable fixed window.
		if windowKind == "" {
			// No exact window requested — refuse to invent a match.
			continue
		}
		for _, w := range a.Windows {
			if !windowKindExact(windowKind, string(w.Kind)) {
				continue
			}
			if f := capacitysnapshot.RemainingFraction(w); f != nil {
				src := strings.TrimSpace(w.Source)
				if src == "" {
					src = "capacity_snapshot"
				}
				resetEv := ""
				blob := strings.ToLower(src + " " + string(w.Freshness))
				if strings.Contains(blob, "reset") {
					resetEv = src
				}
				return f, src, string(w.Freshness), resetEv, true
			}
		}
	}
	return nil, "", "", "", false
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

// collectUsage reports providers/models/depths that actually executed a
// provider process with independent proof. Planned-only pins and fail-closed
// pre-spawn mismatches (no PID/argv/usage) must not count as multi-provider
// execution evidence.
func collectUsage(children []ChildReport) (providers, models, depths []string) {
	ps, ms, ds := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range children {
		if !childActuallyExecutedProvider(c) {
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

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// childActuallyExecutedProvider is true only when the child left independent
// execution evidence (argv digest, capacity actual, route ActualSources, or
// integrated success). Stage=planned alone never counts. Terminal=failed with
// empty sources (e.g. install_ref mismatch before PID) never counts.
func childActuallyExecutedProvider(c ChildReport) bool {
	if c.Unavailable {
		return false
	}
	stage := strings.ToLower(strings.TrimSpace(c.Stage))
	if stage == "planned" || stage == "unavailable" {
		return false
	}
	if strings.TrimSpace(c.ArgvDigest) != "" {
		return true
	}
	if c.CapacityActual != nil {
		return true
	}
	if strings.TrimSpace(c.ActualSources.Install) != "" ||
		strings.TrimSpace(c.ActualSources.Model) != "" ||
		strings.TrimSpace(c.ActualSources.Account) != "" ||
		strings.TrimSpace(c.ActualSources.Effort) != "" ||
		strings.TrimSpace(c.ActualSources.Permission) != "" {
		return true
	}
	// Integrated success is durable product proof even if some sources were stripped.
	if stage == "integrated" && strings.EqualFold(strings.TrimSpace(c.Terminal), "succeeded") {
		return true
	}
	return false
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
	// Prior hold: exact workflow attempt ID only (holds map / PriorHoldAttempt /
	// FailedAttemptID). Never fall back to WorkItemID — that cannot bind prior.
	priorAttempt := strings.TrimSpace(in.PriorHoldAttempt)
	if h, ok := g.holds[in.WorkItemID]; ok && strings.TrimSpace(h.attemptID) != "" {
		priorAttempt = strings.TrimSpace(h.attemptID)
	}
	if priorAttempt == "" {
		priorAttempt = strings.TrimSpace(in.FailedAttemptID)
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
		act := *in.PriorActual
		priorTrans = workflowrun.CapacityTransition{
			AttemptID: priorAttempt, Role: "prior", State: ent.State,
			Provider:   firstNonEmpty(ent.Provider, in.FailedProvider),
			Model:      firstNonEmpty(ent.Model, in.FailedModel),
			Depth:      firstNonEmpty(ent.Depth, in.FailedDepth, in.Depth),
			Permission: firstNonEmpty(in.FailedPermission),
			AccountRef: ent.AccountRef, WindowKind: ent.WindowKind,
			ReservationID: ent.ReservationID, Actual: &act, Source: strings.TrimSpace(ent.ActualSource),
		}
	} else {
		ent, err := g.ledger.Release(g.projectID, g.runID, priorAttempt, "model_unavailable_supersede")
		if err != nil {
			return zero, fmt.Errorf("release prior reservation: %w", err)
		}
		priorState = ent.State
		// Actual nil + ActualSource empty is honest unknown (never invent "unknown").
		priorTrans = workflowrun.CapacityTransition{
			AttemptID: priorAttempt, Role: "prior", State: ent.State,
			Provider:   firstNonEmpty(ent.Provider, in.FailedProvider),
			Model:      firstNonEmpty(ent.Model, in.FailedModel),
			Depth:      firstNonEmpty(ent.Depth, in.FailedDepth, in.Depth),
			Permission: firstNonEmpty(in.FailedPermission),
			AccountRef: ent.AccountRef, WindowKind: ent.WindowKind,
			ReservationID: ent.ReservationID, Actual: nil, Source: strings.TrimSpace(ent.ActualSource),
		}
	}
	delete(g.holds, in.WorkItemID)
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
	g.holds[in.WorkItemID] = capacityHold{
		projectID: g.projectID, runID: g.runID, attemptID: newAttempt,
	}
	altTrans := workflowrun.CapacityTransition{
		AttemptID: newAttempt, Role: "alternate", State: entry.State,
		Provider:   firstNonEmpty(entry.Provider, in.AltProvider),
		Model:      firstNonEmpty(entry.Model, in.AltModel),
		Depth:      firstNonEmpty(entry.Depth, in.AltDepth, in.Depth),
		Permission: firstNonEmpty(in.AltPermission, in.Permission),
		AccountRef: entry.AccountRef, WindowKind: entry.WindowKind,
		ReservationID: entry.ReservationID, Actual: nil, Source: "",
	}
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
		att := strings.TrimSpace(byRole[role].AttemptID)
		ent, ok := ledger.Get(projectID, runID, att)
		if !ok {
			return nil
		}
		final := workflowrun.CapacityTransition{
			AttemptID:     ent.AttemptID,
			Role:          role,
			State:         ent.State,
			Provider:      ent.Provider,
			Model:         ent.Model,
			Depth:         ent.Depth,
			AccountRef:    ent.AccountRef,
			WindowKind:    ent.WindowKind,
			ReservationID: ent.ReservationID,
			Actual:        ent.Actual,
			Source:        strings.TrimSpace(ent.ActualSource),
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
	// Clear hold map entries pointing at this attempt.
	for k, h := range g.holds {
		if h.attemptID == newAttemptID {
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

// recordModelUnavailableExcludes binds durable typed failure → reroute evidence
// from workflow child outcomes (Claimed=true on the failed attempt; new attempt
// is the retry id). Success alternate outcomes carry SupersedesAttemptID +
// RerouteEventRef; the failed attempt row carries FailureClass=model_unavailable.
func recordModelUnavailableExcludes(wres workflowrun.Result, record func(childID, provider, reason, msg string, hard, soft, claimed bool)) string {
	retryID := ""
	eventRef := ""
	var failedProv, failedChild string
	for _, co := range wres.Children {
		if strings.EqualFold(strings.TrimSpace(co.FailureClass), "model_unavailable") {
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
