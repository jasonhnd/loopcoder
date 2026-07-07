package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/lockfile"
	"github.com/jasonhnd/loopcoder/internal/mcp"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/skills"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	WorkerHardCap      = lcdefaults.WorkerHardCap
	WorkerStallTimeout = lcdefaults.WorkerStallTimeout
)

type Options struct {
	RepoPath        string
	IssueNumber     int
	IssueTitle      string
	IssueBody       string
	BaseBranch      string
	Branch          string
	RunID           string
	Attempt         int
	RecoveryContext string
	Provider        string
	Model           string
	Effort          string
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
	Git         GitClient
	GitHub      func(repoPath string) GitHubClient
	AgentLookup func(provider string) (agent.Runner, error)
	AcquireLock func(repoPath string, timeout time.Duration) (Lock, error)
	Now         func() time.Time
	PID         func() int
	MkdirTemp   func(dir, pattern string) (string, error)
	RemoveAll   func(path string) error
	RepoSkills  func(repoPath string, domainSkills config.DomainSkills) (string, error)
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
		Now:       time.Now,
		PID:       os.Getpid,
		MkdirTemp: os.MkdirTemp,
		RemoveAll: os.RemoveAll,
		RepoSkills: func(repoPath string, domainSkills config.DomainSkills) (string, error) {
			return skills.BuildPromptSection(skills.PromptSectionOptions{
				RepoPath:            repoPath,
				Paths:               domainSkills.Paths,
				MachineLibraryPaths: domainSkills.MachineLibrary.Paths,
				Select:              domainSkills.Select,
				BudgetBytes:         domainSkills.PromptBudgetBytes,
			})
		},
	}
}

type dispatchContext struct {
	opts     Options
	deps     Deps
	warnings io.Writer
	repoPath string
	github   GitHubClient
	agentRun agent.Runner

	scratch      string
	worktreePath string
	promptPath   string
	summaryPath  string
	logPath      string
	jobID        string
	attemptPath  string
	tracker      *attemptTracker

	domainPolicy      domainWorkerPolicy
	activePhase       string
	dispatchSucceeded bool
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
		opts:     opts,
		deps:     deps,
		warnings: warnings,
		repoPath: repoPath,
		github:   github,
		agentRun: agentRunner,
	}, nil
}

func prepareWorktree(ctx context.Context, dispatch *dispatchContext) error {
	scratch, err := dispatch.deps.MkdirTemp("", "loopcoder-*")
	if err != nil {
		return fmt.Errorf("create scratch directory: %w", err)
	}
	dispatch.scratch = scratch
	dispatch.worktreePath = filepath.Join(scratch, "wt")
	dispatch.promptPath = filepath.Join(scratch, "prompt.txt")
	dispatch.summaryPath = filepath.Join(scratch, "summary.txt")
	dispatch.logPath = filepath.Join(scratch, "codex.log")
	dispatch.jobID = fmt.Sprintf("job-%d-%d", dispatch.opts.IssueNumber, dispatch.deps.PID())
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
	dispatch.activePhase = "worktree_created"
	dispatch.cleanupStatus = "succeeded"
	dispatch.failureStatus = "failed"

	if err := dispatch.deps.Git.FetchOriginBase(ctx, dispatch.repoPath, dispatch.opts.BaseBranch); err != nil {
		return fmt.Errorf("git fetch origin %s: %w", dispatch.opts.BaseBranch, err)
	}
	if err := addWorktreeWithLock(ctx, dispatch.deps, dispatch.repoPath, dispatch.opts.Branch, dispatch.worktreePath, dispatch.opts.BaseBranch); err != nil {
		return err
	}
	dispatch.tracker.transition(dispatch.activePhase, "running", nil, nil)
	return nil
}

func buildInvocation(ctx context.Context, dispatch *dispatchContext) (agent.Invocation, error) {
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
		HardCap:         config.DurationSeconds(resilience.Worker.HardCapSeconds, WorkerHardCap),
		StallTimeout:    config.DurationSeconds(resilience.Worker.StallTimeoutSeconds, WorkerStallTimeout),
		LivenessMode:    domainPolicy.AgentLivenessMode(),
		LivenessCommand: domainPolicy.LivenessCommand,
		RunID:           dispatch.opts.RunID,
		Role:            "worker",
		MCPServers:      mcpServers,
	}, nil
}

