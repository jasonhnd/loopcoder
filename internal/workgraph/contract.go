package workgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	SchemaGraph     = "loopcoder.workgraph.v1"
	SchemaItem      = "loopcoder.workitem.v1"
	SchemaDep       = "loopcoder.workgraph.dependency.v1"
	SchemaLimits    = "loopcoder.workgraph.limits.v1"
	SchemaReplan    = "loopcoder.workgraph.replan.v1"
	ContractVersion = "workgraph-contract-v1"
)

// DependencyKind is how one WorkItem depends on another.
type DependencyKind string

const (
	// DepFinishToStart: successor may start only after predecessor terminal success.
	DepFinishToStart DependencyKind = "finish_to_start"
	// DepOutput: successor consumes predecessor output artifact.
	DepOutput DependencyKind = "output"
	// DepSoft: ordering preference only; not a hard gate.
	DepSoft DependencyKind = "soft_order"
)

// ItemStatus is required vs optional in the graph.
type ItemStatus string

const (
	ItemRequired ItemStatus = "required"
	ItemOptional ItemStatus = "optional"
)

// TerminalState of a WorkItem (history immutable once set to a terminal).
type TerminalState string

const (
	TermNone      TerminalState = ""
	TermSucceeded TerminalState = "succeeded"
	TermFailed    TerminalState = "failed"
	TermCancelled TerminalState = "cancelled"
	TermSkipped   TerminalState = "skipped"
)

// SourceKind is how the graph was authored. Forbidden sources are rejected.
type SourceKind string

const (
	SourceExplicitYAML      SourceKind = "explicit_definition"
	SourceOwnerApproved     SourceKind = "owner_approved"
	SourceDirectMaterialize SourceKind = "direct_run_materialize"
	// Forbidden:
	SourceRoadmapCompile SourceKind = "roadmap_compile" // rejected
	SourceSyntheticEpic  SourceKind = "synthetic_epic"  // rejected
	SourceSelfBootstrap  SourceKind = "self_bootstrap"  // rejected
)

// Ownership distinguishes LoopCoder objects from external / provider-native ones.
type Ownership string

const (
	// OwnLoopCoderWorkItem is a LoopCoder WorkItem (this contract).
	OwnLoopCoderWorkItem Ownership = "loopcoder_workitem"
	// OwnAttempt is a LoopCoder route attempt (not a WorkItem).
	OwnAttempt Ownership = "loopcoder_attempt"
	// OwnProviderNativeChild is a provider-native session/process (not a WorkItem).
	OwnProviderNativeChild Ownership = "provider_native_child"
	// OwnGitHubIssuePR is a GitHub issue/PR reference (not a WorkItem).
	OwnGitHubIssuePR Ownership = "github_issue_or_pr"
)

// WorkItem is one node in the Work Graph.
type WorkItem struct {
	Schema string `json:"schema"`
	// ID stable within the graph (not a GitHub number).
	ID string `json:"id"`
	// Intent human summary of the unit of work.
	Intent string     `json:"intent"`
	Status ItemStatus `json:"status"`
	// Owner is the LoopCoder role/actor responsible (not provider child).
	Owner string `json:"owner"`
	// Ownership must be OwnLoopCoderWorkItem for graph nodes.
	Ownership Ownership `json:"ownership"`
	// RouteRequirement opaque digest/class pin for routing (filled by later issues).
	RouteRequirement string `json:"route_requirement,omitempty"`
	// OutputContract describes expected outputs (paths, PR, etc.) without executing.
	OutputContract string `json:"output_contract,omitempty"`
	// IntegrationOrder is the visible merge/integration sequence rank (1-based).
	IntegrationOrder int `json:"integration_order"`
	// Terminal is empty until finished; once set, history cannot rewrite it.
	Terminal TerminalState `json:"terminal,omitempty"`
	// AttemptID links to a LoopCoder attempt when execution started (optional).
	AttemptID string `json:"attempt_id,omitempty"`
	// ProviderChildRef is explicitly NOT a WorkItem id when present.
	ProviderChildRef string `json:"provider_child_ref,omitempty"`
	// GitHubRef optional issue/PR number as reference only.
	GitHubRef string `json:"github_ref,omitempty"`
}

// Dependency is an edge between WorkItems.
type Dependency struct {
	Schema string         `json:"schema"`
	From   string         `json:"from"` // predecessor WorkItem ID
	To     string         `json:"to"`   // successor WorkItem ID
	Kind   DependencyKind `json:"kind"`
}

