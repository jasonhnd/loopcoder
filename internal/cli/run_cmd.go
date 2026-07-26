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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/ciwatch"
	"github.com/jasonhnd/loopcoder/internal/commitstage"
	"github.com/jasonhnd/loopcoder/internal/depthpolicy"
	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/directdelivery"
	"github.com/jasonhnd/loopcoder/internal/directrun"
	"github.com/jasonhnd/loopcoder/internal/execidentity"
	"github.com/jasonhnd/loopcoder/internal/preflight"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
	"github.com/jasonhnd/loopcoder/internal/prstage"
	"github.com/jasonhnd/loopcoder/internal/pushstage"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/routepin"
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
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`
	// ProjectID is the one canonical project identity for this run (registered
	// runtime ID when available, else repo slug). Reader-facing; matches ledger,
	// directrun receipt, and autoroute Input.ProjectID.
	ProjectID         string     `json:"project_id,omitempty"`
	Request           RunRequest `json:"request"`
	Status            string     `json:"status"` // accepted|dry_run|rejected
	Message           string     `json:"message,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	PreflightDigest   string     `json:"preflight_digest,omitempty"`
	PreflightAllow    *bool      `json:"preflight_allow_launch,omitempty"`
	PreflightDecision string     `json:"preflight_decision,omitempty"`
	// Canonical execution identity (from execidentity.BuildDirectContract).
	// Never preflight digest or ad hoc sha256:direct-* strings.
	PlanDigest          string                `json:"plan_digest,omitempty"`
	GraphDigest         string                `json:"graph_digest,omitempty"`
	TaskClass           string                `json:"task_class,omitempty"`
	ChildContractDigest string                `json:"child_contract_digest,omitempty"`
	Capacity            *capacityledger.Entry `json:"capacity,omitempty"`
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
	at := now().UTC()
	// Real issue title/body (or durable task payload) — never classify "issue N".
	// Repo may be unresolved until non-dry-run launch; payload can come from env/file/gh.
	repoAbs, _ := resolveRepo(req.Repo)
	// ONE canonical ProjectID for inventory decision, contract materialize, ledger,
	// directrun receipt/report — resolve registered runtime ID first, else slug.
	projectID := resolveCanonicalProjectID(req.Repo, repoAbs)
	issueTitle, issueBody, ierr := loadIssuePayload(req.Issue, repoAbs)
	if ierr != nil {
		return emitRunRejected(stdout, stderr, req, "issue payload unavailable: "+ierr.Error(), exitRunPrecondition, deps)
	}
	taskPrompt := strings.TrimSpace(issueTitle + "\n\n" + issueBody)
	if taskPrompt == "" {
		return emitRunRejected(stdout, stderr, req, "empty issue/task payload", exitRunPrecondition, deps)
	}
	rr, cerr := taskroute.ClassifyRun(projectID, req.Issue, taskPrompt, req.Permission, at)
	if cerr != nil {
		return emitRunRejected(stdout, stderr, req, "task classification unavailable (fail closed): "+cerr.Error(), exitRunPrecondition, deps)
	}
	taskClass := rr.TaskClass
	difficulty := rr.Difficulty
	fmt.Fprintf(stderr, "run: task class=%s difficulty=%s risk=%s quality=%s\n",
		taskClass, difficulty, rr.RiskTier, rr.QualityFloor)
	// Production launch requires real repo + exact base SHA (fail closed).
	baseSHA := ""
	if !req.DryRun {
		if repoAbs == "" {
			var rerr error
			repoAbs, rerr = resolveRepo(req.Repo)
			if rerr != nil {
				return emitRunRejected(stdout, stderr, req, "repo resolve failed: "+rerr.Error(), exitRunPrecondition, deps)
			}
		}
		var berr error
		baseSHA, berr = gitRevParse(repoAbs, firstNonEmptyCLI(req.BaseBranch, "HEAD"))
		if berr != nil {
			return emitRunRejected(stdout, stderr, req, "base SHA resolve failed: "+berr.Error(), exitRunPrecondition, deps)
		}
	}
	// Product path (auto-route OR explicit pin): always load fresh inventory.
	// Explicit pin must not bypass account/install/window observation or reserve.
	routeInv := deps.AutoRouteInventory
	capSnap := deps.LastCapacitySnapshot
	// Inventory is required for auto-route and explicit pin even on dry-run
	// (route selection/bind evidence). Reserve still runs only after preflight
	// AllowLaunch on non-dry-run.
	needInv := wantAuto || (strings.TrimSpace(req.Provider) != "" && strings.TrimSpace(req.Model) != "")
	if routeInv == nil && needInv {
		if deps.LoadAutoRouteInventory != nil {
			loaded, loadErr := deps.LoadAutoRouteInventory(context.Background(), req.Repo, at)
			if loadErr != nil {
				msg := "inventory load failed: " + loadErr.Error()
				return emitRunRejected(stdout, stderr, req, msg, exitRunPrecondition, deps)
			}
			routeInv = loaded
		} else {
			inv, snap, loadErr := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
				RepoPath: req.Repo, Now: at,
			})
			if loadErr != nil {
				msg := "inventory load failed: " + loadErr.Error()
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
	routeIn := autoroute.Input{
		AutoRoute: wantAuto, Provider: req.Provider, Model: req.Model,
		Effort: req.Effort, Permission: req.Permission,
		ProjectID: projectID, DecisionKey: "run-route|" + req.Repo + "|" + req.Issue,
		Now: at, Inventory: routeInv, TaskClass: taskClass,
	}
	resolveRoute := autoroute.Resolve
	if deps.RouteResolve != nil {
		resolveRoute = deps.RouteResolve
	}
	routeRes, routeErr := resolveRoute(routeIn)
	if routeErr != nil || (routeRes.Outcome != autoroute.OutcomeExplicitPin && routeRes.Outcome != autoroute.OutcomeSelected) {
		msg := routeRes.Message
		if msg == "" && routeErr != nil {
			msg = routeErr.Error()
		}
		if msg == "" {
			msg = "route resolution failed"
		}
		code := exitRunPrecondition
		if routeRes.Outcome == autoroute.OutcomeInvalid || routeRes.Outcome == autoroute.OutcomePinFail {
			if routeRes.Outcome == autoroute.OutcomeInvalid {
				code = exitRunUsage
			}
		}
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
		// Bind exact inventory account/install/window for the owner pin (no override).
		if req.DryRun {
			routeReason = "explicit owner pin (dry-run; inventory bind deferred)"
		} else {
			bound, berr := autoroute.BindExplicitPinWithClass(req.Provider, req.Model, req.Effort, req.Permission, taskClass, routeInv)
			if berr != nil {
				return emitRunRejected(stdout, stderr, req, "explicit pin bind failed: "+berr.Error(), exitRunPrecondition, deps)
			}
			routeRes.AccountRef = bound.AccountRef
			routeRes.InstallRef = bound.InstallRef
			routeRes.WindowKind = bound.WindowKind
			if bound.Effort != "" {
				req.Effort = bound.Effort
				routeRes.Effort = bound.Effort
			}
			if bound.Permission != "" {
				req.Permission = bound.Permission
				routeRes.Permission = bound.Permission
			}
			routeReason = "explicit owner pin bound exact inventory identity"
			fmt.Fprintf(stderr, "run: explicit pin bound account=%s install=%s window=%s\n",
				shortHashRun(bound.AccountRef), shortHashRun(bound.InstallRef), bound.WindowKind)
		}
	}

	// Canonical attempt ID created once before reserve and carried unchanged.
	runID := stableRunID(req, taskPrompt, routeRes, now().UTC())
	attemptID := "att_" + shortHashRun(runID+"|"+req.Provider+"|"+req.Model+"|"+req.Effort+"|"+routeRes.AccountRef+"|"+routeRes.InstallRef+"|"+routeRes.WindowKind+"|g0")
	accepted := RunAccepted{
		Schema: schemaRunAccepted, RunID: runID, ProjectID: projectID, Request: req,
		CreatedAt: now().UTC(),
	}

	// V090-026: preflight BEFORE capacity reserve so preflight errors never leave
	// a live hold. Dry-run still evaluates probes but never EnsureLayout / never launches.
	pfIn := preflight.Input{
		Repo: req.Repo, Provider: req.Provider, Model: req.Model,
		EnsureLayout: false,
	}
	var snap preflight.Snapshot
	var pfErr error
	if deps.PreflightEvaluate != nil {
		snap, pfErr = deps.PreflightEvaluate(context.Background(), pfIn)
	} else {
		snap, pfErr = preflight.Evaluate(context.Background(), pfIn, preflight.DefaultDeps())
	}
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
		// Dry-run never reserves capacity (preflight-only evidence).
		return emitRunAccepted(stdout, accepted)
	}
	if !snap.AllowLaunch {
		accepted.Status = "rejected"
		accepted.Message = "preflight blocked launch: " + string(snap.Decision)
		accepted.RunID = "" // no run identity when gate fails
		_ = emitRunAccepted(stdout, accepted)
		fmt.Fprintf(stderr, "run: preflight blocked decision=%s digest=%s\n", snap.Decision, snap.Digest)
		return exitRunPrecondition
	}

	// Freeze canonical single-node execution identity before reserve/launch.
	// Fail closed on missing class/depth/permission/base — never invent digests or default Tera.
	if strings.TrimSpace(string(taskClass)) == "" {
		return emitRunRejected(stdout, stderr, req,
			"task_class required for direct-run identity (no default Tera)",
			exitRunPrecondition, deps)
	}
	if strings.TrimSpace(baseSHA) == "" {
		return emitRunRejected(stdout, stderr, req,
			"base_sha required for direct-run identity before reserve",
			exitRunPrecondition, deps)
	}
	depth := strings.TrimSpace(req.Effort)
	if depth == "" {
		return emitRunRejected(stdout, stderr, req,
			"depth/effort required for direct-run identity before reserve",
			exitRunPrecondition, deps)
	}
	perm := strings.TrimSpace(req.Permission)
	if perm == "" || perm == "default" {
		return emitRunRejected(stdout, stderr, req,
			"canonical permission required for direct-run identity (exact read-only|bounded_write)",
			exitRunPrecondition, deps)
	}
	dc, dcErr := execidentity.BuildDirectContract(execidentity.DirectContractInput{
		IssueTitle:     issueTitle,
		IssueBody:      issueBody,
		BaseSHA:        baseSHA,
		TaskClass:      string(taskClass),
		Depth:          depth,
		Permission:     perm,
		OutputContract: execidentity.DirectRunOutputContract,
		Actor:          "owner",
		ProjectID:      projectID,
		Now:            now().UTC(),
	})
	if dcErr != nil {
		return emitRunRejected(stdout, stderr, req,
			"direct-run execution contract: "+dcErr.Error(),
			exitRunPrecondition, deps)
	}
	accepted.PlanDigest = dc.PlanDigest
	accepted.GraphDigest = dc.GraphDigest
	accepted.TaskClass = dc.TaskClass
	accepted.ChildContractDigest = dc.ChildContractDigest

	// Capacity reserve only after preflight AllowLaunch + frozen identity.
	// Fail closed without real snapshot — never invent capacity identity.
	var capEntry *capacityledger.Entry
	var capLedger *capacityledger.Ledger
	needReserve := routeRes.Outcome == autoroute.OutcomeSelected || routeRes.Outcome == autoroute.OutcomeExplicitPin
	if needReserve {
		if lg, lerr := openCapacityLedger(deps, now); lerr == nil && lg != nil {
			capLedger = lg
			if capSnap == nil {
				return emitRunRejected(stdout, stderr, req,
					"capacity reserve refused: no real capacity snapshot (fail closed; inventory must preserve Snapshot)",
					exitRunPrecondition, deps)
			}
			if strings.TrimSpace(routeRes.AccountRef) == "" || strings.TrimSpace(routeRes.InstallRef) == "" ||
				strings.TrimSpace(routeRes.WindowKind) == "" {
				return emitRunRejected(stdout, stderr, req,
					"capacity reserve refused: exact account/install/window required (explicit pin and auto-route)",
					exitRunPrecondition, deps)
			}
			demand := estimateDemandFraction(difficulty)
			e, rerr := lg.Reserve(capacityledger.ReserveInput{
				ProjectID: projectID, RunID: runID, AttemptID: attemptID,
				PlanDigest: dc.PlanDigest, GraphDigest: dc.GraphDigest,
				TaskClass: dc.TaskClass, ChildContractDigest: dc.ChildContractDigest,
				Policy:   capacityledger.DefaultPolicy(),
				Provider: req.Provider, Model: req.Model, Depth: depth,
				AccountRef: routeRes.AccountRef, InstallRef: routeRes.InstallRef, WindowKind: routeRes.WindowKind,
				Snapshot: capSnap, RouteReason: routeReason,
				DemandFraction: demand, DemandConfidence: quotapolicy.EvidenceEstimated,
			})
			if rerr != nil {
				fmt.Fprintf(stderr, "run: capacity reserve refused: %v\n", rerr)
				return emitRunRejected(stdout, stderr, req, "capacity reserve refused: "+rerr.Error(), exitRunPrecondition, deps)
			}
			// Persist equality: ledger entry must echo frozen contract fields.
			if e.PlanDigest != dc.PlanDigest || e.GraphDigest != dc.GraphDigest ||
				e.TaskClass != dc.TaskClass || e.ChildContractDigest != dc.ChildContractDigest {
				return emitRunRejected(stdout, stderr, req,
					"capacity entry identity mismatch vs frozen direct-run contract",
					exitRunPrecondition, deps)
			}
			if e.ProjectID != projectID {
				return emitRunRejected(stdout, stderr, req,
					"capacity entry project_id mismatch vs canonical project",
					exitRunPrecondition, deps)
			}
			capEntry = &e
			accepted.Capacity = capEntry
			fmt.Fprintf(stderr, "run: %s\n", e.HumanReport())
		} else if lerr != nil {
			return emitRunRejected(stdout, stderr, req, "capacity ledger open failed: "+lerr.Error(), exitRunPrecondition, deps)
		} else {
			return emitRunRejected(stdout, stderr, req, "capacity ledger unavailable (fail closed)", exitRunPrecondition, deps)
		}
	}

	// Production direct-run (V090-RB02) then post-worker delivery (V090-RB03 / #1314):
	// worker lifecycle through cleanup-terminal, then localverify→commit→push→PR→
	// ciwatch→verifier, stopping at the human merge gate (no auto-merge).
	// projectID already resolved canonically before route/contract/reserve.
	reportMode := termui.ModeHuman
	if req.Format == "jsonl" {
		reportMode = termui.ModeJSONL
	}
	// Operator reports go to stderr so stdout keeps the accepted/result envelope.
	reportOut := stderr
	if deps.IsTerminal != nil && !deps.IsTerminal(stderr) {
		reportOut = stderr
	}
	// Production directrun uses real agent.Runner via providerexec.AgentAdapter
	// (never silent Fake). Fake only via explicit Deps.Provider injection in tests.
	reservationID := ""
	if capEntry != nil {
		reservationID = capEntry.ReservationID
	}
	installRef := strings.TrimSpace(routeRes.InstallRef)
	drDeps := directrun.Deps{
		Now: now,
		// Provider left nil → ProductionDefaultProvider (real agent.Runner).
		// Tests may inject deps.AgentLookup for a fixture/subprocess runner.
		Preflight: func(ctx context.Context, in preflight.Input) (preflight.Snapshot, error) {
			if in.Repo == req.Repo && in.Provider == req.Provider && in.Model == req.Model {
				return snap, nil
			}
			return preflight.Evaluate(ctx, in, preflight.DefaultDeps())
		},
	}
	if deps.AgentLookup != nil {
		a := providerexec.NewAgentAdapter()
		a.Lookup = deps.AgentLookup
		drDeps.Provider = a.Execute
	}
	svc := directrun.Service{Deps: drDeps}
	execRes, execErr := svc.Execute(context.Background(), directrun.Request{
		Repo: req.Repo, Issue: req.Issue, Prompt: taskPrompt, RepoPath: repoAbs,
		Provider: req.Provider, Model: req.Model,
		Effort: req.Effort, Permission: req.Permission, BaseBranch: req.BaseBranch,
		AccountRef: routeRes.AccountRef, InstallRef: installRef,
		WindowKind: routeRes.WindowKind, ReservationID: reservationID, RouteReason: routeReason,
		RequiredUI: req.RequiredUI, OptionalUI: req.OptionalUI, Detach: req.Detach,
		ProjectID: projectID, RunID: runID, AttemptID: attemptID, BaseSHA: baseSHA,
		PlanDigest: accepted.PlanDigest, GraphDigest: accepted.GraphDigest,
		TaskClass: accepted.TaskClass, ChildContractDigest: accepted.ChildContractDigest,
		ReportOut: reportOut, ReportMode: reportMode,
	})
	if execErr != nil {
		accepted.Status = "failed"
		accepted.Message = execRes.Error
		if accepted.Message == "" {
			accepted.Message = execErr.Error()
		}
		// Provider failure: release reservation without inventing "no spend".
		// Actual stays unknown; ObserveAfter may attach a fresh remaining later.
		if capLedger != nil && capEntry != nil {
			if re, rerr := capLedger.Release(projectID, runID, attemptID, "execution_failed_unknown_actual"); rerr == nil {
				accepted.Capacity = observeAfterIfPossible(capLedger, re, projectID, runID, attemptID, routeRes, capSnap, now, stderr)
				fmt.Fprintf(stderr, "run: %s\n", accepted.Capacity.HumanReport())
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
			if re, rerr := capLedger.Release(projectID, runID, attemptID, "incomplete_terminal_unknown_actual"); rerr == nil {
				accepted.Capacity = observeAfterIfPossible(capLedger, re, projectID, runID, attemptID, routeRes, capSnap, now, stderr)
			}
		}
		_ = emitRunAccepted(stdout, accepted)
		fmt.Fprintf(stderr, "run: incomplete terminal state %s\n", execRes.State)
		return exitRunPrecondition
	}
	// Persist worker marker before delivery so crashes remain inspectable.
	_ = writeRunMarker(execRes)

	// Production delivery ports: real git/remote/github/CI on the worker worktree.
	// Never silent Fake* — tests inject deps.Delivery with explicit Fake* ports.
	var delivDeps directdelivery.Deps
	if deps.Delivery != nil {
		delivDeps = *deps.Delivery
		if delivDeps.Now == nil {
			delivDeps.Now = now
		}
	} else {
		var derr error
		delivDeps, derr = productionDeliveryDeps(now, execRes, req)
		if derr != nil {
			accepted.Status = "delivery_blocked"
			accepted.Message = "delivery ports: " + derr.Error()
			accepted.RunID = execRes.RunID
			if capLedger != nil && capEntry != nil {
				if re, rerr := capLedger.Release(projectID, runID, attemptID, "delivery_ports_unavailable_unknown_actual"); rerr == nil {
					accepted.Capacity = observeAfterIfPossible(capLedger, re, projectID, runID, attemptID, routeRes, capSnap, now, stderr)
				}
			}
			_ = emitRunAccepted(stdout, accepted)
			fmt.Fprintf(stderr, "run: delivery ports unavailable: %v\n", derr)
			return exitRunPrecondition
		}
	}
	deliv := directdelivery.Service{Deps: delivDeps}
	owned := execRes.ChangedPaths
	// When tests inject delivery with synthetic verify, they may also pass OwnedPaths
	// via worker.ChangedPaths or rely on explicit test Dirty paths on FakeGit.
	dRes, dErr := deliv.Execute(context.Background(), directdelivery.Request{
		Worker: execRes, Repo: req.Repo, Issue: req.Issue, BaseBranch: req.BaseBranch,
		OwnedPaths: owned, RequiredChecks: []string{"verify", "test", "race", "security"},
	})
	if dErr != nil {
		accepted.Status = "delivery_blocked"
		accepted.Message = dRes.Message
		if accepted.Message == "" {
			accepted.Message = dErr.Error()
		}
		accepted.RunID = execRes.RunID
		// Delivery failure after worker ran: actual unknown; never claim zero spend.
		if capLedger != nil && capEntry != nil {
			if re, rerr := capLedger.Release(projectID, runID, attemptID, "worker_ran_delivery_blocked_unknown_actual"); rerr == nil {
				accepted.Capacity = observeAfterIfPossible(capLedger, re, projectID, runID, attemptID, routeRes, capSnap, now, stderr)
				fmt.Fprintf(stderr, "run: %s\n", accepted.Capacity.HumanReport())
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
		// NEVER use reserved-as-actual. Actual only from truthful before/after
		// delta or provider-reported compatible units; otherwise keep unknown.
		accepted.Capacity = finalizeCapacityAfterRun(capLedger, *capEntry, projectID, runID, attemptID,
			routeRes, capSnap, execRes, now, stderr)
		if accepted.Capacity != nil {
			fmt.Fprintf(stderr, "run: %s\n", accepted.Capacity.HumanReport())
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

// stableRunID is a durable goal/run objective identity. It binds repo/issue/
// task prompt digest and base branch only — NOT provider/model/depth/account/
// install/window. Alternate routes preserve the same run id and bind identity
// on a new generation/attempt (attemptID carries exact route dimensions).
func stableRunID(req RunRequest, taskPrompt string, route autoroute.Result, at time.Time) string {
	_ = at
	_ = route // route dimensions bind attempt generation, not run objective
	h := sha256.New()
	promptDig := shortHashRun(strings.TrimSpace(taskPrompt))
	fmt.Fprintf(h, "%s|%s|%s|%s",
		req.Repo, req.Issue, promptDig, req.BaseBranch)
	return "run_" + hex.EncodeToString(h.Sum(nil))[:16]
}

func shortHashRun(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// estimateDemandFraction derives an estimated demand from classified difficulty
// only. Always labeled EvidenceEstimated — never presented as actual usage.
func estimateDemandFraction(diff depthpolicy.Difficulty) float64 {
	switch diff {
	case depthpolicy.DifficultyTiny:
		return 0.02
	case depthpolicy.DifficultyHard:
		return 0.10
	case depthpolicy.DifficultyHuman:
		return 0.05
	default:
		return 0.04
	}
}

// productionDeliveryDeps wires real fail-closed ports on the worker worktree.
// Never returns Fake* defaults.
func productionDeliveryDeps(now func() time.Time, worker directrun.Result, req RunRequest) (directdelivery.Deps, error) {
	wt := strings.TrimSpace(worker.WorktreePath)
	if wt == "" {
		return directdelivery.Deps{}, fmt.Errorf("worker worktree path required")
	}
	git, err := commitstage.NewLocalGit(wt)
	if err != nil {
		return directdelivery.Deps{}, err
	}
	remote, err := pushstage.NewLocalRemote(wt)
	if err != nil {
		return directdelivery.Deps{}, err
	}
	gh := prstage.NewGHClient(wt)
	owner, name, oerr := splitRepoOwnerName(req.Repo)
	if oerr != nil {
		return directdelivery.Deps{}, oerr
	}
	observe := func(ctx context.Context, pr int, head string, checks []string) (ciwatch.RemoteSnapshot, error) {
		rows, err := prstage.ObserveChecks(wt, owner, name, pr, head, checks)
		if err != nil {
			return ciwatch.RemoteSnapshot{}, err
		}
		cs := make([]ciwatch.CheckState, 0, len(rows))
		for _, r := range rows {
			cs = append(cs, ciwatch.CheckState{Name: r.Name, Conclusion: r.Conclusion, Required: r.Required})
		}
		return ciwatch.RemoteSnapshot{PRNumber: pr, HeadOID: head, Checks: cs, ObservedAt: now().UTC()}, nil
	}
	// Verifier route: independent capacity-routed read-only pin from fresh inventory
	// when available; never hardcode a company/model. Empty → fail closed.
	vRoute, verr := selectIndependentVerifierRoute(now)
	if verr != nil {
		// Honest blocked: no eligible verifier — delivery ports still construct
		// but VerifierRoute empty fails closed in directdelivery.
		vRoute = routepin.Fields{}
	}
	return directdelivery.Deps{
		Now: now, Git: git, Remote: remote, GitHub: gh,
		ObserveCI: observe, VerifierRoute: vRoute,
		// Real hook argv execution via git-path discovery; missing → pass.
		HookExec: realHookExec(wt),
		// Independent read-only verifier when route selected.
		VerifierExec: realVerifierExec(wt),
	}, nil
}

// selectIndependentVerifierRoute picks a read-only capable route from fresh
// capacity inventory (not hardcoded codex/gpt-5.5/low).
func selectIndependentVerifierRoute(now func() time.Time) (routepin.Fields, error) {
	inv, _, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now: now().UTC(),
	})
	if err != nil {
		return routepin.Fields{}, err
	}
	// Prefer a non-worker-class provider with read-only permission support.
	for _, c := range inv.Candidates {
		if !c.PermissionOK.KnownTrue() && strings.ToLower(c.Permission) != "read-only" && strings.ToLower(c.Permission) != "readonly" {
			// Allow if permission field advertises read-only.
			if !strings.Contains(strings.ToLower(c.Permission), "read") {
				continue
			}
		}
		// Skip providers that cannot affirm exact account (gemini/agy when account required).
		prov := strings.ToLower(strings.TrimSpace(c.Provider))
		if prov == "" || prov == "fixture" {
			continue
		}
		// Prefer low/medium depth for verifier.
		depth := strings.TrimSpace(c.Effort)
		if depth == "" {
			depth = "low"
		}
		perm := "read-only"
		return routepin.Fields{
			Provider: c.Provider, Model: c.Model, Effort: depth,
			Permission: perm, AccountRef: c.AccountRef, InstallRef: c.InstallRef,
			WindowKind: c.WindowKind, SubagentPolicy: routepin.SubagentForbidden,
		}, nil
	}
	return routepin.Fields{}, fmt.Errorf("no eligible independent verifier route in fresh inventory")
}

func realHookExec(worktree string) func(ctx context.Context, hook string, env []string) (int, []byte, time.Duration, bool, error) {
	return func(ctx context.Context, hook string, env []string) (int, []byte, time.Duration, bool, error) {
		start := time.Now()
		hook = strings.TrimSpace(hook)
		if hook == "" {
			return 0, []byte("empty-hook"), time.Since(start), false, nil
		}
		// Linked worktrees: .git is a file. Discover via git rev-parse --git-path.
		path := ""
		if out, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "--git-path", "hooks/"+hook).CombinedOutput(); err == nil {
			p := strings.TrimSpace(string(out))
			if p != "" && !filepath.IsAbs(p) {
				p = filepath.Join(worktree, p)
			}
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				path = p
			}
		}
		if path == "" {
			// Missing hook is non-blocking under ModeRespect when discover is empty.
			return 0, []byte("hook-absent"), time.Since(start), false, nil
		}
		// Bounded scrubbed env: only PATH + explicitly allowed keys from caller.
		cmd := exec.CommandContext(ctx, path)
		cmd.Dir = worktree
		if len(env) > 0 {
			cmd.Env = env
		} else {
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
		}
		out, err := cmd.CombinedOutput()
		// Bound output size for evidence.
		if len(out) > 64<<10 {
			out = out[:64<<10]
		}
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				return 1, out, time.Since(start), false, err
			}
		}
		return code, out, time.Since(start), false, nil
	}
}

