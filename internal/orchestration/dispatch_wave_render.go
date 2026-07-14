package orchestration

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/observability"
	"github.com/jasonhnd/loopcoder/internal/reporter"
)

func RenderDispatchWaveText(report DispatchWaveReport) string {
	report = SanitizeDispatchWaveReportForOutput(report)
	var out bytes.Buffer
	succeeded, failed, skipped, needsHuman := dispatchWaveCounts(report.Results)
	dispatched := succeeded + failed

	fmt.Fprintln(&out, "DISPATCH WAVE")
	fmt.Fprintf(&out, "Repo: %s\n", report.Repo)
	fmt.Fprintf(&out, "Base branch: %s\n", report.BaseBranch)
	fmt.Fprintf(&out, "RunId: %s\n", report.RunID)
	fmt.Fprintf(&out, "Issues requested: %s\n", formatIssueList(report.IssuesRequested))
	fmt.Fprintf(&out, "Issues dispatched: %d\n", dispatched)
	fmt.Fprintf(&out, "Issues skipped: %d\n", skipped)
	fmt.Fprintf(&out, "Issues needs-human: %d\n", needsHuman)
	fmt.Fprintf(&out, "Started at: %s\n", report.StartedAt)
	fmt.Fprintf(&out, "Finished at: %s\n", report.FinishedAt)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Results")
	if len(report.Results) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, result := range report.Results {
			fmt.Fprintf(&out, "- #%d %s\n", result.Issue, result.Status)
			if strings.TrimSpace(result.Branch) != "" {
				fmt.Fprintf(&out, "  branch: %s\n", result.Branch)
			}
			if strings.TrimSpace(result.PR) != "" {
				fmt.Fprintf(&out, "  pr: %s\n", result.PR)
			}
			if result.Report != nil {
				fmt.Fprintf(&out, "  report: %s\n", formatDispatchWaveReport(*result.Report))
			}
			if strings.TrimSpace(result.AttemptPath) != "" {
				fmt.Fprintf(&out, "  attempt_id: %s\n", result.AttemptPath)
			}
			if strings.TrimSpace(result.RecoveryContextPath) != "" {
				fmt.Fprintf(&out, "  recovery_id: %s\n", result.RecoveryContextPath)
			}
			if strings.TrimSpace(result.Reason) != "" {
				fmt.Fprintf(&out, "  reason: %s\n", result.Reason)
			}
			if strings.TrimSpace(result.NextAction) != "" {
				fmt.Fprintf(&out, "  next_action: %s\n", result.NextAction)
			}
			if strings.TrimSpace(result.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", result.Error)
			}
		}
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Next")
	fmt.Fprintln(&out, "- Verify successful PRs before calling them merge-eligible.")
	fmt.Fprintln(&out, "- Recover failed attempts before retrying the issue.")
	fmt.Fprintln(&out, "- Run resume after human review, merge, or interruption.")
	return out.String()
}

func RenderDispatchWaveIssueCompletion(result DispatchWaveIssueResult, pretty string) string {
	result = sanitizeDispatchWaveIssueResultForOutput(result)
	var out bytes.Buffer
	fmt.Fprintf(&out, "DISPATCH WAVE WORKER #%d %s\n", result.Issue, result.Status)
	if strings.TrimSpace(result.Branch) != "" {
		fmt.Fprintf(&out, "branch: %s\n", result.Branch)
	}
	if strings.TrimSpace(result.PR) != "" {
		fmt.Fprintf(&out, "pr: %s\n", result.PR)
	}
	if strings.TrimSpace(result.AttemptPath) != "" {
		fmt.Fprintf(&out, "attempt_id: %s\n", result.AttemptPath)
	}
	if strings.TrimSpace(result.RecoveryContextPath) != "" {
		fmt.Fprintf(&out, "recovery_id: %s\n", result.RecoveryContextPath)
	}
	if strings.TrimSpace(result.Reason) != "" {
		fmt.Fprintf(&out, "reason: %s\n", result.Reason)
	}
	if strings.TrimSpace(result.NextAction) != "" {
		fmt.Fprintf(&out, "next_action: %s\n", result.NextAction)
	}
	if strings.TrimSpace(result.Error) != "" {
		fmt.Fprintf(&out, "error: %s\n", result.Error)
	}
	pretty = strings.TrimRight(pretty, "\r\n")
	if pretty != "" {
		fmt.Fprintln(&out, pretty)
	}
	fmt.Fprintln(&out)
	return out.String()
}

func SanitizeDispatchWaveReportForOutput(report DispatchWaveReport) DispatchWaveReport {
	if len(report.Results) == 0 {
		return report
	}
	report.Results = append([]DispatchWaveIssueResult(nil), report.Results...)
	for i := range report.Results {
		report.Results[i] = sanitizeDispatchWaveIssueResultForOutput(report.Results[i])
	}
	return report
}

func sanitizeDispatchWaveIssueResultForOutput(result DispatchWaveIssueResult) DispatchWaveIssueResult {
	if strings.TrimSpace(result.AttemptPath) != "" {
		result.AttemptPath = observability.StableRecordID(result.AttemptPath)
	}
	if strings.TrimSpace(result.RecoveryContextPath) != "" {
		result.RecoveryContextPath = observability.StableRecordID(result.RecoveryContextPath)
	}
	return result
}

func formatDispatchWaveReport(record reporter.Report) string {
	return fmt.Sprintf(
		"provider=%s model=%s source=%s permission=%s duration=%s tokens input=%s output=%s total=%s verified=%t",
		record.Provider,
		reporter.ModelDepthDisplay(record.Model, record.Effort),
		record.ModelSource,
		record.Permission,
		formatDispatchWaveDuration(record.DurationMS),
		formatDispatchWaveToken(record.Usage.InputTokens),
		formatDispatchWaveToken(record.Usage.OutputTokens),
		formatDispatchWaveToken(record.Usage.TotalTokens),
		record.Verified,
	)
}

func formatDispatchWaveDuration(durationMS int64) string {
	return (time.Duration(durationMS) * time.Millisecond).String()
}

func formatDispatchWaveToken(value *int64) string {
	if value == nil {
		return "not reported"
	}
	return fmt.Sprintf("%d", *value)
}

func DispatchWaveHasFailures(report DispatchWaveReport) bool {
	for _, result := range report.Results {
		if result.Status == DispatchWaveStatusFailed ||
			result.Status == DispatchWaveStatusNeedsHuman ||
			result.Status == DispatchWaveStatusCancelled ||
			result.Status == DispatchWaveStatusTimedOut ||
			result.Status == DispatchWaveStatusAbandoned {
			return true
		}
	}
	return false
}

func dispatchWaveCounts(results []DispatchWaveIssueResult) (succeeded, failed, skipped, needsHuman int) {
	for _, result := range results {
		switch result.Status {
		case DispatchWaveStatusSucceeded:
			succeeded++
		case DispatchWaveStatusFailed, DispatchWaveStatusCancelled, DispatchWaveStatusTimedOut, DispatchWaveStatusAbandoned:
			failed++
		case DispatchWaveStatusSkipped:
			skipped++
		case DispatchWaveStatusNeedsHuman:
			needsHuman++
		}
	}
	return succeeded, failed, skipped, needsHuman
}

func formatIssueList(numbers []int) string {
	if len(numbers) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(numbers))
	for _, number := range numbers {
		parts = append(parts, fmt.Sprintf("#%d", number))
	}
	return strings.Join(parts, ", ")
}
