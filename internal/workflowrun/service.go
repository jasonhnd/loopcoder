package workflowrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/waveschedule"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

const (
	StatusHumanGate = "human_gate"
	StatusBlocked   = "workflow_blocked"
	StatusInvalid   = "workflow_invalid"
)

// Request executes one bounded workflow.
type Request struct {
	ProjectID string
	// RunID uniquely namespaces this execution (attempts, worktrees). Empty → generated.
	// Prevents stale/cross-run reuse of attempt IDs and durable worktree paths.
	// On resume, pass the same RunID so attempt IDs stay stable (exactly-once).
	RunID string
	// Definition is the frozen user graph (JSON-serializable).
	Definition workflowdef.Definition
	// Actor is the approving owner identity (required for materialize).
	Actor string
	// Provider/Model optional default child route pin when ChildRoutes lacks an item.
	Provider string
	Model    string
	// ChildRoutes optional per-work-item routes (goalrun auto-route winners).
	ChildRoutes map[string]ChildRoute
	// SameDepthAlternates are production decision-set candidates (fresh
	// machine-readable, hard-eligible) keyed by work item. Used only after typed
	// model_unavailable to claim a distinct attempt generation — never reopen the
	// closed failed attempt.
	SameDepthAlternates map[string][]AlternateCandidate
	// CapacityReroute optional hook: release failed reservation + reserve alternate
	// under the new attempt id (goalrun ledger wiring).
	CapacityReroute CapacityRerouteHook
	// RepoPath optional base repo for child worktrees and goal-branch integration.
	RepoPath string
	// BaseRef is the starting point for the shared goal branch (default main).
	BaseRef string
	// GoalBranch is the shared branch all succeeded children integrate into.
	// Empty → loopcoder/goal-<runID>. Required for product PR (not receipt-only).
	GoalBranch string
	// SkipIntegrate disables product-branch integration (tests without a git repo).
	// Production with RepoPath set always integrates on success.
	SkipIntegrate bool
	// MaxWaves hard cap (default 32).
	MaxWaves int
	// PriorSucceeded seeds already-terminal succeeded children from a prior
	// interrupted run. Same attempt_id is reused; executor is NOT re-invoked
	// (exactly-once provider call / file / capacity).
	PriorSucceeded map[string]ChildOutcome
	// AttemptGeneration bumps attempt IDs for aborted/in-flight children on resume
	// (completed children stay generation 0 / prior attempt). Key = work item id.
	AttemptGeneration map[string]int
	// Integrator injects branch integration (tests). nil → GitBranchIntegrator
	// when RepoPath is set and SkipIntegrate is false.
	Integrator BranchIntegrator
	// EventLogPath optional pre-opened path; empty → open under HomeDir/project/run.
	EventLogPath string
}

// AlternateCandidate is one same-depth hard-eligible route for model_unavailable
// generation-safe reroute. Effort must match the failed attempt's required depth.
type AlternateCandidate struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Permission   string `json:"permission,omitempty"`
	HardEligible bool   `json:"hard_eligible"`
	SoftExcluded bool   `json:"soft_excluded"`
}

// CapacityRerouteInput binds failed → alternate capacity transfer (no double hold).
// Call only after the new attempt has been claimed under NewAttemptID.
type CapacityRerouteInput struct {
	WorkItemID       string
	FailedAttemptID  string // workflow attempt id (audit)
	PriorHoldAttempt string // ledger attempt id for prior reservation (often work-item id)
	NewAttemptID     string
	FailedProvider   string
	FailedModel      string
	AltProvider      string
	AltModel         string
	Depth            string
	RouteReason      string
	// PriorActual/Source: when known from failed child, reconcile; else honest release.
	PriorActual     *float64
	PriorSource     string
	SupersedesEvent string
}

// CapacityRerouteResult binds the alternate reservation (account must not cross companies).
type CapacityRerouteResult struct {
	AccountRef     string
	WindowKind     string
	PriorState     string // released|reconciled
	AlternateState string // reserved
	ReservationID  string
}

// CapacityRerouteHook transfers capacity after a new attempt claim.
// Must not invent capacity or double-hold. Returns alternate account_ref.
type CapacityRerouteHook interface {
	OnModelUnavailableAlternate(in CapacityRerouteInput) (CapacityRerouteResult, error)
}

// ChildOutcome is per-child terminal + capacity-relevant evidence for reports.
type ChildOutcome struct {
	WorkItemID     string   `json:"work_item_id"`
	Provider       string   `json:"provider,omitempty"`
	Model          string   `json:"model,omitempty"`
	Depth          string   `json:"depth,omitempty"`
	AccountRef     string   `json:"account_ref,omitempty"`
	RouteReason    string   `json:"route_reason,omitempty"`
	Terminal       string   `json:"terminal,omitempty"`
	OutputEvidence string   `json:"output_evidence,omitempty"`
	WorktreePath   string   `json:"worktree_path,omitempty"`
	AttemptID      string   `json:"attempt_id,omitempty"`
	ExitCode       int      `json:"exit_code,omitempty"`
	FailureClass   string   `json:"failure_class,omitempty"`
	Message        string   `json:"message,omitempty"`
	ActualCapacity *float64 `json:"actual_capacity,omitempty"`
	ActualSource   string   `json:"actual_source,omitempty"`
	FilesTouched   []string `json:"files_touched,omitempty"`
	// IntegrateCommitSHA is the goal-branch commit that absorbed this child (if any).
	IntegrateCommitSHA string `json:"integrate_commit_sha,omitempty"`
	// SupersedesAttemptID when this outcome is a generation-safe alternate.
	SupersedesAttemptID string `json:"supersedes_attempt_id,omitempty"`
	// RerouteEventRef binds durable model_unavailable → reroute event evidence.
	RerouteEventRef string `json:"reroute_event_ref,omitempty"`
}

