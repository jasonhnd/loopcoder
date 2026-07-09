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
	RunNote     string               `json:"run_note,omitempty"`
	GeneratedAt string               `json:"generated_at"`
	GitHub      ResumeGitHubSnapshot `json:"github"`
	Local       ResumeLocalState     `json:"local"`
	Thresholds  ResumeThresholds     `json:"thresholds"`
	RunTree     ResumeRunTree        `json:"run_tree"`
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

type ResumeIssue struct {
	Issue            int                     `json:"issue"`
	Title            string                  `json:"title"`
	State            string                  `json:"state"`
	Labels           []string                `json:"labels"`
	Classification   string                  `json:"classification"`
	ActionKind       string                  `json:"action_kind"`
	Action           string                  `json:"action"`
	Evidence         []string                `json:"evidence"`
	RecoveryDecision *ResumeRecoveryDecision `json:"recovery_decision,omitempty"`
}

type ResumeRecoveryDecision struct {
	Outcome             string   `json:"outcome"`
	Action              string   `json:"action"`
	Reason              string   `json:"reason"`
	SafeToResume        bool     `json:"safe_to_resume"`
	RetryAllowed        bool     `json:"retry_allowed"`
	NeedsHuman          bool     `json:"needs_human"`
	Issue               int      `json:"issue,omitempty"`
	RunID               string   `json:"run_id,omitempty"`
	RecoveryContextPath string   `json:"recovery_context_path,omitempty"`
	ContextPaths        []string `json:"context_paths"`
	PRLinks             []string `json:"pr_links"`
}

type ResumeRunTree struct {
	RootRunID string               `json:"root_run_id,omitempty"`
	Nodes     []ResumeRunTreeNode  `json:"nodes"`
	Summary   ResumeRunTreeSummary `json:"summary"`
}

type ResumeRunTreeNode struct {
	RunID            string                  `json:"run_id"`
	ParentRunID      string                  `json:"parent_run_id,omitempty"`
	ChildRunIDs      []string                `json:"child_run_ids"`
	Depth            int                     `json:"depth"`
	State            string                  `json:"state"`
	Source           string                  `json:"source,omitempty"`
	Interrupted      bool                    `json:"interrupted"`
	RecoveryDecision *ResumeRecoveryDecision `json:"recovery_decision,omitempty"`
}

type ResumeRunTreeSummary struct {
	RunCount        int `json:"run_count"`
	InterruptedRuns int `json:"interrupted_runs"`
	RetryableRuns   int `json:"retryable_runs"`
	NeedsHumanRuns  int `json:"needs_human_runs"`
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
	report = normalizeResume(report)
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
	fmt.Fprintln(&out, "Run tree")
	if len(report.RunTree.Nodes) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, node := range report.RunTree.Nodes {
			children := "none"
			if len(node.ChildRunIDs) > 0 {
				children = strings.Join(node.ChildRunIDs, ", ")
			}
			interrupted := "no"
			if node.Interrupted {
				interrupted = "yes"
			}
			fmt.Fprintf(&out, "- %s\n", node.RunID)
			if strings.TrimSpace(node.ParentRunID) != "" {
				fmt.Fprintf(&out, "  parent: %s\n", node.ParentRunID)
			}
			fmt.Fprintf(&out, "  state: %s; depth: %d; children: %s; interrupted: %s\n", node.State, node.Depth, children, interrupted)
			if node.RecoveryDecision != nil {
				fmt.Fprintf(&out, "  recovery: %s; needs_human=%t; retry_allowed=%t\n", node.RecoveryDecision.Outcome, node.RecoveryDecision.NeedsHuman, node.RecoveryDecision.RetryAllowed)
			}
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
	if resume.Issues == nil {
		resume.Issues = []ResumeIssue{}
	}
	for i := range resume.Issues {
		if resume.Issues[i].Labels == nil {
			resume.Issues[i].Labels = []string{}
		}
		if resume.Issues[i].Evidence == nil {
			resume.Issues[i].Evidence = []string{}
		}
		normalizeRecoveryDecision(resume.Issues[i].RecoveryDecision)
	}
	if resume.RunTree.Nodes == nil {
		resume.RunTree.Nodes = []ResumeRunTreeNode{}
	}
	for i := range resume.RunTree.Nodes {
		if resume.RunTree.Nodes[i].ChildRunIDs == nil {
			resume.RunTree.Nodes[i].ChildRunIDs = []string{}
		}
		normalizeRecoveryDecision(resume.RunTree.Nodes[i].RecoveryDecision)
	}
	resume.RunTree.Summary = summarizeRunTree(resume.RunTree.Nodes)
	return resume
}

func normalizeRecoveryDecision(decision *ResumeRecoveryDecision) {
	if decision == nil {
		return
	}
	if decision.ContextPaths == nil {
		decision.ContextPaths = []string{}
	}
	if decision.PRLinks == nil {
		decision.PRLinks = []string{}
	}
}

func summarizeRunTree(nodes []ResumeRunTreeNode) ResumeRunTreeSummary {
	summary := ResumeRunTreeSummary{RunCount: len(nodes)}
	for _, node := range nodes {
		if node.Interrupted {
			summary.InterruptedRuns++
		}
		if node.RecoveryDecision == nil {
			continue
		}
		if node.RecoveryDecision.RetryAllowed {
			summary.RetryableRuns++
		}
		if node.RecoveryDecision.NeedsHuman {
			summary.NeedsHumanRuns++
		}
	}
	return summary
}