func realVerifierExec(worktree string) func(ctx context.Context, route routepin.Fields, pr int, head string) (pass bool, digest string, err error) {
	return func(ctx context.Context, route routepin.Fields, pr int, head string) (bool, string, error) {
		if strings.TrimSpace(route.Provider) == "" || strings.ToLower(route.Permission) != "read-only" && strings.ToLower(route.Permission) != "readonly" {
			return false, "", fmt.Errorf("verifier requires read-only routed provider")
		}
		lookup := agent.Lookup
		runner, err := lookup(route.Provider)
		if err != nil {
			return false, "", fmt.Errorf("verifier provider: %w", err)
		}
		logPath := filepath.Join(worktree, ".loopcoder-verifier.log")
		inv := agent.Invocation{
			WorktreePath: worktree,
			Prompt:       fmt.Sprintf("Read-only verification of PR #%d head %s. Report findings only; do not mutate.", pr, head),
			Model:        route.Model, Effort: route.Effort, Permission: "read-only",
			ReadOnly: true, BoundedWrite: false, DisableDelegation: true,
			Role: "nested-read-only", LogPath: logPath, RunID: fmt.Sprintf("verifier-pr-%d", pr),
			HardCap: 5 * time.Minute,
		}
		res, rerr := runner.Run(ctx, inv)
		if rerr != nil {
			return false, "", rerr
		}
		if res.ExitCode != 0 {
			return false, "", fmt.Errorf("verifier exit %d", res.ExitCode)
		}
		// Digest of redacted summary only.
		sum := sha256.Sum256([]byte(strings.TrimSpace(res.Summary)))
		return true, "sha256:" + hex.EncodeToString(sum[:]), nil
	}
}

