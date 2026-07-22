package workflowdef

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
	"gopkg.in/yaml.v3"
)

const (
	SchemaDefinition  = "loopcoder.workflow.definition.v1"
	SchemaPlan        = "loopcoder.workflow.plan.v1"
	SchemaApproval    = "loopcoder.workflow.approval.v1"
	SchemaMaterialize = "loopcoder.workflow.materialize.v1"
	InputVersion      = 1
)

// Definition is the user-authored YAML/JSON input (pre-normalization).
type Definition struct {
	SchemaVersion int    `json:"schema_version" yaml:"schema_version"`
	GraphID       string `json:"graph_id,omitempty" yaml:"graph_id,omitempty"`
	// Source must be explicit; forbidden sources rejected.
	Source string    `json:"source,omitempty" yaml:"source,omitempty"`
	Items  []DefItem `json:"items" yaml:"items"`
	Deps   []DefDep  `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	// Limits optional; defaults applied on normalize.
	MaxItems    int `json:"max_items,omitempty" yaml:"max_items,omitempty"`
	MaxDepth    int `json:"max_depth,omitempty" yaml:"max_depth,omitempty"`
	MaxParallel int `json:"max_parallel,omitempty" yaml:"max_parallel,omitempty"`
}

// DefItem is one WorkItem in the input.
type DefItem struct {
	ID               string `json:"id" yaml:"id"`
	Intent           string `json:"intent" yaml:"intent"`
	Status           string `json:"status,omitempty" yaml:"status,omitempty"` // required|optional
	Owner            string `json:"owner,omitempty" yaml:"owner,omitempty"`
	RouteRequirement string `json:"route_requirement,omitempty" yaml:"route_requirement,omitempty"`
	OutputContract   string `json:"output_contract,omitempty" yaml:"output_contract,omitempty"`
	IntegrationOrder int    `json:"integration_order,omitempty" yaml:"integration_order,omitempty"`
}

// DefDep is one dependency edge in the input.
type DefDep struct {
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty"` // finish_to_start|output|soft_order
}

// Plan is the normalized dry-run plan (byte-stable JSON).
type Plan struct {
	Schema       string           `json:"schema"`
	GraphID      string           `json:"graph_id"`
	Digest       string           `json:"digest"`
	Source       string           `json:"source"`
	Items        []DefItem        `json:"items"`
	Dependencies []DefDep         `json:"dependencies"`
	Limits       workgraph.Limits `json:"limits"`
	Integration  []string         `json:"integration_order"`
	HumanSummary string           `json:"human_summary"`
}

// Approval binds an actor to an exact plan digest.
type Approval struct {
	Schema     string    `json:"schema"`
	Digest     string    `json:"digest"`
	Actor      string    `json:"actor"`
	Reason     string    `json:"reason,omitempty"`
	ApprovedAt time.Time `json:"approved_at"`
}

// MaterializeResult is the outcome of materialization.
type MaterializeResult struct {
	Schema     string          `json:"schema"`
	Graph      workgraph.Graph `json:"graph"`
	Digest     string          `json:"digest"`
	Idempotent bool            `json:"idempotent"`
}

var (
	ErrInvalid   = errors.New("workflowdef: invalid")
	ErrForbidden = errors.New("workflowdef: forbidden source")
	ErrApproval  = errors.New("workflowdef: approval")
	ErrConflict  = errors.New("workflowdef: conflict")
)

// Forbidden input markers that must never create WorkItems.
var forbiddenMarkers = []string{
	"ROADMAP.md",
	"roadmap_compile",
	"synthetic_epic",
	"self_bootstrap",
	"github_epic",
	"auto_split",
	"model_generated_graph",
}

// ParseJSON parses a JSON definition.
func ParseJSON(raw []byte) (Definition, error) {
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return Definition{}, fmt.Errorf("%w: json: %v", ErrInvalid, err)
	}
	return d, nil
}

// ParseYAML parses a YAML definition.
func ParseYAML(raw []byte) (Definition, error) {
	var d Definition
	if err := yaml.Unmarshal(raw, &d); err != nil {
		return Definition{}, fmt.Errorf("%w: yaml: %v", ErrInvalid, err)
	}
	return d, nil
}

