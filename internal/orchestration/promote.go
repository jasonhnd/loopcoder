package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

const (
	PromoteReportVersion = 1

	PromoteStatusSucceeded = "succeeded"
	PromoteStatusFailed    = "failed"
)

type PromotionWriter interface {
	KickBackFromPreProd(ctx context.Context, item, preProdBranch string) (gh.PreProdKickBackResult, error)
	PromotePreProdToMain(ctx context.Context, preProdBranch string) (gh.MainPromotionResult, error)
	SyncPreProdFromMain(ctx context.Context, preProdBranch string) (gh.PreProdSyncResult, error)
}

type PromoteOptions struct {
	Writer        PromotionWriter
	RepoPath      string
	PreProdBranch string
	Gate          string
	KickBackItems []string
}

type PromoteReport struct {
	Version       int                     `json:"version"`
	RepoPath      string                  `json:"repo_path"`
	PreProdBranch string                  `json:"pre_prod_branch"`
	MainBranch    string                  `json:"main_branch"`
	Gate          string                  `json:"gate"`
	Status        string                  `json:"status"`
	KickedBack    []PromoteKickBackResult `json:"kicked_back"`
	Promoted      PromoteMainResult       `json:"promoted"`
	Sync          PromoteSyncResult       `json:"sync"`
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

type PromoteSummary struct {
	KickedBackCount int `json:"kicked_back_count"`
	PromotedCount   int `json:"promoted_count"`
	FailureCount    int `json:"failure_count"`
}

func Promote(ctx context.Context, opts PromoteOptions) (PromoteReport, error) {
	opts.PreProdBranch = strings.TrimSpace(opts.PreProdBranch)
	opts.Gate = normalizePromotionGate(opts.Gate)
	report := PromoteReport{
		Version:       PromoteReportVersion,
		RepoPath:      opts.RepoPath,
		PreProdBranch: opts.PreProdBranch,
		MainBranch:    "main",
		Gate:          opts.Gate,
		Status:        PromoteStatusSucceeded,
		KickedBack:    []PromoteKickBackResult{},
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
			return report, nil
		}
		kick.PRNumber = kicked.PRNumber
		kick.Branch = firstNonEmpty(kicked.Branch, opts.PreProdBranch)
		kick.RevertedSHA = kicked.RevertedSHA
		kick.SHA = kicked.SHA
		kick.URL = kicked.URL
		report.KickedBack = append(report.KickedBack, kick)
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
		return report, nil
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
		return report, nil
	}
	report.Sync.PreProdBranch = firstNonEmpty(synced.PreProdBranch, opts.PreProdBranch)
	report.Sync.MainBranch = firstNonEmpty(synced.MainBranch, "main")
	report.Sync.SHA = synced.SHA
	report.Sync.URL = synced.URL
	return report, nil
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
	fmt.Fprintf(&out, "Status: %s\n", report.Status)

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