// Limits bound workflow size and concurrency.
type Limits struct {
	Schema             string `json:"schema"`
	MaxItems           int    `json:"max_items"`
	MaxDepth           int    `json:"max_depth"`
	MaxParallel        int    `json:"max_parallel"`
	MaxAutomaticReplan int    `json:"max_automatic_replan"`
}

// DefaultLimits returns conservative defaults.
func DefaultLimits() Limits {
	return Limits{Schema: SchemaLimits, MaxItems: 32, MaxDepth: 8, MaxParallel: 4, MaxAutomaticReplan: 1}
}

// Graph is the versioned workflow definition.
type Graph struct {
	Schema          string `json:"schema"`
	ContractVersion string `json:"contract_version"`
	// GraphID identity of this definition version.
	GraphID string `json:"graph_id"`
	// Version increments on replan; completed history retains prior digests.
	Version int `json:"version"`
	// Source of the graph (forbidden sources rejected).
	Source SourceKind `json:"source"`
	// ExplicitOptIn must be true for multi-node workflows.
	ExplicitOptIn bool `json:"explicit_opt_in"`
	// ApprovedBy non-empty for multi-node before any child starts.
	ApprovedBy string `json:"approved_by,omitempty"`
	// DirectRunEquivalent when true: one-node graph ≡ direct path.
	DirectRunEquivalent bool         `json:"direct_run_equivalent"`
	Items               []WorkItem   `json:"items"`
	Dependencies        []Dependency `json:"dependencies"`
	Limits              Limits       `json:"limits"`
	// ExecutionStarted when true, mutations require Replan.
	ExecutionStarted bool      `json:"execution_started"`
	PlanDigest       string    `json:"plan_digest"`
	CreatedAt        time.Time `json:"created_at"`
}

