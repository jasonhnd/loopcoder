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
	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/lockfile"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/skills"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	WorkerHardCap      = 30 * time.Minute
	WorkerStallTimeout = 120 * time.Second
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
	OK          bool                           `json:"ok"`
	Issue       int                            `json:"issue"`
	Branch      string                         `json:"branch"`
	RunID       string                         `json:"run_id"`
	PR          string                         `json:"pr"`
	Summary     string                         `json:"summary"`
	AttemptPath string                         `json:"attempt_path"`
	Status      string                         `json:"status"`
	ExitCode    int                            `json:"exit_code"`
	LogBytes    int64                          `json:"log_bytes"`
	Attestation *attestation.AttestationRecord `json:"attestation,omitempty"`
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
	RepoSkills  func(repoPath string) (string, error)
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
		RepoSkills: func(repoPath string) (string, error) {
			return skills.BuildPromptSection(skills.PromptSectionOptions{RepoPath: repoPath})
		},
	}
}

func Dispatch(ctx context.Context, opts Options, deps Deps) (result Result, err error) {
	deps = withDefaults(deps)
	warnings := opts.Stderr
	if warnings == nil {
		warnings = io.Discard
	}

	if opts.IssueNumber <= 0 {
		return Result{}, errors.New("issue number is required")
	}
	if strings.TrimSpace(opts.IssueTitle) == "" {
		return Result{}, errors.New("issue title is required")
	}
	if strings.TrimSpace(opts.Provider) == "" {
		opts.Provider = "codex"
	}
	agentRunner, lookupErr := deps.AgentLookup(opts.Provider)
	if lookupErr != nil {
		return Result{}, lookupErr
	}
	if agentRunner == nil {
		return Result{}, fmt.Errorf("provider %q resolved to nil runner", opts.Provider)
	}
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = "main"
	}
	if strings.TrimSpace(opts.Branch) == "" {
		opts.Branch = fmt.Sprintf("loop/issue-%d", opts.IssueNumber)
	}
	if opts.Attempt <= 0 {
		opts.Attempt = 1
	}

	repoPath, err := resolveRepo(opts.RepoPath)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(opts.RunID) == "" {
		opts.RunID = state.RunIDForIssue(opts.IssueNumber, deps.Now())
	}

	github := deps.GitHub(repoPath)
	if github == nil {
		return Result{}, errors.New("github client is not configured")
	}
	if repoName, err := github.RepoName(ctx); err != nil {
		return Result{}, fmt.Errorf("resolve GitHub repo: %w", err)
	} else if strings.TrimSpace(repoName) == "" {
		return Result{}, errors.New("resolve GitHub repo: empty repo name")
	}

	scratch, err := deps.MkdirTemp("", "loopcoder-*")
	if err != nil {
		return Result{}, fmt.Errorf("create scratch directory: %w", err)
	}

	worktreePath := filepath.Join(scratch, "wt")
	promptPath := filepath.Join(scratch, "prompt.txt")
	summaryPath := filepath.Join(scratch, "summary.txt")
	logPath := filepath.Join(scratch, "codex.log")
	jobID := fmt.Sprintf("job-%d-%d", opts.IssueNumber, deps.PID())
	attemptPath := state.AttemptPath(repoPath, opts.RunID, jobID)

	tracker := newAttemptTracker(attemptTrackerOptions{
		repoPath:    repoPath,
		runID:       opts.RunID,
		jobID:       jobID,
		issue:       opts.IssueNumber,
		attempt:     opts.Attempt,
		provider:    opts.Provider,
		pid:         deps.PID(),
		branch:      opts.Branch,
		logPath:     logPath,
		startedAt:   deps.Now(),
		now:         deps.Now,
		warnings:    warnings,
		attemptPath: attemptPath,
	})

	activePhase := "worktree_created"
	dispatchSucceeded := false
	cleanupStatus := "succeeded"
	failureStatus := "failed"
	defer func() {
		if dispatchSucceeded {
			tracker.transition("cleanup", cleanupStatus, tracker.exitCode, nil)
			if opts.KeepWorktree {
				fmt.Fprintf(warnings, "[loopcoder] kept worktree: %s   (scratch: %s)\n", worktreePath, scratch)
				return
			}
			if cleanupErr := deps.Git.WorktreeRemove(context.Background(), repoPath, worktreePath); cleanupErr != nil {
				fmt.Fprintf(warnings, "[loopcoder] warning: failed to remove worktree %s: %v\n", worktreePath, cleanupErr)
			}
			if cleanupErr := deps.Git.BranchDelete(context.Background(), repoPath, opts.Branch); cleanupErr != nil {
				fmt.Fprintf(warnings, "[loopcoder] warning: failed to delete local branch %s: %v\n", opts.Branch, cleanupErr)
			}
			if cleanupErr := deps.RemoveAll(scratch); cleanupErr != nil {
				fmt.Fprintf(warnings, "[loopcoder] warning: failed to remove scratch directory %s: %v\n", scratch, cleanupErr)
			}
			return
		}
		if err == nil {
			return
		}

		failurePhase := activePhase
		if failurePhase == "" {
			failurePhase = tracker.phase
		}
		if failurePhase == "" {
			failurePhase = "worktree_created"
		}
		errText := err.Error()
		tracker.transition(failurePhase, failureStatus, tracker.exitCode, &errText)
		if briefErr := writeRecoveryBrief(ctx, recoveryBriefOptions{
			repoPath:     repoPath,
			runID:        opts.RunID,
			jobID:        jobID,
			issueNumber:  opts.IssueNumber,
			issueTitle:   opts.IssueTitle,
			branch:       opts.Branch,
			worktreePath: worktreePath,
			logPath:      logPath,
			summaryPath:  summaryPath,
			attempt:      opts.Attempt,
			lastPhase:    failurePhase,
			status:       failureStatus,
			errorMessage: errText,
			git:          deps.Git,
			github:       github,
			warnings:     warnings,
		}); briefErr != nil {
			fmt.Fprintf(warnings, "[loopcoder] warning: failed to write recovery brief for %s: %v\n", jobID, briefErr)
		}
		fmt.Fprintf(warnings, "[loopcoder] preserved %s attempt artifacts: %s\n", failureStatus, scratch)
	}()

	if err := deps.Git.FetchOriginBase(ctx, repoPath, opts.BaseBranch); err != nil {
		return Result{}, fmt.Errorf("git fetch origin %s: %w", opts.BaseBranch, err)
	}
	if err := addWorktreeWithLock(ctx, deps, repoPath, opts.Branch, worktreePath, opts.BaseBranch); err != nil {
		return Result{}, err
	}
	tracker.transition(activePhase, "running", nil, nil)

	activePhase = "prompt_written"
	repoSkills, err := deps.RepoSkills(worktreePath)
	if err != nil {
		return Result{}, fmt.Errorf("read repo skills: %w", err)
	}
	prompt := BuildPrompt(PromptOptions{
		IssueNumber:     opts.IssueNumber,
		IssueTitle:      opts.IssueTitle,
		IssueBody:       opts.IssueBody,
		Branch:          opts.Branch,
		RecoveryContext: opts.RecoveryContext,
		RepoSkills:      repoSkills,
	})
	if err := os.WriteFile(promptPath, []byte(prompt), 0o644); err != nil {
		return Result{}, fmt.Errorf("write prompt: %w", err)
	}
	tracker.transition(activePhase, "running", nil, nil)

	activePhase = "codex_started"
	tracker.transition(activePhase, "running", nil, nil)
	resilience, err := config.ResilienceForRepo(ctx, repoPath, config.LoadOptions{
		BaseBranch:     opts.BaseBranch,
		ConfigFromBase: opts.ConfigFromBase,
	})
	if err != nil {
		return Result{}, err
	}
	agentResult, agentErr := agentRunner.Run(ctx, agent.Invocation{
		WorktreePath: worktreePath,
		Prompt:       prompt,
		LogPath:      logPath,
		Model:        opts.Model,
		Effort:       opts.Effort,
		HardCap:      config.DurationSeconds(resilience.Worker.HardCapSeconds, WorkerHardCap),
		StallTimeout: config.DurationSeconds(resilience.Worker.StallTimeoutSeconds, WorkerStallTimeout),
		RunID:        opts.RunID,
		Role:         "worker",
	})
	activePhase = "codex_exited"
	var exitCodePtr *int
	if agentResult.ExitCode >= 0 {
		exitCode := agentResult.ExitCode
		exitCodePtr = &exitCode
	}
	tracker.transition(activePhase, "running", exitCodePtr, nil)
	if agentResult.Hung {
		failureStatus = "hung"
		hungErr := workerHungError(opts.Provider, agentResult.HungReason, logPath)
		// The deferred failure handler records the "hung" transition (phase +
		// error) on return; only the distinct hung run-event is emitted here.
		tracker.appendEvent("worker_hung", "hung", map[string]string{
			"reason":      "hung",
			"hung_reason": firstNonEmpty(agentResult.HungReason, "unknown"),
			"provider":    opts.Provider,
		})
		harvest, harvestErr := harvestHungWorktree(ctx, hungHarvestOptions{
			repoPath:     repoPath,
			runID:        opts.RunID,
			jobID:        jobID,
			worktreePath: worktreePath,
			logPath:      logPath,
			summaryPath:  summaryPath,
			opts:         opts,
			agentResult:  agentResult,
			errorMessage: hungErr,
			git:          deps.Git,
			github:       github,
			now:          deps.Now,
			warnings:     warnings,
		})
		if harvestErr != nil {
			return Result{
				OK:          false,
				Issue:       opts.IssueNumber,
				Branch:      opts.Branch,
				RunID:       opts.RunID,
				AttemptPath: attemptPath,
				Status:      "hung",
				ExitCode:    agentResult.ExitCode,
				LogBytes:    fileSize(logPath),
			}, fmt.Errorf("%s; harvest failed: %w", hungErr, harvestErr)
		}
		if harvest != nil {
			exitCode := 0
			tracker.branch = harvest.Branch
			tracker.setAttestation(harvest.Attestation)
			tracker.setUsage(harvest.Attestation.Usage)
			tracker.transition(harvest.Phase, "needs-human", &exitCode, nil)
			tracker.appendEvent("worker_harvested", "needs-human", map[string]string{
				"branch": harvest.Branch,
				"pr":     harvest.PR,
				"mode":   harvest.Mode,
			})
			dispatchSucceeded = true
			cleanupStatus = "needs-human"
			return Result{
				OK:          true,
				Issue:       opts.IssueNumber,
				Branch:      harvest.Branch,
				RunID:       opts.RunID,
				PR:          harvest.PR,
				Summary:     harvest.Summary,
				AttemptPath: attemptPath,
				Status:      "needs-human",
				ExitCode:    exitCode,
				LogBytes:    fileSize(logPath),
				Attestation: &harvest.Attestation,
			}, nil
		}
		return Result{
			OK:          false,
			Issue:       opts.IssueNumber,
			Branch:      opts.Branch,
			RunID:       opts.RunID,
			AttemptPath: attemptPath,
			Status:      "hung",
			ExitCode:    agentResult.ExitCode,
			LogBytes:    fileSize(logPath),
		}, errors.New(hungErr)
	}
	if agentErr != nil {
		return Result{}, fmt.Errorf("%s exec failed: %w", opts.Provider, agentErr)
	}
	if agentResult.ExitCode != 0 {
		return Result{}, fmt.Errorf("%s exec failed (exit %d). See %s", opts.Provider, agentResult.ExitCode, logPath)
	}

	attestationRecord := buildWorkerAttestation(opts, agentResult)
	tracker.setAttestation(attestationRecord)
	tracker.writeAttempt()
	if err := attestationRecord.Validate(); err != nil {
		return Result{}, fmt.Errorf("validate worker attestation: %w", err)
	}
	tracker.setUsage(attestationRecord.Usage)
	tracker.writeAttempt()
	if _, err := attestationRecord.CanonicalJSON(); err != nil {
		return Result{}, fmt.Errorf("render worker attestation JSON: %w", err)
	}

	summary := fmt.Sprintf("(%s produced no summary)", opts.Provider)
	if trimmed := strings.TrimSpace(agentResult.Summary); trimmed != "" {
		summary = trimmed
	}

	activePhase = "dirty_checked"
	dirty, err := deps.Git.StatusPorcelain(ctx, worktreePath)
	if err != nil {
		return Result{}, fmt.Errorf("git status --porcelain: %w", err)
	}
	if strings.TrimSpace(dirty) == "" {
		return Result{}, fmt.Errorf("codex made no file changes for issue #%d (nothing to commit)", opts.IssueNumber)
	}
	tracker.transition(activePhase, "running", tracker.exitCode, nil)

	if err := deps.Git.AddAll(ctx, worktreePath); err != nil {
		return Result{}, fmt.Errorf("git add -A: %w", err)
	}
	activePhase = "committed"
	if err := deps.Git.Commit(ctx, worktreePath, buildCommitMessage(opts.IssueTitle, opts.IssueNumber)); err != nil {
		return Result{}, fmt.Errorf("git commit: %w", err)
	}
	tracker.transition(activePhase, "running", tracker.exitCode, nil)

	activePhase = "pushed"
	if err := deps.Git.PushUpstream(ctx, worktreePath, opts.Branch); err != nil {
		return Result{}, fmt.Errorf("git push -u origin %s: %w", opts.Branch, err)
	}
	tracker.transition(activePhase, "running", tracker.exitCode, nil)

	activePhase = "pr_opened"
	body := buildPRBody(opts.IssueNumber, summary)
	prURL, err := github.CreatePR(ctx, opts.Branch, opts.BaseBranch, opts.IssueTitle, body)
	if err != nil {
		return Result{}, fmt.Errorf("gh pr create: %w", err)
	}
	tracker.transition(activePhase, "succeeded", tracker.exitCode, nil)
	dispatchSucceeded = true

	exitCode := 0
	logBytes := fileSize(logPath)
	return Result{
		OK:          true,
		Issue:       opts.IssueNumber,
		Branch:      opts.Branch,
		RunID:       opts.RunID,
		PR:          prURL,
		Summary:     summary,
		AttemptPath: attemptPath,
		Status:      "succeeded",
		ExitCode:    exitCode,
		LogBytes:    logBytes,
		Attestation: &attestationRecord,
	}, nil
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

