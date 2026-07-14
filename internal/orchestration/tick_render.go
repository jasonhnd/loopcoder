package orchestration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/observability"
)

func MarshalTickJSON(report TickReport) ([]byte, error) {
	report = normalizeTickReport(report)
	report = sanitizeTickReportForOutput(report)
	payload := struct {
		SchemaVersion string                 `json:"schema_version"`
		Observability observability.Document `json:"observability"`
		TickReport
	}{
		SchemaVersion: "loopcoder.tick_result.v1",
		Observability: TickObservabilityDocument(report),
		TickReport:    report,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal tick JSON: %w", err)
	}
	return append(data, '\n'), nil
}

func sanitizeTickReportForOutput(report TickReport) TickReport {
	if report.DispatchWave != nil {
		wave := SanitizeDispatchWaveReportForOutput(*report.DispatchWave)
		report.DispatchWave = &wave
	}
	return report
}

func TickObservabilityDocument(report TickReport) observability.Document {
	items := []observability.RenderItem{}
	if report.DispatchWave != nil {
		for _, result := range report.DispatchWave.Results {
			if result.Report == nil {
				continue
			}
			items = append(items, observability.ItemFromReport(fmt.Sprintf("issue-%d", result.Issue), "worker", result.Status, result.Reason, result.NextAction, *result.Report, []observability.SourceRef{{
				Table:         "dispatch_wave",
				RecordID:      result.AttemptPath,
				DeliveryRunID: report.RunID,
				Provenance:    "tick-dispatch-wave",
			}}))
		}
	}
	for _, review := range report.Reviews {
		if review.Report == nil {
			continue
		}
		items = append(items, observability.ItemFromReport(fmt.Sprintf("pr-%d", review.PRNumber), "verifier", review.Verdict, review.Reason, review.NextAction, *review.Report, []observability.SourceRef{{
			Table:         "reviews",
			RecordID:      review.PR,
			DeliveryRunID: report.RunID,
			Provenance:    "tick-review",
		}}))
	}
	return observability.NewDocument("tick", observability.Correlation{DeliveryRunID: report.RunID, Source: "tick"}, items, nil)
}

