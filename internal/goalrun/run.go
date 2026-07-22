package goalrun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// Request is one product-path goal execution.
type Request struct {
	ProjectID string
	Goal      string
	Issue     string
	Actor     string
	Owner     string
	// Provider/Model: empty or "auto" → per-child bare auto-route with durable
	// capacity rehydrate. Explicit pin (including "fixture") skips auto-route.
	Provider string
	Model    string
	// RepoPath for live inventory discover (optional).
	RepoPath string
	// DryRun when true only plans routes and releases capacity without executing
	// children (route preview). Default false → real child execution.
	DryRun *bool
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
	Children            []ChildReport      `json:"children"`
	Workflow            workflowrun.Result `json:"workflow"`
	Message             string             `json:"message"`
	HumanSummary        string             `json:"human_summary"`
	ProvidersUsed       []string           `json:"providers_used,omitempty"`
	ModelsUsed          []string           `json:"models_used,omitempty"`
	DepthsUsed          []string           `json:"depths_used,omitempty"`
	MultiProviderOK     bool               `json:"multi_provider_ok"`
	MultiModelOrDepthOK bool               `json:"multi_model_or_depth_ok"`
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
	if projectID == "" {
		projectID = "local-project"
	}
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

	runID := "goalrun_" + shortID(g.GraphID)
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
	wres, werr := svc.Execute(ctx, workflowrun.Request{
		ProjectID:   projectID,
		Definition:  def,
		Actor:       actor,
		Provider:    firstNonEmpty(wfProv, "auto"),
		Model:       firstNonEmpty(wfModel, "auto"),
		ChildRoutes: childRoutes,
		RepoPath:    req.RepoPath,
	})
	out := Result{
		GraphID: g.GraphID, PlanDigest: g.PlanDigest, Children: children, Workflow: wres,
	}
	out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed = collectUsage(children)
	out.MultiProviderOK = len(out.ProvidersUsed) >= 2
	out.MultiModelOrDepthOK = len(out.ModelsUsed) >= 2 || len(out.DepthsUsed) >= 2

	// Reconcile capacity from real child outcomes — never fabricate actual.
	out.Children = applyChildOutcomes(out.Children, wres, ledger, holds)

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
		for _, cr := range out.Children {
			emitChild(req.ReportOut, cr)
		}
		return out, werr
	}
	out.Status = wres.Status
	out.Message = wres.Message
	out.HumanSummary = fmt.Sprintf(
		"goal graph %s digest=%s children=%d status=%s providers=%v models=%v depths=%v multi_provider=%v",
		g.GraphID, g.PlanDigest, len(g.Items), wres.Status, out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed, out.MultiProviderOK,
	)
	for _, cr := range out.Children {
		emitChild(req.ReportOut, cr)
	}
	return out, nil
}

// applyChildOutcomes merges workflow child terminal/evidence into reports and
// reconciles capacity when actual is known; otherwise releases with honest unknown.
func applyChildOutcomes(children []ChildReport, wres workflowrun.Result, ledger *capacityledger.Ledger, holds map[string]capacityHold) []ChildReport {
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
	}
	return children
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