// Result is durable parent evidence.
type Result struct {
	Status         string
	Message        string
	GraphID        string
	PlanDigest     string
	RunID          string // execution namespace (unique per parent launch)
	GraphVersion   int
	ClaimCount     int
	LaunchCount    int // child launches (== claims on success path)
	ReuseCount     int // prior-succeeded children reused without re-exec
	Integrated     []string
	Children       []ChildOutcome
	Events         []string
	DirectRunEquiv bool
	AutoMerge      bool
	Error          string
	// Ceilings are best-effort peaks observed during this process (restart evidence).
	WorktreePeak int `json:"worktree_peak,omitempty"`
	ProcessPeak  int `json:"process_peak,omitempty"`
	// GoalBranch is the shared product branch (child integrations land here).
	GoalBranch string `json:"goal_branch,omitempty"`
	// IntegrateCommits are exactly-once product commits onto GoalBranch.
	IntegrateCommits []IntegrateCommit `json:"integrate_commits,omitempty"`
	// EventLogPath is the append-only raw event JSONL for interrupt evidence.
	EventLogPath string `json:"event_log_path,omitempty"`
	// Interrupted true when a forced interrupt was recorded this process.
	Interrupted bool `json:"interrupted,omitempty"`
	// AbortedAttempts maps work_item_id → aborted attempt_id (must get new gen on resume).
	AbortedAttempts map[string]string `json:"aborted_attempts,omitempty"`
}

// Service runs bounded workflows.
type Service struct {
	Now func() time.Time
	// Executor runs claimed children. nil → DefaultChildExecutor (production real path).
	// Focused tests inject FakeChildExecutor; remote acceptance must use production.
	Executor ChildExecutor
	// HomeDir overrides layout for child worktrees (tests).
	HomeDir string
}

