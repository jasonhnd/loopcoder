package orchestration

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

type GitHubReader interface {
	RepoName(ctx context.Context) (string, error)
	ListIssues(ctx context.Context, state string) ([]gh.Issue, error)
	ViewIssue(ctx context.Context, number int) (gh.Issue, error)
	ListOpenPRs(ctx context.Context) ([]gh.PullRequest, error)
	PRChecks(ctx context.Context, number int) ([]gh.Check, error)
}

type ProcessAliveFunc func(pid int) bool

type Options struct {
	Reader        GitHubReader
	RepoPath      string
	BaseBranch    string
	RunID         string
	IncludeClosed bool
	Attempts      []state.Attempt
	Thresholds    config.ResilienceWorker
	ProcessAlive  ProcessAliveFunc
	Now           time.Time
}

type checkedPR struct {
	PR           gh.PullRequest
	IssueNumbers []int
	CheckSummary checkSummary
}

type checkSummary struct {
	State string
	Text  string
}

type dependencyStatus struct {
	Number    int
	Title     string
	State     string
	Completed bool
	Reason    string
}

type attemptDisposition struct {
	Classification string
	Reason         string
}

func ComputeReadySet(ctx context.Context, opts Options) (report.ReadySetReport, error) {
	if opts.Reader == nil {
		return report.ReadySetReport{}, fmt.Errorf("github reader is required")
	}
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = "main"
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	} else {
		opts.Now = opts.Now.UTC()
	}
	opts.Thresholds = withThresholdDefaults(opts.Thresholds)
	if opts.ProcessAlive == nil {
		opts.ProcessAlive = func(int) bool { return false }
	}

	repoName, err := opts.Reader.RepoName(ctx)
	if err != nil || strings.TrimSpace(repoName) == "" {
		repoName = opts.RepoPath
	}

	issueState := "open"
	if opts.IncludeClosed {
		issueState = "all"
	}
	issues, err := opts.Reader.ListIssues(ctx, issueState)
	if err != nil {
		return report.ReadySetReport{}, fmt.Errorf("list GitHub issues: %w", err)
	}
	normalizeIssues(issues, opts.IncludeClosed)

	dependencyNumbers := collectDependencyNumbers(issues)
	dependencyCache := make(map[int]dependencyStatus, len(dependencyNumbers))
	for _, number := range dependencyNumbers {
		dependencyCache[number] = loadDependencyStatus(ctx, opts.Reader, number)
	}

	openPRs, err := opts.Reader.ListOpenPRs(ctx)
	if err != nil {
		return report.ReadySetReport{}, fmt.Errorf("list open GitHub PRs: %w", err)
	}
	checkedPRs := make([]checkedPR, 0, len(openPRs))
	for _, pr := range openPRs {
		checks, checkErr := opts.Reader.PRChecks(ctx, pr.Number)
		checkedPRs = append(checkedPRs, checkedPR{
			PR:           pr,
			IssueNumbers: prIssueNumbers(pr),
			CheckSummary: summarizeChecks(checks, checkErr),
		})
	}

	attemptsByIssue := groupAttemptsByIssue(opts.Attempts)
	prsByIssue, prsByBranch := groupPRs(checkedPRs)

	sort.Slice(issues, func(i, j int) bool {
		return issues[i].Number < issues[j].Number
	})

	ready := make([]report.ReadyIssue, 0)
	blocked := make([]report.BlockedIssue, 0)
	for _, issue := range issues {
		number := issue.Number
		issueAttempts := attemptsByIssue[number]
		latest := latestAttempt(issueAttempts)
		candidateBranches := candidateBranches(number, issueAttempts)
		matchingPRs := matchingPRsForIssue(number, candidateBranches, prsByIssue, prsByBranch)
		dependencies := blockedDependencyNumbers(labelNames(issue.Labels))
		var unsatisfied []dependencyStatus
		for _, dep := range dependencies {
			status, ok := dependencyCache[dep]
			if !ok {
				status = loadDependencyStatus(ctx, opts.Reader, dep)
				dependencyCache[dep] = status
			}
			if !status.Completed {
				unsatisfied = append(unsatisfied, status)
			}
		}

		attemptSummaries := attemptSummaries(opts.RepoPath, issueAttempts)
		prSummaries := prSummaries(matchingPRs, latest)

		if strings.ToUpper(issue.State) != "OPEN" {
			if opts.IncludeClosed {
				blocked = append(blocked, report.BlockedIssue{
					Issue:          number,
					Title:          issue.Title,
					Classification: "closed",
					Reason:         fmt.Sprintf("issue state is %s; ready-set dispatch candidates must be open", issue.State),
					Dependencies:   dependencies,
					OpenPRs:        prSummaries,
					Attempts:       attemptSummaries,
				})
			}
			continue
		}

		if len(unsatisfied) > 0 {
			blocked = append(blocked, report.BlockedIssue{
				Issue:          number,
				Title:          issue.Title,
				Classification: "blocked-by-unmerged-dep",
				Reason:         dependencyReason(unsatisfied),
				Dependencies:   dependencies,
				OpenPRs:        prSummaries,
				Attempts:       attemptSummaries,
			})
			continue
		}

		if len(matchingPRs) > 0 {
			sort.Slice(matchingPRs, func(i, j int) bool {
				return matchingPRs[i].PR.Number < matchingPRs[j].PR.Number
			})
			primary := matchingPRs[0]
			subState := prSubState(primary, latest)
			reason := fmt.Sprintf("open PR #%d exists for %s", primary.PR.Number, primary.PR.HeadRefName)
			if subState == "adopt-PR" {
				reason = fmt.Sprintf("open PR #%d exists for %s; local attempt should adopt it", primary.PR.Number, primary.PR.HeadRefName)
			}
			blocked = append(blocked, report.BlockedIssue{
				Issue:          number,
				Title:          issue.Title,
				Classification: "has-open-PR",
				Reason:         reason,
				Dependencies:   dependencies,
				OpenPRs:        prSummaries,
				Attempts:       attemptSummaries,
			})
			continue
		}

		if latest != nil {
			if disposition := localAttemptDisposition(*latest, opts.Thresholds, opts.ProcessAlive, opts.Now); disposition != nil {
				blocked = append(blocked, report.BlockedIssue{
					Issue:          number,
					Title:          issue.Title,
					Classification: disposition.Classification,
					Reason:         disposition.Reason,
					Dependencies:   dependencies,
					OpenPRs:        prSummaries,
					Attempts:       attemptSummaries,
				})
				continue
			}
		}

		ready = append(ready, report.ReadyIssue{
			Issue:  number,
			Title:  issue.Title,
			Reason: "dependencies completed; no open PR; no live local attempt",
		})
	}

	runID := runIDPtr(opts.RunID)
	return report.ReadySetReport{
		Version:     1,
		Repo:        repoName,
		RepoPath:    filepath.ToSlash(opts.RepoPath),
		BaseBranch:  opts.BaseBranch,
		RunID:       runID,
		GeneratedAt: state.FormatTimestamp(opts.Now),
		Ready:       ready,
		Blocked:     blocked,
		Summary:     summarizeReadySet(ready, blocked),
	}, nil
}

