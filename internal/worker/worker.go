package worker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/lockfile"
	"github.com/jasonhnd/loopcoder/internal/mcp"
	"github.com/jasonhnd/loopcoder/internal/pathid"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/skills"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	WorkerHardCap      = lcdefaults.WorkerHardCap
	WorkerStallTimeout = lcdefaults.WorkerStallTimeout
)

const scratchOwnerFile = ".loopcoder-attempt.json"

type Options struct {
	RepoPath        string
	IssueNumber     int
	IssueTitle      string
	IssueBody       string
	BaseBranch      string
	Branch          string
	RunID           string
	ProviderKey     string
	Attempt         int
	RecoveryContext string
	Provider        string
	Model           string
	Effort          string
	Timeout         time.Duration
	ConfigFromBase  bool
	KeepWorktree    bool
	Stderr          io.Writer
}

type Result struct {
	OK          bool             `json:"ok"`
	Issue       int              `json:"issue"`
	Branch      string           `json:"branch"`
	RunID       string           `json:"run_id"`
	PR          string           `json:"pr"`
	Summary     string           `json:"summary"`
	AttemptPath string           `json:"attempt_path"`
	Status      string           `json:"status"`
	ExitCode    int              `json:"exit_code"`
	LogBytes    int64            `json:"log_bytes"`
	Reason      string           `json:"reason,omitempty"`
	NextAction  string           `json:"next_action,omitempty"`
	Report      *reporter.Report `json:"report,omitempty"`
}

type GitClient interface {
	FetchOriginBase(ctx context.Context, repoPath, baseBranch string) error
	WorktreeAdd(ctx context.Context, repoPath, branch, worktreePath, baseBranch string) error
	WorktreeRemove(ctx context.Context, repoPath, worktreePath string) error
	StatusPorcelain(ctx context.Context, repoPath string) (string, error)
	AddAll(ctx context.Context, repoPath string) error
	Commit(ctx context.Context, repoPath, message string) error
	PushUpstream(ctx context.Context, repoPath, branch string) error
	PushUpstreamForceWithLease(ctx context.Context, repoPath, branch string) error
	BranchDelete(ctx context.Context, repoPath, branch string) error
}

type GitHubClient interface {
	RepoName(ctx context.Context) (string, error)
	CreatePR(ctx context.Context, head, base, title, body string) (string, error)
	ListHeadPRs(ctx context.Context, branch string) ([]gh.PullRequestReference, error)
	ListOpenPRs(ctx context.Context) ([]gh.PullRequest, error)
}

type Lock interface {
	Release() error
}

type Deps struct {
	Git                GitClient
	GitHub             func(repoPath string) GitHubClient
	AgentLookup        func(provider string) (agent.Runner, error)
	AcquireLock        func(repoPath string, timeout time.Duration) (Lock, error)
	Now                func() time.Time
	PID                func() int
	MkdirTemp          func(dir, pattern string) (string, error)
	MkdirAll           func(path string, perm os.FileMode) error
	WriteFile          func(path string, data []byte, perm os.FileMode) error
	Stat               func(path string) (os.FileInfo, error)
	RemoveAll          func(path string) error
	RepoSkills         func(repoPath string, domainSkills config.DomainSkills) (string, error)
	OpenProgressStore  func(context.Context, storage.Options) (storage.Store, error)
	OpenStore          func(context.Context, storage.Options) (storage.Store, error)
	ProgressClock      progress.Clock
	ProgressMaxSilence time.Duration
}

func DefaultDeps() Deps {
	return Deps{
		Git: gitutil.New(),
		GitHub: func(repoPath string) GitHubClient {
			return gh.New(repoPath)
		},
		AgentLookup: agent.Lookup,
		AcquireLock: func(repoPath string, timeout time.Duration) (Lock, error) {
			return lockfile.Acquire(repoPath, timeout)
		},
		Now:               time.Now,
		PID:               os.Getpid,
		MkdirTemp:         os.MkdirTemp,
		MkdirAll:          os.MkdirAll,
		WriteFile:         os.WriteFile,
		Stat:              os.Stat,
		RemoveAll:         os.RemoveAll,
		OpenProgressStore: storage.Open,
		RepoSkills: func(repoPath string, domainSkills config.DomainSkills) (string, error) {
			return skills.BuildPromptSection(skills.PromptSectionOptions{
				RepoPath:            repoPath,
				Paths:               domainSkills.Paths,
				MachineLibraryPaths: domainSkills.MachineLibrary.Paths,
				Select:              domainSkills.Select,
				BudgetBytes:         domainSkills.PromptBudgetBytes,
			})
		},
		OpenStore: storage.Open,
	}
}

type dispatchContext struct {
	opts     Options
	deps     Deps
	warnings io.Writer
	repoPath string
	github   GitHubClient
	agentRun agent.Runner

	scratch        string
	worktreePath   string
	promptPath     string
	summaryPath    string
	logPath        string
	runtimeRoots   runtimepath.Roots
	ownershipStore storage.Store
	ownershipLease *storage.AgentOwnershipLease
	jobID          string
	attemptPath    string
	tracker        *attemptTracker

	domainPolicy      domainWorkerPolicy
	progressRecorder  *progressRecorder
	activePhase       string
	dispatchSucceeded bool
	preserveArtifacts bool
	preserveReason    string
	cleanupStatus     string
	failureStatus     string
}

func Dispatch(ctx context.Context, opts Options, deps Deps) (result Result, err error) {
	dispatch, err := prepareDispatch(ctx, opts, deps)
	if err != nil {
		return Result{}, err
	}
	if err := prepareWorktree(ctx, dispatch); err != nil {
		writeRecovery(ctx, dispatch, err)
		return Result{}, err
	}
	defer func() {
		cleanup(ctx, dispatch, err)
	}()

	invocation, err := buildInvocation(ctx, dispatch)
	if err != nil {
		return Result{}, err
	}
	agentResult, agentErr := runAgent(ctx, dispatch, invocation)
	if agentResult.Hung {
		result, err = handleHungOrPartialWork(ctx, dispatch, agentResult)
		return result, err
	}
	if agentErr != nil {
		return Result{}, fmt.Errorf("%s exec failed: %w", dispatch.opts.Provider, agentErr)
	}
	if agentResult.ExitCode != 0 {
		return Result{}, fmt.Errorf("%s exec failed (exit %d). See %s", dispatch.opts.Provider, agentResult.ExitCode, dispatch.logPath)
	}
	return commitAndOpenPR(ctx, dispatch, agentResult)
}

func prepareDispatch(ctx context.Context, opts Options, deps Deps) (*dispatchContext, error) {
	deps = withDefaults(deps)
	warnings := opts.Stderr
	if warnings == nil {
		warnings = io.Discard
	}

	if opts.IssueNumber <= 0 {
		return nil, errors.New("issue number is required")
	}
	if strings.TrimSpace(opts.IssueTitle) == "" {
		return nil, errors.New("issue title is required")
	}
	if strings.TrimSpace(opts.Provider) == "" {
		opts.Provider = "codex"
	}
	agentRunner, lookupErr := deps.AgentLookup(opts.Provider)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if agentRunner == nil {
		return nil, fmt.Errorf("provider %q resolved to nil runner", opts.Provider)
	}
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = lcdefaults.BaseBranch
	}
	if strings.TrimSpace(opts.Branch) == "" {
		opts.Branch = fmt.Sprintf("loop/issue-%d", opts.IssueNumber)
	}
	if opts.Attempt <= 0 {
		opts.Attempt = 1
	}

	repoPath, err := resolveRepo(opts.RepoPath)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.RunID) == "" {
		opts.RunID = state.RunIDForIssue(opts.IssueNumber, deps.Now())
	}
	runtimeRoots, err := runtimepath.Resolve(ctx, repoPath)
	if err != nil {
		return nil, err
	}

	github := deps.GitHub(repoPath)
	if github == nil {
		return nil, errors.New("github client is not configured")
	}
	if repoName, err := github.RepoName(ctx); err != nil {
		return nil, fmt.Errorf("resolve GitHub repo: %w", err)
	} else if strings.TrimSpace(repoName) == "" {
		return nil, errors.New("resolve GitHub repo: empty repo name")
	}

	return &dispatchContext{
		opts:         opts,
		deps:         deps,
		warnings:     warnings,
		repoPath:     repoPath,
		runtimeRoots: runtimeRoots,
		github:       github,
		agentRun:     agentRunner,
	}, nil
}

func prepareWorktree(ctx context.Context, dispatch *dispatchContext) error {
	tempRoot := ""
	if dispatch.runtimeRoots.Registered {
		tempRoot = dispatch.runtimeRoots.TmpRoot
		if err := os.MkdirAll(tempRoot, 0o700); err != nil {
			return fmt.Errorf("create registered temp root: %w", err)
		}
	}
	// Unregistered projects intentionally keep the legacy fallback behavior:
	// use the OS temp directory, outside the repo, until the user registers the
	// project and opts into the v0.7 home-scoped runtime contract.
	scratch, err := dispatch.deps.MkdirTemp(tempRoot, "loopcoder-*")
	if err != nil {
		return fmt.Errorf("create scratch directory: %w", err)
	}
	dispatch.scratch = scratch
	dispatch.worktreePath = filepath.Join(scratch, "wt")
	dispatch.promptPath = filepath.Join(scratch, "prompt.txt")
	dispatch.summaryPath = filepath.Join(scratch, "summary.txt")
	dispatch.logPath = filepath.Join(scratch, "codex.log")
	dispatch.jobID = fmt.Sprintf("job-%d-%d", dispatch.opts.IssueNumber, dispatch.deps.PID())
	if err := writeScratchOwner(dispatch); err != nil {
		return err
	}
	if dispatch.runtimeRoots.Registered {
		logDir := filepath.Join(dispatch.runtimeRoots.LogsRoot, dispatch.opts.RunID)
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			return fmt.Errorf("create registered log root: %w", err)
		}
		dispatch.logPath = filepath.Join(logDir, dispatch.jobID+".log")
	}
	dispatch.attemptPath = state.AttemptPath(dispatch.repoPath, dispatch.opts.RunID, dispatch.jobID)
	dispatch.tracker = newAttemptTracker(attemptTrackerOptions{
		repoPath:    dispatch.repoPath,
		runID:       dispatch.opts.RunID,
		jobID:       dispatch.jobID,
		issue:       dispatch.opts.IssueNumber,
		attempt:     dispatch.opts.Attempt,
		provider:    dispatch.opts.Provider,
		pid:         dispatch.deps.PID(),
		branch:      dispatch.opts.Branch,
		logPath:     dispatch.logPath,
		startedAt:   dispatch.deps.Now(),
		now:         dispatch.deps.Now,
		warnings:    dispatch.warnings,
		attemptPath: dispatch.attemptPath,
	})
	recorder, err := newProgressRecorder(ctx, dispatch.opts, dispatch.deps, dispatch.runtimeRoots, dispatch.jobID, dispatch.warnings, func(ctx context.Context) error {
		return validateWorkerOwnership(ctx, dispatch)
	})
	if err != nil {
		return err
	}
	dispatch.progressRecorder = recorder
	dispatch.tracker.progress = recorder
	dispatch.activePhase = "worktree_created"
	dispatch.cleanupStatus = "succeeded"
	dispatch.failureStatus = "failed"

	if err := acquireWorkerOwnership(ctx, dispatch); err != nil {
		return err
	}
	ownershipPrepared := false
	defer func() {
		if !ownershipPrepared {
			releaseWorkerOwnership(dispatch)
			closeWorkerOwnershipStore(dispatch)
		}
	}()
	if err := dispatch.deps.Git.FetchOriginBase(ctx, dispatch.repoPath, dispatch.opts.BaseBranch); err != nil {
		return fmt.Errorf("git fetch origin %s: %w", dispatch.opts.BaseBranch, err)
	}
	if err := addWorktreeWithLock(ctx, dispatch.deps, dispatch.repoPath, dispatch.opts.Branch, dispatch.worktreePath, dispatch.opts.BaseBranch); err != nil {
		return err
	}
	dispatch.tracker.transition(dispatch.activePhase, "running", nil, nil)
	ownershipPrepared = true
	return nil
}

