package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/depthpolicy"
	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/directdelivery"
	"github.com/jasonhnd/loopcoder/internal/directrun"
	"github.com/jasonhnd/loopcoder/internal/preflight"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/taskroute"
	"github.com/jasonhnd/loopcoder/internal/termui"
)

// RunRequest is the normalized loopcoder run command input (V090-025 / #1124).
// Thin shell only: no provider/worktree/GitHub side effects.
type RunRequest struct {
	Repo       string   `json:"repo"`
	Issue      string   `json:"issue"`
	Provider   string   `json:"provider"`
	Model      string   `json:"model"`
	Effort     string   `json:"effort,omitempty"`
	Permission string   `json:"permission,omitempty"`
	BaseBranch string   `json:"base_branch"`
	RequiredUI []string `json:"required_ui,omitempty"`
	OptionalUI []string `json:"optional_ui,omitempty"`
	Detach     bool     `json:"detach"`
	DryRun     bool     `json:"dry_run"`
	Format     string   `json:"format"` // human|json|jsonl
	AutoRoute  bool     `json:"auto_route"`
}

// RunAccepted is printed before long work with a stable run identity.
type RunAccepted struct {
	Schema            string                `json:"schema"`
	RunID             string                `json:"run_id"`
	Request           RunRequest            `json:"request"`
	Status            string                `json:"status"` // accepted|dry_run|rejected
	Message           string                `json:"message,omitempty"`
	CreatedAt         time.Time             `json:"created_at"`
	PreflightDigest   string                `json:"preflight_digest,omitempty"`
	PreflightAllow    *bool                 `json:"preflight_allow_launch,omitempty"`
	PreflightDecision string                `json:"preflight_decision,omitempty"`
	Capacity          *capacityledger.Entry `json:"capacity,omitempty"`
}

const schemaRunAccepted = "loopcoder.run.accepted.v1"

// Exit categories for run command.
const (
	exitRunOK           = 0
	exitRunUsage        = 2
	exitRunUnsupported  = 3
	exitRunPrecondition = 4
)