func withThresholdDefaults(worker config.ResilienceWorker) config.ResilienceWorker {
	defaults := config.Default().Resilience.Worker
	if worker.HeartbeatIntervalSeconds <= 0 {
		worker.HeartbeatIntervalSeconds = defaults.HeartbeatIntervalSeconds
	}
	if worker.StaleAfterSeconds <= 0 {
		worker.StaleAfterSeconds = defaults.StaleAfterSeconds
	}
	if worker.HungAfterSeconds <= 0 {
		worker.HungAfterSeconds = defaults.HungAfterSeconds
	}
	return worker
}

func normalizeIssues(issues []gh.Issue, includeClosed bool) {
	for i := range issues {
		if strings.TrimSpace(issues[i].State) == "" {
			if includeClosed {
				issues[i].State = "UNKNOWN"
			} else {
				issues[i].State = "OPEN"
			}
		}
	}
}

func labelNames(labels []gh.Label) []string {
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		if strings.TrimSpace(label.Name) != "" {
			names = append(names, label.Name)
		}
	}
	return names
}

var blockedByPattern = regexp.MustCompile(`(?i)^blocked-by:\s*#(\d+)$`)

func blockedDependencyNumbers(labels []string) []int {
	seen := map[int]bool{}
	for _, label := range labels {
		matches := blockedByPattern.FindStringSubmatch(label)
		if len(matches) != 2 {
			continue
		}
		number, err := strconv.Atoi(matches[1])
		if err == nil && number > 0 {
			seen[number] = true
		}
	}
	return sortedInts(seen)
}

func collectDependencyNumbers(issues []gh.Issue) []int {
	seen := map[int]bool{}
	for _, issue := range issues {
		for _, number := range blockedDependencyNumbers(labelNames(issue.Labels)) {
			seen[number] = true
		}
	}
	return sortedInts(seen)
}

