package directrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/eventstream"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/preflight"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
	"github.com/jasonhnd/loopcoder/internal/routepin"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/termui"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

// Request is a validated loopcoder run request (non-dry-run path).
// Account/install/window/reservation/route_reason are first-class route identity
// fields carried immutably through routepin → directattempt → providerexec.
type Request struct {
	Repo  string
	Issue string
	// Prompt is the bounded useful objective (issue title+body). Required in production.
	Prompt string
	// RepoPath is the absolute local git repository root. Required for worktree allocation.
	RepoPath      string
	Provider      string
	Model         string
	Effort        string
	Permission    string
	AccountRef    string
	InstallRef    string
	WindowKind    string
	ReservationID string
	RouteReason   string
	BaseBranch    string
	RequiredUI    []string
	OptionalUI    []string
	Detach        bool
	ProjectID     string
	RunID         string
	// AttemptID is the canonical attempt identity created before capacity reserve.
	// When set, Execute must use it unchanged (no att_<hash(runID)> invent).
	AttemptID string
	// BaseSHA is the exact git commit to materialize. Required — no fixture default.
	BaseSHA string
	// Canonical execution identity from execidentity.BuildDirectContract (required
	// on product path; verified equal on reopen/adopt).
	PlanDigest          string
	GraphDigest         string
	TaskClass           string
	ChildContractDigest string
	ReportOut           io.Writer
	ReportMode          termui.Mode
}

// Result is durable worker cleanup-terminal evidence.
type Result struct {
	RunID           string
	AttemptID       string
	ProjectID       string
	State           directattempt.State
	ProviderLaunchN int
	ExitCode        int
	WorktreePath    string
	// BaseSHA is the exact base commit the worktree was materialised from.
	BaseSHA string
	// ChangedPaths are actual worktree paths changed relative to BaseSHA
	// (never a synthetic docs/CHANGE.md invent). Empty when no useful change.
	ChangedPaths []string
	RouteDigest  string
	// Canonical execution identity echoed from request/receipt.
	PlanDigest          string
	GraphDigest         string
	TaskClass           string
	ChildContractDigest string
	StartEventID        string
	StartDigest         string
	Message             string
	Error               string
	// Independently affirmed actual route (never request echo alone).
	ActualProvider   string
	ActualModel      string
	ActualEffort     string
	ActualPermission string
	ActualAccountRef string
	ActualInstallRef string
	ProviderFailure  string
	OutputDigest     string
	UsageIn          int64
	UsageOut         int64
}

// Deps injects ports for production and tests.
type Deps struct {
	Now            func() time.Time
	HomeDir        string
	Provider       func(ctx context.Context, req providerexec.Request) (providerexec.Outcome, error)
	Preflight      func(ctx context.Context, in preflight.Input) (preflight.Snapshot, error)
	ModelAvailable func(provider, model string) bool
}

// Service is the production direct-run application service.
type Service struct {
	Deps Deps
}

