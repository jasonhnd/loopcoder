package deliveryresume

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SchemaPlan     = "loopcoder.delivery.resume.plan.v1"
	SchemaEvidence = "loopcoder.delivery.resume.evidence.v1"
)

// StageName is a delivery pipeline stage after worker completion.
type StageName string

const (
	StageWorker    StageName = "worker"
	StageCommit    StageName = "commit"
	StagePush      StageName = "push"
	StagePR        StageName = "pr"
	StageCIWait    StageName = "ci_wait"
	StageVerifier  StageName = "verifier"
	StageHumanGate StageName = "human_gate"
	StageTerminal  StageName = "terminal"
)

// OrderedStages is reconcile order after worker.
var OrderedStages = []StageName{
	StageWorker, StageCommit, StagePush, StagePR, StageCIWait, StageVerifier, StageHumanGate, StageTerminal,
}

// Outcome classifies a stage for resume planning.
type Outcome string

const (
	OutcomeIncomplete Outcome = "incomplete"
	OutcomeCompleted  Outcome = "completed"
	OutcomeAdoptable  Outcome = "completed_adoptable"
	OutcomeRetryable  Outcome = "retryable_failure"
	OutcomeDefinitive Outcome = "definitive_failure"
	OutcomeAmbiguous  Outcome = "ambiguous_completion"
	OutcomeNeedsHuman Outcome = "needs_human"
)

// Action is a proposed next step (never silent worker replay).
type Action string

const (
	ActionNone              Action = "none"
	ActionResumeCommit      Action = "resume_commit"
	ActionResumePush        Action = "resume_push"
	ActionResumePR          Action = "resume_pr"
	ActionResumeCIWatch     Action = "resume_ci_watch"
	ActionResumeVerifier    Action = "resume_verifier"
	ActionAwaitHumanGate    Action = "await_human_gate"
	ActionAdoptSideEffect   Action = "adopt_side_effect"
	ActionReadBackThenRetry Action = "read_back_then_retry"
	ActionNeedsOwner        Action = "needs_owner"
	ActionDone              Action = "done"
	// ActionNewWorker is only allowed with explicit owner approval when worker invalid.
	ActionNewWorker Action = "new_worker_attempt_requires_owner"
)

// StageEvidence is independently observed + persisted receipt state for one stage.
type StageEvidence struct {
	Stage            StageName `json:"stage"`
	Outcome          Outcome   `json:"outcome"`
	ReceiptPresent   bool      `json:"receipt_present"`
	ObservedComplete bool      `json:"observed_complete"` // e.g. remote OID/PR exists matching intent
	// Digests for drift detection (empty = not applicable).
	WorktreeDigest     string `json:"worktree_digest,omitempty"`
	BaseSHA            string `json:"base_sha,omitempty"`
	PRHeadOID          string `json:"pr_head_oid,omitempty"`
	RouteDigest        string `json:"route_digest,omitempty"`
	PolicyDigest       string `json:"policy_digest,omitempty"`
	VerificationDigest string `json:"verification_digest,omitempty"`
	// Expected* are frozen intent digests from immutable stage inputs.
	ExpectedWorktreeDigest     string `json:"expected_worktree_digest,omitempty"`
	ExpectedBaseSHA            string `json:"expected_base_sha,omitempty"`
	ExpectedPRHeadOID          string `json:"expected_pr_head_oid,omitempty"`
	ExpectedRouteDigest        string `json:"expected_route_digest,omitempty"`
	ExpectedPolicyDigest       string `json:"expected_policy_digest,omitempty"`
	ExpectedVerificationDigest string `json:"expected_verification_digest,omitempty"`
	Reason                     string `json:"reason,omitempty"`
}

// RunSnapshot is the full resume input (immutable pins + observed state).
type RunSnapshot struct {
	RunID             string `json:"run_id"`
	WorkerLaunchCount int    `json:"worker_launch_count"`
	// WorkerCleanupTerminal means worker finished and must not be re-launched.
	WorkerCleanupTerminal bool `json:"worker_cleanup_terminal"`
	// OwnerApprovedNewWorker allows ActionNewWorker only when set.
	OwnerApprovedNewWorker bool                        `json:"owner_approved_new_worker"`
	Stages                 map[StageName]StageEvidence `json:"stages"`
}

