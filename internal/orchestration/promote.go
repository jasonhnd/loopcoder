package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	PromoteReportVersion = 1

	PromoteStatusSucceeded = "succeeded"
	PromoteStatusFailed    = "failed"

	PromoteOutcomePromoted      = "promoted"
	PromoteOutcomeSkippedAsDone = "skipped-as-done"
	PromoteOutcomeFailed        = "failed"

	promoteLedgerEvent = "promote.attempt"
)

type PromotionWriter interface {
	KickBackFromPreProd(ctx context.Context, item, preProdBranch string) (gh.PreProdKickBackResult, error)
	PromotePreProdToMain(ctx context.Context, preProdBranch string) (gh.MainPromotionResult, error)
	SyncPreProdFromMain(ctx context.Context, preProdBranch string) (gh.PreProdSyncResult, error)
}

type PromoteOptions struct {
	Writer        PromotionWriter
	RepoPath      string
	RunID         string
	PreProdBranch string
	Gate          string
	KickBackItems []string
	Clock         func() time.Time
	StatePush     StatePushFunc
}

type PromoteReport struct {
	Version       int                     `json:"version"`
	RepoPath      string                  `json:"repo_path"`
	RunID         string                  `json:"run_id"`
	PreProdBranch string                  `json:"pre_prod_branch"`
	MainBranch    string                  `json:"main_branch"`
	Gate          string                  `json:"gate"`
	Status        string                  `json:"status"`
	StartedAt     string                  `json:"started_at"`
	FinishedAt    string                  `json:"finished_at"`
	KickedBack    []PromoteKickBackResult `json:"kicked_back"`
	NeedsHuman    []PromoteNeedsHuman     `json:"needs_human"`
	Promoted      PromoteMainResult       `json:"promoted"`
	Sync          PromoteSyncResult       `json:"sync"`
	StatePush     *PromoteStatePush       `json:"state_push,omitempty"`
	Summary       PromoteSummary          `json:"summary"`
}

type PromoteKickBackResult struct {
	Item        string `json:"item"`
	PRNumber    int    `json:"pr_number,omitempty"`
	Branch      string `json:"branch"`
	RevertedSHA string `json:"reverted_sha,omitempty"`
	SHA         string `json:"sha,omitempty"`
	URL         string `json:"url,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

type PromoteMainResult struct {
	PreProdBranch   string `json:"pre_prod_branch"`
	MainBranch      string `json:"main_branch"`
	Head            string `json:"head,omitempty"`
	SHA             string `json:"sha,omitempty"`
	URL             string `json:"url,omitempty"`
	AlreadyUpToDate bool   `json:"already_up_to_date,omitempty"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
}

type PromoteSyncResult struct {
	PreProdBranch string `json:"pre_prod_branch"`
	MainBranch    string `json:"main_branch"`
	SHA           string `json:"sha,omitempty"`
	URL           string `json:"url,omitempty"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
}

type PromoteNeedsHuman struct {
	Step     string `json:"step"`
	Item     string `json:"item,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`
	Detail   string `json:"detail"`
}

type PromoteStatePush struct {
	Branch    string   `json:"branch"`
	Remote    string   `json:"remote"`
	Committed bool     `json:"committed"`
	Pushed    bool     `json:"pushed"`
	PushError string   `json:"push_error,omitempty"`
	Files     []string `json:"files"`
	Error     string   `json:"error,omitempty"`
}

type PromoteSummary struct {
	KickedBackCount int `json:"kicked_back_count"`
	PromotedCount   int `json:"promoted_count"`
	NeedsHumanCount int `json:"needs_human_count"`
	FailureCount    int `json:"failure_count"`
}