func runAgent(ctx context.Context, dispatch *dispatchContext, invocation agent.Invocation) (agent.Result, error) {
	agentResult, agentErr := dispatch.agentRun.Run(ctx, invocation)
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
		dispatch.cleanupStatus = "needs-human"
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
			Report:      &harvest.Report,
		}, nil
	}
	return hungResult(dispatch, agentResult), errors.New(hungErr)
}

func commitAndOpenPR(ctx context.Context, dispatch *dispatchContext, agentResult agent.Result) (Result, error) {
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
	dirty, err := dispatch.deps.Git.StatusPorcelain(ctx, dispatch.worktreePath)
	if err != nil {
		return Result{}, fmt.Errorf("git status --porcelain: %w", err)
	}
	if strings.TrimSpace(dirty) == "" {
		return Result{}, fmt.Errorf("codex made no file changes for issue #%d (nothing to commit)", dispatch.opts.IssueNumber)
	}
	dispatch.tracker.transition(dispatch.activePhase, "running", dispatch.tracker.exitCode, nil)

	if err := dispatch.deps.Git.AddAll(ctx, dispatch.worktreePath); err != nil {
		return Result{}, fmt.Errorf("git add -A: %w", err)
	}
	dispatch.activePhase = "committed"
	if err := dispatch.deps.Git.Commit(ctx, dispatch.worktreePath, buildCommitMessage(dispatch.opts.IssueTitle, dispatch.opts.IssueNumber)); err != nil {
		return Result{}, fmt.Errorf("git commit: %w", err)
	}
	dispatch.tracker.transition(dispatch.activePhase, "running", dispatch.tracker.exitCode, nil)

	dispatch.activePhase = "pushed"
	if err := dispatch.deps.Git.PushUpstream(ctx, dispatch.worktreePath, dispatch.opts.Branch); err != nil {
		return Result{}, fmt.Errorf("git push -u origin %s: %w", dispatch.opts.Branch, err)
	}
	dispatch.tracker.transition(dispatch.activePhase, "running", dispatch.tracker.exitCode, nil)

	dispatch.activePhase = "pr_opened"
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
	failurePhase := dispatch.activePhase
	if failurePhase == "" {
		failurePhase = dispatch.tracker.phase
	}
	if failurePhase == "" {
		failurePhase = "worktree_created"
	}
	errText := failure.Error()
	dispatch.tracker.transition(failurePhase, dispatch.failureStatus, dispatch.tracker.exitCode, &errText)
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
		fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: failed to write recovery brief for %s: %v\n", dispatch.jobID, briefErr)
	}
	fmt.Fprintf(dispatch.warnings, "[loopcoder] preserved %s attempt artifacts: %s\n", dispatch.failureStatus, dispatch.scratch)
}

func cleanup(ctx context.Context, dispatch *dispatchContext, failure error) {
	if dispatch == nil || dispatch.tracker == nil {
		return
	}
	if dispatch.dispatchSucceeded {
		dispatch.tracker.transition("cleanup", dispatch.cleanupStatus, dispatch.tracker.exitCode, nil)
		if dispatch.opts.KeepWorktree {
			fmt.Fprintf(dispatch.warnings, "[loopcoder] kept worktree: %s   (scratch: %s)\n", dispatch.worktreePath, dispatch.scratch)
			return
		}
		if cleanupErr := dispatch.deps.Git.WorktreeRemove(context.Background(), dispatch.repoPath, dispatch.worktreePath); cleanupErr != nil {
			fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: failed to remove worktree %s: %v\n", dispatch.worktreePath, cleanupErr)
		}
		if cleanupErr := dispatch.deps.Git.BranchDelete(context.Background(), dispatch.repoPath, dispatch.opts.Branch); cleanupErr != nil {
			fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: failed to delete local branch %s: %v\n", dispatch.opts.Branch, cleanupErr)
		}
		if cleanupErr := dispatch.deps.RemoveAll(dispatch.scratch); cleanupErr != nil {
			fmt.Fprintf(dispatch.warnings, "[loopcoder] warning: failed to remove scratch directory %s: %v\n", dispatch.scratch, cleanupErr)
		}
		return
	}
	if failure == nil {
		return
	}
	writeRecovery(ctx, dispatch, failure)
}

