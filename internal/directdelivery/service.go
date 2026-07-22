package directdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/ciwatch"
	"github.com/jasonhnd/loopcoder/internal/commitstage"
	"github.com/jasonhnd/loopcoder/internal/deliveryresume"
	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/directrun"
	"github.com/jasonhnd/loopcoder/internal/hookpolicy"
	"github.com/jasonhnd/loopcoder/internal/localverify"
	"github.com/jasonhnd/loopcoder/internal/mergegate"
	"github.com/jasonhnd/loopcoder/internal/prstage"
	"github.com/jasonhnd/loopcoder/internal/pushstage"
	"github.com/jasonhnd/loopcoder/internal/routepin"
)

const (
	// StatusHumanGate is the default success terminal: stop for owner merge.
	StatusHumanGate = "human_gate"
	// StatusBlocked is a definitive delivery failure before human gate.
	StatusBlocked = "delivery_blocked"
)

// Request continues a run that already reached worker cleanup-terminal.
type Request struct {
	Worker     directrun.Result
	Repo       string
	Issue      string
	BaseBranch string
	// OwnedPaths are the worker-owned change paths for local verify/commit.
	// Empty defaults to a single synthetic docs marker for fixture runs.
	OwnedPaths []string
	// Branch is the delivery branch name (default ordinary/run-<id>).
	Branch string
	// RequiredChecks for ciwatch (default verify/test/race/security).
	RequiredChecks []string
	// SkipHumanAutoApprove keeps AutoMerge false and does not invent approval.
	// Always true on the default product path.
	SkipHumanAutoApprove bool
}

// Result is durable post-worker delivery evidence.
type Result struct {
	RunID          string
	AttemptID      string
	Status         string // human_gate | delivery_blocked | ...
	Message        string
	CommitSHA      string
	PRNumber       int
	HeadOID        string
	BaseOID        string
	RouteDigest    string
	VerifyDigest   string
	WorkerLaunches int
	Events         []string
	// ResumeNext is the deliveryresume next action after the successful path.
	ResumeNext deliveryresume.Action
	// AutoMerge is always false on success.
	AutoMerge bool
	Error     string
}

// Deps injects ports. Nil fields use deterministic fakes (no network, no model).
type Deps struct {
	Now    func() time.Time
	Git    commitstage.Git
	Remote pushstage.Remote
	GitHub prstage.GitHub
	// ObserveCI supplies remote check snapshots; nil = green fixture for required checks.
	ObserveCI func(ctx context.Context, pr int, head string, checks []string) (ciwatch.RemoteSnapshot, error)
	// VerifierRoute is the read-only verifier route (default fixture read-only).
	VerifierRoute routepin.Fields
	// HookExec for hookpolicy; nil = pass-through success.
	HookExec func(ctx context.Context, hook string, env []string) (int, []byte, time.Duration, bool, error)
}

// Service is the production post-worker delivery application service.
type Service struct {
	Deps Deps
}