func buildWorkerAttestation(opts Options, result agent.Result) attestation.AttestationRecord {
	return attestation.AttestationRecord{
		Role:        attestation.RoleWorker,
		Provider:    opts.Provider,
		Model:       result.Model,
		ModelSource: attestation.ModelSourceParsed,
		Effort:      result.Effort,
		Permission:  attestation.PermissionWrite,
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
	Branch      string
	PR          string
	Summary     string
	Phase       string
	Mode        string
	Attestation attestation.AttestationRecord
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
	existing := findOpenHarvestPR(ctx, opts.github, opts.opts.IssueNumber, harvestBranch)
	ended := opts.now()
	if existing != nil {
		record := buildHarvestConductorAttestation(opts.opts, started, ended)
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("validate harvest conductor attestation: %w", err)
		}
		return &hungHarvestResult{
			Branch:      firstNonEmpty(existing.HeadRefName, harvestBranch),
			PR:          existing.URL,
			Summary:     "harvested from hung/killed worker - existing needs-human PR already open",
			Phase:       "harvest_adopted",
			Mode:        "existing-pr",
			Attestation: record,
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
	record := buildHarvestConductorAttestation(opts.opts, started, ended)
	if err := record.Validate(); err != nil {
		return nil, fmt.Errorf("validate harvest conductor attestation: %w", err)
	}
	body := buildHarvestPRBody(opts.opts, opts.agentResult, briefText)
	prURL, err := opts.github.CreatePR(ctx, harvestBranch, opts.opts.BaseBranch, buildHarvestPRTitle(opts.opts.IssueTitle, opts.opts.IssueNumber), body)
	if err != nil {
		return nil, fmt.Errorf("gh pr create harvest: %w", err)
	}
	return &hungHarvestResult{
		Branch:      harvestBranch,
		PR:          prURL,
		Summary:     "harvested from hung/killed worker - possibly incomplete; needs human review",
		Phase:       "harvest_pr_opened",
		Mode:        "created-pr",
		Attestation: record,
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
	fmt.Fprintf(&out, "Refs #%d\n", opts.IssueNumber)
	fmt.Fprintf(&out, "Part of #%d\n\n", opts.IssueNumber)
	fmt.Fprintln(&out, "This needs-human PR was harvested from a hung/killed worker and is possibly incomplete. Human review is required before any merge.")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "## Recovery brief")
	if strings.TrimSpace(recoveryBrief) == "" {
		fmt.Fprintln(&out, "(recovery brief unavailable)")
	} else {
		fmt.Fprintln(&out, strings.TrimRight(recoveryBrief, "\r\n"))
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "## Hung worker partial attestation")
	fmt.Fprintln(&out, "```text")
	fmt.Fprintln(&out, formatHungWorkerPartialAttestation(opts, result))
	fmt.Fprintln(&out, "```")
	return out.String()
}

func formatHungWorkerPartialAttestation(opts Options, result agent.Result) string {
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

func buildHarvestConductorAttestation(opts Options, started, ended time.Time) attestation.AttestationRecord {
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
	return attestation.AttestationRecord{
		Role:        attestation.RoleConductor,
		Provider:    firstNonEmpty(opts.Provider, "loopcoder"),
		Model:       firstNonEmpty(opts.Model, "loopcoder-harvest"),
		ModelSource: attestation.ModelSourceSelfReported,
		Effort:      opts.Effort,
		Permission:  attestation.PermissionOrchestrate,
		Action:      fmt.Sprintf("harvest hung worker issue #%d", opts.IssueNumber),
		ExitCode:    0,
		StartedAt:   state.FormatTimestamp(started),
		EndedAt:     state.FormatTimestamp(ended),
		DurationMS:  ended.Sub(started).Milliseconds(),
		Usage: attestation.Usage{
			TotalTokens: &totalTokens,
		},
		Verified: false,
	}
}

func findOpenHarvestPR(ctx context.Context, github GitHubClient, issueNumber int, currentHarvestBranch string) *gh.PullRequest {
	if github == nil {
		return nil
	}
	if prs, err := github.ListHeadPRs(ctx, currentHarvestBranch); err == nil && len(prs) > 0 {
		return &gh.PullRequest{
			Number:      prs[0].Number,
			URL:         prs[0].URL,
			HeadRefName: currentHarvestBranch,
		}
	}
	openPRs, err := github.ListOpenPRs(ctx)
	if err != nil {
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
	usage          *attestation.Usage
	attestation    *attestation.AttestationRecord
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

func (t *attemptTracker) setUsage(usage attestation.Usage) {
	t.usage = cloneUsage(&usage)
}

func (t *attemptTracker) setAttestation(record attestation.AttestationRecord) {
	t.attestation = cloneAttestation(&record)
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
		Attestation:    cloneAttestation(t.attestation),
	}
	if _, err := state.WriteAttempt(t.repoPath, t.runID, record); err != nil {
		fmt.Fprintf(t.warnings, "[loopcoder] warning: failed to write durable attempt state %s: %v\n", t.attemptPath, err)
	}
}

func cloneAttestation(record *attestation.AttestationRecord) *attestation.AttestationRecord {
	if record == nil {
		return nil
	}
	clone := *record
	clone.Usage = *cloneUsage(&record.Usage)
	return &clone
}

func cloneUsage(usage *attestation.Usage) *attestation.Usage {
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
	if err := os.WriteFile(briefPath, []byte(brief), 0o644); err != nil {
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