func runRun(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		repo        = fs.String("repo", "", "repository path or owner/name")
		issue       = fs.String("issue", "", "issue number or URL")
		provider    = fs.String("provider", "", "explicit provider pin (omit both provider+model for auto-route)")
		model       = fs.String("model", "", "explicit model pin")
		effort      = fs.String("effort", "", "explicit effort")
		permission  = fs.String("permission", "default", "explicit permission profile")
		base        = fs.String("base", "pre-prod", "base branch")
		requiredUI  = fs.String("ui-required", "terminal", "comma-separated required UI clients")
		optionalUI  = fs.String("ui-optional", "", "comma-separated optional UI clients")
		detach      = fs.Bool("detach", false, "explicit per-run detach (default foreground)")
		dryRun      = fs.Bool("dry-run", false, "normalize and report without mutation")
		format      = fs.String("format", "human", "human|json|jsonl")
		autoRoute   = fs.Bool("auto-route", false, "enable automatic route selection from inventory evidence")
		capSnapPath = fs.String("capacity-snapshot", "", "optional path to capacitysnapshot JSON (release measure / offline qualify)")
	)
	if err := fs.Parse(args); err != nil {
		return exitRunUsage
	}
	req := RunRequest{
		Repo: strings.TrimSpace(*repo), Issue: strings.TrimSpace(*issue),
		Provider: strings.TrimSpace(*provider), Model: strings.TrimSpace(*model),
		Effort: strings.TrimSpace(*effort), Permission: strings.TrimSpace(*permission),
		BaseBranch: strings.TrimSpace(*base),
		RequiredUI: splitCSV(*requiredUI), OptionalUI: splitCSV(*optionalUI),
		Detach: *detach, DryRun: *dryRun, Format: strings.ToLower(strings.TrimSpace(*format)),
		AutoRoute: *autoRoute,
	}
	// Load optional capacity snapshot for offline exact-artifact measure (not a silent fake matrix).
	if p := strings.TrimSpace(*capSnapPath); p != "" {
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			fmt.Fprintf(stderr, "run: capacity-snapshot read: %v\n", rerr)
			return exitRunUsage
		}
		var snap capacitysnapshot.Snapshot
		if jerr := json.Unmarshal(raw, &snap); jerr != nil {
			fmt.Fprintf(stderr, "run: capacity-snapshot json: %v\n", jerr)
			return exitRunUsage
		}
		deps.LastCapacitySnapshot = &snap
		if inv, ierr := capacitysnapshot.ToRouteInventory(snap, time.Now().UTC()); ierr == nil {
			deps.AutoRouteInventory = &inv
		} else {
			fmt.Fprintf(stderr, "run: capacity-snapshot not routeable: %v\n", ierr)
			return exitRunPrecondition
		}
	}
	if req.Format == "" {
		req.Format = "human"
	}
	if req.Format != "human" && req.Format != "json" && req.Format != "jsonl" {
		fmt.Fprintf(stderr, "run: invalid --format %q (want human|json|jsonl)\n", req.Format)
		return exitRunUsage
	}
	if req.Repo == "" {
		return emitRunRejected(stdout, stderr, req, "missing required --repo", exitRunUsage, deps)
	}
	if req.Issue == "" {
		return emitRunRejected(stdout, stderr, req, "missing required --issue", exitRunUsage, deps)
	}
	if req.BaseBranch == "" {
		return emitRunRejected(stdout, stderr, req, "missing --base", exitRunUsage, deps)
	}
	if req.Permission == "" {
		return emitRunRejected(stdout, stderr, req, "missing --permission", exitRunUsage, deps)
	}
	if len(req.RequiredUI) == 0 {
		return emitRunRejected(stdout, stderr, req, "at least one --ui-required client is required", exitRunUsage, deps)
	}

	now := deps.Now
	if now == nil {
		now = time.Now
	}

	// P4 / V090-RB04 / V090-CRO-002/004/006: resolve route before run identity freeze.
	// Explicit provider+model pin is never overridden. Omitted both (or
	// --auto-route) evaluates inventory evidence via autoroute/routedecision.
	// Production loads capacitysnapshot from provider inventory when no
	// explicit deps.AutoRouteInventory is injected; fails closed if unusable.
	// Task class/depth come from taskrequirements via taskroute (not silent Tera/high).
	wantAuto := req.AutoRoute || (req.Provider == "" && req.Model == "")
	routeProject := slugProjectFromRepo(req.Repo)
	at := now().UTC()
	taskClass := capclass.ClassTera
	difficulty := depthpolicy.DifficultyStandard
	if rr, cerr := taskroute.ClassifyRun(routeProject, req.Issue, "issue "+req.Issue, req.Permission, at); cerr == nil {
		taskClass = rr.TaskClass
		difficulty = rr.Difficulty
		fmt.Fprintf(stderr, "run: task class=%s difficulty=%s risk=%s quality=%s\n",
			taskClass, difficulty, rr.RiskTier, rr.QualityFloor)
	} else {
		fmt.Fprintf(stderr, "run: task classification unavailable (%v); using tera/standard\n", cerr)
	}
	routeInv := deps.AutoRouteInventory
	capSnap := deps.LastCapacitySnapshot
	if routeInv == nil && wantAuto {
		if deps.LoadAutoRouteInventory != nil {
			loaded, loadErr := deps.LoadAutoRouteInventory(context.Background(), req.Repo, at)
			if loadErr != nil {
				msg := "auto-route inventory load failed: " + loadErr.Error()
				return emitRunRejected(stdout, stderr, req, msg, exitRunPrecondition, deps)
			}
			routeInv = loaded
		} else {
			inv, snap, loadErr := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
				RepoPath: req.Repo, Now: at,
			})
			if loadErr != nil {
				msg := "auto-route inventory load failed: " + loadErr.Error()
				return emitRunRejected(stdout, stderr, req, msg, exitRunPrecondition, deps)
			}
			routeInv = &inv
			capSnap = &snap
		}
	}
	// Stamp candidate efforts from classified difficulty (never universal high).
	if routeInv != nil && wantAuto && req.Effort == "" {
		applyDifficultyEffort(routeInv, difficulty)
	}
	routeRes, routeErr := autoroute.Resolve(autoroute.Input{
		AutoRoute: wantAuto, Provider: req.Provider, Model: req.Model,
		Effort: req.Effort, Permission: req.Permission,
		ProjectID: routeProject, DecisionKey: "run-route|" + req.Repo + "|" + req.Issue,
		Now: at, Inventory: routeInv, TaskClass: taskClass,
	})
	if routeErr != nil || (routeRes.Outcome != autoroute.OutcomeExplicitPin && routeRes.Outcome != autoroute.OutcomeSelected) {
		msg := routeRes.Message
		if msg == "" && routeErr != nil {
			msg = routeErr.Error()
		}
		if msg == "" {
			msg = "route resolution failed"
		}
		code := exitRunPrecondition
		if routeRes.Outcome == autoroute.OutcomeInvalid {
			code = exitRunUsage
		}
		// write explain to stderr when available (read-only, redacted by construction)
		if routeRes.Explain != nil && routeRes.Explain.Human != "" {
			fmt.Fprintf(stderr, "run: route explain:\n%s\n", routeRes.Explain.Human)
		}
		return emitRunRejected(stdout, stderr, req, msg, code, deps)
	}
	req.Provider = routeRes.Provider
	req.Model = routeRes.Model
	if routeRes.Effort != "" {
		req.Effort = routeRes.Effort
	}
	if routeRes.Permission != "" {
		req.Permission = routeRes.Permission
	}
	routeReason := ""
	if routeRes.Outcome == autoroute.OutcomeSelected {
		fmt.Fprintf(stderr, "run: auto-route selected provider=%s model=%s digest=%s\n",
			req.Provider, req.Model, routeRes.Digest)
		if routeRes.Explain != nil && routeRes.Explain.WinnerLine != "" {
			fmt.Fprintf(stderr, "run: route winner: %s\n", routeRes.Explain.WinnerLine)
			routeReason = routeRes.Explain.WinnerLine
		}
		if routeRes.Explain != nil && routeRes.Explain.Human != "" {
			fmt.Fprintf(stderr, "run: route explain:\n%s\n", routeRes.Explain.Human)
		}
	} else if routeRes.Outcome == autoroute.OutcomeExplicitPin {
		routeReason = "explicit owner pin"
	}

	runID := stableRunID(req, now().UTC())
	accepted := RunAccepted{
		Schema: schemaRunAccepted, RunID: runID, Request: req,
		CreatedAt: now().UTC(),
	}

	// V090-CRO-007: soft capacity reserve after route selection (product path).
	// Durable ledger prevents double-reserve on restart; unknown/stale windows refuse.
	var capEntry *capacityledger.Entry
	var capLedger *capacityledger.Ledger
	if wantAuto && routeRes.Outcome == autoroute.OutcomeSelected {
		if lg, lerr := openCapacityLedger(deps, now); lerr == nil && lg != nil {
			capLedger = lg
			attemptID := runID + "-attempt-1"
			snapForReserve := capSnap
			if snapForReserve == nil {
				snapForReserve = snapshotFromRouteInventory(routeInv, req.Provider, req.Model, at)
			}
			e, rerr := lg.Reserve(capacityledger.ReserveInput{
				ProjectID: routeProject, RunID: runID, AttemptID: attemptID,
				Policy:   capacityledger.DefaultPolicy(),
				Provider: req.Provider, Model: req.Model, Depth: req.Effort,
				Snapshot: snapForReserve, RouteReason: routeReason,
				DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceEstimated,
			})
			if rerr != nil {
				fmt.Fprintf(stderr, "run: capacity reserve refused: %v\n", rerr)
				// Fail closed for auto-route when reserve cannot be established.
				return emitRunRejected(stdout, stderr, req, "capacity reserve refused: "+rerr.Error(), exitRunPrecondition, deps)
			}
			capEntry = &e
			accepted.Capacity = capEntry
			fmt.Fprintf(stderr, "run: %s\n", e.HumanReport())
		}
	}

	// V090-026: preflight evidence gate (read-only unless ensure later ports set it).
	// dry-run still evaluates probes but never EnsureLayout / never launches.
	pfIn := preflight.Input{
		Repo: req.Repo, Provider: req.Provider, Model: req.Model,
		EnsureLayout: false,
	}
	snap, pfErr := preflight.Evaluate(context.Background(), pfIn, preflight.DefaultDeps())
	if pfErr != nil {
		return emitRunRejected(stdout, stderr, req, "preflight error: "+pfErr.Error(), exitRunPrecondition, deps)
	}
	accepted.PreflightDigest = snap.Digest
	allow := snap.AllowLaunch
	accepted.PreflightAllow = &allow
	accepted.PreflightDecision = string(snap.Decision)

	if req.DryRun {
		accepted.Status = "dry_run"
		accepted.Message = "normalized inputs + preflight snapshot; no mutation, no provider, no worktree, no GitHub"
		// Dry-run keeps reserve as evidence then releases (no actual usage).
		if capLedger != nil && capEntry != nil {
			if re, rerr := capLedger.Release(routeProject, runID, runID+"-attempt-1", "dry_run"); rerr == nil {
				accepted.Capacity = &re
				fmt.Fprintf(stderr, "run: %s\n", re.HumanReport())
			}
		}
		return emitRunAccepted(stdout, accepted)
	}
	if !snap.AllowLaunch {
		accepted.Status = "rejected"
		accepted.Message = "preflight blocked launch: " + string(snap.Decision)
		accepted.RunID = "" // no run identity when gate fails
		if capLedger != nil && capEntry != nil {
			_, _ = capLedger.Release(routeProject, runID, runID+"-attempt-1", "preflight_blocked")
		}
		_ = emitRunAccepted(stdout, accepted)
		fmt.Fprintf(stderr, "run: preflight blocked decision=%s digest=%s\n", snap.Decision, snap.Digest)
		return exitRunPrecondition
	}

	// Production direct-run (V090-RB02) then post-worker delivery (V090-RB03 / #1314):
	// worker lifecycle through cleanup-terminal, then localverify→commit→push→PR→
	// ciwatch→verifier, stopping at the human merge gate (no auto-merge).
	projectID := ""
	if resolved, err := resolveRepo(req.Repo); err == nil {
		if roots, rerr := runtimepath.Resolve(context.Background(), resolved); rerr == nil && roots.Registered {
			projectID = roots.ProjectID
		}
	}
	reportMode := termui.ModeHuman
	if req.Format == "jsonl" {
		reportMode = termui.ModeJSONL
	}
	// Operator reports go to stderr so stdout keeps the accepted/result envelope.
	reportOut := stderr
	if deps.IsTerminal != nil && !deps.IsTerminal(stderr) {
		reportOut = stderr
	}
	svc := directrun.Service{Deps: directrun.Deps{
		Now: now,
		Preflight: func(ctx context.Context, in preflight.Input) (preflight.Snapshot, error) {
			// reuse already-evaluated snapshot for the same inputs
			if in.Repo == req.Repo && in.Provider == req.Provider && in.Model == req.Model {
				return snap, nil
			}
			return preflight.Evaluate(ctx, in, preflight.DefaultDeps())
		},
	}}
	execRes, execErr := svc.Execute(context.Background(), directrun.Request{
		Repo: req.Repo, Issue: req.Issue, Provider: req.Provider, Model: req.Model,
		Effort: req.Effort, Permission: req.Permission, BaseBranch: req.BaseBranch,
		RequiredUI: req.RequiredUI, OptionalUI: req.OptionalUI, Detach: req.Detach,
		ProjectID: projectID, RunID: runID, ReportOut: reportOut, ReportMode: reportMode,
	})
	if execErr != nil {
		accepted.Status = "failed"
		accepted.Message = execRes.Error
		if accepted.Message == "" {
			accepted.Message = execErr.Error()
		}
		if capLedger != nil && capEntry != nil {
			if re, rerr := capLedger.Release(routeProject, runID, runID+"-attempt-1", "execution_failed"); rerr == nil {
				accepted.Capacity = &re
				fmt.Fprintf(stderr, "run: %s\n", re.HumanReport())
			}
		}
		_ = emitRunAccepted(stdout, accepted)
		fmt.Fprintf(stderr, "run: execution failed: %v\n", execErr)
		return exitRunPrecondition
	}
	if execRes.State != directattempt.StateCleanupTerminal {
		accepted.Status = "failed"
		accepted.Message = execRes.Message
		if capLedger != nil && capEntry != nil {
			if re, rerr := capLedger.Release(routeProject, runID, runID+"-attempt-1", "incomplete_terminal"); rerr == nil {
				accepted.Capacity = &re
			}
		}
		_ = emitRunAccepted(stdout, accepted)
		fmt.Fprintf(stderr, "run: incomplete terminal state %s\n", execRes.State)
		return exitRunPrecondition
	}
	// Persist worker marker before delivery so crashes remain inspectable.
	_ = writeRunMarker(execRes)

	deliv := directdelivery.Service{Deps: directdelivery.Deps{Now: now}}
	dRes, dErr := deliv.Execute(context.Background(), directdelivery.Request{
		Worker: execRes, Repo: req.Repo, Issue: req.Issue, BaseBranch: req.BaseBranch,
	})
	if dErr != nil {
		accepted.Status = "delivery_blocked"
		accepted.Message = dRes.Message
		if accepted.Message == "" {
			accepted.Message = dErr.Error()
		}
		accepted.RunID = execRes.RunID
		// Worker ran: reconcile estimated actual rather than full release.
		if capLedger != nil && capEntry != nil {
			if re, rerr := capLedger.Reconcile(routeProject, runID, runID+"-attempt-1", 0.04, "worker_ran_delivery_blocked"); rerr == nil {
				accepted.Capacity = &re
				fmt.Fprintf(stderr, "run: %s\n", re.HumanReport())
			}
		}
		_ = emitRunAccepted(stdout, accepted)
		fmt.Fprintf(stderr, "run: delivery blocked: %v\n", dErr)
		return exitRunPrecondition
	}
	accepted.Status = dRes.Status // human_gate
	accepted.Message = dRes.Message
	accepted.RunID = execRes.RunID
	if capLedger != nil && capEntry != nil {
		// Soft actual usage estimate from reserved demand (provider-authoritative tokens when available later).
		actual := capEntry.Reserved
		if actual <= 0 {
			actual = 0.03
		}
		if re, rerr := capLedger.Reconcile(routeProject, runID, runID+"-attempt-1", actual, "local_estimate"); rerr == nil {
			accepted.Capacity = &re
			fmt.Fprintf(stderr, "run: %s\n", re.HumanReport())
		}
	}
	if dRes.PRNumber > 0 {
		fmt.Fprintf(stderr, "run: delivery pr=%d commit=%s status=%s auto_merge=%v\n",
			dRes.PRNumber, dRes.CommitSHA, dRes.Status, dRes.AutoMerge)
	}
	return emitRunAccepted(stdout, accepted)
}

