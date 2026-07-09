package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type ResumeReport struct {
	Version     int                  `json:"version"`
	Repo        string               `json:"repo"`
	RepoPath    string               `json:"repo_path"`
	BaseBranch  string               `json:"base_branch"`
	RunID       *string              `json:"run_id"`
	RunNote     string               `json:"run_note"`
	GeneratedAt string               `json:"generated_at"`
	GitHub      ResumeGitHubSnapshot `json:"github"`
	Local       ResumeLocalState     `json:"local"`
	Thresholds  ResumeThresholds     `json:"thresholds"`
	RunTree     []ResumeRun          `json:"run_tree"`
	Issues      []ResumeIssue        `json:"issues"`
}

type ResumeGitHubSnapshot struct {
	OpenIssueCount int `json:"open_issue_count"`
	OpenPRCount    int `json:"open_pr_count"`
}

type ResumeLocalState struct {
	AttemptCount int `json:"attempt_count"`
	EventCount   int `json:"event_count"`
}

type ResumeThresholds struct {
	HeartbeatFreshSeconds int `json:"heartbeat_fresh_seconds"`
	StaleAfterSeconds     int `json:"stale_after_seconds"`
	HungAfterSeconds      int `json:"hung_after_seconds"`
}

type ResumeRun struct {
	RunID        string   `json:"run_id"`
	ParentRunID  string   `json:"parent_run_id,omitempty"`
	ChildRunIDs  []string `json:"child_run_ids"`
	Status       string   `json:"status,omitempty"`
	AttemptCount int      `json:"attempt_count"`
	EventCount   int      `json:"event_count"`
	SourcePath   string   `json:"source_path,omitempty"`
}

type ResumeIssue struct {
	Issue            int                    `json:"issue"`
	Title            string                 `json:"title"`
	State            string                 `json:"state"`
	Labels           []string               `json:"labels"`
	RunID            string                 `json:"run_id,omitempty"`
	ParentRunID      string                 `json:"parent_run_id,omitempty"`
	ChildRunIDs      []string               `json:"child_run_ids,omitempty"`
	Classification   string                 `json:"classification"`
	ActionKind       string                 `json:"action_kind"`
	Action           string                 `json:"action"`
	RecoveryDecision ResumeRecoveryDecision `json:"recovery_decision"`
	Evidence         []string               `json:"evidence"`
}

type ResumeRecoveryDecision struct {
	Kind                string `json:"kind"`
	SafeToResume        bool   `json:"safe_to_resume"`
	NeedsHuman          bool   `json:"needs_human"`
	Reason              string `json:"reason"`
	RecoveryContextPath string `json:"recovery_context_path,omitempty"`
	PR                  string `json:"pr,omitempty"`
	Branch              string `json:"branch,omitempty"`
}

func MarshalResumeJSON(resume ResumeReport) ([]byte, error) {
	resume = normalizeResume(resume)
	data, err := json.MarshalIndent(resume, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal resume JSON: %w", err)
	}
	return append(data, '\n'), nil
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
	fmt.Fprintln(&out, "Run tree")
	if len(report.RunTree) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, run := range report.RunTree {
			children := "none"
			if len(run.ChildRunIDs) > 0 {
				children = strings.Join(run.ChildRunIDs, ", ")
			}
			parent := run.ParentRunID
			if strings.TrimSpace(parent) == "" {
				parent = "none"
			}
			status := run.Status
			if strings.TrimSpace(status) == "" {
				status = "unknown"
			}
			fmt.Fprintf(&out, "- %s\n", run.RunID)
			fmt.Fprintf(&out, "  parent: %s; children: %s; status: %s\n", parent, children, status)
			fmt.Fprintf(&out, "  attempts: %d; events: %d\n", run.AttemptCount, run.EventCount)
		}
	}

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
			if strings.TrimSpace(issue.RunID) != "" {
				fmt.Fprintf(&out, "  run: %s\n", issue.RunID)
			}
			fmt.Fprintf(&out, "  state: %s; labels: %s\n", issue.State, labelText)
			fmt.Fprintf(&out, "  classification: %s\n", issue.Classification)
			fmt.Fprintf(&out, "  decision: %s; safe_to_resume=%t; needs_human=%t\n", issue.RecoveryDecision.Kind, issue.RecoveryDecision.SafeToResume, issue.RecoveryDecision.NeedsHuman)
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

func normalizeResume(resume ResumeReport) ResumeReport {
	if resume.RunTree == nil {
		resume.RunTree = []ResumeRun{}
	}
	if resume.Issues == nil {
		resume.Issues = []ResumeIssue{}
	}
	for i := range resume.RunTree {
		if resume.RunTree[i].ChildRunIDs == nil {
			resume.RunTree[i].ChildRunIDs = []string{}
		}
	}
	for i := range resume.Issues {
		if resume.Issues[i].Labels == nil {
			resume.Issues[i].Labels = []string{}
		}
		if resume.Issues[i].ChildRunIDs == nil {
			resume.Issues[i].ChildRunIDs = []string{}
		}
		if resume.Issues[i].Evidence == nil {
			resume.Issues[i].Evidence = []string{}
		}
	}
	return resume
}
