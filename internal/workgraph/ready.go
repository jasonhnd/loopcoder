package workgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	// SchemaReady is the ready-set evaluation envelope (V090-058).
	SchemaReady = "loopcoder.workgraph.ready.v1"
	// ReadyPolicyVersion is the versioned readiness rule identity.
	ReadyPolicyVersion = "workgraph-ready-v1"
)

// ItemLifecycle is the computed readiness state for one WorkItem.
type ItemLifecycle string

const (
	// LifeReady: nonterminal; all hard required deps satisfied.
	LifeReady ItemLifecycle = "ready"
	// LifeBlocked: nonterminal; waiting on hard deps.
	LifeBlocked ItemLifecycle = "blocked"
	// LifeTerminal: already finished (succeeded/failed/cancelled/skipped).
	LifeTerminal ItemLifecycle = "terminal"
	// LifeIgnored: optional item skipped because a required path failed, or
	// explicitly marked skipped and not blocking others.
	LifeIgnored ItemLifecycle = "ignored"
)

// Hard dependency kinds gate readiness (soft_order does not).
func hardDep(k DependencyKind) bool {
	return k == DepFinishToStart || k == DepOutput
}

// TerminalEvidence maps stable WorkItem ID → accepted terminal state from
// durable events (overlays graph payload terminals when set).
type TerminalEvidence map[string]TerminalState

// ItemState is one item's auditable readiness row.
type ItemState struct {
	ID       string        `json:"id"`
	Status   ItemStatus    `json:"status"`
	Terminal TerminalState `json:"terminal,omitempty"`
	Life     ItemLifecycle `json:"lifecycle"`
	// IntegrationOrder for stable ready ordering.
	IntegrationOrder int      `json:"integration_order"`
	Reasons          []string `json:"reasons"`
}

// ReadyResult is the pure readiness evaluation for one immutable graph version.
type ReadyResult struct {
	Schema        string `json:"schema"`
	PolicyVersion string `json:"policy_version"`
	GraphID       string `json:"graph_id"`
	GraphVersion  int    `json:"graph_version"`
	PlanDigest    string `json:"plan_digest"`
	// Ready IDs in deterministic order (integration_order, then id).
	Ready []string `json:"ready"`
	// All item states keyed for explain; ordered list for stable digest.
	Items []ItemState `json:"items"`
	// Valid is false when the graph fails executable validation.
	Valid  bool     `json:"valid"`
	Errors []string `json:"errors,omitempty"`
	Digest string   `json:"digest"`
}

// ValidateExecutable performs full pre-execution validation (V090-058).
// On failure it returns a multi-error list; callers must not materialize any
// partial executable graph, claim, worker, branch, or PR.
func ValidateExecutable(g Graph) error {
	// Base contract first.
	if err := ValidateGraph(g); err != nil {
		return err
	}
	return validateExecutableExtras(g)
}