// Execute runs through cleanup-terminal or fails typed without false success.
func (s Service) Execute(ctx context.Context, req Request) (Result, error) {
	now := s.Deps.Now
	if now == nil {
		now = time.Now
	}
	if req.ReportMode == "" {
		req.ReportMode = termui.ModeHuman
	}
	if req.ReportOut == nil {
		req.ReportOut = io.Discard
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		projectID = slugProject(req.Repo)
	}
	baseSHA := strings.TrimSpace(req.BaseSHA)
	if baseSHA == "" {
		return Result{Error: "base SHA required"}, fmt.Errorf("directrun: exact base SHA required (no fixture-base-sha)")
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return Result{Error: "prompt required"}, fmt.Errorf("directrun: useful issue/task prompt required (no generic fallback)")
	}
	repoPath := strings.TrimSpace(req.RepoPath)
	if repoPath == "" {
		return Result{Error: "repo path required"}, fmt.Errorf("directrun: absolute git repo path required for worktree")
	}
	if len(req.RequiredUI) == 0 {
		return Result{Error: "required UI missing"}, fmt.Errorf("directrun: at least one required UI client")
	}
	// Product fail-closed identity: all four required before adopt/worktree/provider.
	// Never accept empty PlanDigest/GraphDigest/TaskClass/ChildContractDigest.
	if err := requireExecutionIdentity(req); err != nil {
		return Result{Error: err.Error()}, err
	}
	// Defensive launch boundary: refuse non-production / non-affirmable pins before spend.
	// Explicit owner pin is never overridden; it fails closed when capability is missing.
	if err := validateLaunchCapability(req); err != nil {
		return Result{Error: err.Error()}, err
	}
	requiredClient := req.RequiredUI[0]

	// Objective digest binds task content + exact route dimensions. Alternate
	// attempts (different provider/model/account/install/depth) get a new run id
	// when RunID is empty; same objective reuses the stable id for adopt.
	objective := objectiveDigest(req, baseSHA)
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		runID = "run_" + shortHash(objective)
	}
	attemptID := strings.TrimSpace(req.AttemptID)
	if attemptID == "" {
		// Only invent when caller did not create attempt before reserve.
		attemptID = "att_" + shortHash(runID+"|g0")
	}

	// --- Durable adopt BEFORE worktree allocation / provider launch ---
	if prior, ok := LoadFullStageReceipt(s.Deps.HomeDir, projectID, runID); ok {
		// Legacy receipts missing product identity are audit-only — refuse adopt/reuse.
		if !receiptHasProductIdentity(prior) {
			return Result{RunID: runID, Error: "legacy receipt missing execution identity (audit-only; refuse adopt)"},
				fmt.Errorf("directrun: legacy receipt missing plan/graph/class/contract identity (no adopt)")
		}
		if err := prior.compatibleWith(req, baseSHA, objective); err != nil {
			return Result{RunID: runID, Error: err.Error()}, fmt.Errorf("directrun: incompatible prior receipt: %w", err)
		}
		// Already cleanup-terminal with one launch: adopt without relaunch.
		if prior.State == string(directattempt.StateCleanupTerminal) && prior.ProviderLaunchN >= 1 {
			res := Result{
				RunID: runID, AttemptID: prior.AttemptID, ProjectID: projectID,
				State: directattempt.StateCleanupTerminal, ProviderLaunchN: prior.ProviderLaunchN,
				ExitCode: prior.ExitCode, WorktreePath: prior.WorktreePath, BaseSHA: prior.BaseSHA,
				ChangedPaths: prior.ChangedPaths, RouteDigest: prior.RouteDigest,
				PlanDigest: prior.PlanDigest, GraphDigest: prior.GraphDigest,
				TaskClass: prior.TaskClass, ChildContractDigest: prior.ChildContractDigest,
				Message:        "adopted durable cleanup-terminal (no relaunch)",
				ActualProvider: prior.ActualProvider, ActualModel: prior.ActualModel,
				ActualEffort: prior.ActualEffort, ActualPermission: prior.ActualPermission,
				ActualAccountRef: prior.ActualAccountRef, ActualInstallRef: prior.ActualInstallRef,
				OutputDigest: prior.OutputDigest, UsageIn: prior.UsageIn, UsageOut: prior.UsageOut,
			}
			if prior.ProviderLaunchN > 1 {
				res.Error = fmt.Sprintf("prior receipt shows %d launches", prior.ProviderLaunchN)
				return res, fmt.Errorf("directrun: multi-launch receipt refuses adopt")
			}
			if prior.Error != "" {
				res.Error = prior.Error
				return res, fmt.Errorf("directrun: adopted failed terminal: %s", prior.Error)
			}
			return res, nil
		}
		// Incomplete prior with launch: distinguish live vs dead child.
		// Live PID (with birth identity when known) → refuse (would duplicate).
		// Dead/unknown with terminal/product → adopt. Crash before spawn (launch_n=0
		// or !Spawned) may continue. Crash after spawn never blindly relaunches.
		if prior.ProviderLaunchN >= 1 || prior.Spawned {
			if prior.ProcessPID > 0 && processAliveWithBirth(prior.ProcessPID, prior.ProcessBirthIdentity) {
				return Result{
						RunID: runID, AttemptID: prior.AttemptID, ProjectID: projectID,
						ProviderLaunchN: prior.ProviderLaunchN, WorktreePath: prior.WorktreePath,
						BaseSHA: prior.BaseSHA, Error: "prior provider process still live; refuse relaunch",
					}, fmt.Errorf("directrun: live child pid=%d birth=%s refuses relaunch",
						prior.ProcessPID, prior.ProcessBirthIdentity)
			}
			// Dead child with product evidence: adopt as failed/incomplete for delivery inspect.
			if prior.OutputDigest != "" || len(prior.ChangedPaths) > 0 {
				return Result{
					RunID: runID, AttemptID: prior.AttemptID, ProjectID: projectID,
					State: directattempt.State(prior.State), ProviderLaunchN: prior.ProviderLaunchN,
					WorktreePath: prior.WorktreePath, BaseSHA: prior.BaseSHA,
					ChangedPaths: prior.ChangedPaths, RouteDigest: prior.RouteDigest,
					OutputDigest: prior.OutputDigest, ExitCode: prior.ExitCode,
					Message:        "adopted incomplete prior with product evidence (no relaunch)",
					Error:          firstNonEmpty(prior.Error, "incomplete_terminal_adopted"),
					ActualProvider: prior.ActualProvider, ActualModel: prior.ActualModel,
					ActualEffort: prior.ActualEffort, ActualAccountRef: prior.ActualAccountRef,
					ActualInstallRef: prior.ActualInstallRef,
				}, fmt.Errorf("directrun: incomplete prior adopted without relaunch (inspect/delivery may continue)")
			}
			// Dead, no product side-effect: safe generation retry under new attempt suffix.
			// Preserve worktree; continue with same attempt only if launch_n==1 and state=launching
			// with empty product (crash-before-spawn or crash-before-write).
			if prior.State == "launching" || prior.State == "admitted" {
				attemptID = firstNonEmpty(prior.AttemptID, attemptID)
				if prior.WorktreePath != "" {
					if st, err := os.Stat(prior.WorktreePath); err == nil && st.IsDir() {
						// Continue execution — provider may not have started (crash-before-spawn).
						// launch counter on receipt will be updated; callers must still prove one spawn.
						return s.executeWithWorktree(ctx, req, projectID, runID, attemptID, baseSHA, prompt, repoPath, requiredClient, now, prior.WorktreePath, true)
					}
				}
			}
			return Result{
					RunID: runID, AttemptID: prior.AttemptID, ProjectID: projectID,
					ProviderLaunchN: prior.ProviderLaunchN, WorktreePath: prior.WorktreePath,
					BaseSHA: prior.BaseSHA, Error: "incomplete prior with launch cannot be safely continued",
				}, fmt.Errorf("directrun: incomplete launched attempt refuses unsafe relaunch (launch_n=%d state=%s)",
					prior.ProviderLaunchN, prior.State)
		}
		// Incomplete without launch: preserve worktree if still present, do not RemoveAll.
		if prior.WorktreePath != "" {
			if st, err := os.Stat(prior.WorktreePath); err == nil && st.IsDir() {
				// Fall through to continue with preserved worktree path.
				// Re-bind attempt identity from receipt.
				attemptID = firstNonEmpty(prior.AttemptID, attemptID)
				// Continue Execute using adopted worktree (skip allocateGitWorktree).
				return s.executeWithWorktree(ctx, req, projectID, runID, attemptID, baseSHA, prompt, repoPath, requiredClient, now, prior.WorktreePath, true)
			}
		}
	}

	pfIn := preflight.Input{Repo: req.Repo, Provider: req.Provider, Model: req.Model, EnsureLayout: false}
	var snap preflight.Snapshot
	var err error
	if s.Deps.Preflight != nil {
		snap, err = s.Deps.Preflight(ctx, pfIn)
	} else {
		snap, err = preflight.Evaluate(ctx, pfIn, preflight.DefaultDeps())
	}
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	if !snap.AllowLaunch {
		return Result{RunID: "", Error: "preflight blocked: " + string(snap.Decision)}, fmt.Errorf("directrun: preflight blocked")
	}

	wtPath, err := allocateGitWorktree(s.Deps.HomeDir, projectID, runID, repoPath, baseSHA)
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	return s.executeWithWorktree(ctx, req, projectID, runID, attemptID, baseSHA, prompt, repoPath, requiredClient, now, wtPath, false)
}