// Execute freezes, materializes, claims, runs LoopCoder-owned children, integrates;
// never auto-merges. Children are executed via ChildExecutor (production default
// calls the routed provider in an exclusive worktree — never fake Claim→Close).
func (s Service) Execute(ctx context.Context, req Request) (Result, error) {
	now := s.Now
	if now == nil {
		now = time.Now
	}
	t0 := now().UTC()
	out := Result{AutoMerge: false}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		// Unique per parent launch — never reuse att-* across disposable canaries.
		runID = "run_" + short(fmt.Sprintf("%s|%d|%s", req.ProjectID, t0.UnixNano(), req.Actor))
	}
	out.RunID = runID
	emit := func(e string) { out.Events = append(out.Events, e) }

	if ctx.Err() != nil {
		return fail(out, StatusBlocked, "context cancelled")
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		projectID = "local-project"
	}
	// Shared goal branch: all product integrations land here; later children
	// materialize from this branch so they see prior integrated state.
	goalBranch := strings.TrimSpace(req.GoalBranch)
	if goalBranch == "" {
		goalBranch = "loopcoder/goal-" + sanitizeBranch(runID)
	}
	out.GoalBranch = goalBranch
	baseRef := strings.TrimSpace(req.BaseRef)
	if baseRef == "" {
		baseRef = "main"
	}
	var integrator BranchIntegrator
	// Product integration requires a real git repository path. Fake/missing
	// paths (unit tests that only set RepoPath for inventory) skip integrate.
	doIntegrate := !req.SkipIntegrate && isGitRepo(req.RepoPath)
	if doIntegrate {
		integrator = req.Integrator
		if integrator == nil {
			integrator = GitBranchIntegrator{Now: now}
		}
		if _, err := integrator.EnsureGoalBranch(ctx, req.RepoPath, baseRef, goalBranch); err != nil {
			return fail(out, StatusBlocked, "ensure goal branch: "+err.Error())
		}
		emit("goal_branch.ready:" + goalBranch)
	}

	// Append-only event log (forced interrupt / exactly-once evidence source).
	elog, _ := OpenEventLog(s.HomeDir, projectID, runID)
	if elog != nil {
		out.EventLogPath = elog.Path()
		// Recover interrupt facts for open launches left by hard process kill
		// (parent exited before graceful cancel wrote interrupt). Ledger-derived only.
		if n, rerr := RecoverOpenLaunchInterrupts(elog, projectID, runID); rerr == nil && n > 0 {
			out.Interrupted = true
			if events, rerr2 := elog.ReadAll(); rerr2 == nil {
				_, aborted := InterruptedFromEvents(events)
				if out.AbortedAttempts == nil {
					out.AbortedAttempts = map[string]string{}
				}
				for id, att := range aborted {
					out.AbortedAttempts[id] = att
				}
			}
			emit(fmt.Sprintf("interrupt:recover_open_launches n=%d", n))
		}
		_, _ = elog.Append(Event{ProjectID: projectID, RunID: runID, Kind: "run.start", Message: "workflow execute"})
	}
	// logEv persists and returns the durable event. Failures return error —
	// model_unavailable/reroute/claim linkage must not invent event IDs.
	logEv := func(ev Event) (Event, error) {
		if elog == nil {
			return Event{}, fmt.Errorf("workflowrun: event log unavailable")
		}
		ev.ProjectID, ev.RunID = projectID, runID
		out, err := elog.Append(ev)
		if err != nil {
			return Event{}, err
		}
		if strings.TrimSpace(out.EventID) == "" {
			return Event{}, fmt.Errorf("workflowrun: empty persisted event_id kind=%s", ev.Kind)
		}
		return out, nil
	}
	// logEvBestEffort for non-linkage lifecycle lines (interrupt recovery paths).
	logEvBestEffort := func(ev Event) {
		if _, err := logEv(ev); err != nil {
			emit("event_log_warn:" + err.Error())
		}
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "owner"
	}
	defaultProvider := strings.TrimSpace(req.Provider)
	if defaultProvider == "" {
		defaultProvider = "fixture"
	}
	defaultModel := strings.TrimSpace(req.Model)
	if defaultModel == "" {
		defaultModel = "fixture-model"
	}
	maxWaves := req.MaxWaves
	if maxWaves <= 0 {
		maxWaves = 32
	}

	exec := s.Executor
	if exec == nil {
		exec = ProductionChildExecutor{HomeDir: s.HomeDir, Now: now}
	}

	def := req.Definition
	if def.SchemaVersion == 0 {
		def.SchemaVersion = 1
	}
	if strings.TrimSpace(def.Source) == "" {
		def.Source = "explicit_definition"
	}

	// --- freeze plan (no side effects) ---
	plan, err := workflowdef.Normalize(def)
	if err != nil {
		return fail(out, StatusInvalid, "normalize: "+err.Error())
	}
	out.PlanDigest = plan.Digest
	emit("plan.ok:" + short(plan.Digest))

	// --- approve + materialize ---
	ap, err := workflowdef.Approve(plan.Digest, actor, "bounded workflow run", t0)
	if err != nil {
		return fail(out, StatusBlocked, "approve: "+err.Error())
	}
	reg := workflowdef.NewRegistry()
	mat, err := reg.Materialize(projectID, def, ap, t0)
	if err != nil {
		// invalid/cyclic/oversized must create zero claims
		return fail(out, StatusInvalid, "materialize: "+err.Error())
	}
	g := mat.Graph
	out.GraphID = g.GraphID
	out.GraphVersion = g.Version
	out.DirectRunEquiv = g.DirectRunEquivalent
	emit(fmt.Sprintf("materialize.ok items=%d equiv=%v", len(g.Items), g.DirectRunEquivalent))

	// --- schedule + claim + real child execute each ready item once ---
	cs := workclaim.NewStore(now)
	ev := workgraph.TerminalEvidence{}
	claimed := map[string]int{}
	launches := 0
	integrated := []string{}
	itemByID := map[string]workgraph.WorkItem{}
	for _, it := range g.Items {
		itemByID[it.ID] = it
	}

	bounds := waveschedule.DefaultBounds()
	for wave := 0; wave < maxWaves; wave++ {
		if ctx.Err() != nil {
			out.Interrupted = true
			logEvBestEffort(Event{Kind: "interrupt", Message: "cancelled mid-wave (forced process interrupt)"})
			emit("interrupt:mid_wave")
			_ = writePartialPrior(s.HomeDir, projectID, runID, out)
			return fail(out, StatusBlocked, "cancelled mid-wave")
		}
		ready := workgraph.EvaluateReady(g, ev)
		if !ready.Valid {
			return fail(out, StatusBlocked, "ready invalid: "+strings.Join(ready.Errors, ";"))
		}
		if len(ready.Ready) == 0 {
			// check all terminal
			if allTerminal(g, ev) {
				emit("waves.complete")
				break
			}
			return fail(out, StatusBlocked, "no ready items but graph incomplete")
		}

		// wave plan under budgets (deterministic order)
		snap := waveschedule.Snapshot{
			Graph: g, Evidence: ev, Bounds: bounds, WaveSeq: wave,
		}
		wp, err := waveschedule.PlanWave(snap)
		if err != nil {
			return fail(out, StatusBlocked, "wave plan: "+err.Error())
		}
		members := ready.Ready
		if len(wp.Members) > 0 {
			members = nil
			for _, m := range wp.Members {
				members = append(members, m.WorkItemID)
			}
		}
		emit(fmt.Sprintf("wave.%d ready=%d", wave, len(members)))

		for _, id := range members {
			if claimed[id] > 0 {
				continue
			}
			// Exactly-once resume: prior succeeded outcome with stable attempt_id
			// is restored without re-claim / re-exec (no second provider call,
			// file write, or capacity deduction). ClaimCount/LaunchCount stay
			// for real claims only; ReuseCount records durable reuses.
			if prior, ok := req.PriorSucceeded[id]; ok &&
				strings.EqualFold(strings.TrimSpace(prior.Terminal), string(workgraph.TermSucceeded)) &&
				strings.TrimSpace(prior.AttemptID) != "" &&
				strings.TrimSpace(prior.OutputEvidence) != "" {
				// Mark claimed so claim-once map stays consistent without a second claim.
				claimed[id] = 1
				out.ReuseCount++
				prior.WorkItemID = id
				if prior.Terminal == "" {
					prior.Terminal = string(workgraph.TermSucceeded)
				}
				ev[id] = workgraph.TermSucceeded
				out.Children = append(out.Children, prior)
				// Prior succeeded already integrated; do not re-exec or re-commit.
				integrated = append(integrated, id)
				emit(fmt.Sprintf("child.reuse:%s attempt=%s evidence=%s", id, prior.AttemptID, short(prior.OutputEvidence)))
				logEvBestEffort(Event{Kind: "reuse", WorkItemID: id, AttemptID: prior.AttemptID, Evidence: prior.OutputEvidence})
				if prior.WorktreePath != "" {
					out.WorktreePeak++
				}
				continue
			}
			// Bind attempt to plan digest AND unique run ID (no cross-run reuse).
			// Generation bumps for aborted/in-flight children on forced-interrupt resume.
			gen := 0
			if req.AttemptGeneration != nil {
				gen = req.AttemptGeneration[id]
			}
			attemptID := fmt.Sprintf("att-%s-%s-g%d", id, short(out.PlanDigest+"|"+runID), gen)
			res, err := cs.Claim(workclaim.ClaimRequest{
				ProjectID: projectID, Graph: g, Evidence: ev, WorkItemID: id,
				AttemptID:  attemptID,
				ExecutorID: "workflowrun", Lease: time.Minute,
			})
			if err != nil || res.Code != workclaim.ResultClaimed {
				// Already-running same attempt is treated as conflict unless prior seed.
				return fail(out, StatusBlocked, fmt.Sprintf("claim %s: %v code=%v", id, err, res.Code))
			}
			claimed[id]++
			out.ClaimCount++
			launches++
			out.LaunchCount = launches
			out.ProcessPeak++
			logEvBestEffort(Event{Kind: "claim", WorkItemID: id, AttemptID: attemptID, Generation: gen})

			route := resolveChildRoute(req.ChildRoutes, id, defaultProvider, defaultModel)
			emit(fmt.Sprintf("child.launch:%s route=%s/%s depth=%s gen=%d", id, route.Provider, route.Model, route.Depth, gen))
			logEvBestEffort(Event{Kind: "launch", WorkItemID: id, AttemptID: attemptID, Generation: gen,
				Message: route.Provider + "/" + route.Model})

			it := itemByID[id]
			readOnly := strings.Contains(strings.ToLower(it.RouteRequirement), "read-only") ||
				strings.Contains(strings.ToLower(it.RouteRequirement), "readonly") ||
				strings.EqualFold(it.Owner, "research")

			childIn := ChildExecInput{
				ProjectID: projectID, GraphID: g.GraphID, WorkItemID: id,
				ClaimID: res.Claim.ClaimID, AttemptID: attemptID,
				Intent: it.Intent, Route: route, RepoPath: req.RepoPath,
				// Materialize child from goal branch so prior integrations are visible.
				BaseRef:  firstNonEmpty(goalBranch, baseRef),
				ReadOnly: readOnly,
			}
			childOut, cerr := exec.Execute(ctx, childIn)
			if childOut.ProcessPID > 0 {
				logEvBestEffort(Event{Kind: "pid", WorkItemID: id, AttemptID: attemptID, PID: childOut.ProcessPID})
			}
			if childOut.WorktreePath != "" {
				out.WorktreePeak++
			}
			outcome := ChildOutcome{
				WorkItemID: id, Provider: firstNonEmpty(childOut.Provider, route.Provider),
				Model:      firstNonEmpty(childOut.Model, route.Model),
				Depth:      firstNonEmpty(childOut.Depth, route.Depth),
				AccountRef: route.AccountRef, RouteReason: route.RouteReason,
				AttemptID: attemptID, WorktreePath: childOut.WorktreePath,
				OutputEvidence: childOut.OutputEvidence, ExitCode: childOut.ExitCode,
				FailureClass: childOut.FailureClass, Message: childOut.Message,
				ActualCapacity: childOut.ActualCapacity, ActualSource: childOut.ActualSource,
				FilesTouched: childOut.FilesTouched,
			}

			term := childOut.Terminal
			if term == workgraph.TermNone {
				if cerr != nil {
					term = workgraph.TermFailed
				} else {
					term = workgraph.TermFailed
					outcome.FailureClass = "missing_terminal"
					outcome.Message = "executor returned no terminal"
				}
			}
			// Context cancel mid-child → cancelled terminal + forced interrupt record.
			if ctx.Err() != nil && term != workgraph.TermSucceeded {
				term = workgraph.TermCancelled
				if outcome.FailureClass == "" {
					outcome.FailureClass = "forced_interrupt"
				}
				out.Interrupted = true
				if out.AbortedAttempts == nil {
					out.AbortedAttempts = map[string]string{}
				}
				out.AbortedAttempts[id] = attemptID
				logEvBestEffort(Event{Kind: "interrupt", WorkItemID: id, AttemptID: attemptID,
					PID: childOut.ProcessPID, Message: "forced interrupt; attempt aborted", Terminal: string(term)})
				emit(fmt.Sprintf("interrupt:%s attempt=%s pid=%d", id, attemptID, childOut.ProcessPID))
			}
			outcome.Terminal = string(term)

			// Close claim only with real output evidence on success; failure/cancel
			// still close the claim so capacity/wave recovery can proceed idempotently.
			closeTerm := term
			closeEvidence := childOut.OutputEvidence
			if closeTerm == workgraph.TermSucceeded {
				if strings.TrimSpace(closeEvidence) == "" {
					closeTerm = workgraph.TermFailed
					outcome.Terminal = string(closeTerm)
					outcome.FailureClass = "missing_evidence"
					outcome.Message = "refusing success close without output evidence"
					closeEvidence = "failed:missing_evidence:" + id
				}
			} else if strings.TrimSpace(closeEvidence) == "" {
				closeEvidence = "failed:" + firstNonEmpty(outcome.FailureClass, string(closeTerm)) + ":" + id
			}

			_, closeErr := cs.Close(workclaim.CloseRequest{
				ClaimID: res.Claim.ClaimID, Generation: res.Claim.Generation,
				ExecutorID: "workflowrun", AttemptID: res.Claim.AttemptID,
				Terminal: closeTerm, OutputEvidence: closeEvidence,
			})
			if closeErr != nil {
				outcome.Message = "close: " + closeErr.Error()
				out.Children = append(out.Children, outcome)
				return fail(out, StatusBlocked, "close "+id+": "+closeErr.Error())
			}
			logEvBestEffort(Event{Kind: "terminal", WorkItemID: id, AttemptID: attemptID,
				Terminal: string(closeTerm), Evidence: closeEvidence, Message: outcome.FailureClass})

			// Generation-safe same-depth alternate after typed model_unavailable:
			// prior attempt stays immutable; claim new AttemptID first, then
			// transfer capacity, then launch. Final effective childOut drives
			// accept/integrate (never failed attempt files/worktree).
			effectiveOut := childOut
			if closeTerm != workgraph.TermSucceeded &&
				strings.EqualFold(strings.TrimSpace(outcome.FailureClass), "model_unavailable") &&
				ctx.Err() == nil {
				failedAttemptID := attemptID
				failedProv, failedModel := outcome.Provider, outcome.Model
				failedActual, failedSource := childOut.ActualCapacity, childOut.ActualSource
				reqDepth := firstNonEmpty(route.Depth, outcome.Depth)
				reqPerm := "bounded_write"
				if readOnly {
					reqPerm = "read-only"
				}
				alt := pickSameDepthAlternate(req.SameDepthAlternates[id], failedProv, failedModel, reqDepth, reqPerm)
				if alt.Provider != "" {
					evFail, lerr := logEv(Event{
						Kind: "model_unavailable", WorkItemID: id, AttemptID: failedAttemptID,
						Terminal: string(closeTerm), Message: failedProv + "/" + failedModel,
						Payload: eventJSONPayload(map[string]string{
							"attempt_id": failedAttemptID,
							"provider":   failedProv,
							"model":      failedModel,
						}),
						Evidence: closeEvidence, Generation: gen,
					})
					if lerr != nil {
						return fail(out, StatusBlocked, "model_unavailable event: "+lerr.Error())
					}
					failedOutcome := outcome
					out.Children = append(out.Children, failedOutcome)

					newGen := gen + 1
					newAttemptID := fmt.Sprintf("att-%s-%s-g%d", id, short(out.PlanDigest+"|"+runID), newGen)

					// 1) Claim new immutable attempt BEFORE capacity transfer.
					res2, cerr2 := cs.Claim(workclaim.ClaimRequest{
						ProjectID: projectID, Graph: g, Evidence: ev, WorkItemID: id,
						AttemptID: newAttemptID, ExecutorID: "workflowrun", Lease: time.Minute,
						SupersedesAttemptID: failedAttemptID,
					})
					if cerr2 != nil || res2.Code != workclaim.ResultClaimed {
						outcome.Message = fmt.Sprintf("alternate claim: %v code=%v", cerr2, res2.Code)
						ev[id] = closeTerm
						out.Children = append(out.Children, outcome)
						return fail(out, StatusBlocked, "alternate claim "+id+": "+outcome.Message)
					}
					claimed[id]++
					out.ClaimCount++
					launches++
					out.LaunchCount = launches
					out.ProcessPeak++

					evClaim, lerr := logEv(Event{
						Kind: "claim", WorkItemID: id, AttemptID: newAttemptID, Generation: newGen,
						Payload: eventJSONPayload(map[string]string{
							"supersedes_attempt_id": failedAttemptID,
							"retry_attempt_id":      newAttemptID,
						}),
					})
					if lerr != nil {
						// Fail closed: close new claim without capacity.
						_, _ = cs.Close(workclaim.CloseRequest{
							ClaimID: res2.Claim.ClaimID, Generation: res2.Claim.Generation,
							ExecutorID: "workflowrun", AttemptID: res2.Claim.AttemptID,
							Terminal: workgraph.TermFailed, OutputEvidence: "failed:event_log:" + id,
						})
						return fail(out, StatusBlocked, "claim event: "+lerr.Error())
					}

					altRoute := ChildRoute{
						Provider: alt.Provider, Model: alt.Model, Depth: reqDepth,
						RouteReason: fmt.Sprintf(
							"model_unavailable_reroute from %s/%s attempt=%s; winner=%s/%s depth=%s permission=%s",
							failedProv, failedModel, failedAttemptID, alt.Provider, alt.Model, reqDepth, reqPerm,
						),
					}

					// 2) Capacity transfer only after claim; on failure close new claim typed.
					var capRes CapacityRerouteResult
					if req.CapacityReroute != nil {
						// Prior hold attempt is typically work-item id (goalrun reserve key).
						priorHold := id
						cr, rerr := req.CapacityReroute.OnModelUnavailableAlternate(CapacityRerouteInput{
							WorkItemID: id, FailedAttemptID: failedAttemptID, PriorHoldAttempt: priorHold,
							NewAttemptID:   newAttemptID,
							FailedProvider: failedProv, FailedModel: failedModel,
							AltProvider: alt.Provider, AltModel: alt.Model, Depth: reqDepth,
							RouteReason: altRoute.RouteReason,
							PriorActual: failedActual, PriorSource: failedSource,
							SupersedesEvent: "event_id=" + evFail.EventID + ";claim_event_id=" + evClaim.EventID,
						})
						if rerr != nil {
							_, _ = cs.Close(workclaim.CloseRequest{
								ClaimID: res2.Claim.ClaimID, Generation: res2.Claim.Generation,
								ExecutorID: "workflowrun", AttemptID: res2.Claim.AttemptID,
								Terminal:       workgraph.TermFailed,
								OutputEvidence: "failed:capacity_refused:" + id,
							})
							outcome = ChildOutcome{
								WorkItemID: id, Provider: alt.Provider, Model: alt.Model, Depth: reqDepth,
								AttemptID: newAttemptID, Terminal: string(workgraph.TermFailed),
								FailureClass: "capacity_refused", Message: rerr.Error(),
								SupersedesAttemptID: failedAttemptID,
							}
							ev[id] = workgraph.TermFailed
							out.Children = append(out.Children, outcome)
							return fail(out, StatusBlocked, "capacity reroute "+id+": "+rerr.Error())
						}
						capRes = cr
						altRoute.AccountRef = strings.TrimSpace(cr.AccountRef)
					}

					evReroute, lerr := logEv(Event{
						Kind: "reroute", WorkItemID: id, AttemptID: newAttemptID, Generation: newGen,
						Message: altRoute.RouteReason,
						Payload: eventJSONPayload(map[string]string{
							"supersedes_attempt_id":      failedAttemptID,
							"retry_attempt_id":           newAttemptID,
							"alt_provider":               alt.Provider,
							"alt_model":                  alt.Model,
							"depth":                      reqDepth,
							"permission":                 reqPerm,
							"account_ref":                altRoute.AccountRef,
							"model_unavailable_event_id": evFail.EventID,
							"claim_event_id":             evClaim.EventID,
						}),
					})
					if lerr != nil {
						_, _ = cs.Close(workclaim.CloseRequest{
							ClaimID: res2.Claim.ClaimID, Generation: res2.Claim.Generation,
							ExecutorID: "workflowrun", AttemptID: res2.Claim.AttemptID,
							Terminal: workgraph.TermFailed, OutputEvidence: "failed:reroute_event:" + id,
						})
						return fail(out, StatusBlocked, "reroute event: "+lerr.Error())
					}
					rerouteRef := fmt.Sprintf(
						"event_id=%s;event_id=%s;event_id=%s;supersedes_attempt_id=%s;retry_attempt_id=%s",
						evFail.EventID, evClaim.EventID, evReroute.EventID, failedAttemptID, newAttemptID,
					)
					_ = capRes
					emit(fmt.Sprintf("child.reroute:%s from=%s/%s to=%s/%s gen=%d", id, failedProv, failedModel, alt.Provider, alt.Model, newGen))
					if _, lerr := logEv(Event{Kind: "launch", WorkItemID: id, AttemptID: newAttemptID, Generation: newGen,
						Message: alt.Provider + "/" + alt.Model,
						Payload: eventJSONPayload(map[string]string{"retry_attempt_id": newAttemptID}),
					}); lerr != nil {
						_, _ = cs.Close(workclaim.CloseRequest{
							ClaimID: res2.Claim.ClaimID, Generation: res2.Claim.Generation,
							ExecutorID: "workflowrun", AttemptID: res2.Claim.AttemptID,
							Terminal: workgraph.TermFailed, OutputEvidence: "failed:launch_event:" + id,
						})
						return fail(out, StatusBlocked, "launch event: "+lerr.Error())
					}

					childIn2 := ChildExecInput{
						ProjectID: projectID, GraphID: g.GraphID, WorkItemID: id,
						ClaimID: res2.Claim.ClaimID, AttemptID: newAttemptID,
						Intent: it.Intent, Route: altRoute, RepoPath: req.RepoPath,
						BaseRef: firstNonEmpty(goalBranch, baseRef), ReadOnly: readOnly,
					}
					childOut2, _ := exec.Execute(ctx, childIn2)
					if childOut2.ProcessPID > 0 {
						logEvBestEffort(Event{Kind: "pid", WorkItemID: id, AttemptID: newAttemptID, PID: childOut2.ProcessPID})
					}
					if childOut2.WorktreePath != "" {
						out.WorktreePeak++
					}
					// Final effective output is the alternate — never first failed childOut.
					effectiveOut = childOut2
					outcome = ChildOutcome{
						WorkItemID: id, Provider: firstNonEmpty(childOut2.Provider, alt.Provider),
						Model:      firstNonEmpty(childOut2.Model, alt.Model),
						Depth:      firstNonEmpty(childOut2.Depth, reqDepth),
						AccountRef: altRoute.AccountRef, RouteReason: altRoute.RouteReason,
						AttemptID: newAttemptID, WorktreePath: childOut2.WorktreePath,
						OutputEvidence: childOut2.OutputEvidence, ExitCode: childOut2.ExitCode,
						FailureClass: childOut2.FailureClass, Message: childOut2.Message,
						ActualCapacity: childOut2.ActualCapacity, ActualSource: childOut2.ActualSource,
						FilesTouched:        childOut2.FilesTouched,
						SupersedesAttemptID: failedAttemptID, RerouteEventRef: rerouteRef,
					}
					term2 := childOut2.Terminal
					if term2 == workgraph.TermNone {
						term2 = workgraph.TermFailed
						if outcome.FailureClass == "" {
							outcome.FailureClass = "missing_terminal"
						}
					}
					if ctx.Err() != nil && term2 != workgraph.TermSucceeded {
						term2 = workgraph.TermCancelled
						outcome.FailureClass = firstNonEmpty(outcome.FailureClass, "forced_interrupt")
						out.Interrupted = true
						if out.AbortedAttempts == nil {
							out.AbortedAttempts = map[string]string{}
						}
						out.AbortedAttempts[id] = newAttemptID
					}
					closeTerm = term2
					closeEvidence = childOut2.OutputEvidence
					if closeTerm == workgraph.TermSucceeded && strings.TrimSpace(closeEvidence) == "" {
						closeTerm = workgraph.TermFailed
						outcome.FailureClass = "missing_evidence"
						closeEvidence = "failed:missing_evidence:" + id
					} else if closeTerm != workgraph.TermSucceeded && strings.TrimSpace(closeEvidence) == "" {
						closeEvidence = "failed:" + firstNonEmpty(outcome.FailureClass, string(closeTerm)) + ":" + id
					}
					outcome.Terminal = string(closeTerm)
					if _, c2err := cs.Close(workclaim.CloseRequest{
						ClaimID: res2.Claim.ClaimID, Generation: res2.Claim.Generation,
						ExecutorID: "workflowrun", AttemptID: res2.Claim.AttemptID,
						Terminal: closeTerm, OutputEvidence: closeEvidence,
					}); c2err != nil {
						outcome.Message = "close alternate: " + c2err.Error()
						out.Children = append(out.Children, outcome)
						return fail(out, StatusBlocked, "close alternate "+id+": "+c2err.Error())
					}
					attemptID = newAttemptID
					gen = newGen
					route = altRoute
					logEvBestEffort(Event{Kind: "terminal", WorkItemID: id, AttemptID: attemptID,
						Terminal: string(closeTerm), Evidence: closeEvidence, Message: outcome.FailureClass})
				}
			}

			// Logical terminal evidence only after final attempt for this child.
			ev[id] = closeTerm

			// Task-specific acceptance: clarification/empty/no-tests must not succeed.
			// Use final effective output (alternate childOut2 when rerouted).
			if closeTerm == workgraph.TermSucceeded {
				if aerr := AcceptSucceededChild(id, it.Intent, it.Owner, effectiveOut.FilesTouched, effectiveOut.WorktreePath, closeEvidence); aerr != nil {
					closeTerm = workgraph.TermFailed
					outcome.Terminal = string(closeTerm)
					outcome.FailureClass = "acceptance_failed"
					outcome.Message = aerr.Error()
					ev[id] = closeTerm
					out.Children = append(out.Children, outcome)
					emit(fmt.Sprintf("accept.fail:%s err=%s", id, aerr.Error()))
					if it.Status == workgraph.ItemRequired {
						return fail(out, StatusBlocked, "accept "+id+": "+aerr.Error())
					}
					continue
				}
				emit(fmt.Sprintf("accept.ok:%s role=%s", id, ClassifyTaskRole(id, it.Intent, it.Owner)))
			}

			// Product integration: succeeded children commit onto shared goal branch
			// exactly-once so subsequent ready children see prior state.
			if closeTerm == workgraph.TermSucceeded && doIntegrate && integrator != nil {
				ic, ierr := integrator.IntegrateChild(ctx, IntegrateRequest{
					RepoPath: req.RepoPath, GoalBranch: goalBranch,
					WorkItemID: id, AttemptID: attemptID,
					ChildWorktree: effectiveOut.WorktreePath,
					ProductFiles:  effectiveOut.FilesTouched,
					Intent:        it.Intent,
				})
				if ierr != nil {
					// Fail closed: required success without product integrate is blocked.
					outcome.Terminal = string(workgraph.TermFailed)
					outcome.FailureClass = "integrate_failed"
					outcome.Message = ierr.Error()
					ev[id] = workgraph.TermFailed
					out.Children = append(out.Children, outcome)
					emit(fmt.Sprintf("integrate.fail:%s err=%s", id, ierr.Error()))
					if it.Status == workgraph.ItemRequired {
						return fail(out, StatusBlocked, "integrate "+id+": "+ierr.Error())
					}
					continue
				}
				outcome.IntegrateCommitSHA = ic.CommitSHA
				outcome.FilesTouched = firstNonEmptySlice(ic.Files, outcome.FilesTouched)
				out.IntegrateCommits = append(out.IntegrateCommits, ic)
				integrated = append(integrated, id)
				logEvBestEffort(Event{Kind: "integrate", WorkItemID: id, AttemptID: attemptID, CommitSHA: ic.CommitSHA})
				if ic.Skipped {
					emit(fmt.Sprintf("integrate.skip:%s attempt=%s commit=%s", id, attemptID, short(ic.CommitSHA)))
				} else {
					emit(fmt.Sprintf("integrate.ok:%s attempt=%s commit=%s files=%d", id, attemptID, short(ic.CommitSHA), len(ic.Files)))
				}
			}

			out.Children = append(out.Children, outcome)
			emit(fmt.Sprintf("child.terminal:%s term=%s evidence=%s", id, closeTerm, short(closeEvidence)))
			// Durable partial snapshot for forced process kill recovery.
			_ = writePartialPrior(s.HomeDir, projectID, runID, out)

			// Required child failure blocks the parent (do not pretend human_gate success).
			if it.Status == workgraph.ItemRequired && closeTerm != workgraph.TermSucceeded {
				msg := fmt.Sprintf("required child %s terminal=%s", id, closeTerm)
				if outcome.Message != "" {
					msg += ": " + outcome.Message
				}
				return fail(out, StatusBlocked, msg)
			}
			if cerr != nil && it.Status == workgraph.ItemRequired {
				return fail(out, StatusBlocked, "required child "+id+": "+cerr.Error())
			}
		}
	}

	// When integrate is skipped (no repo), keep legacy in-order integrated list.
	if !doIntegrate {
		order := workgraph.IntegrationOrder(g)
		for _, id := range order {
			if term, ok := ev[id]; ok && term == workgraph.TermSucceeded {
				integrated = append(integrated, id)
				emit("integrate:" + id)
			}
		}
	}
	out.Integrated = integrated

	// Claim budget per logical child: exactly one normal claim, or two when a
	// generation-safe model_unavailable alternate claimed a distinct AttemptID.
	// Never more than two (no retry storms); never zero for launched members.
	for id, n := range claimed {
		if n < 1 || n > 2 {
			return fail(out, StatusBlocked, fmt.Sprintf("item %s claimed %d times", id, n))
		}
	}
	if out.LaunchCount != out.ClaimCount {
		return fail(out, StatusBlocked, "launch/claim mismatch")
	}

	// parent cannot succeed before required children terminal
	for _, it := range g.Items {
		if it.Status == workgraph.ItemRequired {
			if term, ok := ev[it.ID]; !ok || term != workgraph.TermSucceeded {
				return fail(out, StatusBlocked, "required child not terminal: "+it.ID)
			}
		}
	}

	emit("human_gate.await_owner")
	out.Status = StatusHumanGate
	out.Message = fmt.Sprintf("bounded workflow graph=%s claims=%d launches=%d integrated=%d; auto_merge=false",
		out.GraphID, out.ClaimCount, out.LaunchCount, len(out.Integrated))
	out.AutoMerge = false
	return out, nil
}

