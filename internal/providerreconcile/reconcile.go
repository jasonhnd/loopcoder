package providerreconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerauthority"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	SchemaVersion = "loopcoder.provider_reconciliation.v1"

	ActionContinue      = "continue"
	ActionObserve       = "observe"
	ActionReconcile     = "reconcile"
	ActionRetry         = "retry"
	ActionHarvest       = "harvest"
	ActionNeedsHuman    = "needs-human"
	ActionReuseTerminal = "reuse-terminal"

	OutcomeNoAuthority        = "no-authority"
	OutcomeLiveOwnerProvider  = "live-owner-provider"
	OutcomeLiveProviderStale  = "live-provider-stale-owner"
	OutcomeDeadChanged        = "dead-provider-changed-worktree"
	OutcomeDeadNoMaterialWork = "dead-provider-no-material-work"
	OutcomeAmbiguous          = "ambiguous-provider-authority"
	OutcomeLaunchAmbiguous    = "ambiguous-launch-phase"
	OutcomeTerminal           = "terminal-provider-result"
	OutcomeUnregistered       = "unregistered-runtime"
)

type Runtime interface {
	Registered() bool
	Load(ctx context.Context, runID, attemptID string) (providerauthority.View, error)
	ValidateOwnership(ctx context.Context, view providerauthority.View, at time.Time) error
}

type RuntimeFunc func(ctx context.Context, repoPath string, now func() time.Time) (Runtime, func(), error)

type Options struct {
	RepoPath string
	RunID    string
	Issue    int
	Attempts []state.Attempt
	Now      time.Time

	OpenRuntime RuntimeFunc
}

type Receipt struct {
	SchemaVersion   string   `json:"schema_version"`
	Issue           int      `json:"issue"`
	RunID           string   `json:"run_id"`
	AttemptID       string   `json:"attempt_id,omitempty"`
	AuthorityID     string   `json:"authority_id,omitempty"`
	OwnerID         string   `json:"owner_id,omitempty"`
	ClaimGeneration int64    `json:"claim_generation,omitempty"`
	ProviderPID     int      `json:"provider_pid,omitempty"`
	ProviderPGID    int      `json:"provider_pgid,omitempty"`
	AuthorityState  string   `json:"authority_state,omitempty"`
	Outcome         string   `json:"outcome"`
	Action          string   `json:"action"`
	RetryAllowed    bool     `json:"retry_allowed"`
	NeedsHuman      bool     `json:"needs_human"`
	BlockRedispatch bool     `json:"block_redispatch"`
	Reason          string   `json:"reason"`
	NextAction      string   `json:"next_action"`
	Evidence        []string `json:"evidence"`
	DecidedAt       string   `json:"decided_at"`
}

func Check(ctx context.Context, opts Options) Receipt {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	base := Receipt{
		SchemaVersion: SchemaVersion,
		Issue:         opts.Issue,
		RunID:         strings.TrimSpace(opts.RunID),
		DecidedAt:     state.FormatTimestamp(now),
	}
	if opts.Issue <= 0 {
		return base.with(OutcomeAmbiguous, ActionNeedsHuman, "issue is required for provider reconciliation", "request human review before launching another provider", true, false, true, nil)
	}
	if strings.TrimSpace(opts.RunID) == "" {
		return base.with(OutcomeNoAuthority, ActionContinue, "run_id is empty; no durable provider authority namespace is available", "continue only if legacy liveness checks allow it", false, true, false, nil)
	}

	openRuntime := opts.OpenRuntime
	if openRuntime == nil {
		openRuntime = func(ctx context.Context, repoPath string, now func() time.Time) (Runtime, func(), error) {
			runtime, err := providerauthority.OpenRuntime(ctx, repoPath, now)
			return runtime, runtime.Close, err
		}
	}
	runtime, closeRuntime, err := openRuntime(ctx, opts.RepoPath, func() time.Time { return now })
	if closeRuntime != nil {
		defer closeRuntime()
	}
	if err != nil {
		return base.with(OutcomeAmbiguous, ActionNeedsHuman, "provider authority runtime is unavailable: "+err.Error(), "request human review before launching another provider", true, false, true, nil)
	}
	if runtime == nil || !runtime.Registered() {
		return base.with(OutcomeUnregistered, ActionContinue, "provider authority runtime is not registered; using legacy attempt state only", "continue only if legacy liveness checks allow it", false, true, false, nil)
	}

	attempts := issueAttempts(opts.Attempts, opts.Issue)
	if len(attempts) == 0 {
		return base.with(OutcomeNoAuthority, ActionContinue, "no local attempt exists for issue", "continue dispatch preflight", false, true, false, nil)
	}

	for _, attempt := range attempts {
		receipt := base
		receipt.AttemptID = strings.TrimSpace(attempt.JobID)
		if receipt.AttemptID == "" {
			continue
		}
		view, err := runtime.Load(ctx, opts.RunID, attempt.JobID)
		if err != nil {
			return receipt.with(OutcomeAmbiguous, ActionNeedsHuman, "load provider authority: "+err.Error(), "request human review before launching another provider", true, false, true, attemptEvidence(attempt))
		}
		receipt = receipt.withAuthority(view)
		if view.State == providerauthority.StateMissingRow {
			if launchPhaseAmbiguous(attempt) {
				return receipt.with(OutcomeLaunchAmbiguous, ActionNeedsHuman, "attempt is in launch/execution phase but provider authority is missing", "request human review; do not consume another provider call", true, false, true, attemptEvidence(attempt))
			}
			continue
		}
		return classifyView(ctx, runtime, receipt, view, attempt, now)
	}
	return base.with(OutcomeNoAuthority, ActionContinue, "no provider authority row matched issue attempts", "continue dispatch preflight", false, true, false, nil)
}

