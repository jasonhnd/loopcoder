package reporter

import (
	"fmt"
	"sort"
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
	Mode    PrettyMode
	Verbose bool
}

// Pretty renders a human-oriented, multi-line report summary.
//
// Pretty output is not a machine parse target. Use CanonicalJSON or Header for
// durable machine or greppable output.
func (r Report) Pretty(options PrettyOptions) string {
	receipt := r.prettyReceipt()
	statusLine := receipt.statusLine
	if options.Mode == PrettyModePlain {
		statusLine = strings.TrimLeft(statusLine, "\u2705\u274c\u26a0\ufe0f ")
		statusLine = strings.Replace(statusLine, "report verified", "report: verified", 1)
		statusLine = strings.Replace(statusLine, "report failed", "report: failed", 1)
		statusLine = strings.Replace(statusLine, "report self-reported", "report: self-reported", 1)
	}

	lines := []string{statusLine}
	lines = append(lines, "Target")
	lines = appendReceiptSection(lines, receipt.target)
	lines = append(lines, "Verdict")
	lines = appendReceiptSection(lines, receipt.verdict)
	if len(receipt.review) > 0 {
		lines = append(lines, "Review summary")
		lines = appendReceiptSection(lines, receipt.review)
		if options.Verbose {
			lines = appendVerboseFindings(lines, r.Findings)
		}
	}
	lines = append(lines, "Run")
	lines = appendReceiptSection(lines, receipt.run)
	lines = append(lines, "Next")
	lines = appendReceiptSection(lines, receipt.next)

	return strings.Join(lines, "\n")
}

type prettyReceipt struct {
	statusLine string
	target     []receiptField
	verdict    []receiptField
	review     []receiptField
	run        []receiptField
	next       []receiptField
}

type receiptField struct {
	Label string
	Value string
}

func (r Report) prettyReceipt() prettyReceipt {
	status := r.prettyStatus()
	receipt := prettyReceipt{
		statusLine: statusLine(status, string(r.Role), r.Verified, r.ExitCode),
		target: []receiptField{
			{Label: "work id", Value: displayPretty(r.WorkID)},
		},
		verdict: []receiptField{
			{Label: "status", Value: status},
			{Label: "blocking defects", Value: strconv.Itoa(r.blockingDefects(status))},
			{Label: "reason", Value: r.prettyReason(status)},
		},
		run: []receiptField{
			{Label: string(r.Role), Value: prettyRunAgent(r)},
			{Label: "permission", Value: displayPretty(string(r.Permission))},
			{Label: "action", Value: strconv.Quote(r.Action)},
			{Label: "exit", Value: strconv.Itoa(r.ExitCode)},
			{Label: "duration", Value: formatPrettyDuration(r.DurationMS)},
			{Label: "started", Value: formatPrettyTimestamp(r.StartedAt)},
			{Label: "ended", Value: formatPrettyTimestamp(r.EndedAt)},
			{Label: "verified", Value: strconv.FormatBool(r.Verified)},
			{Label: "tokens", Value: formatPrettyUsage(r.Usage)},
		},
		next: r.prettyNext(status),
	}

	if r.Issue > 0 {
		receipt.target = append(receipt.target, receiptField{Label: "issue", Value: "#" + strconv.Itoa(r.Issue)})
	}
	if r.PR > 0 {
		receipt.target = append(receipt.target, receiptField{Label: "PR", Value: "#" + strconv.Itoa(r.PR)})
	} else if pr := prFromAction(r.Action); pr > 0 {
		receipt.target = append(receipt.target, receiptField{Label: "PR", Value: "#" + strconv.Itoa(pr)})
	}
	if strings.TrimSpace(r.Branch) != "" {
		receipt.target = append(receipt.target, receiptField{Label: "branch", Value: r.Branch})
	}
	if strings.TrimSpace(r.Worktree) != "" {
		receipt.target = append(receipt.target, receiptField{Label: "worktree", Value: r.Worktree})
	}
	if r.Round > 0 {
		receipt.target = append(receipt.target, receiptField{Label: "round", Value: strconv.Itoa(r.Round)})
	}
	if r.Role == RoleVerifier || len(r.Findings) > 0 || strings.TrimSpace(r.SpecStatus) != "" {
		receipt.review = []receiptField{
			{Label: "acceptance criteria", Value: prettySpecStatus(r.SpecStatus)},
			{Label: "findings", Value: formatFindingsSummary(r.Findings)},
		}
	}
	return receipt
}

func (r Report) prettyStatus() string {
	status := strings.TrimSpace(r.Status)
	if status != "" {
		return status
	}
	if r.Role == RoleVerifier {
		if r.ExitCode == 0 {
			return "pass"
		}
		return "fail"
	}
	if strings.Contains(strings.ToLower(r.Action), "harvest hung worker") {
		return "needs-human"
	}
	if r.ExitCode != 0 {
		return "fail"
	}
	return "success"
}

func statusLine(status, role string, verified bool, exitCode int) string {
	trust := "report verified"
	icon := "\u2705"
	if exitCode != 0 || status == "fail" || status == "failed" || status == "timeout" || status == "cancelled" || status == "partial-child-failure" {
		trust = "report failed"
		icon = "\u274c"
	} else if !verified {
		trust = "report self-reported"
		icon = "\u26a0\ufe0f"
	}
	return fmt.Sprintf("%s %s - loopcoder report: %s %s", icon, trust, displayPretty(role), displayPretty(status))
}