func buildInvocation(ctx context.Context, dispatch *dispatchContext) (agent.Invocation, error) {
	if err := validateWorkerOwnership(ctx, dispatch); err != nil {
		return agent.Invocation{}, err
	}
	dispatch.activePhase = "prompt_written"
	cfg, err := config.LoadForRepo(ctx, dispatch.repoPath, config.LoadOptions{
		BaseBranch:     dispatch.opts.BaseBranch,
		ConfigFromBase: dispatch.opts.ConfigFromBase,
		Warnings:       dispatch.warnings,
	})
	if err != nil {
		return agent.Invocation{}, err
	}
	repoSkills, err := dispatch.deps.RepoSkills(dispatch.worktreePath, cfg.Domain.Skills)
	if err != nil {
		return agent.Invocation{}, fmt.Errorf("read repo skills: %w", err)
	}
	prompt := BuildPrompt(PromptOptions{
		IssueNumber:     dispatch.opts.IssueNumber,
		IssueTitle:      dispatch.opts.IssueTitle,
		IssueBody:       dispatch.opts.IssueBody,
		Branch:          dispatch.opts.Branch,
		ProviderKey:     dispatch.opts.ProviderKey,
		RecoveryContext: dispatch.opts.RecoveryContext,
		RepoSkills:      repoSkills,
	})
	if err := os.WriteFile(dispatch.promptPath, []byte(prompt), 0o600); err != nil {
		return agent.Invocation{}, fmt.Errorf("write prompt: %w", err)
	}
	dispatch.tracker.transition(dispatch.activePhase, "running", nil, nil)

	dispatch.activePhase = "codex_started"
	dispatch.tracker.transition(dispatch.activePhase, "running", nil, nil)
	mcpServers, err := mcp.ServersForInvocation(cfg.MCP, mcp.RoleWorker, false)
	if err != nil {
		return agent.Invocation{}, err
	}
	resilience := cfg.Resilience
	hardCap := config.DurationSeconds(resilience.Worker.HardCapSeconds, WorkerHardCap)
	if dispatch.opts.Timeout > 0 {
		hardCap = dispatch.opts.Timeout
	}
	domainPolicy, err := resolveDomainWorkerPolicy(cfg)
	if err != nil {
		return agent.Invocation{}, err
	}
	dispatch.domainPolicy = domainPolicy
	return agent.Invocation{
		WorktreePath:    dispatch.worktreePath,
		Prompt:          prompt,
		LogPath:         dispatch.logPath,
		Stderr:          dispatch.warnings,
		Model:           dispatch.opts.Model,
		Effort:          dispatch.opts.Effort,
		HardCap:         hardCap,
		StallTimeout:    config.DurationSeconds(resilience.Worker.StallTimeoutSeconds, WorkerStallTimeout),
		LivenessMode:    domainPolicy.AgentLivenessMode(),
		LivenessCommand: domainPolicy.LivenessCommand,
		RunID:           dispatch.opts.RunID,
		ProviderKey:     dispatch.opts.ProviderKey,
		Role:            "worker",
		MCPServers:      mcpServers,
	}, nil
}

func runAgent(ctx context.Context, dispatch *dispatchContext, invocation agent.Invocation) (agent.Result, error) {
	if err := validateWorkerOwnership(ctx, dispatch); err != nil {
		return agent.Result{}, err
	}
	agentResult, agentErr := dispatch.agentRun.Run(ctx, invocation)
	if err := validateWorkerOwnership(ctx, dispatch); err != nil {
		return agentResult, err
	}
	dispatch.activePhase = "codex_exited"
	var exitCodePtr *int
	if agentResult.ExitCode >= 0 {
		exitCode := agentResult.ExitCode
		exitCodePtr = &exitCode
	}
	dispatch.tracker.transition(dispatch.activePhase, "running", exitCodePtr, nil)
	return agentResult, agentErr
}

func handleHungOrPartialWork(ctx context.Context, dispatch *dispatchContext, agentResult agent.Result) (Result, error) {
	if err := validateWorkerOwnership(ctx, dispatch); err != nil {
		return hungResult(dispatch, agentResult), err
	}
	dispatch.failureStatus = "hung"
	hungErr := workerHungError(dispatch.opts.Provider, agentResult.HungReason, dispatch.logPath)
	// The deferred failure handler records the "hung" transition (phase +
	// error) on return; only the distinct hung run-event is emitted here.
	dispatch.tracker.appendEvent("worker_hung", "hung", map[string]string{
		"reason":      "hung",
		"hung_reason": firstNonEmpty(agentResult.HungReason, "unknown"),
		"provider":    dispatch.opts.Provider,
	})
	if dispatch.domainPolicy.PartialWorkMode == partialWorkModeReportOnly {
		dirty, dirtyErr := dispatch.deps.Git.StatusPorcelain(ctx, dispatch.worktreePath)
		if dirtyErr != nil {
			return hungResult(dispatch, agentResult), fmt.Errorf("%s; report-only partial-work check failed: %w", hungErr, dirtyErr)
		}
		if strings.TrimSpace(dirty) != "" {
			fmt.Fprintf(dispatch.warnings, "[loopcoder] domain.partial_work.mode=report-only preserved partial work at %s; no harvest PR opened\n", dispatch.scratch)
			dispatch.tracker.appendEvent("worker_partial_work_reported", "hung", map[string]string{
				"mode": "report-only",
			})
		}
		return hungResult(dispatch, agentResult), errors.New(hungErr)
	}
	harvest, harvestErr := harvestHungWorktree(ctx, hungHarvestOptions{
		repoPath:     dispatch.repoPath,
		runID:        dispatch.opts.RunID,
		jobID:        dispatch.jobID,
		worktreePath: dispatch.worktreePath,
		logPath:      dispatch.logPath,
		summaryPath:  dispatch.summaryPath,
		opts:         dispatch.opts,
		agentResult:  agentResult,
		errorMessage: hungErr,
		git:          dispatch.deps.Git,
		github:       dispatch.github,
		now:          dispatch.deps.Now,
		warnings:     dispatch.warnings,
		validateOwnership: func(ctx context.Context) error {
			return validateWorkerOwnership(ctx, dispatch)
		},
	})
	if harvestErr != nil {
		return hungResult(dispatch, agentResult), fmt.Errorf("%s; harvest failed: %w", hungErr, harvestErr)
	}
	if harvest != nil {
		exitCode := 0
		dispatch.tracker.branch = harvest.Branch
		dispatch.tracker.setReport(harvest.Report)
		dispatch.tracker.setUsage(harvest.Report.Usage)
		dispatch.tracker.transition(harvest.Phase, "needs-human", &exitCode, nil)
		dispatch.tracker.appendEvent("worker_harvested", "needs-human", map[string]string{
			"branch": harvest.Branch,
			"pr":     harvest.PR,
			"mode":   harvest.Mode,
		})
		dispatch.dispatchSucceeded = true
		dispatch.preserveReason = "harvested hung/killed worker needs human review"
		dispatch.cleanupStatus = "needs-human"
		selectArtifactPreservation(dispatch, dispatch.preserveReason, nil)
		return Result{
			OK:          true,
			Issue:       dispatch.opts.IssueNumber,
			Branch:      harvest.Branch,
			RunID:       dispatch.opts.RunID,
			PR:          harvest.PR,
			Summary:     harvest.Summary,
			AttemptPath: dispatch.attemptPath,
			Status:      "needs-human",
			ExitCode:    exitCode,
			LogBytes:    fileSize(dispatch.logPath),
			Reason:      harvest.Summary,
			NextAction:  "human should review the harvested partial work before continuing",
			Report:      &harvest.Report,
		}, nil
	}
	return hungResult(dispatch, agentResult), errors.New(hungErr)
}

