package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
)

// runWorkflowGoal: loopcoder workflow goal --goal TEXT [--issue N]
func runWorkflowGoal(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("workflow goal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		goal     = fs.String("goal", "", "goal text")
		issue    = fs.String("issue", "", "issue id")
		project  = fs.String("project-id", "local-project", "project id")
		actor    = fs.String("actor", "owner", "approving actor")
		provider = fs.String("provider", "", "optional child provider pin (empty=auto-route per child)")
		model    = fs.String("model", "", "optional child model pin (empty=auto-route per child)")
		repo     = fs.String("repo", ".", "repository path for capacity inventory discover")
		format   = fs.String("format", "json", "json|human")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resolvedRepo := strings.TrimSpace(*repo)
	if resolvedRepo == "" {
		resolvedRepo = "."
	}
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: *project, Goal: *goal, Issue: *issue, Actor: *actor,
		Provider: *provider, Model: *model, RepoPath: resolvedRepo,
		ReportOut: stderr, Now: deps.Now,
	})
	if err != nil && res.GraphID == "" {
		fmt.Fprintf(stderr, "workflow goal: %v\n", err)
		return 4
	}
	if strings.ToLower(*format) == "human" {
		fmt.Fprintf(stdout, "status=%s graph=%s digest=%s children=%d multi_provider=%v\n%s\n",
			res.Status, res.GraphID, res.PlanDigest, len(res.Children), res.MultiProviderOK, res.HumanSummary)
		for _, c := range res.Children {
			before, reserved, actual, after := "n/a", "n/a", "unknown", "n/a"
			if c.CapacityBefore != nil {
				before = fmt.Sprintf("%.3f", *c.CapacityBefore)
			}
			if c.CapacityReserved != nil {
				reserved = fmt.Sprintf("%.3f", *c.CapacityReserved)
			}
			if c.CapacityActual != nil {
				actual = fmt.Sprintf("%.3f", *c.CapacityActual)
			}
			if c.CapacityAfter != nil {
				after = fmt.Sprintf("%.3f", *c.CapacityAfter)
			}
			fmt.Fprintf(stdout, "- %s stage=%s provider=%s account=%s model=%s depth=%s route=%s\n",
				c.ChildID, c.Stage, c.Provider, c.AccountRef, c.Model, c.Depth, c.RouteReason)
			fmt.Fprintf(stdout, "    capacity before=%s reserved=%s actual=%s after=%s state=%s source=%s\n",
				before, reserved, actual, after, c.CapacityState, c.ActualSource)
			fmt.Fprintf(stdout, "    attempt=%s terminal=%s evidence=%s worktree=%s next=%s\n",
				c.AttemptID, c.Terminal, c.OutputEvidence, c.WorktreePath, c.NextAction)
		}
	} else {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	}
	if err != nil {
		return 4
	}
	return 0
}
