package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

var (
	githubTokenPattern = regexp.MustCompile(`(?i)(ghp|github_pat|gho|ghu|ghs|ghr)_[A-Za-z0-9_]+`)
	apiKeyPattern      = regexp.MustCompile(`(?i)sk-[A-Za-z0-9_-]{20,}`)
	bearerPattern      = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]+`)
	assignmentPattern  = regexp.MustCompile(`(?i)((token|password|secret|api[_-]?key)\s*[=:]\s*)\S+`)
)

type BriefInput struct {
	IssueNumber    int
	IssueTitle     string
	Branch         string
	WorktreePath   string
	LogPath        string
	SummaryPath    string
	AttemptNumber  int
	LastPhase      string
	Status         string
	Error          string
	ChangedFiles   string
	ExistingPRText string
	LogTail        string
}

type Action string

const (
	ActionAdopt     Action = "adopt"
	ActionBlocked   Action = "blocked"
	ActionRetry     Action = "retry"
	ActionSucceeded Action = "succeeded"

	AttemptStrategySameConfig     = "same_config"
	AttemptStrategyUpgradedConfig = "upgraded_config"

	AttemptRecordVersion = 1
	StatusHung           = "hung"
	HungReasonLedger     = "reason=hung"
)

type Options struct {
	RepoPath         string
	IssueNumber      int
	IssueTitle       string
	IssueBody        string
	RunID            string
	BaseBranch       string
	MaxAttempts      int
	BackoffSeconds   []int
	Provider         string
	Model            string
	Effort           string
	ConfigFromBase   bool
	UpgradedModel    string
	UpgradedEffort   string
	FailureContext   string
	SkipAdoptPR      bool
	VerifierProvider string
	VerifierModel    string
	VerifierEffort   string
	VerifierTimeout  time.Duration
	Budget           config.GuardrailBudget
	CircuitBreaker   config.GuardrailCircuitBreaker
	Now              time.Time
	Stderr           io.Writer
}

type DispatchOptions struct {
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
	Stderr          io.Writer
}

type DispatchResult struct {
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

type Result struct {
	Action           Action
	Report           string
	DispatchResult   *DispatchResult
	ReviewResult     *loopreview.Result
	RecoveryAttempts []AttemptRecord
	AdoptedPR        *AdoptedPR
}

type PullRequestReader interface {
	ListHeadPRs(ctx context.Context, branch string) ([]gh.PullRequestReference, error)
	ListOpenPRs(ctx context.Context) ([]gh.PullRequest, error)
}

type DispatchFunc func(ctx context.Context, opts DispatchOptions) (DispatchResult, error)
type ReviewFunc func(ctx context.Context, opts loopreview.Options) (loopreview.Result, error)
type SleepFunc func(ctx context.Context, duration time.Duration) error
type RecordAttemptFunc func(repoPath, runID string, record AttemptRecord) error

type Deps struct {
	GitHub        func(repoPath string) PullRequestReader
	LoadAttempts  func(repoPath, runID string) ([]state.Attempt, error)
	ReadFile      func(path string) ([]byte, error)
	Sleep         SleepFunc
	Dispatch      DispatchFunc
	Review        ReviewFunc
	RecordAttempt RecordAttemptFunc
}

type issuePR struct {
	Number      int
	URL         string
	HeadRefName string
}

type AdoptedPR struct {
	Number      int    `json:"number"`
	URL         string `json:"url,omitempty"`
	HeadRefName string `json:"head_ref_name,omitempty"`
}

type attemptHistoryEntry struct {
	Record              state.Attempt
	RecoveryContextPath string
}

type AttemptRecord struct {
	Version             int                 `json:"version"`
	Issue               int                 `json:"issue"`
	RunID               string              `json:"run_id"`
	Attempt             int                 `json:"attempt"`
	Strategy            string              `json:"strategy"`
	Status              string              `json:"status"`
	Branch              string              `json:"branch,omitempty"`
	PR                  string              `json:"pr,omitempty"`
	RecoveryContextPath string              `json:"recovery_context_path,omitempty"`
	Model               string              `json:"model,omitempty"`
	Effort              string              `json:"effort,omitempty"`
	Error               string              `json:"error,omitempty"`
	StartedAt           string              `json:"started_at,omitempty"`
	FinishedAt          string              `json:"finished_at,omitempty"`
	DispatchResult      *DispatchResult     `json:"dispatch_result,omitempty"`
	Review              *loopreview.Verdict `json:"review,omitempty"`
}

// Scrub redacts common secret shapes before text is written to recovery state.
func Scrub(text string) string {
	text = githubTokenPattern.ReplaceAllString(text, "[REDACTED_GITHUB_TOKEN]")
	text = apiKeyPattern.ReplaceAllString(text, "[REDACTED_API_KEY]")
	text = bearerPattern.ReplaceAllString(text, "${1}[REDACTED_TOKEN]")
	text = assignmentPattern.ReplaceAllString(text, "${1}[REDACTED_SECRET]")
	return text
}

// RenderBrief renders the Markdown recovery context consumed by retry workers.
func RenderBrief(input BriefInput) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "# Recovery context for issue #%d\n\n", input.IssueNumber)
	fmt.Fprintf(&out, "- Issue: #%d\n", input.IssueNumber)
	fmt.Fprintf(&out, "- Title: %s\n", input.IssueTitle)
	fmt.Fprintf(&out, "- Branch: %s\n", input.Branch)
	fmt.Fprintf(&out, "- Worktree path: %s\n", input.WorktreePath)
	fmt.Fprintf(&out, "- Log path: %s\n", input.LogPath)
	fmt.Fprintf(&out, "- Summary path: %s\n", input.SummaryPath)
	fmt.Fprintf(&out, "- Attempt: %d\n", input.AttemptNumber)
	fmt.Fprintf(&out, "- Last phase: %s\n", input.LastPhase)
	fmt.Fprintf(&out, "- Status: %s\n", input.Status)
	fmt.Fprintf(&out, "- Error: %s\n", Scrub(input.Error))
	fmt.Fprintln(&out)
	writeSection(&out, "Changed files", input.ChangedFiles)
	writeSection(&out, "Existing PR for branch", input.ExistingPRText)
	writeSection(&out, "Scrubbed log tail (last 50 lines)", Scrub(input.LogTail))
	return out.String()
}

func writeSection(out *bytes.Buffer, title, body string) {
	fmt.Fprintf(out, "## %s\n\n", title)
	fmt.Fprintln(out, "```text")
	fmt.Fprintln(out, strings.TrimRight(body, "\r\n"))
	fmt.Fprintln(out, "```")
	fmt.Fprintln(out)
}