// Normalize validates and produces a byte-stable Plan (dry-run, nonmutating).
func Normalize(d Definition) (Plan, error) {
	if d.SchemaVersion != 0 && d.SchemaVersion != InputVersion {
		return Plan{}, fmt.Errorf("%w: schema_version path=schema_version want=%d got=%d", ErrInvalid, InputVersion, d.SchemaVersion)
	}
	src := strings.TrimSpace(d.Source)
	if src == "" {
		src = string(workgraph.SourceExplicitYAML)
	}
	if workgraph.ForbiddenSource(workgraph.SourceKind(src)) {
		return Plan{}, fmt.Errorf("%w: source=%s", ErrForbidden, src)
	}
	for _, m := range forbiddenMarkers {
		if strings.EqualFold(src, m) || strings.Contains(strings.ToLower(src), strings.ToLower(m)) {
			return Plan{}, fmt.Errorf("%w: source marker %s", ErrForbidden, m)
		}
	}
	if len(d.Items) == 0 {
		return Plan{}, fmt.Errorf("%w: path=items reason=empty", ErrInvalid)
	}

	items := append([]DefItem{}, d.Items...)
	// Assign default integration orders if missing: stable by id order then 1..n
	needOrder := false
	for _, it := range items {
		if it.IntegrationOrder <= 0 {
			needOrder = true
			break
		}
	}
	if needOrder {
		sort.SliceStable(items, func(i, j int) bool {
			return strings.TrimSpace(items[i].ID) < strings.TrimSpace(items[j].ID)
		})
		for i := range items {
			items[i].IntegrationOrder = i + 1
		}
	}
	// Normalize fields
	seen := map[string]struct{}{}
	for i := range items {
		items[i].ID = strings.TrimSpace(items[i].ID)
		items[i].Intent = strings.TrimSpace(items[i].Intent)
		items[i].Owner = strings.TrimSpace(items[i].Owner)
		if items[i].Owner == "" {
			items[i].Owner = "worker"
		}
		st := strings.ToLower(strings.TrimSpace(items[i].Status))
		if st == "" {
			st = "required"
		}
		if st != "required" && st != "optional" {
			return Plan{}, fmt.Errorf("%w: path=items[%d].status reason=invalid", ErrInvalid, i)
		}
		items[i].Status = st
		if items[i].ID == "" || items[i].Intent == "" {
			return Plan{}, fmt.Errorf("%w: path=items[%d] reason=id_or_intent_required", ErrInvalid, i)
		}
		if _, ok := seen[items[i].ID]; ok {
			return Plan{}, fmt.Errorf("%w: path=items[%d].id reason=duplicate", ErrInvalid, i)
		}
		seen[items[i].ID] = struct{}{}
	}
	// Stable sort by integration_order then id for digest
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].IntegrationOrder != items[j].IntegrationOrder {
			return items[i].IntegrationOrder < items[j].IntegrationOrder
		}
		return items[i].ID < items[j].ID
	})

	deps := append([]DefDep{}, d.Deps...)
	for i := range deps {
		deps[i].From = strings.TrimSpace(deps[i].From)
		deps[i].To = strings.TrimSpace(deps[i].To)
		k := strings.ToLower(strings.TrimSpace(deps[i].Kind))
		if k == "" {
			k = string(workgraph.DepFinishToStart)
		}
		switch k {
		case string(workgraph.DepFinishToStart), string(workgraph.DepOutput), string(workgraph.DepSoft):
		default:
			return Plan{}, fmt.Errorf("%w: path=dependencies[%d].kind reason=invalid", ErrInvalid, i)
		}
		deps[i].Kind = k
		if _, ok := seen[deps[i].From]; !ok {
			return Plan{}, fmt.Errorf("%w: path=dependencies[%d].from reason=missing_endpoint", ErrInvalid, i)
		}
		if _, ok := seen[deps[i].To]; !ok {
			return Plan{}, fmt.Errorf("%w: path=dependencies[%d].to reason=missing_endpoint", ErrInvalid, i)
		}
	}
	sort.SliceStable(deps, func(i, j int) bool {
		if deps[i].From != deps[j].From {
			return deps[i].From < deps[j].From
		}
		if deps[i].To != deps[j].To {
			return deps[i].To < deps[j].To
		}
		return deps[i].Kind < deps[j].Kind
	})

	lim := workgraph.DefaultLimits()
	if d.MaxItems > 0 {
		lim.MaxItems = d.MaxItems
	}
	if d.MaxDepth > 0 {
		lim.MaxDepth = d.MaxDepth
	}
	if d.MaxParallel > 0 {
		lim.MaxParallel = d.MaxParallel
	}

	graphID := strings.TrimSpace(d.GraphID)
	if graphID == "" {
		graphID = "g_wf_" + short(items[0].ID)
	}

	// Build workgraph for executable validation
	g, err := toGraph(graphID, src, items, deps, lim)
	if err != nil {
		return Plan{}, err
	}
	if err := workgraph.ValidateExecutable(g); err != nil {
		return Plan{}, fmt.Errorf("%w: graph: %v", ErrInvalid, err)
	}

	integ := workgraph.IntegrationOrder(g)
	p := Plan{
		Schema: SchemaPlan, GraphID: graphID, Source: src,
		Items: items, Dependencies: deps, Limits: lim, Integration: integ,
	}
	p.Digest = digestPlan(p)
	p.HumanSummary = humanPlan(p)
	return p, nil
}

