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
	"github.com/jasonhnd/loopcoder/internal/goalpr"
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
	// capacity rehydrate. Explicit pin (including "fixture") skips auto-route.
	Provider string
	Model    string
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
	// Focused tests inject workflowrun.FakeChildExecutor.
	Executor workflowrun.ChildExecutor
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
}

// ChildReport is one transparent child line for UI/JSONL.
type ChildReport struct {
	ChildID          string   `json:"child_id"`
	Intent           string   `json:"intent"`
	Owner            string   `json:"owner"`
	RouteRequirement string   `json:"route_requirement"`
	Provider         string   `json:"provider,omitempty"`
	AccountRef       string   `json:"account_ref,omitempty"`
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
	ActualSource     string   `json:"actual_source,omitempty"`
	AttemptID        string   `json:"attempt_id,omitempty"`
	OutputEvidence   string   `json:"output_evidence,omitempty"`
	WorktreePath     string   `json:"worktree_path,omitempty"`
	Terminal         string   `json:"terminal,omitempty"`
	NextAction       string   `json:"next_action,omitempty"`
	Unavailable      bool     `json:"unavailable,omitempty"`
}

// Result is parent evidence after bounded child execution.
type Result struct {
	Status              string             `json:"status"`
	GraphID             string             `json:"graph_id"`
	PlanDigest          string             `json:"plan_digest"`
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
}

// capacityHold tracks a live reservation for post-execute reconcile/release.
type capacityHold struct {
	projectID, runID, attemptID string
}