func commitAndOpenPR(ctx context.Context, dispatch *dispatchContext, agentResult agent.Result) (Result, error) {
	if err := validateWorkerOwnership(ctx, dispatch); err != nil {
		return Result{}, err
	}
	reportRecord := buildWorkerReport(dispatch.opts, agentResult)
	reportRecord.Worktree = dispatch.worktreePath
	dispatch.tracker.setReport(reportRecord)
	dispatch.tracker.writeAttempt()
	if err := reportRecord.Validate(); err != nil {
		return Result{}, fmt.Errorf("validate worker report: %w", err)
	}
	dispatch.tracker.setUsage(reportRecord.Usage)
	dispatch.tracker.writeAttempt()
	if _, err := reportRecord.CanonicalJSON(); err != nil {
		return Result{}, fmt.Errorf("render worker report JSON: %w", err)
	}

	summary := fmt.Sprintf("(%s produced no summary)", dispatch.opts.Provider)
	if trimmed := strings.TrimSpace(agentResult.Summary); trimmed != "" {
		summary = trimmed
	}

	dispatch.activePhase = "dirty_checked"
	if err := validateWorkerOwnership(ctx, dispatch); err != nil {
		return Result{}, err
	}
	dirty, err := dispatch.deps.Git.StatusPorcelain(ctx, dispatch.worktreePath)
	if err != nil {
		return Result{}, fmt.Errorf("git status --porcelain: %w", err)
	}
	if strings.TrimSpace(dirty) == "" {
		return Result{}, fmt.Errorf("codex made no file changes for issue #%d (nothing to commit)", dispatch.opts.IssueNumber)
	}
	dispatch.tracker.transition(dispatch.activePhase, "running", dispatch.tracker.exitCode, nil)

	if err := validateWorkerOwnership(ctx, dispatch); err != nil {
		return Result{}, err
	}
	if err := dispatch.deps.Git.AddAll(ctx, dispatch.worktreePath); err != nil {
		return Result{}, fmt.Errorf("git add -A: %w", err)
	}
	dispatch.activePhase = "committed"
	if err := validateWorkerOwnership(ctx, dispatch); err != nil {
		return Result{}, err
	}
	if err := dispatch.deps.Git.Commit(ctx, dispatch.worktreePath, buildCommitMessage(dispatch.opts.IssueTitle, dispatch.opts.IssueNumber)); err != nil {
		return Result{}, fmt.Errorf("git commit: %w", err)
	}
	dispatch.tracker.transition(dispatch.activePhase, "running", dispatch.tracker.exitCode, nil)

	dispatch.activePhase = "pushed"
	if err := validateWorkerOwnership(ctx, dispatch); err != nil {
		return Result{}, err
	}
	if err := dispatch.deps.Git.PushUpstream(ctx, dispatch.worktreePath, dispatch.opts.Branch); err != nil {
		return Result{}, fmt.Errorf("git push -u origin %s: %w", dispatch.opts.Branch, err)
	}
	dispatch.tracker.transition(dispatch.activePhase, "running", dispatch.tracker.exitCode, nil)

	dispatch.activePhase = "pr_opened"
	if err := validateWorkerOwnership(ctx, dispatch); err != nil {
		return Result{}, err
	}
	body := buildPRBody(dispatch.opts.IssueNumber, summary)
	prURL, err := dispatch.github.CreatePR(ctx, dispatch.opts.Branch, dispatch.opts.BaseBranch, dispatch.opts.IssueTitle, body)
	if err != nil {
		return Result{}, fmt.Errorf("gh pr create: %w", err)
	}
	dispatch.tracker.transition(dispatch.activePhase, "succeeded", dispatch.tracker.exitCode, nil)
	dispatch.dispatchSucceeded = true

	exitCode := 0
	logBytes := fileSize(dispatch.logPath)
	return Result{
		OK:          true,
		Issue:       dispatch.opts.IssueNumber,
		Branch:      dispatch.opts.Branch,
		RunID:       dispatch.opts.RunID,
		PR:          prURL,
		Summary:     summary,
		AttemptPath: dispatch.attemptPath,
		Status:      "succeeded",
		ExitCode:    exitCode,
		LogBytes:    logBytes,
		Report:      &reportRecord,
	}, nil
}

func writeRecovery(ctx context.Context, dispatch *dispatchContext, failure error) {
	if dispatch == nil || dispatch.tracker == nil || failure == nil {
		return
	}
	selectArtifactPreservation(dispatch, dispatch.failureStatus+" attempt", nil)
	failurePhase := dispatch.activePhase
	if failurePhase == "" {
		failurePhase = dispatch.tracker.phase
	}
	if failurePhase == "" {
		failurePhase = "worktree_created"
	}
	errText := failure.Error()
	dispatch.tracker.transition(failurePhase, dispatch.failureStatus, dispatch.tracker.exitCode, &errText)
	var preservationErrors []string
	if briefErr := writeRecoveryBrief(ctx, recoveryBriefOptions{
		repoPath:     dispatch.repoPath,
		runID:        dispatch.opts.RunID,
		jobID:        dispatch.jobID,
		issueNumber:  dispatch.opts.IssueNumber,
		issueTitle:   dispatch.opts.IssueTitle,
		branch:       dispatch.opts.Branch,
		worktreePath: dispatch.worktreePath,
		logPath:      dispatch.logPath,
		summaryPath:  dispatch.summaryPath,
		attempt:      dispatch.opts.Attempt,
		lastPhase:    failurePhase,
		status:       dispatch.failureStatus,
		errorMessage: errText,
		git:          dispatch.deps.Git,
		github:       dispatch.github,
		warnings:     dispatch.warnings,
	}); briefErr != nil {
		msg := fmt.Sprintf("failed to write recovery brief for %s: %v", dispatch.jobID, briefErr)
		preservationErrors = append(preservationErrors, msg)
		fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: %s\n", msg)
	}
	preserveAttemptArtifacts(dispatch, dispatch.failureStatus+" attempt", preservationErrors)
}

func cleanup(ctx context.Context, dispatch *dispatchContext, failure error) {
	if dispatch == nil || dispatch.tracker == nil {
		return
	}
	defer closeWorkerOwnershipStore(dispatch)
	if dispatch.progressRecorder != nil {
		defer dispatch.progressRecorder.Stop()
	}
	if dispatch.dispatchSucceeded {
		if dispatch.opts.KeepWorktree || dispatch.preserveArtifacts {
			if !dispatch.preserveArtifacts {
				selectArtifactPreservation(dispatch, "keep-worktree requested", nil)
			}
			dispatch.tracker.transition("cleanup", dispatch.cleanupStatus, dispatch.tracker.exitCode, nil)
			reason := dispatch.preserveReason
			if reason == "" {
				reason = "keep-worktree requested"
			}
			preserveAttemptArtifacts(dispatch, reason, nil)
			releaseWorkerOwnership(dispatch)
			return
		}
		if err := validateWorkerOwnership(ctx, dispatch); err != nil {
			fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: refused cleanup without active ownership fence for %s: %v\n", dispatch.worktreePath, err)
			selectArtifactPreservation(dispatch, "cleanup ownership refused", []string{err.Error()})
			preserveAttemptArtifacts(dispatch, "cleanup ownership refused", []string{err.Error()})
			return
		}
		selectArtifactCleanup(dispatch, "successful attempt cleanup")
		dispatch.tracker.transition("cleanup", dispatch.cleanupStatus, dispatch.tracker.exitCode, nil)
		var cleanupErrors []string
		if cleanupErr := dispatch.deps.Git.WorktreeRemove(context.Background(), dispatch.repoPath, dispatch.worktreePath); cleanupErr != nil {
			fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: failed to remove worktree %s: %v\n", dispatch.worktreePath, cleanupErr)
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("remove worktree %s: %v", dispatch.worktreePath, cleanupErr))
		}
		if cleanupErr := dispatch.deps.Git.BranchDelete(context.Background(), dispatch.repoPath, dispatch.opts.Branch); cleanupErr != nil {
			fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: failed to delete local branch %s: %v\n", dispatch.opts.Branch, cleanupErr)
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("delete local branch %s: %v", dispatch.opts.Branch, cleanupErr))
		}
		if cleanupErr := removeOwnedScratch(dispatch); cleanupErr != nil {
			fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: failed to remove scratch directory %s: %v\n", dispatch.scratch, cleanupErr)
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("remove scratch %s: %v", dispatch.scratch, cleanupErr))
		}
		completeArtifactCleanup(dispatch, cleanupErrors)
		releaseWorkerOwnership(dispatch)
		return
	}
	if failure == nil {
		releaseWorkerOwnership(dispatch)
		return
	}
	if dispatch.failureStatus == state.StatusFailed {
		dispatch.failureStatus = state.FailureStatus(failure)
	}
	writeRecovery(ctx, dispatch, failure)
	releaseWorkerOwnership(dispatch)
}

func hungResult(dispatch *dispatchContext, agentResult agent.Result) Result {
	reason := workerHungError(dispatch.opts.Provider, agentResult.HungReason, dispatch.logPath)
	return Result{
		OK:          false,
		Issue:       dispatch.opts.IssueNumber,
		Branch:      dispatch.opts.Branch,
		RunID:       dispatch.opts.RunID,
		AttemptPath: dispatch.attemptPath,
		Status:      "hung",
		ExitCode:    agentResult.ExitCode,
		LogBytes:    fileSize(dispatch.logPath),
		Reason:      reason,
		NextAction:  "inspect the hung worker log and recover or retry before continuing",
	}
}

type PromptOptions struct {
	IssueNumber     int
	IssueTitle      string
	IssueBody       string
	Branch          string
	ProviderKey     string
	RecoveryContext string
	RepoSkills      string
}

func BuildPrompt(opts PromptOptions) string {
	prompt := fmt.Sprintf(`You are implementing GitHub issue #%d. The current working directory is a fresh git worktree on branch %s.

# Title
%s

# Details
%s

# Rules
- Implement the change so the issue is satisfied. Keep it minimal and follow existing conventions in the repo.
- You may read files and run commands, but do NOT run git commit or git push — the harness commits and opens the PR.
- When finished, print a 2-4 sentence final summary in English describing exactly what you changed.
`, opts.IssueNumber, opts.Branch, opts.IssueTitle, opts.IssueBody)

	if strings.TrimSpace(opts.ProviderKey) != "" {
		prompt += fmt.Sprintf(`
Provider idempotency key: %s
`, strings.TrimSpace(opts.ProviderKey))
	}

	if strings.TrimSpace(opts.RepoSkills) != "" {
		prompt += fmt.Sprintf(`

%s
`, opts.RepoSkills)
	}

	if strings.TrimSpace(opts.RecoveryContext) != "" {
		prompt += fmt.Sprintf(`

## Recovery context from a prior failed attempt (reuse what is valid, fix what failed)

%s
`, opts.RecoveryContext)
	}
	return prompt
}

func buildWorkerReport(opts Options, result agent.Result) reporter.Report {
	return reporter.Report{
		WorkID:      opts.RunID,
		Issue:       opts.IssueNumber,
		Branch:      opts.Branch,
		Round:       opts.Attempt,
		Role:        reporter.RoleWorker,
		Provider:    opts.Provider,
		Model:       firstNonEmpty(opts.Model, result.Model),
		ModelSource: reporter.ModelSourceForProvider(opts.Provider),
		Effort:      firstNonEmpty(opts.Effort, result.Effort),
		Permission:  reporter.PermissionWrite,
		Action:      providerAttributedAction(fmt.Sprintf("implement issue #%d", opts.IssueNumber), opts.RunID, result),
		ExitCode:    result.ExitCode,
		StartedAt:   result.StartedAt,
		EndedAt:     result.EndedAt,
		DurationMS:  result.DurationMS,
		Usage:       result.Usage,
		Verified:    true,
	}
}