func splitRepoOwnerName(repo string) (owner, name string, err error) {
	repo = strings.TrimSpace(repo)
	parts := strings.Split(repo, "/")
	if len(parts) >= 2 {
		o, n := parts[len(parts)-2], parts[len(parts)-1]
		if o != "" && n != "" {
			return o, n, nil
		}
	}
	return "", "", fmt.Errorf("repo must be owner/name (got %q)", repo)
}

// observeAfterIfPossible attaches a fresh same-window remaining when inventory
// can be reloaded; never invents after=before as "unchanged spend".
func observeAfterIfPossible(
	lg *capacityledger.Ledger, e capacityledger.Entry,
	projectID, runID, attemptID string, routeRes autoroute.Result,
	beforeSnap *capacitysnapshot.Snapshot,
	now func() time.Time, stderr io.Writer,
) *capacityledger.Entry {
	if lg == nil {
		return &e
	}
	after, src, fresh, conf, capAt, ok := freshWindowRemaining(routeRes.Provider, routeRes.AccountRef, routeRes.InstallRef, routeRes.WindowKind, now)
	if !ok {
		// Keep actual unknown; do not claim after unchanged.
		return &e
	}
	re, err := lg.ObserveAfterBound(projectID, runID, attemptID, after, src, fresh, capacityledger.ObserveAfterOpts{
		AccountRef: routeRes.AccountRef, WindowKind: routeRes.WindowKind, InstallRef: routeRes.InstallRef,
		ObservedAt: capAt, Confidence: conf,
	})
	if err != nil {
		fmt.Fprintf(stderr, "run: observe-after skipped: %v\n", err)
		return &e
	}
	return &re
}

