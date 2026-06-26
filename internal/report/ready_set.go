package report

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type ReadySetReport struct {
	Version     int             `json:"version"`
	Repo        string          `json:"repo"`
	RepoPath    string          `json:"repo_path"`
	BaseBranch  string          `json:"base_branch"`
	RunID       *string         `json:"run_id"`
	GeneratedAt string          `json:"generated_at"`
	Ready       []ReadyIssue    `json:"ready"`
	Blocked     []BlockedIssue  `json:"blocked"`
	Summary     ReadySetSummary `json:"summary"`
}

type ReadyIssue struct {
	Issue  int    `json:"issue"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

type BlockedIssue struct {
	Issue          int              `json:"issue"`
	Title          string           `json:"title"`
	Classification string           `json:"classification"`
	Reason         string           `json:"reason"`
	Dependencies   []int            `json:"dependencies"`
	OpenPRs        []OpenPRSummary  `json:"open_prs"`
	Attempts       []AttemptSummary `json:"attempts"`
}

type OpenPRSummary struct {
	Number   int    `json:"number"`
	URL      string `json:"url"`
	Head     string `json:"head"`
	SubState string `json:"sub_state"`
}

type AttemptSummary struct {
	JobID          string `json:"job_id"`
	Attempt        int    `json:"attempt"`
	Status         string `json:"status"`
	Phase          string `json:"phase"`
	PID            *int   `json:"pid"`
	Branch         string `json:"branch"`
	Path           string `json:"path"`
	HeartbeatAt    string `json:"heartbeat_at"`
	LastProgressAt string `json:"last_progress_at"`
}

type ReadySetSummary struct {
	ReadyCount                int `json:"ready_count"`
	BlockedCount              int `json:"blocked_count"`
	BlockedByUnmergedDepCount int `json:"blocked_by_unmerged_dep_count"`
	HasOpenPRCount            int `json:"has_open_pr_count"`
	HasLiveAttemptCount       int `json:"has_live_attempt_count"`
	RecoveryNeededCount       int `json:"recovery_needed_count"`
}

func MarshalReadySetJSON(readySet ReadySetReport) ([]byte, error) {
	readySet = normalizeReadySet(readySet)
	data, err := json.MarshalIndent(readySet, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal ready-set JSON: %w", err)
	}
	return append(data, '\n'), nil
}

func RenderReadySetText(report ReadySetReport) string {
	var out bytes.Buffer
	runID := "(none)"
	if report.RunID != nil && *report.RunID != "" {
		runID = *report.RunID
	}

	fmt.Fprintln(&out, "READY SET")
	fmt.Fprintf(&out, "Repo: %s\n", report.Repo)
	fmt.Fprintf(&out, "Base branch: %s\n", report.BaseBranch)
	fmt.Fprintf(&out, "RunId: %s\n", runID)
	fmt.Fprintf(&out, "Generated at: %s\n", report.GeneratedAt)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Ready")
	if len(report.Ready) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, item := range report.Ready {
			fmt.Fprintf(&out, "- #%d %s\n", item.Issue, item.Title)
			fmt.Fprintf(&out, "  reason: %s\n", item.Reason)
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Non-ready")
	if len(report.Blocked) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, item := range report.Blocked {
			fmt.Fprintf(&out, "- #%d %s\n", item.Issue, item.Title)
			fmt.Fprintf(&out, "  classification: %s\n", item.Classification)
			fmt.Fprintf(&out, "  reason: %s\n", item.Reason)
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Safety")
	fmt.Fprintln(&out, "- ready-set is read-only: no dispatch, no merge, no push, and no GitHub mutation was attempted.")
	return out.String()
}

func normalizeReadySet(readySet ReadySetReport) ReadySetReport {
	if readySet.Ready == nil {
		readySet.Ready = []ReadyIssue{}
	}
	if readySet.Blocked == nil {
		readySet.Blocked = []BlockedIssue{}
	}
	for i := range readySet.Blocked {
		if readySet.Blocked[i].Dependencies == nil {
			readySet.Blocked[i].Dependencies = []int{}
		}
		if readySet.Blocked[i].OpenPRs == nil {
			readySet.Blocked[i].OpenPRs = []OpenPRSummary{}
		}
		if readySet.Blocked[i].Attempts == nil {
			readySet.Blocked[i].Attempts = []AttemptSummary{}
		}
	}
	return readySet
}