func resolveChildRoute(routes map[string]ChildRoute, id, defProv, defModel string) ChildRoute {
	if routes != nil {
		if r, ok := routes[id]; ok {
			if strings.TrimSpace(r.Provider) == "" {
				r.Provider = defProv
			}
			if strings.TrimSpace(r.Model) == "" {
				r.Model = defModel
			}
			if strings.TrimSpace(r.Depth) == "" {
				r.Depth = "medium"
			}
			return r
		}
	}
	return ChildRoute{Provider: defProv, Model: defModel, Depth: "medium", RouteReason: "default_pin"}
}

func eventJSONPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// pickSameDepthAlternate selects a hard-eligible, non-soft-excluded candidate
// matching required depth and permission. Never rewrites candidate depth.
func pickSameDepthAlternate(
	cands []AlternateCandidate,
	failedProvider, failedModel, reqDepth, reqPerm string,
) AlternateCandidate {
	reqDepth = strings.ToLower(strings.TrimSpace(reqDepth))
	reqPerm = strings.ToLower(strings.TrimSpace(reqPerm))
	failedProvider = strings.TrimSpace(failedProvider)
	failedModel = strings.TrimSpace(failedModel)
	if reqDepth == "" {
		return AlternateCandidate{}
	}
	for _, cv := range cands {
		if !cv.HardEligible || cv.SoftExcluded {
			continue
		}
		if strings.TrimSpace(cv.Provider) == "" || strings.TrimSpace(cv.Model) == "" {
			continue
		}
		if strings.EqualFold(cv.Provider, failedProvider) && strings.EqualFold(cv.Model, failedModel) {
			continue
		}
		if strings.ToLower(strings.TrimSpace(cv.Effort)) != reqDepth {
			continue
		}
		if reqPerm != "" {
			p := strings.ToLower(strings.TrimSpace(cv.Permission))
			// Empty observed permission never matches a required permission.
			if p == "" || p != reqPerm {
				continue
			}
		}
		return AlternateCandidate{
			Provider: cv.Provider, Model: cv.Model, Effort: strings.ToLower(strings.TrimSpace(cv.Effort)),
			Permission: strings.ToLower(strings.TrimSpace(cv.Permission)), HardEligible: true,
		}
	}
	return AlternateCandidate{}
}