func classifyView(ctx context.Context, runtime Runtime, receipt Receipt, view providerauthority.View, attempt state.Attempt, now time.Time) Receipt {
	evidence := attemptEvidence(attempt)
	evidence = append(evidence, authorityEvidence(view)...)
	switch view.State {
	case providerauthority.StateActive:
		ownerErr := runtime.ValidateOwnership(ctx, view, now)
		if ownerErr == nil {
			return receipt.with(OutcomeLiveOwnerProvider, ActionObserve, "valid live owner and provider authority", "observe or attach to the live provider; do not redispatch", true, false, false, evidence)
		}
		if errors.Is(ownerErr, storage.ErrOwnershipStale) {
			return receipt.with(OutcomeLiveProviderStale, ActionReconcile, "provider is live but supervisor ownership is stale", "reconcile under the existing provider fence; do not redispatch", true, false, false, append(evidence, "owner_validation=stale"))
		}
		return receipt.with(OutcomeAmbiguous, ActionNeedsHuman, "provider ownership validation is ambiguous: "+ownerErr.Error(), "request human review before launching another provider", true, false, true, evidence)
	case providerauthority.StateTerminal:
		if terminalBlocksRedispatch(view.Authority.TerminalState, attempt.Status) {
			return receipt.with(OutcomeTerminal, ActionReuseTerminal, "terminal provider result is already recorded", "reuse the terminal result idempotently; do not launch another provider", true, false, false, evidence)
		}
		return receipt.with(OutcomeTerminal, ActionRetry, "terminal provider result permits bounded retry", "retry only within the configured budget", false, true, false, evidence)
	case providerauthority.StateStale:
		material, materialEvidence, err := materialWorktreeChanged(view.Authority.WorktreePath)
		evidence = append(evidence, materialEvidence...)
		if err != nil {
			return receipt.with(OutcomeAmbiguous, ActionNeedsHuman, "dead provider worktree could not be inspected: "+err.Error(), "request human review before launching another provider", true, false, true, evidence)
		}
		if material {
			return receipt.with(OutcomeDeadChanged, ActionHarvest, "provider is dead and the worktree has material changes", "harvest the worktree or request human review; do not redispatch yet", true, false, true, evidence)
		}
		return receipt.with(OutcomeDeadNoMaterialWork, ActionRetry, "provider is dead and no material worktree changes were found", "retry only within the configured budget", false, true, false, evidence)
	case providerauthority.StateAmbiguous, providerauthority.StateIdentityMismatch, providerauthority.StateCorruptRow:
		return receipt.with(OutcomeAmbiguous, ActionNeedsHuman, "provider authority identity is ambiguous: "+view.Reason, "request human review before launching another provider", true, false, true, evidence)
	default:
		return receipt.with(OutcomeAmbiguous, ActionNeedsHuman, "provider authority state is unknown: "+view.State, "request human review before launching another provider", true, false, true, evidence)
	}
}

func (r Receipt) with(outcome, action, reason, nextAction string, block, retryAllowed, needsHuman bool, evidence []string) Receipt {
	r.Outcome = outcome
	r.Action = action
	r.Reason = reason
	r.NextAction = nextAction
	r.BlockRedispatch = block
	r.RetryAllowed = retryAllowed
	r.NeedsHuman = needsHuman
	r.Evidence = compactEvidence(append(r.Evidence, evidence...))
	return r
}

