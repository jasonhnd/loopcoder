package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type ReadySetReport struct {
	Version      int                   `json:"version"`
	Repo         string                `json:"repo"`
	RepoPath     string                `json:"repo_path"`
	BaseBranch   string                `json:"base_branch"`
	RunID        *string               `json:"run_id"`
	GeneratedAt  string                `json:"generated_at"`
	Ready        []ReadyIssue          `json:"ready"`
	Blocked      []BlockedIssue        `json:"blocked"`
	EpicOrdering []EpicOrderingSummary `json:"epic_ordering,omitempty"`
	Summary      ReadySetSummary       `json:"summary"`
}

type ReadyIssue struct {
	Issue          int      `json:"issue"`
	Title          string   `json:"title"`
	Reason         string   `json:"reason"`
	EpicID         string   `json:"epic_id,omitempty"`
	EpicTitle      string   `json:"epic_title,omitempty"`
	SliceID        string   `json:"slice_id,omitempty"`
	SliceRef       string   `json:"slice_ref,omitempty"`
	UnblockCount   int      `json:"unblock_count,omitempty"`
	OnCriticalPath bool     `json:"on_critical_path,omitempty"`
	DependsOn      []string `json:"depends_on,omitempty"`
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

type EpicOrderingSummary struct {
	EpicID          string               `json:"epic_id"`
	EpicTitle       string               `json:"epic_title"`
	ArtifactPath    string               `json:"artifact_path"`
	Ready           []string             `json:"ready"`
	CriticalPath    []string             `json:"critical_path"`
	CriticalPathETA int                  `json:"critical_path_eta"`
	AtomicSlices    []AtomicSliceSummary `json:"atomic_slices"`
}

type AtomicSliceSummary struct {
	Ref     string   `json:"ref"`
	Issue   int      `json:"issue,omitempty"`
	Members []string `json:"members"`
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
			if item.SliceRef != "" {
				fmt.Fprintf(&out, "  epic_slice: %s\n", item.SliceRef)
			}
			if item.OnCriticalPath {
				fmt.Fprintln(&out, "  critical_path: yes")
			}
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

	if len(report.EpicOrdering) > 0 {
		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "Epic ordering")
		for _, epic := range report.EpicOrdering {
			fmt.Fprintf(&out, "- %s\n", epic.EpicTitle)
			fmt.Fprintf(&out, "  artifact: %s\n", epic.ArtifactPath)
			fmt.Fprintf(&out, "  ready: %s\n", renderStringList(epic.Ready))
			fmt.Fprintf(&out, "  critical_path_eta: %d\n", epic.CriticalPathETA)
			fmt.Fprintf(&out, "  critical_path: %s\n", renderStringList(epic.CriticalPath))
			if len(epic.AtomicSlices) == 0 {
				fmt.Fprintln(&out, "  atomic_slices: none")
			} else {
				for _, atomic := range epic.AtomicSlices {
					fmt.Fprintf(&out, "  atomic_slice: %s (%s)\n", atomic.Ref, renderStringList(atomic.Members))
				}
			}
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
	if readySet.EpicOrdering == nil {
		readySet.EpicOrdering = []EpicOrderingSummary{}
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

func renderStringList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}