func sortedInts(seen map[int]bool) []int {
	out := make([]int, 0, len(seen))
	for number := range seen {
		out = append(out, number)
	}
	sort.Ints(out)
	return out
}

func loadDependencyStatus(ctx context.Context, reader GitHubReader, number int) dependencyStatus {
	issue, err := reader.ViewIssue(ctx, number)
	if err != nil {
		return dependencyStatus{
			Number: number,
			State:  "UNKNOWN",
			Reason: fmt.Sprintf("#%d state is unknown", number),
		}
	}

	stateValue := strings.ToUpper(issue.State)
	stateReason := strings.ToUpper(issue.StateReason)
	closedAsCompleted := stateValue == "CLOSED" && stateReason == "COMPLETED"
	hasClosingPRRef := len(issue.ClosedByPullRequestsReferences) > 0
	completed := closedAsCompleted || hasClosingPRRef

	reason := ""
	switch {
	case closedAsCompleted:
		reason = fmt.Sprintf("#%d is closed as completed", number)
	case hasClosingPRRef:
		reason = fmt.Sprintf("#%d has a closing PR reference", number)
	case stateValue == "UNKNOWN":
		reason = fmt.Sprintf("#%d state is unknown", number)
	case stateValue == "OPEN":
		reason = fmt.Sprintf("#%d is still open", number)
	case stateValue == "CLOSED" && strings.TrimSpace(issue.StateReason) == "":
		reason = fmt.Sprintf("#%d is closed without a completed state reason", number)
	case stateValue == "CLOSED":
		reason = fmt.Sprintf("#%d is closed as %s", number, issue.StateReason)
	default:
		reason = fmt.Sprintf("#%d state is %s", number, issue.State)
	}

	return dependencyStatus{
		Number:    number,
		Title:     issue.Title,
		State:     issue.State,
		Completed: completed,
		Reason:    reason,
	}
}

var branchIssuePattern = regexp.MustCompile(`(?i)(?:^|[/_-])issue[/_-]?(\d+)(?:\D|$)`)
var hashIssuePattern = regexp.MustCompile(`#(\d+)`)

func prIssueNumbers(pr gh.PullRequest) []int {
	seen := map[int]bool{}
	for _, ref := range pr.ClosingIssuesReferences {
		if ref.Number > 0 {
			seen[ref.Number] = true
		}
	}
	if number := issueNumberFromText(pr.HeadRefName); number > 0 {
		seen[number] = true
	}
	if number := issueNumberFromText(pr.Title); number > 0 {
		seen[number] = true
	}
	return sortedInts(seen)
}

func issueNumberFromText(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	if matches := branchIssuePattern.FindStringSubmatch(text); len(matches) == 2 {
		number, _ := strconv.Atoi(matches[1])
		return number
	}
	if matches := hashIssuePattern.FindStringSubmatch(text); len(matches) == 2 {
		number, _ := strconv.Atoi(matches[1])
		return number
	}
	return 0
}

func summarizeChecks(checks []gh.Check, err error) checkSummary {
	if err != nil {
		return checkSummary{State: "unknown", Text: "checks unavailable"}
	}
	if len(checks) == 0 {
		return checkSummary{State: "none", Text: "no checks reported"}
	}

	counts := map[string]int{}
	for _, check := range checks {
		bucket := strings.TrimSpace(check.Bucket)
		if bucket == "" {
			bucket = strings.TrimSpace(check.State)
		}
		if bucket == "" {
			bucket = "unknown"
		}
		counts[bucket]++
	}

	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}

	status := "unknown"
	if counts["fail"]+counts["cancel"] > 0 {
		status = "fail"
	} else if counts["pending"] > 0 {
		status = "pending"
	} else if counts["pass"]+counts["skipping"] > 0 {
		status = "pass"
	}
	return checkSummary{State: status, Text: strings.Join(parts, ", ")}
}

func groupAttemptsByIssue(attempts []state.Attempt) map[int][]state.Attempt {
	out := map[int][]state.Attempt{}
	for _, attempt := range attempts {
		if attempt.Issue <= 0 {
			continue
		}
		out[attempt.Issue] = append(out[attempt.Issue], attempt)
	}
	for issue := range out {
		sort.Slice(out[issue], func(i, j int) bool {
			if out[issue][i].Attempt != out[issue][j].Attempt {
				return out[issue][i].Attempt < out[issue][j].Attempt
			}
			if !out[issue][i].LastWriteUTC.Equal(out[issue][j].LastWriteUTC) {
				return out[issue][i].LastWriteUTC.Before(out[issue][j].LastWriteUTC)
			}
			return out[issue][i].JobID < out[issue][j].JobID
		})
	}
	return out
}

