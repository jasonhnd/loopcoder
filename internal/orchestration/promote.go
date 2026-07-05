package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/equivalence"
	"github.com/jasonhnd/loopcoder/internal/kickback"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	PromoteReportVersion = 1

	PromoteStatusSucceeded = "succeeded"
	PromoteStatusFailed    = "failed"

	PromoteOutcomePromoted      = "promoted"
	PromoteOutcomeSkippedAsDone = "skipped-as-done"
	PromoteOutcomeFailed        = "failed"

	promoteLedgerEvent         = "promote.attempt"
	promoteRollbackLedgerEvent = "promote.rollback"
)

const (
	GateHumanMerge = "human-merge"
	GateAuto       = "auto"
)

type PromotionWriter interface {
	BranchHeadSHA(ctx context.Context, branch string) (string, error)
	BranchChecks(ctx context.Context, branch string) (gh.BranchChecksResult, error)
	CompareBranches(ctx context.Context, base, head string) (files []string, diff string, err error)
	KickBackFromPreProd(ctx context.Context, item, preProdBranch string) (gh.PreProdKickBackResult, error)
	RouteKickBackToNeedsHuman(ctx context.Context, prNumber int) (gh.NeedsHumanRouteResult, error)
	PromotePreProdToMain(ctx context.Context, preProdBranch string) (gh.MainPromotionResult, error)
	RevertProductionMerge(ctx context.Context, mainBranch, mergeCommit, priorStableCommit string) (gh.ProductionRevertResult, error)
	SyncPreProdFromMain(ctx context.Context, preProdBranch string) (gh.PreProdSyncResult, error)
}

type PromoteOptions struct {
	Writer               PromotionWriter
	RepoPath             string
	RunID                string
	PreProdBranch        string
	Gate                 string
	KickBackItems        []string
	Clock                func() time.Time
	StatePush            StatePushFunc
	ToggleInventory      PromoteToggleInventoryFunc
	ParallelRun          *PromoteParallelRunConfig
	ReconcileParallelRun PromoteParallelRunReconcileFunc
	AutoGate             *AutoGateInputs
	ResolveAutoGate      func(ctx context.Context) (*AutoGateInputs, error)
	RequiredChecks       []string
}

type PromoteParallelRunReconcileFunc func(equivalence.Contract, equivalence.ParallelRunInput) (equivalence.ParallelRunReport, error)

type PromoteParallelRunConfig struct {
	Contract equivalence.Contract
	Input    equivalence.ParallelRunInput
}

type AutoGateInputs struct {
	CIGreen         *bool
	VerdictPass     *bool
	EvidencePresent *bool
	RedLineClean    *bool
}

type PromoteReport struct {
	Version          int                            `json:"version"`
	RepoPath         string                         `json:"repo_path"`
	RunID            string                         `json:"run_id"`
	PreProdBranch    string                         `json:"pre_prod_branch"`
	MainBranch       string                         `json:"main_branch"`
	Gate             string                         `json:"gate"`
	Status           string                         `json:"status"`
	StartedAt        string                         `json:"started_at"`
	FinishedAt       string                         `json:"finished_at"`
	KickedBack       []PromoteKickBackResult        `json:"kicked_back"`
	NeedsHuman       []PromoteNeedsHuman            `json:"needs_human"`
	ToggleInventory  PromoteToggleInventory         `json:"toggle_inventory,omitempty"`
	GoNoGoPanel      *PromoteGoNoGoPanel            `json:"go_no_go_panel,omitempty"`
	Promoted         PromoteMainResult              `json:"promoted"`
	ProductionHealth *PromoteProductionHealthResult `json:"production_health,omitempty"`
	Rollback         *PromoteRollbackResult         `json:"rollback,omitempty"`
	Sync             PromoteSyncResult              `json:"sync"`
	StatePush        *PromoteStatePush              `json:"state_push,omitempty"`
	Summary          PromoteSummary                 `json:"summary"`
}

type PromoteGoNoGoPanel struct {
	Reconciliation  *equivalence.ParallelRunReport `json:"reconciliation,omitempty"`
	ToggleInventory *PromoteToggleInventory        `json:"toggle_inventory,omitempty"`
	NeedsHuman      []PromoteNeedsHuman            `json:"needs_human,omitempty"`
	Failed          []PromoteFailedItem            `json:"failed,omitempty"`
}