func RenderTickText(report TickReport) string {
	report = normalizeTickReport(report)
	var out bytes.Buffer

	fmt.Fprintln(&out, "TICK")
	fmt.Fprintf(&out, "Repo: %s\n", report.Repo)
	fmt.Fprintf(&out, "Base branch: %s\n", report.BaseBranch)
	fmt.Fprintf(&out, "Pre-prod branch: %s\n", report.PreProdBranch)
	fmt.Fprintf(&out, "RunId: %s\n", report.RunID)
	fmt.Fprintf(&out, "Status: %s\n", report.Status)
	fmt.Fprintf(&out, "Stop reason: %s\n", report.StopReason)
	fmt.Fprintf(&out, "Started at: %s\n", report.StartedAt)
	fmt.Fprintf(&out, "Finished at: %s\n", report.FinishedAt)

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Compile")
	fmt.Fprintf(&out, "- created=%d updated=%d unchanged=%d closed=%d plan_approval_required=%s\n",
		report.Summary.CompiledCreatedCount,
		report.Summary.CompiledUpdatedCount,
		report.Summary.CompiledUnchangedCount,
		report.Summary.CompiledClosedCount,
		tickYesNo(report.Compile.PlanApprovalRequired),
	)

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Ready set")
	fmt.Fprintf(&out, "- ready=%d blocked=%d\n", report.Summary.ReadyCount, report.Summary.BlockedCount)

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Pending promotion")
	if len(report.PendingPromotion) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, item := range report.PendingPromotion {
			fmt.Fprintf(&out, "- %s %s", formatTickPendingPromotionTarget(item), firstNonEmpty(item.Status, tickStatusPendingPromotion))
			if strings.TrimSpace(item.Branch) != "" {
				fmt.Fprintf(&out, " branch=%s", item.Branch)
			}
			fmt.Fprintln(&out)
			if strings.TrimSpace(item.RunID) != "" {
				fmt.Fprintf(&out, "  run_id: %s\n", item.RunID)
			}
			if strings.TrimSpace(item.Head) != "" {
				fmt.Fprintf(&out, "  head: %s\n", item.Head)
			}
			if strings.TrimSpace(item.SHA) != "" {
				fmt.Fprintf(&out, "  sha: %s\n", item.SHA)
			}
			if strings.TrimSpace(item.URL) != "" {
				fmt.Fprintf(&out, "  url: %s\n", item.URL)
			}
			if strings.TrimSpace(item.Evidence) != "" {
				fmt.Fprintf(&out, "  evidence: %s\n", item.Evidence)
			}
			renderTickConfiguredEvidence(&out, item.ConfiguredEvidence)
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Dispatch")
	if report.DispatchWave == nil || len(report.DispatchWave.Results) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, result := range report.DispatchWave.Results {
			fmt.Fprintf(&out, "- #%d %s\n", result.Issue, result.Status)
			if strings.TrimSpace(result.PR) != "" {
				fmt.Fprintf(&out, "  pr: %s\n", result.PR)
			}
			if strings.TrimSpace(result.Branch) != "" {
				fmt.Fprintf(&out, "  branch: %s\n", result.Branch)
			}
			renderTickConfiguredEvidence(&out, result.ConfiguredEvidence)
			if strings.TrimSpace(result.Error) != "" {
				fmt.Fprintf(&out, "  detail: %s\n", result.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Recoveries")
	if len(report.Recoveries) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, recovered := range report.Recoveries {
			fmt.Fprintf(&out, "- #%d %s\n", recovered.Issue, recovered.Action)
			if strings.TrimSpace(recovered.PR) != "" {
				fmt.Fprintf(&out, "  pr: %s\n", recovered.PR)
			}
			if strings.TrimSpace(recovered.Detail) != "" {
				fmt.Fprintf(&out, "  detail: %s\n", recovered.Detail)
			}
			for _, attempt := range recovered.Attempts {
				fmt.Fprintf(&out, "  attempt %d %s %s\n", attempt.Attempt, attempt.Strategy, attempt.Status)
				if strings.TrimSpace(attempt.PR) != "" {
					fmt.Fprintf(&out, "    pr: %s\n", attempt.PR)
				}
				if strings.TrimSpace(attempt.Branch) != "" {
					fmt.Fprintf(&out, "    branch: %s\n", attempt.Branch)
				}
				if strings.TrimSpace(attempt.Effort) != "" {
					fmt.Fprintf(&out, "    effort: %s\n", attempt.Effort)
				}
				if strings.TrimSpace(attempt.Error) != "" {
					fmt.Fprintf(&out, "    error: %s\n", attempt.Error)
				}
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Reviews")
	if len(report.Reviews) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, review := range report.Reviews {
			target := review.PR
			if review.PRNumber > 0 {
				target = fmt.Sprintf("#%d", review.PRNumber)
			}
			fmt.Fprintf(&out, "- PR %s issue #%d %s\n", target, review.Issue, review.Verdict)
			if strings.TrimSpace(review.SpecConformance) != "" {
				fmt.Fprintf(&out, "  spec_conformance: %s\n", review.SpecConformance)
			}
			if strings.TrimSpace(review.Reason) != "" {
				fmt.Fprintf(&out, "  reason: %s\n", review.Reason)
			}
			if strings.TrimSpace(review.NextAction) != "" {
				fmt.Fprintf(&out, "  next_action: %s\n", review.NextAction)
			}
			if strings.TrimSpace(review.Evidence) != "" {
				fmt.Fprintf(&out, "  evidence: %s\n", review.Evidence)
			}
			renderTickConfiguredEvidence(&out, review.ConfiguredEvidence)
			renderTickRenderedArtifacts(&out, review.RenderedArtifacts)
			if strings.TrimSpace(review.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", review.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Risk gates")
	if len(report.RiskGates) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, gate := range report.RiskGates {
			target := gate.PR
			if gate.PRNumber > 0 {
				target = fmt.Sprintf("#%d", gate.PRNumber)
			}
			fmt.Fprintf(&out, "- PR %s issue #%d %s\n", target, gate.Issue, gate.Status)
			if len(gate.RequiredChecks) > 0 {
				fmt.Fprintf(&out, "  required_checks: %s\n", strings.Join(gate.RequiredChecks, ", "))
			}
			if len(gate.ChangedFiles) > 0 {
				fmt.Fprintf(&out, "  changed_files: %s\n", strings.Join(gate.ChangedFiles, ", "))
			}
			if len(gate.RedLines) > 0 {
				fmt.Fprintf(&out, "  red_lines: %s\n", formatRiskRedLines(gate.RedLines))
			}
			renderTickConfiguredEvidence(&out, gate.ConfiguredEvidence)
			if strings.TrimSpace(gate.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", gate.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Pre-prod merges")
	if len(report.PreProdMerges) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, merged := range report.PreProdMerges {
			target := merged.PR
			if merged.PRNumber > 0 {
				target = fmt.Sprintf("#%d", merged.PRNumber)
			}
			fmt.Fprintf(&out, "- PR %s issue #%d %s branch=%s\n", target, merged.Issue, merged.Status, merged.Branch)
			if strings.TrimSpace(merged.Head) != "" {
				fmt.Fprintf(&out, "  head: %s\n", merged.Head)
			}
			if strings.TrimSpace(merged.SHA) != "" {
				fmt.Fprintf(&out, "  sha: %s\n", merged.SHA)
			}
			if strings.TrimSpace(merged.URL) != "" {
				fmt.Fprintf(&out, "  url: %s\n", merged.URL)
			}
			renderTickConfiguredEvidence(&out, merged.ConfiguredEvidence)
			if strings.TrimSpace(merged.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", merged.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Pre-prod health")
	if len(report.PreProdHealth) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, health := range report.PreProdHealth {
			target := health.PR
			if health.PRNumber > 0 {
				target = fmt.Sprintf("#%d", health.PRNumber)
			}
			fmt.Fprintf(&out, "- PR %s issue #%d %s branch=%s\n", target, health.Issue, health.Status, health.Branch)
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
			renderTickConfiguredEvidence(&out, health.ConfiguredEvidence)
			if strings.TrimSpace(health.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", health.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Pre-prod reverts")
	if len(report.PreProdReverts) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, reverted := range report.PreProdReverts {
			target := reverted.PR
			if reverted.PRNumber > 0 {
				target = fmt.Sprintf("#%d", reverted.PRNumber)
			}
			fmt.Fprintf(&out, "- PR %s issue #%d %s branch=%s\n", target, reverted.Issue, reverted.Status, reverted.Branch)
			if strings.TrimSpace(reverted.RevertedSHA) != "" {
				fmt.Fprintf(&out, "  reverted_sha: %s\n", reverted.RevertedSHA)
			}
			if strings.TrimSpace(reverted.SHA) != "" {
				fmt.Fprintf(&out, "  sha: %s\n", reverted.SHA)
			}
			if strings.TrimSpace(reverted.URL) != "" {
				fmt.Fprintf(&out, "  url: %s\n", reverted.URL)
			}
			renderTickConfiguredEvidence(&out, reverted.ConfiguredEvidence)
			if strings.TrimSpace(reverted.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", reverted.Error)
			}
		}
	}

	renderTickIssueSection(&out, "Needs human", report.NeedsHuman)
	renderTickIssueSection(&out, "Failures", report.Failures)

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

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Next")
	switch report.Status {
	case TickStatusSucceeded:
		fmt.Fprintln(&out, "- Clean passing PRs were integrated into pre-prod; human promotion to main remains separate.")
	case TickStatusNoReadyWork:
		fmt.Fprintln(&out, "- No ready issues were dispatched in this pass.")
	case TickStatusNeedsHuman:
		fmt.Fprintln(&out, "- Resolve the needs-human items before the next unattended pass.")
	default:
		fmt.Fprintln(&out, "- Fix or recover failed items before the next unattended pass.")
	}
	return out.String()
}

func TickExitCode(report TickReport) int {
	switch report.Status {
	case TickStatusSucceeded, TickStatusNoReadyWork:
		return 0
	case TickStatusNeedsHuman:
		return 2
	default:
		return 1
	}
}

func renderTickConfiguredEvidence(out *bytes.Buffer, evidence []config.EvidenceArtifact) {
	evidence = normalizeConfiguredEvidence(evidence)
	for _, item := range evidence {
		parts := make([]string, 0, 4)
		if item.PreviewURL != "" {
			parts = append(parts, "preview_url="+formatTickEvidenceValue(item.PreviewURL))
		}
		if item.ExampleOutput != "" {
			parts = append(parts, "example_output="+formatTickEvidenceValue(item.ExampleOutput))
		}
		if item.TestResults != "" {
			parts = append(parts, "test_results="+formatTickEvidenceValue(item.TestResults))
		}
		if item.PreviewBuild != "" {
			parts = append(parts, "preview_build="+formatTickEvidenceValue(item.PreviewBuild))
		}
		if len(parts) == 0 {
			continue
		}
		fmt.Fprintf(out, "  configured_evidence: %s %s\n", item.ProjectType, strings.Join(parts, " "))
	}
}

func renderTickRenderedArtifacts(out *bytes.Buffer, artifacts []loopreview.RenderedArtifact) {
	artifacts = normalizeRenderedArtifacts(artifacts)
	for _, artifact := range artifacts {
		parts := make([]string, 0, 6)
		if artifact.Path != "" {
			parts = append(parts, "path="+formatTickEvidenceValue(artifact.Path))
		}
		if artifact.DeclaredOutput != "" && artifact.DeclaredOutput != artifact.Path {
			parts = append(parts, "declared_output="+formatTickEvidenceValue(artifact.DeclaredOutput))
		}
		if artifact.Kind != "" {
			parts = append(parts, "kind="+formatTickEvidenceValue(artifact.Kind))
		}
		if artifact.Bytes > 0 {
			parts = append(parts, fmt.Sprintf("bytes=%d", artifact.Bytes))
		}
		if artifact.Summary != "" {
			parts = append(parts, "summary="+formatTickEvidenceValue(artifact.Summary))
		}
		if artifact.Error != "" {
			parts = append(parts, "error="+formatTickEvidenceValue(artifact.Error))
		}
		if len(parts) == 0 {
			continue
		}
		fmt.Fprintf(out, "  rendered_artifact: %s %s %s\n", artifact.Source, artifact.Status, strings.Join(parts, " "))
	}
}

func formatTickEvidenceValue(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}

func renderTickIssueSection(out *bytes.Buffer, title string, issues []TickIssue) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, title)
	if len(issues) == 0 {
		fmt.Fprintln(out, "- none")
		return
	}
	for _, item := range issues {
		target := item.Step
		if item.Issue > 0 {
			target += fmt.Sprintf(" #%d", item.Issue)
		}
		if strings.TrimSpace(item.PR) != "" {
			target += " " + item.PR
		}
		fmt.Fprintf(out, "- %s: %s\n", target, item.Detail)
		renderTickConfiguredEvidence(out, item.ConfiguredEvidence)
	}
}

func formatTickPendingPromotionTarget(item TickPendingPromotion) string {
	target := strings.TrimSpace(item.PR)
	if item.PRNumber > 0 {
		target = fmt.Sprintf("PR #%d", item.PRNumber)
	} else if target != "" {
		target = "PR " + target
	} else if item.Issue > 0 {
		target = fmt.Sprintf("issue #%d", item.Issue)
	} else if strings.TrimSpace(item.SHA) != "" {
		target = "commit " + item.SHA
	} else {
		target = "item"
	}
	if item.Issue > 0 && !strings.EqualFold(target, fmt.Sprintf("issue #%d", item.Issue)) {
		target += fmt.Sprintf(" issue #%d", item.Issue)
	}
	return target
}

func tickYesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
