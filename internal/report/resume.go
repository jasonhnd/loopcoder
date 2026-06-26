package report

import (
	"bytes"
	"fmt"
	"strings"
)

type ResumeReport struct {
	Version     int
	Repo        string
	RepoPath    string
	BaseBranch  string
	RunID       *string
	RunNote     string
	GeneratedAt string
	GitHub      ResumeGitHubSnapshot
	Local       ResumeLocalState
	Thresholds  ResumeThresholds
	Issues      []ResumeIssue
}

type ResumeGitHubSnapshot struct {
	OpenIssueCount int
	OpenPRCount    int
}

type ResumeLocalState struct {
	AttemptCount int
	EventCount   int
}

type ResumeThresholds struct {
	HeartbeatFreshSeconds int
	StaleAfterSeconds     int
	HungAfterSeconds      int
}

type ResumeIssue struct {
	Issue          int
	Title          string
	State          string
	Labels         []string
	Classification string
	ActionKind     string
	Action         string
	Evidence       []string
}

func RenderResumeText(report ResumeReport) string {
	var out bytes.Buffer
	runID := "(none)"
	if report.RunID != nil && *report.RunID != "" {
		runID = *report.RunID
	}
	if strings.TrimSpace(report.RunNote) != "" {
		runID = fmt.Sprintf("%s (%s)", runID, report.RunNote)
	}

	fmt.Fprintln(&out, "RESUME REPORT")
	fmt.Fprintf(&out, "Repo: %s\n", report.Repo)
	fmt.Fprintf(&out, "Base branch: %s\n", report.BaseBranch)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	fmt.Fprintf(&out, "Generated at: %s\n", report.GeneratedAt)
	fmt.Fprintf(&out, "GitHub snapshot: open issues=%d, open PRs=%d\n", report.GitHub.OpenIssueCount, report.GitHub.OpenPRCount)
	fmt.Fprintf(&out, "Local state: attempts=%d, events=%d\n", report.Local.AttemptCount, report.Local.EventCount)
	fmt.Fprintf(
		&out,
		"Thresholds: heartbeat fresh <= %ds, stale progress > %ds, hung progress > %ds\n",
		report.Thresholds.HeartbeatFreshSeconds,
		report.Thresholds.StaleAfterSeconds,
		report.Thresholds.HungAfterSeconds,
	)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Issues")

	if len(report.Issues) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, issue := range report.Issues {
			labelText := "(none)"
			if len(issue.Labels) > 0 {
				labelText = strings.Join(issue.Labels, ", ")
			}
			fmt.Fprintf(&out, "- #%d %s\n", issue.Issue, issue.Title)
			fmt.Fprintf(&out, "  state: %s; labels: %s\n", issue.State, labelText)
			fmt.Fprintf(&out, "  classification: %s\n", issue.Classification)
			for _, evidence := range issue.Evidence {
				fmt.Fprintf(&out, "  evidence: %s\n", evidence)
			}
			fmt.Fprintf(&out, "  next: %s\n", issue.Action)
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Next ready actions")
	ready := resumeIssuesByAction(report.Issues, "ready")
	if len(ready) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, issue := range ready {
			fmt.Fprintf(&out, "- #%d: %s (classification=%s)\n", issue.Issue, issue.Action, issue.Classification)
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Blocked / awaiting human input")
	blocked := resumeIssuesByAction(report.Issues, "blocked")
	if len(blocked) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, issue := range blocked {
			fmt.Fprintf(&out, "- #%d: %s (classification=%s)\n", issue.Issue, issue.Action, issue.Classification)
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Safety")
	fmt.Fprintln(&out, "- resume is read-only: no dispatch, no merge, no push, no GitHub mutation, and no run-state writes were attempted.")
	return out.String()
}

func resumeIssuesByAction(issues []ResumeIssue, actionKind string) []ResumeIssue {
	out := make([]ResumeIssue, 0)
	for _, issue := range issues {
		if issue.ActionKind == actionKind {
			out = append(out, issue)
		}
	}
	return out
}
