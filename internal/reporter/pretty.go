package reporter

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PrettyMode selects the human-oriented report rendering style.
type PrettyMode int

const (
	// PrettyModeEmoji renders the preferred human form with a status emoji.
	PrettyModeEmoji PrettyMode = iota
	// PrettyModePlain renders the plain ASCII fallback with no emoji or ANSI.
	PrettyModePlain
)

// PrettyOptions configures human-oriented report rendering.
type PrettyOptions struct {
	Mode               PrettyMode
	Status             string
	PR                 string
	BlockingDefects    *int
	Reason             string
	SpecConformance    string
	Findings           []PrettyFinding
	ShowFindingDetails bool
	DetailCommand      string
	RawJSONCommand     string
	Next               []string
}

// PrettyFinding is the rendering-only finding shape used by human receipts.
// It intentionally stays separate from provider/verifier JSON schemas.
type PrettyFinding struct {
	Severity string
	File     string
	Note     string
}

// Pretty renders a human-oriented, multi-line report summary.
//
// Pretty output is not a machine parse target. Use CanonicalJSON or Header for
// durable machine or greppable output.
func (r Report) Pretty(options PrettyOptions) string {
	model := formatPrettyModel(r.Model, r.Effort, r.ModelSource)
	status := buildPrettyReceiptStatus(r, options)
	title := fmt.Sprintf("loopcoder report: %s %s", prettyValue(string(r.Role)), status.label)
	if options.Mode != PrettyModePlain {
		title = status.icon + " " + title
	}

	lines := []string{
		title,
		"",
		"Target",
	}
	lines = append(lines, targetLines(r, options.PR)...)
	lines = append(lines,
		"",
		"Verdict",
		"- status: "+status.value,
		"- blocking defects: "+formatBlockingDefects(r, options),
		"- reason: "+prettyReason(r, options, status),
		"",
		"Review summary",
		"- acceptance criteria: "+formatAcceptanceCriteria(options.SpecConformance),
		"- regressions found: "+formatRegressions(status, options.Findings),
		"- findings: "+formatFindingCounts(options.Findings),
	)
	if options.ShowFindingDetails && len(options.Findings) > 0 {
		lines = append(lines, findingDetailLines(options.Findings)...)
	}
	lines = append(lines,
		"",
		"Run",
		fmt.Sprintf("- %s: %s / %s / %s", prettyValue(string(r.Role)), prettyProviderDisplay(r.Provider), model, prettyValue(r.Effort)),
		"- permission: "+prettyValue(string(r.Permission)),
		"- action: "+strconv.Quote(r.Action),
		"- exit: "+strconv.Itoa(r.ExitCode),
		"- duration: "+formatPrettyDuration(r.DurationMS),
		"- tokens: "+formatPrettyUsage(r.Usage),
		"- started: "+formatPrettyTimestamp(r.StartedAt),
		"- ended: "+formatPrettyTimestamp(r.EndedAt),
		"- verified: "+strconv.FormatBool(r.Verified),
		"",
		"Next",
	)
	lines = append(lines, nextLines(r, status, options)...)

	return strings.Join(lines, "\n")
}

type prettyReceiptStatus struct {
	label string
	value string
	icon  string
}

func buildPrettyReceiptStatus(r Report, options PrettyOptions) prettyReceiptStatus {
	status := strings.TrimSpace(options.Status)
	if status == "" {
		status = inferredStatus(r)
	}
	normalized := normalizeStatus(status)
	switch normalized {
	case "pass":
		return prettyReceiptStatus{label: "pass", value: "pass", icon: "\u2705"}
	case "succeeded", "success":
		return prettyReceiptStatus{label: "succeeded", value: "succeeded", icon: "\u2705"}
	case "needs-human":
		return prettyReceiptStatus{label: "needs human", value: "needs-human", icon: "\u26a0\ufe0f"}
	case "failed", "fail":
		if normalized == "fail" {
			return prettyReceiptStatus{label: "fail", value: "fail", icon: "\u274c"}
		}
		return prettyReceiptStatus{label: "failed", value: "failed", icon: "\u274c"}
	case "cancelled":
		return prettyReceiptStatus{label: "cancelled", value: "cancelled", icon: "\u274c"}
	case "timed-out", "timed_out", "timeout":
		return prettyReceiptStatus{label: "timed out", value: "timed_out", icon: "\u274c"}
	case "self-reported":
		return prettyReceiptStatus{label: "self reported", value: "self-reported", icon: "\u26a0\ufe0f"}
	default:
		label := strings.ReplaceAll(normalized, "-", " ")
		label = strings.ReplaceAll(label, "_", " ")
		return prettyReceiptStatus{label: prettyValue(label), value: prettyValue(normalized), icon: "\u26a0\ufe0f"}
	}
}