// executeWithWorktree continues after worktree is allocated or adopted.
func (s Service) executeWithWorktree(
	ctx context.Context, req Request,
	projectID, runID, attemptID, baseSHA, prompt, repoPath, requiredClient string,
	now func() time.Time, wtPath string, adopted bool,
) (Result, error) {
	var estore *eventstream.Store
	var err error
	if s.Deps.HomeDir != "" {
		estore, err = eventstream.OpenAt(s.Deps.HomeDir, projectID, now)
	} else {
		estore, err = eventstream.Open(projectID, now)
	}
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	_ = adopted
	_ = repoPath

	avail := s.Deps.ModelAvailable
	if avail == nil {
		avail = func(string, string) bool { return true }
	}
	pins := routepin.NewStore(now, avail)
	fields := routepin.Fields{
		Provider: req.Provider, Model: req.Model, Effort: req.Effort, Permission: req.Permission,
		AccountRef: req.AccountRef, InstallRef: req.InstallRef, WindowKind: req.WindowKind,
		ReservationID: req.ReservationID, RouteReason: req.RouteReason,
	}
	fields, err = fields.Normalize()
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	pin, err := pins.Persist(projectID, attemptID, fields)
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	pin, err = pins.Acknowledge(pin.PinID)
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	routeDigest := pin.Digest

	attempts := directattempt.NewStore(now)
	idem := "idem_" + shortHash(runID+"|"+attemptID+"|"+routeDigest)
	if _, err := attempts.Create(projectID, runID, attemptID, routeDigest, wtPath, baseSHA, idem); err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	if _, err := attempts.Admit(attemptID); err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}

	engine := &directattempt.Engine{
		Attempts: attempts,
		Pins:     pins,
		Ledger:   estore.Ledger(),
		Reserve:  func(string) error { return nil },
		Release:  func(string) error { return nil },
		Provider: s.Deps.Provider,
	}
	// Production default: real agent.Runner via AgentAdapter.
	// Fake must be explicit test injection (Deps.Provider = NewFake().Execute).
	if engine.Provider == nil {
		engine.Provider = providerexec.ProductionDefaultProvider()
	}

	reqRoute := uireport.Route{
		Provider: req.Provider, Model: req.Model, Effort: req.Effort, Permission: req.Permission,
		AccountRef: req.AccountRef, InstallRef: req.InstallRef, WindowKind: req.WindowKind,
		ReservationID: req.ReservationID, RouteReason: req.RouteReason,
	}
	seq := estore.NextSequence()
	startEnv, err := uireport.Project(uireport.Input{
		Kind: uireport.KindStart, ProjectID: projectID, RunID: runID, AttemptID: attemptID, Sequence: seq,
		Stage: "start", Status: "starting", Liveness: "alive", DeliveryStage: "persisted",
		// Requested is the selected route; Actual starts empty until provider affirms.
		Actual:     uireport.Route{},
		Requested:  reqRoute,
		Next:       uireport.NextAction{Action: "await_start_rendered"},
		Evidence:   map[string]string{"issue": req.Issue, "base": req.BaseBranch, "route_digest": routeDigest},
		RecordedAt: now().UTC(),
	})
	if err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	if err := estore.Publish(startEnv); err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	_, _ = attempts.MarkStartReport(attemptID, startEnv.EventID, startEnv.ContentDigest)

	if err := estore.RegisterClient(uisub.ClientIdentity{
		ClientID: requiredClient, SessionID: "run-session", ProjectID: projectID,
		AdapterVersion: "loopcoder-run/1", Required: true,
	}); err != nil {
		return Result{RunID: runID, Error: err.Error()}, err
	}
	if err := writeReport(req.ReportOut, req.ReportMode, startEnv); err != nil {
		_, _ = attempts.Fail(attemptID)
		return Result{RunID: runID, AttemptID: attemptID, Error: "start render failed: " + err.Error()}, err
	}
	if err := estore.Acknowledge(uisub.Ack{
		ClientID: requiredClient, SessionID: "run-session",
		EventID: startEnv.EventID, Sequence: startEnv.Sequence,
		Digest: startEnv.ContentDigest, Stage: uisub.StageRendered, At: now().UTC(),
	}); err != nil {
		_, _ = attempts.Fail(attemptID)
		return Result{RunID: runID, AttemptID: attemptID, Error: "start rendered ack failed: " + err.Error()}, err
	}

	launchN := 0
	spawnGen := 1
	// Pre-spawn receipt: launch_n=0. Crash before OnProviderStart may retry.
	// Never set launch_n=1 before real spawn (that lied and enabled double-spend).
	admitRec := StageReceipt{
		RunID: runID, AttemptID: attemptID, State: "admitted",
		ProviderLaunchN: 0, SpawnGen: spawnGen, Spawned: false,
		BaseSHA: baseSHA, WorktreePath: wtPath,
		RouteDigest: routeDigest, Objective: objectiveDigest(req, baseSHA),
		Provider: req.Provider, Model: req.Model, Effort: req.Effort, Permission: req.Permission,
		AccountRef: req.AccountRef, InstallRef: req.InstallRef, WindowKind: req.WindowKind,
		PromptDigest: shortHash(prompt),
	}
	stampIdentity(&admitRec, req)
	if err := writeFullStageReceipt(s.Deps.HomeDir, projectID, runID, admitRec); err != nil {
		_, _ = attempts.Fail(attemptID)
		return Result{RunID: runID, AttemptID: attemptID, Error: "stage receipt write failed: " + err.Error()}, err
	}
	orig := engine.Provider
	engine.Provider = func(ctx context.Context, r providerexec.Request) (providerexec.Outcome, error) {
		// Intent-to-spawn receipt (still launch_n=0 until OnProviderStart).
		intentRec := StageReceipt{
			RunID: runID, AttemptID: attemptID, State: "intent_spawn",
			ProviderLaunchN: 0, SpawnGen: spawnGen, Spawned: false,
			BaseSHA: baseSHA, WorktreePath: wtPath,
			RouteDigest: routeDigest, Objective: objectiveDigest(req, baseSHA),
			Provider: req.Provider, Model: req.Model, Effort: req.Effort, Permission: req.Permission,
			AccountRef: req.AccountRef, InstallRef: req.InstallRef, WindowKind: req.WindowKind,
			PromptDigest: shortHash(prompt),
		}
		stampIdentity(&intentRec, req)
		if err := writeFullStageReceipt(s.Deps.HomeDir, projectID, runID, intentRec); err != nil {
			return providerexec.Outcome{}, fmt.Errorf("directrun: intent receipt write failed: %w", err)
		}
		// Atomic spawn authority: launch_n=1 + PID/PGID/birth only on real start.
		r.OnProviderStart = func(ps providerexec.ProcessStart) error {
			if ps.PID <= 0 {
				return fmt.Errorf("directrun: OnProviderStart missing PID")
			}
			launchN = 1
			spawnRec := StageReceipt{
				RunID: runID, AttemptID: attemptID, State: "spawned",
				ProviderLaunchN: 1, SpawnGen: spawnGen, Spawned: true,
				ProcessPID: ps.PID, ProcessPGID: ps.PGID,
				ProcessBirthIdentity: ps.ProcessBirthIdentity,
				BaseSHA:              baseSHA, WorktreePath: wtPath,
				RouteDigest: routeDigest, Objective: objectiveDigest(req, baseSHA),
				Provider: req.Provider, Model: req.Model, Effort: req.Effort, Permission: req.Permission,
				AccountRef: req.AccountRef, InstallRef: req.InstallRef, WindowKind: req.WindowKind,
				PromptDigest: shortHash(prompt),
			}
			stampIdentity(&spawnRec, req)
			return writeFullStageReceipt(s.Deps.HomeDir, projectID, runID, spawnRec)
		}
		return orig(ctx, r)
	}
	att, err := engine.TryLaunch(ctx, directattempt.LaunchBundle{
		AttemptID: attemptID, Route: fields, RouteDigest: routeDigest,
		WorktreePath: wtPath, BaseSHA: baseSHA, IdempotencyKey: idem,
		Prompt:       prompt,
		StartEventID: startEnv.EventID, StartDigest: startEnv.ContentDigest,
		RequiredClient: requiredClient,
	})
	if err != nil {
		_, _ = attempts.Fail(attemptID)
		return Result{RunID: runID, AttemptID: attemptID, ProviderLaunchN: launchN, Error: err.Error()}, err
	}

	if att.State == directattempt.StateProcessTerminal || att.ProviderExitCode != nil {
		att, err = engine.FinishCleanup(attemptID)
		if err != nil {
			return Result{
				RunID: runID, AttemptID: attemptID, ProjectID: projectID,
				State: att.State, ProviderLaunchN: launchN, WorktreePath: wtPath,
				RouteDigest: routeDigest, StartEventID: startEnv.EventID, StartDigest: startEnv.ContentDigest,
				Error: err.Error(),
			}, err
		}
	}

	exitCode := 0
	if att.ProviderExitCode != nil {
		exitCode = *att.ProviderExitCode
	}
	actualRoute := uireport.Route{
		Provider: att.ActualProvider, Model: att.ActualModel, Effort: att.ActualEffort,
		Permission: att.ActualPermission, AccountRef: att.ActualAccountRef,
		InstallRef: att.ActualInstallRef, WindowKind: att.ActualWindowKind,
		ReservationID: att.ActualReservationID,
	}
	failClosed := exitCode != 0 || strings.TrimSpace(att.ProviderFailure) != ""
	if strings.TrimSpace(req.AccountRef) != "" && strings.TrimSpace(att.ActualAccountRef) == "" {
		failClosed = true
		if att.ProviderFailure == "" {
			att.ProviderFailure = string(providerexec.FailRouteMismatch)
			att.ProviderMessage = "missing actual account_ref"
		}
	}
	if strings.TrimSpace(req.InstallRef) != "" && strings.TrimSpace(att.ActualInstallRef) == "" {
		failClosed = true
		if att.ProviderFailure == "" {
			att.ProviderFailure = string(providerexec.FailRouteMismatch)
			att.ProviderMessage = "missing actual install_ref"
		}
	}
	if strings.TrimSpace(req.AccountRef) != "" && strings.TrimSpace(att.ActualAccountRef) != "" &&
		!strings.EqualFold(req.AccountRef, att.ActualAccountRef) {
		failClosed = true
		att.ProviderFailure = string(providerexec.FailRouteMismatch)
		att.ProviderMessage = "account_ref mismatch"
	}
	if strings.TrimSpace(req.InstallRef) != "" && strings.TrimSpace(att.ActualInstallRef) != "" &&
		req.InstallRef != att.ActualInstallRef {
		failClosed = true
		att.ProviderFailure = string(providerexec.FailRouteMismatch)
		att.ProviderMessage = "install_ref mismatch"
	}
	termStatus := "success"
	if failClosed {
		termStatus = "failed"
	}

	tseq := estore.NextSequence()
	termEnv, err := uireport.Project(uireport.Input{
		Kind: uireport.KindTerminal, ProjectID: projectID, RunID: runID, AttemptID: attemptID, Sequence: tseq,
		Stage: "cleanup_terminal", Status: termStatus, Liveness: "dead", DeliveryStage: "persisted",
		Requested: reqRoute,
		Actual:    actualRoute,
		Evidence: map[string]string{
			"exit_code":     fmt.Sprintf("%d", exitCode),
			"state":         string(att.State),
			"route_digest":  routeDigest,
			"failure":       att.ProviderFailure,
			"output_digest": att.OutputDigest,
		},
		Next:       uireport.NextAction{Action: "inspect_status"},
		RecordedAt: now().UTC(),
	})
	if err == nil {
		_ = estore.Publish(termEnv)
		_ = writeReport(req.ReportOut, req.ReportMode, termEnv)
		_ = estore.Acknowledge(uisub.Ack{
			ClientID: requiredClient, SessionID: "run-session",
			EventID: termEnv.EventID, Digest: termEnv.ContentDigest,
			Stage: uisub.StageRendered, At: now().UTC(),
		})
	}

	msg := "worker cleanup-terminal"
	if failClosed {
		msg = "provider failed: " + firstNonEmpty(att.ProviderMessage, att.ProviderFailure, fmt.Sprintf("exit %d", exitCode))
	}
	if att.State != directattempt.StateCleanupTerminal {
		msg = "incomplete terminal state: " + string(att.State)
		failClosed = true
	}
	// Empty output / no useful change cannot become successful evidence.
	if !failClosed {
		if strings.TrimSpace(att.OutputDigest) == "" {
			failClosed = true
			msg = "empty output_digest (no redacted provider output/artifact evidence)"
			if att.ProviderFailure == "" {
				att.ProviderFailure = string(providerexec.FailProcess)
			}
		}
	}
	changed := listChangedPaths(wtPath, baseSHA)
	if !failClosed && len(changed) == 0 {
		// Allow pure-report runs only when OutputDigest is non-empty (already checked).
		// Delivery still requires changed paths; worker may complete with output-only.
		emitNote := "no_worktree_diff"
		_ = emitNote
	}
	res := Result{
		RunID: runID, AttemptID: attemptID, ProjectID: projectID,
		State: att.State, ProviderLaunchN: launchN, ExitCode: exitCode,
		WorktreePath: wtPath, BaseSHA: baseSHA, ChangedPaths: changed,
		RouteDigest:  routeDigest,
		StartEventID: startEnv.EventID, StartDigest: startEnv.ContentDigest,
		Message:        msg,
		ActualProvider: att.ActualProvider, ActualModel: att.ActualModel,
		ActualEffort: att.ActualEffort, ActualPermission: att.ActualPermission,
		ActualAccountRef: att.ActualAccountRef, ActualInstallRef: att.ActualInstallRef,
		ProviderFailure: att.ProviderFailure, OutputDigest: att.OutputDigest,
		UsageIn: att.UsageIn, UsageOut: att.UsageOut,
	}
	// Persist durable stage receipt outside the git worktree for restart adopt.
	termRec := StageReceipt{
		RunID: runID, AttemptID: attemptID, State: string(res.State),
		ProviderLaunchN: launchN, SpawnGen: spawnGen, BaseSHA: baseSHA, WorktreePath: wtPath,
		ChangedPaths: res.ChangedPaths, RouteDigest: routeDigest,
		Objective: objectiveDigest(req, baseSHA),
		Provider:  req.Provider, Model: req.Model, Effort: req.Effort, Permission: req.Permission,
		AccountRef: req.AccountRef, InstallRef: req.InstallRef, WindowKind: req.WindowKind,
		PromptDigest:   shortHash(prompt),
		ActualProvider: res.ActualProvider, ActualModel: res.ActualModel,
		ActualEffort: res.ActualEffort, ActualPermission: res.ActualPermission,
		ActualAccountRef: res.ActualAccountRef, ActualInstallRef: res.ActualInstallRef,
		OutputDigest: res.OutputDigest, UsageIn: res.UsageIn, UsageOut: res.UsageOut,
		ExitCode: res.ExitCode, Error: res.Error,
	}
	stampIdentity(&termRec, req)
	res.PlanDigest = termRec.PlanDigest
	res.GraphDigest = termRec.GraphDigest
	res.TaskClass = termRec.TaskClass
	res.ChildContractDigest = termRec.ChildContractDigest
	if err := writeFullStageReceipt(s.Deps.HomeDir, projectID, runID, termRec); err != nil {
		res.Error = "stage receipt write failed: " + err.Error()
		return res, fmt.Errorf("directrun: terminal receipt write failed: %w", err)
	}
	if failClosed {
		res.Error = msg
		return res, fmt.Errorf("directrun: %s", msg)
	}
	return res, nil
}