type PromoteFailedItem struct {
	Step     string `json:"step"`
	Item     string `json:"item,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
}

type PromoteKickBackResult struct {
	Item        string `json:"item"`
	PRNumber    int    `json:"pr_number,omitempty"`
	Branch      string `json:"branch"`
	RevertedSHA string `json:"reverted_sha,omitempty"`
	SHA         string `json:"sha,omitempty"`
	URL         string `json:"url,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

type PromoteMainResult struct {
	PreProdBranch     string `json:"pre_prod_branch"`
	MainBranch        string `json:"main_branch"`
	Head              string `json:"head,omitempty"`
	SHA               string `json:"sha,omitempty"`
	PriorStableCommit string `json:"prior_stable_commit,omitempty"`
	URL               string `json:"url,omitempty"`
	AlreadyUpToDate   bool   `json:"already_up_to_date,omitempty"`
	Status            string `json:"status"`
	Error             string `json:"error,omitempty"`
}

type PromoteProductionHealthResult struct {
	Branch         string     `json:"branch"`
	HeadSHA        string     `json:"head_sha,omitempty"`
	MergeSHA       string     `json:"merge_sha,omitempty"`
	Status         string     `json:"status"`
	RequiredChecks []string   `json:"required_checks"`
	Checks         []gh.Check `json:"checks"`
	Problems       []string   `json:"problems"`
	Error          string     `json:"error,omitempty"`
}

type PromoteRollbackResult struct {
	MainBranch        string `json:"main_branch"`
	MergeCommit       string `json:"merge_commit,omitempty"`
	PriorStableCommit string `json:"prior_stable_commit,omitempty"`
	RevertSHA         string `json:"revert_sha,omitempty"`
	URL               string `json:"url,omitempty"`
	Status            string `json:"status"`
	Error             string `json:"error,omitempty"`
}

type PromoteSyncResult struct {
	PreProdBranch string `json:"pre_prod_branch"`
	MainBranch    string `json:"main_branch"`
	SHA           string `json:"sha,omitempty"`
	URL           string `json:"url,omitempty"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
}

type PromoteNeedsHuman struct {
	Step         string `json:"step"`
	Item         string `json:"item,omitempty"`
	PRNumber     int    `json:"pr_number,omitempty"`
	Label        string `json:"label,omitempty"`
	RoutedIssues []int  `json:"routed_issues,omitempty"`
	Detail       string `json:"detail"`
}

type PromoteStatePush struct {
	Branch    string   `json:"branch"`
	Remote    string   `json:"remote"`
	Committed bool     `json:"committed"`
	Pushed    bool     `json:"pushed"`
	PushError string   `json:"push_error,omitempty"`
	Files     []string `json:"files"`
	Error     string   `json:"error,omitempty"`
}

type PromoteSummary struct {
	KickedBackCount int `json:"kicked_back_count"`
	PromotedCount   int `json:"promoted_count"`
	NeedsHumanCount int `json:"needs_human_count"`
	FailureCount    int `json:"failure_count"`
}

func Promote(ctx context.Context, opts PromoteOptions) (PromoteReport, error) {
	opts = withPromoteDefaults(opts)
	opts.PreProdBranch = strings.TrimSpace(opts.PreProdBranch)
	opts.Gate = normalizePromotionGate(opts.Gate)
	opts.RunID = strings.TrimSpace(opts.RunID)
	started := opts.Clock().UTC()
	if opts.RunID == "" {
		opts.RunID = state.RunIDForWave(started)
	}
	report := PromoteReport{
		Version:       PromoteReportVersion,
		RepoPath:      opts.RepoPath,
		RunID:         opts.RunID,
		PreProdBranch: opts.PreProdBranch,
		MainBranch:    lcdefaults.BaseBranch,
		Gate:          opts.Gate,
		Status:        PromoteStatusSucceeded,
		StartedAt:     state.FormatTimestamp(started),
		KickedBack:    []PromoteKickBackResult{},
		NeedsHuman:    []PromoteNeedsHuman{},
	}
	finish := func() (PromoteReport, error) {
		report.FinishedAt = state.FormatTimestamp(opts.Clock().UTC())
		report.Summary.NeedsHumanCount = len(report.NeedsHuman)
		report.GoNoGoPanel = assemblePromoteGoNoGoPanel(report)
		report = normalizePromoteReport(report)
		if err := recordPromoteAttempt(ctx, opts, &report); err != nil {
			report.Status = PromoteStatusFailed
			report.Summary.FailureCount++
			if report.StatePush == nil {
				report.StatePush = &PromoteStatePush{Files: []string{}, Error: err.Error()}
			} else if strings.TrimSpace(report.StatePush.Error) == "" && strings.TrimSpace(report.StatePush.PushError) == "" {
				report.StatePush.Error = err.Error()
			}
			report.GoNoGoPanel = assemblePromoteGoNoGoPanel(report)
		}
		return normalizePromoteReport(report), nil
	}
	failBeforeMain := func(err error) (PromoteReport, error) {
		report.Status = PromoteStatusFailed
		report.Summary.FailureCount++
		report.Promoted = PromoteMainResult{
			PreProdBranch: opts.PreProdBranch,
			MainBranch:    lcdefaults.BaseBranch,
			Head:          opts.PreProdBranch,
			Status:        PromoteStatusFailed,
			Error:         err.Error(),
		}
		finished, _ := finish()
		return finished, err
	}
	needsHumanBeforeMain := func(step, detail string) (PromoteReport, error) {
		report.Status = PromoteStatusFailed
		report.Summary.FailureCount++
		report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
			Step:   step,
			Detail: detail,
		})
		report.Promoted = PromoteMainResult{
			PreProdBranch: opts.PreProdBranch,
			MainBranch:    lcdefaults.BaseBranch,
			Head:          opts.PreProdBranch,
			Status:        PromoteStatusFailed,
			Error:         detail,
		}
		return finish()
	}

	if opts.Writer == nil {
		return failBeforeMain(errors.New("promotion writer is required"))
	}
	if opts.PreProdBranch == "" {
		return failBeforeMain(errors.New("pre-prod branch is required"))
	}
	if isReservedPromotionBranch(opts.PreProdBranch) {
		return failBeforeMain(fmt.Errorf("pre-prod branch %q is reserved for human promotion", opts.PreProdBranch))
	}
	if err := validatePromotionGate(opts.Gate); err != nil {
		return failBeforeMain(err)
	}
	if opts.Gate == GateAuto && opts.AutoGate == nil && opts.ResolveAutoGate != nil {
		autoGate, err := opts.ResolveAutoGate(ctx)
		if err != nil {
			return needsHumanBeforeMain("auto-gate", "auto-gate inputs unavailable: "+err.Error())
		}
		opts.AutoGate = autoGate
	}
	switch opts.Gate {
	case GateHumanMerge:
	case GateAuto:
		if opts.AutoGate == nil {
			return needsHumanBeforeMain("auto-gate", "auto-gate inputs unavailable")
		}
		allowed, reason := evaluateAutoGate(*opts.AutoGate)
		if !allowed {
			return needsHumanBeforeMain("auto-gate", reason)
		}
	}

	if opts.ParallelRun != nil {
		reconciliation, err := opts.ReconcileParallelRun(opts.ParallelRun.Contract, opts.ParallelRun.Input)
		if err != nil {
			report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
				Step:   "parallel-run-reconciliation",
				Detail: "parallel-run reconciliation could not be loaded: " + err.Error(),
			})
		} else {
			report.GoNoGoPanel = &PromoteGoNoGoPanel{Reconciliation: &reconciliation}
		}
	}

	inventory, err := opts.ToggleInventory(ctx, opts.RepoPath)
	if err != nil {
		report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
			Step:   "toggle-inventory",
			Detail: "toggle inventory could not be loaded: " + err.Error(),
		})
	} else {
		report.ToggleInventory = inventory
	}

	for _, item := range normalizeKickBackItems(opts.KickBackItems) {
		kick := PromoteKickBackResult{
			Item:   item,
			Branch: opts.PreProdBranch,
			Status: PromoteStatusSucceeded,
		}
		kicked, err := opts.Writer.KickBackFromPreProd(ctx, item, opts.PreProdBranch)
		if err != nil {
			kick.Status = PromoteStatusFailed
			kick.Error = err.Error()
			report.KickedBack = append(report.KickedBack, kick)
			report.Status = PromoteStatusFailed
			report.Summary.FailureCount++
			return finish()
		}
		kick.PRNumber = kicked.PRNumber
		kick.Branch = firstNonEmpty(kicked.Branch, opts.PreProdBranch)
		kick.RevertedSHA = kicked.RevertedSHA
		kick.SHA = kicked.SHA
		kick.URL = kicked.URL
		report.KickedBack = append(report.KickedBack, kick)
		report.Summary.KickedBackCount++

		needsHuman := PromoteNeedsHuman{
			Step:     "kick-back",
			Item:     item,
			PRNumber: kicked.PRNumber,
			Detail:   "kicked back from pre-prod; needs-human route could not be applied because no PR number was resolved",
		}
		if kicked.PRNumber > 0 {
			routed, err := opts.Writer.RouteKickBackToNeedsHuman(ctx, kicked.PRNumber)
			if err != nil {
				needsHuman.Detail = "kicked back from pre-prod, but needs-human routing failed: " + err.Error()
				report.NeedsHuman = append(report.NeedsHuman, needsHuman)
				report.Status = PromoteStatusFailed
				report.Summary.FailureCount++
				report.Promoted = PromoteMainResult{
					PreProdBranch: opts.PreProdBranch,
					MainBranch:    lcdefaults.BaseBranch,
					Head:          opts.PreProdBranch,
					Status:        PromoteStatusFailed,
					Error:         fmt.Sprintf("route kick-back PR #%d to needs-human: %v", kicked.PRNumber, err),
				}
				return finish()
			}
			needsHuman.Label = firstNonEmpty(routed.Label, "needs-human")
			needsHuman.RoutedIssues = append([]int(nil), routed.IssueNumbers...)
			needsHuman.Detail = promoteNeedsHumanRouteDetail(routed)
		}
		report.NeedsHuman = append(report.NeedsHuman, needsHuman)
	}

	priorStableCommit := readPromotePriorStableCommit(ctx, opts)
	promoted, err := opts.Writer.PromotePreProdToMain(ctx, opts.PreProdBranch)
	report.Promoted = PromoteMainResult{
		PreProdBranch:     opts.PreProdBranch,
		MainBranch:        lcdefaults.BaseBranch,
		Head:              opts.PreProdBranch,
		PriorStableCommit: priorStableCommit,
		Status:            PromoteStatusSucceeded,
	}
	if err != nil {
		report.Promoted.Status = PromoteStatusFailed
		report.Promoted.Error = err.Error()
		report.Status = PromoteStatusFailed
		report.Summary.FailureCount++
		return finish()
	}
	report.Promoted.PreProdBranch = firstNonEmpty(promoted.PreProdBranch, opts.PreProdBranch)
	report.Promoted.MainBranch = firstNonEmpty(promoted.MainBranch, lcdefaults.BaseBranch)
	report.Promoted.Head = promoted.Head
	report.Promoted.SHA = promoted.SHA
	report.Promoted.URL = promoted.URL
	report.Promoted.AlreadyUpToDate = promoted.AlreadyUpToDate
	report.Summary.PromotedCount = 1

	runPromoteProductionKeepsGreen(ctx, opts, &report)

	synced, err := opts.Writer.SyncPreProdFromMain(ctx, opts.PreProdBranch)
	report.Sync = PromoteSyncResult{
		PreProdBranch: opts.PreProdBranch,
		MainBranch:    lcdefaults.BaseBranch,
		Status:        PromoteStatusSucceeded,
	}
	if err != nil {
		report.Sync.Status = PromoteStatusFailed
		report.Sync.Error = err.Error()
		report.Status = PromoteStatusFailed
		report.Summary.FailureCount++
		return finish()
	}
	report.Sync.PreProdBranch = firstNonEmpty(synced.PreProdBranch, opts.PreProdBranch)
	report.Sync.MainBranch = firstNonEmpty(synced.MainBranch, lcdefaults.BaseBranch)
	report.Sync.SHA = synced.SHA
	report.Sync.URL = synced.URL
	return finish()
}

func runPromoteProductionKeepsGreen(ctx context.Context, opts PromoteOptions, report *PromoteReport) {
	if opts.Gate != GateAuto {
		return
	}
	if report == nil || report.Promoted.Status != PromoteStatusSucceeded || report.Promoted.AlreadyUpToDate {
		return
	}
	mainBranch := firstNonEmpty(report.Promoted.MainBranch, lcdefaults.BaseBranch)
	mergeSHA := strings.TrimSpace(report.Promoted.SHA)
	if mergeSHA == "" {
		detail := "production promotion did not return a merge commit SHA"
		report.ProductionHealth = &PromoteProductionHealthResult{
			Branch:         mainBranch,
			Status:         PreProdHealthStatusUnknown,
			RequiredChecks: normalizeRequiredChecks(opts.RequiredChecks),
			Checks:         []gh.Check{},
			Problems:       []string{detail},
			Error:          detail,
		}
		report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
			Step:   "production-health",
			Detail: detail,
		})
		return
	}

	branchChecks, err := opts.Writer.BranchChecks(ctx, mainBranch)
	health := PromoteProductionHealthResult{
		Branch:         firstNonEmpty(branchChecks.Branch, mainBranch),
		HeadSHA:        branchChecks.HeadSHA,
		MergeSHA:       mergeSHA,
		RequiredChecks: normalizeRequiredChecks(opts.RequiredChecks),
		Checks:         append([]gh.Check(nil), branchChecks.Checks...),
		Problems:       []string{},
	}
	if err != nil {
		health.Status = PreProdHealthStatusUnknown
		health.Error = err.Error()
		report.ProductionHealth = &health
		report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
			Step:   "production-health",
			Detail: err.Error(),
		})
		return
	}
	health.Status, health.Problems = preProdHealthStatus(health.RequiredChecks, health.Checks)
	report.ProductionHealth = &health

	switch health.Status {
	case PreProdHealthStatusGreen:
		return
	case PreProdHealthStatusRed:
		if sameGitSHA(health.HeadSHA, mergeSHA) {
			runPromoteProductionRollback(ctx, opts, report, mainBranch, health)
			return
		}
		detail := fmt.Sprintf("production CI is red at %s, not the just-promoted commit %s", firstNonEmpty(health.HeadSHA, "unknown"), mergeSHA)
		report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
			Step:   "production-health",
			Detail: detail,
		})
	case PreProdHealthStatusPending:
		return
	default:
		detail := "production CI is not green: " + strings.Join(health.Problems, ", ")
		if len(health.Problems) == 0 {
			detail = "production CI status is " + health.Status
		}
		report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
			Step:   "production-health",
			Detail: detail,
		})
	}
}

func runPromoteProductionRollback(ctx context.Context, opts PromoteOptions, report *PromoteReport, mainBranch string, health PromoteProductionHealthResult) {
	mergeCommit := strings.TrimSpace(report.Promoted.SHA)
	priorStableCommit := strings.TrimSpace(report.Promoted.PriorStableCommit)
	if mergeCommit == "" || priorStableCommit == "" {
		detail := "production CI red after promotion, but merge commit or prior stable commit is missing; refusing blind rollback"
		report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
			Step:   "production-rollback",
			Detail: detail,
		})
		return
	}

	rollback := PromoteRollbackResult{
		MainBranch:        mainBranch,
		MergeCommit:       mergeCommit,
		PriorStableCommit: priorStableCommit,
		Status:            PromoteStatusSucceeded,
	}
	reverted, err := opts.Writer.RevertProductionMerge(ctx, mainBranch, mergeCommit, priorStableCommit)
	if err != nil {
		rollback.Status = PromoteStatusFailed
		rollback.Error = err.Error()
		report.Rollback = &rollback
		report.Status = PromoteStatusFailed
		report.Summary.FailureCount++
		report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
			Step:   "production-rollback",
			Detail: "production CI red after promotion, and automatic rollback failed: " + err.Error(),
		})
		return
	}

	rollback.MainBranch = firstNonEmpty(reverted.Branch, mainBranch)
	rollback.MergeCommit = firstNonEmpty(reverted.MergeCommit, mergeCommit)
	rollback.PriorStableCommit = firstNonEmpty(reverted.PriorStableCommit, priorStableCommit)
	rollback.RevertSHA = reverted.SHA
	rollback.URL = reverted.URL
	report.Rollback = &rollback

	detail := fmt.Sprintf("production CI red after merge %s; reverted %s to prior stable %s", mergeCommit, rollback.MainBranch, priorStableCommit)
	if strings.TrimSpace(rollback.RevertSHA) != "" {
		detail += " with revert " + rollback.RevertSHA
	}
	if len(health.Problems) > 0 {
		detail += ": " + strings.Join(health.Problems, ", ")
	}
	report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
		Step:   "production-rollback",
		Detail: detail,
	})
}

func withPromoteDefaults(opts PromoteOptions) PromoteOptions {
	if opts.Clock == nil {
		opts.Clock = func() time.Time {
			return time.Now().UTC()
		}
	}
	if opts.StatePush == nil {
		opts.StatePush = func(ctx context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error) {
			return statebranch.Push(ctx, opts, statebranch.DefaultDeps())
		}
	}
	if opts.ToggleInventory == nil {
		opts.ToggleInventory = BuildPromoteToggleInventory
	}
	if opts.ReconcileParallelRun == nil {
		opts.ReconcileParallelRun = equivalence.ReconcileParallelRun
	}
	return opts
}

func recordPromoteAttempt(ctx context.Context, opts PromoteOptions, report *PromoteReport) error {
	reportJSON, err := json.Marshal(normalizePromoteReport(*report))
	if err != nil {
		return fmt.Errorf("marshal promote ledger event: %w", err)
	}

	exitCode := PromoteExitCode(*report)
	var errorMessage *string
	if report.Status == PromoteStatusFailed {
		text := promoteReportError(*report)
		errorMessage = &text
	}
	outcome := promoteLedgerOutcome(*report)
	if err := state.AppendEvent(opts.RepoPath, report.RunID, state.Event{
		Timestamp:         report.FinishedAt,
		RunID:             report.RunID,
		JobID:             "promote",
		Issue:             0,
		Phase:             "promote",
		Status:            outcome,
		LogBytes:          0,
		ExitCode:          &exitCode,
		Error:             errorMessage,
		Event:             promoteLedgerEvent,
		Outcome:           outcome,
		MergeCommit:       strings.TrimSpace(report.Promoted.SHA),
		PriorStableCommit: strings.TrimSpace(report.Promoted.PriorStableCommit),
		Details:           json.RawMessage(reportJSON),
	}); err != nil {
		return fmt.Errorf("append promote ledger event: %w", err)
	}

	if report.Rollback != nil {
		rollbackJSON, err := json.Marshal(report.Rollback)
		if err != nil {
			return fmt.Errorf("marshal promote rollback ledger event: %w", err)
		}
		rollbackExitCode := 0
		var rollbackError *string
		rollbackOutcome := strings.TrimSpace(report.Rollback.Status)
		if rollbackOutcome == "" {
			rollbackOutcome = PromoteStatusSucceeded
		}
		if rollbackOutcome == PromoteStatusFailed {
			rollbackExitCode = 1
			text := firstNonEmpty(report.Rollback.Error, "production rollback failed")
			rollbackError = &text
		}
		if err := state.AppendEvent(opts.RepoPath, report.RunID, state.Event{
			Timestamp:         report.FinishedAt,
			RunID:             report.RunID,
			JobID:             "promote",
			Issue:             0,
			Phase:             "promote",
			Status:            rollbackOutcome,
			LogBytes:          0,
			ExitCode:          &rollbackExitCode,
			Error:             rollbackError,
			Event:             promoteRollbackLedgerEvent,
			Outcome:           rollbackOutcome,
			MergeCommit:       strings.TrimSpace(report.Rollback.MergeCommit),
			PriorStableCommit: strings.TrimSpace(report.Rollback.PriorStableCommit),
			Details:           json.RawMessage(rollbackJSON),
		}); err != nil {
			return fmt.Errorf("append promote rollback ledger event: %w", err)
		}
	}

	result, err := opts.StatePush(ctx, statebranch.PushOptions{
		RepoPath: opts.RepoPath,
		RunID:    report.RunID,
	})
	report.StatePush = promoteStatePush(result)
	if err != nil {
		report.StatePush.Error = err.Error()
		return fmt.Errorf("push promote ledger: %w", err)
	}
	if strings.TrimSpace(result.PushError) != "" {
		return fmt.Errorf("push promote ledger: %s", result.PushError)
	}
	return nil
}

func readPromotePriorStableCommit(ctx context.Context, opts PromoteOptions) string {
	sha, err := opts.Writer.BranchHeadSHA(ctx, lcdefaults.BaseBranch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read %s head before promotion: %v\n", lcdefaults.BaseBranch, err)
		return ""
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		fmt.Fprintf(os.Stderr, "warning: %s head read before promotion returned an empty SHA\n", lcdefaults.BaseBranch)
	}
	return sha
}

func promoteStatePush(result statebranch.PushResult) *PromoteStatePush {
	return &PromoteStatePush{
		Branch:    result.Branch,
		Remote:    result.Remote,
		Committed: result.Committed,
		Pushed:    result.Pushed,
		PushError: result.PushError,
		Files:     append([]string(nil), result.Files...),
	}
}

func promoteLedgerOutcome(report PromoteReport) string {
	if report.Status != PromoteStatusSucceeded {
		return PromoteOutcomeFailed
	}
	if report.Promoted.AlreadyUpToDate {
		return PromoteOutcomeSkippedAsDone
	}
	return PromoteOutcomePromoted
}

func promoteReportError(report PromoteReport) string {
	for _, kicked := range report.KickedBack {
		if strings.TrimSpace(kicked.Error) != "" {
			return kicked.Error
		}
	}
	if strings.TrimSpace(report.Promoted.Error) != "" {
		return report.Promoted.Error
	}
	if report.Rollback != nil && strings.TrimSpace(report.Rollback.Error) != "" {
		return report.Rollback.Error
	}
	if strings.TrimSpace(report.Sync.Error) != "" {
		return report.Sync.Error
	}
	if report.StatePush != nil {
		if strings.TrimSpace(report.StatePush.Error) != "" {
			return report.StatePush.Error
		}
		if strings.TrimSpace(report.StatePush.PushError) != "" {
			return report.StatePush.PushError
		}
	}
	return "promote failed"
}

func assemblePromoteGoNoGoPanel(report PromoteReport) *PromoteGoNoGoPanel {
	panel := PromoteGoNoGoPanel{}
	if report.GoNoGoPanel != nil {
		panel = *report.GoNoGoPanel
	}
	if promoteToggleInventoryHasItems(report.ToggleInventory) {
		inventory := normalizePromoteToggleInventory(report.ToggleInventory)
		panel.ToggleInventory = &inventory
	}
	if len(report.NeedsHuman) > 0 {
		panel.NeedsHuman = append([]PromoteNeedsHuman(nil), report.NeedsHuman...)
	}
	if failed := promoteFailedItems(report); len(failed) > 0 {
		panel.Failed = failed
	}
	return normalizePromoteGoNoGoPanel(panel)
}

func normalizePromoteGoNoGoPanel(panel PromoteGoNoGoPanel) *PromoteGoNoGoPanel {
	if panel.Reconciliation != nil && !parallelRunReportHasEvidence(*panel.Reconciliation) {
		panel.Reconciliation = nil
	}
	if panel.ToggleInventory != nil {
		inventory := normalizePromoteToggleInventory(*panel.ToggleInventory)
		if promoteToggleInventoryHasItems(inventory) {
			panel.ToggleInventory = &inventory
		} else {
			panel.ToggleInventory = nil
		}
	}
	if len(panel.NeedsHuman) == 0 {
		panel.NeedsHuman = nil
	}
	if len(panel.Failed) == 0 {
		panel.Failed = nil
	}
	if panel.Reconciliation == nil && panel.ToggleInventory == nil && len(panel.NeedsHuman) == 0 && len(panel.Failed) == 0 {
		return nil
	}
	return &panel
}

func parallelRunReportHasEvidence(report equivalence.ParallelRunReport) bool {
	return report.Version != 0 ||
		strings.TrimSpace(report.Status) != "" ||
		report.MatchedCount != 0 ||
		report.OldOnlyCount != 0 ||
		report.NewOnlyCount != 0 ||
		report.MismatchCount != 0 ||
		len(report.Matched) != 0 ||
		len(report.Unmatched) != 0
}

func promoteToggleInventoryHasItems(inventory PromoteToggleInventory) bool {
	return len(inventory.FlipOn) > 0 || len(inventory.LeaveDark) > 0
}

func promoteFailedItems(report PromoteReport) []PromoteFailedItem {
	var failed []PromoteFailedItem
	for _, kicked := range report.KickedBack {
		if !promoteResultFailed(kicked.Status, kicked.Error) {
			continue
		}
		failed = append(failed, PromoteFailedItem{
			Step:     "kick-back",
			Item:     kicked.Item,
			PRNumber: kicked.PRNumber,
			Status:   firstNonEmpty(kicked.Status, PromoteStatusFailed),
			Detail:   firstNonEmpty(kicked.Error, "kick-back failed"),
		})
	}
	if promoteResultFailed(report.Promoted.Status, report.Promoted.Error) {
		failed = append(failed, PromoteFailedItem{
			Step:   "promote",
			Status: firstNonEmpty(report.Promoted.Status, PromoteStatusFailed),
			Detail: firstNonEmpty(report.Promoted.Error, "promote failed"),
		})
	}
	if promoteResultFailed(report.Sync.Status, report.Sync.Error) {
		failed = append(failed, PromoteFailedItem{
			Step:   "pre-prod-sync",
			Status: firstNonEmpty(report.Sync.Status, PromoteStatusFailed),
			Detail: firstNonEmpty(report.Sync.Error, "pre-prod sync failed"),
		})
	}
	if report.Rollback != nil && promoteResultFailed(report.Rollback.Status, report.Rollback.Error) {
		failed = append(failed, PromoteFailedItem{
			Step:   "production-rollback",
			Status: firstNonEmpty(report.Rollback.Status, PromoteStatusFailed),
			Detail: firstNonEmpty(report.Rollback.Error, "production rollback failed"),
		})
	}
	if report.StatePush != nil && promoteResultFailed("", firstNonEmpty(report.StatePush.Error, report.StatePush.PushError)) {
		failed = append(failed, PromoteFailedItem{
			Step:   "state-push",
			Status: PromoteStatusFailed,
			Detail: firstNonEmpty(report.StatePush.Error, report.StatePush.PushError),
		})
	}
	return failed
}

func promoteResultFailed(status, detail string) bool {
	return status == PromoteStatusFailed || strings.TrimSpace(detail) != ""
}

func PromoteExitCode(report PromoteReport) int {
	if report.Status == PromoteStatusSucceeded {
		return 0
	}
	return 1
}

func normalizePromoteReport(report PromoteReport) PromoteReport {
	if report.Version == 0 {
		report.Version = PromoteReportVersion
	}
	if strings.TrimSpace(report.MainBranch) == "" {
		report.MainBranch = lcdefaults.BaseBranch
	}
	report.Gate = normalizePromotionGate(report.Gate)
	if report.KickedBack == nil {
		report.KickedBack = []PromoteKickBackResult{}
	}
	if report.NeedsHuman == nil {
		report.NeedsHuman = []PromoteNeedsHuman{}
	}
	report.ToggleInventory = normalizePromoteToggleInventory(report.ToggleInventory)
	if report.GoNoGoPanel != nil {
		report.GoNoGoPanel = normalizePromoteGoNoGoPanel(*report.GoNoGoPanel)
	} else {
		report.GoNoGoPanel = assemblePromoteGoNoGoPanel(report)
	}
	if report.ProductionHealth != nil {
		health := *report.ProductionHealth
		health.Branch = strings.TrimSpace(health.Branch)
		health.HeadSHA = strings.TrimSpace(health.HeadSHA)
		health.MergeSHA = strings.TrimSpace(health.MergeSHA)
		health.Status = strings.TrimSpace(health.Status)
		health.RequiredChecks = normalizeRequiredChecks(health.RequiredChecks)
		if health.Checks == nil {
			health.Checks = []gh.Check{}
		}
		if health.Problems == nil {
			health.Problems = []string{}
		}
		report.ProductionHealth = &health
	}
	if report.Rollback != nil {
		rollback := *report.Rollback
		rollback.MainBranch = strings.TrimSpace(rollback.MainBranch)
		rollback.MergeCommit = strings.TrimSpace(rollback.MergeCommit)
		rollback.PriorStableCommit = strings.TrimSpace(rollback.PriorStableCommit)
		rollback.RevertSHA = strings.TrimSpace(rollback.RevertSHA)
		rollback.Status = strings.TrimSpace(rollback.Status)
		if rollback.Status == "" {
			rollback.Status = PromoteStatusSucceeded
		}
		report.Rollback = &rollback
	}
	if strings.TrimSpace(report.Promoted.PreProdBranch) == "" {
		report.Promoted.PreProdBranch = report.PreProdBranch
	}
	if strings.TrimSpace(report.Promoted.MainBranch) == "" {
		report.Promoted.MainBranch = report.MainBranch
	}
	if strings.TrimSpace(report.Sync.PreProdBranch) == "" {
		report.Sync.PreProdBranch = report.PreProdBranch
	}
	if strings.TrimSpace(report.Sync.MainBranch) == "" {
		report.Sync.MainBranch = report.MainBranch
	}
	if report.StatePush != nil && report.StatePush.Files == nil {
		report.StatePush.Files = []string{}
	}
	report.Summary.NeedsHumanCount = len(report.NeedsHuman)
	return report
}

func normalizePromotionGate(gate string) string {
	gate = strings.TrimSpace(gate)
	if gate == "" {
		return GateAuto
	}
	return gate
}

func validatePromotionGate(gate string) error {
	switch gate {
	case GateHumanMerge, GateAuto:
		return nil
	default:
		return fmt.Errorf("invalid adapters.gate %q; allowed values: %s, %s", gate, GateHumanMerge, GateAuto)
	}
}

func evaluateAutoGate(in AutoGateInputs) (bool, string) {
	if in.RedLineClean == nil {
		return false, "red-line floor signal missing"
	}
	if !*in.RedLineClean {
		return false, "red-line floor blocked promotion"
	}
	if in.CIGreen == nil {
		return false, "CI green signal missing"
	}
	if !*in.CIGreen {
		return false, "CI green is false"
	}
	if in.VerdictPass == nil {
		return false, "verdict pass signal missing"
	}
	if !*in.VerdictPass {
		return false, "verdict pass is false"
	}
	if in.EvidencePresent == nil {
		return false, "evidence present signal missing"
	}
	if !*in.EvidencePresent {
		return false, "evidence present is false"
	}
	return true, "auto-gate passed"
}

func normalizeKickBackItems(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range items {
		item = normalizeKickBackItem(item)
		key := strings.ToLower(item)
		if item == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

func normalizeKickBackItem(item string) string {
	return kickback.CanonicalizeItem(item)
}

func isReservedPromotionBranch(branch string) bool {
	switch strings.ToLower(strings.TrimSpace(branch)) {
	case "main", "master", "prod", "production":
		return true
	default:
		return false
	}
}

func promoteNeedsHumanRouteDetail(routed gh.NeedsHumanRouteResult) string {
	label := firstNonEmpty(routed.Label, "needs-human")
	if len(routed.IssueNumbers) > 0 {
		return fmt.Sprintf("kicked back from pre-prod; applied %s label to %s", label, formatPromoteIssueRefs(routed.IssueNumbers))
	}
	if routed.PRLabeled {
		return fmt.Sprintf("kicked back from pre-prod; applied %s label to PR #%d", label, routed.PRNumber)
	}
	return fmt.Sprintf("kicked back from pre-prod; applied %s needs-human route", label)
}

func formatPromoteIssueRefs(numbers []int) string {
	parts := make([]string, 0, len(numbers))
	for _, number := range numbers {
		if number > 0 {
			parts = append(parts, fmt.Sprintf("#%d", number))
		}
	}
	if len(parts) == 0 {
		return "no issues"
	}
	return strings.Join(parts, ", ")
}