// PlanStep is one dry-run / execute step.
type PlanStep struct {
	Stage      StageName `json:"stage"`
	Action     Action    `json:"action"`
	SideEffect string    `json:"side_effect"`
	Reason     string    `json:"reason"`
	Mutates    bool      `json:"mutates"`
}

// Plan is the resume plan (dry-run or applied).
type Plan struct {
	Schema            string     `json:"schema"`
	RunID             string     `json:"run_id"`
	DryRun            bool       `json:"dry_run"`
	WorkerLaunchCount int        `json:"worker_launch_count"`
	NextAction        Action     `json:"next_action"`
	Terminal          bool       `json:"terminal"`
	Steps             []PlanStep `json:"steps"`
	// EvidenceSummary lists stage outcomes used.
	EvidenceSummary []string  `json:"evidence_summary"`
	GeneratedAt     time.Time `json:"generated_at"`
}

// Planner derives resume plans without calling providers.
type Planner struct {
	now func() time.Time
}

// NewPlanner creates a planner.
func NewPlanner(now func() time.Time) *Planner {
	if now == nil {
		now = time.Now
	}
	return &Planner{now: now}
}

// Plan builds a resume plan. dryRun never mutates; ExecutePlan may apply adoptable steps.
func (p *Planner) Plan(snap RunSnapshot, dryRun bool) (Plan, error) {
	if snap.RunID == "" {
		return Plan{}, errors.New("deliveryresume: run_id required")
	}
	if snap.Stages == nil {
		snap.Stages = map[StageName]StageEvidence{}
	}

	plan := Plan{
		Schema: SchemaPlan, RunID: snap.RunID, DryRun: dryRun,
		WorkerLaunchCount: snap.WorkerLaunchCount, GeneratedAt: p.now().UTC(),
	}

	// Drift check across stages that carry expected digests
	if drift, reason := detectDrift(snap); drift {
		plan.NextAction = ActionNeedsOwner
		plan.Steps = []PlanStep{{
			Stage: StageWorker, Action: ActionNeedsOwner, Mutates: false,
			SideEffect: "none", Reason: reason,
		}}
		plan.EvidenceSummary = summarize(snap)
		return plan, nil
	}

	// Worker incomplete / invalid
	w := snap.Stages[StageWorker]
	if !snap.WorkerCleanupTerminal {
		if w.Outcome == OutcomeDefinitive || w.Outcome == OutcomeNeedsHuman {
			plan.NextAction = ActionNeedsOwner
			plan.Steps = []PlanStep{{
				Stage: StageWorker, Action: ActionNeedsOwner, Mutates: false,
				SideEffect: "none", Reason: nonEmpty(w.Reason, "worker not cleanup-terminal"),
			}}
			plan.EvidenceSummary = summarize(snap)
			return plan, nil
		}
		// Worker incomplete: only new worker if owner approved AND output invalid
		if snap.OwnerApprovedNewWorker && (w.Outcome == OutcomeIncomplete || w.Outcome == OutcomeDefinitive) {
			plan.NextAction = ActionNewWorker
			plan.Steps = []PlanStep{{
				Stage: StageWorker, Action: ActionNewWorker, Mutates: true,
				SideEffect: "new_worker_attempt", Reason: "owner approved new worker; worker output absent/invalid",
			}}
			plan.EvidenceSummary = summarize(snap)
			return plan, nil
		}
		plan.NextAction = ActionNeedsOwner
		plan.Steps = []PlanStep{{
			Stage: StageWorker, Action: ActionNeedsOwner, Mutates: false,
			SideEffect: "none",
			Reason:     "worker incomplete; refusing automatic worker replay (set owner_approved_new_worker only if output invalid)",
		}}
		plan.EvidenceSummary = summarize(snap)
		return plan, nil
	}

	// Delivery stages: never increment worker launch
	for _, stage := range OrderedStages {
		if stage == StageWorker {
			continue
		}
		ev := snap.Stages[stage]
		if ev.Stage == "" {
			ev.Stage = stage
		}
		step, done := planStage(stage, ev)
		if done {
			// completed — continue
			continue
		}
		// first incomplete / actionable
		plan.Steps = append(plan.Steps, step)
		plan.NextAction = step.Action
		if step.Action == ActionDone {
			plan.Terminal = true
		}
		plan.EvidenceSummary = summarize(snap)
		// guarantee zero worker replay signal
		if plan.WorkerLaunchCount < 0 {
			plan.WorkerLaunchCount = 0
		}
		return plan, nil
	}

	plan.NextAction = ActionDone
	plan.Terminal = true
	plan.Steps = []PlanStep{{
		Stage: StageTerminal, Action: ActionDone, Mutates: false,
		SideEffect: "none", Reason: "all stages complete",
	}}
	plan.EvidenceSummary = summarize(snap)
	return plan, nil
}

