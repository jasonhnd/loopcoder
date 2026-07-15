package orchestration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/providerreconcile"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

type ResumeOptions struct {
	Reader       GitHubReader
	RepoPath     string
	BaseBranch   string
	RunID        string
	RunNote      string
	Attempts     []state.Attempt
	EventCount   int
	Thresholds   config.ResilienceWorker
	ProcessAlive ProcessAliveFunc
	Now          time.Time
}

type resumeAction struct {
	Classification      string
	ActionKind          string
	Action              string
	HeartbeatAge        *float64
	ProgressAge         *float64
	PIDEvidence         string
	RecoveryContextPath string
}

func ComputeResume(ctx context.Context, opts ResumeOptions) (report.ResumeReport, error) {
	if opts.Reader == nil {
		return report.ResumeReport{}, fmt.Errorf("github reader is required")
	}
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = lcdefaults.BaseBranch
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

	openIssues, err := opts.Reader.ListIssues(ctx, "open")
	if err != nil {
		return report.ResumeReport{}, fmt.Errorf("list GitHub issues: %w", err)
	}
	normalizeIssues(openIssues, false)
	issueMap := make(map[int]gh.Issue, len(openIssues))
	for _, issue := range openIssues {
		issue.State = "OPEN"
		issueMap[issue.Number] = issue
	}

	openPRs, err := opts.Reader.ListOpenPRs(ctx)
	if err != nil {
		return report.ResumeReport{}, fmt.Errorf("list open GitHub PRs: %w", err)
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

	candidateIssueNumbers := resumeCandidateIssueNumbers(openIssues, checkedPRs, opts.Attempts)
	for _, number := range candidateIssueNumbers {
		if _, ok := issueMap[number]; ok {
			continue
		}
		issue, err := opts.Reader.ViewIssue(ctx, number)
		if err != nil {
			issue = gh.Issue{
				Number: number,
				Title:  "(issue details unavailable)",
				State:  "UNKNOWN",
			}
		}
		if issue.Number == 0 {
			issue.Number = number
		}
		if strings.TrimSpace(issue.Title) == "" {
			issue.Title = "(issue details unavailable)"
		}
		if strings.TrimSpace(issue.State) == "" {
			issue.State = "UNKNOWN"
		}
		issueMap[number] = issue
	}

	attemptsByIssue := groupAttemptsByIssue(opts.Attempts)
	prsByIssue, prsByBranch := groupPRs(checkedPRs)
	frozenByIssue, err := guardrails.FrozenIssues(opts.RepoPath, opts.BaseBranch)
	if err != nil {
		return report.ResumeReport{}, fmt.Errorf("read guardrail ledger: %w", err)
	}

	issues := make([]report.ResumeIssue, 0, len(candidateIssueNumbers))
	for _, number := range candidateIssueNumbers {
		issue := issueMap[number]
		issueAttempts := attemptsByIssue[number]
		latest := latestAttempt(issueAttempts)
		candidateBranches := candidateBranches(number, issueAttempts)
		matchingPRs := matchingPRsForIssue(number, candidateBranches, prsByIssue, prsByBranch)
		var primaryPR *checkedPR
		if len(matchingPRs) > 0 {
			primary := matchingPRs[0]
			primaryPR = &primary
		}

		action := classifyResumeIssue(issue, latest, primaryPR, candidateBranches, opts.Thresholds, opts.ProcessAlive, opts.Now)
		if latest != nil && primaryPR == nil {
			if reconcileAction, ok := resumeProviderReconcileAction(ctx, opts, number, issueAttempts); ok {
				action = reconcileAction
			}
		}
		labels := labelNames(issue.Labels)
		evidence := resumeEvidenceLines(
			opts.RepoPath,
			issue,
			latest,
			primaryPR,
			len(matchingPRs),
			action.HeartbeatAge,
			action.ProgressAge,
			action.PIDEvidence,
		)
		if strings.HasPrefix(action.Classification, "provider-") || action.Classification == "needs-human" {
			if strings.TrimSpace(action.Action) != "" && strings.Contains(action.Action, providerreconcile.SchemaVersion) {
				evidence = append(evidence, action.Action)
			}
		}
		if frozen, ok := frozenByIssue[number]; ok && action.Classification != "done" && primaryPR == nil {
			action = resumeAction{
				Classification:      "guardrail-frozen",
				ActionKind:          "blocked",
				Action:              frozen.Message,
				PIDEvidence:         "pid: n/a",
				RecoveryContextPath: frozen.RecoveryContextPath,
			}
			evidence = append(evidence,
				fmt.Sprintf("guardrail: %s", frozen.Reason),
				fmt.Sprintf("no-progress waves=%d, attempts=%d", frozen.Observed.NoProgressWaves, frozen.Observed.NoProgressAttempts),
				fmt.Sprintf("last material progress=%s", firstNonEmpty(frozen.LastMaterialProgressAt, "unknown")),
			)
			if strings.TrimSpace(frozen.RecoveryContextPath) != "" {
				evidence = append(evidence, "recovery: "+filepath.ToSlash(frozen.RecoveryContextPath))
			}
		}
		recoveryDecision := resumeIssueRecoveryDecision(opts.RepoPath, opts.RunID, number, latest, primaryPR, action)
		issues = append(issues, report.ResumeIssue{
			Issue:            number,
			Title:            issue.Title,
			State:            normalizeIssueState(issue.State),
			Labels:           labels,
			Classification:   action.Classification,
			ActionKind:       action.ActionKind,
			Action:           action.Action,
			Evidence:         evidence,
			RecoveryDecision: recoveryDecision,
		})
	}
	runTree := loadResumeRunTree(opts.RepoPath, opts.RunID)

	return report.ResumeReport{
		Version:     1,
		Repo:        repoName,
		RepoPath:    filepath.ToSlash(opts.RepoPath),
		BaseBranch:  opts.BaseBranch,
		RunID:       runIDPtr(opts.RunID),
		RunNote:     opts.RunNote,
		GeneratedAt: state.FormatTimestamp(opts.Now),
		GitHub: report.ResumeGitHubSnapshot{
			OpenIssueCount: len(openIssues),
			OpenPRCount:    len(openPRs),
		},
		Local: report.ResumeLocalState{
			AttemptCount: len(opts.Attempts),
			EventCount:   opts.EventCount,
		},
		Thresholds: report.ResumeThresholds{
			HeartbeatFreshSeconds: opts.Thresholds.HeartbeatIntervalSeconds * 2,
			StaleAfterSeconds:     opts.Thresholds.StaleAfterSeconds,
			HungAfterSeconds:      opts.Thresholds.HungAfterSeconds,
		},
		RunTree: runTree,
		Issues:  issues,
	}, nil
}

func resumeProviderReconcileAction(ctx context.Context, opts ResumeOptions, issue int, attempts []state.Attempt) (resumeAction, bool) {
	decision := providerreconcile.Check(ctx, providerreconcile.Options{
		RepoPath: opts.RepoPath,
		RunID:    opts.RunID,
		Issue:    issue,
		Attempts: attempts,
		Now:      opts.Now,
	})
	if !decision.BlockRedispatch {
		return resumeAction{}, false
	}
	classification := "provider-reconciliation"
	actionKind := "blocked"
	if decision.NeedsHuman {
		classification = "needs-human"
	}
	if decision.Action == providerreconcile.ActionObserve || decision.Action == providerreconcile.ActionReconcile {
		classification = "running"
	}
	return resumeAction{
		Classification: classification,
		ActionKind:     actionKind,
		Action:         decision.Summary() + "; receipt=" + decision.JSONLine(),
		PIDEvidence:    fmt.Sprintf("pid %d provider-authority state=%s", decision.ProviderPID, decision.AuthorityState),
	}, true
}

func resumeCandidateIssueNumbers(openIssues []gh.Issue, checkedPRs []checkedPR, attempts []state.Attempt) []int {
	seen := map[int]bool{}
	for _, issue := range openIssues {
		if issue.Number > 0 {
			seen[issue.Number] = true
		}
	}
	for _, attempt := range attempts {
		if attempt.Issue > 0 {
			seen[attempt.Issue] = true
		}
	}
	for _, pr := range checkedPRs {
		for _, number := range pr.IssueNumbers {
			if number > 0 {
				seen[number] = true
			}
		}
	}
	return sortedInts(seen)
}

func classifyResumeIssue(
	issue gh.Issue,
	latest *state.Attempt,
	primaryPR *checkedPR,
	candidateBranches []string,
	thresholds config.ResilienceWorker,
	alive ProcessAliveFunc,
	now time.Time,
) resumeAction {
	if resumeIssueDone(issue) {
		return resumeAction{
			Classification: "done",
			ActionKind:     "none",
			Action:         "done on GitHub; no local recovery needed",
			PIDEvidence:    "pid: n/a",
		}
	}

	if primaryPR != nil {
		classification := resumePRClassification(*primaryPR)
		action := fmt.Sprintf("open PR #%d exists; do not dispatch a duplicate worker", primaryPR.PR.Number)
		if latest != nil && strings.EqualFold(strings.TrimSpace(latest.Status), "running") &&
			stringSliceContains(candidateBranches, primaryPR.PR.HeadRefName) {
			classification = "adopt-PR"
			action = fmt.Sprintf("adopt open PR #%d; do not dispatch a duplicate worker", primaryPR.PR.Number)
		}
		return resumeAction{
			Classification: classification,
			ActionKind:     "blocked",
			Action:         action,
			PIDEvidence:    "pid: n/a",
		}
	}

	if latest != nil {
		return classifyResumeAttempt(*latest, thresholds, alive, now)
	}

	blockedLabels := resumeBlockedLabels(labelNames(issue.Labels))
	if len(blockedLabels) > 0 {
		return resumeAction{
			Classification: "needs-inspection",
			ActionKind:     "blocked",
			Action:         "blocked by labels: " + strings.Join(blockedLabels, ", "),
			PIDEvidence:    "pid: n/a",
		}
	}
	if strings.EqualFold(strings.TrimSpace(issue.State), "OPEN") {
		return resumeAction{
			Classification: "ready",
			ActionKind:     "ready",
			Action:         "ready to dispatch; no open PR or live local attempt found",
			PIDEvidence:    "pid: n/a",
		}
	}
	return resumeAction{
		Classification: "needs-inspection",
		ActionKind:     "blocked",
		Action:         fmt.Sprintf("issue state is %s without completed state reason or closing PR reference", normalizeIssueState(issue.State)),
		PIDEvidence:    "pid: n/a",
	}
}

func resumeIssueDone(issue gh.Issue) bool {
	stateValue := strings.ToUpper(strings.TrimSpace(issue.State))
	stateReason := strings.ToUpper(strings.TrimSpace(issue.StateReason))
	return stateValue == "CLOSED" && (stateReason == "COMPLETED" || len(issue.ClosedByPullRequestsReferences) > 0)
}

func resumePRClassification(pr checkedPR) string {
	switch pr.CheckSummary.State {
	case "fail":
		return "fixing"
	case "pending", "unknown":
		return "gated"
	default:
		return "in-review"
	}
}

func classifyResumeAttempt(
	attempt state.Attempt,
	thresholds config.ResilienceWorker,
	alive ProcessAliveFunc,
	now time.Time,
) resumeAction {
	switch state.NormalizeStatus(attempt.Status) {
	case state.StatusFailed, state.StatusCancelled, state.StatusTimedOut, state.StatusAbandoned:
		return resumeAction{
			Classification: "recovery-needed",
			ActionKind:     "ready",
			Action:         fmt.Sprintf("recover or mark needs-human; latest attempt ended with status %s", state.NormalizeStatus(attempt.Status)),
			PIDEvidence:    resumePIDEvidence(attempt.PID, false),
		}
	case state.StatusNeedsHuman:
		return resumeAction{
			Classification: "needs-human",
			ActionKind:     "blocked",
			Action:         "human review required before retrying or dispatching dependents",
			PIDEvidence:    resumePIDEvidence(attempt.PID, false),
		}
	}

	heartbeatAge, hasHeartbeatAge := ageSeconds(now, attempt.HeartbeatAt)
	progressAge, hasProgressAge := ageSeconds(now, attempt.LastProgressAt)
	heartbeatFreshSeconds := float64(thresholds.HeartbeatIntervalSeconds * 2)
	heartbeatFresh := hasHeartbeatAge && heartbeatAge <= heartbeatFreshSeconds
	heartbeatStale := hasHeartbeatAge && heartbeatAge > heartbeatFreshSeconds
	progressStale := hasProgressAge && progressAge > float64(thresholds.StaleAfterSeconds)
	progressHung := hasProgressAge && progressAge > float64(thresholds.HungAfterSeconds)
	pidAlive := attempt.PID != nil && alive(*attempt.PID)

	heartbeatAgePtr := optionalAge(heartbeatAge, hasHeartbeatAge)
	progressAgePtr := optionalAge(progressAge, hasProgressAge)
	pidEvidence := resumePIDEvidence(attempt.PID, pidAlive)

	switch {
	case heartbeatFresh && pidAlive && !progressStale:
		return resumeAction{
			Classification: "running",
			ActionKind:     "blocked",
			Action:         "live local attempt exists; do not dispatch",
			HeartbeatAge:   heartbeatAgePtr,
			ProgressAge:    progressAgePtr,
			PIDEvidence:    pidEvidence,
		}
	case heartbeatFresh && progressHung:
		actionKind := "ready"
		action := "recover the hung attempt; no open PR or live pid found"
		if pidAlive {
			actionKind = "blocked"
			action = "progress is hung but pid is still alive; inspect before recovery"
		}
		return resumeAction{
			Classification: "hung",
			ActionKind:     actionKind,
			Action:         action,
			HeartbeatAge:   heartbeatAgePtr,
			ProgressAge:    progressAgePtr,
			PIDEvidence:    pidEvidence,
		}
	case heartbeatFresh && progressStale:
		return resumeAction{
			Classification: "stale",
			ActionKind:     "blocked",
			Action:         "progress is stale; inspect before deciding to recover",
			HeartbeatAge:   heartbeatAgePtr,
			ProgressAge:    progressAgePtr,
			PIDEvidence:    pidEvidence,
		}
	case heartbeatStale && !pidAlive:
		return resumeAction{
			Classification: "orphaned",
			ActionKind:     "ready",
			Action:         "recover or re-dispatch from the local recovery context; no open PR or live pid found",
			HeartbeatAge:   heartbeatAgePtr,
			ProgressAge:    progressAgePtr,
			PIDEvidence:    pidEvidence,
		}
	case progressHung:
		actionKind := "ready"
		action := "recover the hung attempt; no open PR or live pid found"
		if pidAlive {
			actionKind = "blocked"
			action = "progress is hung and pid is alive; inspect before recovery"
		}
		return resumeAction{
			Classification: "hung",
			ActionKind:     actionKind,
			Action:         action,
			HeartbeatAge:   heartbeatAgePtr,
			ProgressAge:    progressAgePtr,
			PIDEvidence:    pidEvidence,
		}
	case progressStale:
		return resumeAction{
			Classification: "stale",
			ActionKind:     "blocked",
			Action:         "progress is stale; inspect before deciding to recover",
			HeartbeatAge:   heartbeatAgePtr,
			ProgressAge:    progressAgePtr,
			PIDEvidence:    pidEvidence,
		}
	default:
		return resumeAction{
			Classification: "needs-inspection",
			ActionKind:     "blocked",
			Action:         "attempt or branch exists without an open PR; inspect before dispatching",
			HeartbeatAge:   heartbeatAgePtr,
			ProgressAge:    progressAgePtr,
			PIDEvidence:    pidEvidence,
		}
	}
}

func optionalAge(age float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return &age
}

func resumePIDEvidence(pid *int, alive bool) string {
	if pid == nil {
		return "pid missing"
	}
	if alive {
		return fmt.Sprintf("pid %d alive on this host", *pid)
	}
	return fmt.Sprintf("pid %d not found on this host", *pid)
}

func resumeBlockedLabels(labels []string) []string {
	out := make([]string, 0)
	for _, label := range labels {
		lower := strings.ToLower(label)
		if blockedByPattern.MatchString(label) ||
			strings.Contains(lower, "blocked") ||
			strings.Contains(lower, "needs-human") ||
			strings.Contains(lower, "needs:human") {
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

func resumeEvidenceLines(
	repoPath string,
	issue gh.Issue,
	latest *state.Attempt,
	primaryPR *checkedPR,
	matchingPRCount int,
	heartbeatAge *float64,
	progressAge *float64,
	pidEvidence string,
) []string {
	prEvidence := "PR: none open"
	if primaryPR != nil {
		prEvidence = fmt.Sprintf("PR: #%d %s, checks=%s", primaryPR.PR.Number, primaryPR.PR.HeadRefName, primaryPR.CheckSummary.Text)
	} else if matchingPRCount > 0 {
		prEvidence = fmt.Sprintf("PR: %d matching open PRs", matchingPRCount)
	}

	attemptEvidence := "attempt: none"
	branchEvidence := "branch: n/a"
	if latest != nil {
		sidecar := repoRelativePath(repoPath, latest.Path)
		if strings.TrimSpace(sidecar) == "" {
			sidecar = "(sidecar path unavailable)"
		}
		attemptEvidence = fmt.Sprintf(
			"attempt: job=%s, attempt=%d, status=%s, phase=%s, sidecar=%s",
			latest.JobID,
			latest.Attempt,
			latest.Status,
			latest.Phase,
			sidecar,
		)
		if strings.TrimSpace(latest.Branch) != "" {
			branchEvidence = "branch: " + latest.Branch
		}
	}

	if strings.TrimSpace(pidEvidence) == "" {
		pidEvidence = "pid: n/a"
	}
	if !strings.HasPrefix(pidEvidence, "pid") {
		pidEvidence = "pid: " + pidEvidence
	}

	return []string{
		prEvidence,
		attemptEvidence,
		pidEvidence,
		branchEvidence,
		fmt.Sprintf("heartbeat age=%s, progress age=%s", formatResumeAge(heartbeatAge), formatResumeAge(progressAge)),
		resumeClosureEvidence(issue),
	}
}

func resumeClosureEvidence(issue gh.Issue) string {
	if len(issue.ClosedByPullRequestsReferences) > 0 {
		refs := make([]string, 0, len(issue.ClosedByPullRequestsReferences))
		for _, ref := range issue.ClosedByPullRequestsReferences {
			if ref.Number > 0 {
				refs = append(refs, fmt.Sprintf("#%d", ref.Number))
			} else {
				refs = append(refs, "#?")
			}
		}
		return "closing PRs: " + strings.Join(refs, ", ")
	}
	if resumeIssueDone(issue) && strings.EqualFold(strings.TrimSpace(issue.StateReason), "COMPLETED") {
		return "closing PRs: closed as completed"
	}
	return "closing PRs: none observed"
}

func formatResumeAge(age *float64) string {
	if age == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.0fs", *age)
}

func normalizeIssueState(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "UNKNOWN"
	}
	return strings.ToUpper(value)
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func resumeIssueRecoveryDecision(repoPath, runID string, issueNumber int, latest *state.Attempt, primaryPR *checkedPR, action resumeAction) *report.ResumeRecoveryDecision {
	decision := &report.ResumeRecoveryDecision{
		Issue:        issueNumber,
		RunID:        runID,
		Action:       action.Action,
		Reason:       action.Classification,
		ContextPaths: []string{},
		PRLinks:      []string{},
	}
	if latest != nil {
		decision.ContextPaths = appendNonEmptyString(decision.ContextPaths, repoRelativePath(repoPath, latest.Path))
		recoveryPath := resumeAttemptRecoveryContextPath(repoPath, runID, *latest)
		relativeRecoveryPath := repoRelativePath(repoPath, recoveryPath)
		decision.ContextPaths = appendNonEmptyString(decision.ContextPaths, relativeRecoveryPath)
		decision.RecoveryContextPath = relativeRecoveryPath
	}
	if strings.TrimSpace(action.RecoveryContextPath) != "" {
		relativeRecoveryPath := resumeRecoveryContextReportPath(repoPath, action.RecoveryContextPath)
		decision.ContextPaths = appendNonEmptyString(decision.ContextPaths, relativeRecoveryPath)
		decision.RecoveryContextPath = relativeRecoveryPath
	}
	if primaryPR != nil {
		decision.PRLinks = appendNonEmptyString(decision.PRLinks, primaryPR.PR.URL)
		if strings.TrimSpace(primaryPR.PR.URL) == "" && primaryPR.PR.Number > 0 {
			decision.PRLinks = appendNonEmptyString(decision.PRLinks, fmt.Sprintf("#%d", primaryPR.PR.Number))
		}
	}
	switch action.Classification {
	case "done":
		decision.Outcome = "none"
	case "ready":
		decision.Outcome = "dispatch"
		decision.SafeToResume = true
	case "adopt-PR":
		decision.Outcome = "adopt"
		decision.SafeToResume = true
	case "recovery-needed", "orphaned", "hung":
		if action.ActionKind == "ready" {
			decision.Outcome = "retry"
			decision.SafeToResume = true
			decision.RetryAllowed = true
		} else {
			decision.Outcome = "inspect"
			decision.NeedsHuman = true
		}
	case "guardrail-frozen", "needs-human":
		decision.Outcome = "needs-human"
		decision.NeedsHuman = true
	case "running", "stale":
		decision.Outcome = "wait"
	default:
		if action.ActionKind == "blocked" {
			decision.Outcome = "inspect"
			decision.NeedsHuman = true
		} else {
			decision.Outcome = action.ActionKind
		}
	}
	if decision.Outcome == "" {
		decision.Outcome = "inspect"
	}
	return decision
}

func resumeAttemptRecoveryContextPath(repoPath, runID string, attempt state.Attempt) string {
	if strings.TrimSpace(attempt.RecoveryContextPath) != "" {
		if filepath.IsAbs(attempt.RecoveryContextPath) {
			return filepath.Clean(attempt.RecoveryContextPath)
		}
		return filepath.Join(repoPath, attempt.RecoveryContextPath)
	}
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(attempt.JobID) == "" {
		return ""
	}
	return state.RecoveryBriefPathForRead(repoPath, runID, attempt.JobID)
}

func resumeRecoveryContextReportPath(repoPath, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return repoRelativePath(repoPath, path)
	}
	return filepath.ToSlash(path)
}

func appendNonEmptyString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func loadResumeRunTree(repoPath, runID string) report.ResumeRunTree {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return report.ResumeRunTree{Nodes: []report.ResumeRunTreeNode{}}
	}
	rootID := runID
	if parent := resumeRunParentFromState(repoPath, runID); parent != "" {
		rootID = parent
	}
	nodesByID := map[string]*report.ResumeRunTreeNode{}
	visiting := map[string]bool{}
	collectResumeRunNode(repoPath, rootID, "", 0, nodesByID, visiting)
	nodes := make([]report.ResumeRunTreeNode, 0, len(nodesByID))
	for _, node := range nodesByID {
		sort.Strings(node.ChildRunIDs)
		nodes = append(nodes, *node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Depth != nodes[j].Depth {
			return nodes[i].Depth < nodes[j].Depth
		}
		return nodes[i].RunID < nodes[j].RunID
	})
	return report.ResumeRunTree{
		RootRunID: rootID,
		Nodes:     nodes,
	}
}

func collectResumeRunNode(repoPath, runID, parentRunID string, depth int, nodes map[string]*report.ResumeRunTreeNode, visiting map[string]bool) {
	runID = strings.TrimSpace(runID)
	if runID == "" || visiting[runID] {
		return
	}
	visiting[runID] = true
	defer delete(visiting, runID)

	lifecycle, lifecycleErr := state.LoadLifecycle(repoPath, runID)
	eventChildren, eventParent := resumeRunEdgesFromEvents(repoPath, runID)
	if strings.TrimSpace(parentRunID) == "" {
		parentRunID = firstNonEmpty(lifecycle.ParentRunID, eventParent)
	}
	children := map[string]bool{}
	for _, child := range lifecycle.ChildRunIDs {
		if child != runID {
			children[child] = true
		}
	}
	for _, child := range eventChildren {
		if child != runID {
			children[child] = true
		}
	}

	stateValue := strings.TrimSpace(string(lifecycle.State))
	source := lifecycle.Source
	if lifecycleErr != nil {
		stateValue = "unknown"
		source = "unreadable: " + lifecycleErr.Error()
	} else if stateValue == "" {
		stateValue = "unknown"
		if source == "" {
			source = "none"
		}
	}
	childRunIDs := sortedStringSet(children)
	node := &report.ResumeRunTreeNode{
		RunID:            runID,
		ParentRunID:      parentRunID,
		ChildRunIDs:      childRunIDs,
		Depth:            depth,
		State:            stateValue,
		Source:           source,
		Interrupted:      resumeRunInterrupted(stateValue),
		RecoveryDecision: resumeRunRecoveryDecision(runID, stateValue, lifecycleErr),
	}
	nodes[runID] = node
	for _, child := range childRunIDs {
		collectResumeRunNode(repoPath, child, runID, depth+1, nodes, visiting)
	}
}

func resumeRunParentFromState(repoPath, runID string) string {
	lifecycle, err := state.LoadLifecycle(repoPath, runID)
	if err == nil && strings.TrimSpace(lifecycle.ParentRunID) != "" {
		return strings.TrimSpace(lifecycle.ParentRunID)
	}
	_, parent := resumeRunEdgesFromEvents(repoPath, runID)
	return parent
}

func resumeRunEdgesFromEvents(repoPath, runID string) ([]string, string) {
	path := state.EventsPath(repoPath, runID)
	file, err := os.Open(path)
	if err != nil {
		return nil, ""
	}
	defer file.Close()

	children := map[string]bool{}
	parent := ""
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Details json.RawMessage `json:"details"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		var details struct {
			ParentRunID string `json:"parent_run_id"`
			Child       struct {
				RunID string `json:"run_id"`
			} `json:"child"`
			Result struct {
				RunID string `json:"run_id"`
			} `json:"result"`
		}
		if len(event.Details) == 0 {
			continue
		}
		if err := json.Unmarshal(event.Details, &details); err != nil {
			continue
		}
		if strings.TrimSpace(details.ParentRunID) != "" && details.ParentRunID != runID {
			parent = strings.TrimSpace(details.ParentRunID)
		}
		children[details.Child.RunID] = true
		children[details.Result.RunID] = true
		delete(children, "")
	}
	return sortedStringSet(children), parent
}

func resumeRunInterrupted(stateValue string) bool {
	switch state.LifecycleState(strings.ToLower(strings.TrimSpace(stateValue))) {
	case state.StateSucceeded, state.StateFailed, state.StateCancelled, state.StateAbandoned, state.StateNeedsHuman:
		return false
	default:
		return true
	}
}

func resumeRunRecoveryDecision(runID, stateValue string, err error) *report.ResumeRecoveryDecision {
	decision := &report.ResumeRecoveryDecision{
		RunID:        runID,
		Reason:       stateValue,
		ContextPaths: []string{},
		PRLinks:      []string{},
	}
	if err != nil {
		decision.Outcome = "needs-human"
		decision.Action = "inspect unreadable run lifecycle before resuming or retrying"
		decision.Reason = err.Error()
		decision.NeedsHuman = true
		return decision
	}
	switch state.LifecycleState(strings.ToLower(strings.TrimSpace(stateValue))) {
	case state.StateSucceeded:
		decision.Outcome = "none"
		decision.Action = "run already succeeded"
	case state.StateFailed, state.StateCancelled, state.StateAbandoned:
		decision.Outcome = "retry"
		decision.Action = "inspect recovery context, then retry if still within budget and circuit-breaker policy"
		decision.SafeToResume = true
		decision.RetryAllowed = true
	case state.StateNeedsHuman:
		decision.Outcome = "needs-human"
		decision.Action = "human review required before retrying or resuming child work"
		decision.NeedsHuman = true
	default:
		decision.Outcome = "resume"
		decision.Action = "inspect active run state and resume safe unfinished work"
		decision.SafeToResume = true
	}
	return decision
}

func sortedStringSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	sort.Strings(out)
	return out
}