func DefaultDeps() Deps {
	return Deps{
		GitHub: func(repoPath string) PullRequestReader {
			return gh.New(repoPath)
		},
		LoadAttempts:  state.LoadAttempts,
		ReadFile:      os.ReadFile,
		Sleep:         SleepContext,
		RecordAttempt: RecordAttempt,
	}
}

func SleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func Run(ctx context.Context, opts Options, deps Deps) (Result, error) {
	deps = withRecoverDefaults(deps)
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = lcdefaults.BaseBranch
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = lcdefaults.WorkerMaxAttempts
	}
	if opts.MaxAttempts < 0 {
		return Result{}, errors.New("max attempts must be non-negative")
	}
	if opts.BackoffSeconds == nil {
		opts.BackoffSeconds = lcdefaults.WorkerRetryBackoffSeconds()
	}
	for _, seconds := range opts.BackoffSeconds {
		if seconds < 0 {
			return Result{}, errors.New("backoff seconds must be non-negative")
		}
	}
	if strings.TrimSpace(opts.Provider) == "" {
		opts.Provider = "codex"
	}
	if opts.VerifierTimeout == 0 {
		opts.VerifierTimeout = loopreview.DefaultVerifierTimeout
	}
	if opts.IssueNumber <= 0 {
		return Result{}, errors.New("issue number is required")
	}
	if strings.TrimSpace(opts.IssueTitle) == "" {
		return Result{}, errors.New("issue title is required")
	}
	if strings.TrimSpace(opts.RunID) == "" {
		return Result{}, errors.New("run id is required")
	}

	repoPath, err := resolveRepo(opts.RepoPath)
	if err != nil {
		return Result{}, err
	}

	attempts, priorAttempts, latestStatus, latestBriefPath, latestBriefText, err := loadRecoverySnapshot(repoPath, opts, deps)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(latestBriefText) == "" {
		latestBriefText = strings.TrimSpace(opts.FailureContext)
	}

	var adopted *issuePR
	if !opts.SkipAdoptPR && deps.GitHub != nil {
		adopted = findOpenIssuePR(ctx, deps.GitHub(repoPath), opts.IssueNumber, attempts, opts.MaxAttempts)
	}
	if adopted != nil {
		if opts.CircuitBreaker.Enabled() {
			now := opts.Now
			if now.IsZero() {
				now = time.Now().UTC()
			}
			decision := guardrails.EvaluateCircuitBreaker(guardrails.CircuitOptions{
				RepoPath:       repoPath,
				RunID:          opts.RunID,
				BaseBranch:     opts.BaseBranch,
				Issue:          opts.IssueNumber,
				CircuitBreaker: opts.CircuitBreaker,
				Outcome: &guardrails.CircuitOutcome{
					Kind:             guardrails.CircuitOutcomeAttempt,
					MaterialProgress: true,
					Detail:           fmt.Sprintf("adopted PR #%d", adopted.Number),
				},
				Now: now,
			})
			if _, err := guardrails.RecordDecision(repoPath, decision); err != nil {
				return Result{
					Action: ActionBlocked,
					Report: renderCircuitBlockedReport(opts.IssueNumber, opts.RunID, priorAttempts, latestStatus, latestBriefPath, latestBriefText, attempts, ledgerWriteFailedCircuitDecision(opts, repoPath, err)),
				}, nil
			}
		}
		return Result{
			Action:    ActionAdopt,
			Report:    renderAdoptReport(opts.IssueNumber, opts.RunID, priorAttempts, latestStatus, *adopted),
			AdoptedPR: &AdoptedPR{Number: adopted.Number, URL: adopted.URL, HeadRefName: adopted.HeadRefName},
		}, nil
	}

	if deps.Dispatch == nil {
		return Result{}, errors.New("dispatch is not configured")
	}

	var report strings.Builder
	recoveryAttempts := []AttemptRecord{}
	localPriorAttempts := priorAttempts
	var lastDispatchResult *DispatchResult
	var lastReviewResult *loopreview.Result
	for {
		attempts, priorAttempts, latestStatus, latestBriefPath, latestBriefText, err = loadRecoverySnapshot(repoPath, opts, deps)
		if err != nil {
			return Result{}, err
		}
		if localPriorAttempts > priorAttempts {
			priorAttempts = localPriorAttempts
		}
		if strings.TrimSpace(latestBriefText) == "" {
			latestBriefText = strings.TrimSpace(opts.FailureContext)
		}
		latestHung := latestFailureHung(attempts, latestStatus, latestBriefText) || lastRecoveryAttemptHung(recoveryAttempts)
		if priorAttempts >= opts.MaxAttempts {
			blockReason := ""
			if latestHung {
				blockReason = HungReasonLedger
				recoveryAttempts = append(recoveryAttempts, recordHungNeedsHuman(repoPath, opts, deps, priorAttempts, latestBriefPath))
			}
			report.WriteString(renderBlockedReport(opts.IssueNumber, opts.RunID, priorAttempts, opts.MaxAttempts, latestStatus, latestBriefPath, latestBriefText, attempts, blockReason))
			return Result{Action: ActionBlocked, Report: report.String(), DispatchResult: lastDispatchResult, ReviewResult: lastReviewResult, RecoveryAttempts: recoveryAttempts}, nil
		}

		if blocked, blockedReport := evaluateRecoverGuardrails(repoPath, opts, attempts, priorAttempts, latestStatus, latestBriefPath, latestBriefText); blocked {
			report.WriteString(blockedReport)
			return Result{Action: ActionBlocked, Report: report.String(), DispatchResult: lastDispatchResult, ReviewResult: lastReviewResult, RecoveryAttempts: recoveryAttempts}, nil
		}

		nextAttempt := priorAttempts + 1
		localPriorAttempts = nextAttempt
		strategy := strategyForAttempt(nextAttempt, opts.MaxAttempts)
		if latestHung {
			strategy = AttemptStrategySameConfig
		}
		model, effort := modelEffortForStrategy(opts, strategy)
		retryBranch := fmt.Sprintf("loop/issue-%d-retry-%d", opts.IssueNumber, nextAttempt)
		backoffSeconds := selectBackoffSeconds(priorAttempts, opts.BackoffSeconds)
		report.WriteString(renderRetryReport(opts.IssueNumber, opts.RunID, priorAttempts, latestStatus, latestBriefPath, retryBranch, strategy, model, effort, backoffSeconds))

		record := AttemptRecord{
			Version:             AttemptRecordVersion,
			Issue:               opts.IssueNumber,
			RunID:               opts.RunID,
			Attempt:             nextAttempt,
			Strategy:            strategy,
			Status:              "running",
			Branch:              retryBranch,
			RecoveryContextPath: latestBriefPath,
			Model:               model,
			Effort:              effort,
			StartedAt:           state.FormatTimestamp(recoverNow(opts)),
		}

		if err := deps.Sleep(ctx, time.Duration(backoffSeconds)*time.Second); err != nil {
			record.Status = "failed"
			record.Error = err.Error()
			record.FinishedAt = state.FormatTimestamp(recoverNow(opts))
			_ = deps.RecordAttempt(repoPath, opts.RunID, record)
			recoveryAttempts = append(recoveryAttempts, record)
			return Result{Action: ActionRetry, Report: report.String(), RecoveryAttempts: recoveryAttempts}, err
		}

		dispatchResult, dispatchErr := deps.Dispatch(ctx, DispatchOptions{
			RepoPath:        repoPath,
			IssueNumber:     opts.IssueNumber,
			IssueTitle:      opts.IssueTitle,
			IssueBody:       opts.IssueBody,
			BaseBranch:      opts.BaseBranch,
			Branch:          retryBranch,
			RunID:           opts.RunID,
			Attempt:         nextAttempt,
			RecoveryContext: latestBriefText,
			Provider:        opts.Provider,
			Model:           model,
			Effort:          effort,
			ConfigFromBase:  opts.ConfigFromBase,
			Stderr:          opts.Stderr,
		})
		lastDispatchResult = &dispatchResult
		record.DispatchResult = &dispatchResult
		record.PR = dispatchResult.PR
		record.FinishedAt = state.FormatTimestamp(recoverNow(opts))
		if dispatchErr != nil || !dispatchResult.OK || strings.TrimSpace(dispatchResult.PR) == "" {
			record.Status = "failed"
			record.Error = dispatchFailureDetail(dispatchResult, dispatchErr)
			if dispatchFailureHung(dispatchResult, dispatchErr) {
				record.Status = StatusHung
				record.Error = ensureHungReason(record.Error)
			}
			if opts.CircuitBreaker.Enabled() {
				decision := recordRetryCircuitOutcome(repoPath, opts, priorAttempts, false, record.Error)
				if !decision.Allowed {
					record.Status = guardrails.StatusNeedsHuman
					_ = deps.RecordAttempt(repoPath, opts.RunID, record)
					recoveryAttempts = append(recoveryAttempts, record)
					attempts, priorAttempts, latestStatus, latestBriefPath, latestBriefText, _ = loadRecoverySnapshot(repoPath, opts, deps)
					report.WriteString(renderCircuitBlockedReport(opts.IssueNumber, opts.RunID, priorAttempts, latestStatus, latestBriefPath, latestBriefText, attempts, decision))
					return Result{Action: ActionBlocked, Report: report.String(), DispatchResult: &dispatchResult, RecoveryAttempts: recoveryAttempts}, nil
				}
			}
			_ = deps.RecordAttempt(repoPath, opts.RunID, record)
			recoveryAttempts = append(recoveryAttempts, record)
			if data, marshalErr := json.Marshal(dispatchResult); marshalErr == nil {
				report.WriteString(string(data) + "\n")
			}
			opts.FailureContext = dispatchFailureContext(latestBriefText, record.Error)
			continue
		}

		if strings.EqualFold(strings.TrimSpace(dispatchResult.Status), guardrails.StatusNeedsHuman) {
			record.Status = guardrails.StatusNeedsHuman
			record.Error = firstNonEmpty(dispatchResult.Summary, "dispatch produced a needs-human PR")
			_ = deps.RecordAttempt(repoPath, opts.RunID, record)
			recoveryAttempts = append(recoveryAttempts, record)
			report.WriteString(renderReviewBlockedReport(opts.IssueNumber, opts.RunID, dispatchResult.PR, record.Error))
			return Result{Action: ActionBlocked, Report: report.String(), DispatchResult: &dispatchResult, RecoveryAttempts: recoveryAttempts}, nil
		}

		if opts.CircuitBreaker.Enabled() {
			recordRetryCircuitOutcome(repoPath, opts, priorAttempts, true, dispatchResult.Status)
		}

		if deps.Review == nil || strings.TrimSpace(opts.VerifierProvider) == "" {
			record.Status = "succeeded"
			_ = deps.RecordAttempt(repoPath, opts.RunID, record)
			recoveryAttempts = append(recoveryAttempts, record)
			data, marshalErr := json.Marshal(dispatchResult)
			if marshalErr != nil {
				return Result{Action: ActionRetry, Report: report.String(), DispatchResult: &dispatchResult, RecoveryAttempts: recoveryAttempts}, marshalErr
			}
			report.WriteString(string(data) + "\n")
			return Result{Action: ActionRetry, Report: report.String(), DispatchResult: &dispatchResult, RecoveryAttempts: recoveryAttempts}, nil
		}

		prNumber, ok := parsePRNumber(dispatchResult.PR)
		if !ok {
			record.Status = guardrails.StatusNeedsHuman
			record.Error = "could not parse pull request number"
			_ = deps.RecordAttempt(repoPath, opts.RunID, record)
			recoveryAttempts = append(recoveryAttempts, record)
			report.WriteString(renderReviewBlockedReport(opts.IssueNumber, opts.RunID, dispatchResult.PR, record.Error))
			return Result{Action: ActionBlocked, Report: report.String(), DispatchResult: &dispatchResult, RecoveryAttempts: recoveryAttempts}, nil
		}
		reviewResult, reviewErr := deps.Review(ctx, loopreview.Options{
			RepoPath:       repoPath,
			PRNumber:       prNumber,
			Provider:       opts.VerifierProvider,
			Model:          opts.VerifierModel,
			Effort:         opts.VerifierEffort,
			BaseBranch:     opts.BaseBranch,
			ConfigFromBase: opts.ConfigFromBase,
			Timeout:        opts.VerifierTimeout,
			Stderr:         opts.Stderr,
		})
		lastReviewResult = &reviewResult
		record.Review = &reviewResult.Verdict
		if reviewErr != nil {
			record.Status = guardrails.StatusNeedsHuman
			record.Error = reviewErr.Error()
			_ = deps.RecordAttempt(repoPath, opts.RunID, record)
			recoveryAttempts = append(recoveryAttempts, record)
			report.WriteString(renderReviewBlockedReport(opts.IssueNumber, opts.RunID, dispatchResult.PR, reviewErr.Error()))
			return Result{Action: ActionBlocked, Report: report.String(), DispatchResult: &dispatchResult, ReviewResult: &reviewResult, RecoveryAttempts: recoveryAttempts}, nil
		}
		switch reviewResult.Verdict.Verdict {
		case loopreview.VerdictPass:
			record.Status = "succeeded"
			_ = deps.RecordAttempt(repoPath, opts.RunID, record)
			recoveryAttempts = append(recoveryAttempts, record)
			report.WriteString(renderReviewResultReport(opts.IssueNumber, opts.RunID, dispatchResult.PR, reviewResult))
			return Result{Action: ActionSucceeded, Report: report.String(), DispatchResult: &dispatchResult, ReviewResult: &reviewResult, RecoveryAttempts: recoveryAttempts}, nil
		case loopreview.VerdictFail:
			record.Status = "failed"
			record.Error = firstNonEmpty(reviewResult.Verdict.Evidence, "verifier returned fail")
			_ = deps.RecordAttempt(repoPath, opts.RunID, record)
			recoveryAttempts = append(recoveryAttempts, record)
			report.WriteString(renderReviewResultReport(opts.IssueNumber, opts.RunID, dispatchResult.PR, reviewResult))
			opts.FailureContext = renderReviewFailureContext(opts.IssueNumber, dispatchResult.PR, reviewResult)
			continue
		default:
			record.Status = guardrails.StatusNeedsHuman
			record.Error = firstNonEmpty(reviewResult.Verdict.Evidence, "verifier returned needs-human")
			_ = deps.RecordAttempt(repoPath, opts.RunID, record)
			recoveryAttempts = append(recoveryAttempts, record)
			report.WriteString(renderReviewBlockedReport(opts.IssueNumber, opts.RunID, dispatchResult.PR, record.Error))
			return Result{Action: ActionBlocked, Report: report.String(), DispatchResult: &dispatchResult, ReviewResult: &reviewResult, RecoveryAttempts: recoveryAttempts}, nil
		}
	}
}