func hungResult(dispatch *dispatchContext, agentResult agent.Result) Result {
	return Result{
		OK:          false,
		Issue:       dispatch.opts.IssueNumber,
		Branch:      dispatch.opts.Branch,
		RunID:       dispatch.opts.RunID,
		AttemptPath: dispatch.attemptPath,
		Status:      "hung",
		ExitCode:    agentResult.ExitCode,
		LogBytes:    fileSize(dispatch.logPath),
	}
}

type PromptOptions struct {
	IssueNumber     int
	IssueTitle      string
	IssueBody       string
	Branch          string
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
		Action:      fmt.Sprintf("implement issue #%d", opts.IssueNumber),
		ExitCode:    result.ExitCode,
		StartedAt:   result.StartedAt,
		EndedAt:     result.EndedAt,
		DurationMS:  result.DurationMS,
		Usage:       result.Usage,
		Verified:    true,
	}
}

func buildCommitMessage(title string, issueNumber int) string {
	return fmt.Sprintf("%s (closes #%d)", title, issueNumber)
}

func buildPRBody(issueNumber int, summary string) string {
	return fmt.Sprintf("Closes #%d\n\n%s", issueNumber, summary)
}

type hungHarvestOptions struct {
	repoPath     string
	runID        string
	jobID        string
	worktreePath string
	logPath      string
	summaryPath  string
	opts         Options
	agentResult  agent.Result
	errorMessage string
	git          GitClient
	github       GitHubClient
	now          func() time.Time
	warnings     io.Writer
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

	if err := opts.git.AddAll(ctx, opts.worktreePath); err != nil {
		return nil, fmt.Errorf("git add -A for harvest: %w", err)
	}
	if err := opts.git.Commit(ctx, opts.worktreePath, buildHarvestCommitMessage(opts.opts.IssueTitle, opts.opts.IssueNumber)); err != nil {
		return nil, fmt.Errorf("git commit harvest: %w", err)
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
	if deps.RemoveAll == nil {
		deps.RemoveAll = defaults.RemoveAll
	}
	if deps.RepoSkills == nil {
		deps.RepoSkills = defaults.RepoSkills
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
}

type attemptTracker struct {
	repoPath       string
	runID          string
	jobID          string
	issue          int
	attempt        int
	provider       string
	pid            int
	branch         string
	logPath        string
	startedAt      string
	heartbeatAt    string
	lastProgressAt string
	phase          string
	status         string
	logBytes       int64
	exitCode       *int
	errorMessage   *string
	usage          *reporter.Usage
	reporter       *reporter.Report
	now            func() time.Time
	warnings       io.Writer
	attemptPath    string
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
	}
}

func (t *attemptTracker) setUsage(usage reporter.Usage) {
	t.usage = cloneUsage(&usage)
}

func (t *attemptTracker) setReport(record reporter.Report) {
	t.reporter = cloneReport(&record)
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
}

func (t *attemptTracker) writeAttempt() {
	record := state.AttemptRecord{
		Version:        1,
		JobID:          t.jobID,
		Issue:          t.issue,
		Attempt:        t.attempt,
		Provider:       t.provider,
		PID:            t.pid,
		Phase:          t.phase,
		Status:         t.status,
		Branch:         t.branch,
		StartedAt:      t.startedAt,
		HeartbeatAt:    t.heartbeatAt,
		LastProgressAt: t.lastProgressAt,
		LogBytes:       t.logBytes,
		ExitCode:       t.exitCode,
		Error:          t.errorMessage,
		Usage:          cloneUsage(t.usage),
		Report:         cloneReport(t.reporter),
	}
	if _, err := state.WriteAttempt(t.repoPath, t.runID, record); err != nil {
		fmt.Fprintf(t.warnings, "[loopcoder] warning: failed to write durable attempt state %s: %v\n", t.attemptPath, err)
	}
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
