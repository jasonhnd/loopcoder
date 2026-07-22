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
	// DryRun releases capacity after reserve (default true for bounded goal wave).
	DryRun *bool
	// LoadInventory injects inventory for tests.
	LoadInventory func(ctx context.Context, repo string, now time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error)
	// OpenLedger injects ledger for tests.
	OpenLedger func(now func() time.Time) (*capacityledger.Ledger, error)
	ReportOut  io.Writer
	Now        func() time.Time
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

// Execute decomposes the goal, routes each LoopCoder-owned child independently
// via bare auto-route + durable capacity rehydrate when not pinned, records
// capacity ledger lines, runs the bounded workflow graph to human gate, and
// emits transparent reports (no provider-native subagents).
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

	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}

	usedProviders := map[string]bool{}
	children := make([]ChildReport, 0, len(g.Items))
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
			children = append(children, cr)
			emitChild(req.ReportOut, cr)
			continue
		}

		// Independent per-child auto-route (shared inventory; durable rehydrate).
		routeIn := autoroute.Input{
			AutoRoute:   true,
			Permission:  "bounded_write",
			ProjectID:   projectID,
			DecisionKey: "goalrun|" + g.GraphID + "|" + it.ID,
			Inventory:   inv,
			Effort:      depth,
			Now:         now,
		}
		// After first child, prefer alternate provider when soft ranking still
		// leaves other eligible candidates (multi-provider acceptance).
		if len(usedProviders) > 0 {
			// Soft bias via re-resolve then pick alternate from decision candidates.
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

		prov, model, effort := res.Provider, res.Model, firstNonEmpty(res.Effort, depth, "medium")
		// Prefer alternate provider if already used and decision lists others.
		if usedProviders[prov] && res.Decision != nil {
			for _, cv := range res.Decision.Candidates {
				if cv.Provider == "" || usedProviders[cv.Provider] || !cv.HardEligible || cv.SoftExcluded {
					continue
				}
				prov, model = cv.Provider, cv.Model
				break
			}
		}
		cr.Provider = prov
		cr.Model = model
		cr.Depth = effort
		cr.RouteReason = res.Message
		if res.Explain != nil && res.Explain.WinnerLine != "" {
			cr.RouteReason = res.Explain.WinnerLine
		}

		if ledger != nil && snap != nil {
			runID := "goalrun_" + shortID(g.GraphID)
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
				if rel, lerr := ledger.Release(projectID, runID, attemptID, "goalrun_child_wave"); lerr == nil {
					cr.CapacityState = rel.State
					cr.CapacityNote += "; released=child_wave"
					cr.RouteReason = rel.RouteReason
				}
			}
		} else {
			cr.CapacityNote = "ledger_unavailable"
		}

		usedProviders[prov] = true
		cr.Stage = "planned"
		cr.NextAction = "await_wave"
		children = append(children, cr)
		emitChild(req.ReportOut, cr)
	}

	wfProv, wfModel := pinProv, pinModel
	for _, c := range children {
		if c.Provider != "" && !c.Unavailable {
			wfProv, wfModel = c.Provider, c.Model
			break
		}
	}
	svc := workflowrun.Service{Now: nowFn}
	wres, werr := svc.Execute(ctx, workflowrun.Request{
		ProjectID:  projectID,
		Definition: def,
		Actor:      actor,
		Provider:   firstNonEmpty(wfProv, "auto"),
		Model:      firstNonEmpty(wfModel, "auto"),
	})
	out := Result{
		GraphID: g.GraphID, PlanDigest: g.PlanDigest, Children: children, Workflow: wres,
	}
	out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed = collectUsage(children)
	out.MultiProviderOK = len(out.ProvidersUsed) >= 2
	out.MultiModelOrDepthOK = len(out.ModelsUsed) >= 2 || len(out.DepthsUsed) >= 2
	if werr != nil {
		out.Status = "blocked"
		out.Message = werr.Error()
		if wres.Error != "" {
			out.Message = wres.Error
		}
		out.Children = stampTerminals(out.Children, wres)
		return out, werr
	}
	out.Status = wres.Status
	out.Message = wres.Message
	out.HumanSummary = fmt.Sprintf(
		"goal graph %s digest=%s children=%d status=%s providers=%v models=%v depths=%v multi_provider=%v",
		g.GraphID, g.PlanDigest, len(g.Items), wres.Status, out.ProvidersUsed, out.ModelsUsed, out.DepthsUsed, out.MultiProviderOK,
	)
	out.Children = stampTerminals(out.Children, wres)
	for _, cr := range out.Children {
		emitChild(req.ReportOut, cr)
	}
	return out, nil
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

func stampTerminals(children []ChildReport, wres workflowrun.Result) []ChildReport {
	done := map[string]bool{}
	for _, id := range wres.Integrated {
		done[id] = true
	}
	for i := range children {
		if children[i].Unavailable {
			continue
		}
		if done[children[i].ChildID] {
			children[i].Stage = "integrated"
			children[i].Terminal = "succeeded"
			children[i].NextAction = "parent_continue"
		} else if wres.Status == workflowrun.StatusHumanGate {
			children[i].Stage = "human_gate"
			children[i].NextAction = "owner_merge"
		}
	}
	return children
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
