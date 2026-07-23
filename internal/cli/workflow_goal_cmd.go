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
		goal      = fs.String("goal", "", "goal text")
		issue     = fs.String("issue", "", "issue id")
		project   = fs.String("project-id", "", "project id (empty=unique disposable namespace; never share global local-project)")
		runID     = fs.String("run-id", "", "stable run namespace for forced restart (empty=generated)")
		resume    = fs.Bool("resume", false, "resume from durable checkpoint for --run-id (exactly-once reuse)")
		actor     = fs.String("actor", "owner", "approving actor")
		provider  = fs.String("provider", "", "optional child provider pin (empty=auto-route per child)")
		model     = fs.String("model", "", "optional child model pin (empty=auto-route per child)")
		repo      = fs.String("repo", ".", "repository path for capacity inventory discover")
		format    = fs.String("format", "json", "json|human")
		dryRun    = fs.Bool("dry-run", false, "route+capacity preview only; no child execution")
		capSnap   = fs.String("capacity-snapshot", "", "optional capacitysnapshot JSON path (offline measure / qualify)")
		openPR    = fs.Bool("open-pr", false, "after human_gate open real branch/commit/push/PR (never auto-merge)")
		prBase    = fs.String("pr-base", "main", "base ref for --open-pr")
		verifier  = fs.String("independent-verifier", "", "independent verifier provider/company for PR evidence")
		verEv     = fs.String("verifier-evidence", "", "independent verifier evidence digest/path (sha256; no pending-live)")
		waitPR    = fs.Bool("wait-pr-checks", false, "with --open-pr wait for meaningful product CI then finalize evidence")
		prWait    = fs.Duration("pr-check-wait", 15*time.Minute, "max wait for --wait-pr-checks")
		canaryOut = fs.String("canary-evidence-out", "", "write loopcoder.canary_evidence.v1 derived from events (exact-binary path)")
		archDig   = fs.String("archive-digest", "", "exact RC archive sha256 for canary evidence binding")
		preSHA    = fs.String("pre-prod-sha", "", "exact pre-prod SHA for canary evidence binding")
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
		ProjectID: projectID, RunID: strings.TrimSpace(*runID),
		Goal: *goal, Issue: *issue, Actor: *actor,
		Provider: *provider, Model: *model, RepoPath: resolvedRepo,
		DryRun: dry, Resume: *resume, OpenPR: *openPR, PRBaseRef: *prBase,
		IndependentVerifier: *verifier, VerifierEvidence: *verEv,
		WaitPRChecks: *waitPR, PRCheckWait: *prWait,
		ReportOut: stderr, Now: deps.Now,
	}
	if p := strings.TrimSpace(*canaryOut); p != "" {
		req.CanaryEmit = &goalrun.CanaryEmitOptions{
			OutPath: p, ArchiveDigest: strings.TrimSpace(*archDig),
			PreProdSHA:    strings.TrimSpace(*preSHA),
			BinaryVersion: deps.BuildInfo.Version, BinaryCommit: deps.BuildInfo.Commit,
		}
	}
	if *resume && strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "workflow goal: --resume requires --run-id")
		return 2
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
	// Use CommandContext so SIGTERM/SIGINT cancel propagates into workflowrun
	// and forces interrupt events into the append-only ledger (true process kill).
	res, err := goalrun.Execute(CommandContext(), req)
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
