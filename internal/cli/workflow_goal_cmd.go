package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
)

// runWorkflowGoal: loopcoder workflow goal --goal TEXT [--issue N]
func runWorkflowGoal(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("workflow goal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		goal     = fs.String("goal", "", "goal text")
		issue    = fs.String("issue", "", "issue id")
		project  = fs.String("project-id", "", "project id (empty=unique disposable namespace; never share global local-project)")
		actor    = fs.String("actor", "owner", "approving actor")
		provider = fs.String("provider", "", "optional child provider pin (empty=auto-route per child)")
		model    = fs.String("model", "", "optional child model pin (empty=auto-route per child)")
		repo     = fs.String("repo", ".", "repository path for capacity inventory discover")
		format   = fs.String("format", "json", "json|human")
		dryRun   = fs.Bool("dry-run", false, "route+capacity preview only; no child execution")
		capSnap  = fs.String("capacity-snapshot", "", "optional capacitysnapshot JSON path (offline measure / qualify)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resolvedRepo := strings.TrimSpace(*repo)
	if resolvedRepo == "" {
		resolvedRepo = "."
	}
	projectID := strings.TrimSpace(*project)
	// Empty or explicit local-project → unique namespace (isolation for disposable canaries).
	if projectID == "" || projectID == "local-project" {
		projectID = goalrun.UniqueProjectID(resolvedRepo, deps.Now)
	}
	var dry *bool
	if *dryRun {
		t := true
		dry = &t
	}
	req := goalrun.Request{
		ProjectID: projectID, Goal: *goal, Issue: *issue, Actor: *actor,
		Provider: *provider, Model: *model, RepoPath: resolvedRepo,
		DryRun: dry, ReportOut: stderr, Now: deps.Now,
	}
	if p := strings.TrimSpace(*capSnap); p != "" {
		req.LoadInventory = func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			raw, rerr := os.ReadFile(p)
			if rerr != nil {
				return autoroute.Inventory{}, capacitysnapshot.Snapshot{}, rerr
			}
			var snap capacitysnapshot.Snapshot
			if jerr := json.Unmarshal(raw, &snap); jerr != nil {
				return autoroute.Inventory{}, capacitysnapshot.Snapshot{}, jerr
			}
			inv, ierr := capacitysnapshot.ToRouteInventory(snap, at)
			return inv, snap, ierr
		}
	}
	res, err := goalrun.Execute(context.Background(), req)
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