func providerAttributedAction(action, attempt string, result agent.Result) string {
	var parts []string
	if strings.TrimSpace(result.AdapterVersion) != "" {
		parts = append(parts, "adapter="+strings.TrimSpace(result.AdapterVersion))
	}
	if strings.TrimSpace(result.ExternalSessionRef) == "" && len(parts) == 0 {
		return action
	}
	if strings.TrimSpace(attempt) != "" {
		parts = append(parts, "attempt="+strings.TrimSpace(attempt))
	}
	if strings.TrimSpace(result.ExternalSessionRef) != "" {
		parts = append(parts, "session="+strings.TrimSpace(result.ExternalSessionRef))
	}
	if len(parts) == 0 {
		return action
	}
	return action + " [" + strings.Join(parts, " ") + "]"
}

func buildCommitMessage(title string, issueNumber int) string {
	return fmt.Sprintf("%s (closes #%d)", title, issueNumber)
}

func buildPRBody(issueNumber int, summary string) string {
	return fmt.Sprintf("Closes #%d\n\n%s", issueNumber, summary)
}

type scratchOwnerRecord struct {
	Version     int    `json:"version"`
	RunID       string `json:"run_id"`
	JobID       string `json:"job_id"`
	Issue       int    `json:"issue"`
	Attempt     int    `json:"attempt"`
	Branch      string `json:"branch"`
	Generation  int    `json:"generation"`
	OwnerID     string `json:"owner_id"`
	ScratchRoot string `json:"scratch_root,omitempty"`
	Scratch     string `json:"scratch"`
}

type preservationManifest struct {
	Version              int      `json:"version"`
	RunID                string   `json:"run_id"`
	JobID                string   `json:"job_id"`
	Issue                int      `json:"issue"`
	Attempt              int      `json:"attempt"`
	Branch               string   `json:"branch"`
	Status               string   `json:"status"`
	Reason               string   `json:"reason"`
	WorktreePath         string   `json:"worktree_path"`
	ScratchPath          string   `json:"scratch_path"`
	LogPath              string   `json:"log_path"`
	PromptPath           string   `json:"prompt_path"`
	SummaryPath          string   `json:"summary_path"`
	AttemptPath          string   `json:"attempt_path"`
	RecoveryContextPath  string   `json:"recovery_context_path"`
	ManifestPath         string   `json:"manifest_path"`
	PartialArtifactPaths []string `json:"partial_artifact_paths,omitempty"`
	DisposalGuidance     string   `json:"disposal_guidance"`
	PreservationErrors   []string `json:"preservation_errors,omitempty"`
	PreservedAt          string   `json:"preserved_at"`
}

func writeScratchOwner(dispatch *dispatchContext) error {
	record := scratchOwnerRecord{
		Version:    1,
		RunID:      dispatch.opts.RunID,
		JobID:      dispatch.jobID,
		Issue:      dispatch.opts.IssueNumber,
		Attempt:    dispatch.opts.Attempt,
		Branch:     dispatch.opts.Branch,
		Generation: 1,
		OwnerID:    workerOwnershipOwnerID(dispatch),
		Scratch:    dispatch.scratch,
	}
	if dispatch.runtimeRoots.Registered {
		record.ScratchRoot = dispatch.runtimeRoots.TmpRoot
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal scratch owner marker: %w", err)
	}
	path := filepath.Join(dispatch.scratch, scratchOwnerFile)
	if err := dispatch.deps.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write scratch owner marker: %w", err)
	}
	return nil
}

func removeOwnedScratch(dispatch *dispatchContext) error {
	if dispatch == nil || strings.TrimSpace(dispatch.scratch) == "" {
		return errors.New("scratch path is empty")
	}
	if err := validateScratchCleanupDecision(dispatch); err != nil {
		return err
	}
	path := filepath.Join(dispatch.scratch, scratchOwnerFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read scratch owner marker: %w", err)
	}
	var record scratchOwnerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("parse scratch owner marker: %w", err)
	}
	recordScratch, err := pathid.Identity(record.Scratch)
	if err != nil {
		return fmt.Errorf("resolve scratch owner marker identity: %w", err)
	}
	dispatchScratch, err := pathid.Identity(dispatch.scratch)
	if err != nil {
		return fmt.Errorf("resolve scratch identity: %w", err)
	}
	repoIdentity, err := pathid.Identity(dispatch.repoPath)
	if err != nil {
		return fmt.Errorf("resolve repository identity: %w", err)
	}
	if dispatchScratch == repoIdentity {
		return fmt.Errorf("refusing to remove repository root as scratch directory")
	}
	if sameOrDescendantPhysicalPath(dispatchScratch, repoIdentity) {
		return fmt.Errorf("refusing to remove repository ancestor as scratch directory")
	}
	if sameOrDescendantPhysicalPath(repoIdentity, dispatchScratch) {
		return fmt.Errorf("refusing to remove repository descendant as scratch directory")
	}
	if dispatch.runtimeRoots.Registered {
		tmpRoot, err := pathid.Identity(dispatch.runtimeRoots.TmpRoot)
		if err != nil {
			return fmt.Errorf("resolve registered temp root identity: %w", err)
		}
		if !sameOrDescendantPhysicalPath(tmpRoot, dispatchScratch) || tmpRoot == dispatchScratch {
			return fmt.Errorf("scratch path is outside registered temp root")
		}
		if strings.TrimSpace(record.ScratchRoot) == "" {
			return fmt.Errorf("scratch owner marker is missing registered scratch root")
		}
		recordScratchRoot, err := pathid.Identity(record.ScratchRoot)
		if err != nil {
			return fmt.Errorf("resolve scratch owner marker root identity: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(recordScratchRoot), []byte(tmpRoot)) != 1 {
			return fmt.Errorf("scratch owner marker root does not match registered temp root")
		}
	}
	if record.Version != 1 ||
		record.Issue != dispatch.opts.IssueNumber ||
		record.Attempt != dispatch.opts.Attempt ||
		record.Generation != 1 ||
		subtle.ConstantTimeCompare([]byte(record.RunID), []byte(dispatch.opts.RunID)) != 1 ||
		subtle.ConstantTimeCompare([]byte(record.JobID), []byte(dispatch.jobID)) != 1 ||
		subtle.ConstantTimeCompare([]byte(record.OwnerID), []byte(workerOwnershipOwnerID(dispatch))) != 1 ||
		subtle.ConstantTimeCompare([]byte(recordScratch), []byte(dispatchScratch)) != 1 {
		return fmt.Errorf("scratch owner marker does not match attempt %s/%s", dispatch.opts.RunID, dispatch.jobID)
	}
	return dispatch.deps.RemoveAll(dispatch.scratch)
}

func validateScratchCleanupDecision(dispatch *dispatchContext) error {
	if dispatch == nil || dispatch.tracker == nil || dispatch.tracker.artifactDecision == nil {
		return nil
	}
	decision := dispatch.tracker.artifactDecision
	switch decision.State {
	case artifactDecisionCleanupSelected, artifactDecisionCleanupPartial, artifactDecisionCleanupCompleted:
	case artifactDecisionPreserveSelected:
		return fmt.Errorf("refusing scratch cleanup because artifact preservation is selected for %s/%s", dispatch.opts.RunID, dispatch.jobID)
	default:
		return fmt.Errorf("refusing scratch cleanup with ambiguous artifact decision state %q", decision.State)
	}
	if decision.Generation != 1 ||
		subtle.ConstantTimeCompare([]byte(decision.OwnerID), []byte(workerOwnershipOwnerID(dispatch))) != 1 ||
		subtle.ConstantTimeCompare([]byte(decision.ScratchPath), []byte(dispatch.scratch)) != 1 {
		return fmt.Errorf("artifact cleanup decision does not match attempt %s/%s", dispatch.opts.RunID, dispatch.jobID)
	}
	if dispatch.runtimeRoots.Registered {
		if strings.TrimSpace(decision.ScratchRoot) == "" {
			return fmt.Errorf("artifact cleanup decision is missing registered scratch root")
		}
		decisionRoot, err := pathid.Identity(decision.ScratchRoot)
		if err != nil {
			return fmt.Errorf("resolve artifact cleanup decision scratch root: %w", err)
		}
		tmpRoot, err := pathid.Identity(dispatch.runtimeRoots.TmpRoot)
		if err != nil {
			return fmt.Errorf("resolve registered temp root identity: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(decisionRoot), []byte(tmpRoot)) != 1 {
			return fmt.Errorf("artifact cleanup decision scratch root does not match registered temp root")
		}
	}
	return nil
}

func preserveAttemptArtifacts(dispatch *dispatchContext, reason string, preservationErrors []string) {
	if dispatch == nil {
		return
	}
	selectArtifactPreservation(dispatch, reason, preservationErrors)
	decision := dispatch.tracker.artifactDecision
	if decision == nil {
		return
	}
	manifest := preservationManifest{
		Version:              1,
		RunID:                dispatch.opts.RunID,
		JobID:                dispatch.jobID,
		Issue:                dispatch.opts.IssueNumber,
		Attempt:              dispatch.opts.Attempt,
		Branch:               dispatch.tracker.branch,
		Status:               dispatch.tracker.status,
		Reason:               reason,
		WorktreePath:         decision.WorktreePath,
		ScratchPath:          decision.ScratchPath,
		LogPath:              decision.LogPath,
		PromptPath:           decision.PromptPath,
		SummaryPath:          decision.SummaryPath,
		AttemptPath:          decision.AttemptPath,
		RecoveryContextPath:  decision.RecoveryContextPath,
		ManifestPath:         decision.ManifestPath,
		PartialArtifactPaths: append([]string(nil), decision.PartialArtifactPaths...),
		DisposalGuidance:     "Inspect the preserved worktree and recovery brief before deleting anything. Dispose only with an ownership check that matches this run_id and job_id, generation, owner_id, scratch root, and scratch owner marker.",
		PreservationErrors:   append([]string(nil), decision.PreservationErrors...),
		PreservedAt:          state.FormatTimestamp(dispatch.deps.Now()),
	}
	if manifest.Branch == "" {
		manifest.Branch = dispatch.opts.Branch
	}
	if manifest.Status == "" {
		manifest.Status = dispatch.cleanupStatus
	}
	manifestPath := state.PreservationManifestPath(dispatch.repoPath, dispatch.opts.RunID, dispatch.jobID)
	manifest.ManifestPath = manifestPath
	if err := writePreservationManifest(dispatch, manifestPath, manifest); err != nil {
		fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: failed to write preservation manifest %s: %v\n", manifestPath, err)
		manifest.PreservationErrors = append(manifest.PreservationErrors, err.Error())
		manifest.ManifestPath = ""
		updateArtifactDecisionErrors(dispatch, manifest.PreservationErrors)
	} else {
		updateArtifactDecisionManifest(dispatch, manifestPath)
	}
	printPreservationManifest(dispatch.warnings, manifest)
}

func writePreservationManifest(dispatch *dispatchContext, path string, manifest preservationManifest) error {
	if err := dispatch.deps.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create preservation directory: %w", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal preservation manifest: %w", err)
	}
	data = append(data, '\n')
	if err := dispatch.deps.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write preservation manifest: %w", err)
	}
	return nil
}