// finalizeCapacityAfterRun attaches after observation and may record estimated
// window-delta actual. Never labels concurrent-window delta as exact. Never
// sets actual := reserved. Provider token usage is not converted unless units
// are known-compatible (currently: leave unknown).
func finalizeCapacityAfterRun(
	lg *capacityledger.Ledger, e capacityledger.Entry,
	projectID, runID, attemptID string, routeRes autoroute.Result,
	beforeSnap *capacitysnapshot.Snapshot,
	execRes directrun.Result,
	now func() time.Time, stderr io.Writer,
) *capacityledger.Entry {
	if lg == nil {
		return &e
	}
	_ = execRes // usage tokens not unit-compatible with window fraction without provider scale
	after, src, fresh, conf, capAt, ok := freshWindowRemaining(routeRes.Provider, routeRes.AccountRef, routeRes.InstallRef, routeRes.WindowKind, now)
	if ok {
		// Observe after first (same account/window/install) with real window CapturedAt.
		reObs, oerr := lg.ObserveAfterBound(projectID, runID, attemptID, after, src, fresh, capacityledger.ObserveAfterOpts{
			AccountRef: routeRes.AccountRef, WindowKind: routeRes.WindowKind,
			InstallRef: routeRes.InstallRef, ObservedAt: capAt, Confidence: conf,
		})
		if oerr != nil {
			fmt.Fprintf(stderr, "run: observe-after skipped: %v\n", oerr)
		} else {
			e = reObs
		}
		// Window aggregate delta is estimated only (concurrent use possible).
		if after <= e.Before+0.001 {
			delta := e.Before - after
			if delta < 0 {
				delta = 0
			}
			if re, rerr := lg.ReconcileWithConfidence(projectID, runID, attemptID, delta,
				"before_after_delta:"+src, quotapolicy.EvidenceEstimated); rerr == nil {
				return &re
			}
		}
		// After rose without reset: keep actual unknown, after observed.
		return &e
	}
	// No fresh after: actual unknown — release without inventing zero spend.
	if re, rerr := lg.Release(projectID, runID, attemptID, "unknown_actual"); rerr == nil {
		return &re
	}
	return &e
}