func writeRunMarker(res directrun.Result) error {
	if res.WorktreePath == "" {
		return nil
	}
	// marker beside worktree for local inspection
	dir := filepath.Dir(res.WorktreePath)
	return os.WriteFile(filepath.Join(dir, "cleanup_terminal.json"), []byte(fmt.Sprintf(
		`{"run_id":%q,"attempt_id":%q,"state":%q,"exit_code":%d}`+"\n",
		res.RunID, res.AttemptID, res.State, res.ExitCode,
	)), 0o600)
}

func emitRunRejected(stdout, stderr io.Writer, req RunRequest, msg string, code int, deps Deps) int {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	acc := RunAccepted{
		Schema: schemaRunAccepted, RunID: "", Request: req,
		Status: "rejected", Message: msg, CreatedAt: now().UTC(),
	}
	_ = emitRunAccepted(stdout, acc)
	fmt.Fprintf(stderr, "run: %s\n", msg)
	return code
}

func emitRunAccepted(stdout io.Writer, acc RunAccepted) int {
	switch acc.Request.Format {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(acc)
	case "jsonl":
		b, _ := json.Marshal(acc)
		fmt.Fprintln(stdout, string(b))
	default:
		fmt.Fprintf(stdout, "run_id=%s status=%s\n", acc.RunID, acc.Status)
		if acc.Message != "" {
			fmt.Fprintf(stdout, "message=%s\n", acc.Message)
		}
		fmt.Fprintf(stdout, "repo=%s issue=%s provider=%s model=%s base=%s detach=%v dry_run=%v\n",
			acc.Request.Repo, acc.Request.Issue, acc.Request.Provider, acc.Request.Model,
			acc.Request.BaseBranch, acc.Request.Detach, acc.Request.DryRun)
		if len(acc.Request.RequiredUI) > 0 {
			fmt.Fprintf(stdout, "ui_required=%s\n", strings.Join(acc.Request.RequiredUI, ","))
		}
	}
	return exitRunOK
}