// DryRunJSON returns byte-stable plan JSON (dry-run, nonmutating).
func DryRunJSON(d Definition) ([]byte, Plan, error) {
	p, err := Normalize(d)
	if err != nil {
		return nil, Plan{}, err
	}
	// Marshal without HumanSummary for byte stability of digest wire? Human is derived.
	// Acceptance: repeated dry-run is byte-stable — include full plan with sorted fields.
	b, err := json.Marshal(p)
	if err != nil {
		return nil, Plan{}, err
	}
	return b, p, nil
}

// Approve records approval for an exact digest.
func Approve(digest, actor, reason string, now time.Time) (Approval, error) {
	if strings.TrimSpace(digest) == "" || strings.TrimSpace(actor) == "" {
		return Approval{}, fmt.Errorf("%w: digest and actor required", ErrApproval)
	}
	return Approval{
		Schema: SchemaApproval, Digest: strings.TrimSpace(digest),
		Actor: strings.TrimSpace(actor), Reason: strings.TrimSpace(reason),
		ApprovedAt: now.UTC(),
	}, nil
}

// Registry holds approved digests and materialized graphs (in-memory).
type Registry struct {
	mu       sync.Mutex
	approved map[string]Approval        // digest → approval
	graphs   map[string]workgraph.Graph // project|graph|version
	byDigest map[string]workgraph.Graph // plan digest → graph
}

// NewRegistry creates an empty materialization registry.
func NewRegistry() *Registry {
	return &Registry{
		approved: map[string]Approval{},
		graphs:   map[string]workgraph.Graph{},
		byDigest: map[string]workgraph.Graph{},
	}
}