// freshWindowRemaining reloads capacity inventory and returns remaining fraction
// for the exact provider/account/install/window when available (never first-match).
// Returns real window Source/CapturedAt/Confidence — never invents capacity_snapshot or now.
func freshWindowRemaining(provider, accountRef, installRef, windowKind string, now func() time.Time) (
	frac float64, source, freshness string, conf quotapolicy.EvidenceClass, capturedAt time.Time, ok bool,
) {
	provider = strings.TrimSpace(provider)
	accountRef = strings.TrimSpace(accountRef)
	installRef = strings.TrimSpace(installRef)
	windowKind = strings.TrimSpace(windowKind)
	// Production: exact nonempty account/install/window required — no install wildcard.
	if provider == "" || accountRef == "" || installRef == "" || windowKind == "" {
		return 0, "", "", "", time.Time{}, false
	}
	_, snap, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now: now().UTC(),
	})
	if err != nil {
		return 0, "", "", "", time.Time{}, false
	}
	for _, a := range snap.Accounts {
		if !strings.EqualFold(a.Provider, provider) {
			continue
		}
		if capacityledger.CanonicalAccountRef(a.AccountRef) != capacityledger.CanonicalAccountRef(accountRef) {
			continue
		}
		if strings.TrimSpace(a.InstallRef) != installRef {
			continue
		}
		for _, w := range a.Windows {
			if !strings.EqualFold(string(w.Kind), windowKind) {
				continue
			}
			if w.Freshness != capacitysnapshot.FreshnessFresh {
				continue
			}
			rf := capacitysnapshot.RemainingFraction(w)
			if rf == nil {
				continue
			}
			src := strings.TrimSpace(w.Source)
			if src == "" {
				// Incomplete window source — refuse rather than invent.
				continue
			}
			if w.CapturedAt.IsZero() {
				continue
			}
			c := quotapolicy.EvidenceEstimated
			switch w.Confidence {
			case capacitysnapshot.ConfidenceExact:
				c = quotapolicy.EvidenceExact
			case capacitysnapshot.ConfidenceEstimated:
				c = quotapolicy.EvidenceEstimated
			default:
				c = quotapolicy.EvidenceUnknown
			}
			return *rf, src, string(w.Freshness), c, w.CapturedAt.UTC(), true
		}
	}
	return 0, "", "", "", time.Time{}, false
}