func stableRunID(req RunRequest, at time.Time) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%d",
		req.Repo, req.Issue, req.Provider, req.Model, req.BaseBranch, req.Permission, at.UnixNano())
	return "run_" + hex.EncodeToString(h.Sum(nil))[:16]
}

func slugProjectFromRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	repo = strings.ReplaceAll(repo, "/", "-")
	if repo == "" {
		return "local-project"
	}
	var b strings.Builder
	for _, r := range repo {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		return "local-project"
	}
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applyDifficultyEffort rewrites candidate efforts using depthpolicy for the
// classified difficulty band. Owner-explicit --effort is applied later by Resolve.
func applyDifficultyEffort(inv *autoroute.Inventory, diff depthpolicy.Difficulty) {
	if inv == nil {
		return
	}
	for i := range inv.Candidates {
		c := &inv.Candidates[i]
		supported := []string{"low", "medium", "high", "xhigh"}
		if c.Effort != "" {
			supported = []string{c.Effort, "low", "medium", "high", "xhigh"}
		}
		if d, err := depthpolicy.Select(diff, supported, ""); err == nil && d != "" {
			c.Effort = d
		}
	}
}

func openCapacityLedger(deps Deps, now func() time.Time) (*capacityledger.Ledger, error) {
	if strings.TrimSpace(deps.CapacityLedgerPath) != "" {
		return capacityledger.OpenPath(deps.CapacityLedgerPath, now)
	}
	return capacityledger.Open(now)
}

