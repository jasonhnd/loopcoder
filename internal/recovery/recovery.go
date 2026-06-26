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
	"strings"
	"time"

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
	ActionAdopt   Action = "adopt"
	ActionBlocked Action = "blocked"
	ActionRetry   Action = "retry"
)

type Options struct {
	RepoPath       string
	IssueNumber    int
	IssueTitle     string
	IssueBody      string
	RunID          string
	BaseBranch     string
	MaxAttempts    int
	BackoffSeconds []int
	Provider       string
	Model          string
	Effort         string
	Stderr         io.Writer
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
	Stderr          io.Writer
}

type DispatchResult struct {
	OK          bool   `json:"ok"`
	Issue       int    `json:"issue"`
	Branch      string `json:"branch"`
	RunID       string `json:"run_id"`
	PR          string `json:"pr"`
	Summary     string `json:"summary"`
	AttemptPath string `json:"attempt_path"`
	Status      string `json:"status"`
	ExitCode    int    `json:"exit_code"`
	LogBytes    int64  `json:"log_bytes"`
}

type Result struct {
	Action         Action
	Report         string
	DispatchResult *DispatchResult
}

type PullRequestReader interface {
	ListHeadPRs(ctx context.Context, branch string) ([]gh.PullRequestReference, error)
	ListOpenPRs(ctx context.Context) ([]gh.PullRequest, error)
}

type DispatchFunc func(ctx context.Context, opts DispatchOptions) (DispatchResult, error)
type SleepFunc func(ctx context.Context, duration time.Duration) error

type Deps struct {
	GitHub       func(repoPath string) PullRequestReader
	LoadAttempts func(repoPath, runID string) ([]state.Attempt, error)
	ReadFile     func(path string) ([]byte, error)
	Sleep        SleepFunc
	Dispatch     DispatchFunc
}

type issuePR struct {
	Number      int
	URL         string
	HeadRefName string
}

type attemptHistoryEntry struct {
	Record              state.Attempt
	RecoveryContextPath string
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
		LoadAttempts: state.LoadAttempts,
		ReadFile:     os.ReadFile,
		Sleep:        SleepContext,
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
		opts.BaseBranch = "main"
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 3
	}
	if opts.MaxAttempts < 0 {
		return Result{}, errors.New("max attempts must be non-negative")
	}
	if opts.BackoffSeconds == nil {
		opts.BackoffSeconds = []int{10, 30, 120}
	}
	for _, seconds := range opts.BackoffSeconds {
		if seconds < 0 {
			return Result{}, errors.New("backoff seconds must be non-negative")
		}
	}
	if strings.TrimSpace(opts.Provider) == "" {
		opts.Provider = "codex"
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

	allAttempts, err := deps.LoadAttempts(repoPath, opts.RunID)
	if err != nil {
		return Result{}, err
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

	var adopted *issuePR
	if deps.GitHub != nil {
		adopted = findOpenIssuePR(ctx, deps.GitHub(repoPath), opts.IssueNumber, attempts, opts.MaxAttempts)
	}
	if adopted != nil {
		return Result{
			Action: ActionAdopt,
			Report: renderAdoptReport(opts.IssueNumber, opts.RunID, priorAttempts, latestStatus, *adopted),
		}, nil
	}

	if priorAttempts >= opts.MaxAttempts {
		return Result{
			Action: ActionBlocked,
			Report: renderBlockedReport(opts.IssueNumber, opts.RunID, priorAttempts, opts.MaxAttempts, latestStatus, latestBriefPath, latestBriefText, attempts),
		}, nil
	}

	if deps.Dispatch == nil {
		return Result{}, errors.New("dispatch is not configured")
	}
	nextAttempt := priorAttempts + 1
	retryBranch := fmt.Sprintf("loop/issue-%d-retry-%d", opts.IssueNumber, nextAttempt)
	backoffSeconds := selectBackoffSeconds(priorAttempts, opts.BackoffSeconds)
	report := renderRetryReport(opts.IssueNumber, opts.RunID, priorAttempts, latestStatus, latestBriefPath, retryBranch, backoffSeconds)

	if err := deps.Sleep(ctx, time.Duration(backoffSeconds)*time.Second); err != nil {
		return Result{Action: ActionRetry, Report: report}, err
	}
	dispatchResult, err := deps.Dispatch(ctx, DispatchOptions{
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
		Model:           opts.Model,
		Effort:          opts.Effort,
		Stderr:          opts.Stderr,
	})
	if err != nil {
		return Result{Action: ActionRetry, Report: report}, err
	}
	data, err := json.Marshal(dispatchResult)
	if err != nil {
		return Result{Action: ActionRetry, Report: report}, err
	}
	report += string(data) + "\n"
	return Result{
		Action:         ActionRetry,
		Report:         report,
		DispatchResult: &dispatchResult,
	}, nil
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

func renderBlockedReport(issueNumber int, runID string, priorAttempts, maxAttempts int, latestStatus, latestBriefPath, latestBriefText string, attempts []attemptHistoryEntry) string {
	var out bytes.Buffer
	fmt.Fprintln(&out, "BLOCKED: retry limit reached")
	fmt.Fprintf(&out, "Issue: #%d\n", issueNumber)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	fmt.Fprintf(&out, "Prior attempts: %d\n", priorAttempts)
	fmt.Fprintf(&out, "Max attempts: %d\n", maxAttempts)
	fmt.Fprintf(&out, "Latest status: %s\n", latestStatus)
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

func renderRetryReport(issueNumber int, runID string, priorAttempts int, latestStatus, latestBriefPath, retryBranch string, backoffSeconds int) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "RETRY: dispatching issue #%d attempt %d\n", issueNumber, priorAttempts+1)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	fmt.Fprintf(&out, "Prior attempts: %d\n", priorAttempts)
	fmt.Fprintf(&out, "Latest status: %s\n", latestStatus)
	fmt.Fprintf(&out, "Latest recovery brief: %s\n", latestBriefPath)
	fmt.Fprintf(&out, "Retry branch: %s\n", retryBranch)
	fmt.Fprintf(&out, "Backoff seconds: %d\n", backoffSeconds)
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
