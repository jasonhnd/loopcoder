package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// runWorkflowPlan: loopcoder workflow plan --goal "..." [--issue N]
// Deterministic goal→WorkGraph decomposition (≥4 LoopCoder-owned children).
func runWorkflowPlan(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("workflow plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		goal   = fs.String("goal", "", "goal text to decompose")
		issue  = fs.String("issue", "", "issue number/id contributing to the goal")
		actor  = fs.String("actor", "owner", "approving actor for multi-node graph")
		owner  = fs.String("owner", "worker", "default write worker owner label")
		format = fs.String("format", "json", "json|human")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	g, err := workgraph.DecomposeGoal(workgraph.DecomposeOptions{
		Goal: strings.TrimSpace(*goal), Issue: strings.TrimSpace(*issue),
		Actor: strings.TrimSpace(*actor), Owner: strings.TrimSpace(*owner),
		Now: depsNow(deps),
	})
	if err != nil {
		fmt.Fprintf(stderr, "workflow plan: %v\n", err)
		return 4
	}
	switch strings.ToLower(strings.TrimSpace(*format)) {
	case "human":
		fmt.Fprintf(stdout, "graph_id=%s\nplan_digest=%s\nsource=%s\nitems=%d\nmax_parallel=%d\n",
			g.GraphID, g.PlanDigest, g.Source, len(g.Items), g.Limits.MaxParallel)
		for _, it := range g.Items {
			fmt.Fprintf(stdout, "- %s [%s] order=%d owner=%s route=%s\n  %s\n",
				it.ID, it.Status, it.IntegrationOrder, it.Owner, it.RouteRequirement, it.Intent)
		}
		for _, d := range g.Dependencies {
			fmt.Fprintf(stdout, "dep %s -> %s (%s)\n", d.From, d.To, d.Kind)
		}
	default:
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(g); err != nil {
			fmt.Fprintf(stderr, "workflow plan: encode: %v\n", err)
			return 4
		}
	}
	return 0
}

func depsNow(deps Deps) time.Time {
	if deps.Now != nil {
		return deps.Now().UTC()
	}
	return time.Now().UTC()
}