func Promote(ctx context.Context, opts PromoteOptions) (PromoteReport, error) {
	opts = withPromoteDefaults(opts)
	opts.PreProdBranch = strings.TrimSpace(opts.PreProdBranch)
	opts.Gate = normalizePromotionGate(opts.Gate)
	opts.RunID = strings.TrimSpace(opts.RunID)
	started := opts.Clock().UTC()
	if opts.RunID == "" {
		opts.RunID = state.RunIDForWave(started)
	}
	report := PromoteReport{
		Version:       PromoteReportVersion,
		RepoPath:      opts.RepoPath,
		RunID:         opts.RunID,
		PreProdBranch: opts.PreProdBranch,
		MainBranch:    "main",
		Gate:          opts.Gate,
		Status:        PromoteStatusSucceeded,
		StartedAt:     state.FormatTimestamp(started),
		KickedBack:    []PromoteKickBackResult{},
		NeedsHuman:    []PromoteNeedsHuman{},
	}
	finish := func() (PromoteReport, error) {
		report.FinishedAt = state.FormatTimestamp(opts.Clock().UTC())
		report.Summary.NeedsHumanCount = len(report.NeedsHuman)
		report = normalizePromoteReport(report)
		if err := recordPromoteAttempt(ctx, opts, &report); err != nil {
			report.Status = PromoteStatusFailed
			report.Summary.FailureCount++
			if report.StatePush == nil {
				report.StatePush = &PromoteStatePush{Files: []string{}, Error: err.Error()}
			} else if strings.TrimSpace(report.StatePush.Error) == "" && strings.TrimSpace(report.StatePush.PushError) == "" {
				report.StatePush.Error = err.Error()
			}
		}
		return normalizePromoteReport(report), nil
	}

	if opts.Writer == nil {
		return report, errors.New("promotion writer is required")
	}
	if opts.PreProdBranch == "" {
		return report, errors.New("pre-prod branch is required")
	}
	if strings.EqualFold(opts.PreProdBranch, "main") {
		return report, errors.New("pre-prod branch must not be main")
	}
	if opts.Gate != "human-merge" {
		return report, fmt.Errorf("promote requires adapters.gate human-merge, got %q", opts.Gate)
	}

	for _, item := range normalizeKickBackItems(opts.KickBackItems) {
		kick := PromoteKickBackResult{
			Item:   item,
			Branch: opts.PreProdBranch,
			Status: PromoteStatusSucceeded,
		}
		kicked, err := opts.Writer.KickBackFromPreProd(ctx, item, opts.PreProdBranch)
		if err != nil {
			kick.Status = PromoteStatusFailed
			kick.Error = err.Error()
			report.KickedBack = append(report.KickedBack, kick)
			report.Status = PromoteStatusFailed
			report.Summary.FailureCount++
			return finish()
		}
		kick.PRNumber = kicked.PRNumber
		kick.Branch = firstNonEmpty(kicked.Branch, opts.PreProdBranch)
		kick.RevertedSHA = kicked.RevertedSHA
		kick.SHA = kicked.SHA
		kick.URL = kicked.URL
		report.KickedBack = append(report.KickedBack, kick)
		report.NeedsHuman = append(report.NeedsHuman, PromoteNeedsHuman{
			Step:     "kick-back",
			Item:     item,
			PRNumber: kicked.PRNumber,
			Detail:   "kicked back from pre-prod; return item to needs-human before a future promotion",
		})
		report.Summary.KickedBackCount++
	}

	promoted, err := opts.Writer.PromotePreProdToMain(ctx, opts.PreProdBranch)
	report.Promoted = PromoteMainResult{
		PreProdBranch: opts.PreProdBranch,
		MainBranch:    "main",
		Head:          opts.PreProdBranch,
		Status:        PromoteStatusSucceeded,
	}
	if err != nil {
		report.Promoted.Status = PromoteStatusFailed
		report.Promoted.Error = err.Error()
		report.Status = PromoteStatusFailed
		report.Summary.FailureCount++
		return finish()
	}
	report.Promoted.PreProdBranch = firstNonEmpty(promoted.PreProdBranch, opts.PreProdBranch)
	report.Promoted.MainBranch = firstNonEmpty(promoted.MainBranch, "main")
	report.Promoted.Head = promoted.Head
	report.Promoted.SHA = promoted.SHA
	report.Promoted.URL = promoted.URL
	report.Promoted.AlreadyUpToDate = promoted.AlreadyUpToDate
	report.Summary.PromotedCount = 1

	synced, err := opts.Writer.SyncPreProdFromMain(ctx, opts.PreProdBranch)
	report.Sync = PromoteSyncResult{
		PreProdBranch: opts.PreProdBranch,
		MainBranch:    "main",
		Status:        PromoteStatusSucceeded,
	}
	if err != nil {
		report.Sync.Status = PromoteStatusFailed
		report.Sync.Error = err.Error()
		report.Status = PromoteStatusFailed
		report.Summary.FailureCount++
		return finish()
	}
	report.Sync.PreProdBranch = firstNonEmpty(synced.PreProdBranch, opts.PreProdBranch)
	report.Sync.MainBranch = firstNonEmpty(synced.MainBranch, "main")
	report.Sync.SHA = synced.SHA
	report.Sync.URL = synced.URL
	return finish()
}

func withPromoteDefaults(opts PromoteOptions) PromoteOptions {
	if opts.Clock == nil {
		opts.Clock = func() time.Time {
			return time.Now().UTC()
		}
	}
	if opts.StatePush == nil {
		opts.StatePush = func(ctx context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error) {
			return statebranch.Push(ctx, opts, statebranch.DefaultDeps())
		}
	}
	return opts
}