// snapshotFromRouteInventory builds a minimal capacitysnapshot from soft ranking
// windows already attached to the inventory used for routing.
func snapshotFromRouteInventory(inv *autoroute.Inventory, provider, model string, now time.Time) *capacitysnapshot.Snapshot {
	if inv == nil {
		return nil
	}
	var win capacitysnapshot.Window
	found := false
	for _, s := range inv.Soft {
		if !strings.EqualFold(s.Provider, provider) || !strings.EqualFold(s.Model, model) {
			continue
		}
		for _, w := range s.Windows {
			if w.RemainingFraction == nil {
				continue
			}
			conf := capacitysnapshot.ConfidenceEstimated
			switch w.Evidence {
			case quotapolicy.EvidenceExact:
				conf = capacitysnapshot.ConfidenceExact
			case quotapolicy.EvidenceEstimated:
				conf = capacitysnapshot.ConfidenceEstimated
			default:
				continue
			}
			rf := *w.RemainingFraction * 100
			win = capacitysnapshot.Window{
				Kind: string(w.Kind), Unit: capacitysnapshot.UnitPercentage,
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: rf, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: conf, Freshness: capacitysnapshot.FreshnessFresh,
				Source: "route_inventory_soft", CapturedAt: now,
			}
			if w.TimeToReset != nil {
				t := now.Add(*w.TimeToReset)
				win.ResetAt = &t
			}
			found = true
			break
		}
		if found {
			break
		}
	}
	if !found {
		return nil
	}
	acc := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: provider, AccountRef: "acct-" + provider,
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceEstimated,
		HealthFreshness:  capacitysnapshot.FreshnessFresh,
		Windows:          []capacitysnapshot.Window{win},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: model, Present: true, SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium",
		}},
		Source: "route_inventory", CapturedAt: now,
	})
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acc}, now)
	if err != nil {
		return nil
	}
	return &s
}