func withRecoverDefaults(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.GitHub == nil {
		deps.GitHub = defaults.GitHub
	}
	if deps.LoadAttempts == nil {
		deps.LoadAttempts = defaults.LoadAttempts
	}
	if deps.ReadFile == nil {
		deps.ReadFile = defaults.ReadFile
	}
	if deps.Sleep == nil {
		deps.Sleep = defaults.Sleep
	}
	if deps.RecordAttempt == nil {
		deps.RecordAttempt = defaults.RecordAttempt
	}
	return deps
}

func RecordAttempt(repoPath, runID string, record AttemptRecord) error {
	if strings.TrimSpace(runID) == "" || record.Issue <= 0 || record.Attempt <= 0 {
		return nil
	}
	record.Version = AttemptRecordVersion
	path := filepath.Join(state.RecoveryPath(repoPath, runID), fmt.Sprintf("issue-%d-attempts.jsonl", record.Issue))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create recovery attempt directory: %w", err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal recovery attempt record: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open recovery attempt record: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append recovery attempt record: %w", err)
	}
	return nil
}

func loadRecoverySnapshot(repoPath string, opts Options, deps Deps) ([]attemptHistoryEntry, int, string, string, string, error) {
	allAttempts, err := deps.LoadAttempts(repoPath, opts.RunID)
	if err != nil {
		return nil, 0, "", "", "", err
	}
	attempts := attemptHistory(repoPath, opts.RunID, opts.IssueNumber, allAttempts)
	priorAttempts := len(attempts)
	latest := latestHistoryAttempt(attempts)
	latestStatus := "missing-state"
	latestBriefPath := ""
	latestBriefText := ""
	if latest != nil {
		latestStatus = latest.Record.Status
		latestBriefPath = latest.RecoveryContextPath
		if strings.TrimSpace(latestBriefPath) != "" {
			if data, readErr := deps.ReadFile(latestBriefPath); readErr == nil {
				latestBriefText = string(data)
			}
		}
	}
	return attempts, priorAttempts, latestStatus, latestBriefPath, latestBriefText, nil
}