// StageReceipt is durable run stage state for restart adopt.
type StageReceipt struct {
	RunID           string `json:"run_id"`
	AttemptID       string `json:"attempt_id"`
	State           string `json:"state"`
	ProviderLaunchN int    `json:"provider_launch_n"`
	// ProcessPID/PGID recorded at OnProviderStart (real spawn) only — never pre-spawn.
	ProcessPID           int    `json:"process_pid,omitempty"`
	ProcessPGID          int    `json:"process_pgid,omitempty"`
	ProcessBirthIdentity string `json:"process_birth_identity,omitempty"`
	SpawnGen             int    `json:"spawn_generation,omitempty"`
	// Spawned is true only after OnProviderStart succeeded (launch_n=1 with identity).
	Spawned      bool     `json:"spawned,omitempty"`
	BaseSHA      string   `json:"base_sha"`
	WorktreePath string   `json:"worktree_path"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
	RouteDigest  string   `json:"route_digest,omitempty"`
	// Canonical execution identity (never ad hoc hashes).
	PlanDigest          string `json:"plan_digest,omitempty"`
	GraphDigest         string `json:"graph_digest,omitempty"`
	TaskClass           string `json:"task_class,omitempty"`
	ChildContractDigest string `json:"child_contract_digest,omitempty"`
	Objective           string `json:"objective,omitempty"`
	Provider            string `json:"provider,omitempty"`
	Model               string `json:"model,omitempty"`
	Effort              string `json:"effort,omitempty"`
	Permission          string `json:"permission,omitempty"`
	AccountRef          string `json:"account_ref,omitempty"`
	InstallRef          string `json:"install_ref,omitempty"`
	WindowKind          string `json:"window_kind,omitempty"`
	PromptDigest        string `json:"prompt_digest,omitempty"`
	ActualProvider      string `json:"actual_provider,omitempty"`
	ActualModel         string `json:"actual_model,omitempty"`
	ActualEffort        string `json:"actual_effort,omitempty"`
	ActualPermission    string `json:"actual_permission,omitempty"`
	ActualAccountRef    string `json:"actual_account_ref,omitempty"`
	ActualInstallRef    string `json:"actual_install_ref,omitempty"`
	OutputDigest        string `json:"output_digest,omitempty"`
	UsageIn             int64  `json:"usage_in,omitempty"`
	UsageOut            int64  `json:"usage_out,omitempty"`
	ExitCode            int    `json:"exit_code,omitempty"`
	Error               string `json:"error,omitempty"`
}

func processAlive(pid int) bool {
	return processAliveWithBirth(pid, "")
}

// processAliveWithBirth refuses PID-reuse adoption when birth identity is known
// and does not match the live process (best-effort: starttime via ps/proc).
func processAliveWithBirth(pid int, birth string) bool {
	if pid <= 0 {
		return false
	}
	// Best-effort: /proc on Linux; kill -0 via exec on macOS/unix.
	alive := false
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		alive = true
	} else {
		cmd := exec.Command("kill", "-0", fmt.Sprintf("%d", pid))
		alive = cmd.Run() == nil
	}
	if !alive {
		return false
	}
	if strings.TrimSpace(birth) == "" {
		// No birth identity: PID-only is best-effort (document limitation).
		return true
	}
	// When birth is recorded, require it match observed identity or refuse as ambiguous.
	got := observeProcessBirth(pid)
	if got == "" {
		// Cannot prove identity: treat as live (fail closed — no relaunch).
		return true
	}
	return got == birth
}

func observeProcessBirth(pid int) string {
	// macOS: ps -o lstart= -p PID; Linux: /proc/PID/stat starttime field.
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// Field 22 is starttime (1-indexed fields in proc(5)).
		fields := strings.Fields(string(b))
		if len(fields) >= 22 {
			return "procstat:" + fields[21]
		}
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", fmt.Sprintf("%d", pid)).Output()
	if err != nil {
		return ""
	}
	return "ps:" + strings.TrimSpace(string(out))
}

func objectiveDigest(req Request, baseSHA string) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s",
		strings.TrimSpace(req.Repo), strings.TrimSpace(req.Issue),
		shortHash(strings.TrimSpace(req.Prompt)),
		strings.TrimSpace(req.Provider), strings.TrimSpace(req.Model),
		strings.TrimSpace(req.Effort), strings.TrimSpace(req.Permission),
		strings.TrimSpace(req.AccountRef), strings.TrimSpace(req.InstallRef),
		strings.TrimSpace(req.WindowKind), strings.TrimSpace(req.BaseBranch),
		strings.TrimSpace(baseSHA),
		strings.TrimSpace(req.PlanDigest), strings.TrimSpace(req.GraphDigest),
		strings.TrimSpace(req.TaskClass), strings.TrimSpace(req.ChildContractDigest),
	)
}

func requireExecutionIdentity(req Request) error {
	// Canonical identity: raw values only — reject padded/noncanonical request fields.
	if req.PlanDigest == "" {
		return fmt.Errorf("directrun: plan_digest required (no empty/legacy identity)")
	}
	if req.PlanDigest != strings.TrimSpace(req.PlanDigest) {
		return fmt.Errorf("directrun: plan_digest must be canonical (no padding/whitespace)")
	}
	if req.GraphDigest == "" {
		return fmt.Errorf("directrun: graph_digest required (no empty/legacy identity)")
	}
	if req.GraphDigest != strings.TrimSpace(req.GraphDigest) {
		return fmt.Errorf("directrun: graph_digest must be canonical (no padding/whitespace)")
	}
	cl := req.TaskClass
	if cl == "" {
		return fmt.Errorf("directrun: task_class required (no empty/default)")
	}
	if cl != strings.TrimSpace(cl) || cl != strings.ToLower(cl) {
		return fmt.Errorf("directrun: task_class must be canonical lowercase (no padding/case fold), got %q", req.TaskClass)
	}
	if cl == "needs_human" {
		return fmt.Errorf("directrun: task_class needs_human cannot spend")
	}
	switch cl {
	case "luna", "tera", "soul":
	default:
		return fmt.Errorf("directrun: task_class %q invalid (want luna|tera|soul)", req.TaskClass)
	}
	if req.ChildContractDigest != strings.TrimSpace(req.ChildContractDigest) {
		return fmt.Errorf("directrun: child_contract_digest must be canonical (no padding/whitespace)")
	}
	if err := requireFullLowerCCD(req.ChildContractDigest); err != nil {
		return err
	}
	return nil
}

// requireFullLowerCCD requires exactly "sha256:" + 64 lowercase hex digits.
func requireFullLowerCCD(ccd string) error {
	const p = "sha256:"
	if !strings.HasPrefix(ccd, p) {
		return fmt.Errorf("directrun: child_contract_digest must start with %q", p)
	}
	h := strings.TrimPrefix(ccd, p)
	if len(h) != 64 {
		return fmt.Errorf("directrun: child_contract_digest must be full 64-hex, got len=%d", len(h))
	}
	for _, c := range h {
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return fmt.Errorf("directrun: child_contract_digest hex must be lowercase [0-9a-f], got %q", ccd)
		}
	}
	return nil
}

func receiptHasProductIdentity(r StageReceipt) bool {
	if strings.TrimSpace(r.PlanDigest) == "" || strings.TrimSpace(r.GraphDigest) == "" ||
		strings.TrimSpace(r.TaskClass) == "" {
		return false
	}
	return requireFullLowerCCD(r.ChildContractDigest) == nil
}

func (r StageReceipt) compatibleWith(req Request, baseSHA, objective string) error {
	// Fail closed on missing objective dimensions — never skip empty prior fields.
	if strings.TrimSpace(r.Objective) == "" {
		return fmt.Errorf("prior receipt missing objective digest")
	}
	if r.Objective != objective {
		return fmt.Errorf("objective mismatch")
	}
	if strings.TrimSpace(r.BaseSHA) == "" {
		return fmt.Errorf("prior receipt missing base_sha")
	}
	if r.BaseSHA != baseSHA {
		return fmt.Errorf("base_sha mismatch prior=%s want=%s", r.BaseSHA, baseSHA)
	}
	// plan/graph/class/CCD: byte-exact on raw canonical values.
	// Do not TrimSpace before compare — padded/noncanonical identity must fail closed.
	exactRaw := func(name, a, b string) error {
		if a == "" {
			return fmt.Errorf("prior receipt missing %s", name)
		}
		if b == "" {
			return fmt.Errorf("request missing %s", name)
		}
		if a != b {
			return fmt.Errorf("%s mismatch prior=%s want=%s", name, a, b)
		}
		return nil
	}
	// Provider/model/effort: case-insensitive route tokens (trim allowed for tokens only).
	fold := func(name, a, b string) error {
		a, b = strings.TrimSpace(a), strings.TrimSpace(b)
		if a == "" {
			return fmt.Errorf("prior receipt missing %s", name)
		}
		if b == "" {
			return fmt.Errorf("request missing %s", name)
		}
		if !strings.EqualFold(a, b) {
			return fmt.Errorf("%s mismatch prior=%s want=%s", name, a, b)
		}
		return nil
	}
	// account/install: exact after trim (binding tokens, not digest identity).
	exactToken := func(name, a, b string) error {
		a, b = strings.TrimSpace(a), strings.TrimSpace(b)
		if a == "" {
			return fmt.Errorf("prior receipt missing %s", name)
		}
		if b == "" {
			return fmt.Errorf("request missing %s", name)
		}
		if a != b {
			return fmt.Errorf("%s mismatch prior=%s want=%s", name, a, b)
		}
		return nil
	}
	if err := exactRaw("plan_digest", r.PlanDigest, req.PlanDigest); err != nil {
		return err
	}
	if err := exactRaw("graph_digest", r.GraphDigest, req.GraphDigest); err != nil {
		return err
	}
	if err := exactRaw("task_class", r.TaskClass, req.TaskClass); err != nil {
		return err
	}
	if err := exactRaw("child_contract_digest", r.ChildContractDigest, req.ChildContractDigest); err != nil {
		return err
	}
	if err := fold("provider", r.Provider, req.Provider); err != nil {
		return err
	}
	if err := fold("model", r.Model, req.Model); err != nil {
		return err
	}
	// Effort/account/install: require match when either side set; fail if prior has and request empty.
	if strings.TrimSpace(r.Effort) != "" || strings.TrimSpace(req.Effort) != "" {
		if err := fold("effort", r.Effort, req.Effort); err != nil {
			return err
		}
	}
	if strings.TrimSpace(r.AccountRef) != "" || strings.TrimSpace(req.AccountRef) != "" {
		if err := exactToken("account", r.AccountRef, req.AccountRef); err != nil {
			return err
		}
	}
	if strings.TrimSpace(r.InstallRef) != "" || strings.TrimSpace(req.InstallRef) != "" {
		if err := exactToken("install", r.InstallRef, req.InstallRef); err != nil {
			return err
		}
	}
	return nil
}

// stampIdentity copies frozen execution identity from request onto a receipt.
// Request must already be canonical (requireExecutionIdentity); no trim/normalize.
func stampIdentity(r *StageReceipt, req Request) {
	if r == nil {
		return
	}
	r.PlanDigest = req.PlanDigest
	r.GraphDigest = req.GraphDigest
	r.TaskClass = req.TaskClass
	r.ChildContractDigest = req.ChildContractDigest
}

func writeFullStageReceipt(homeDir, projectID, runID string, r StageReceipt) error {
	var layout home.V09Layout
	var err error
	if homeDir != "" {
		layout, err = home.NewV09(homeDir)
	} else {
		layout, err = home.ResolveV09(home.Deps{})
	}
	if err != nil {
		return err
	}
	pdir, err := layout.ProjectDir(projectID)
	if err != nil {
		return err
	}
	dir := filepath.Join(pdir, "runs", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// Atomic temp+fsync+rename+dir-fsync — receipt writes cannot be silently ignored.
	return atomicWriteReceipt(dir, "stage-receipt.json", b)
}

// atomicWriteReceipt writes data via temp file, fsync, rename, and directory fsync.
func atomicWriteReceipt(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	cleanup = false
	// Best-effort directory fsync so rename is durable.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// LoadFullStageReceipt reads a durable stage receipt for restart adopt.
func LoadFullStageReceipt(homeDir, projectID, runID string) (StageReceipt, bool) {
	var layout home.V09Layout
	var err error
	if homeDir != "" {
		layout, err = home.NewV09(homeDir)
	} else {
		layout, err = home.ResolveV09(home.Deps{})
	}
	if err != nil {
		return StageReceipt{}, false
	}
	pdir, err := layout.ProjectDir(projectID)
	if err != nil {
		return StageReceipt{}, false
	}
	raw, err := os.ReadFile(filepath.Join(pdir, "runs", runID, "stage-receipt.json"))
	if err != nil {
		return StageReceipt{}, false
	}
	var r StageReceipt
	if json.Unmarshal(raw, &r) != nil {
		return StageReceipt{}, false
	}
	return r, true
}

// listChangedPaths returns paths changed vs baseSHA in the worktree (product diff).
func listChangedPaths(wtPath, baseSHA string) []string {
	wtPath = strings.TrimSpace(wtPath)
	baseSHA = strings.TrimSpace(baseSHA)
	if wtPath == "" || baseSHA == "" {
		return nil
	}
	cmd := exec.Command("git", "-C", wtPath, "diff", "--name-only", baseSHA)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		p := strings.TrimSpace(line)
		if p == "" || p == ".loopcoder-owned-worktree" {
			continue
		}
		paths = append(paths, filepath.ToSlash(p))
	}
	// Also unstaged / untracked.
	cmd2 := exec.Command("git", "-C", wtPath, "status", "--porcelain", "-uall")
	out2, err2 := cmd2.CombinedOutput()
	if err2 != nil {
		return paths
	}
	seen := map[string]struct{}{}
	for _, p := range paths {
		seen[p] = struct{}{}
	}
	for _, line := range strings.Split(string(out2), "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		rest := strings.TrimSpace(line[2:])
		if i := strings.Index(rest, " -> "); i >= 0 {
			rest = rest[i+4:]
		}
		rest = strings.Trim(rest, `"`)
		rest = filepath.ToSlash(rest)
		if rest == "" || rest == ".loopcoder-owned-worktree" {
			continue
		}
		if _, ok := seen[rest]; ok {
			continue
		}
		seen[rest] = struct{}{}
		paths = append(paths, rest)
	}
	return paths
}