func inferredStatus(r Report) string {
	if r.ExitCode != 0 {
		return "failed"
	}
	if r.Role == RoleConductor && !r.Verified {
		return "self-reported"
	}
	return "succeeded"
}

func normalizeStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	status = strings.ReplaceAll(status, " ", "-")
	return status
}

func targetLines(r Report, pr string) []string {
	lines := []string{"- work ID: " + prettyValue(r.WorkID)}
	if r.Issue > 0 {
		lines = append(lines, "- issue: #"+strconv.Itoa(r.Issue))
	} else {
		lines = append(lines, "- issue: "+prettyValue(""))
	}
	if strings.TrimSpace(pr) != "" {
		lines = append(lines, "- PR: "+pr)
	}
	if strings.TrimSpace(r.Branch) != "" {
		lines = append(lines, "- branch: "+r.Branch)
	}
	if strings.TrimSpace(r.Worktree) != "" {
		lines = append(lines, "- worktree: "+r.Worktree)
	}
	if r.Round > 0 {
		lines = append(lines, "- round: "+strconv.Itoa(r.Round))
	}
	return lines
}

func prettyValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unset"
	}
	return value
}

func prettyProviderDisplay(provider string) string {
	provider = strings.TrimSpace(provider)
	switch provider {
	case "codex":
		return "OpenAI Codex / codex"
	case "claude":
		return "Anthropic / claude"
	case "gemini":
		return "Google / gemini"
	case "antigravity":
		return "Google Antigravity / antigravity"
	default:
		return prettyValue(provider)
	}
}

func formatPrettyModel(model, effort string, source ModelSource) string {
	suffix := prettyValue(string(source))
	return fmt.Sprintf("%s (%s)", prettyValue(ModelDepthDisplay(model, effort)), suffix)
}

func formatPrettyTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return prettyValue(value)
	}
	return parsed.In(time.Local).Format("2006-01-02 15:04:05 MST")
}

func formatPrettyDuration(durationMS int64) string {
	return formatCompactPrettyDuration(durationMS)
}

func formatCompactPrettyDuration(durationMS int64) string {
	sign := ""
	if durationMS < 0 {
		sign = "-"
		durationMS = -durationMS
	}
	totalTenths := (durationMS + 50) / 100
	hours := totalTenths / 36000
	totalTenths %= 36000
	minutes := totalTenths / 600
	totalTenths %= 600
	seconds := totalTenths / 10
	tenths := totalTenths % 10

	switch {
	case hours > 0:
		return fmt.Sprintf("%s%dh%dm%d.%ds", sign, hours, minutes, seconds, tenths)
	case minutes > 0:
		return fmt.Sprintf("%s%dm%d.%ds", sign, minutes, seconds, tenths)
	default:
		return fmt.Sprintf("%s%d.%ds", sign, seconds, tenths)
	}
}

func formatPrettyUsage(usage Usage) string {
	var parts []string
	if usage.InputTokens != nil {
		parts = append(parts, fmt.Sprintf("input=%s", formatPrettyInt(*usage.InputTokens)))
	}
	if usage.OutputTokens != nil {
		parts = append(parts, fmt.Sprintf("output=%s", formatPrettyInt(*usage.OutputTokens)))
	}
	if usage.TotalTokens != nil {
		parts = append(parts, fmt.Sprintf("total=%s", formatPrettyInt(*usage.TotalTokens)))
	} else if usage.InputTokens != nil && usage.OutputTokens != nil {
		parts = append(parts, fmt.Sprintf("total=%s", formatPrettyInt(*usage.InputTokens+*usage.OutputTokens)))
	}
	if len(parts) == 0 {
		return "unset"
	}
	return strings.Join(parts, "  ")
}

func formatBlockingDefects(r Report, options PrettyOptions) string {
	if options.BlockingDefects != nil {
		return strconv.Itoa(*options.BlockingDefects)
	}
	if r.ExitCode != 0 {
		return "unknown"
	}
	return "0"
}