func evaluateRecoverGuardrails(repoPath string, opts Options, attempts []attemptHistoryEntry, priorAttempts int, latestStatus, latestBriefPath, latestBriefText string) (bool, string) {
	if opts.Budget.Enabled() {
		now := recoverNow(opts)
		decision := guardrails.EvaluateBudget(guardrails.BudgetOptions{
			RepoPath:         repoPath,
			RunID:            opts.RunID,
			BaseBranch:       opts.BaseBranch,
			Issue:            opts.IssueNumber,
			Budget:           opts.Budget,
			ProposedAttempts: 1,
			Now:              now,
		})
		if _, err := guardrails.RecordDecision(repoPath, decision); err != nil {
			decision.Allowed = false
			decision.Status = guardrails.StatusNeedsHuman
			decision.Reason = "guardrails.budget.ledger-write-failed"
			decision.Message = fmt.Sprintf("needs-human: guardrails.budget ledger write failed: %v", err)
		}
		if !decision.Allowed {
			return true, renderBudgetBlockedReport(opts.IssueNumber, opts.RunID, priorAttempts, latestStatus, latestBriefPath, latestBriefText, attempts, decision)
		}
	}

	if opts.CircuitBreaker.Enabled() {
		now := recoverNow(opts)
		decision := guardrails.EvaluateCircuitBreaker(guardrails.CircuitOptions{
			RepoPath:       repoPath,
			RunID:          opts.RunID,
			BaseBranch:     opts.BaseBranch,
			Issue:          opts.IssueNumber,
			CircuitBreaker: opts.CircuitBreaker,
			Now:            now,
		})
		if _, err := guardrails.RecordDecision(repoPath, decision); err != nil {
			decision = ledgerWriteFailedCircuitDecision(opts, repoPath, err)
		}
		if !decision.Allowed {
			return true, renderCircuitBlockedReport(opts.IssueNumber, opts.RunID, priorAttempts, latestStatus, latestBriefPath, latestBriefText, attempts, decision)
		}
	}
	return false, ""
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

func attemptHistory(repoPath, runID string, issueNumber int, attempts []state.Attempt) []attemptHistoryEntry {
	history := make([]attemptHistoryEntry, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.Issue != issueNumber {
			continue
		}
		briefPath := resolveStatePath(repoPath, attempt.RecoveryContextPath)
		if strings.TrimSpace(briefPath) == "" {
			briefPath = state.RecoveryBriefPath(repoPath, runID, attempt.JobID)
		}
		history = append(history, attemptHistoryEntry{
			Record:              attempt,
			RecoveryContextPath: briefPath,
		})
	}
	sort.Slice(history, func(i, j int) bool {
		if history[i].Record.Attempt != history[j].Record.Attempt {
			return history[i].Record.Attempt < history[j].Record.Attempt
		}
		if !history[i].Record.LastWriteUTC.Equal(history[j].Record.LastWriteUTC) {
			return history[i].Record.LastWriteUTC.Before(history[j].Record.LastWriteUTC)
		}
		return history[i].Record.JobID < history[j].Record.JobID
	})
	return history
}

func resolveStatePath(repoPath, path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	if filepath.IsAbs(trimmed) {
		return filepath.Clean(trimmed)
	}
	return filepath.Join(repoPath, trimmed)
}

func latestHistoryAttempt(attempts []attemptHistoryEntry) *attemptHistoryEntry {
	if len(attempts) == 0 {
		return nil
	}
	latest := attempts[len(attempts)-1]
	return &latest
}

func findOpenIssuePR(ctx context.Context, reader PullRequestReader, issueNumber int, attempts []attemptHistoryEntry, maxAttempts int) *issuePR {
	if reader == nil {
		return nil
	}
	baseIssueBranch := fmt.Sprintf("loop/issue-%d", issueNumber)
	retryPrefix := baseIssueBranch + "-retry-"
	for _, branch := range candidateBranches(issueNumber, attempts, maxAttempts) {
		prs, err := reader.ListHeadPRs(ctx, branch)
		if err != nil || len(prs) == 0 {
			continue
		}
		return &issuePR{
			Number:      prs[0].Number,
			URL:         prs[0].URL,
			HeadRefName: branch,
		}
	}

	openPRs, err := reader.ListOpenPRs(ctx)
	if err != nil {
		return nil
	}
	for _, pr := range openPRs {
		if pr.HeadRefName == baseIssueBranch || strings.HasPrefix(pr.HeadRefName, retryPrefix) {
			return &issuePR{
				Number:      pr.Number,
				URL:         pr.URL,
				HeadRefName: pr.HeadRefName,
			}
		}
	}
	return nil
}

func candidateBranches(issueNumber int, attempts []attemptHistoryEntry, maxAttempts int) []string {
	baseIssueBranch := fmt.Sprintf("loop/issue-%d", issueNumber)
	retryPrefix := baseIssueBranch + "-retry-"
	seen := map[string]bool{}
	branches := make([]string, 0)
	add := func(branch string) {
		branch = strings.TrimSpace(branch)
		if branch == "" || seen[branch] {
			return
		}
		seen[branch] = true
		branches = append(branches, branch)
	}
	add(baseIssueBranch)
	for _, attempt := range attempts {
		add(attempt.Record.Branch)
		if attempt.Record.Attempt > 1 {
			add(fmt.Sprintf("%s%d", retryPrefix, attempt.Record.Attempt))
		}
	}
	maxRetryBranch := maxAttempts + 1
	if priorBased := len(attempts) + 2; priorBased > maxRetryBranch {
		maxRetryBranch = priorBased
	}
	for i := 2; i <= maxRetryBranch; i++ {
		add(fmt.Sprintf("%s%d", retryPrefix, i))
	}
	return branches
}

func selectBackoffSeconds(priorAttempts int, backoff []int) int {
	if len(backoff) == 0 {
		return 0
	}
	index := priorAttempts - 1
	if index < 0 {
		index = 0
	}
	if index >= len(backoff) {
		index = len(backoff) - 1
	}
	return backoff[index]
}

func strategyForAttempt(attempt, maxAttempts int) string {
	if maxAttempts <= 0 {
		maxAttempts = lcdefaults.WorkerMaxAttempts
	}
	if attempt >= maxAttempts {
		return AttemptStrategyUpgradedConfig
	}
	return AttemptStrategySameConfig
}

func modelEffortForStrategy(opts Options, strategy string) (string, string) {
	model := opts.Model
	effort := opts.Effort
	if strategy != AttemptStrategyUpgradedConfig {
		return model, effort
	}
	if strings.TrimSpace(opts.UpgradedModel) != "" {
		model = opts.UpgradedModel
	}
	if strings.TrimSpace(opts.UpgradedEffort) != "" {
		effort = opts.UpgradedEffort
	} else {
		effort = defaultUpgradedEffort(effort)
	}
	return model, effort
}

func defaultUpgradedEffort(current string) string {
	switch strings.ToLower(strings.TrimSpace(current)) {
	case "xhigh", "max":
		return strings.TrimSpace(current)
	default:
		return "xhigh"
	}
}

func recoverNow(opts Options) time.Time {
	if !opts.Now.IsZero() {
		return opts.Now.UTC()
	}
	return time.Now().UTC()
}

func latestFailureHung(attempts []attemptHistoryEntry, latestStatus, _ string) bool {
	if statusOrDetailHung(latestStatus, "") {
		return true
	}
	latest := latestHistoryAttempt(attempts)
	return latest != nil && statusOrDetailHung(latest.Record.Status, latest.Record.Error)
}

func lastRecoveryAttemptHung(attempts []AttemptRecord) bool {
	if len(attempts) == 0 {
		return false
	}
	latest := attempts[len(attempts)-1]
	return statusOrDetailHung(latest.Status, latest.Error)
}

func dispatchFailureHung(result DispatchResult, err error) bool {
	return statusOrDetailHung(result.Status, dispatchFailureDetail(result, err))
}

func statusOrDetailHung(status, detail string) bool {
	if strings.EqualFold(strings.TrimSpace(status), StatusHung) {
		return true
	}
	return strings.Contains(strings.ToLower(detail), HungReasonLedger)
}

func ensureHungReason(detail string) string {
	detail = strings.TrimSpace(detail)
	if strings.Contains(strings.ToLower(detail), HungReasonLedger) {
		return detail
	}
	if detail == "" {
		return HungReasonLedger
	}
	return HungReasonLedger + ": " + detail
}

func recordHungNeedsHuman(repoPath string, opts Options, deps Deps, priorAttempts int, latestBriefPath string) AttemptRecord {
	now := state.FormatTimestamp(recoverNow(opts))
	record := AttemptRecord{
		Version:             AttemptRecordVersion,
		Issue:               opts.IssueNumber,
		RunID:               opts.RunID,
		Attempt:             priorAttempts,
		Strategy:            AttemptStrategySameConfig,
		Status:              guardrails.StatusNeedsHuman,
		RecoveryContextPath: latestBriefPath,
		Error:               HungReasonLedger + ": retry limit reached after hung attempt",
		StartedAt:           now,
		FinishedAt:          now,
	}
	if err := deps.RecordAttempt(repoPath, opts.RunID, record); err != nil && opts.Stderr != nil {
		fmt.Fprintf(opts.Stderr, "[loopcoder] warning: failed to record hung needs-human attempt for issue #%d: %v\n", opts.IssueNumber, err)
	}
	return record
}

func dispatchFailureDetail(result DispatchResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if strings.TrimSpace(result.Status) != "" && strings.TrimSpace(result.Summary) != "" {
		return result.Status + ": " + result.Summary
	}
	if strings.TrimSpace(result.Status) != "" {
		return result.Status
	}
	return "dispatch did not produce a PR"
}

func dispatchFailureContext(previous, detail string) string {
	var out bytes.Buffer
	if strings.TrimSpace(previous) != "" {
		out.WriteString(previous)
		out.WriteString("\n\n")
	}
	out.WriteString("## Recovery attempt failure\n\n")
	out.WriteString("```text\n")
	out.WriteString(Scrub(strings.TrimSpace(detail)))
	out.WriteString("\n```\n")
	return out.String()
}

var retryPRNumberPattern = regexp.MustCompile(`(?i)(?:/pull/|#)([1-9]\d*)\s*$`)

func parsePRNumber(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if number, err := strconv.Atoi(value); err == nil && number > 0 {
		return number, true
	}
	matches := retryPRNumberPattern.FindStringSubmatch(value)
	if len(matches) != 2 {
		return 0, false
	}
	number, err := strconv.Atoi(matches[1])
	return number, err == nil && number > 0
}

func renderAdoptReport(issueNumber int, runID string, priorAttempts int, latestStatus string, pr issuePR) string {
	var out bytes.Buffer
	fmt.Fprintln(&out, "ADOPT EXISTING PR; NO RETRY")
	fmt.Fprintf(&out, "Issue: #%d\n", issueNumber)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	fmt.Fprintf(&out, "Prior attempts: %d\n", priorAttempts)
	fmt.Fprintf(&out, "Latest status: %s\n", latestStatus)
	fmt.Fprintf(&out, "PR: #%d %s\n", pr.Number, pr.URL)
	fmt.Fprintf(&out, "Head branch: %s\n", pr.HeadRefName)
	return out.String()
}

func renderBlockedReport(issueNumber int, runID string, priorAttempts, maxAttempts int, latestStatus, latestBriefPath, latestBriefText string, attempts []attemptHistoryEntry, reason string) string {
	var out bytes.Buffer
	fmt.Fprintln(&out, "BLOCKED: retry limit reached")
	fmt.Fprintf(&out, "Issue: #%d\n", issueNumber)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	fmt.Fprintf(&out, "Prior attempts: %d\n", priorAttempts)
	fmt.Fprintf(&out, "Max attempts: %d\n", maxAttempts)
	fmt.Fprintf(&out, "Latest status: %s\n", latestStatus)
	if strings.TrimSpace(reason) != "" {
		fmt.Fprintf(&out, "Reason: %s\n", reason)
	}
	fmt.Fprintf(&out, "Latest recovery brief: %s\n", latestBriefPath)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Latest recovery brief contents:")
	if strings.TrimSpace(latestBriefText) == "" {
		fmt.Fprintln(&out, "(no recovery brief available)")
	} else {
		fmt.Fprintln(&out, latestBriefText)
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Attempt history:")
	fmt.Fprintln(&out, formatAttemptHistory(attempts))
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Human decision needed: inspect the latest recovery brief, then decide whether to fix credentials/environment, clarify the issue, raise the retry limit and dispatch manually, or close/supersede the failed branch.")
	return out.String()
}

func renderBudgetBlockedReport(issueNumber int, runID string, priorAttempts int, latestStatus, latestBriefPath, latestBriefText string, attempts []attemptHistoryEntry, decision guardrails.Decision) string {
	var out bytes.Buffer
	fmt.Fprintln(&out, "BLOCKED: guardrails budget needs-human")
	fmt.Fprintf(&out, "Issue: #%d\n", issueNumber)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	fmt.Fprintf(&out, "Prior attempts: %d\n", priorAttempts)
	fmt.Fprintf(&out, "Latest status: %s\n", latestStatus)
	fmt.Fprintf(&out, "Latest recovery brief: %s\n", latestBriefPath)
	fmt.Fprintf(&out, "Guardrail: %s\n", decision.Reason)
	fmt.Fprintf(&out, "Budget evidence: %s\n", decision.Message)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Latest recovery brief contents:")
	if strings.TrimSpace(latestBriefText) == "" {
		fmt.Fprintln(&out, "(no recovery brief available)")
	} else {
		fmt.Fprintln(&out, latestBriefText)
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Attempt history:")
	fmt.Fprintln(&out, formatAttemptHistory(attempts))
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Human decision needed: inspect the latest recovery brief and budget evidence, then decide whether to raise the cap, clarify the issue, close/supersede it, or explicitly start a new scoped run.")
	return out.String()
}

func renderCircuitBlockedReport(issueNumber int, runID string, priorAttempts int, latestStatus, latestBriefPath, latestBriefText string, attempts []attemptHistoryEntry, decision guardrails.Decision) string {
	var out bytes.Buffer
	fmt.Fprintln(&out, "BLOCKED: guardrails circuit-breaker needs-human")
	fmt.Fprintf(&out, "Issue: #%d\n", issueNumber)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	fmt.Fprintf(&out, "Prior attempts: %d\n", priorAttempts)
	fmt.Fprintf(&out, "Latest status: %s\n", latestStatus)
	fmt.Fprintf(&out, "Latest recovery brief: %s\n", latestBriefPath)
	fmt.Fprintf(&out, "Guardrail: %s\n", decision.Reason)
	fmt.Fprintf(&out, "No-progress waves: %d\n", decision.Observed.NoProgressWaves)
	fmt.Fprintf(&out, "No-progress attempts: %d\n", decision.Observed.NoProgressAttempts)
	if strings.TrimSpace(decision.LastMaterialProgressAt) == "" {
		fmt.Fprintln(&out, "Last material progress: unknown")
	} else {
		fmt.Fprintf(&out, "Last material progress: %s\n", decision.LastMaterialProgressAt)
	}
	fmt.Fprintf(&out, "Circuit-breaker evidence: %s\n", decision.Message)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Latest recovery brief contents:")
	if strings.TrimSpace(latestBriefText) == "" {
		fmt.Fprintln(&out, "(no recovery brief available)")
	} else {
		fmt.Fprintln(&out, latestBriefText)
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Attempt history:")
	fmt.Fprintln(&out, formatAttemptHistory(attempts))
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Human decision needed: inspect the latest recovery brief and circuit-breaker evidence, then decide whether to clarify the issue, raise the no-progress threshold, close/supersede it, or explicitly start a new scoped run.")
	return out.String()
}

func ledgerWriteFailedCircuitDecision(opts Options, repoPath string, err error) guardrails.Decision {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	decision := guardrails.Decision{
		Guardrail:       guardrails.GuardrailCircuitBreaker,
		Enabled:         true,
		Allowed:         false,
		Status:          guardrails.StatusNeedsHuman,
		Reason:          "guardrails.circuit_breaker.ledger-write-failed",
		DeliveryScopeID: fmt.Sprintf("%s:%d", firstNonEmpty(opts.BaseBranch, lcdefaults.BaseBranch), opts.IssueNumber),
		BaseBranch:      firstNonEmpty(opts.BaseBranch, lcdefaults.BaseBranch),
		Issue:           opts.IssueNumber,
		RunID:           opts.RunID,
		DecisionAt:      now,
	}
	decision.Message = fmt.Sprintf("needs-human: guardrails.circuit_breaker ledger write failed: %v", err)
	return decision
}

func recordRetryCircuitOutcome(repoPath string, opts Options, priorAttempts int, materialProgress bool, detail string) guardrails.Decision {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	countedInState := retryAttemptCount(repoPath, opts.RunID, opts.IssueNumber) > priorAttempts
	decision := guardrails.EvaluateCircuitBreaker(guardrails.CircuitOptions{
		RepoPath:       repoPath,
		RunID:          opts.RunID,
		BaseBranch:     opts.BaseBranch,
		Issue:          opts.IssueNumber,
		CircuitBreaker: opts.CircuitBreaker,
		Outcome: &guardrails.CircuitOutcome{
			Kind:             guardrails.CircuitOutcomeAttempt,
			MaterialProgress: materialProgress,
			Detail:           detail,
			CountedInState:   countedInState,
		},
		Now: now,
	})
	if _, err := guardrails.RecordDecision(repoPath, decision); err != nil {
		return ledgerWriteFailedCircuitDecision(opts, repoPath, err)
	}
	return decision
}

func retryAttemptCount(repoPath, runID string, issueNumber int) int {
	attempts, err := state.LoadAttempts(repoPath, runID)
	if err != nil {
		return 0
	}
	count := 0
	for _, attempt := range attempts {
		if attempt.Issue == issueNumber {
			count++
		}
	}
	return count
}

func renderRetryReport(issueNumber int, runID string, priorAttempts int, latestStatus, latestBriefPath, retryBranch, strategy, model, effort string, backoffSeconds int) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "RETRY: dispatching issue #%d attempt %d\n", issueNumber, priorAttempts+1)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	fmt.Fprintf(&out, "Prior attempts: %d\n", priorAttempts)
	fmt.Fprintf(&out, "Latest status: %s\n", latestStatus)
	fmt.Fprintf(&out, "Latest recovery brief: %s\n", latestBriefPath)
	fmt.Fprintf(&out, "Retry branch: %s\n", retryBranch)
	fmt.Fprintf(&out, "Recovery strategy: %s\n", strategy)
	if strings.TrimSpace(model) != "" {
		fmt.Fprintf(&out, "Model: %s\n", model)
	}
	if strings.TrimSpace(effort) != "" {
		fmt.Fprintf(&out, "Effort: %s\n", effort)
	}
	fmt.Fprintf(&out, "Backoff seconds: %d\n", backoffSeconds)
	return out.String()
}

func renderReviewResultReport(issueNumber int, runID, pr string, result loopreview.Result) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "REVIEW: recovered issue #%d\n", issueNumber)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	fmt.Fprintf(&out, "PR: %s\n", pr)
	fmt.Fprintf(&out, "Verdict: %s\n", result.Verdict.Verdict)
	if strings.TrimSpace(result.Verdict.SpecConformance) != "" {
		fmt.Fprintf(&out, "Spec conformance: %s\n", result.Verdict.SpecConformance)
	}
	if strings.TrimSpace(result.Verdict.Evidence) != "" {
		fmt.Fprintf(&out, "Evidence: %s\n", result.Verdict.Evidence)
	}
	return out.String()
}