func printPreservationManifest(w io.Writer, manifest preservationManifest) {
	if w == nil {
		return
	}
	label := strings.TrimSpace(manifest.Reason)
	if !strings.HasSuffix(label, "attempt") {
		label = strings.TrimSpace(manifest.Status) + " attempt"
	}
	if strings.TrimSpace(label) == "" {
		label = "attempt"
	}
	fmt.Fprintf(w, "[loopcoder] preserved %s artifacts for %s/%s (%s)\n", label, manifest.RunID, manifest.JobID, manifest.Reason)
	for _, line := range []struct {
		label string
		value string
	}{
		{"worktree", manifest.WorktreePath},
		{"scratch", manifest.ScratchPath},
		{"log", manifest.LogPath},
		{"prompt", manifest.PromptPath},
		{"summary", manifest.SummaryPath},
		{"attempt", manifest.AttemptPath},
		{"recovery", manifest.RecoveryContextPath},
		{"manifest", manifest.ManifestPath},
	} {
		if strings.TrimSpace(line.value) != "" {
			fmt.Fprintf(w, "[loopcoder] preserved %s: %s\n", line.label, line.value)
		}
	}
	if len(manifest.PreservationErrors) > 0 {
		fmt.Fprintf(w, "[loopcoder] preservation incomplete: %s\n", strings.Join(manifest.PreservationErrors, "; "))
	}
	if len(manifest.PartialArtifactPaths) > 0 {
		fmt.Fprintf(w, "[loopcoder] preserved partial artifacts: %s\n", strings.Join(manifest.PartialArtifactPaths, ", "))
	}
	fmt.Fprintf(w, "[loopcoder] disposal: %s\n", manifest.DisposalGuidance)
}

const (
	artifactDecisionPreserveSelected = "preserve-selected"
	artifactDecisionCleanupSelected  = "cleanup-selected"
	artifactDecisionCleanupCompleted = "cleanup-completed"
	artifactDecisionCleanupPartial   = "cleanup-partial"
)

func selectArtifactPreservation(dispatch *dispatchContext, reason string, preservationErrors []string) {
	if dispatch == nil || dispatch.tracker == nil {
		return
	}
	dispatch.preserveArtifacts = true
	dispatch.preserveReason = firstNonEmpty(reason, dispatch.preserveReason)
	existing := cloneArtifactDecision(dispatch.tracker.artifactDecision)
	alreadySelected := existing != nil && existing.State == artifactDecisionPreserveSelected
	decision := buildArtifactDecision(dispatch, artifactDecisionPreserveSelected, dispatch.preserveReason)
	if alreadySelected {
		decision.DecidedAt = existing.DecidedAt
		decision.Generation = existing.Generation
		decision.ManifestPath = existing.ManifestPath
	}
	if existing != nil {
		decision.PreservationErrors = append(decision.PreservationErrors, existing.PreservationErrors...)
	}
	decision.PreservationErrors = sortedUniqueNonEmpty(append(decision.PreservationErrors, preservationErrors...))
	dispatch.tracker.setArtifactDecision(decision)
	dispatch.tracker.writeAttempt()
	if !alreadySelected {
		dispatch.tracker.appendEvent("artifact_preservation_selected", artifactDecisionPreserveSelected, artifactDecisionEventDetails(decision))
	}
}

func selectArtifactCleanup(dispatch *dispatchContext, reason string) {
	if dispatch == nil || dispatch.tracker == nil {
		return
	}
	decision := buildArtifactDecision(dispatch, artifactDecisionCleanupSelected, reason)
	dispatch.tracker.setArtifactDecision(decision)
	dispatch.tracker.writeAttempt()
	dispatch.tracker.appendEvent("artifact_cleanup_selected", artifactDecisionCleanupSelected, artifactDecisionEventDetails(decision))
}

func completeArtifactCleanup(dispatch *dispatchContext, cleanupErrors []string) {
	if dispatch == nil || dispatch.tracker == nil || dispatch.tracker.artifactDecision == nil {
		return
	}
	decision := cloneArtifactDecision(dispatch.tracker.artifactDecision)
	decision.UpdatedAt = state.FormatTimestamp(dispatch.deps.Now())
	decision.CleanupErrors = append([]string(nil), cleanupErrors...)
	if len(cleanupErrors) > 0 {
		decision.State = artifactDecisionCleanupPartial
	} else {
		decision.State = artifactDecisionCleanupCompleted
	}
	dispatch.tracker.setArtifactDecision(*decision)
	dispatch.tracker.writeAttempt()
	dispatch.tracker.appendEvent("artifact_cleanup_completed", decision.State, artifactDecisionEventDetails(*decision))
}

func updateArtifactDecisionManifest(dispatch *dispatchContext, manifestPath string) {
	if dispatch == nil || dispatch.tracker == nil || dispatch.tracker.artifactDecision == nil {
		return
	}
	decision := cloneArtifactDecision(dispatch.tracker.artifactDecision)
	if existing, ok := existingArtifactPath(dispatch, "manifest", manifestPath); ok {
		decision.ManifestPath = existing
		decision.PartialArtifactPaths = sortedUniqueNonEmpty(append(decision.PartialArtifactPaths, existing))
	}
	decision.UpdatedAt = state.FormatTimestamp(dispatch.deps.Now())
	dispatch.tracker.setArtifactDecision(*decision)
	dispatch.tracker.writeAttempt()
}

func updateArtifactDecisionErrors(dispatch *dispatchContext, preservationErrors []string) {
	if dispatch == nil || dispatch.tracker == nil || dispatch.tracker.artifactDecision == nil {
		return
	}
	decision := cloneArtifactDecision(dispatch.tracker.artifactDecision)
	decision.PreservationErrors = sortedUniqueNonEmpty(append(decision.PreservationErrors, preservationErrors...))
	decision.UpdatedAt = state.FormatTimestamp(dispatch.deps.Now())
	dispatch.tracker.setArtifactDecision(*decision)
	dispatch.tracker.writeAttempt()
}

func buildArtifactDecision(dispatch *dispatchContext, stateValue, reason string) state.ArtifactDecision {
	now := state.FormatTimestamp(dispatch.deps.Now())
	decision := state.ArtifactDecision{
		State:      stateValue,
		Reason:     strings.TrimSpace(reason),
		Generation: 1,
		OwnerID:    workerOwnershipOwnerID(dispatch),
		DecidedAt:  now,
		UpdatedAt:  now,
	}
	if dispatch.runtimeRoots.Registered {
		decision.ScratchRoot = dispatch.runtimeRoots.TmpRoot
	}
	type pathField struct {
		label string
		value string
		set   func(string)
	}
	for _, field := range []pathField{
		{label: "worktree", value: dispatch.worktreePath, set: func(v string) { decision.WorktreePath = v }},
		{label: "scratch", value: dispatch.scratch, set: func(v string) { decision.ScratchPath = v }},
		{label: "log", value: dispatch.logPath, set: func(v string) { decision.LogPath = v }},
		{label: "prompt", value: dispatch.promptPath, set: func(v string) { decision.PromptPath = v }},
		{label: "summary", value: dispatch.summaryPath, set: func(v string) { decision.SummaryPath = v }},
		{label: "attempt", value: dispatch.attemptPath, set: func(v string) { decision.AttemptPath = v }},
		{label: "recovery", value: state.RecoveryBriefPath(dispatch.repoPath, dispatch.opts.RunID, dispatch.jobID), set: func(v string) { decision.RecoveryContextPath = v }},
	} {
		existing, ok := existingArtifactPath(dispatch, field.label, field.value)
		if ok {
			field.set(existing)
			decision.PartialArtifactPaths = append(decision.PartialArtifactPaths, existing)
			continue
		}
		if stateValue == artifactDecisionPreserveSelected && strings.TrimSpace(field.value) != "" {
			decision.PreservationErrors = append(decision.PreservationErrors, fmt.Sprintf("%s path is not preserved because it is absent: %s", field.label, field.value))
		}
	}
	decision.PartialArtifactPaths = sortedUniqueNonEmpty(decision.PartialArtifactPaths)
	decision.PreservationErrors = sortedUniqueNonEmpty(decision.PreservationErrors)
	return decision
}

func existingArtifactPath(dispatch *dispatchContext, _, path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	info, err := dispatch.deps.Stat(path)
	if err != nil || info == nil {
		return "", false
	}
	return path, true
}

func artifactDecisionEventDetails(decision state.ArtifactDecision) map[string]any {
	return map[string]any{
		"state":                  decision.State,
		"reason":                 decision.Reason,
		"generation":             decision.Generation,
		"owner_id":               decision.OwnerID,
		"scratch_root":           decision.ScratchRoot,
		"partial_artifact_paths": append([]string(nil), decision.PartialArtifactPaths...),
	}
}

type hungHarvestOptions struct {
	repoPath          string
	runID             string
	jobID             string
	worktreePath      string
	logPath           string
	summaryPath       string
	opts              Options
	agentResult       agent.Result
	errorMessage      string
	git               GitClient
	github            GitHubClient
	now               func() time.Time
	warnings          io.Writer
	validateOwnership func(context.Context) error
}

type hungHarvestResult struct {
	Branch  string
	PR      string
	Summary string
	Phase   string
	Mode    string
	Report  reporter.Report
}

