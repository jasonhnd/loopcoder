package audit

import (
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/reporter"
)

func DecisionReceipt(result Result) reporter.DecisionReceipt {
	result = Finalize(result)
	input := reporter.DecisionInput{
		Status:             result.Verdict,
		Findings:           auditDecisionFindings(result),
		ConcreteError:      firstRuntimeFailure(result.RuntimeFailures),
		FallbackReason:     auditFallbackReason(result),
		FallbackNextAction: auditFallbackNextAction(result.Verdict),
	}
	if len(result.NeedsHuman) > 0 {
		item := result.NeedsHuman[0]
		input.ExplicitReason = strings.TrimSpace(item.Reason)
		if strings.TrimSpace(item.Layer) != "" && input.ExplicitReason != "" {
			input.ExplicitReason = item.Layer + ": " + input.ExplicitReason
		}
	}
	return reporter.NormalizeDecision(input)
}

func auditDecisionFindings(result Result) []reporter.DecisionFinding {
	findings := make([]reporter.DecisionFinding, 0, len(result.Findings))
	for _, finding := range result.Findings {
		findings = append(findings, reporter.DecisionFinding{
			Severity: finding.Severity,
			File:     findingLocation(finding),
			Message:  finding.Message,
			Blocking: !finding.Waived && findingFailsGate(finding, result.Threshold),
		})
	}
	return findings
}

func findingLocation(finding Finding) string {
	location := strings.TrimSpace(finding.File)
	if location == "" {
		return ""
	}
	if finding.Line > 0 {
		return fmt.Sprintf("%s:%d", location, finding.Line)
	}
	return location
}

func firstRuntimeFailure(failures []string) string {
	for _, failure := range failures {
		if strings.TrimSpace(failure) != "" {
			return failure
		}
	}
	return ""
}

func auditFallbackReason(result Result) string {
	switch result.Verdict {
	case VerdictClean:
		return "audit completed without gate findings"
	case VerdictFindings:
		return "audit reported gate findings"
	case VerdictNeedsHuman:
		return "human judgment is required before continuing"
	default:
		return "audit returned an unrecognized verdict"
	}
}

func auditFallbackNextAction(verdict string) string {
	switch verdict {
	case VerdictClean:
		return "continue with the configured release gate"
	case VerdictFindings:
		return "fix or waive the gate findings before continuing"
	case VerdictNeedsHuman:
		return "human should decide whether the audit uncertainty is acceptable"
	default:
		return "inspect the audit result before continuing"
	}
}
