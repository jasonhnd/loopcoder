package orchestration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/equivalence"
)

type promoteReportJSON struct {
	Version          int                            `json:"version"`
	RepoPath         string                         `json:"repo_path"`
	RunID            string                         `json:"run_id"`
	PreProdBranch    string                         `json:"pre_prod_branch"`
	MainBranch       string                         `json:"main_branch"`
	Gate             string                         `json:"gate"`
	Status           string                         `json:"status"`
	StartedAt        string                         `json:"started_at"`
	FinishedAt       string                         `json:"finished_at"`
	KickedBack       []PromoteKickBackResult        `json:"kicked_back"`
	NeedsHuman       []PromoteNeedsHuman            `json:"needs_human"`
	ToggleInventory  *PromoteToggleInventory        `json:"toggle_inventory,omitempty"`
	GoNoGoPanel      *PromoteGoNoGoPanel            `json:"go_no_go_panel,omitempty"`
	Promoted         PromoteMainResult              `json:"promoted"`
	ProductionHealth *PromoteProductionHealthResult `json:"production_health,omitempty"`
	Rollback         *PromoteRollbackResult         `json:"rollback,omitempty"`
	Sync             PromoteSyncResult              `json:"sync"`
	StatePush        *PromoteStatePush              `json:"state_push,omitempty"`
	Summary          PromoteSummary                 `json:"summary"`
}