// Replan is a versioned successor graph definition (does not rewrite history).
type Replan struct {
	Schema       string `json:"schema"`
	PriorGraphID string `json:"prior_graph_id"`
	PriorVersion int    `json:"prior_version"`
	PriorDigest  string `json:"prior_digest"`
	// New graph version body.
	Next Graph `json:"next"`
	// Reason and actor required.
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

var (
	ErrInvalid   = errors.New("workgraph: invalid")
	ErrForbidden = errors.New("workgraph: forbidden source")
	ErrLimits    = errors.New("workgraph: limits")
	ErrMutation  = errors.New("workgraph: mutation requires replan")
	ErrNotEquiv  = errors.New("workgraph: not direct-run equivalent")
)

// ForbiddenSource reports whether s is rejected as a workflow source.
func ForbiddenSource(s SourceKind) bool {
	switch s {
	case SourceRoadmapCompile, SourceSyntheticEpic, SourceSelfBootstrap:
		return true
	}
	return false
}

// ValidateGraph checks contract rules (golden-friendly, no execution).
func ValidateGraph(g Graph) error {
	if g.Schema != "" && g.Schema != SchemaGraph {
		return fmt.Errorf("%w: schema", ErrInvalid)
	}
	if g.ContractVersion != "" && g.ContractVersion != ContractVersion {
		return fmt.Errorf("%w: contract version", ErrInvalid)
	}
	if ForbiddenSource(g.Source) {
		return fmt.Errorf("%w: %s", ErrForbidden, g.Source)
	}
	if g.Source == "" {
		return fmt.Errorf("%w: source required", ErrInvalid)
	}
	if g.Version < 1 {
		return fmt.Errorf("%w: version", ErrInvalid)
	}
	if strings.TrimSpace(g.GraphID) == "" {
		return fmt.Errorf("%w: graph_id", ErrInvalid)
	}
	lim := g.Limits
	if lim.MaxItems <= 0 {
		lim = DefaultLimits()
	}
	if len(g.Items) == 0 {
		return fmt.Errorf("%w: empty items", ErrInvalid)
	}
	if len(g.Items) > lim.MaxItems {
		return fmt.Errorf("%w: max items", ErrLimits)
	}
	// Multi-node requires explicit opt-in + approval before children.
	if len(g.Items) > 1 {
		if !g.ExplicitOptIn {
			return fmt.Errorf("%w: multi-node requires explicit_opt_in", ErrInvalid)
		}
		if strings.TrimSpace(g.ApprovedBy) == "" {
			return fmt.Errorf("%w: multi-node requires approved_by before children", ErrInvalid)
		}
		if g.DirectRunEquivalent {
			return fmt.Errorf("%w: multi-node cannot claim direct_run_equivalent", ErrInvalid)
		}
	}
	// One-node may be direct-run equivalent.
	if len(g.Items) == 1 && g.DirectRunEquivalent {
		// no extra provider call by contract: no deps, single required item
		if len(g.Dependencies) != 0 {
			return fmt.Errorf("%w: one-node equivalent forbids dependencies", ErrNotEquiv)
		}
	}

	ids := map[string]struct{}{}
	orders := map[int]string{}
	for _, it := range g.Items {
		if err := validateItem(it); err != nil {
			return err
		}
		if _, ok := ids[it.ID]; ok {
			return fmt.Errorf("%w: duplicate item %s", ErrInvalid, it.ID)
		}
		ids[it.ID] = struct{}{}
		if it.IntegrationOrder < 1 {
			return fmt.Errorf("%w: integration_order for %s", ErrInvalid, it.ID)
		}
		if prev, ok := orders[it.IntegrationOrder]; ok {
			return fmt.Errorf("%w: duplicate integration_order %d (%s,%s)", ErrInvalid, it.IntegrationOrder, prev, it.ID)
		}
		orders[it.IntegrationOrder] = it.ID
	}
	for _, d := range g.Dependencies {
		if d.Schema != "" && d.Schema != SchemaDep {
			return fmt.Errorf("%w: dep schema", ErrInvalid)
		}
		if _, ok := ids[d.From]; !ok {
			return fmt.Errorf("%w: dep from unknown %s", ErrInvalid, d.From)
		}
		if _, ok := ids[d.To]; !ok {
			return fmt.Errorf("%w: dep to unknown %s", ErrInvalid, d.To)
		}
		if d.From == d.To {
			return fmt.Errorf("%w: self dependency", ErrInvalid)
		}
		switch d.Kind {
		case DepFinishToStart, DepOutput, DepSoft:
		default:
			return fmt.Errorf("%w: dep kind", ErrInvalid)
		}
	}
	// cycle check on hard deps
	if hasCycle(g.Items, g.Dependencies) {
		return fmt.Errorf("%w: dependency cycle", ErrInvalid)
	}
	return nil
}

func validateItem(it WorkItem) error {
	if strings.TrimSpace(it.ID) == "" || strings.TrimSpace(it.Intent) == "" {
		return fmt.Errorf("%w: item id/intent", ErrInvalid)
	}
	if it.Status != ItemRequired && it.Status != ItemOptional {
		return fmt.Errorf("%w: item status", ErrInvalid)
	}
	if it.Ownership != OwnLoopCoderWorkItem {
		return fmt.Errorf("%w: workitem ownership must be %s (got %s)", ErrInvalid, OwnLoopCoderWorkItem, it.Ownership)
	}
	if strings.TrimSpace(it.Owner) == "" {
		return fmt.Errorf("%w: owner", ErrInvalid)
	}
	switch it.Terminal {
	case TermNone, TermSucceeded, TermFailed, TermCancelled, TermSkipped:
	default:
		return fmt.Errorf("%w: terminal", ErrInvalid)
	}
	// Provider child and GitHub refs are references only — not ownership identity.
	return nil
}

// MaterializeDirectRun builds a one-node graph equivalent to a direct run.
// No extra provider call is introduced by materialization itself.
func MaterializeDirectRun(graphID, intent, owner string, now time.Time) (Graph, error) {
	if strings.TrimSpace(intent) == "" || strings.TrimSpace(owner) == "" {
		return Graph{}, fmt.Errorf("%w: intent and owner", ErrInvalid)
	}
	g := Graph{
		Schema: SchemaGraph, ContractVersion: ContractVersion,
		GraphID: strings.TrimSpace(graphID), Version: 1,
		Source: SourceDirectMaterialize, ExplicitOptIn: false,
		DirectRunEquivalent: true,
		Items: []WorkItem{{
			Schema: SchemaItem, ID: "wi_main", Intent: intent, Status: ItemRequired,
			Owner: owner, Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 1,
		}},
		Limits: DefaultLimits(), CreatedAt: now.UTC(),
	}
	if g.GraphID == "" {
		g.GraphID = "g_direct_" + shortHash(intent+"|"+owner)
	}
	g.PlanDigest = DigestGraph(g)
	if err := ValidateGraph(g); err != nil {
		return Graph{}, err
	}
	return g, nil
}

// DigestGraph returns a stable plan digest (excludes CreatedAt wall noise by
// using fixed fields only — caller should set CreatedAt before digest if included).
func DigestGraph(g Graph) string {
	// Normalize for stability: sort items by id, deps by from/to.
	type wire struct {
		Schema          string       `json:"schema"`
		ContractVersion string       `json:"contract_version"`
		GraphID         string       `json:"graph_id"`
		Version         int          `json:"version"`
		Source          SourceKind   `json:"source"`
		ExplicitOptIn   bool         `json:"explicit_opt_in"`
		ApprovedBy      string       `json:"approved_by,omitempty"`
		DirectRun       bool         `json:"direct_run_equivalent"`
		Items           []WorkItem   `json:"items"`
		Dependencies    []Dependency `json:"dependencies"`
		Limits          Limits       `json:"limits"`
	}
	items := append([]WorkItem{}, g.Items...)
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	deps := append([]Dependency{}, g.Dependencies...)
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].From != deps[j].From {
			return deps[i].From < deps[j].From
		}
		return deps[i].To < deps[j].To
	})
	w := wire{
		Schema: SchemaGraph, ContractVersion: ContractVersion,
		GraphID: g.GraphID, Version: g.Version, Source: g.Source,
		ExplicitOptIn: g.ExplicitOptIn, ApprovedBy: g.ApprovedBy,
		DirectRun: g.DirectRunEquivalent, Items: items, Dependencies: deps, Limits: g.Limits,
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "sha256:error"
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

// ApplyReplan validates a replan and returns the next graph version.
// Completed item terminals from prior cannot be rewritten.
func ApplyReplan(prior Graph, rp Replan) (Graph, error) {
	if prior.ExecutionStarted && strings.TrimSpace(rp.Actor) == "" {
		return Graph{}, fmt.Errorf("%w: actor required after execution started", ErrMutation)
	}
	if strings.TrimSpace(rp.Reason) == "" {
		return Graph{}, fmt.Errorf("%w: replan reason", ErrInvalid)
	}
	if rp.PriorGraphID != prior.GraphID || rp.PriorVersion != prior.Version {
		return Graph{}, fmt.Errorf("%w: replan prior mismatch", ErrInvalid)
	}
	if rp.PriorDigest != "" && rp.PriorDigest != prior.PlanDigest {
		return Graph{}, fmt.Errorf("%w: prior digest mismatch", ErrInvalid)
	}
	next := rp.Next
	next.GraphID = prior.GraphID
	next.Version = prior.Version + 1
	next.Schema = SchemaGraph
	next.ContractVersion = ContractVersion
	// Preserve completed terminals from prior items with same ID.
	priorTerm := map[string]TerminalState{}
	for _, it := range prior.Items {
		if it.Terminal != TermNone {
			priorTerm[it.ID] = it.Terminal
		}
	}
	for i := range next.Items {
		if t, ok := priorTerm[next.Items[i].ID]; ok {
			if next.Items[i].Terminal != TermNone && next.Items[i].Terminal != t {
				return Graph{}, fmt.Errorf("%w: cannot rewrite completed history for %s", ErrMutation, next.Items[i].ID)
			}
			next.Items[i].Terminal = t
		}
	}
	if err := ValidateGraph(next); err != nil {
		return Graph{}, err
	}
	next.PlanDigest = DigestGraph(next)
	return next, nil
}

// IntegrationOrder returns item IDs sorted by IntegrationOrder (visible sequence).
func IntegrationOrder(g Graph) []string {
	items := append([]WorkItem{}, g.Items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].IntegrationOrder != items[j].IntegrationOrder {
			return items[i].IntegrationOrder < items[j].IntegrationOrder
		}
		return items[i].ID < items[j].ID
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

// OwnershipLabels documents the unambiguous ownership vocabulary.
func OwnershipLabels() map[Ownership]string {
	return map[Ownership]string{
		OwnLoopCoderWorkItem:   "LoopCoder WorkItem node in the Work Graph",
		OwnAttempt:             "LoopCoder route Attempt (not a WorkItem)",
		OwnProviderNativeChild: "Provider-native child session/process (not a WorkItem)",
		OwnGitHubIssuePR:       "GitHub issue/PR reference (not a WorkItem)",
	}
}

func hasCycle(items []WorkItem, deps []Dependency) bool {
	// only hard edges
	adj := map[string][]string{}
	for _, it := range items {
		adj[it.ID] = nil
	}
	for _, d := range deps {
		if d.Kind == DepSoft {
			continue
		}
		adj[d.From] = append(adj[d.From], d.To)
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var dfs func(string) bool
	dfs = func(u string) bool {
		color[u] = gray
		for _, v := range adj[u] {
			switch color[v] {
			case gray:
				return true
			case white:
				if dfs(v) {
					return true
				}
			}
		}
		color[u] = black
		return false
	}
	for _, it := range items {
		if color[it.ID] == white {
			if dfs(it.ID) {
				return true
			}
		}
	}
	return false
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:10]
}