func recordPromoteAttempt(ctx context.Context, opts PromoteOptions, report *PromoteReport) error {
	reportJSON, err := json.Marshal(normalizePromoteReport(*report))
	if err != nil {
		return fmt.Errorf("marshal promote ledger event: %w", err)
	}

	exitCode := PromoteExitCode(*report)
	var errorMessage *string
	if report.Status == PromoteStatusFailed {
		text := promoteReportError(*report)
		errorMessage = &text
	}
	outcome := promoteLedgerOutcome(*report)
	if err := state.AppendEvent(opts.RepoPath, report.RunID, state.Event{
		Timestamp: report.FinishedAt,
		RunID:     report.RunID,
		JobID:     "promote",
		Issue:     0,
		Phase:     "promote",
		Status:    outcome,
		LogBytes:  0,
		ExitCode:  &exitCode,
		Error:     errorMessage,
		Event:     promoteLedgerEvent,
		Outcome:   outcome,
		Details:   json.RawMessage(reportJSON),
	}); err != nil {
		return fmt.Errorf("append promote ledger event: %w", err)
	}

	result, err := opts.StatePush(ctx, statebranch.PushOptions{
		RepoPath: opts.RepoPath,
		RunID:    report.RunID,
	})
	report.StatePush = promoteStatePush(result)
	if err != nil {
		report.StatePush.Error = err.Error()
		return fmt.Errorf("push promote ledger: %w", err)
	}
	if strings.TrimSpace(result.PushError) != "" {
		return fmt.Errorf("push promote ledger: %s", result.PushError)
	}
	return nil
}

func promoteStatePush(result statebranch.PushResult) *PromoteStatePush {
	return &PromoteStatePush{
		Branch:    result.Branch,
		Remote:    result.Remote,
		Committed: result.Committed,
		Pushed:    result.Pushed,
		PushError: result.PushError,
		Files:     append([]string(nil), result.Files...),
	}
}

func promoteLedgerOutcome(report PromoteReport) string {
	if report.Status != PromoteStatusSucceeded {
		return PromoteOutcomeFailed
	}
	if report.Promoted.AlreadyUpToDate {
		return PromoteOutcomeSkippedAsDone
	}
	return PromoteOutcomePromoted
}

func promoteReportError(report PromoteReport) string {
	for _, kicked := range report.KickedBack {
		if strings.TrimSpace(kicked.Error) != "" {
			return kicked.Error
		}
	}
	if strings.TrimSpace(report.Promoted.Error) != "" {
		return report.Promoted.Error
	}
	if strings.TrimSpace(report.Sync.Error) != "" {
		return report.Sync.Error
	}
	if report.StatePush != nil {
		if strings.TrimSpace(report.StatePush.Error) != "" {
			return report.StatePush.Error
		}
		if strings.TrimSpace(report.StatePush.PushError) != "" {
			return report.StatePush.PushError
		}
	}
	return "promote failed"
}

func MarshalPromoteJSON(report PromoteReport) ([]byte, error) {
	report = normalizePromoteReport(report)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal promote JSON: %w", err)
	}
	return append(data, '\n'), nil
}

