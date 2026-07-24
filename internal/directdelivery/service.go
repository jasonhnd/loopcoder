package directdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
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
	// Empty → derived from Worker.ChangedPaths (or worktree diff vs BaseSHA).
	// Never defaults to a synthetic docs/CHANGE.md marker.
	OwnedPaths []string
	// Branch is the delivery branch name (default ordinary/run-<id>).
	Branch string
	// RequiredChecks for ciwatch. Empty → observe whatever the remote reports;
	// never invent a green fixture set.
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

// Deps injects ports. Nil Git/Remote/GitHub/ObserveCI FAIL CLOSED on production
// path — never silently substitute FakeGit/FakeRemote/FakeGitHub/auto-green CI.
// Tests must inject Fake* ports explicitly.
type Deps struct {
	Now    func() time.Time
	Git    commitstage.Git
	Remote pushstage.Remote
	GitHub prstage.GitHub
	// ObserveCI supplies remote check snapshots for the exact PR head.
	// Nil → fail closed (never auto-green fixture).
	ObserveCI func(ctx context.Context, pr int, head string, checks []string) (ciwatch.RemoteSnapshot, error)
	// VerifierRoute is the read-only verifier route. Empty Provider → fail closed
	// (never invent fixture-verifier success).
	VerifierRoute routepin.Fields
	// VerifierExec launches an independent read-only provider verifier.
	// Production must wire this; nil fails closed (no auto-pass).
	// Tests may set AllowSyntheticVerifier to complete gate without launch.
	VerifierExec func(ctx context.Context, route routepin.Fields, pr int, head string) (pass bool, digest string, err error)
	// AllowSyntheticVerifier permits gate CompleteVerifier without VerifierExec
	// (tests only).
	AllowSyntheticVerifier bool
	// HookExec for hookpolicy; nil = pass-through success only when explicitly
	// allowed via AllowNilHookExec (tests). Production wires a real executor.
	HookExec         func(ctx context.Context, hook string, env []string) (int, []byte, time.Duration, bool, error)
	AllowNilHookExec bool
	// AllowSyntheticLocalVerify records exit-0 without running commands.
	// Production leaves this false and executes plan commands in the worktree.
	AllowSyntheticLocalVerify bool
}

// Service is the production post-worker delivery application service.
type Service struct {
	Deps Deps
}

