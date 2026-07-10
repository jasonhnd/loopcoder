package reporter

import "strings"

const maxDecisionTextRunes = 220

// DecisionReceipt is the normalized human/machine decision surface layered on
// top of invocation reports. It keeps the product verdict separate from process
// success and keeps the reason distinct from the requested next action.
type DecisionReceipt struct {
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	NextAction string `json:"next_action"`
}

// DecisionFinding is the minimal cross-domain finding shape needed to derive a
// receipt reason without coupling reporter to verifier or audit packages.
type DecisionFinding struct {
	Severity string
	File     string
	Message  string
	Blocking bool
}

// DecisionInput configures normalized receipt construction.
type DecisionInput struct {
	Status             string
	ExplicitReason     string
	Findings           []DecisionFinding
	ConcreteError      string
	FallbackReason     string
	ExplicitNextAction string
	FallbackNextAction string
}

// NormalizeDecision applies the shared reason precedence:
// explicit reason, blocking finding/gate, concrete error, then bounded fallback.
func NormalizeDecision(input DecisionInput) DecisionReceipt {
	status := normalizeDecisionStatus(input.Status)
	reason := firstDecisionText(input.ExplicitReason)
	if reason == "" {
		if finding, ok := highestDecisionFinding(input.Findings, true); ok {
			reason = findingDecisionText(finding)
		}
	}
	if reason == "" && actionableDecisionStatus(status) {
		if finding, ok := highestDecisionFinding(input.Findings, false); ok {
			reason = findingDecisionText(finding)
		}
	}
	if reason == "" {
		reason = firstDecisionText(input.ConcreteError)
	}
	if reason == "" {
		reason = firstDecisionText(input.FallbackReason)
	}
	if reason == "" {
		reason = defaultDecisionReason(status)
	}

	next := firstDecisionText(input.ExplicitNextAction)
	if next == "" {
		next = firstDecisionText(input.FallbackNextAction)
	}
	if next == "" {
		next = defaultDecisionNextAction(status)
	}

	return DecisionReceipt{
		Status:     status,
		Reason:     reason,
		NextAction: next,
	}
}

// BoundDecisionText returns a single-line, bounded receipt field.
func BoundDecisionText(value string) string {
	return firstDecisionText(value)
}

func normalizeDecisionStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	status = strings.ReplaceAll(status, " ", "-")
	if status == "" {
		return "unknown"
	}
	return status
}

func actionableDecisionStatus(status string) bool {
	switch status {
	case "pass", "succeeded", "success", "clean":
		return false
	default:
		return true
	}
}

func highestDecisionFinding(findings []DecisionFinding, blockingOnly bool) (DecisionFinding, bool) {
	bestIndex := -1
	bestRank := 999
	for index, finding := range findings {
		if blockingOnly && !finding.Blocking {
			continue
		}
		if strings.TrimSpace(finding.Message) == "" {
			continue
		}
		rank := decisionSeverityRank(finding.Severity)
		if bestIndex < 0 || rank < bestRank {
			bestIndex = index
			bestRank = rank
		}
	}
	if bestIndex < 0 {
		return DecisionFinding{}, false
	}
	return findings[bestIndex], true
}

func decisionSeverityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "blocking":
		return 0
	case "error", "high":
		return 1
	case "warning", "medium":
		return 2
	case "low":
		return 3
	case "info", "note":
		return 4
	default:
		return 5
	}
}

func findingDecisionText(finding DecisionFinding) string {
	message := firstDecisionText(finding.Message)
	if strings.TrimSpace(finding.File) == "" {
		return message
	}
	return firstDecisionText(strings.TrimSpace(finding.File) + ": " + message)
}

func firstDecisionText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	line, _, _ := strings.Cut(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	line = strings.Join(strings.Fields(line), " ")
	runes := []rune(line)
	if len(runes) <= maxDecisionTextRunes {
		return line
	}
	return strings.TrimSpace(string(runes[:maxDecisionTextRunes])) + " [truncated]"
}

func defaultDecisionReason(status string) string {
	switch status {
	case "pass", "succeeded", "success", "clean":
		return "completed without a blocking report signal"
	case "needs-human":
		return "human judgment is required before continuing"
	case "fail", "failed", "findings":
		return "reported a failing verdict"
	case "timed_out", "timed-out", "timeout":
		return "command timed out"
	case "cancelled":
		return "command was cancelled"
	case "self-reported":
		return "record was self-reported and not independently verified"
	default:
		return "no additional reason reported"
	}
}

func defaultDecisionNextAction(status string) string {
	switch status {
	case "pass", "succeeded", "success", "clean":
		return "continue with the configured next step"
	case "needs-human":
		return "human should decide whether the reported uncertainty is acceptable"
	case "fail", "failed", "findings", "timed_out", "timed-out", "timeout", "cancelled":
		return "inspect the failure and recover or retry before continuing"
	case "self-reported":
		return "inspect the self-reported record before continuing"
	default:
		return "inspect the report before continuing"
	}
}