func RenderPromoteText(report PromoteReport) string {
	report = normalizePromoteReport(report)
	var out bytes.Buffer

	fmt.Fprintln(&out, "PROMOTE")
	fmt.Fprintf(&out, "Repo path: %s\n", report.RepoPath)
	fmt.Fprintf(&out, "Pre-prod branch: %s\n", report.PreProdBranch)
	fmt.Fprintf(&out, "Main branch: %s\n", report.MainBranch)
	fmt.Fprintf(&out, "Gate: %s\n", report.Gate)
	fmt.Fprintf(&out, "RunId: %s\n", report.RunID)
	fmt.Fprintf(&out, "Status: %s\n", report.Status)
	if strings.TrimSpace(report.StartedAt) != "" {
		fmt.Fprintf(&out, "Started at: %s\n", report.StartedAt)
	}
	if strings.TrimSpace(report.FinishedAt) != "" {
		fmt.Fprintf(&out, "Finished at: %s\n", report.FinishedAt)
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Kicked back")
	if len(report.KickedBack) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, item := range report.KickedBack {
			target := item.Item
			if item.PRNumber > 0 {
				target = fmt.Sprintf("PR #%d", item.PRNumber)
			}
			fmt.Fprintf(&out, "- %s %s branch=%s\n", target, item.Status, item.Branch)
			if strings.TrimSpace(item.RevertedSHA) != "" {
				fmt.Fprintf(&out, "  reverted_sha: %s\n", item.RevertedSHA)
			}
			if strings.TrimSpace(item.SHA) != "" {
				fmt.Fprintf(&out, "  sha: %s\n", item.SHA)
			}
			if strings.TrimSpace(item.URL) != "" {
				fmt.Fprintf(&out, "  url: %s\n", item.URL)
			}
			if strings.TrimSpace(item.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", item.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Needs human")
	if len(report.NeedsHuman) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, item := range report.NeedsHuman {
			target := item.Item
			if item.PRNumber > 0 {
				target = fmt.Sprintf("PR #%d", item.PRNumber)
			}
			fmt.Fprintf(&out, "- %s %s: %s\n", item.Step, target, item.Detail)
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Promoted")
	fmt.Fprintf(&out, "- %s -> %s %s\n", report.Promoted.PreProdBranch, report.Promoted.MainBranch, report.Promoted.Status)
	if report.Promoted.AlreadyUpToDate {
		fmt.Fprintln(&out, "  already_up_to_date: true")
	}
	if strings.TrimSpace(report.Promoted.SHA) != "" {
		fmt.Fprintf(&out, "  sha: %s\n", report.Promoted.SHA)
	}
	if strings.TrimSpace(report.Promoted.URL) != "" {
		fmt.Fprintf(&out, "  url: %s\n", report.Promoted.URL)
	}
	if strings.TrimSpace(report.Promoted.Error) != "" {
		fmt.Fprintf(&out, "  error: %s\n", report.Promoted.Error)
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Pre-prod sync")
	fmt.Fprintf(&out, "- %s -> %s %s\n", report.Sync.MainBranch, report.Sync.PreProdBranch, report.Sync.Status)
	if strings.TrimSpace(report.Sync.SHA) != "" {
		fmt.Fprintf(&out, "  sha: %s\n", report.Sync.SHA)
	}
	if strings.TrimSpace(report.Sync.URL) != "" {
		fmt.Fprintf(&out, "  url: %s\n", report.Sync.URL)
	}
	if strings.TrimSpace(report.Sync.Error) != "" {
		fmt.Fprintf(&out, "  error: %s\n", report.Sync.Error)
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "State")
	if report.StatePush == nil {
		fmt.Fprintln(&out, "- not pushed")
	} else {
		fmt.Fprintf(&out, "- branch=%s remote=%s committed=%t pushed=%t files=%d\n",
			report.StatePush.Branch,
			report.StatePush.Remote,
			report.StatePush.Committed,
			report.StatePush.Pushed,
			len(report.StatePush.Files),
		)
		if strings.TrimSpace(report.StatePush.PushError) != "" {
			fmt.Fprintf(&out, "  push_error: %s\n", report.StatePush.PushError)
		}
		if strings.TrimSpace(report.StatePush.Error) != "" {
			fmt.Fprintf(&out, "  error: %s\n", report.StatePush.Error)
		}
	}
	return out.String()
}

func PromoteExitCode(report PromoteReport) int {
	if report.Status == PromoteStatusSucceeded {
		return 0
	}
	return 1
}

func normalizePromoteReport(report PromoteReport) PromoteReport {
	if report.Version == 0 {
		report.Version = PromoteReportVersion
	}
	if strings.TrimSpace(report.MainBranch) == "" {
		report.MainBranch = "main"
	}
	report.Gate = normalizePromotionGate(report.Gate)
	if report.KickedBack == nil {
		report.KickedBack = []PromoteKickBackResult{}
	}
	if report.NeedsHuman == nil {
		report.NeedsHuman = []PromoteNeedsHuman{}
	}
	if strings.TrimSpace(report.Promoted.PreProdBranch) == "" {
		report.Promoted.PreProdBranch = report.PreProdBranch
	}
	if strings.TrimSpace(report.Promoted.MainBranch) == "" {
		report.Promoted.MainBranch = report.MainBranch
	}
	if strings.TrimSpace(report.Sync.PreProdBranch) == "" {
		report.Sync.PreProdBranch = report.PreProdBranch
	}
	if strings.TrimSpace(report.Sync.MainBranch) == "" {
		report.Sync.MainBranch = report.MainBranch
	}
	if report.StatePush != nil && report.StatePush.Files == nil {
		report.StatePush.Files = []string{}
	}
	report.Summary.NeedsHumanCount = len(report.NeedsHuman)
	return report
}

func normalizePromotionGate(gate string) string {
	gate = strings.TrimSpace(gate)
	if gate == "" {
		return "human-merge"
	}
	return gate
}

func normalizeKickBackItems(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