func validateExecutableExtras(g Graph) error {
	lim := g.Limits
	if lim.MaxItems <= 0 {
		lim = DefaultLimits()
	}
	if len(g.Items) > lim.MaxItems {
		return fmt.Errorf("%w: node count %d > max_items %d", ErrLimits, len(g.Items), lim.MaxItems)
	}

	// Duplicate edges (same from/to/kind) and conflicting multi-kind same from→to for hard deps.
	type edgeKey struct{ from, to, kind string }
	seen := map[edgeKey]struct{}{}
	hardPairs := map[string]DependencyKind{} // from\to → first hard kind
	for _, d := range g.Dependencies {
		ek := edgeKey{d.From, d.To, string(d.Kind)}
		if _, ok := seen[ek]; ok {
			return fmt.Errorf("%w: duplicate edge %s->%s kind=%s", ErrInvalid, d.From, d.To, d.Kind)
		}
		seen[ek] = struct{}{}
		if hardDep(d.Kind) {
			pair := d.From + "\x00" + d.To
			if prev, ok := hardPairs[pair]; ok && prev != d.Kind {
				// finish_to_start + output on same pair is allowed as complementary;
				// only reject true conflicts: same kind already handled; allow both FTS and Output.
				// Conflicting means two identical hard kinds (already duplicate) — no-op here.
				_ = prev
			} else {
				hardPairs[pair] = d.Kind
			}
		}
	}

	// Self-loop already rejected in ValidateGraph; re-check for clarity.
	for _, d := range g.Dependencies {
		if d.From == d.To {
			return fmt.Errorf("%w: self-loop %s", ErrInvalid, d.From)
		}
	}

	// Depth (longest hard-dep path) and fan-out limits.
	ids := make([]string, 0, len(g.Items))
	for _, it := range g.Items {
		ids = append(ids, it.ID)
	}
	adj := hardAdj(g.Dependencies)
	// fan-out: hard successors count
	for _, id := range ids {
		if lim.MaxParallel > 0 && len(adj[id]) > lim.MaxParallel {
			// MaxParallel is concurrent fan-out bound for ready scheduling; use as fan-out cap when set.
			// Documented: fan-out per node limited by MaxParallel as a conservative bound.
			return fmt.Errorf("%w: fan-out of %s is %d > max_parallel %d", ErrLimits, id, len(adj[id]), lim.MaxParallel)
		}
	}
	if lim.MaxDepth > 0 {
		depth := maxHardDepth(ids, adj)
		if depth > lim.MaxDepth {
			return fmt.Errorf("%w: depth %d > max_depth %d", ErrLimits, depth, lim.MaxDepth)
		}
	}

	// Output integration order: if DepOutput, producer should integrate before consumer
	// (integration_order producer < consumer) when both have orders.
	order := map[string]int{}
	for _, it := range g.Items {
		order[it.ID] = it.IntegrationOrder
	}
	for _, d := range g.Dependencies {
		if d.Kind != DepOutput {
			continue
		}
		if order[d.From] >= order[d.To] {
			return fmt.Errorf("%w: output dep %s->%s requires producer integration_order < consumer", ErrInvalid, d.From, d.To)
		}
	}

	// Invalid required/optional: optional item cannot be sole predecessor of a
	// required item via hard dep (required cannot be gated only by optional).
	// More precisely: every hard predecessor of a required item must itself be required.
	status := map[string]ItemStatus{}
	for _, it := range g.Items {
		status[it.ID] = it.Status
	}
	// reverse hard edges: to <- from
	for _, d := range g.Dependencies {
		if !hardDep(d.Kind) {
			continue
		}
		if status[d.To] == ItemRequired && status[d.From] == ItemOptional {
			return fmt.Errorf("%w: required item %s hard-depends on optional %s", ErrInvalid, d.To, d.From)
		}
	}
	return nil
}

func hardAdj(deps []Dependency) map[string][]string {
	adj := map[string][]string{}
	for _, d := range deps {
		if !hardDep(d.Kind) {
			continue
		}
		adj[d.From] = append(adj[d.From], d.To)
	}
	for k := range adj {
		sort.Strings(adj[k])
	}
	return adj
}

func maxHardDepth(ids []string, adj map[string][]string) int {
	memo := map[string]int{}
	var dfs func(string, map[string]bool) int
	dfs = func(u string, stack map[string]bool) int {
		if v, ok := memo[u]; ok {
			return v
		}
		if stack[u] {
			return 0 // cycle handled elsewhere
		}
		stack[u] = true
		best := 1
		for _, v := range adj[u] {
			d := dfs(v, stack) + 1
			if d > best {
				best = d
			}
		}
		delete(stack, u)
		memo[u] = best
		return best
	}
	max := 0
	for _, id := range ids {
		d := dfs(id, map[string]bool{})
		if d > max {
			max = d
		}
	}
	return max
}