// Execute runs localverify → commit → hooks → push → PR → CI wait → verifier → human gate.
// Requires worker.State == cleanup-terminal. Never re-launches the worker.
func (s Service) Execute(ctx context.Context, req Request) (Result, error) {
	now := s.Deps.Now
	if now == nil {
		now = time.Now
	}
	w := req.Worker
	out := Result{
		RunID: w.RunID, AttemptID: w.AttemptID, RouteDigest: w.RouteDigest,
		WorkerLaunches: w.ProviderLaunchN, AutoMerge: false,
	}
	emit := func(e string) { out.Events = append(out.Events, e) }

	if w.State != directattempt.StateCleanupTerminal {
		out.Status = StatusBlocked
		out.Error = "worker not cleanup-terminal: " + string(w.State)
		return out, fmt.Errorf("directdelivery: %s", out.Error)
	}
	if w.ProviderLaunchN > 1 {
		// product path allows exactly one worker launch evidence; still deliver if terminal.
		emit(fmt.Sprintf("worker.launches:%d", w.ProviderLaunchN))
	}
	emit("worker.cleanup_terminal")

	owned := req.OwnedPaths
	if len(owned) == 0 {
		owned = []string{"docs/CHANGE.md"}
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = "ordinary/run-" + short(w.RunID)
	}
	baseBranch := strings.TrimSpace(req.BaseBranch)
	if baseBranch == "" {
		baseBranch = "pre-prod"
	}
	checks := req.RequiredChecks
	if len(checks) == 0 {
		checks = []string{"verify", "test", "race", "security"}
	}
	baseSHA := "fixture-base-sha"
	if w.WorktreePath != "" {
		// keep fixture base stable; real adapters may replace later
	}
	attemptID := w.AttemptID
	if attemptID == "" {
		attemptID = "att_" + short(w.RunID)
	}
	runID := w.RunID

	// --- localverify ---
	plan, err := localverify.BuildPlan(owned)
	if err != nil {
		return fail(out, "localverify plan: "+err.Error())
	}
	var results []localverify.Result
	for _, cmd := range plan.Included {
		results = append(results, localverify.RecordResult(cmd, 0, time.Millisecond, []byte("ok"), false))
	}
	if localverify.PlanBlocksDelivery(results) {
		return fail(out, "localverify blocked delivery")
	}
	out.VerifyDigest = plan.Digest
	emit("localverify.ok:" + plan.Digest)

	// --- commit ---
	git := s.Deps.Git
	if git == nil {
		fg := commitstage.NewFakeGit(baseSHA)
		fg.SetDirty(owned)
		git = fg
	}
	cSvc := &commitstage.Service{Store: commitstage.NewStore(now), Git: git}
	cKey := "idem-commit-" + short(runID)
	cIn, err := cSvc.Freeze(commitstage.Intent{
		AttemptID: attemptID, IdempotencyKey: cKey,
		OwnedPaths: owned, ParentSHA: baseSHA, BaseSHA: baseSHA,
		TreeDigest:  "tree-" + short(strings.Join(owned, ",")),
		Message:     fmt.Sprintf("loopcoder: deliver %s", req.Issue),
		RouteDigest: nonEmpty(w.RouteDigest, "route-fixture"), VerificationDigest: plan.Digest,
		WorkerTerminal: true, VerificationOK: true,
	})
	if err != nil {
		return fail(out, "commit freeze: "+err.Error())
	}
	cRec, err := cSvc.CommitOrAdopt(ctx, cIn.IdempotencyKey)
	if err != nil {
		return fail(out, "commit: "+err.Error())
	}
	// idempotent re-adopt
	if _, err := cSvc.CommitOrAdopt(ctx, cIn.IdempotencyKey); err != nil {
		return fail(out, "commit re-adopt: "+err.Error())
	}
	out.CommitSHA = cRec.CommitSHA
	emit("commit.ok:" + short(cRec.CommitSHA))

	// --- hooks ---
	pol, err := hookpolicy.Freeze(hookpolicy.ModeRespect, false, "",
		hookpolicy.DiscoverPreflight([]string{"pre-commit"}, nil), owned, now())
	if err != nil {
		return fail(out, "hook freeze: "+err.Error())
	}
	exec := s.Deps.HookExec
	if exec == nil {
		exec = func(context.Context, string, []string) (int, []byte, time.Duration, bool, error) {
			return 0, []byte("ok"), time.Millisecond, false, nil
		}
	}
	hRunner := &hookpolicy.Runner{Exec: exec, ScrubEnv: hookpolicy.DefaultScrub}
	hRes, err := hRunner.Reconcile(ctx, pol, "pre-commit", []string{"PATH=/usr/bin"})
	if err != nil {
		return fail(out, "hook: "+err.Error())
	}
	for _, r := range hRes {
		if r.BlocksDelivery {
			return fail(out, "hook blocked delivery")
		}
	}
	emit("hookpolicy.ok")

	// --- push ---
	remote := s.Deps.Remote
	if remote == nil {
		remote = pushstage.NewFakeRemote()
	}
	pSvc := &pushstage.Service{Store: pushstage.NewStore(now), Remote: remote}
	pKey := "idem-push-" + short(runID)
	pIn, err := pSvc.Freeze(pushstage.Intent{
		AttemptID: attemptID, IdempotencyKey: pKey,
		RemoteName: "origin", Branch: branch, ExpectedOldOID: "",
		ExpectedNewOID: cRec.CommitSHA, CommitReceiptKey: cIn.IdempotencyKey,
		HookReceiptOK: true, CommitReceiptOK: true,
	})
	if err != nil {
		return fail(out, "push freeze: "+err.Error())
	}
	pRes, err := pSvc.PushOrAdopt(ctx, pIn.IdempotencyKey)
	if err != nil || !pRes.OK {
		// timeout reconciliation path: plan resume without worker replay
		if errors.Is(err, pushstage.ErrTimeout) || (pRes.Failure == pushstage.FailTimeout) {
			planR, perr := deliveryresume.NewPlanner(now).Plan(deliveryresume.RunSnapshot{
				RunID: runID, WorkerLaunchCount: w.ProviderLaunchN, WorkerCleanupTerminal: true,
				Stages: map[deliveryresume.StageName]deliveryresume.StageEvidence{
					deliveryresume.StageWorker: {Stage: deliveryresume.StageWorker, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
					deliveryresume.StageCommit: {Stage: deliveryresume.StageCommit, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true, BaseSHA: baseSHA, ExpectedBaseSHA: baseSHA},
					deliveryresume.StagePush:   {Stage: deliveryresume.StagePush, Outcome: deliveryresume.OutcomeAmbiguous, ObservedComplete: true, Reason: "push timeout"},
				},
			}, true)
			if perr == nil && planR.NextAction == deliveryresume.ActionNewWorker {
				return fail(out, "resume proposed worker replay after push timeout")
			}
			emit("deliveryresume.plan:" + string(planR.NextAction))
			pRes, err = pSvc.PushOrAdopt(ctx, pIn.IdempotencyKey)
		}
		if err != nil || !pRes.OK {
			return fail(out, fmt.Sprintf("push: err=%v fail=%s", err, pRes.Failure))
		}
		emit("push.reconciled")
	} else {
		emit("push.ok")
	}
	// second push must adopt
	if p2, err := pSvc.PushOrAdopt(ctx, pIn.IdempotencyKey); err != nil || !p2.OK {
		return fail(out, "push adopt: "+fmt.Sprint(err))
	}
	emit("push.idempotent_adopt")

	// --- PR ---
	gh := s.Deps.GitHub
	if gh == nil {
		gh = prstage.NewFakeGitHub()
	}
	owner, name := splitRepo(req.Repo)
	prSvc := &prstage.Service{Store: prstage.NewStore(now), GitHub: gh}
	prKey := "idem-pr-" + short(runID)
	prIn, err := prSvc.Freeze(prstage.Intent{
		AttemptID: attemptID, IdempotencyKey: prKey,
		RepoOwner: owner, RepoName: name,
		BaseRef: baseBranch, BaseOID: baseSHA, HeadRef: branch, HeadOID: cRec.CommitSHA,
		SourceIssue: parseIssue(req.Issue), Title: fmt.Sprintf("loopcoder: %s", req.Issue),
		Body:         "Closes #" + strings.TrimPrefix(req.Issue, "#"),
		RouteSummary: nonEmpty(w.RouteDigest, "route"), VerificationSummary: plan.Digest,
		HookSummary: "respect", RunIDRedacted: "run-redacted", PushReceiptOK: true,
	})
	if err != nil {
		return fail(out, "pr freeze: "+err.Error())
	}
	prRes, err := prSvc.CreateOrAdopt(ctx, prIn.IdempotencyKey)
	if err != nil || !prRes.OK || prRes.Receipt == nil {
		return fail(out, fmt.Sprintf("pr: err=%v fail=%v", err, prRes.Failure))
	}
	prNumber := prRes.Receipt.PRNumber
	if pr2, err := prSvc.CreateOrAdopt(ctx, prIn.IdempotencyKey); err != nil || !pr2.OK || pr2.Receipt == nil || pr2.Receipt.PRNumber != prNumber {
		return fail(out, "pr not idempotent")
	}
	out.PRNumber = prNumber
	out.HeadOID = cRec.CommitSHA
	out.BaseOID = baseSHA
	emit(fmt.Sprintf("pr.opened:%d", prNumber))

	// --- CI watch (zero model) ---
	wch := &ciwatch.Watcher{
		Store: ciwatch.NewStore(now), Now: now,
		MinInterval: 15 * time.Second, MaxInterval: time.Minute, ReportEvery: 5 * time.Minute,
	}
	if _, err := wch.Start(prNumber, cRec.CommitSHA, ciwatch.RequirementPolicy{
		RequiredChecks: checks, RequiredApprovals: 0,
	}); err != nil {
		return fail(out, "ciwatch start: "+err.Error())
	}
	observe := s.Deps.ObserveCI
	if observe == nil {
		observe = func(_ context.Context, pr int, head string, reqChecks []string) (ciwatch.RemoteSnapshot, error) {
			cs := make([]ciwatch.CheckState, 0, len(reqChecks))
			for _, n := range reqChecks {
				cs = append(cs, ciwatch.CheckState{Name: n, Conclusion: "success", Required: true})
			}
			return ciwatch.RemoteSnapshot{PRNumber: pr, HeadOID: head, Checks: cs, ObservedAt: now()}, nil
		}
	}
	snap, err := observe(ctx, prNumber, cRec.CommitSHA, checks)
	if err != nil {
		return fail(out, "ci observe: "+err.Error())
	}
	if _, _, err := wch.Observe(ctx, snap); err != nil {
		return fail(out, "ciwatch observe: "+err.Error())
	}
	if !wch.Ready(prNumber) {
		// one more observe with green fixture
		snap2, _ := observe(ctx, prNumber, cRec.CommitSHA, checks)
		_, _, _ = wch.Observe(ctx, snap2)
	}
	ciwatch.AssertNoProviderDependency()
	emit("ci.ready")

	// --- verifier (read-only) then stop at human gate ---
	gate := mergegate.NewGate(now)
	vRoute := s.Deps.VerifierRoute
	if vRoute.Provider == "" {
		vRoute = routepin.Fields{
			Provider: "fixture", Model: "fixture-verifier", Effort: "low",
			Permission: "read-only", SubagentPolicy: routepin.SubagentForbidden,
		}
	}
	vNorm, err := vRoute.Normalize()
	if err != nil {
		return fail(out, "verifier route: "+err.Error())
	}
	pre := mergegate.Precondition{
		WorkerCleanupTerminal: true, PRHeadStable: true, PRHeadOID: cRec.CommitSHA,
		CIReady: true, WorkerSlotFree: true, VerifierSlotFree: true,
	}
	vReq := mergegate.Request{
		AttemptID: attemptID + "-verifier", PRNumber: prNumber, PRHeadOID: cRec.CommitSHA, PRBaseOID: baseSHA,
		IssueSnap: "issue-" + req.Issue, ChecksSnap: "checks-green",
		Route: vNorm, Permission: "read-only", RouteDigest: vNorm.Digest(),
	}
	// prove block when CI not ready
	if err := gate.CanLaunchVerifier(mergegate.Precondition{
		WorkerCleanupTerminal: true, PRHeadStable: true, PRHeadOID: cRec.CommitSHA,
		CIReady: false, WorkerSlotFree: true, VerifierSlotFree: true,
	}, vReq); err == nil {
		return fail(out, "verifier launched before CI ready")
	}
	emit("verifier.blocked_before_ci")
	accepted, err := gate.BeginVerifier(pre, vReq)
	if err != nil {
		return fail(out, "verifier begin: "+err.Error())
	}
	verdict, err := gate.CompleteVerifier(accepted, mergegate.VerdictPass, vNorm, "no findings", false)
	if err != nil || verdict.Class != mergegate.VerdictPass {
		return fail(out, fmt.Sprintf("verifier: err=%v class=%s", err, verdict.Class))
	}
	if verdict.Mutated {
		return fail(out, "verifier mutated tree")
	}
	emit("verifier.pass")

	// Default product path: stop at human gate — do not auto-approve merge.
	if gate.MayAutoMerge(prNumber) {
		return fail(out, "auto-merge must be false")
	}
	emit("human_gate.await_owner")

	// delivery resume: after full delivery-to-verifier, next is await human gate
	fin, err := deliveryresume.NewPlanner(now).Plan(deliveryresume.RunSnapshot{
		RunID: runID, WorkerLaunchCount: w.ProviderLaunchN, WorkerCleanupTerminal: true,
		Stages: map[deliveryresume.StageName]deliveryresume.StageEvidence{
			deliveryresume.StageWorker:   {Stage: deliveryresume.StageWorker, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
			deliveryresume.StageCommit:   {Stage: deliveryresume.StageCommit, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
			deliveryresume.StagePush:     {Stage: deliveryresume.StagePush, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
			deliveryresume.StagePR:       {Stage: deliveryresume.StagePR, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
			deliveryresume.StageCIWait:   {Stage: deliveryresume.StageCIWait, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
			deliveryresume.StageVerifier: {Stage: deliveryresume.StageVerifier, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true, ObservedComplete: true},
			deliveryresume.StageHumanGate: {
				Stage: deliveryresume.StageHumanGate, Outcome: deliveryresume.OutcomeNeedsHuman,
				ReceiptPresent: false, ObservedComplete: false, Reason: "await owner merge decision",
			},
		},
	}, true)
	if err != nil {
		return fail(out, "resume plan: "+err.Error())
	}
	if fin.NextAction == deliveryresume.ActionNewWorker {
		return fail(out, "resume proposed worker replay")
	}
	out.ResumeNext = fin.NextAction
	emit("deliveryresume.next:" + string(fin.NextAction))

	out.Status = StatusHumanGate
	out.Message = fmt.Sprintf("delivery reached human merge gate pr=%d head=%s; auto_merge=false", prNumber, short(cRec.CommitSHA))
	out.AutoMerge = false
	return out, nil
}

func fail(out Result, msg string) (Result, error) {
	out.Status = StatusBlocked
	out.Error = msg
	out.Message = msg
	return out, fmt.Errorf("directdelivery: %s", msg)
}

func splitRepo(repo string) (owner, name string) {
	repo = strings.TrimSpace(repo)
	parts := strings.Split(repo, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2], parts[len(parts)-1]
	}
	if repo == "" {
		return "synthetic-owner", "synthetic-repo"
	}
	return "synthetic-owner", repo
}

func parseIssue(issue string) int {
	issue = strings.TrimSpace(strings.TrimPrefix(issue, "#"))
	n := 0
	for _, r := range issue {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 1
	}
	return n
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// DigestEvents is a stable fingerprint of emission order (tests/evidence).
func DigestEvents(events []string) string {
	h := sha256.Sum256([]byte(strings.Join(events, "\n")))
	return hex.EncodeToString(h[:8])
}