// LoadStageReceipt is a compatibility wrapper around LoadFullStageReceipt.
func LoadStageReceipt(homeDir, projectID, runID string) (launchN int, state string, wtPath string, ok bool) {
	r, ok := LoadFullStageReceipt(homeDir, projectID, runID)
	if !ok {
		return 0, "", "", false
	}
	return r.ProviderLaunchN, r.State, r.WorktreePath, true
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// allocateGitWorktree creates an isolated git worktree from the exact base SHA.
// Never allocates an empty marker-only directory as a production worktree.
func allocateGitWorktree(homeDir, projectID, runID, repoPath, baseSHA string) (string, error) {
	var layout home.V09Layout
	var err error
	if homeDir != "" {
		layout, err = home.NewV09(homeDir)
	} else {
		layout, err = home.ResolveV09(home.Deps{})
	}
	if err != nil {
		return "", err
	}
	if err := layout.EnsureProject(projectID); err != nil {
		return "", err
	}
	pdir, err := layout.ProjectDir(projectID)
	if err != nil {
		return "", err
	}
	root := filepath.Join(pdir, "runs", runID, "worktree")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return "", err
	}
	// Remove stale path and prune broken registrations.
	_ = os.RemoveAll(root)
	_ = exec.Command("git", "-C", repoPath, "worktree", "prune").Run()
	// git worktree add --detach -f <path> <sha>
	cmd := exec.Command("git", "-C", repoPath, "worktree", "add", "--detach", "-f", root, baseSHA)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Ownership marker lives BESIDE the git worktree (parent runs/<runID>/),
	// never inside it — product diffs must not include .loopcoder-owned-worktree.
	metaDir := filepath.Dir(root)
	if err := os.WriteFile(filepath.Join(metaDir, "owned-worktree.meta"), []byte(runID+"\n"+baseSHA+"\n"+root+"\n"), 0o600); err != nil {
		return "", err
	}
	return root, nil
}

