package workflowdef

import (
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// FromGraph converts a validated WorkGraph into a workflow Definition for materialize/run.
func FromGraph(g workgraph.Graph) (Definition, error) {
	if err := workgraph.ValidateGraph(g); err != nil {
		return Definition{}, err
	}
	src := string(g.Source)
	if src == "" {
		src = "explicit_definition"
	}
	// Map goal_decompose to owner_approved-style allowed source for materialize.
	if g.Source == workgraph.SourceGoalDecompose {
		src = "owner_approved"
	}
	d := Definition{
		SchemaVersion: InputVersion,
		GraphID:       g.GraphID,
		Source:        src,
		MaxItems:      g.Limits.MaxItems,
		MaxDepth:      g.Limits.MaxDepth,
		MaxParallel:   g.Limits.MaxParallel,
	}
	for _, it := range g.Items {
		d.Items = append(d.Items, DefItem{
			ID: it.ID, Intent: it.Intent, Status: string(it.Status),
			Owner: it.Owner, RouteRequirement: it.RouteRequirement,
			OutputContract: it.OutputContract, IntegrationOrder: it.IntegrationOrder,
		})
	}
	for _, dep := range g.Dependencies {
		d.Deps = append(d.Deps, DefDep{From: dep.From, To: dep.To, Kind: string(dep.Kind)})
	}
	if strings.TrimSpace(d.GraphID) == "" {
		return Definition{}, fmt.Errorf("%w: graph_id", ErrInvalid)
	}
	return d, nil
}