// EvaluateReady is a pure function: immutable graph + terminal evidence → ready set.
// Invalid graphs return Valid=false with errors and empty Ready (no side effects).
func EvaluateReady(g Graph, evidence TerminalEvidence) ReadyResult {
	res := ReadyResult{
		Schema:        SchemaReady,
		PolicyVersion: ReadyPolicyVersion,
		GraphID:       g.GraphID,
		GraphVersion:  g.Version,
		PlanDigest:    g.PlanDigest,
		Items:         []ItemState{},
		Ready:         []string{},
	}
	if res.PlanDigest == "" {
		res.PlanDigest = DigestGraph(g)
	}

	if err := ValidateExecutable(g); err != nil {
		res.Valid = false
		res.Errors = []string{err.Error()}
		res.Digest = digestReady(res)
		return res
	}
	res.Valid = true

	// Effective terminals: evidence overrides graph field when present.
	term := map[string]TerminalState{}
	status := map[string]ItemStatus{}
	order := map[string]int{}
	for _, it := range g.Items {
		status[it.ID] = it.Status
		order[it.ID] = it.IntegrationOrder
		t := it.Terminal
		if evidence != nil {
			if et, ok := evidence[it.ID]; ok && et != TermNone {
				t = et
			}
		}
		term[it.ID] = t
	}

	// Hard predecessors for each item.
	preds := map[string][]Dependency{}
	for _, d := range g.Dependencies {
		if !hardDep(d.Kind) {
			continue
		}
		preds[d.To] = append(preds[d.To], d)
	}
	for k := range preds {
		sort.Slice(preds[k], func(i, j int) bool {
			if preds[k][i].From != preds[k][j].From {
				return preds[k][i].From < preds[k][j].From
			}
			return preds[k][i].Kind < preds[k][j].Kind
		})
	}

	// Optional items that failed/cancelled may be ignored; required failure blocks dependents.
	states := make([]ItemState, 0, len(g.Items))
	ready := []string{}

	for _, it := range g.Items {
		st := ItemState{
			ID: it.ID, Status: it.Status, Terminal: term[it.ID],
			IntegrationOrder: it.IntegrationOrder, Reasons: []string{},
		}
		t := term[it.ID]
		if t != TermNone {
			st.Life = LifeTerminal
			st.Reasons = append(st.Reasons, "terminal."+string(t))
			// Optional failed → also mark ignored semantics for dependents
			if it.Status == ItemOptional && (t == TermFailed || t == TermCancelled || t == TermSkipped) {
				st.Reasons = append(st.Reasons, "optional.non_success")
			}
			states = append(states, st)
			continue
		}

		// Nonterminal: check hard deps
		blocked := false
		ignore := false
		for _, d := range preds[it.ID] {
			pt := term[d.From]
			ps := status[d.From]
			switch {
			case pt == TermNone:
				blocked = true
				st.Reasons = append(st.Reasons, "waiting."+d.From+"."+string(d.Kind))
			case pt == TermSucceeded:
				st.Reasons = append(st.Reasons, "satisfied."+d.From+"."+string(d.Kind))
			case ps == ItemOptional && (pt == TermFailed || pt == TermCancelled || pt == TermSkipped):
				// Optional pred non-success: for hard deps this is invalid graph config
				// (ValidateExecutable rejects required←optional). If we still see it on optional→optional:
				if it.Status == ItemOptional {
					ignore = true
					st.Reasons = append(st.Reasons, "optional_pred_failed."+d.From)
				} else {
					blocked = true
					st.Reasons = append(st.Reasons, "required_blocked_by_failed."+d.From)
				}
			default:
				// required pred failed/cancelled/skipped → block dependent forever (no auto-ignore required)
				blocked = true
				st.Reasons = append(st.Reasons, "dep_failed."+d.From+"."+string(pt))
			}
		}

		switch {
		case ignore && !blocked:
			st.Life = LifeIgnored
			st.Reasons = append(st.Reasons, "ignored.optional_path")
		case blocked:
			st.Life = LifeBlocked
		default:
			st.Life = LifeReady
			st.Reasons = append(st.Reasons, "ready")
			ready = append(ready, it.ID)
		}
		states = append(states, st)
	}

	// Stable order: integration_order then id
	sort.SliceStable(states, func(i, j int) bool {
		if states[i].IntegrationOrder != states[j].IntegrationOrder {
			return states[i].IntegrationOrder < states[j].IntegrationOrder
		}
		return states[i].ID < states[j].ID
	})
	sort.SliceStable(ready, func(i, j int) bool {
		if order[ready[i]] != order[ready[j]] {
			return order[ready[i]] < order[ready[j]]
		}
		return ready[i] < ready[j]
	})

	res.Items = states
	res.Ready = ready
	res.Digest = digestReady(res)
	return res
}

// MaterializeIfValid validates then returns the graph only if executable.
// Invalid plans produce no partial graph (caller must not persist).
func MaterializeIfValid(g Graph) (Graph, error) {
	if err := ValidateExecutable(g); err != nil {
		return Graph{}, err
	}
	if g.PlanDigest == "" {
		g.PlanDigest = DigestGraph(g)
	}
	return g, nil
}

func digestReady(r ReadyResult) string {
	type wire struct {
		Schema        string      `json:"schema"`
		PolicyVersion string      `json:"policy_version"`
		GraphID       string      `json:"graph_id"`
		GraphVersion  int         `json:"graph_version"`
		PlanDigest    string      `json:"plan_digest"`
		Ready         []string    `json:"ready"`
		Items         []ItemState `json:"items"`
		Valid         bool        `json:"valid"`
		Errors        []string    `json:"errors,omitempty"`
	}
	w := wire{
		Schema: r.Schema, PolicyVersion: r.PolicyVersion,
		GraphID: r.GraphID, GraphVersion: r.GraphVersion, PlanDigest: r.PlanDigest,
		Ready: r.Ready, Items: r.Items, Valid: r.Valid, Errors: r.Errors,
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "sha256:error"
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

// ReadyContains reports whether id is in the ready set.
func ReadyContains(r ReadyResult, id string) bool {
	id = strings.TrimSpace(id)
	for _, x := range r.Ready {
		if x == id {
			return true
		}
	}
	return false
}
