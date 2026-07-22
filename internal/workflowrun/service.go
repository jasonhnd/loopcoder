package workflowrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/waveschedule"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

const (
	StatusHumanGate = "human_gate"
	StatusBlocked   = "workflow_blocked"
	StatusInvalid   = "workflow_invalid"
)

// Request executes one bounded workflow.
type Request struct {
	ProjectID string
	// Definition is the frozen user graph (JSON-serializable).
	Definition workflowdef.Definition
	// Actor is the approving owner identity (required for materialize).
	Actor string
	// Provider/Model optional default child route pin when ChildRoutes lacks an item.
	Provider string
	Model    string
	// ChildRoutes optional per-work-item routes (goalrun auto-route winners).
	ChildRoutes map[string]ChildRoute
	// RepoPath optional base repo for child worktrees.
	RepoPath string
	// MaxWaves hard cap (default 32).
	MaxWaves int
}

// ChildOutcome is per-child terminal + capacity-relevant evidence for reports.
type ChildOutcome struct {
	WorkItemID     string   `json:"work_item_id"`
	Provider       string   `json:"provider,omitempty"`
	Model          string   `json:"model,omitempty"`
	Depth          string   `json:"depth,omitempty"`
	AccountRef     string   `json:"account_ref,omitempty"`
	RouteReason    string   `json:"route_reason,omitempty"`
	Terminal       string   `json:"terminal,omitempty"`
	OutputEvidence string   `json:"output_evidence,omitempty"`
	WorktreePath   string   `json:"worktree_path,omitempty"`
	AttemptID      string   `json:"attempt_id,omitempty"`
	ExitCode       int      `json:"exit_code,omitempty"`
	FailureClass   string   `json:"failure_class,omitempty"`
	Message        string   `json:"message,omitempty"`
	ActualCapacity *float64 `json:"actual_capacity,omitempty"`
	ActualSource   string   `json:"actual_source,omitempty"`
	FilesTouched   []string `json:"files_touched,omitempty"`
}

// Result is durable parent evidence.
type Result struct {
	Status         string
	Message        string
	GraphID        string
	PlanDigest     string
	GraphVersion   int
	ClaimCount     int
	LaunchCount    int // child launches (== claims on success path)
	Integrated     []string
	Children       []ChildOutcome
	Events         []string
	DirectRunEquiv bool
	AutoMerge      bool
	Error          string
}