func groupPRs(prs []checkedPR) (map[int][]checkedPR, map[string]checkedPR) {
	byIssue := map[int][]checkedPR{}
	byBranch := map[string]checkedPR{}
	for _, pr := range prs {
		if strings.TrimSpace(pr.PR.HeadRefName) != "" {
			byBranch[pr.PR.HeadRefName] = pr
		}
		for _, number := range pr.IssueNumbers {
			byIssue[number] = append(byIssue[number], pr)
		}
	}
	return byIssue, byBranch
}

func latestAttempt(attempts []state.Attempt) *state.Attempt {
	if len(attempts) == 0 {
		return nil
	}
	latest := attempts[0]
	for _, attempt := range attempts[1:] {
		if attempt.Attempt > latest.Attempt ||
			(attempt.Attempt == latest.Attempt && attempt.LastWriteUTC.After(latest.LastWriteUTC)) {
			latest = attempt
		}
	}
	return &latest
}

func candidateBranches(issueNumber int, attempts []state.Attempt) []string {
	seen := map[string]bool{
		fmt.Sprintf("loop/issue-%d", issueNumber): true,
	}
	for _, attempt := range attempts {
		if strings.TrimSpace(attempt.Branch) != "" {
			seen[attempt.Branch] = true
		}
		if attempt.Attempt > 1 {
			seen[fmt.Sprintf("loop/issue-%d-retry-%d", issueNumber, attempt.Attempt)] = true
		}
	}
	branches := make([]string, 0, len(seen))
	for branch := range seen {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	return branches
}

func matchingPRsForIssue(issueNumber int, branches []string, byIssue map[int][]checkedPR, byBranch map[string]checkedPR) []checkedPR {
	seen := map[int]checkedPR{}
	for _, pr := range byIssue[issueNumber] {
		seen[pr.PR.Number] = pr
	}
	for _, branch := range branches {
		if pr, ok := byBranch[branch]; ok {
			seen[pr.PR.Number] = pr
		}
	}
	out := make([]checkedPR, 0, len(seen))
	for _, pr := range seen {
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PR.Number < out[j].PR.Number
	})
	return out
}

func attemptSummaries(repoPath string, attempts []state.Attempt) []report.AttemptSummary {
	out := make([]report.AttemptSummary, 0, len(attempts))
	for _, attempt := range attempts {
		out = append(out, report.AttemptSummary{
			JobID:          attempt.JobID,
			Attempt:        attempt.Attempt,
			Status:         attempt.Status,
			Phase:          attempt.Phase,
			PID:            attempt.PID,
			Branch:         attempt.Branch,
			Path:           repoRelativePath(repoPath, attempt.Path),
			HeartbeatAt:    attempt.HeartbeatAt,
			LastProgressAt: attempt.LastProgressAt,
		})
	}
	return out
}

func prSummaries(prs []checkedPR, latest *state.Attempt) []report.OpenPRSummary {
	out := make([]report.OpenPRSummary, 0, len(prs))
	for _, pr := range prs {
		out = append(out, report.OpenPRSummary{
			Number:   pr.PR.Number,
			URL:      pr.PR.URL,
			Head:     pr.PR.HeadRefName,
			SubState: prSubState(pr, latest),
		})
	}
	return out
}

func prSubState(pr checkedPR, latest *state.Attempt) string {
	if latest != nil && strings.ToLower(latest.Status) == "running" {
		return "adopt-PR"
	}
	if pr.PR.IsDraft {
		return "gated"
	}
	if pr.CheckSummary.State == "fail" {
		return "fixing"
	}
	if pr.CheckSummary.State == "pending" || pr.CheckSummary.State == "unknown" {
		return "gated"
	}
	return "in-review"
}

func dependencyReason(unsatisfied []dependencyStatus) string {
	if len(unsatisfied) == 1 {
		return "blocked by " + unsatisfied[0].Reason
	}
	parts := make([]string, 0, len(unsatisfied))
	for _, dep := range unsatisfied {
		parts = append(parts, dep.Reason)
	}
	return "blocked by dependencies: " + strings.Join(parts, "; ")
}