// resolveCanonicalProjectID picks ONE project identity for the whole run path.
// Prefer registered runtimepath ProjectID when available; else repo slug.
// Never use a different ID for ledger vs directrun receipt.
func resolveCanonicalProjectID(repo, repoAbs string) string {
	path := strings.TrimSpace(repoAbs)
	if path == "" {
		if resolved, err := resolveRepo(repo); err == nil {
			path = resolved
		}
	}
	if path != "" {
		if roots, err := runtimepath.Resolve(context.Background(), path); err == nil && roots.Registered && strings.TrimSpace(roots.ProjectID) != "" {
			return strings.TrimSpace(roots.ProjectID)
		}
	}
	return slugProjectFromRepo(repo)
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

// applyDifficultyEffort selects a depth for each model from its observed
// SupportedDepths only (the efforts already present on inventory candidates for
// that model). Never invents a generic [low,medium,high,xhigh] ladder.
// Owner-explicit --effort is applied later by Resolve.
func applyDifficultyEffort(inv *autoroute.Inventory, diff depthpolicy.Difficulty) {
	if inv == nil {
		return
	}
	type key struct{ p, m, a, i string }
	// Collect observed efforts per model identity from fresh inventory rows.
	supportedBy := map[key][]string{}
	for _, c := range inv.Candidates {
		k := key{c.Provider, c.Model, c.AccountRef, c.InstallRef}
		if e := strings.TrimSpace(c.Effort); e != "" {
			// dedupe
			seen := false
			for _, s := range supportedBy[k] {
				if s == e {
					seen = true
					break
				}
			}
			if !seen {
				supportedBy[k] = append(supportedBy[k], e)
			}
		}
	}
	// Prefer depth for difficulty from observed set only; drop candidates whose
	// effort is not the selected depth when a selection succeeds.
	selectedBy := map[key]string{}
	for k, supported := range supportedBy {
		if len(supported) == 0 {
			continue
		}
		d, err := depthpolicy.Select(diff, supported, "")
		if err != nil || d == "" {
			// Fail closed: leave candidates as-is; Resolve will filter by EffortOK.
			continue
		}
		selectedBy[k] = d
	}
	if len(selectedBy) == 0 {
		return
	}
	// Keep only candidates whose Effort matches the difficulty-selected depth
	// for their model. Do not rewrite a medium-only model to high.
	out := inv.Candidates[:0]
	for _, c := range inv.Candidates {
		k := key{c.Provider, c.Model, c.AccountRef, c.InstallRef}
		want, ok := selectedBy[k]
		if !ok {
			out = append(out, c)
			continue
		}
		if strings.EqualFold(strings.TrimSpace(c.Effort), want) {
			out = append(out, c)
		}
		// else drop unsupported (not invent)
	}
	inv.Candidates = out
}

func openCapacityLedger(deps Deps, now func() time.Time) (*capacityledger.Ledger, error) {
	if strings.TrimSpace(deps.CapacityLedgerPath) != "" {
		return capacityledger.OpenPath(deps.CapacityLedgerPath, now)
	}
	return capacityledger.Open(now)
}

// gitRevParse returns the exact commit SHA for ref in repo.
func gitRevParse(repo, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "HEAD"
	}
	cmd := exec.Command("git", "-C", repo, "rev-parse", ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	sha := strings.TrimSpace(string(out))
	if len(sha) < 7 {
		return "", fmt.Errorf("invalid sha %q", sha)
	}
	return sha, nil
}

// loadIssuePayload loads issue title/body via durable task payload or gh.
// Fail closed when unavailable — no synthetic "issue N" classification text.
func loadIssuePayload(issue, repo string) (title, body string, err error) {
	issue = strings.TrimSpace(issue)
	if issue == "" {
		return "", "", fmt.Errorf("issue empty")
	}
	// Durable local task payload (tests + offline): explicit env wins.
	if p := strings.TrimSpace(os.Getenv("LOOPCODER_TASK_PAYLOAD")); p != "" {
		if b, rerr := os.ReadFile(p); rerr == nil && len(b) > 0 {
			return "task", string(b), nil
		}
	}
	if b, rerr := os.ReadFile(issue); rerr == nil && len(b) > 0 {
		return "task", string(b), nil
	}
	// Prefer gh issue view for GitHub-backed repos when available.
	if strings.TrimSpace(repo) != "" {
		cmd := exec.Command("gh", "issue", "view", issue, "--json", "title,body", "-q", `.title + "\n\n" + .body`)
		cmd.Dir = repo
		out, gerr := cmd.CombinedOutput()
		if gerr == nil {
			text := strings.TrimSpace(string(out))
			if text != "" {
				parts := strings.SplitN(text, "\n\n", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
				}
				return text, "", nil
			}
		}
	}
	return "", "", fmt.Errorf("cannot load issue %q (need gh or LOOPCODER_TASK_PAYLOAD)", issue)
}