// Service runs bounded workflows.
type Service struct {
	Now func() time.Time
	// Executor runs claimed children. nil → DefaultChildExecutor (production real path).
	// Focused tests inject FakeChildExecutor; remote acceptance must use production.
	Executor ChildExecutor
	// HomeDir overrides layout for child worktrees (tests).
	HomeDir string
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
	emit := func(e string) { out.Events = append(out.Events, e) }

	if ctx.Err() != nil {
		return fail(out, StatusBlocked, "context cancelled")
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		projectID = "local-project"
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "owner"
	}
	defaultProvider := strings.TrimSpace(req.Provider)
	if defaultProvider == "" {
		defaultProvider = "fixture"
	}
	defaultModel := strings.TrimSpace(req.Model)
	if defaultModel == "" {
		defaultModel = "fixture-model"
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
	out.PlanDigest = plan.Digest
	emit("plan.ok:" + short(plan.Digest))

	// --- approve + materialize ---
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
	out.DirectRunEquiv = g.DirectRunEquivalent
	emit(fmt.Sprintf("materialize.ok items=%d equiv=%v", len(g.Items), g.DirectRunEquivalent))

	// --- schedule + claim + real child execute each ready item once ---
	cs := workclaim.NewStore(now)
	ev := workgraph.TerminalEvidence{}
	claimed := map[string]int{}
	launches := 0
	integrated := []string{}
	itemByID := map[string]workgraph.WorkItem{}
	for _, it := range g.Items {
		itemByID[it.ID] = it
	}

	bounds := waveschedule.DefaultBounds()
	for wave := 0; wave < maxWaves; wave++ {
		if ctx.Err() != nil {
			return fail(out, StatusBlocked, "cancelled mid-wave")
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
			attemptID := "att-" + id + "-" + short(out.PlanDigest)
			res, err := cs.Claim(workclaim.ClaimRequest{
				ProjectID: projectID, Graph: g, Evidence: ev, WorkItemID: id,
				AttemptID:  attemptID,
				ExecutorID: "workflowrun", Lease: time.Minute,
			})
			if err != nil || res.Code != workclaim.ResultClaimed {
				return fail(out, StatusBlocked, fmt.Sprintf("claim %s: %v code=%v", id, err, res.Code))
			}
			claimed[id]++
			out.ClaimCount++
			launches++
			out.LaunchCount = launches

			route := resolveChildRoute(req.ChildRoutes, id, defaultProvider, defaultModel)
			emit(fmt.Sprintf("child.launch:%s route=%s/%s depth=%s", id, route.Provider, route.Model, route.Depth))

			it := itemByID[id]
			readOnly := strings.Contains(strings.ToLower(it.RouteRequirement), "read-only") ||
				strings.Contains(strings.ToLower(it.RouteRequirement), "readonly") ||
				strings.EqualFold(it.Owner, "research")

			childIn := ChildExecInput{
				ProjectID: projectID, GraphID: g.GraphID, WorkItemID: id,
				ClaimID: res.Claim.ClaimID, AttemptID: attemptID,
				Intent: it.Intent, Route: route, RepoPath: req.RepoPath,
				ReadOnly: readOnly,
			}
			childOut, cerr := exec.Execute(ctx, childIn)
			outcome := ChildOutcome{
				WorkItemID: id, Provider: firstNonEmpty(childOut.Provider, route.Provider),
				Model:      firstNonEmpty(childOut.Model, route.Model),
				Depth:      firstNonEmpty(childOut.Depth, route.Depth),
				AccountRef: route.AccountRef, RouteReason: route.RouteReason,
				AttemptID: attemptID, WorktreePath: childOut.WorktreePath,
				OutputEvidence: childOut.OutputEvidence, ExitCode: childOut.ExitCode,
				FailureClass: childOut.FailureClass, Message: childOut.Message,
				ActualCapacity: childOut.ActualCapacity, ActualSource: childOut.ActualSource,
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
			// Context cancel mid-child → cancelled terminal + release path for callers.
			if ctx.Err() != nil && term != workgraph.TermSucceeded {
				term = workgraph.TermCancelled
				if outcome.FailureClass == "" {
					outcome.FailureClass = "cancelled"
				}
			}
			outcome.Terminal = string(term)

			// Close claim only with real output evidence on success; failure/cancel
			// still close the claim so capacity/wave recovery can proceed idempotently.
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

			_, closeErr := cs.Close(workclaim.CloseRequest{
				ClaimID: res.Claim.ClaimID, Generation: res.Claim.Generation,
				ExecutorID: "workflowrun", AttemptID: res.Claim.AttemptID,
				Terminal: closeTerm, OutputEvidence: closeEvidence,
			})
			if closeErr != nil {
				outcome.Message = "close: " + closeErr.Error()
				out.Children = append(out.Children, outcome)
				return fail(out, StatusBlocked, "close "+id+": "+closeErr.Error())
			}
			ev[id] = closeTerm
			out.Children = append(out.Children, outcome)
			emit(fmt.Sprintf("child.terminal:%s term=%s evidence=%s", id, closeTerm, short(closeEvidence)))

			// Required child failure blocks the parent (do not pretend human_gate success).
			if it.Status == workgraph.ItemRequired && closeTerm != workgraph.TermSucceeded {
				msg := fmt.Sprintf("required child %s terminal=%s", id, closeTerm)
				if outcome.Message != "" {
					msg += ": " + outcome.Message
				}
				return fail(out, StatusBlocked, msg)
			}
			if cerr != nil && it.Status == workgraph.ItemRequired {
				return fail(out, StatusBlocked, "required child "+id+": "+cerr.Error())
			}
		}
	}

	// --- integration order ---
	order := workgraph.IntegrationOrder(g)
	for _, id := range order {
		if term, ok := ev[id]; ok && term == workgraph.TermSucceeded {
			integrated = append(integrated, id)
			emit("integrate:" + id)
		}
	}
	out.Integrated = integrated

	// claim-once guarantee
	for id, n := range claimed {
		if n != 1 {
			return fail(out, StatusBlocked, fmt.Sprintf("item %s claimed %d times", id, n))
		}
	}
	if out.LaunchCount != out.ClaimCount {
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
	return out, nil
}

func resolveChildRoute(routes map[string]ChildRoute, id, defProv, defModel string) ChildRoute {
	if routes != nil {
		if r, ok := routes[id]; ok {
			if strings.TrimSpace(r.Provider) == "" {
				r.Provider = defProv
			}
			if strings.TrimSpace(r.Model) == "" {
				r.Model = defModel
			}
			if strings.TrimSpace(r.Depth) == "" {
				r.Depth = "medium"
			}
			return r
		}
	}
	return ChildRoute{Provider: defProv, Model: defModel, Depth: "medium", RouteReason: "default_pin"}
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
			{ID: "only", Intent: intent, Status: "required", IntegrationOrder: 1},
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
			{ID: "a", Intent: "A", Status: "required", IntegrationOrder: 1},
			{ID: "b", Intent: "B", Status: "required", IntegrationOrder: 2},
			{ID: "c", Intent: "C", Status: "required", IntegrationOrder: 3},
		},
		Deps: []workflowdef.DefDep{
			{From: "a", To: "b", Kind: "finish_to_start"},
			{From: "b", To: "c", Kind: "finish_to_start"},
		},
	}
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

func fail(out Result, status, msg string) (Result, error) {
	out.Status = status
	out.Message = msg
	out.Error = msg
	return out, fmt.Errorf("workflowrun: %s", msg)
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