func planStage(stage StageName, ev StageEvidence) (PlanStep, bool) {
	// returns (step, completedAndSkip)
	switch ev.Outcome {
	case OutcomeCompleted, OutcomeAdoptable:
		if ev.Outcome == OutcomeAdoptable {
			return PlanStep{
				Stage: stage, Action: ActionAdoptSideEffect, Mutates: true,
				SideEffect: "persist_receipt_from_observation", Reason: nonEmpty(ev.Reason, "side effect complete; adopt"),
			}, false
		}
		return PlanStep{}, true
	case OutcomeAmbiguous:
		return PlanStep{
			Stage: stage, Action: ActionReadBackThenRetry, Mutates: false,
			SideEffect: "git_github_read_back", Reason: nonEmpty(ev.Reason, "ambiguous; read back before retry"),
		}, false
	case OutcomeRetryable:
		return PlanStep{
			Stage: stage, Action: actionForStage(stage), Mutates: true,
			SideEffect: string(actionForStage(stage)), Reason: nonEmpty(ev.Reason, "retryable stage failure"),
		}, false
	case OutcomeDefinitive:
		return PlanStep{
			Stage: stage, Action: ActionNeedsOwner, Mutates: false,
			SideEffect: "none", Reason: nonEmpty(ev.Reason, "definitive failure"),
		}, false
	case OutcomeNeedsHuman:
		if stage == StageHumanGate {
			return PlanStep{
				Stage: stage, Action: ActionAwaitHumanGate, Mutates: false,
				SideEffect: "none", Reason: nonEmpty(ev.Reason, "await human merge decision"),
			}, false
		}
		return PlanStep{
			Stage: stage, Action: ActionNeedsOwner, Mutates: false,
			SideEffect: "none", Reason: nonEmpty(ev.Reason, "needs human"),
		}, false
	case OutcomeIncomplete, "":
		if stage == StageTerminal {
			return PlanStep{}, true
		}
		if stage == StageHumanGate {
			return PlanStep{
				Stage: stage, Action: ActionAwaitHumanGate, Mutates: false,
				SideEffect: "none", Reason: "human gate incomplete",
			}, false
		}
		return PlanStep{
			Stage: stage, Action: actionForStage(stage), Mutates: true,
			SideEffect: string(actionForStage(stage)), Reason: nonEmpty(ev.Reason, "stage incomplete"),
		}, false
	default:
		return PlanStep{
			Stage: stage, Action: ActionNeedsOwner, Mutates: false,
			SideEffect: "none", Reason: "unknown outcome " + string(ev.Outcome),
		}, false
	}
}

func actionForStage(s StageName) Action {
	switch s {
	case StageCommit:
		return ActionResumeCommit
	case StagePush:
		return ActionResumePush
	case StagePR:
		return ActionResumePR
	case StageCIWait:
		return ActionResumeCIWatch
	case StageVerifier:
		return ActionResumeVerifier
	case StageHumanGate:
		return ActionAwaitHumanGate
	default:
		return ActionNeedsOwner
	}
}