// Execute runs localverify → commit → hooks → push → PR → CI wait → verifier → human gate.
// Requires worker.State == cleanup-terminal. Never re-launches the worker.
// Never reports human_gate from fake/synthetic evidence.
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
		emit(fmt.Sprintf("worker.launches:%d", w.ProviderLaunchN))
	}
	emit("worker.cleanup_terminal")

	// Fail closed: real ports required (no silent Fake* defaults).
	if s.Deps.Git == nil {
		return fail(out, "git port required (inject commitstage.LocalGit or explicit test FakeGit)")
	}
	if s.Deps.Remote == nil {
		return fail(out, "remote port required (inject pushstage.LocalRemote or explicit test FakeRemote)")
	}
	if s.Deps.GitHub == nil {
		return fail(out, "github port required (inject prstage.GHClient or explicit test FakeGitHub)")
	}
	if s.Deps.ObserveCI == nil {
		return fail(out, "ObserveCI required (exact-head required checks; never auto-green fixture)")
	}
	if strings.TrimSpace(s.Deps.VerifierRoute.Provider) == "" {
		return fail(out, "VerifierRoute required (read-only routed verifier; never fixture-verifier)")
	}

	// Base SHA: worker evidence only — never fixture-base-sha.
	baseSHA := strings.TrimSpace(w.BaseSHA)
	if baseSHA == "" {
		return fail(out, "worker BaseSHA required (no fixture-base-sha)")
	}

	// Owned paths: request → worker ChangedPaths → real worktree diff. Never docs/CHANGE.md invent.
	owned := append([]string(nil), req.OwnedPaths...)
	if len(owned) == 0 {
		owned = append([]string(nil), w.ChangedPaths...)
	}
	if len(owned) == 0 && strings.TrimSpace(w.WorktreePath) != "" {
		if lg, ok := s.Deps.Git.(interface {
			ChangedPathsSince(string) ([]string, error)
		}); ok {
			if paths, err := lg.ChangedPathsSince(baseSHA); err == nil {
				owned = paths
			}
		}
	}
	if len(owned) == 0 {
		return fail(out, "owned/changed paths required (empty product diff is not delivery evidence)")
	}

	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = "ordinary/run-" + short(w.RunID)
	}
	baseBranch := strings.TrimSpace(req.BaseBranch)
	if baseBranch == "" {
		return fail(out, "base branch required")
	}
	checks := append([]string(nil), req.RequiredChecks...)
	if len(checks) == 0 {
		// Empty required-check list is allowed only when ObserveCI supplies exact-head evidence;
		// still fail closed if ObserveCI returns nothing useful later.
		checks = []string{}
	}
	attemptID := w.AttemptID
	if attemptID == "" {
		attemptID = "att_" + short(w.RunID)
	}
	runID := w.RunID
	if strings.TrimSpace(runID) == "" {
		return fail(out, "run id required")
	}

	// --- localverify ---
	plan, err := localverify.BuildPlan(owned)
	if err != nil {
		return fail(out, "localverify plan: "+err.Error())
	}
	var results []localverify.Result
	if s.Deps.AllowSyntheticLocalVerify {
		for _, cmd := range plan.Included {
			results = append(results, localverify.RecordResult(cmd, 0, time.Millisecond, []byte("ok"), false))
		}
	} else {
		wt := strings.TrimSpace(w.WorktreePath)
		if wt == "" {
			return fail(out, "worktree path required for real localverify")
		}
		for _, cmd := range plan.Included {
			r, rerr := runLocalVerifyCmd(ctx, wt, cmd)
			if rerr != nil {
				return fail(out, "localverify exec: "+rerr.Error())
			}
			results = append(results, r)
		}
	}
	if localverify.PlanBlocksDelivery(results) {
		return fail(out, "localverify blocked delivery")
	}
	out.VerifyDigest = plan.Digest
	emit("localverify.ok:" + plan.Digest)

	// --- hooks BEFORE commit (on staged content) ---
	// Discover real hook path via `git rev-parse --git-path hooks/pre-commit`
	// (linked worktrees have .git as a file — never look only under worktree/.git/hooks).
	git := s.Deps.Git
	pol, err := hookpolicy.Freeze(hookpolicy.ModeRespect, false, "",
		hookpolicy.DiscoverPreflight([]string{"pre-commit"}, nil), owned, now())
	if err != nil {
		return fail(out, "hook freeze: "+err.Error())
	}
	exec := s.Deps.HookExec
	if exec == nil {
		if !s.Deps.AllowNilHookExec {
			return fail(out, "HookExec required for production delivery (or AllowNilHookExec for tests)")
		}
		exec = func(context.Context, string, []string) (int, []byte, time.Duration, bool, error) {
			return 0, []byte("ok"), time.Millisecond, false, nil
		}
	}
	// Stage owned paths first so pre-commit sees staged content.
	if stager, ok := git.(interface {
		StagePaths(owned []string, allDirty []string) error
		IndexDirty() ([]string, error)
	}); ok {
		dirty, _ := stager.IndexDirty()
		if serr := stager.StagePaths(owned, dirty); serr != nil {
			return fail(out, "pre-commit stage: "+serr.Error())
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

	// --- commit (after hooks pass) ---
	// Parent must be actual HEAD (or worker BaseSHA when clean relative to base).
	parentSHA := baseSHA
	if head, herr := git.HEAD(); herr == nil && strings.TrimSpace(head) != "" {
		parentSHA = head
	}
	cSvc := &commitstage.Service{Store: commitstage.NewStore(now), Git: git}
	cKey := "idem-commit-" + short(runID)
	cIn, err := cSvc.Freeze(commitstage.Intent{
		AttemptID: attemptID, IdempotencyKey: cKey,
		OwnedPaths: owned, ParentSHA: parentSHA, BaseSHA: baseSHA,
		TreeDigest:  "tree-" + short(strings.Join(owned, ",")),
		Message:     fmt.Sprintf("loopcoder: deliver %s", req.Issue),
		RouteDigest: nonEmpty(w.RouteDigest, "route-missing"), VerificationDigest: plan.Digest,
		WorkerTerminal: true, VerificationOK: true,
	})
	if err != nil {
		return fail(out, "commit freeze: "+err.Error())
	}
	cRec, err := cSvc.CommitOrAdopt(ctx, cIn.IdempotencyKey)
	if err != nil {
		return fail(out, "commit: "+err.Error())
	}
	if _, err := cSvc.CommitOrAdopt(ctx, cIn.IdempotencyKey); err != nil {
		return fail(out, "commit re-adopt: "+err.Error())
	}
	out.CommitSHA = cRec.CommitSHA
	emit("commit.ok:" + short(cRec.CommitSHA))

	// --- push ---
	remote := s.Deps.Remote
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
	if p2, err := pSvc.PushOrAdopt(ctx, pIn.IdempotencyKey); err != nil || !p2.OK {
		return fail(out, "push adopt: "+fmt.Sprint(err))
	}
	emit("push.idempotent_adopt")

	// --- PR ---
	gh := s.Deps.GitHub
	owner, name, oerr := splitRepoStrict(req.Repo)
	if oerr != nil {
		return fail(out, oerr.Error())
	}
	issueN, ierr := parseIssueStrict(req.Issue)
	if ierr != nil {
		return fail(out, ierr.Error())
	}
	prSvc := &prstage.Service{Store: prstage.NewStore(now), GitHub: gh}
	prKey := "idem-pr-" + short(runID)
	prIn, err := prSvc.Freeze(prstage.Intent{
		AttemptID: attemptID, IdempotencyKey: prKey,
		RepoOwner: owner, RepoName: name,
		BaseRef: baseBranch, BaseOID: baseSHA, HeadRef: branch, HeadOID: cRec.CommitSHA,
		SourceIssue: issueN, Title: fmt.Sprintf("loopcoder: %s", req.Issue),
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

	// --- CI watch (zero model) — exact head required checks only ---
	if len(checks) == 0 {
		return fail(out, "RequiredChecks empty: cannot prove CI ready without exact required check names")
	}
	wch := &ciwatch.Watcher{
		Store: ciwatch.NewStore(now), Now: now,
		MinInterval: 15 * time.Second, MaxInterval: time.Minute, ReportEvery: 5 * time.Minute,
	}
	if _, err := wch.Start(prNumber, cRec.CommitSHA, ciwatch.RequirementPolicy{
		RequiredChecks: checks, RequiredApprovals: 0,
	}); err != nil {
		return fail(out, "ciwatch start: "+err.Error())
	}
	snap, err := s.Deps.ObserveCI(ctx, prNumber, cRec.CommitSHA, checks)
	if err != nil {
		return fail(out, "ci observe: "+err.Error())
	}
	if _, _, err := wch.Observe(ctx, snap); err != nil {
		return fail(out, "ciwatch observe: "+err.Error())
	}
	if !wch.Ready(prNumber) {
		// One more honest observe — never invent green.
		snap2, oerr := s.Deps.ObserveCI(ctx, prNumber, cRec.CommitSHA, checks)
		if oerr != nil {
			return fail(out, "ci re-observe: "+oerr.Error())
		}
		_, _, _ = wch.Observe(ctx, snap2)
	}
	if !wch.Ready(prNumber) {
		return fail(out, "ci not ready on exact head (no auto-green)")
	}
	ciwatch.AssertNoProviderDependency()
	emit("ci.ready")

	// --- verifier (read-only) then stop at human gate ---
	gate := mergegate.NewGate(now)
	vRoute := s.Deps.VerifierRoute
	vNorm, err := vRoute.Normalize()
	if err != nil {
		return fail(out, "verifier route: "+err.Error())
	}
	if strings.ToLower(vNorm.Permission) != "read-only" && strings.ToLower(vNorm.Permission) != "readonly" {
		return fail(out, "verifier permission must be read-only")
	}
	pre := mergegate.Precondition{
		WorkerCleanupTerminal: true, PRHeadStable: true, PRHeadOID: cRec.CommitSHA,
		CIReady: true, WorkerSlotFree: true, VerifierSlotFree: true,
	}
	vReq := mergegate.Request{
		AttemptID: attemptID + "-verifier", PRNumber: prNumber, PRHeadOID: cRec.CommitSHA, PRBaseOID: baseSHA,
		IssueSnap: "issue-" + req.Issue, ChecksSnap: "checks-observed",
		Route: vNorm, Permission: "read-only", RouteDigest: vNorm.Digest(),
	}
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
	// Independent read-only verifier launch (not just a route record).
	var vDigest string
	if s.Deps.VerifierExec != nil {
		pass, dig, verr := s.Deps.VerifierExec(ctx, vNorm, prNumber, cRec.CommitSHA)
		if verr != nil {
			return fail(out, "verifier exec: "+verr.Error())
		}
		if !pass {
			return fail(out, "verifier did not pass")
		}
		vDigest = dig
		emit("verifier.exec_ok:" + short(dig))
	} else if !s.Deps.AllowSyntheticVerifier {
		return fail(out, "VerifierExec required (independent read-only provider; no auto-pass)")
	} else {
		vDigest = "synthetic-verifier-test"
		emit("verifier.synthetic_test")
	}
	verdict, err := gate.CompleteVerifier(accepted, mergegate.VerdictPass, vNorm, "routed verifier completed "+vDigest, false)
	if err != nil || verdict.Class != mergegate.VerdictPass {
		return fail(out, fmt.Sprintf("verifier: err=%v class=%s", err, verdict.Class))
	}
	if verdict.Mutated {
		return fail(out, "verifier mutated tree")
	}
	emit("verifier.pass")

	if gate.MayAutoMerge(prNumber) {
		return fail(out, "auto-merge must be false")
	}
	emit("human_gate.await_owner")

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

func runLocalVerifyCmd(ctx context.Context, worktree string, cmd localverify.Command) (localverify.Result, error) {
	if len(cmd.Argv) == 0 {
		return localverify.Result{}, fmt.Errorf("empty argv")
	}
	start := time.Now()
	c := exec.CommandContext(ctx, cmd.Argv[0], cmd.Argv[1:]...)
	c.Dir = worktree
	out, err := c.CombinedOutput()
	dur := time.Since(start)
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = 1
		}
	}
	return localverify.RecordResult(cmd, code, dur, out, false), nil
}

func fail(out Result, msg string) (Result, error) {
	out.Status = StatusBlocked
	out.Error = msg
	out.Message = msg
	return out, fmt.Errorf("directdelivery: %s", msg)
}

func splitRepoStrict(repo string) (owner, name string, err error) {
	raw := strings.TrimSpace(repo)
	if o, n, ok := parseOwnerName(raw); ok {
		return o, n, nil
	}
	// Local path: try origin remote (real evidence, not synthetic invent).
	if strings.HasPrefix(raw, "/") || strings.Contains(raw, string(filepath.Separator)) {
		if o, n, oerr := ownerNameFromGitRemote(raw); oerr == nil {
			return o, n, nil
		}
	}
	return "", "", fmt.Errorf("repo must be owner/name (got %q); no synthetic-owner fallback", repo)
}

func parseOwnerName(repo string) (owner, name string, ok bool) {
	repo = strings.TrimSpace(repo)
	repo = strings.TrimSuffix(repo, ".git")
	if i := strings.LastIndex(repo, ":"); i >= 0 && !strings.Contains(repo[i:], "/") {
		// git@host:owner/name
		repo = repo[i+1:]
	}
	if i := strings.Index(repo, "://"); i >= 0 {
		repo = repo[i+3:]
		if j := strings.Index(repo, "/"); j >= 0 {
			repo = repo[j+1:]
		}
	}
	parts := strings.Split(repo, "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	if len(parts) >= 2 {
		o, n := parts[len(parts)-2], parts[len(parts)-1]
		if o != "" && n != "" && o != "synthetic-owner" {
			return o, n, true
		}
	}
	return "", "", false
}

func ownerNameFromGitRemote(repoPath string) (owner, name string, err error) {
	cmd := exec.Command("git", "-C", repoPath, "remote", "get-url", "origin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", err
	}
	o, n, ok := parseOwnerName(strings.TrimSpace(string(out)))
	if !ok {
		return "", "", fmt.Errorf("cannot parse origin %q", strings.TrimSpace(string(out)))
	}
	return o, n, nil
}

func parseIssueStrict(issue string) (int, error) {
	issue = strings.TrimSpace(strings.TrimPrefix(issue, "#"))
	if issue == "" {
		return 0, fmt.Errorf("issue required; no synthetic issue=1")
	}
	n := 0
	for _, r := range issue {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return 0, fmt.Errorf("issue must be positive integer (got %q); no synthetic fallback", issue)
	}
	return n, nil
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