func prettyReason(r Report, options PrettyOptions, status prettyReceiptStatus) string {
	if reason := strings.TrimSpace(options.Reason); reason != "" {
		return firstPrettyLine(reason)
	}
	switch status.value {
	case "pass", "succeeded":
		return "completed without a blocking report signal"
	case "needs-human":
		return "human judgment is required before continuing"
	case "fail", "failed":
		return "command exited non-zero or reported failure"
	case "timed_out":
		return "command timed out"
	case "cancelled":
		return "command was cancelled"
	case "self-reported":
		return "record was self-reported and not independently verified"
	default:
		if r.ExitCode != 0 {
			return "command exited with code " + strconv.Itoa(r.ExitCode)
		}
		return "no additional reason reported"
	}
}

func formatAcceptanceCriteria(specConformance string) string {
	switch strings.TrimSpace(specConformance) {
	case "pass":
		return "satisfied"
	case "fail":
		return "failed"
	case "not-applicable":
		return "not applicable"
	case "":
		return "not reviewed"
	default:
		return specConformance
	}
}

func formatRegressions(status prettyReceiptStatus, findings []PrettyFinding) string {
	if status.value == "pass" || status.value == "succeeded" {
		return "none reported"
	}
	for _, finding := range findings {
		switch strings.ToLower(strings.TrimSpace(finding.Severity)) {
		case "error", "critical", "high", "blocking":
			return "see findings"
		}
	}
	return "none reported"
}

func formatFindingCounts(findings []PrettyFinding) string {
	if len(findings) == 0 {
		return "none"
	}
	order := []string{"critical", "error", "warning", "high", "medium", "low", "info"}
	counts := map[string]int{}
	for _, finding := range findings {
		severity := strings.ToLower(strings.TrimSpace(finding.Severity))
		if severity == "" {
			severity = "unknown"
		}
		counts[severity]++
	}
	var parts []string
	for _, severity := range order {
		if count := counts[severity]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, severity))
			delete(counts, severity)
		}
	}
	var remaining []string
	for severity := range counts {
		remaining = append(remaining, severity)
	}
	sortStrings(remaining)
	for _, severity := range remaining {
		parts = append(parts, fmt.Sprintf("%d %s", counts[severity], severity))
	}
	return strings.Join(parts, ", ")
}

func findingDetailLines(findings []PrettyFinding) []string {
	lines := []string{"- finding details:"}
	for _, finding := range findings {
		location := strings.TrimSpace(finding.File)
		if location == "" {
			location = "general"
		}
		lines = append(lines, fmt.Sprintf("  - %s: %s - %s", prettyValue(finding.Severity), location, firstPrettyLine(finding.Note)))
	}
	return lines
}

func nextLines(r Report, status prettyReceiptStatus, options PrettyOptions) []string {
	if len(options.Next) > 0 {
		return prefixBullets(options.Next)
	}
	var lines []string
	switch status.value {
	case "pass":
		lines = append(lines, "continue with the configured merge or promotion gate")
	case "succeeded":
		if r.Role == RoleWorker {
			lines = append(lines, "run verifier review before calling the PR merge-eligible")
		} else {
			lines = append(lines, "continue with the next configured step")
		}
	case "needs-human":
		lines = append(lines, "human should decide whether the reported uncertainty is acceptable")
	case "fail", "failed", "timed_out", "cancelled":
		lines = append(lines, "inspect the failure and recover or retry before continuing")
	default:
		lines = append(lines, "inspect the report before continuing")
	}
	if strings.TrimSpace(options.DetailCommand) != "" {
		lines = append(lines, "details: "+options.DetailCommand)
	}
	if strings.TrimSpace(options.RawJSONCommand) != "" {
		lines = append(lines, "raw JSON: "+options.RawJSONCommand)
	}
	return prefixBullets(lines)
}

func prefixBullets(values []string) []string {
	lines := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		lines = append(lines, "- "+value)
	}
	if len(lines) == 0 {
		return []string{"- inspect the report before continuing"}
	}
	return lines
}

func firstPrettyLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	line, _, _ := strings.Cut(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	return line
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func formatPrettyInt(value int64) string {
	if value == 0 {
		return "0"
	}
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	digits := strconv.FormatInt(value, 10)
	var groups []string
	for len(digits) > 3 {
		groups = append(groups, digits[len(digits)-3:])
		digits = digits[:len(digits)-3]
	}
	groups = append(groups, digits)
	for i, j := 0, len(groups)-1; i < j; i, j = i+1, j-1 {
		groups[i], groups[j] = groups[j], groups[i]
	}
	return sign + strings.Join(groups, ",")
}