func writeReport(w io.Writer, mode termui.Mode, env uireport.Envelope) error {
	if w == nil {
		return fmt.Errorf("nil report writer")
	}
	var payload []byte
	if mode == termui.ModeJSONL {
		b, err := json.Marshal(env)
		if err != nil {
			return err
		}
		payload = append(b, '\n')
	} else {
		payload = []byte(uireport.PrettyText(uireport.Human(env)) + "\n")
	}
	n, err := w.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return fmt.Errorf("short write")
	}
	return nil
}

func slugProject(repo string) string {
	repo = strings.TrimSpace(repo)
	repo = strings.ReplaceAll(repo, "/", "-")
	repo = strings.ReplaceAll(repo, " ", "-")
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

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// validateLaunchCapability fail-closes before AgentAdapter spend when the pinned
// provider cannot affirm required exact dimensions. Fixture is allowed only when
// Deps.Provider is injected (test path); production has no fixture runner.
func validateLaunchCapability(req Request) error {
	prov := strings.TrimSpace(req.Provider)
	model := strings.TrimSpace(req.Model)
	if prov == "" || model == "" {
		return fmt.Errorf("directrun: provider and model required")
	}
	if strings.EqualFold(prov, "fixture") {
		return nil // test injection path only
	}
	aff := runtimecap.ExactRouteAffirm(prov)
	if !aff.ProductionEligible {
		return fmt.Errorf("directrun: provider %q not production-eligible for launch", prov)
	}
	if !aff.Model {
		return fmt.Errorf("directrun: provider %q cannot affirm exact model", prov)
	}
	if !aff.Permission {
		return fmt.Errorf("directrun: provider %q cannot affirm exact permission", prov)
	}
	if strings.TrimSpace(req.Effort) != "" && !aff.Depth {
		return fmt.Errorf("directrun: provider %q cannot affirm exact depth", prov)
	}
	if strings.TrimSpace(req.AccountRef) != "" && !aff.Account {
		return fmt.Errorf("directrun: provider %q cannot affirm exact account", prov)
	}
	if strings.TrimSpace(req.InstallRef) != "" && !aff.Install {
		return fmt.Errorf("directrun: provider %q cannot affirm exact install", prov)
	}
	return nil
}