func harvestHungWorktree(ctx context.Context, opts hungHarvestOptions) (*hungHarvestResult, error) {
	if err := validateHarvestOwnership(ctx, opts); err != nil {
		return nil, err
	}
	dirty, err := opts.git.StatusPorcelain(ctx, opts.worktreePath)
	if err != nil {
		return nil, fmt.Errorf("git status --porcelain before harvest: %w", err)
	}
	if strings.TrimSpace(dirty) == "" {
		return nil, nil
	}

	harvestBranch := harvestBranchName(opts.opts.IssueNumber, opts.opts.Attempt)
	briefText := ""
	briefPath := state.RecoveryBriefPath(opts.repoPath, opts.runID, opts.jobID)
	if err := writeRecoveryBrief(ctx, recoveryBriefOptions{
		repoPath:     opts.repoPath,
		runID:        opts.runID,
		jobID:        opts.jobID,
		issueNumber:  opts.opts.IssueNumber,
		issueTitle:   opts.opts.IssueTitle,
		branch:       harvestBranch,
		worktreePath: opts.worktreePath,
		logPath:      opts.logPath,
		summaryPath:  opts.summaryPath,
		attempt:      opts.opts.Attempt,
		lastPhase:    "codex_exited",
		status:       "hung",
		errorMessage: opts.errorMessage,
		git:          opts.git,
		github:       opts.github,
		warnings:     opts.warnings,
	}); err != nil {
		fmt.Fprintf(opts.warnings, "[loopcoder] warning: failed to write harvest recovery brief for %s: %v\n", opts.jobID, err)
	} else if data, readErr := os.ReadFile(briefPath); readErr != nil {
		fmt.Fprintf(opts.warnings, "[loopcoder] warning: failed to read harvest recovery brief %s: %v\n", briefPath, readErr)
	} else {
		briefText = string(data)
	}

	started := opts.now()
	existing := findOpenHarvestPR(ctx, opts.github, opts.opts.IssueNumber, harvestBranch, opts.warnings)
	ended := opts.now()
	if existing != nil {
		record := buildHarvestConductorReport(opts.opts, firstNonEmpty(existing.HeadRefName, harvestBranch), opts.worktreePath, started, ended)
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("validate harvest conductor report: %w", err)
		}
		return &hungHarvestResult{
			Branch:  firstNonEmpty(existing.HeadRefName, harvestBranch),
			PR:      existing.URL,
			Summary: "harvested from hung/killed worker - existing needs-human PR already open",
			Phase:   "harvest_adopted",
			Mode:    "existing-pr",
			Report:  record,
		}, nil
	}

	if err := validateHarvestOwnership(ctx, opts); err != nil {
		return nil, err
	}
	if err := opts.git.AddAll(ctx, opts.worktreePath); err != nil {
		return nil, fmt.Errorf("git add -A for harvest: %w", err)
	}
	if err := validateHarvestOwnership(ctx, opts); err != nil {
		return nil, err
	}
	if err := opts.git.Commit(ctx, opts.worktreePath, buildHarvestCommitMessage(opts.opts.IssueTitle, opts.opts.IssueNumber)); err != nil {
		return nil, fmt.Errorf("git commit harvest: %w", err)
	}
	if err := validateHarvestOwnership(ctx, opts); err != nil {
		return nil, err
	}
	if err := opts.git.PushUpstreamForceWithLease(ctx, opts.worktreePath, harvestBranch); err != nil {
		return nil, fmt.Errorf("git push --force-with-lease origin %s: %w", harvestBranch, err)
	}

	ended = opts.now()
	record := buildHarvestConductorReport(opts.opts, harvestBranch, opts.worktreePath, started, ended)
	if err := record.Validate(); err != nil {
		return nil, fmt.Errorf("validate harvest conductor report: %w", err)
	}
	body := buildHarvestPRBody(opts.opts, opts.agentResult, briefText)
	if err := validateHarvestOwnership(ctx, opts); err != nil {
		return nil, err
	}
	prURL, err := opts.github.CreatePR(ctx, harvestBranch, opts.opts.BaseBranch, buildHarvestPRTitle(opts.opts.IssueTitle, opts.opts.IssueNumber), body)
	if err != nil {
		return nil, fmt.Errorf("gh pr create harvest: %w", err)
	}
	return &hungHarvestResult{
		Branch:  harvestBranch,
		PR:      prURL,
		Summary: "harvested from hung/killed worker - possibly incomplete; needs human review",
		Phase:   "harvest_pr_opened",
		Mode:    "created-pr",
		Report:  record,
	}, nil
}

func validateHarvestOwnership(ctx context.Context, opts hungHarvestOptions) error {
	if opts.validateOwnership == nil {
		return nil
	}
	return opts.validateOwnership(ctx)
}

func harvestBranchName(issueNumber, attempt int) string {
	if attempt <= 0 {
		attempt = 1
	}
	return fmt.Sprintf("loop/issue-%d-retry-%d", issueNumber, attempt)
}

func buildHarvestCommitMessage(title string, issueNumber int) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Sprintf("Harvest issue #%d from hung worker", issueNumber)
	}
	return fmt.Sprintf("Harvest issue #%d from hung worker: %s", issueNumber, title)
}

func buildHarvestPRTitle(title string, issueNumber int) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Sprintf("[needs-human] Harvested issue #%d from hung/killed worker", issueNumber)
	}
	return fmt.Sprintf("[needs-human] Harvested issue #%d from hung/killed worker: %s", issueNumber, title)
}

func buildHarvestPRBody(opts Options, result agent.Result, recoveryBrief string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Refs #%d\n\n", opts.IssueNumber)
	fmt.Fprintln(&out, "This needs-human PR was harvested from a hung/killed worker and is possibly incomplete. Human review is required before any merge.")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "## Recovery brief")
	if strings.TrimSpace(recoveryBrief) == "" {
		fmt.Fprintln(&out, "(recovery brief unavailable)")
	} else {
		fmt.Fprintln(&out, strings.TrimRight(recoveryBrief, "\r\n"))
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "## Hung worker partial report")
	fmt.Fprintln(&out, "```text")
	fmt.Fprintln(&out, formatHungWorkerPartialReport(opts, result))
	fmt.Fprintln(&out, "```")
	return out.String()
}

func formatHungWorkerPartialReport(opts Options, result agent.Result) string {
	lines := []string{
		"role=worker",
		"provider=" + firstNonEmpty(opts.Provider, "unknown"),
		"model=" + firstNonEmpty(result.Model, opts.Model, "unknown"),
		"model_source=parsed",
		"effort=" + firstNonEmpty(result.Effort, opts.Effort, "unknown"),
		"permission=write",
		fmt.Sprintf("action=implement issue #%d", opts.IssueNumber),
		fmt.Sprintf("exit_code=%d", result.ExitCode),
		"hung=true",
		"hung_reason=" + firstNonEmpty(result.HungReason, "unknown"),
		"verified=false",
	}
	if result.StartedAt != "" {
		lines = append(lines, "started_at="+result.StartedAt)
	}
	if result.EndedAt != "" {
		lines = append(lines, "ended_at="+result.EndedAt)
	}
	if result.DurationMS > 0 {
		lines = append(lines, fmt.Sprintf("duration_ms=%d", result.DurationMS))
	}
	if result.Usage.TotalTokens != nil {
		lines = append(lines, fmt.Sprintf("total_tokens=%d", *result.Usage.TotalTokens))
	}
	if result.Usage.InputTokens != nil {
		lines = append(lines, fmt.Sprintf("input_tokens=%d", *result.Usage.InputTokens))
	}
	if result.Usage.OutputTokens != nil {
		lines = append(lines, fmt.Sprintf("output_tokens=%d", *result.Usage.OutputTokens))
	}
	return recovery.Scrub(strings.Join(lines, "\n"))
}

func buildHarvestConductorReport(opts Options, branch, worktreePath string, started, ended time.Time) reporter.Report {
	if started.IsZero() {
		started = time.Now().UTC()
	}
	if ended.IsZero() {
		ended = started
	}
	if ended.Before(started) {
		ended = started
	}
	totalTokens := int64(0)
	return reporter.Report{
		WorkID:      opts.RunID,
		Issue:       opts.IssueNumber,
		Branch:      firstNonEmpty(branch, opts.Branch),
		Worktree:    worktreePath,
		Round:       opts.Attempt,
		Role:        reporter.RoleConductor,
		Provider:    firstNonEmpty(opts.Provider, "loopcoder"),
		Model:       firstNonEmpty(opts.Model, "loopcoder-harvest"),
		ModelSource: reporter.ModelSourceSelfReported,
		Effort:      opts.Effort,
		Permission:  reporter.PermissionOrchestrate,
		Action:      fmt.Sprintf("harvest hung worker issue #%d", opts.IssueNumber),
		ExitCode:    0,
		StartedAt:   state.FormatTimestamp(started),
		EndedAt:     state.FormatTimestamp(ended),
		DurationMS:  ended.Sub(started).Milliseconds(),
		Usage: reporter.Usage{
			TotalTokens: &totalTokens,
		},
		Verified: false,
	}
}

func findOpenHarvestPR(ctx context.Context, github GitHubClient, issueNumber int, currentHarvestBranch string, warnings io.Writer) *gh.PullRequest {
	if github == nil {
		return nil
	}
	if warnings == nil {
		warnings = io.Discard
	}
	if prs, err := github.ListHeadPRs(ctx, currentHarvestBranch); err == nil && len(prs) > 0 {
		return &gh.PullRequest{
			Number:      prs[0].Number,
			URL:         prs[0].URL,
			HeadRefName: currentHarvestBranch,
		}
	} else if err != nil {
		fmt.Fprintf(warnings, "[loopcoder] warning: harvest idempotency check could not list PRs for head %s: %v; proceeding with harvest, duplicate needs-human PR may result\n", currentHarvestBranch, err)
	}
	openPRs, err := github.ListOpenPRs(ctx)
	if err != nil {
		fmt.Fprintf(warnings, "[loopcoder] warning: harvest idempotency check could not list open PRs for issue #%d: %v; proceeding with harvest, duplicate needs-human PR may result\n", issueNumber, err)
		return nil
	}
	prefix := fmt.Sprintf("loop/issue-%d-retry-", issueNumber)
	for _, pr := range openPRs {
		if strings.HasPrefix(strings.TrimSpace(pr.HeadRefName), prefix) {
			prCopy := pr
			return &prCopy
		}
	}
	return nil
}