// Execute decomposes the goal, routes each LoopCoder-owned child independently
// via bare auto-route + durable capacity rehydrate when not pinned, reserves
// capacity, runs real (or injected) child executors, reconciles actual usage
// when known (never fabricates), and emits transparent reports.
// Provider-native subagents are never used.
func Execute(ctx context.Context, req Request) (Result, error) {
	nowFn := req.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "owner"
	}
	g, err := workgraph.DecomposeGoal(workgraph.DecomposeOptions{
		Goal: req.Goal, Issue: req.Issue, Actor: actor, Owner: req.Owner, Now: now,
	})
	if err != nil {
		return Result{}, err
	}
	def, err := workflowdef.FromGraph(g)
	if err != nil {
		return Result{}, err
	}
	plan, err := workflowdef.Normalize(def)
	if err != nil {
		return Result{}, err
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
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		runID = "run_" + shortID(fmt.Sprintf("%s|%d", projectID, now.UnixNano()))
	}
	// Forced restart: load durable checkpoint for same RunID → PriorSucceeded seed.
	priorSucceeded := req.PriorSucceeded
	resumed := false
	var loadedCP Checkpoint
	if req.Resume || priorSucceeded == nil {
		if cp, _, lerr := LoadCheckpoint(req.HomeDir, projectID, runID); lerr == nil {
			loadedCP = cp
			if priorSucceeded == nil && len(cp.PriorSucceeded) > 0 {
				priorSucceeded = cp.PriorSucceeded
			}
			if req.Resume || len(priorSucceeded) > 0 {
				resumed = len(priorSucceeded) > 0
			}
			// Prefer durable graph identity when resuming same run.
			if req.Resume && cp.GraphID != "" && cp.GraphID != g.GraphID {
				// Graph re-decomposition can change IDs; still allow resume by work-item
				// keys present in PriorSucceeded (stable attempt evidence).
			}
		} else if req.Resume && !osIsNotExist(lerr) {
			return Result{}, fmt.Errorf("goalrun: resume load checkpoint: %w", lerr)
		}
	}
	if req.Resume && runID == "" {
		return Result{}, fmt.Errorf("goalrun: resume requires run_id")
	}
	_ = loadedCP
	if _, err := reg.Materialize(projectID, def, approval, now); err != nil {
		return Result{}, err
	}

	pinProv := strings.TrimSpace(req.Provider)
	pinModel := strings.TrimSpace(req.Model)
	// Legacy fixture pin path (tests / offline demos).
	fixturePin := pinProv == "fixture" || pinModel == "fixture-model"
	wantAuto := !fixturePin && (pinProv == "" || pinModel == "" ||
		strings.EqualFold(pinProv, "auto") || strings.EqualFold(pinModel, "auto"))

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
	if wantAuto {
		i, s, lerr := loadInv(ctx, req.RepoPath, now)
		if lerr != nil {
			out := Result{
				GraphID: g.GraphID, PlanDigest: g.PlanDigest,
				Status: "blocked", Message: "auto-route inventory load failed: " + lerr.Error(),
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
		if led, oerr := openLed(nowFn); oerr == nil {
			ledger = led
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

	for idx, it := range g.Items {
		depth := depthFromRoute(it.RouteRequirement, idx)
		cr := ChildReport{
			ChildID: it.ID, Intent: it.Intent, Owner: it.Owner,
			RouteRequirement: it.RouteRequirement,
			Depth:            depth,
			Stage:            "routing",
			NextAction:       "route_child",
		}

		// Exactly-once: prior succeeded child keeps claim/attempt/output/capacity;
		// no re-route, re-reserve, or re-exec.
		if prior, ok := priorSucceeded[it.ID]; ok &&
			isResumeEligible(prior.Terminal, prior.AttemptID, prior.OutputEvidence) {
			cr.Provider = prior.Provider
			cr.Model = prior.Model
			cr.Depth = firstNonEmpty(prior.Depth, depth)
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
			if cr.Provider != "" {
				usedProviders[cr.Provider] = true
			}
			childRoutes[it.ID] = workflowrun.ChildRoute{
				Provider: firstNonEmpty(cr.Provider, "fixture"), Model: firstNonEmpty(cr.Model, "fixture-model"),
				Depth: cr.Depth, AccountRef: cr.AccountRef, RouteReason: cr.RouteReason,
			}
			children = append(children, cr)
			emitChild(req.ReportOut, cr)
			continue
		}

		if !wantAuto {
			cr.Provider = firstNonEmpty(pinProv, "fixture")
			cr.Model = firstNonEmpty(pinModel, "fixture-model")
			cr.Stage = "planned"
			cr.RouteReason = "explicit_pin"
			cr.CapacityNote = "pin_path"
			cr.NextAction = "await_wave"
			childRoutes[it.ID] = workflowrun.ChildRoute{
				Provider: cr.Provider, Model: cr.Model, Depth: cr.Depth,
				RouteReason: cr.RouteReason,
			}
			children = append(children, cr)
			emitChild(req.ReportOut, cr)
			continue
		}

		// Permission from route requirement (research=read-only, implement=bounded_write).
		perm := permissionFromRoute(it.RouteRequirement, it.Owner)
		// Independent per-child auto-route (shared inventory; durable rehydrate).
		routeIn := autoroute.Input{
			AutoRoute:   true,
			Permission:  perm,
			ProjectID:   projectID,
			DecisionKey: "goalrun|" + g.GraphID + "|" + it.ID,
			Inventory:   inv,
			Effort:      depth,
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
		prov, model := res.Provider, res.Model
		// Prefer alternate provider if already used and decision lists others.
		// Candidates were already filtered to this permission+depth by autoroute.Resolve.
		if usedProviders[prov] && res.Decision != nil {
			for _, cv := range res.Decision.Candidates {
				if cv.Provider == "" || usedProviders[cv.Provider] || !cv.HardEligible || cv.SoftExcluded {
					continue
				}
				// Skip write-only providers when this child requires read-only.
				if isReadOnlyPerm(perm) && !providerSupportsReadOnly(cv.Provider) {
					continue
				}
				// CandidateView may omit Effort; required depth remains bound via reqDepth.
				prov, model = cv.Provider, cv.Model
				break
			}
		}
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
		selDepth := normalizeDepth(firstNonEmpty(res.Effort, reqDepth))
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
		winnerLine := res.Message
		if res.Explain != nil && res.Explain.WinnerLine != "" {
			winnerLine = res.Explain.WinnerLine
		}
		cr.RouteReason = fmt.Sprintf(
			"%s; permission=%s; depth requirement=%s selection=%s invocation=%s",
			winnerLine, perm, reqDepth, selDepth, effort,
		)

		if ledger != nil && snap != nil {
			attemptID := it.ID
			entry, rerr := ledger.Reserve(capacityledger.ReserveInput{
				ProjectID: projectID, RunID: runID, AttemptID: attemptID,
				Provider: prov, Model: model, Depth: effort,
				Snapshot: snap, RouteReason: cr.RouteReason,
			})
			if rerr != nil || entry.State == "refused" {
				cr.Unavailable = true
				cr.Stage = "unavailable"
				cr.Terminal = "capacity_refused"
				cr.CapacityNote = "capacity_refused"
				if rerr != nil {
					cr.RouteReason = rerr.Error()
				}
				cr.NextAction = "retry_other_route"
				children = append(children, cr)
				emitChild(req.ReportOut, cr)
				continue
			}
			b, r := entry.Before, entry.Reserved
			cr.AccountRef = entry.AccountRef
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
		} else {
			cr.CapacityNote = "ledger_unavailable"
		}

		usedProviders[prov] = true
		cr.Stage = "planned"
		cr.NextAction = "await_wave"
		childRoutes[it.ID] = workflowrun.ChildRoute{
			Provider: prov, Model: model, Depth: effort,
			AccountRef: cr.AccountRef, RouteReason: cr.RouteReason,
			// Permission is carried in route reason; executor uses ReadOnly from intent/owner.
		}
		children = append(children, cr)
		emitChild(req.ReportOut, cr)
	}

	// Dry-run route preview: do not launch children.
	if dryRun {
		out := Result{
			GraphID: g.GraphID, PlanDigest: g.PlanDigest, Children: children,
			Status: "planned", Message: "dry-run route preview; no child execution",
		}
		out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed = collectUsage(children)
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
				GraphID: g.GraphID, PlanDigest: g.PlanDigest, Children: children,
				Status: "blocked", Message: "one or more children unavailable for route/capacity",
			}
			out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed = collectUsage(children)
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
	wres, werr := svc.Execute(ctx, workflowrun.Request{
		ProjectID:      projectID,
		RunID:          runID,
		Definition:     def,
		Actor:          actor,
		Provider:       firstNonEmpty(wfProv, "auto"),
		Model:          firstNonEmpty(wfModel, "auto"),
		ChildRoutes:    childRoutes,
		RepoPath:       req.RepoPath,
		BaseRef:        firstNonEmpty(req.PRBaseRef, "main"),
		GoalBranch:     goalBranch,
		PriorSucceeded: priorSucceeded,
	})
	out := Result{
		GraphID: g.GraphID, PlanDigest: g.PlanDigest, RunID: runID, ProjectID: projectID,
		Children: children, Workflow: wres, Resumed: resumed,
	}
	out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed = collectUsage(children)
	out.MultiProviderOK = len(out.ProvidersUsed) >= 2
	out.MultiModelOrDepthOK = len(out.ModelsUsed) >= 2 || len(out.DepthsUsed) >= 2
	out.ReuseCount = wres.ReuseCount
	out.WorktreePeak = wres.WorktreePeak
	out.ProcessPeak = wres.ProcessPeak

	// Reconcile capacity from real child outcomes — never fabricate actual.
	// Then attach post-run remaining from a fresh capacity observation when possible
	// so after is never left n/a solely because token actual is unknown.
	var postSnap *capacitysnapshot.Snapshot
	if wantAuto && loadInv != nil {
		if _, s, lerr := loadInv(ctx, req.RepoPath, nowFn().UTC()); lerr == nil {
			postSnap = &s
		}
	}
	out.Children = applyChildOutcomes(out.Children, wres, ledger, holds, postSnap)

	// Always persist durable checkpoint (partial or complete) for forced restart.
	cpPath, _ := saveRunCheckpoint(req.HomeDir, projectID, runID, req.Goal, req.Issue, actor, g.GraphID, g.PlanDigest, out, wres, nowFn().UTC())
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
		// Re-save checkpoint after release notes updated.
		if p, err := saveRunCheckpoint(req.HomeDir, projectID, runID, req.Goal, req.Issue, actor, g.GraphID, g.PlanDigest, out, wres, nowFn().UTC()); err == nil {
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
		g.GraphID, g.PlanDigest, len(g.Items), wres.Status, out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed, out.MultiProviderOK,
		out.ReuseCount, out.WorktreePeak, out.ProcessPeak, out.Resumed,
	)

	// Real PR human merge gate: LoopCoder creates branch/commit/push/PR itself.
	if req.OpenPR && wres.Status == workflowrun.StatusHumanGate {
		prOpen := req.GoalPR
		if prOpen == nil {
			prOpen = goalpr.Open
		}
		// Prefer independent verifier from verify child when not pinned.
		ind := strings.TrimSpace(req.IndependentVerifier)
		verEv := strings.TrimSpace(req.VerifierEvidence)
		if ind == "" || verEv == "" {
			for _, c := range wres.Children {
				if strings.Contains(strings.ToLower(c.WorkItemID), "verify") &&
					strings.EqualFold(c.Terminal, "succeeded") {
					if ind == "" {
						ind = firstNonEmpty(c.Provider, "independent")
					}
					if verEv == "" {
						verEv = c.OutputEvidence
					}
				}
			}
		}
		if ind == "" {
			ind = "independent"
		}
		if verEv == "" {
			verEv = "goalrun:" + runID
		}
		issueN := 0
		if n, err := parseIssueNumber(req.Issue); err == nil {
			issueN = n
		}
		prRes, perr := prOpen(ctx, goalpr.Request{
			RepoPath: req.RepoPath, BaseRef: firstNonEmpty(req.PRBaseRef, "main"),
			// Head is the integrate goal branch (product commits + receipt).
			Branch:    firstNonEmpty(wres.GoalBranch, goalBranch),
			ProjectID: projectID, RunID: runID, GraphID: g.GraphID, PlanDigest: g.PlanDigest,
			SourceIssue: issueN, Actor: actor, Children: wres.Children,
			IndependentVerifier: ind, VerifierEvidence: verEv,
			RequiredCheckNames: req.RequiredCheckNames,
			Now:                nowFn,
		})
		out.PR = &prRes
		if perr != nil {
			out.Message = firstNonEmpty(out.Message, "") + "; pr_open_error=" + perr.Error()
			out.Status = "blocked"
			for _, cr := range out.Children {
				emitChild(req.ReportOut, cr)
			}
			return out, fmt.Errorf("goalrun: open pr: %w", perr)
		}
		out.HumanSummary += fmt.Sprintf(" pr=%s human_gate=true auto_merge=false", prRes.URL)
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

func saveRunCheckpoint(homeDir, projectID, runID, goal, issue, actor, graphID, planDigest string, out Result, wres workflowrun.Result, at time.Time) (string, error) {
	cp := Checkpoint{
		Schema: CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDigest, Goal: goal, Issue: issue, Actor: actor,
		Status: firstNonEmpty(out.Status, wres.Status), Message: firstNonEmpty(out.Message, wres.Message),
		Children: out.Children, WorkflowKids: wres.Children,
		WorktreePeak: wres.WorktreePeak, ProcessPeak: wres.ProcessPeak,
		ReuseCount: wres.ReuseCount, ClaimCount: wres.ClaimCount, LaunchCount: wres.LaunchCount,
		SavedAt: at,
	}
	return SaveCheckpoint(homeDir, cp)
}

func osIsNotExist(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) || os.IsNotExist(err))
}

// applyChildOutcomes merges workflow child terminal/evidence into reports and
// reconciles capacity when actual is known; otherwise releases with honest unknown.
func applyChildOutcomes(children []ChildReport, wres workflowrun.Result, ledger *capacityledger.Ledger, holds map[string]capacityHold, postSnap *capacitysnapshot.Snapshot) []ChildReport {
	byID := map[string]workflowrun.ChildOutcome{}
	for _, c := range wres.Children {
		byID[c.WorkItemID] = c
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
			if children[i].ActualSource == "" {
				children[i].ActualSource = "unknown"
			}
			// Actual stays nil — honest unknown.
		}
		// Always try fresh post-run remaining → after (source-tagged).
		// Required by #1343: after must not be n/a when observation exists.
		if children[i].CapacityAfter == nil {
			if rem, src, fr := remainingForProvider(postSnap, children[i].Provider); rem != nil {
				if entry, err := ledger.ObserveAfter(h.projectID, h.runID, h.attemptID, *rem, src, fr); err == nil {
					children[i].CapacityAfter = entry.After
					children[i].CapacityNote += "; after_source=" + src + "; after_freshness=" + fr
				}
			} else {
				children[i].CapacityNote += "; after_observation=unavailable"
			}
		}
	}
	return children
}

// remainingForProvider returns the first usable remaining fraction for provider
// from a fresh snapshot (exact/estimated). Never invents values.
func remainingForProvider(snap *capacitysnapshot.Snapshot, provider string) (rem *float64, source, freshness string) {
	if snap == nil || strings.TrimSpace(provider) == "" {
		return nil, "", ""
	}
	for _, a := range snap.Accounts {
		if !strings.EqualFold(a.Provider, provider) {
			continue
		}
		for _, w := range a.Windows {
			if f := capacitysnapshot.RemainingFraction(w); f != nil {
				src := strings.TrimSpace(w.Source)
				if src == "" {
					src = "capacity_snapshot"
				}
				return f, src, string(w.Freshness)
			}
		}
	}
	return nil, "", ""
}

func collectUsage(children []ChildReport) (providers, models, depths []string) {
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
	for p := range ps {
		providers = append(providers, p)
	}
	for m := range ms {
		models = append(models, m)
	}
	for d := range ds {
		depths = append(depths, d)
	}
	return providers, models, depths
}

func depthFromRoute(routeReq string, idx int) string {
	for _, part := range strings.Split(routeReq, ",") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 && strings.TrimSpace(kv[0]) == "depth" {
			return strings.TrimSpace(kv[1])
		}
	}
	switch idx % 3 {
	case 0:
		return "high"
	case 1:
		return "medium"
	default:
		return "low"
	}
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

// permissionFromRoute extracts permission from RouteRequirement or owner role.
// research/verify → read-only; implement/tests → bounded_write.
func permissionFromRoute(routeReq, owner string) string {
	for _, part := range strings.Split(routeReq, ",") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 && strings.TrimSpace(kv[0]) == "permission" {
			return normalizePerm(kv[1])
		}
	}
	switch strings.ToLower(strings.TrimSpace(owner)) {
	case "research", "verifier":
		return "read-only"
	default:
		return "bounded_write"
	}
}

func normalizePerm(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "readonly", "read_only", "ro", "read-only":
		return "read-only"
	case "bounded-write", "write", "workspace-write", "bounded_write":
		return "bounded_write"
	default:
		if p == "" {
			return "bounded_write"
		}
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
	case "codex", "claude", "gemini", "fixture":
		return true
	default:
		return true
	}
}
