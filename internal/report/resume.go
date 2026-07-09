package report

import (
	"bytes"
	"fmt"
	"strings"
)

type ResumeReport struct {
	Version      int                  `json:"version"`
	Repo         string               `json:"repo"`
	RepoPath     string               `json:"repo_path,omitempty"`
	BaseBranch   string               `json:"base_branch"`
	RunID        *string              `json:"run_id,omitempty"`
	RunNote      string               `json:"run_note,omitempty"`
	GeneratedAt  string               `json:"generated_at"`
	GitHub       ResumeGitHubSnapshot `json:"github"`
	Local        ResumeLocalState     `json:"local"`
	Thresholds   ResumeThresholds     `json:"thresholds"`
	Issues       []ResumeIssue        `json:"issues"`
	RunDecisions []ResumeRunDecision  `json:"run_decisions,omitempty"`
}

type ResumeGitHubSnapshot struct {
	OpenIssueCount int `json:"open_issue_count"`
	OpenPRCount    int `json:"open_pr_count"`
}

type ResumeLocalState struct {
	AttemptCount int `json:"attempt_count"`
	EventCount   int `json:"event_count"`
	RunCount     int `json:"run_count,omitempty"`
}

type ResumeThresholds struct {
	HeartbeatFreshSeconds int `json:"heartbeat_fresh_seconds"`
	StaleAfterSeconds     int `json:"stale_after_seconds"`
	HungAfterSeconds      int `json:"hung_after_seconds"`
}

type ResumeIssue struct {
	Issue          int      `json:"issue"`
	Title          string   `json:"title"`
	State          string   `json:"state"`
	Labels         []string `json:"labels,omitempty"`
	Classification string   `json:"classification"`
	ActionKind     string   `json:"action_kind"`
	Action         string   `json:"action"`
	Evidence       []string `json:"evidence,omitempty"`
}

type ResumeRunDecision struct {
	RunID               string   `json:"run_id"`
	ParentRunID         string   `json:"parent_run_id,omitempty"`
	Role                string   `json:"role"`
	Issue               int      `json:"issue,omitempty"`
	Status              string   `json:"status,omitempty"`
	Classification      string   `json:"classification"`
	ActionKind          string   `json:"action_kind"`
	Action              string   `json:"action"`
	RecoveryContextPath string   `json:"recovery_context_path,omitempty"`
	PR                  string   `json:"pr,omitempty"`
	Evidence            []string `json:"evidence,omitempty"`
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
	fmt.Fprintf(&out, "Local state: attempts=%d, events=%d", report.Local.AttemptCount, report.Local.EventCount)
	if report.Local.RunCount > 0 {
		fmt.Fprintf(&out, ", runs=%d", report.Local.RunCount)
	}
	fmt.Fprintln(&out)
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

	if len(report.RunDecisions) > 0 {
		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "Run recovery decisions")
		for _, decision := range report.RunDecisions {
			fmt.Fprintf(&out, "- %s (%s)\n", decision.RunID, decision.Role)
			if strings.TrimSpace(decision.ParentRunID) != "" {
				fmt.Fprintf(&out, "  parent: %s\n", decision.ParentRunID)
			}
			if decision.Issue > 0 {
				fmt.Fprintf(&out, "  issue: #%d\n", decision.Issue)
			}
			fmt.Fprintf(&out, "  status: %s\n", displayResumeValue(decision.Status))
			fmt.Fprintf(&out, "  classification: %s\n", decision.Classification)
			for _, evidence := range decision.Evidence {
				fmt.Fprintf(&out, "  evidence: %s\n", evidence)
			}
			fmt.Fprintf(&out, "  next: %s\n", decision.Action)
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

func displayResumeValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}