func localAttemptDisposition(attempt state.Attempt, thresholds config.ResilienceWorker, alive ProcessAliveFunc, now time.Time) *attemptDisposition {
	status := strings.ToLower(strings.TrimSpace(attempt.Status))
	phase := strings.ToLower(strings.TrimSpace(attempt.Phase))
	heartbeatAge, hasHeartbeatAge := ageSeconds(now, attempt.HeartbeatAt)
	progressAge, hasProgressAge := ageSeconds(now, attempt.LastProgressAt)
	heartbeatFreshSeconds := float64(thresholds.HeartbeatIntervalSeconds * 2)
	heartbeatFresh := hasHeartbeatAge && heartbeatAge <= heartbeatFreshSeconds
	heartbeatStale := hasHeartbeatAge && heartbeatAge > heartbeatFreshSeconds
	progressStale := hasProgressAge && progressAge > float64(thresholds.StaleAfterSeconds)
	progressHung := hasProgressAge && progressAge > float64(thresholds.HungAfterSeconds)
	pidAlive := attempt.PID != nil && alive(*attempt.PID)

	switch status {
	case "failed", "failure", "error", "canceled", "cancelled", "timeout", "timed_out":
		return &attemptDisposition{
			Classification: "recovery-needed",
			Reason:         fmt.Sprintf("latest attempt %s failed with status '%s'", attempt.JobID, attempt.Status),
		}
	case "succeeded", "success", "completed", "complete", "done":
		return &attemptDisposition{
			Classification: "recovery-needed",
			Reason:         fmt.Sprintf("latest attempt %s completed locally but no closed issue or open PR was found", attempt.JobID),
		}
	case "idle":
		return &attemptDisposition{
			Classification: "recovery-needed",
			Reason:         fmt.Sprintf("latest attempt %s is idle and needs recovery review", attempt.JobID),
		}
	}
	if strings.Contains(phase, "idle") || strings.Contains(phase, "waiting") {
		return &attemptDisposition{
			Classification: "recovery-needed",
			Reason:         fmt.Sprintf("latest attempt %s is idle and needs recovery review", attempt.JobID),
		}
	}

	if status == "running" || status == "" {
		if progressHung {
			if pidAlive {
				return &attemptDisposition{
					Classification: "has-live-attempt",
					Reason:         fmt.Sprintf("latest attempt %s is hung but pid is still alive", attempt.JobID),
				}
			}
			return &attemptDisposition{
				Classification: "recovery-needed",
				Reason:         fmt.Sprintf("latest attempt %s is hung with no live pid", attempt.JobID),
			}
		}
		if progressStale {
			return &attemptDisposition{
				Classification: "has-live-attempt",
				Reason:         fmt.Sprintf("latest attempt %s progress is stale", attempt.JobID),
			}
		}
		if heartbeatFresh {
			return &attemptDisposition{
				Classification: "has-live-attempt",
				Reason:         fmt.Sprintf("latest attempt %s is running with a fresh heartbeat", attempt.JobID),
			}
		}
		if pidAlive {
			return &attemptDisposition{
				Classification: "has-live-attempt",
				Reason:         fmt.Sprintf("latest attempt %s has a live pid", attempt.JobID),
			}
		}
		if heartbeatStale || !hasHeartbeatAge {
			return &attemptDisposition{
				Classification: "recovery-needed",
				Reason:         fmt.Sprintf("latest attempt %s is orphaned with no fresh heartbeat or live pid", attempt.JobID),
			}
		}
	}

	return &attemptDisposition{
		Classification: "recovery-needed",
		Reason:         fmt.Sprintf("latest attempt %s has status '%s' and needs recovery review", attempt.JobID, attempt.Status),
	}
}

func ageSeconds(now time.Time, timestamp string) (float64, bool) {
	if strings.TrimSpace(timestamp) == "" {
		return 0, false
	}
	parsed, err := state.ParseTimestamp(timestamp)
	if err != nil {
		return 0, false
	}
	seconds := now.Sub(parsed).Seconds()
	if seconds < 0 {
		seconds = 0
	}
	return seconds, true
}

func repoRelativePath(repoPath, path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return filepath.ToSlash(absPath)
	}
	rel, err := filepath.Rel(absRepo, absPath)
	if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(absPath)
}

func summarizeReadySet(ready []report.ReadyIssue, blocked []report.BlockedIssue) report.ReadySetSummary {
	summary := report.ReadySetSummary{
		ReadyCount:   len(ready),
		BlockedCount: len(blocked),
	}
	for _, item := range blocked {
		switch item.Classification {
		case "blocked-by-unmerged-dep":
			summary.BlockedByUnmergedDepCount++
		case "has-open-PR":
			summary.HasOpenPRCount++
		case "has-live-attempt":
			summary.HasLiveAttemptCount++
		case "recovery-needed":
			summary.RecoveryNeededCount++
		}
	}
	return summary
}

func runIDPtr(runID string) *string {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(runID)
	return &trimmed
}