func detectDrift(snap RunSnapshot) (bool, string) {
	for _, name := range OrderedStages {
		ev, ok := snap.Stages[name]
		if !ok {
			continue
		}
		if d := digestDrift("worktree", ev.ExpectedWorktreeDigest, ev.WorktreeDigest); d != "" {
			return true, d
		}
		if d := digestDrift("base", ev.ExpectedBaseSHA, ev.BaseSHA); d != "" {
			return true, d
		}
		if d := digestDrift("pr_head", ev.ExpectedPRHeadOID, ev.PRHeadOID); d != "" {
			return true, d
		}
		if d := digestDrift("route", ev.ExpectedRouteDigest, ev.RouteDigest); d != "" {
			return true, d
		}
		if d := digestDrift("policy", ev.ExpectedPolicyDigest, ev.PolicyDigest); d != "" {
			return true, d
		}
		if d := digestDrift("verification", ev.ExpectedVerificationDigest, ev.VerificationDigest); d != "" {
			return true, d
		}
	}
	return false, ""
}

func digestDrift(label, expected, actual string) string {
	if expected == "" || actual == "" {
		return ""
	}
	if expected != actual {
		return fmt.Sprintf("%s digest changed (expected %s got %s); automatic resume blocked", label, expected, actual)
	}
	return ""
}

func summarize(snap RunSnapshot) []string {
	var lines []string
	for _, name := range OrderedStages {
		ev, ok := snap.Stages[name]
		if !ok {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s=%s receipt=%v observed=%v", name, ev.Outcome, ev.ReceiptPresent, ev.ObservedComplete))
	}
	sort.Strings(lines)
	return lines
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// --- Apply layer (idempotent, no worker) ---

// Applier applies non-worker resume actions using stage handlers.
type Applier struct {
	mu sync.Mutex
	// Applied counts for idempotency tests
	Applied map[Action]int
	// SideEffects records what would be/was done
	SideEffects []string
}

// NewApplier creates an applier.
func NewApplier() *Applier {
	return &Applier{Applied: map[Action]int{}}
}

// Execute applies the first mutating step if not dry-run. Never launches worker
// unless ActionNewWorker (and even then only increments a counter for tests —
// production would require separate owner-approved path).
func (a *Applier) Execute(plan Plan) (Plan, error) {
	if plan.DryRun {
		return plan, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Applied == nil {
		a.Applied = map[Action]int{}
	}
	for _, step := range plan.Steps {
		if step.Action == ActionNone || step.Action == ActionDone || step.Action == ActionNeedsOwner || step.Action == ActionAwaitHumanGate {
			continue
		}
		if step.Action == ActionNewWorker {
			// explicit only — still not a silent replay of completed worker
			a.Applied[step.Action]++
			a.SideEffects = append(a.SideEffects, "owner_approved_new_worker")
			return plan, nil
		}
		// adopt / resume stages
		a.Applied[step.Action]++
		a.SideEffects = append(a.SideEffects, string(step.Action)+":"+string(step.Stage))
		// only first actionable step
		return plan, nil
	}
	return plan, nil
}

// WorkerLaunches returns how many times Execute chose new worker (should be 0
// when worker already cleanup-terminal).
func (a *Applier) WorkerLaunches() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Applied[ActionNewWorker]
}

// Resume is Plan+optional Execute. When dryRun, no mutation.
func Resume(snap RunSnapshot, dryRun bool, now func() time.Time, applier *Applier) (Plan, error) {
	p := NewPlanner(now)
	plan, err := p.Plan(snap, dryRun)
	if err != nil {
		return Plan{}, err
	}
	// Invariant: if worker cleanup terminal, never propose new worker without approval
	if snap.WorkerCleanupTerminal && plan.NextAction == ActionNewWorker && !snap.OwnerApprovedNewWorker {
		return Plan{}, errors.New("deliveryresume: refused worker replay after cleanup-terminal")
	}
	if !dryRun && applier != nil {
		return applier.Execute(plan)
	}
	return plan, nil
}

// Converge runs Plan twice; next action must be stable (idempotent planning).
func Converge(snap RunSnapshot, now func() time.Time) (Action, bool, error) {
	p := NewPlanner(now)
	a, err := p.Plan(snap, true)
	if err != nil {
		return "", false, err
	}
	b, err := p.Plan(snap, true)
	if err != nil {
		return "", false, err
	}
	return a.NextAction, a.NextAction == b.NextAction && a.Terminal == b.Terminal, nil
}
