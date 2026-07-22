package goalrun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

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
	Provider  string
	Model     string
	ReportOut io.Writer
	Now       func() time.Time
}

// ChildReport is one transparent child line for UI/JSONL.
type ChildReport struct {
	ChildID          string `json:"child_id"`
	Intent           string `json:"intent"`
	Owner            string `json:"owner"`
	RouteRequirement string `json:"route_requirement"`
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	Depth            string `json:"depth,omitempty"`
	Stage            string `json:"stage"`
	CapacityNote     string `json:"capacity_note,omitempty"`
	Terminal         string `json:"terminal,omitempty"`
	NextAction       string `json:"next_action,omitempty"`
}

// Result is parent evidence after bounded child execution.
type Result struct {
	Status       string             `json:"status"`
	GraphID      string             `json:"graph_id"`
	PlanDigest   string             `json:"plan_digest"`
	Children     []ChildReport      `json:"children"`
	Workflow     workflowrun.Result `json:"workflow"`
	Message      string             `json:"message"`
	HumanSummary string             `json:"human_summary"`
}

// Execute decomposes the goal, materializes an approved graph, runs bounded
// workflow children, and emits transparent reports (no provider-native agents).
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
	if _, err := reg.Materialize(strings.TrimSpace(req.ProjectID), def, approval, now); err != nil {
		return Result{}, err
	}

	children := make([]ChildReport, 0, len(g.Items))
	for _, it := range g.Items {
		prov, model, depth := parseRouteReq(it.RouteRequirement, req.Provider, req.Model)
		cr := ChildReport{
			ChildID: it.ID, Intent: it.Intent, Owner: it.Owner,
			RouteRequirement: it.RouteRequirement,
			Provider:         prov, Model: model, Depth: depth,
			Stage: "planned", NextAction: "await_wave",
			CapacityNote: "reserve_at_child_launch",
		}
		children = append(children, cr)
		emitChild(req.ReportOut, cr)
	}

	svc := workflowrun.Service{Now: nowFn}
	wres, werr := svc.Execute(ctx, workflowrun.Request{
		ProjectID:  req.ProjectID,
		Definition: def,
		Actor:      actor,
		Provider:   firstNonEmpty(req.Provider, "fixture"),
		Model:      firstNonEmpty(req.Model, "fixture-model"),
	})
	out := Result{
		GraphID: g.GraphID, PlanDigest: g.PlanDigest, Children: children, Workflow: wres,
	}
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
	out.HumanSummary = fmt.Sprintf("goal graph %s digest=%s children=%d status=%s",
		g.GraphID, g.PlanDigest, len(g.Items), wres.Status)
	out.Children = stampTerminals(out.Children, wres)
	for _, cr := range out.Children {
		emitChild(req.ReportOut, cr)
	}
	return out, nil
}

func stampTerminals(children []ChildReport, wres workflowrun.Result) []ChildReport {
	done := map[string]bool{}
	for _, id := range wres.Integrated {
		done[id] = true
	}
	for i := range children {
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

func parseRouteReq(req, pinProv, pinModel string) (provider, model, depth string) {
	provider, model, depth = pinProv, pinModel, "medium"
	for _, part := range strings.Split(req, ",") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		if strings.TrimSpace(kv[0]) == "depth" {
			depth = strings.TrimSpace(kv[1])
		}
	}
	if provider == "" {
		provider = "auto"
	}
	if model == "" {
		model = "auto"
	}
	return provider, model, depth
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