func workerHungError(provider, reason, logPath string) string {
	return fmt.Sprintf("%s exec hung (reason=hung hung_reason=%s). See %s", provider, firstNonEmpty(reason, "unknown"), logPath)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

const (
	partialWorkModeHarvestNeedsHuman = "harvest-needs-human"
	partialWorkModeReportOnly        = "report-only"
)

type domainWorkerPolicy struct {
	PartialWorkMode string
	LivenessMode    supervisedexec.LivenessMode
	LivenessCommand string
}

func (p domainWorkerPolicy) AgentLivenessMode() string {
	if p.LivenessMode == "" || p.LivenessMode == supervisedexec.LivenessModeWorktreeMTime {
		return ""
	}
	return string(p.LivenessMode)
}

func resolveDomainWorkerPolicy(cfg config.Config) (domainWorkerPolicy, error) {
	policy := domainWorkerPolicy{
		PartialWorkMode: partialWorkModeHarvestNeedsHuman,
		LivenessMode:    supervisedexec.LivenessModeWorktreeMTime,
	}

	switch mode := strings.ToLower(strings.TrimSpace(cfg.Domain.PartialWork.Mode)); mode {
	case "", partialWorkModeHarvestNeedsHuman:
		policy.PartialWorkMode = partialWorkModeHarvestNeedsHuman
	case partialWorkModeReportOnly:
		policy.PartialWorkMode = partialWorkModeReportOnly
	default:
		return domainWorkerPolicy{}, fmt.Errorf("invalid delivery config: domain.partial_work.mode must be %q or %q, got %q", partialWorkModeHarvestNeedsHuman, partialWorkModeReportOnly, cfg.Domain.PartialWork.Mode)
	}

	switch mode := strings.ToLower(strings.TrimSpace(cfg.Domain.Liveness.Mode)); mode {
	case "", string(supervisedexec.LivenessModeWorktreeMTime):
		if domainLivenessCommandConfigured(cfg.Domain.Liveness) {
			return domainWorkerPolicy{}, errors.New("invalid delivery config: domain.liveness.command and domain.liveness.argv are only valid when domain.liveness.mode is \"custom\"")
		}
		policy.LivenessMode = supervisedexec.LivenessModeWorktreeMTime
	case string(supervisedexec.LivenessModeLogOnly):
		if domainLivenessCommandConfigured(cfg.Domain.Liveness) {
			return domainWorkerPolicy{}, errors.New("invalid delivery config: domain.liveness.command and domain.liveness.argv are only valid when domain.liveness.mode is \"custom\"")
		}
		policy.LivenessMode = supervisedexec.LivenessModeLogOnly
	case string(supervisedexec.LivenessModeCustom):
		command := strings.TrimSpace(cfg.Domain.Liveness.Command)
		hasCommand := command != ""
		hasArgv := len(cfg.Domain.Liveness.Argv) > 0
		if hasCommand && hasArgv {
			return domainWorkerPolicy{}, errors.New("invalid delivery config: domain.liveness.command and domain.liveness.argv are mutually exclusive")
		}
		if !hasCommand && !hasArgv {
			return domainWorkerPolicy{}, errors.New("invalid delivery config: domain.liveness.mode custom requires exactly one of domain.liveness.command or domain.liveness.argv")
		}
		policy.LivenessMode = supervisedexec.LivenessModeCustom
		if hasArgv {
			policy.LivenessCommand = supervisedexec.EncodeLivenessArgv(cfg.Domain.Liveness.Argv)
		} else {
			policy.LivenessCommand = command
		}
	default:
		return domainWorkerPolicy{}, fmt.Errorf("invalid delivery config: domain.liveness.mode must be %q, %q, or %q, got %q", supervisedexec.LivenessModeWorktreeMTime, supervisedexec.LivenessModeLogOnly, supervisedexec.LivenessModeCustom, cfg.Domain.Liveness.Mode)
	}

	return policy, nil
}

func domainLivenessCommandConfigured(liveness config.DomainLiveness) bool {
	return strings.TrimSpace(liveness.Command) != "" || len(liveness.Argv) > 0
}

func MarshalResult(result Result) ([]byte, error) {
	return json.Marshal(result)
}

func withDefaults(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Git == nil {
		deps.Git = defaults.Git
	}
	if deps.GitHub == nil {
		deps.GitHub = defaults.GitHub
	}
	if deps.AgentLookup == nil {
		deps.AgentLookup = defaults.AgentLookup
	}
	if deps.AcquireLock == nil {
		deps.AcquireLock = defaults.AcquireLock
	}
	if deps.Now == nil {
		deps.Now = defaults.Now
	}
	if deps.PID == nil {
		deps.PID = defaults.PID
	}
	if deps.MkdirTemp == nil {
		deps.MkdirTemp = defaults.MkdirTemp
	}
	if deps.MkdirAll == nil {
		deps.MkdirAll = defaults.MkdirAll
	}
	if deps.WriteFile == nil {
		deps.WriteFile = defaults.WriteFile
	}
	if deps.Stat == nil {
		deps.Stat = defaults.Stat
	}
	if deps.RemoveAll == nil {
		deps.RemoveAll = defaults.RemoveAll
	}
	if deps.RepoSkills == nil {
		deps.RepoSkills = defaults.RepoSkills
	}
	if deps.OpenStore == nil {
		deps.OpenStore = defaults.OpenStore
	}
	if deps.OpenProgressStore == nil {
		deps.OpenProgressStore = defaults.OpenProgressStore
	}
	return deps
}

func resolveRepo(repoPath string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		return "", errors.New("repo path is required")
	}
	absolute, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repo path is not a directory: %s", absolute)
	}
	return absolute, nil
}

func addWorktreeWithLock(ctx context.Context, deps Deps, repoPath, branch, worktreePath, baseBranch string) error {
	lock, err := deps.AcquireLock(repoPath, 60*time.Second)
	if err != nil {
		return err
	}
	addErr := deps.Git.WorktreeAdd(ctx, repoPath, branch, worktreePath, baseBranch)
	releaseErr := lock.Release()
	if addErr != nil {
		return fmt.Errorf("git worktree add: %w", addErr)
	}
	if releaseErr != nil {
		return releaseErr
	}
	return nil
}

func acquireWorkerOwnership(ctx context.Context, dispatch *dispatchContext) error {
	if dispatch == nil || !dispatch.runtimeRoots.Registered {
		return nil
	}
	if dispatch.ownershipLease != nil {
		return nil
	}
	store, err := dispatch.deps.OpenStore(ctx, storage.Options{Path: dispatch.runtimeRoots.DatabasePath, Now: dispatch.deps.Now})
	if err != nil {
		return fmt.Errorf("open ownership store: %w", err)
	}
	dispatch.ownershipStore = store
	worktreeIdentity, err := pathid.Identity(dispatch.worktreePath)
	if err != nil {
		closeWorkerOwnershipStore(dispatch)
		return fmt.Errorf("resolve worktree identity: %w", err)
	}
	now := dispatch.deps.Now().UTC()
	leaseDuration := WorkerHardCap
	if dispatch.opts.Timeout > leaseDuration {
		leaseDuration = dispatch.opts.Timeout
	}
	leaseUntil := now.Add(leaseDuration + 30*time.Minute)
	lease, err := storage.AcquireAgentOwnershipLease(ctx, store, storage.AgentOwnershipLeaseRequest{
		ProjectID:     dispatch.runtimeRoots.ProjectID,
		DeliveryRunID: dispatch.opts.RunID,
		RunID:         dispatch.opts.RunID,
		OwnerID:       workerOwnershipOwnerID(dispatch),
		Now:           now,
		LeaseUntil:    leaseUntil,
		Resources: []storage.AgentOwnershipResource{
			{ResourceKind: "repo-path", ResourceKey: "."},
			{ResourceKind: "worktree", ResourceKey: worktreeIdentity},
			{ResourceKind: "git-ref", ResourceKey: dispatch.opts.Branch},
			{ResourceKind: "runtime-run", ResourceKey: dispatch.opts.RunID},
		},
	})
	if err != nil {
		closeWorkerOwnershipStore(dispatch)
		return fmt.Errorf("acquire worker ownership: %w", err)
	}
	dispatch.ownershipLease = &lease
	return nil
}

func validateWorkerOwnership(ctx context.Context, dispatch *dispatchContext) error {
	if dispatch == nil || dispatch.ownershipStore == nil || dispatch.ownershipLease == nil {
		return nil
	}
	if err := storage.ValidateAgentOwnershipFence(ctx, dispatch.ownershipStore, *dispatch.ownershipLease); err != nil {
		if errors.Is(err, storage.ErrOwnershipStale) {
			dispatch.failureStatus = "needs-human"
		}
		return fmt.Errorf("worker ownership fence: %w", err)
	}
	return nil
}

func releaseWorkerOwnership(dispatch *dispatchContext) {
	if dispatch == nil || dispatch.ownershipStore == nil || dispatch.ownershipLease == nil {
		return
	}
	if err := storage.ReleaseAgentOwnershipLease(context.Background(), dispatch.ownershipStore, *dispatch.ownershipLease, dispatch.deps.Now()); err != nil {
		fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: failed to release ownership lease for %s/%s: %v\n", dispatch.opts.RunID, dispatch.jobID, err)
		return
	}
	dispatch.ownershipLease = nil
}

func closeWorkerOwnershipStore(dispatch *dispatchContext) {
	if dispatch == nil || dispatch.ownershipStore == nil {
		return
	}
	if err := dispatch.ownershipStore.Close(); err != nil {
		fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: failed to close ownership store: %v\n", err)
	}
	dispatch.ownershipStore = nil
}

func workerOwnershipOwnerID(dispatch *dispatchContext) string {
	if dispatch == nil {
		return ""
	}
	return fmt.Sprintf("worker:%s:%s:%d", strings.TrimSpace(dispatch.opts.RunID), strings.TrimSpace(dispatch.jobID), dispatch.opts.Attempt)
}

func sameOrDescendantPhysicalPath(root, child string) bool {
	root = filepath.Clean(root)
	child = filepath.Clean(child)
	if root == child {
		return true
	}
	separator := string(filepath.Separator)
	return strings.HasPrefix(child, strings.TrimSuffix(root, separator)+separator)
}

type attemptTrackerOptions struct {
	repoPath    string
	runID       string
	jobID       string
	issue       int
	attempt     int
	provider    string
	pid         int
	branch      string
	logPath     string
	startedAt   time.Time
	now         func() time.Time
	warnings    io.Writer
	attemptPath string
	progress    *progressRecorder
}