// OneNodeDefinition builds a direct-run-equivalent single-item definition.
func OneNodeDefinition(graphID, intent string) workflowdef.Definition {
	if graphID == "" {
		graphID = "g-one"
	}
	if intent == "" {
		intent = "single direct-equivalent work item"
	}
	return workflowdef.Definition{
		SchemaVersion: 1, GraphID: graphID, Source: "explicit_definition",
		Items: []workflowdef.DefItem{
			{ID: "only", Intent: intent, Status: "required", IntegrationOrder: 1},
		},
	}
}

// ChainDefinition builds a linear required chain a→b→c.
func ChainDefinition(graphID string) workflowdef.Definition {
	if graphID == "" {
		graphID = "g-chain"
	}
	return workflowdef.Definition{
		SchemaVersion: 1, GraphID: graphID, Source: "explicit_definition",
		Items: []workflowdef.DefItem{
			{ID: "a", Intent: "A", Status: "required", IntegrationOrder: 1},
			{ID: "b", Intent: "B", Status: "required", IntegrationOrder: 2},
			{ID: "c", Intent: "C", Status: "required", IntegrationOrder: 3},
		},
		Deps: []workflowdef.DefDep{
			{From: "a", To: "b", Kind: "finish_to_start"},
			{From: "b", To: "c", Kind: "finish_to_start"},
		},
	}
}

// ResultJSON encodes result for CLI.
func ResultJSON(r Result) []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return append(b, '\n')
}

func allTerminal(g workgraph.Graph, ev workgraph.TerminalEvidence) bool {
	for _, it := range g.Items {
		if _, ok := ev[it.ID]; !ok {
			return false
		}
	}
	return true
}

func fail(out Result, status, msg string) (Result, error) {
	out.Status = status
	out.Message = msg
	out.Error = msg
	return out, fmt.Errorf("workflowrun: %s", msg)
}

func isGitRepo(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && (st.IsDir() || st.Mode().IsRegular())
}

func firstNonEmptySlice(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

func sanitizeBranch(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "run"
	}
	if len(out) > 80 {
		return out[:80]
	}
	return out
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 12 {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// Err helpers for tests.
var (
	ErrInvalid = errors.New("workflowrun: invalid")
)
