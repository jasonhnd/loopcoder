package orchestration

import (
	"strings"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func normalizeTickReport(report TickReport) TickReport {
	if report.Reviews == nil {
		report.Reviews = []TickReviewResult{}
	}
	for i := range report.Reviews {
		if report.Reviews[i].Findings == nil {
			report.Reviews[i].Findings = []loopreview.Finding{}
		}
		report.Reviews[i].RenderedArtifacts = normalizeRenderedArtifacts(report.Reviews[i].RenderedArtifacts)
	}
	if report.RiskGates == nil {
		report.RiskGates = []TickRiskGateResult{}
	}
	for i := range report.RiskGates {
		if report.RiskGates[i].RequiredChecks == nil {
			report.RiskGates[i].RequiredChecks = []string{}
		}
		if report.RiskGates[i].ChangedFiles == nil {
			report.RiskGates[i].ChangedFiles = []string{}
		}
		if report.RiskGates[i].Checks == nil {
			report.RiskGates[i].Checks = []gh.Check{}
		}
		if report.RiskGates[i].RedLines == nil {
			report.RiskGates[i].RedLines = []RiskRedLine{}
		}
	}
	if report.PreProdMerges == nil {
		report.PreProdMerges = []TickPreProdMergeResult{}
	}
	if report.PreProdHealth == nil {
		report.PreProdHealth = []TickPreProdHealthResult{}
	}
	for i := range report.PreProdHealth {
		if report.PreProdHealth[i].RequiredChecks == nil {
			report.PreProdHealth[i].RequiredChecks = []string{}
		}
		if report.PreProdHealth[i].Checks == nil {
			report.PreProdHealth[i].Checks = []gh.Check{}
		}
		if report.PreProdHealth[i].Problems == nil {
			report.PreProdHealth[i].Problems = []string{}
		}
	}
	if report.PreProdReverts == nil {
		report.PreProdReverts = []TickPreProdRevertResult{}
	}
	report.PendingPromotion = normalizeTickPendingPromotionItems(report.PendingPromotion, report.PreProdBranch, "")
	if report.NeedsHuman == nil {
		report.NeedsHuman = []TickIssue{}
	}
	if report.Failures == nil {
		report.Failures = []TickIssue{}
	}
	if report.StatePush != nil && report.StatePush.Files == nil {
		report.StatePush.Files = []string{}
	}
	report.Summary = summarizeTick(report)
	return report
}

func summarizeTick(report TickReport) TickSummary {
	summary := TickSummary{
		CompiledCreatedCount:   report.Compile.Summary.CreatedCount,
		CompiledUpdatedCount:   report.Compile.Summary.UpdatedCount,
		CompiledUnchangedCount: report.Compile.Summary.UnchangedCount,
		CompiledClosedCount:    report.Compile.Summary.ClosedCount,
		ReadyCount:             len(report.ReadySet.Ready),
		BlockedCount:           len(report.ReadySet.Blocked),
		PendingPromotionCount:  len(report.PendingPromotion),
		NeedsHumanCount:        len(report.NeedsHuman),
		FailureCount:           len(report.Failures),
	}
	if report.DispatchWave != nil {
		for _, result := range report.DispatchWave.Results {
			if result.Status == DispatchWaveStatusSucceeded && strings.TrimSpace(result.PR) != "" {
				summary.DispatchedPRCount++
			}
		}
	}
	for _, review := range report.Reviews {
		switch review.Verdict {
		case loopreview.VerdictPass:
			summary.ReviewPassCount++
		case loopreview.VerdictFail:
			summary.ReviewFailCount++
		case loopreview.VerdictNeedsHuman:
			summary.ReviewNeedsHumanCount++
		}
	}
	for _, gate := range report.RiskGates {
		switch gate.Status {
		case RiskGateStatusClean:
			summary.RiskGateCleanCount++
		case RiskGateStatusNeedsHuman:
			summary.RiskGateNeedsHumanCount++
		}
	}
	for _, merged := range report.PreProdMerges {
		if merged.Status == TickStatusSucceeded {
			summary.PreProdMergeCount++
		}
	}
	for _, reverted := range report.PreProdReverts {
		if reverted.Status == TickStatusSucceeded {
			summary.PreProdRevertCount++
		}
	}
	return summary
}

func attachTickConfiguredEvidence(report *TickReport, evidence []config.EvidenceArtifact) {
	evidence = normalizeConfiguredEvidence(evidence)
	if len(evidence) == 0 {
		return
	}
	if report.DispatchWave != nil {
		for i := range report.DispatchWave.Results {
			result := &report.DispatchWave.Results[i]
			if tickHasReportTarget(result.Issue, result.PR) {
				result.ConfiguredEvidence = copyConfiguredEvidence(evidence)
			}
		}
	}
	for i := range report.Reviews {
		item := &report.Reviews[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.RiskGates {
		item := &report.RiskGates[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.PreProdMerges {
		item := &report.PreProdMerges[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.PreProdHealth {
		item := &report.PreProdHealth[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.PreProdReverts {
		item := &report.PreProdReverts[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.NeedsHuman {
		item := &report.NeedsHuman[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.Failures {
		item := &report.Failures[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
}

func normalizeConfiguredEvidence(evidence []config.EvidenceArtifact) []config.EvidenceArtifact {
	out := make([]config.EvidenceArtifact, 0, len(evidence))
	for _, item := range evidence {
		item.ProjectType = strings.TrimSpace(item.ProjectType)
		item.PreviewURL = strings.TrimSpace(item.PreviewURL)
		item.ExampleOutput = strings.TrimSpace(item.ExampleOutput)
		item.TestResults = strings.TrimSpace(item.TestResults)
		item.PreviewBuild = strings.TrimSpace(item.PreviewBuild)
		if item.ProjectType == "" || tickConfiguredEvidenceEmpty(item) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func tickConfiguredEvidenceEmpty(item config.EvidenceArtifact) bool {
	return strings.TrimSpace(item.PreviewURL) == "" &&
		strings.TrimSpace(item.ExampleOutput) == "" &&
		strings.TrimSpace(item.TestResults) == "" &&
		strings.TrimSpace(item.PreviewBuild) == ""
}

func copyConfiguredEvidence(evidence []config.EvidenceArtifact) []config.EvidenceArtifact {
	return append([]config.EvidenceArtifact(nil), evidence...)
}

func copyRenderedArtifacts(artifacts []loopreview.RenderedArtifact) []loopreview.RenderedArtifact {
	return append([]loopreview.RenderedArtifact(nil), artifacts...)
}

func tickHasReportTarget(issue int, pr string) bool {
	return issue > 0 || strings.TrimSpace(pr) != ""
}

func normalizeRenderedArtifacts(artifacts []loopreview.RenderedArtifact) []loopreview.RenderedArtifact {
	out := make([]loopreview.RenderedArtifact, 0, len(artifacts))
	for _, item := range artifacts {
		item.Source = strings.TrimSpace(item.Source)
		item.Status = strings.TrimSpace(item.Status)
		item.DeclaredOutput = strings.TrimSpace(item.DeclaredOutput)
		item.Path = strings.TrimSpace(item.Path)
		item.Kind = strings.TrimSpace(item.Kind)
		item.MediaType = strings.TrimSpace(item.MediaType)
		item.SHA256 = strings.TrimSpace(item.SHA256)
		item.Summary = strings.TrimSpace(item.Summary)
		item.Error = strings.TrimSpace(item.Error)
		if item.Source == "" && item.Path == "" && item.Summary == "" && item.Error == "" {
			continue
		}
		if item.Status == "" {
			item.Status = "available"
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