func (r Receipt) withAuthority(view providerauthority.View) Receipt {
	r.AuthorityID = strings.TrimSpace(view.Authority.AuthorityID)
	r.OwnerID = strings.TrimSpace(view.Authority.OwnerID)
	r.ClaimGeneration = view.Authority.ClaimGeneration
	r.ProviderPID = view.Authority.ProviderPID
	r.ProviderPGID = view.Authority.ProviderPGID
	r.AuthorityState = strings.TrimSpace(view.State)
	return r
}

func (r Receipt) Summary() string {
	return fmt.Sprintf("provider_reconciliation outcome=%s action=%s reason=%s", r.Outcome, r.Action, r.Reason)
}

func (r Receipt) JSONLine() string {
	data, err := json.Marshal(r)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func issueAttempts(attempts []state.Attempt, issue int) []state.Attempt {
	out := make([]state.Attempt, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.Issue == issue && strings.TrimSpace(attempt.JobID) != "" {
			out = append(out, attempt)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Attempt != out[j].Attempt {
			return out[i].Attempt > out[j].Attempt
		}
		if !out[i].LastWriteUTC.Equal(out[j].LastWriteUTC) {
			return out[i].LastWriteUTC.After(out[j].LastWriteUTC)
		}
		return out[i].JobID > out[j].JobID
	})
	return out
}

func launchPhaseAmbiguous(attempt state.Attempt) bool {
	status := state.NormalizeStatus(attempt.Status)
	phase := strings.ToLower(strings.TrimSpace(attempt.Phase))
	if status == state.StatusLaunching || status == state.StatusRunning || status == "" {
		return strings.Contains(phase, "launch") || strings.Contains(phase, "started") || strings.Contains(phase, "exec") || strings.Contains(phase, "running")
	}
	return false
}

func terminalBlocksRedispatch(terminalState, attemptStatus string) bool {
	status := state.NormalizeStatus(firstNonEmpty(terminalState, attemptStatus))
	switch status {
	case state.StatusSucceeded, state.StatusSucceededWithOptionalFailures, state.StatusNeedsHuman:
		return true
	default:
		return false
	}
}

func materialWorktreeChanged(worktreePath string) (bool, []string, error) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return false, []string{"worktree=missing"}, nil
	}
	clean := filepath.Clean(worktreePath)
	info, err := os.Stat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return false, []string{"worktree_absent=" + clean}, nil
		}
		return false, nil, err
	}
	if !info.IsDir() {
		return false, nil, fmt.Errorf("worktree path is not a directory: %s", clean)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", clean, "status", "--porcelain", "--untracked-files=normal")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, nil, fmt.Errorf("git status %s: %w: %s", clean, err, strings.TrimSpace(stderr.String()))
	}
	status := strings.TrimSpace(stdout.String())
	if status == "" {
		return false, []string{"worktree_clean=" + clean}, nil
	}
	lines := strings.Split(status, "\n")
	if len(lines) > 5 {
		lines = append(lines[:5], fmt.Sprintf("... %d more", len(strings.Split(status, "\n"))-5))
	}
	return true, []string{"worktree_changed=" + clean, "status=" + strings.Join(lines, " | ")}, nil
}

func attemptEvidence(attempt state.Attempt) []string {
	evidence := []string{
		"attempt=" + firstNonEmpty(attempt.JobID, "unknown"),
		fmt.Sprintf("attempt_number=%d", attempt.Attempt),
		"attempt_status=" + firstNonEmpty(attempt.Status, "unknown"),
		"attempt_phase=" + firstNonEmpty(attempt.Phase, "unknown"),
	}
	if attempt.PID != nil {
		evidence = append(evidence, fmt.Sprintf("attempt_pid=%d", *attempt.PID))
	}
	if strings.TrimSpace(attempt.Path) != "" {
		evidence = append(evidence, "attempt_path="+filepath.ToSlash(attempt.Path))
	}
	return evidence
}

func authorityEvidence(view providerauthority.View) []string {
	return []string{
		"authority_state=" + firstNonEmpty(view.State, "unknown"),
		"authority_reason=" + firstNonEmpty(view.Reason, "unknown"),
		fmt.Sprintf("provider_pid=%d", view.Authority.ProviderPID),
		fmt.Sprintf("provider_pgid=%d", view.Authority.ProviderPGID),
		"owner_id=" + firstNonEmpty(view.Authority.OwnerID, "unknown"),
		fmt.Sprintf("claim_generation=%d", view.Authority.ClaimGeneration),
	}
}

func compactEvidence(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