func (report PromoteReport) MarshalJSON() ([]byte, error) {
	wire := promoteReportJSON{
		Version:          report.Version,
		RepoPath:         report.RepoPath,
		RunID:            report.RunID,
		PreProdBranch:    report.PreProdBranch,
		MainBranch:       report.MainBranch,
		Gate:             report.Gate,
		Status:           report.Status,
		StartedAt:        report.StartedAt,
		FinishedAt:       report.FinishedAt,
		KickedBack:       report.KickedBack,
		NeedsHuman:       report.NeedsHuman,
		GoNoGoPanel:      report.GoNoGoPanel,
		Promoted:         report.Promoted,
		ProductionHealth: report.ProductionHealth,
		Rollback:         report.Rollback,
		Sync:             report.Sync,
		StatePush:        report.StatePush,
		Summary:          report.Summary,
	}
	if promoteToggleInventoryHasItems(report.ToggleInventory) {
		inventory := report.ToggleInventory
		wire.ToggleInventory = &inventory
	}
	return json.Marshal(wire)
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

	renderPromoteGoNoGoPanel(&out, report.GoNoGoPanel)

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
	fmt.Fprintln(&out, "Toggle inventory")
	if len(report.ToggleInventory.FlipOn) == 0 {
		fmt.Fprintln(&out, "- flip_on: none")
	} else {
		for _, item := range report.ToggleInventory.FlipOn {
			fmt.Fprintf(&out, "- flip_on %s build_tag=%s\n", item.SliceRef, item.BuildTag)
		}
	}
	if len(report.ToggleInventory.LeaveDark) == 0 {
		fmt.Fprintln(&out, "- leave_dark: none")
	} else {
		for _, item := range report.ToggleInventory.LeaveDark {
			fmt.Fprintf(&out, "- leave_dark %s build_tag=%s\n", item.SliceRef, item.BuildTag)
			if strings.TrimSpace(item.Reason) != "" {
				fmt.Fprintf(&out, "  reason: %s\n", item.Reason)
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

	if report.ProductionHealth != nil {
		health := report.ProductionHealth
		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "Production health")
		fmt.Fprintf(&out, "- %s status=%s\n", health.Branch, health.Status)
		if strings.TrimSpace(health.HeadSHA) != "" {
			fmt.Fprintf(&out, "  head_sha: %s\n", health.HeadSHA)
		}
		if strings.TrimSpace(health.MergeSHA) != "" {
			fmt.Fprintf(&out, "  merge_sha: %s\n", health.MergeSHA)
		}
		if len(health.RequiredChecks) > 0 {
			fmt.Fprintf(&out, "  required_checks: %s\n", strings.Join(health.RequiredChecks, ", "))
		}
		if len(health.Problems) > 0 {
			fmt.Fprintf(&out, "  problems: %s\n", strings.Join(health.Problems, ", "))
		}
		if strings.TrimSpace(health.Error) != "" {
			fmt.Fprintf(&out, "  error: %s\n", health.Error)
		}
	}

	if report.Rollback != nil {
		rollback := report.Rollback
		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "Production rollback")
		fmt.Fprintf(&out, "- %s %s\n", rollback.MainBranch, rollback.Status)
		if strings.TrimSpace(rollback.MergeCommit) != "" {
			fmt.Fprintf(&out, "  merge_commit: %s\n", rollback.MergeCommit)
		}
		if strings.TrimSpace(rollback.PriorStableCommit) != "" {
			fmt.Fprintf(&out, "  prior_stable_commit: %s\n", rollback.PriorStableCommit)
		}
		if strings.TrimSpace(rollback.RevertSHA) != "" {
			fmt.Fprintf(&out, "  revert_sha: %s\n", rollback.RevertSHA)
		}
		if strings.TrimSpace(rollback.URL) != "" {
			fmt.Fprintf(&out, "  url: %s\n", rollback.URL)
		}
		if strings.TrimSpace(rollback.Error) != "" {
			fmt.Fprintf(&out, "  error: %s\n", rollback.Error)
		}
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

func renderPromoteGoNoGoPanel(out *bytes.Buffer, panel *PromoteGoNoGoPanel) {
	if panel == nil {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Go/no-go panel")
	if panel.Reconciliation != nil {
		report := panel.Reconciliation
		fmt.Fprintf(out, "- reconciliation status=%s matched=%d old_only=%d new_only=%d mismatches=%d\n",
			report.Status,
			report.MatchedCount,
			report.OldOnlyCount,
			report.NewOnlyCount,
			report.MismatchCount,
		)
		for _, unmatched := range report.Unmatched {
			fmt.Fprintf(out, "  unmatched %s side=%s: %s\n", unmatched.Key, unmatched.Side, unmatched.Detail)
		}
		for _, matched := range report.Matched {
			if matched.Status == "" || matched.Status == equivalence.StatusPass {
				continue
			}
			fmt.Fprintf(out, "  matched %s status=%s\n", matched.Key, matched.Status)
		}
	}
	if panel.ToggleInventory != nil {
		fmt.Fprintf(out, "- toggle inventory flip_on=%d leave_dark=%d\n", len(panel.ToggleInventory.FlipOn), len(panel.ToggleInventory.LeaveDark))
		for _, item := range panel.ToggleInventory.FlipOn {
			fmt.Fprintf(out, "  flip_on %s build_tag=%s\n", item.SliceRef, item.BuildTag)
		}
		for _, item := range panel.ToggleInventory.LeaveDark {
			fmt.Fprintf(out, "  leave_dark %s build_tag=%s\n", item.SliceRef, item.BuildTag)
			if strings.TrimSpace(item.Reason) != "" {
				fmt.Fprintf(out, "    reason: %s\n", item.Reason)
			}
		}
	}
	if len(panel.NeedsHuman) > 0 {
		fmt.Fprintf(out, "- needs-human items=%d\n", len(panel.NeedsHuman))
		for _, item := range panel.NeedsHuman {
			target := item.Item
			if item.PRNumber > 0 {
				target = fmt.Sprintf("PR #%d", item.PRNumber)
			}
			fmt.Fprintf(out, "  %s %s: %s\n", item.Step, target, item.Detail)
		}
	}
	if len(panel.Failed) > 0 {
		fmt.Fprintf(out, "- failed items=%d\n", len(panel.Failed))
		for _, item := range panel.Failed {
			target := item.Item
			if item.PRNumber > 0 {
				target = fmt.Sprintf("PR #%d", item.PRNumber)
			}
			fmt.Fprintf(out, "  %s %s %s: %s\n", item.Step, target, item.Status, item.Detail)
		}
	}
}