// RecordApproval stores an approval for a digest.
func (r *Registry) RecordApproval(a Approval) error {
	if a.Digest == "" || a.Actor == "" {
		return fmt.Errorf("%w: incomplete", ErrApproval)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.approved[a.Digest] = a
	return nil
}

// Materialize creates one immutable graph version if approval matches plan digest.
// Idempotent: same digest returns existing graph.
func (r *Registry) Materialize(projectID string, d Definition, approval Approval, now time.Time) (MaterializeResult, error) {
	plan, err := Normalize(d)
	if err != nil {
		return MaterializeResult{}, err
	}
	if approval.Digest != plan.Digest {
		return MaterializeResult{}, fmt.Errorf("%w: digest mismatch plan=%s approval=%s", ErrApproval, plan.Digest, approval.Digest)
	}
	if strings.TrimSpace(approval.Actor) == "" {
		return MaterializeResult{}, fmt.Errorf("%w: actor required", ErrApproval)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Approval must be recorded or match provided
	if ap, ok := r.approved[plan.Digest]; ok {
		if ap.Digest != approval.Digest {
			return MaterializeResult{}, fmt.Errorf("%w: stored approval mismatch", ErrApproval)
		}
	} else {
		r.approved[plan.Digest] = approval
	}

	if g, ok := r.byDigest[plan.Digest]; ok {
		return MaterializeResult{Schema: SchemaMaterialize, Graph: g, Digest: plan.Digest, Idempotent: true}, nil
	}

	g, err := toGraph(plan.GraphID, plan.Source, plan.Items, plan.Dependencies, plan.Limits)
	if err != nil {
		return MaterializeResult{}, err
	}
	g.ApprovedBy = approval.Actor
	g.ExplicitOptIn = len(g.Items) > 1
	g.CreatedAt = now.UTC()
	g.PlanDigest = workgraph.DigestGraph(g)
	// Note: workgraph digest may differ from plan digest slightly if ApprovedBy is in wire;
	// we store plan digest as materialization key and keep graph digest for graph identity.
	if err := workgraph.ValidateExecutable(g); err != nil {
		return MaterializeResult{}, err
	}

	key := projectID + "|" + g.GraphID + "|" + fmt.Sprintf("%d", g.Version)
	r.graphs[key] = g
	r.byDigest[plan.Digest] = g
	return MaterializeResult{Schema: SchemaMaterialize, Graph: g, Digest: plan.Digest, Idempotent: false}, nil
}

// RejectImplicitSources documents that these cannot create WorkItems.
func RejectImplicitSources(kind string) error {
	k := strings.ToLower(strings.TrimSpace(kind))
	for _, m := range forbiddenMarkers {
		if k == strings.ToLower(m) || strings.Contains(k, strings.ToLower(m)) {
			return fmt.Errorf("%w: %s cannot create WorkItems", ErrForbidden, kind)
		}
	}
	return fmt.Errorf("%w: unknown implicit source %s", ErrForbidden, kind)
}

func toGraph(graphID, source string, items []DefItem, deps []DefDep, lim workgraph.Limits) (workgraph.Graph, error) {
	gItems := make([]workgraph.WorkItem, 0, len(items))
	for _, it := range items {
		st := workgraph.ItemRequired
		if it.Status == "optional" {
			st = workgraph.ItemOptional
		}
		gItems = append(gItems, workgraph.WorkItem{
			Schema: workgraph.SchemaItem, ID: it.ID, Intent: it.Intent, Status: st,
			Owner: it.Owner, Ownership: workgraph.OwnLoopCoderWorkItem,
			RouteRequirement: it.RouteRequirement, OutputContract: it.OutputContract,
			IntegrationOrder: it.IntegrationOrder,
		})
	}
	gDeps := make([]workgraph.Dependency, 0, len(deps))
	for _, d := range deps {
		gDeps = append(gDeps, workgraph.Dependency{
			Schema: workgraph.SchemaDep, From: d.From, To: d.To, Kind: workgraph.DependencyKind(d.Kind),
		})
	}
	src := workgraph.SourceKind(source)
	if src == "" {
		src = workgraph.SourceExplicitYAML
	}
	multi := len(gItems) > 1
	g := workgraph.Graph{
		Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
		GraphID: graphID, Version: 1, Source: src,
		ExplicitOptIn: multi, DirectRunEquivalent: len(gItems) == 1 && len(gDeps) == 0,
		Items: gItems, Dependencies: gDeps, Limits: lim,
	}
	// Dry-run / pre-approval structural validation needs multi-node approval
	// placeholder; Materialize overwrites with the real actor.
	if multi {
		g.ApprovedBy = "pending_approval"
	}
	g.PlanDigest = workgraph.DigestGraph(g)
	return g, nil
}

func digestPlan(p Plan) string {
	type wire struct {
		Schema       string           `json:"schema"`
		GraphID      string           `json:"graph_id"`
		Source       string           `json:"source"`
		Items        []DefItem        `json:"items"`
		Dependencies []DefDep         `json:"dependencies"`
		Limits       workgraph.Limits `json:"limits"`
		Integration  []string         `json:"integration_order"`
	}
	w := wire{
		Schema: SchemaPlan, GraphID: p.GraphID, Source: p.Source,
		Items: p.Items, Dependencies: p.Dependencies, Limits: p.Limits, Integration: p.Integration,
	}
	b, _ := json.Marshal(w)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

func humanPlan(p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow plan %s digest=%s\n", p.GraphID, p.Digest)
	fmt.Fprintf(&b, "Source: %s\n", p.Source)
	fmt.Fprintf(&b, "Items (%d):\n", len(p.Items))
	for _, it := range p.Items {
		fmt.Fprintf(&b, "  [%d] %s (%s) owner=%s — %s\n", it.IntegrationOrder, it.ID, it.Status, it.Owner, it.Intent)
	}
	if len(p.Dependencies) > 0 {
		fmt.Fprintln(&b, "Dependencies:")
		for _, d := range p.Dependencies {
			fmt.Fprintf(&b, "  %s -[%s]-> %s\n", d.From, d.Kind, d.To)
		}
	}
	fmt.Fprintf(&b, "Limits: items=%d depth=%d parallel=%d\n", p.Limits.MaxItems, p.Limits.MaxDepth, p.Limits.MaxParallel)
	return b.String()
}

func short(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}