func (r Report) prettyReason(status string) string {
	if strings.TrimSpace(r.Reason) != "" {
		return strings.TrimSpace(r.Reason)
	}
	switch status {
	case "success":
		if r.Role == RoleWorker {
			return "worker completed successfully"
		}
		return "command completed successfully"
	case "pass":
		return "verifier passed"
	case "fail", "failed":
		if r.ExitCode != 0 {
			return fmt.Sprintf("command exited with code %d", r.ExitCode)
		}
		return "blocking defects reported"
	case "needs-human":
		return firstFindingNote(r.Findings, "human judgment required")
	case "timeout":
		return "run timed out"
	case "cancelled":
		return "run was cancelled"
	case "partial-child-failure":
		return "one or more child runs failed"
	default:
		return displayPretty("")
	}
}

func appendReceiptSection(lines []string, fields []receiptField) []string {
	for _, field := range fields {
		lines = append(lines, fmt.Sprintf("- %s: %s", field.Label, displayPretty(field.Value)))
	}
	return lines
}

func appendVerboseFindings(lines []string, findings []Finding) []string {
	if len(findings) == 0 {
		return lines
	}
	lines = append(lines, "Findings")
	for _, finding := range findings {
		file := strings.TrimSpace(finding.File)
		location := ""
		if file != "" {
			location = " " + file + ":"
		}
		lines = append(lines, fmt.Sprintf("- %s:%s %s", displayPretty(finding.Severity), location, displayPretty(finding.Note)))
	}
	return lines
}

func prettyRunAgent(r Report) string {
	return strings.Join([]string{
		displayPretty(r.Provider),
		displayPretty(reporterModelDepth(r.Model, r.Effort)),
		displayPretty(r.Effort),
	}, " / ")
}

func reporterModelDepth(model, effort string) string {
	return ModelDepthDisplay(model, effort)
}

func prettySpecStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "pass":
		return "satisfied"
	case "fail":
		return "not satisfied"
	case "not-applicable":
		return "not applicable"
	case "":
		return "not reported"
	default:
		return status
	}
}

func formatFindingsSummary(findings []Finding) string {
	if len(findings) == 0 {
		return "none"
	}
	counts := map[string]int{}
	for _, finding := range findings {
		severity := strings.ToLower(strings.TrimSpace(finding.Severity))
		if severity == "" {
			severity = "unspecified"
		}
		counts[severity]++
	}
	order := []string{"critical", "blocker", "error", "warning", "medium", "low", "info", "unspecified"}
	var parts []string
	seen := map[string]bool{}
	for _, severity := range order {
		if count := counts[severity]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, severity))
			seen[severity] = true
		}
	}
	var remaining []string
	for severity := range counts {
		if !seen[severity] {
			remaining = append(remaining, severity)
		}
	}
	sort.Strings(remaining)
	for _, severity := range remaining {
		parts = append(parts, fmt.Sprintf("%d %s", counts[severity], severity))
	}
	return strings.Join(parts, ", ")
}

func (r Report) blockingDefects(status string) int {
	count := 0
	for _, finding := range r.Findings {
		switch strings.ToLower(strings.TrimSpace(finding.Severity)) {
		case "critical", "blocker", "error":
			count++
		}
	}
	if count == 0 && (status == "fail" || status == "failed") && r.ExitCode != 0 {
		return 1
	}
	return count
}

func firstFindingNote(findings []Finding, fallback string) string {
	for _, finding := range findings {
		if strings.TrimSpace(finding.Note) != "" {
			return strings.TrimSpace(finding.Note)
		}
	}
	return fallback
}

func (r Report) prettyNext(status string) []receiptField {
	workID := strings.TrimSpace(r.WorkID)
	if workID == "" {
		workID = "<work-id>"
	}
	action := "continue with the next loopcoder gate"
	switch status {
	case "success":
		if r.Role == RoleWorker {
			action = "run verifier review before merge consideration"
		}
	case "pass":
		action = "human may decide whether to merge through the configured gate"
	case "fail", "failed":
		action = "fix the blocking defects, then rerun verification"
	case "needs-human":
		action = "human should decide whether the reported reason is acceptable"
	case "timeout":
		action = "inspect timeout evidence, then retry or escalate"
	case "cancelled":
		action = "confirm cancellation was intentional before retrying"
	case "partial-child-failure":
		action = "inspect failed child runs before continuing"
	}
	return []receiptField{
		{Label: "action", Value: action},
		{Label: "details", Value: "loopcoder report --work-id " + workID + " --verbose"},
		{Label: "raw JSON", Value: "loopcoder report --work-id " + workID + " --format json"},
	}
}

func prFromAction(action string) int {
	fields := strings.Fields(strings.ReplaceAll(action, "#", " #"))
	for i, field := range fields {
		if strings.EqualFold(field, "PR") && i+1 < len(fields) {
			if number := parseHashNumber(fields[i+1]); number > 0 {
				return number
			}
		}
	}
	return 0
}

func parseHashNumber(value string) int {
	value = strings.Trim(strings.TrimSpace(value), ".,;:)]}")
	value = strings.TrimPrefix(value, "#")
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0
	}
	return number
}

func prettyValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unset"
	}
	return value
}

func displayPretty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "not reported"
	}
	return strings.TrimSpace(value)
}

func formatPrettyTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return prettyValue(value)
	}
	return parsed.In(time.Local).Format("2006-01-02 15:04:05 MST")
}

func formatPrettyDuration(durationMS int64) string {
	return fmt.Sprintf("%s (%.1f s)", formatCompactPrettyDuration(durationMS), float64(durationMS)/1000)
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
