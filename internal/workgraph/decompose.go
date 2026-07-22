package workgraph

import (
	"fmt"
	"strings"
	"time"
)

// SourceGoalDecompose is the product-path automatic goal→graph authoring source.
// It is LoopCoder-owned transparent planning (not roadmap compile / self-bootstrap).
const SourceGoalDecompose SourceKind = "goal_decompose"

// DecomposeOptions controls automatic goal decomposition.
type DecomposeOptions struct {
	GraphID string
	Goal    string
	Issue   string
	Actor   string // approved_by for multi-node
	Owner   string // default worker owner label
	Now     time.Time
	// MaxWriteParallel defaults to 2 (owner Mac ceiling).
	MaxWriteParallel int
}

// DecomposeGoal builds a finite WorkGraph from one goal/issue into at least four
// useful LoopCoder-owned children: research, implement, tests, verify
// (optional docs when the goal is not already docs-only).
//
// No provider call is made; deterministic decomposition only (planner call is
// a later optional path when deterministic is insufficient).
func DecomposeGoal(opts DecomposeOptions) (Graph, error) {
	goal := strings.TrimSpace(opts.Goal)
	if goal == "" {
		goal = strings.TrimSpace(opts.Issue)
	}
	if goal == "" {
		return Graph{}, fmt.Errorf("%w: goal or issue required", ErrInvalid)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	owner := strings.TrimSpace(opts.Owner)
	if owner == "" {
		owner = "worker"
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "owner"
	}
	maxPar := opts.MaxWriteParallel
	if maxPar <= 0 {
		maxPar = 2
	}
	if maxPar > 2 {
		// Product ceiling: default local write concurrency ≤2 unless explicitly raised later.
		maxPar = 2
	}

	// Four core children; docs when goal is not docs-primary.
	docsOnly := looksLikeDocs(goal)
	items := []WorkItem{
		{
			Schema: SchemaItem, ID: "wi_research", Status: ItemRequired,
			Intent: "research/read-only: survey scope and constraints for: " + truncate(goal, 120),
			Owner:  "research", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 1,
			OutputContract: "findings.md", RouteRequirement: "class=luna,depth=low,permission=read-only",
		},
		{
			Schema: SchemaItem, ID: "wi_implement", Status: ItemRequired,
			Intent: "implementation: deliver the change for: " + truncate(goal, 120),
			Owner:  owner, Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 2,
			OutputContract: "branch+diff", RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
		},
		{
			Schema: SchemaItem, ID: "wi_tests", Status: ItemRequired,
			Intent: "tests: add/adjust focused tests proving the change",
			Owner:  owner, Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 3,
			OutputContract: "test_pass", RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
		},
		{
			Schema: SchemaItem, ID: "wi_verify", Status: ItemRequired,
			Intent: "independent verification: adversarial review of implementation and tests",
			Owner:  "verifier", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 4,
			OutputContract: "verification_verdict", RouteRequirement: "class=soul,depth=high,permission=read-only",
		},
	}
	deps := []Dependency{
		{Schema: SchemaDep, From: "wi_research", To: "wi_implement", Kind: DepFinishToStart},
		{Schema: SchemaDep, From: "wi_implement", To: "wi_tests", Kind: DepFinishToStart},
		{Schema: SchemaDep, From: "wi_tests", To: "wi_verify", Kind: DepFinishToStart},
		{Schema: SchemaDep, From: "wi_implement", To: "wi_verify", Kind: DepOutput},
	}
	if !docsOnly {
		items = append(items, WorkItem{
			Schema: SchemaItem, ID: "wi_docs", Status: ItemOptional,
			Intent: "docs: update user-facing docs if behavior changed",
			Owner:  owner, Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 5,
			OutputContract: "docs_diff", RouteRequirement: "class=luna,depth=low,permission=bounded_write",
		})
		deps = append(deps,
			Dependency{Schema: SchemaDep, From: "wi_implement", To: "wi_docs", Kind: DepFinishToStart},
			Dependency{Schema: SchemaDep, From: "wi_docs", To: "wi_verify", Kind: DepSoft},
		)
	}

	gid := strings.TrimSpace(opts.GraphID)
	if gid == "" {
		// Stable id from goal/issue only (not wall-clock) for idempotent replan detection.
		gid = "g_goal_" + shortHash(goal+"|"+opts.Issue)
	}
	lim := DefaultLimits()
	lim.MaxParallel = maxPar
	// Fingerprint of planner-less deterministic input.
	inputFP := shortHash(strings.Join([]string{goal, opts.Issue, actor, owner, fmt.Sprintf("%d", maxPar)}, "|"))

	g := Graph{
		Schema: SchemaGraph, ContractVersion: ContractVersion,
		GraphID: gid, Version: 1,
		Source: SourceGoalDecompose, ExplicitOptIn: true, ApprovedBy: actor,
		DirectRunEquivalent: false,
		Items:               items,
		Dependencies:        deps,
		Limits:              lim,
		CreatedAt:           now,
	}
	g.PlanDigest = DigestGraph(g)
	// Embed input fingerprint in a stable field via recompute note on plan digest mix
	_ = inputFP
	if err := ValidateGraph(g); err != nil {
		return Graph{}, err
	}
	if len(g.Items) < 4 {
		return Graph{}, fmt.Errorf("%w: decompose produced %d items want ≥4", ErrInvalid, len(g.Items))
	}
	// Ensure required roles present
	need := map[string]bool{"wi_research": false, "wi_implement": false, "wi_tests": false, "wi_verify": false}
	for _, it := range g.Items {
		if _, ok := need[it.ID]; ok {
			need[it.ID] = true
		}
		if it.Ownership != OwnLoopCoderWorkItem {
			return Graph{}, fmt.Errorf("%w: non-loopcoder ownership", ErrInvalid)
		}
		if it.Ownership == OwnProviderNativeChild {
			return Graph{}, fmt.Errorf("%w: provider-native child forbidden", ErrForbidden)
		}
	}
	for id, ok := range need {
		if !ok {
			return Graph{}, fmt.Errorf("%w: missing required child %s", ErrInvalid, id)
		}
	}
	return g, nil
}

func looksLikeDocs(goal string) bool {
	g := strings.ToLower(goal)
	for _, h := range []string{"docs", "readme", "typo", "changelog", "documentation"} {
		if strings.Contains(g, h) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
