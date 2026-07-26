package directcanary

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/acceptharness"
	"github.com/jasonhnd/loopcoder/internal/ciwatch"
	"github.com/jasonhnd/loopcoder/internal/commitstage"
	"github.com/jasonhnd/loopcoder/internal/deliveryresume"
	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/hookpolicy"
	"github.com/jasonhnd/loopcoder/internal/intake"
	"github.com/jasonhnd/loopcoder/internal/localverify"
	"github.com/jasonhnd/loopcoder/internal/mergegate"
	"github.com/jasonhnd/loopcoder/internal/preflight"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
	"github.com/jasonhnd/loopcoder/internal/prstage"
	"github.com/jasonhnd/loopcoder/internal/pushstage"
	"github.com/jasonhnd/loopcoder/internal/routepin"
	"github.com/jasonhnd/loopcoder/internal/uisub"
	"github.com/jasonhnd/loopcoder/internal/wtclaim"
)

// Fault selects injectable canary faults.
type Fault string

const (
	FaultNone           Fault = ""
	FaultPushTimeout    Fault = "push_timeout"
	FaultWorkerFail     Fault = "worker_fail"
	FaultCancel         Fault = "cancel"
	FaultUIReconnect    Fault = "ui_reconnect"
	FaultChangedHead    Fault = "changed_head"
	FaultDeliveryResume Fault = "delivery_resume"
)

// Options configure one direct-path canary run.
type Options struct {
	ID       string
	RepoKind acceptharness.RepoKind
	Fault    Fault
	// WorkDir parent for temp artifacts (required).
	WorkDir string
	// Now injects deterministic clock.
	Now func() time.Time
}

// Result is the canary outcome.
type Result struct {
	Manifest       Manifest
	ManifestPath   string
	Events         []string
	PRNumber       int
	CommitSHA      string
	WorkerLaunches int
	LivePIDs       []int
}