type attemptTracker struct {
	repoPath         string
	runID            string
	jobID            string
	issue            int
	attempt          int
	provider         string
	pid              int
	branch           string
	logPath          string
	startedAt        string
	heartbeatAt      string
	lastProgressAt   string
	phase            string
	status           string
	logBytes         int64
	exitCode         *int
	errorMessage     *string
	usage            *reporter.Usage
	reporter         *reporter.Report
	now              func() time.Time
	warnings         io.Writer
	attemptPath      string
	artifactDecision *state.ArtifactDecision
	progress         *progressRecorder
}

func newAttemptTracker(opts attemptTrackerOptions) *attemptTracker {
	started := state.FormatTimestamp(opts.startedAt)
	return &attemptTracker{
		repoPath:       opts.repoPath,
		runID:          opts.runID,
		jobID:          opts.jobID,
		issue:          opts.issue,
		attempt:        opts.attempt,
		provider:       opts.provider,
		pid:            opts.pid,
		branch:         opts.branch,
		logPath:        opts.logPath,
		startedAt:      started,
		heartbeatAt:    started,
		lastProgressAt: started,
		status:         "running",
		now:            opts.now,
		warnings:       opts.warnings,
		attemptPath:    opts.attemptPath,
		progress:       opts.progress,
	}
}

func (t *attemptTracker) setUsage(usage reporter.Usage) {
	t.usage = cloneUsage(&usage)
}

func (t *attemptTracker) setReport(record reporter.Report) {
	t.reporter = cloneReport(&record)
}

func (t *attemptTracker) setArtifactDecision(decision state.ArtifactDecision) {
	t.artifactDecision = cloneArtifactDecision(&decision)
}

func (t *attemptTracker) transition(phase, status string, exitCode *int, errorMessage *string) {
	now := state.FormatTimestamp(t.now())
	currentLogBytes := fileSize(t.logPath)
	phaseAdvanced := strings.TrimSpace(phase) != "" && phase != t.phase
	logAdvanced := currentLogBytes > t.logBytes
	if phaseAdvanced || logAdvanced || t.lastProgressAt == "" {
		t.lastProgressAt = now
	}
	if strings.TrimSpace(phase) != "" {
		t.phase = phase
	}
	if strings.TrimSpace(status) != "" {
		t.status = status
	}
	t.heartbeatAt = now
	t.logBytes = currentLogBytes
	if exitCode != nil {
		value := *exitCode
		t.exitCode = &value
	}
	if errorMessage != nil {
		value := *errorMessage
		t.errorMessage = &value
	}

	t.writeAttempt()

	event := state.Event{
		Timestamp: now,
		RunID:     t.runID,
		JobID:     t.jobID,
		Issue:     t.issue,
		Phase:     t.phase,
		Status:    t.status,
		LogBytes:  t.logBytes,
		ExitCode:  t.exitCode,
		Error:     t.errorMessage,
	}
	if err := state.AppendEvent(t.repoPath, t.runID, event); err != nil {
		fmt.Fprintf(t.warnings, "[loopcoder] warning: failed to append event state %s: %v\n", state.EventsPath(t.repoPath, t.runID), err)
	}
	t.appendLifecycle(now, "")
	if t.progress != nil {
		t.progress.RecordAttempt(t.attemptRecord(), t.progressTerminal())
	}
}

func (t *attemptTracker) appendEvent(eventName, outcome string, details any) {
	now := state.FormatTimestamp(t.now())
	event := state.Event{
		Timestamp: now,
		RunID:     t.runID,
		JobID:     t.jobID,
		Issue:     t.issue,
		Phase:     t.phase,
		Status:    t.status,
		LogBytes:  fileSize(t.logPath),
		ExitCode:  t.exitCode,
		Error:     t.errorMessage,
		Event:     eventName,
		Outcome:   outcome,
		Details:   details,
	}
	if err := state.AppendEvent(t.repoPath, t.runID, event); err != nil {
		fmt.Fprintf(t.warnings, "[loopcoder] warning: failed to append event state %s: %v\n", state.EventsPath(t.repoPath, t.runID), err)
	}
	t.appendLifecycle(now, eventName)
	if t.progress != nil && strings.TrimSpace(eventName) != "" {
		record := t.attemptRecord()
		record.Status = firstNonEmpty(outcome, record.Status)
		t.progress.RecordAttempt(record, false)
	}
}

func (t *attemptTracker) appendLifecycle(timestamp, eventName string) {
	lifecycleState, ok := state.LegacyLifecycleState(t.status, t.phase, t.exitCode)
	if !ok {
		return
	}
	history, err := state.LoadLifecycleHistory(t.repoPath, t.runID)
	if err != nil {
		fmt.Fprintf(t.warnings, "[loopcoder] warning: failed to read lifecycle state %s: %v\n", state.LifecyclePath(t.repoPath, t.runID), err)
		return
	}
	if len(history) > 0 && history[len(history)-1].State == lifecycleState {
		return
	}
	if err := state.AppendLifecycleTransition(t.repoPath, state.LifecycleTransition{
		Timestamp: timestamp,
		RunID:     t.runID,
		State:     lifecycleState,
		Reason:    firstNonEmpty(eventName, "worker attempt transition"),
		Source:    "worker",
		Issue:     t.issue,
		JobID:     t.jobID,
	}); err != nil {
		fmt.Fprintf(t.warnings, "[loopcoder] warning: failed to append lifecycle state %s: %v\n", state.LifecyclePath(t.repoPath, t.runID), err)
	}
}

func (t *attemptTracker) writeAttempt() {
	if _, err := state.WriteAttempt(t.repoPath, t.runID, t.attemptRecord()); err != nil {
		fmt.Fprintf(t.warnings, "[loopcoder] warning: failed to write durable attempt state %s: %v\n", t.attemptPath, err)
	}
}

func (t *attemptTracker) attemptRecord() state.AttemptRecord {
	return state.AttemptRecord{
		Version:          1,
		JobID:            t.jobID,
		Issue:            t.issue,
		Attempt:          t.attempt,
		Provider:         t.provider,
		PID:              t.pid,
		Phase:            t.phase,
		Status:           t.status,
		Branch:           t.branch,
		StartedAt:        t.startedAt,
		HeartbeatAt:      t.heartbeatAt,
		LastProgressAt:   t.lastProgressAt,
		LogBytes:         t.logBytes,
		ExitCode:         t.exitCode,
		Error:            t.errorMessage,
		Usage:            cloneUsage(t.usage),
		Report:           cloneReport(t.reporter),
		ArtifactDecision: cloneArtifactDecision(t.artifactDecision),
	}
}

func (t *attemptTracker) progressTerminal() bool {
	if !state.IsTerminalStatus(t.status) {
		return false
	}
	return t.status != state.StatusSucceeded || t.phase == "cleanup"
}

func cloneReport(record *reporter.Report) *reporter.Report {
	if record == nil {
		return nil
	}
	clone := *record
	clone.Usage = *cloneUsage(&record.Usage)
	return &clone
}

func cloneUsage(usage *reporter.Usage) *reporter.Usage {
	if usage == nil {
		return nil
	}
	clone := *usage
	if usage.InputTokens != nil {
		value := *usage.InputTokens
		clone.InputTokens = &value
	}
	if usage.OutputTokens != nil {
		value := *usage.OutputTokens
		clone.OutputTokens = &value
	}
	if usage.TotalTokens != nil {
		value := *usage.TotalTokens
		clone.TotalTokens = &value
	}
	return &clone
}

func cloneArtifactDecision(decision *state.ArtifactDecision) *state.ArtifactDecision {
	if decision == nil {
		return nil
	}
	clone := *decision
	clone.PartialArtifactPaths = append([]string(nil), decision.PartialArtifactPaths...)
	clone.PreservationErrors = append([]string(nil), decision.PreservationErrors...)
	clone.CleanupErrors = append([]string(nil), decision.CleanupErrors...)
	return &clone
}

func sortedUniqueNonEmpty(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

type recoveryBriefOptions struct {
	repoPath     string
	runID        string
	jobID        string
	issueNumber  int
	issueTitle   string
	branch       string
	worktreePath string
	logPath      string
	summaryPath  string
	attempt      int
	lastPhase    string
	status       string
	errorMessage string
	git          GitClient
	github       GitHubClient
	warnings     io.Writer
}

func writeRecoveryBrief(ctx context.Context, opts recoveryBriefOptions) error {
	briefPath := state.RecoveryBriefPath(opts.repoPath, opts.runID, opts.jobID)
	if err := os.MkdirAll(filepath.Dir(briefPath), 0o755); err != nil {
		return fmt.Errorf("create recovery directory: %w", err)
	}

	changedFiles := "(worktree path does not exist)"
	if info, err := os.Stat(opts.worktreePath); err == nil && info.IsDir() {
		statusText, statusErr := opts.git.StatusPorcelain(ctx, opts.worktreePath)
		if statusErr != nil {
			changedFiles = fmt.Sprintf("(git status failed: %v)", statusErr)
		} else if strings.TrimSpace(statusText) == "" {
			changedFiles = "(none)"
		} else {
			changedFiles = strings.TrimRight(statusText, "\r\n")
		}
	}

	existingPRText := "PR lookup failed or unavailable"
	if opts.github != nil {
		prs, err := opts.github.ListHeadPRs(ctx, opts.branch)
		if err != nil {
			existingPRText = fmt.Sprintf("PR lookup failed: %v", err)
		} else if len(prs) == 0 {
			existingPRText = "No open PR found for branch"
		} else {
			lines := make([]string, 0, len(prs))
			for _, pr := range prs {
				lines = append(lines, fmt.Sprintf("#%d %s", pr.Number, pr.URL))
			}
			existingPRText = strings.Join(lines, "\n")
		}
	}

	brief := recovery.RenderBrief(recovery.BriefInput{
		IssueNumber:    opts.issueNumber,
		IssueTitle:     opts.issueTitle,
		Branch:         opts.branch,
		WorktreePath:   opts.worktreePath,
		LogPath:        opts.logPath,
		SummaryPath:    opts.summaryPath,
		AttemptNumber:  opts.attempt,
		LastPhase:      opts.lastPhase,
		Status:         opts.status,
		Error:          opts.errorMessage,
		ChangedFiles:   changedFiles,
		ExistingPRText: existingPRText,
		LogTail:        readLogTail(opts.logPath, 50),
	})
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		return fmt.Errorf("write recovery brief: %w", err)
	}
	return nil
}

func readLogTail(path string, maxLines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "(log file not found)"
		}
		return fmt.Sprintf("(failed to read log tail: %v)", err)
	}
	text := strings.TrimRight(string(data), "\r\n")
	if strings.TrimSpace(text) == "" {
		return "(log tail empty)"
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