func renderReviewBlockedReport(issueNumber int, runID, pr, detail string) string {
	var out bytes.Buffer
	fmt.Fprintln(&out, "BLOCKED: recovery review needs-human")
	fmt.Fprintf(&out, "Issue: #%d\n", issueNumber)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	if strings.TrimSpace(pr) != "" {
		fmt.Fprintf(&out, "PR: %s\n", pr)
	}
	fmt.Fprintf(&out, "Review evidence: %s\n", detail)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Human decision needed: inspect the recovered PR and verifier evidence, then decide whether to clarify the issue, close/supersede it, or explicitly start a new scoped run.")
	return out.String()
}

func renderReviewFailureContext(issueNumber int, pr string, result loopreview.Result) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "# Recovery context for failed loopreview on issue #%d\n\n", issueNumber)
	if strings.TrimSpace(pr) != "" {
		fmt.Fprintf(&out, "- PR: %s\n", pr)
	}
	fmt.Fprintf(&out, "- Verdict: %s\n", result.Verdict.Verdict)
	if strings.TrimSpace(result.Verdict.SpecConformance) != "" {
		fmt.Fprintf(&out, "- Spec conformance: %s\n", result.Verdict.SpecConformance)
	}
	fmt.Fprintln(&out)
	writeSection(&out, "Verifier evidence", result.Verdict.Evidence)
	var findings bytes.Buffer
	for _, finding := range result.Verdict.Findings {
		fmt.Fprintf(&findings, "- %s %s: %s\n", finding.Severity, finding.File, finding.Note)
	}
	writeSection(&out, "Verifier findings", findings.String())
	return out.String()
}

func formatAttemptHistory(attempts []attemptHistoryEntry) string {
	if len(attempts) == 0 {
		return "(no prior attempts found)"
	}
	lines := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		lines = append(lines, fmt.Sprintf(
			"attempt %d job %s: status=%s, phase=%s, error=%s, sidecar=%s, recovery=%s",
			attempt.Record.Attempt,
			attempt.Record.JobID,
			attempt.Record.Status,
			attempt.Record.Phase,
			attempt.Record.Error,
			attempt.Record.Path,
			attempt.RecoveryContextPath,
		))
	}
	return strings.Join(lines, "\n")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