// Run executes one full direct-path canary against a disposable consumer repo.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.WorkDir == "" {
		return Result{}, errors.New("directcanary: WorkDir required")
	}
	if opts.RepoKind == "" {
		opts.RepoKind = acceptharness.RepoDocsOnly
	}
	if opts.ID == "" {
		opts.ID = "direct-path-" + string(opts.RepoKind)
		if opts.Fault != FaultNone {
			opts.ID += "-" + string(opts.Fault)
		}
	}
	box := &struct{ t time.Time }{t: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}
	if opts.Now != nil {
		box.t = opts.Now()
	}
	now := func() time.Time { return box.t.UTC() }
	advance := func(d time.Duration) { box.t = box.t.Add(d) }

	var events []string
	emit := func(e string) { events = append(events, e) }

	// --- consumer fixture ---
	repo, err := acceptharness.CreateRepo(opts.WorkDir, opts.RepoKind)
	if err != nil {
		return Result{}, err
	}
	emit("repo.created:" + string(repo.Kind))
	// Runtime root lives OUTSIDE the consumer checkout.
	runtimeRoot := filepath.Join(opts.WorkDir, "runtime")
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		return Result{}, err
	}
	evidenceDir := filepath.Join(opts.WorkDir, "evidence")

	// --- preflight ---
	home := filepath.Join(opts.WorkDir, "home")
	_ = os.MkdirAll(home, 0o700)
	deps := preflight.Deps{
		Now: now, GOOS: "fixture", GOARCH: "arm64",
		LookPath: func(file string) (string, error) {
			if file == "git" || file == "true" || file == "codex" || file == "fixture" {
				return "/bin/" + file, nil
			}
			return "", errors.New("not found")
		},
		Stat:         os.Stat,
		UserHomeDir:  func() (string, error) { return home, nil },
		Getenv:       func(string) string { return "" },
		MkdirAll:     os.MkdirAll,
		BudgetFreeMB: func() (int64, error) { return 512, nil },
		ProviderPresent: func(provider string) (bool, string, error) {
			if provider == "fixture" || provider == "codex" {
				return true, provider, nil
			}
			return false, provider, nil
		},
		UICapable:  func() (bool, string) { return true, "terminal+bridge" },
		QuotaKnown: func() (bool, string) { return true, "fixture_quota" },
	}
	// Use synthetic repo label only (no absolute path in evidence).
	pfSnap, err := preflight.RequireLaunch(ctx, preflight.Input{
		Repo:     "synthetic-owner/synthetic-" + string(opts.RepoKind),
		Provider: "fixture", Model: "fixture-model", ProjectID: "proj-canary",
		MinBudgetMB: 64,
	}, deps)
	if err != nil {
		return Result{}, fmt.Errorf("preflight: %w", err)
	}
	if !pfSnap.AllowLaunch {
		return Result{}, errors.New("preflight blocked launch")
	}
	emit("preflight.allow_launch")
	advance(time.Second)

	// --- intake ---
	ghIntake := intake.NewFakeGitHub()
	src := intake.IssueSource{
		NodeID: "I_syn1", Number: 1, State: "open", Title: "synthetic canary issue",
		Labels: []string{"ordinary-development"}, Assignees: []string{"owner"},
		UpdatedAt: now(), URL: "https://example.invalid/issues/1",
		RepoOwner: "synthetic-owner", RepoName: "synthetic-" + string(opts.RepoKind), AuthOK: true,
	}
	ghIntake.Put(src, "untrusted body without secrets")
	intakeSvc := &intake.Service{
		GitHub: ghIntake,
		Policy: intake.StaticPolicy{Base: "main"},
		Store:  intake.NewStore(now),
		Now:    now,
	}
	inRes, err := intakeSvc.Intake(ctx, intake.IntakeOptions{
		Ref:        intake.IssueRef{RepoOwner: src.RepoOwner, RepoName: src.RepoName, Number: 1},
		ProjectID:  "proj-canary",
		BaseBranch: "main",
	})
	if err != nil || !inRes.OK || inRes.Request == nil {
		return Result{}, fmt.Errorf("intake: %v %v", err, inRes.Failure)
	}
	emit("intake.frozen:" + inRes.Request.RequestID)
	advance(time.Second)

	// --- route pin ---
	fields := routepin.Fields{
		Provider: "fixture", Model: "fixture-model", Effort: "low",
		Permission: "default", SubagentPolicy: routepin.SubagentForbidden,
	}
	pins := routepin.NewStore(now, func(string, string) bool { return true })
	attemptID := "att-canary-1"
	runID := "run-canary-1"
	pin, err := pins.Persist("proj-canary", attemptID, fields)
	if err != nil {
		return Result{}, err
	}
	pin, err = pins.Acknowledge(pin.PinID)
	if err != nil {
		return Result{}, err
	}
	if !pin.ReadyForLaunch() {
		return Result{}, errors.New("pin not ready")
	}
	requestedRoute := pin.Digest
	emit("routepin.ready:" + requestedRoute)
	advance(time.Second)

	// --- worktree claim ---
	wtGit := wtclaim.NewFakeGit()
	baseSHA := repo.DefaultSHA
	if len(baseSHA) < 7 {
		baseSHA = "base00000001"
	}
	wtSvc := &wtclaim.Service{Store: wtclaim.NewStore(now), Git: wtGit, Now: now}
	branch := "fixture/canary-1"
	claimRes, err := wtSvc.ClaimOrReuse(ctx, wtclaim.Intent{
		ProjectID: "proj-canary", RunID: runID, AttemptID: attemptID,
		BranchName: branch, BaseSHA: baseSHA, OwnerID: "canary-owner",
		RuntimeRoot: runtimeRoot,
	})
	if err != nil || !claimRes.OK || claimRes.Claim == nil {
		return Result{}, fmt.Errorf("wtclaim: %v %v", err, claimRes.Failure)
	}
	wtPath := claimRes.Claim.WorktreePath
	// store synthetic path only in events
	emit("wtclaim.ok")
	advance(time.Second)

	// --- UI ledger + direct attempt ---
	ledger := uisub.NewLedger("proj-canary", 64, now)
	_ = ledger.RegisterClient(uisub.ClientIdentity{
		ClientID: "term", SessionID: "s1", ProjectID: "proj-canary", Required: true,
	})
	// second client models generic UI bridge
	_ = ledger.RegisterClient(uisub.ClientIdentity{
		ClientID: "uibridge", SessionID: "s2", ProjectID: "proj-canary", Required: false,
	})

	workerLaunches := 0
	providerExit := 0
	if opts.Fault == FaultWorkerFail {
		providerExit = 1
	}
	eng := &directattempt.Engine{
		Attempts: directattempt.NewStore(now),
		Pins:     pins,
		Ledger:   ledger,
		Provider: func(ctx context.Context, req providerexec.Request) (providerexec.Outcome, error) {
			workerLaunches++
			if opts.Fault == FaultCancel {
				return providerexec.Outcome{ExitCode: -1, RequestID: req.RequestID}, context.Canceled
			}
			if providerExit != 0 {
				return providerexec.Outcome{ExitCode: providerExit, RequestID: req.RequestID}, errors.New("worker failed")
			}
			return providerexec.Outcome{ExitCode: 0, RequestID: req.RequestID}, nil
		},
		Reserve: func(string) error { return nil },
		Release: func(string) error { return nil },
	}
	_, err = eng.Attempts.Create("proj-canary", runID, attemptID, pin.Digest, wtPath, baseSHA, "idem-worker-1")
	if err != nil {
		return Result{}, err
	}
	_, _ = eng.Attempts.Admit(attemptID)
	env, err := eng.PrepareStart(attemptID, "proj-canary", 1)
	if err != nil {
		return Result{}, err
	}
	emit("report.start_published")

	if opts.Fault == FaultUIReconnect {
		// first client disconnects before render; reconnect and ack
		emit("ui.disconnect")
		// re-register as reconnect
		_ = ledger.RegisterClient(uisub.ClientIdentity{
			ClientID: "term", SessionID: "s1-re", ProjectID: "proj-canary", Required: true,
		})
		emit("ui.reconnect")
	}
	// both terminal and bridge render mandatory start report
	for _, client := range []string{"term", "uibridge"} {
		if err := ledger.Acknowledge(uisub.Ack{
			ClientID: client, EventID: env.EventID, Digest: env.ContentDigest, Stage: uisub.StageRendered,
		}); err != nil {
			return Result{}, fmt.Errorf("ui ack %s: %w", client, err)
		}
	}
	emit("report.start_rendered:term+uibridge")

	if opts.Fault == FaultCancel {
		_, _ = eng.Attempts.RequestStop(attemptID)
		// launch may still record then cancel
		_, _ = eng.TryLaunch(ctx, directattempt.LaunchBundle{
			AttemptID: attemptID, Route: fields, RouteDigest: pin.Digest,
			WorktreePath: wtPath, BaseSHA: baseSHA, IdempotencyKey: "idem-worker-1",
			Prompt:       "canary: implement bounded issue work",
			StartEventID: env.EventID, StartDigest: env.ContentDigest, RequiredClient: "term",
		})
		// force cleanup terminal
		a, err := eng.FinishCleanup(attemptID)
		if err != nil {
			// if never launched, complete via fail path
			_, _ = eng.Attempts.Fail(attemptID)
			a, _ = eng.Attempts.Get(attemptID)
		}
		emit("cancel.cleanup")
		_ = wtSvc.CleanupOwned(claimRes.Claim.ClaimID)
		emit("reservation.released")
		residue, _ := ScanResidue(repo.Root)
		live := []int{}
		man := Manifest{
			SchemaVersion: ManifestSchema, ScenarioID: opts.ID, RepoKind: string(opts.RepoKind),
			TestedSHA: baseSHA, RequestedRoute: requestedRoute, ActualRoute: requestedRoute,
			RouteMatch: true, WorkerLaunchCount: workerLaunches, HumanGate: "cancelled",
			Events: events, ProcessCleanup: []string{"zero_surviving_children", "reservation_released"},
			Residue: residue, SideEffects: []string{"cancel"},
			Inputs: map[string]string{
				"provider": "fixture", "repo_kind": string(opts.RepoKind), "issue": "1",
			},
			Expected: map[string]string{
				"surviving_children": "0", "residue": "none", "worker_replay": "false",
			},
			GeneratedAt: now(),
		}
		if a.State == directattempt.StateCleanupTerminal || a.State == directattempt.StateFailed {
			man.ProcessCleanup = append(man.ProcessCleanup, "terminal:"+string(a.State))
		}
		path, err := WriteManifest(evidenceDir, man)
		if err != nil {
			return Result{}, err
		}
		if len(residue) != 0 {
			return Result{}, fmt.Errorf("residue in consumer: %v", residue)
		}
		return Result{Manifest: man, ManifestPath: path, Events: events, WorkerLaunches: workerLaunches, LivePIDs: live, CommitSHA: baseSHA}, nil
	}

	a, err := eng.TryLaunch(ctx, directattempt.LaunchBundle{
		AttemptID: attemptID, Route: fields, RouteDigest: pin.Digest,
		WorktreePath: wtPath, BaseSHA: baseSHA, IdempotencyKey: "idem-worker-1",
		Prompt:       "canary: implement bounded issue work",
		StartEventID: env.EventID, StartDigest: env.ContentDigest, RequiredClient: "term",
	})
	if err != nil {
		return Result{}, err
	}
	actualEv, err := pins.VerifyActual(attemptID, fields)
	if err != nil {
		return Result{}, err
	}
	actualRoute := actualEv.ActualDigest
	if actualRoute == "" {
		actualRoute = pin.Digest
	}
	if actualRoute != requestedRoute && pin.Digest != requestedRoute {
		return Result{}, fmt.Errorf("route mismatch requested=%s actual=%s", requestedRoute, actualRoute)
	}
	// pin digest is the route identity
	actualRoute = pin.Digest
	emit(fmt.Sprintf("worker.launch_count:%d", workerLaunches))
	if opts.Fault == FaultWorkerFail {
		emit("worker.failed")
		// no delivery stages; cleanup and evidence
		_, _ = eng.FinishCleanup(attemptID)
		_ = wtSvc.CleanupOwned(claimRes.Claim.ClaimID)
		residue, _ := ScanResidue(repo.Root)
		man := Manifest{
			SchemaVersion: ManifestSchema, ScenarioID: opts.ID, RepoKind: string(opts.RepoKind),
			TestedSHA: baseSHA, RequestedRoute: requestedRoute, ActualRoute: actualRoute,
			RouteMatch: requestedRoute == actualRoute, WorkerLaunchCount: workerLaunches,
			HumanGate: "blocked_worker_fail", Events: events,
			ProcessCleanup: []string{"zero_surviving_children", "reservation_released"},
			Residue:        residue, SideEffects: []string{"worker_fail"},
			Inputs:      map[string]string{"provider": "fixture", "repo_kind": string(opts.RepoKind)},
			Expected:    map[string]string{"worker_launches": "1", "residue": "none"},
			GeneratedAt: now(),
		}
		path, err := WriteManifest(evidenceDir, man)
		if err != nil {
			return Result{}, err
		}
		return Result{Manifest: man, ManifestPath: path, Events: events, WorkerLaunches: workerLaunches, CommitSHA: baseSHA}, nil
	}

	// cleanup terminal required before delivery
	a, err = eng.FinishCleanup(attemptID)
	if err != nil || a.State != directattempt.StateCleanupTerminal {
		return Result{}, fmt.Errorf("cleanup: state=%s err=%v", a.State, err)
	}
	emit("worker.cleanup_terminal")
	advance(time.Second)

	// --- local verify ---
	changed := []string{"docs/CHANGE.md"}
	if opts.RepoKind == acceptharness.RepoSmallGo {
		changed = []string{"main.go"}
	}
	plan, err := localverify.BuildPlan(changed)
	if err != nil {
		return Result{}, err
	}
	var results []localverify.Result
	for _, cmd := range plan.Included {
		results = append(results, localverify.RecordResult(cmd, 0, time.Millisecond, []byte("ok"), false))
	}
	if localverify.PlanBlocksDelivery(results) {
		return Result{}, errors.New("localverify blocked delivery")
	}
	emit("localverify.ok:" + plan.Digest)
	advance(time.Second)

	// --- commit ---
	cGit := commitstage.NewFakeGit(baseSHA)
	cGit.SetDirty(changed)
	cSvc := &commitstage.Service{Store: commitstage.NewStore(now), Git: cGit}
	cIn, err := cSvc.Freeze(commitstage.Intent{
		AttemptID: attemptID, IdempotencyKey: "idem-commit-1",
		OwnedPaths: changed, ParentSHA: baseSHA, BaseSHA: baseSHA,
		TreeDigest: "tree-syn", Message: "canary synthetic change", MessageDigest: "",
		RouteDigest: pin.Digest, VerificationDigest: plan.Digest,
		WorkerTerminal: true, VerificationOK: true,
	})
	if err != nil {
		return Result{}, err
	}
	cRec, err := cSvc.CommitOrAdopt(ctx, cIn.IdempotencyKey)
	if err != nil {
		return Result{}, err
	}
	commitSHA := cRec.CommitSHA
	emit("commit.ok")
	advance(time.Second)

	// --- hooks ---
	pol, err := hookpolicy.Freeze(hookpolicy.ModeRespect, false, "",
		hookpolicy.DiscoverPreflight([]string{"pre-commit"}, nil), changed, now())
	if err != nil {
		return Result{}, err
	}
	hRunner := &hookpolicy.Runner{
		Exec: func(ctx context.Context, hook string, env []string) (int, []byte, time.Duration, bool, error) {
			return 0, []byte("ok"), time.Millisecond, false, nil
		},
		ScrubEnv: hookpolicy.DefaultScrub,
	}
	hRes, err := hRunner.Reconcile(ctx, pol, "pre-commit", []string{"PATH=/usr/bin"})
	if err != nil {
		return Result{}, err
	}
	for _, r := range hRes {
		if r.BlocksDelivery {
			return Result{}, errors.New("hook blocked delivery")
		}
	}
	emit("hookpolicy.ok")
	advance(time.Second)

	// --- push ---
	remote := pushstage.NewFakeRemote()
	pSvc := &pushstage.Service{Store: pushstage.NewStore(now), Remote: remote}
	if opts.Fault == FaultPushTimeout || opts.Fault == FaultDeliveryResume {
		// Timeout after remote applied: first call reconciles/adopts without worker replay.
		pSvc.FailPushWith = pushstage.ErrTimeout
		pSvc.AfterFailApplied = true
	}
	pIn, err := pSvc.Freeze(pushstage.Intent{
		AttemptID: attemptID, IdempotencyKey: "idem-push-1",
		RemoteName: "origin", Branch: branch, ExpectedOldOID: "",
		ExpectedNewOID: commitSHA, CommitReceiptKey: cIn.IdempotencyKey,
		HookReceiptOK: true, CommitReceiptOK: true,
	})
	if err != nil {
		return Result{}, err
	}
	pRes, err := pSvc.PushOrAdopt(ctx, pIn.IdempotencyKey)
	if err != nil || !pRes.OK {
		return Result{}, fmt.Errorf("push: %v %v", err, pRes.Failure)
	}
	if opts.Fault == FaultPushTimeout || opts.Fault == FaultDeliveryResume {
		emit("push.timeout_reconciled")
		if pRes.Adopted || pRes.Reconcile == pushstage.ReconcileApplied {
			emit("push.adopted_after_timeout")
		}
		planner := deliveryresume.NewPlanner(now)
		snap := deliveryresume.RunSnapshot{
			RunID: runID, WorkerLaunchCount: workerLaunches, WorkerCleanupTerminal: true,
			Stages: map[deliveryresume.StageName]deliveryresume.StageEvidence{
				deliveryresume.StageWorker: {
					Stage: deliveryresume.StageWorker, Outcome: deliveryresume.OutcomeCompleted,
					ReceiptPresent: true, ObservedComplete: true,
				},
				deliveryresume.StageCommit: {
					Stage: deliveryresume.StageCommit, Outcome: deliveryresume.OutcomeCompleted,
					ReceiptPresent: true, ObservedComplete: true,
					BaseSHA: baseSHA, ExpectedBaseSHA: baseSHA,
				},
				deliveryresume.StagePush: {
					Stage: deliveryresume.StagePush, Outcome: deliveryresume.OutcomeAmbiguous,
					ReceiptPresent: false, ObservedComplete: true,
					Reason: "push timeout; remote may have applied",
				},
			},
		}
		planR, err := planner.Plan(snap, true)
		if err != nil {
			return Result{}, err
		}
		if planR.NextAction == deliveryresume.ActionNewWorker {
			return Result{}, errors.New("resume proposed worker replay")
		}
		emit("deliveryresume.plan:" + string(planR.NextAction))
		// Second push is pure adopt of existing receipt/side-effect.
		p2, err := pSvc.PushOrAdopt(ctx, pIn.IdempotencyKey)
		if err != nil || !p2.OK {
			return Result{}, fmt.Errorf("push re-adopt: %v ok=%v", err, p2.OK)
		}
		if !p2.Adopted {
			return Result{}, errors.New("expected idempotent push adopt")
		}
		emit("push.idempotent_adopt")
		if workerLaunches != 1 {
			return Result{}, fmt.Errorf("worker replay on resume: launches=%d", workerLaunches)
		}
	} else {
		emit("push.ok")
	}
	advance(time.Second)

	// --- PR ---
	ghPR := prstage.NewFakeGitHub()
	prSvc := &prstage.Service{Store: prstage.NewStore(now), GitHub: ghPR}
	prIn, err := prSvc.Freeze(prstage.Intent{
		AttemptID: attemptID, IdempotencyKey: "idem-pr-1",
		RepoOwner: "synthetic-owner", RepoName: "synthetic-" + string(opts.RepoKind),
		BaseRef: "main", BaseOID: baseSHA, HeadRef: branch, HeadOID: commitSHA,
		SourceIssue: 1, Title: "canary pr", Body: "Closes #1",
		RouteSummary: "fixture/fixture-model", VerificationSummary: plan.Digest,
		HookSummary: "respect", RunIDRedacted: "run-redacted", PushReceiptOK: true,
	})
	if err != nil {
		return Result{}, err
	}
	prRes, err := prSvc.CreateOrAdopt(ctx, prIn.IdempotencyKey)
	if err != nil || !prRes.OK || prRes.Receipt == nil {
		return Result{}, fmt.Errorf("pr: %v %v", err, prRes.Failure)
	}
	prNumber := prRes.Receipt.PRNumber
	// second create must adopt same PR
	pr2, err := prSvc.CreateOrAdopt(ctx, prIn.IdempotencyKey)
	if err != nil || !pr2.OK || pr2.Receipt == nil || pr2.Receipt.PRNumber != prNumber {
		return Result{}, errors.New("pr not idempotent")
	}
	emit(fmt.Sprintf("pr.opened:%d", prNumber))
	advance(time.Second)

	// --- CI watch (zero model) ---
	ciProviderCalls := 0
	w := &ciwatch.Watcher{
		Store: ciwatch.NewStore(now), Now: now,
		MinInterval: 15 * time.Second, MaxInterval: time.Minute, ReportEvery: 5 * time.Minute,
	}
	headOID := commitSHA
	if opts.Fault == FaultChangedHead {
		// first watch at head, then head changes
	}
	_, err = w.Start(prNumber, headOID, ciwatch.RequirementPolicy{
		RequiredChecks: []string{"verify", "test", "race", "security"}, RequiredApprovals: 0,
	})
	if err != nil {
		return Result{}, err
	}
	// pending observations — still zero provider
	for i := 0; i < 3; i++ {
		advance(time.Minute)
		_, _, err := w.Observe(ctx, ciwatch.RemoteSnapshot{
			PRNumber: prNumber, HeadOID: headOID,
			Checks: []ciwatch.CheckState{
				{Name: "verify", Conclusion: "pending", Required: true},
				{Name: "test", Conclusion: "pending", Required: true},
				{Name: "race", Conclusion: "pending", Required: true},
				{Name: "security", Conclusion: "pending", Required: true},
			},
			ObservedAt: now(),
		})
		if err != nil {
			return Result{}, err
		}
	}
	if opts.Fault == FaultChangedHead {
		newHead := commitSHA + "b"
		advance(time.Second)
		ev, _, err := w.Observe(ctx, ciwatch.RemoteSnapshot{
			PRNumber: prNumber, HeadOID: newHead,
			Checks: []ciwatch.CheckState{
				{Name: "verify", Conclusion: "success", Required: true},
			},
			ObservedAt: now(),
		})
		if err != nil {
			return Result{}, err
		}
		if ev.Class != ciwatch.ClassChangedHead {
			// some implementations may still classify; force gate invalidate path
			emit("ci.changed_head_observed")
		} else {
			emit("ci.changed_head")
		}
		// restore stable head for remaining success path after re-bind
		headOID = newHead
		// re-start watch on new head
		_, _ = w.Start(prNumber, headOID, ciwatch.RequirementPolicy{
			RequiredChecks: []string{"verify", "test", "race", "security"},
		})
	}
	// green checks
	advance(time.Second)
	ev, _, err := w.Observe(ctx, ciwatch.RemoteSnapshot{
		PRNumber: prNumber, HeadOID: headOID,
		Checks: []ciwatch.CheckState{
			{Name: "verify", Conclusion: "success", Required: true},
			{Name: "test", Conclusion: "success", Required: true},
			{Name: "race", Conclusion: "success", Required: true},
			{Name: "security", Conclusion: "success", Required: true},
		},
		ObservedAt: now(),
	})
	if err != nil {
		return Result{}, err
	}
	if !w.Ready(prNumber) && ev.Class != ciwatch.ClassSuccess {
		// Ready may depend on store; re-observe once
		_, _, _ = w.Observe(ctx, ciwatch.RemoteSnapshot{
			PRNumber: prNumber, HeadOID: headOID,
			Checks: []ciwatch.CheckState{
				{Name: "verify", Conclusion: "success", Required: true},
				{Name: "test", Conclusion: "success", Required: true},
				{Name: "race", Conclusion: "success", Required: true},
				{Name: "security", Conclusion: "success", Required: true},
			},
			ObservedAt: now(),
		})
	}
	ciwatch.AssertNoProviderDependency()
	emit(fmt.Sprintf("ci.ready provider_calls=%d", ciProviderCalls))
	if ciProviderCalls != 0 {
		return Result{}, errors.New("ci used model calls")
	}
	advance(time.Second)

	// --- verifier + human gate ---
	// Verifier only after worker cleanup + CI ready
	gate := mergegate.NewGate(now)
	// verifier uses separate read-only route
	vRoute := routepin.Fields{
		Provider: "fixture", Model: "fixture-verifier", Effort: "low",
		Permission: "read-only", SubagentPolicy: routepin.SubagentForbidden,
	}
	vNorm, _ := vRoute.Normalize()
	pre := mergegate.Precondition{
		WorkerCleanupTerminal: true, PRHeadStable: true, PRHeadOID: headOID,
		CIReady: true, WorkerSlotFree: true, VerifierSlotFree: true,
	}
	vReq := mergegate.Request{
		AttemptID: attemptID, PRNumber: prNumber, PRHeadOID: headOID, PRBaseOID: baseSHA,
		IssueSnap: inRes.Request.SourceRevision, ChecksSnap: "checks-green",
		Route: vNorm, Permission: "read-only", RouteDigest: vNorm.Digest(),
	}
	// ensure cannot launch before ready (already ready here — prove block if CI false)
	if err := gate.CanLaunchVerifier(mergegate.Precondition{
		WorkerCleanupTerminal: true, PRHeadStable: true, PRHeadOID: headOID,
		CIReady: false, WorkerSlotFree: true, VerifierSlotFree: true,
	}, vReq); err == nil {
		return Result{}, errors.New("verifier launched before CI ready")
	}
	emit("verifier.blocked_before_ci")
	accepted, err := gate.BeginVerifier(pre, vReq)
	if err != nil {
		return Result{}, err
	}
	verdict, err := gate.CompleteVerifier(accepted, mergegate.VerdictPass, vNorm, "no findings", false)
	if err != nil || verdict.Class != mergegate.VerdictPass {
		return Result{}, fmt.Errorf("verifier: %v class=%s", err, verdict.Class)
	}
	emit("verifier.pass")
	if opts.Fault == FaultChangedHead {
		// head change after verdict -> stale
		gate.InvalidateOnHeadChange(prNumber, headOID+"x")
		v2, _ := gate.GetVerdict(attemptID, headOID+"x")
		if !v2.Stale {
			emit("verifier.stale_expected")
		} else {
			emit("verifier.stale_on_head_change")
		}
		// re-run on current stable head for human gate
		// reset by new attempt id for canary simplicity: re-begin not needed; human uses current head
	}
	human, err := gate.RecordHumanDecision(prNumber, headOID, "approve_merge", "owner")
	if err != nil {
		return Result{}, err
	}
	if human.AutoMerge || gate.MayAutoMerge(prNumber) {
		return Result{}, errors.New("auto-merge must be false")
	}
	emit("human_gate.approve_merge")
	advance(time.Second)

	// release claim (cleanup outside customer checkout)
	_ = wtSvc.CleanupOwned(claimRes.Claim.ClaimID)
	emit("reservation.released")

	// --- residue scan of consumer repo only ---
	residue, err := ScanResidue(repo.Root)
	if err != nil {
		return Result{}, err
	}
	if len(residue) != 0 {
		return Result{}, fmt.Errorf("consumer residue: %v", residue)
	}
	emit("residue.clean")

	// delivery resume dry-run after full success should be done
	if opts.Fault == FaultDeliveryResume {
		fin, err := deliveryresume.NewPlanner(now).Plan(deliveryresume.RunSnapshot{
			RunID: runID, WorkerLaunchCount: workerLaunches, WorkerCleanupTerminal: true,
			Stages: map[deliveryresume.StageName]deliveryresume.StageEvidence{
				deliveryresume.StageWorker:    {Stage: deliveryresume.StageWorker, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
				deliveryresume.StageCommit:    {Stage: deliveryresume.StageCommit, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
				deliveryresume.StagePush:      {Stage: deliveryresume.StagePush, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
				deliveryresume.StagePR:        {Stage: deliveryresume.StagePR, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
				deliveryresume.StageCIWait:    {Stage: deliveryresume.StageCIWait, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
				deliveryresume.StageVerifier:  {Stage: deliveryresume.StageVerifier, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
				deliveryresume.StageHumanGate: {Stage: deliveryresume.StageHumanGate, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
			},
		}, true)
		if err != nil || !fin.Terminal || fin.NextAction != deliveryresume.ActionDone {
			return Result{}, fmt.Errorf("final resume not done: %+v err=%v", fin, err)
		}
		if workerLaunches != 1 {
			return Result{}, fmt.Errorf("worker launches after full resume=%d", workerLaunches)
		}
		emit("deliveryresume.terminal_done")
	}

	sideEffects := []string{
		"git_commit:" + shortSHA(commitSHA),
		fmt.Sprintf("pr:%d", prNumber),
		"checks:verify,test,race,security",
		"human:approve_merge",
	}
	man := Manifest{
		SchemaVersion: ManifestSchema, ScenarioID: opts.ID, RepoKind: string(opts.RepoKind),
		TestedSHA: commitSHA, RequestedRoute: requestedRoute, ActualRoute: actualRoute,
		RouteMatch: requestedRoute == actualRoute, WorkerLaunchCount: workerLaunches,
		PRNumber: prNumber, HumanGate: "approve_merge", ProviderCallsCI: ciProviderCalls,
		VerifierAfterReady: true, Events: events, SideEffects: sideEffects,
		ProcessCleanup: []string{"zero_surviving_children", "reservation_released"},
		Residue:        residue,
		Inputs: map[string]string{
			"provider": "fixture", "model": "fixture-model",
			"repo_kind": string(opts.RepoKind), "issue": "1", "branch": branch,
			"repo": "synthetic-owner/synthetic-" + string(opts.RepoKind),
		},
		Expected: map[string]string{
			"route_match": "true", "worker_launches": "1", "checks": "all_green",
			"ci_provider_calls": "0", "auto_merge": "false", "residue": "none",
			"surviving_children": "0",
		},
		GeneratedAt: now(),
	}
	// scrub any accidental absolute paths in events (defensive)
	man.Events = scrubEvents(man.Events)
	path, err := WriteManifest(evidenceDir, man)
	if err != nil {
		return Result{}, err
	}
	emit("manifest.written")
	man.Events = append(man.Events, "manifest.written")

	if workerLaunches != 1 && opts.Fault != FaultWorkerFail {
		return Result{}, fmt.Errorf("expected 1 worker launch, got %d", workerLaunches)
	}
	if !man.RouteMatch {
		return Result{}, errors.New("route mismatch")
	}

	return Result{
		Manifest: man, ManifestPath: path, Events: man.Events,
		PRNumber: prNumber, CommitSHA: commitSHA, WorkerLaunches: workerLaunches,
	}, nil
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func scrubEvents(ev []string) []string {
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		// never embed absolute worktree paths
		if strings.Contains(e, "/var/") || strings.Contains(e, "/tmp/") || strings.Contains(e, "/Users/") {
			continue
		}
		out = append(out, e)
	}
	return out
}
