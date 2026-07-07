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
	Mode PrettyMode
}

// Pretty renders a human-oriented, multi-line report summary.
//
// Pretty output is not a machine parse target. Use CanonicalJSON or Header for
// durable machine or greppable output.
func (r Report) Pretty(options PrettyOptions) string {
	status := r.prettyStatus()
	prefix := "  "
	statusLine := status.emojiLine
	if options.Mode == PrettyModePlain {
		statusLine = status.plainLine
	}

	lines := []string{
		statusLine,
		"who",
		prettyField(prefix, "role", prettyValue(string(r.Role))),
		prettyField(prefix, "provider", prettyProviderDisplay(r.Provider)),
		prettyField(prefix, "model", formatPrettyModel(r.Model, r.Effort, r.ModelSource)),
		prettyField(prefix, "permission", prettyValue(string(r.Permission))),
		"what",
	}
	lines = appendOptionalPrettyContext(lines, prefix, r)
	lines = append(lines,
		prettyField(prefix, "action", strconv.Quote(r.Action)),
		"result",
		prettyField(prefix, "exit", strconv.Itoa(r.ExitCode)),
		prettyField(prefix, "duration", formatPrettyDuration(r.DurationMS)),
		prettyField(prefix, "started", formatPrettyTimestamp(r.StartedAt)),
		prettyField(prefix, "ended", formatPrettyTimestamp(r.EndedAt)),
		prettyField(prefix, "verified", strconv.FormatBool(r.Verified)),
		"cost",
		prettyField(prefix, "tokens", formatPrettyUsage(r.Usage)),
	)

	return strings.Join(lines, "\n")
}

type prettyStatus struct {
	emojiLine string
	plainLine string
}

func (r Report) prettyStatus() prettyStatus {
	if r.ExitCode != 0 {
		return prettyStatus{
			emojiLine: "\u274c report failed",
			plainLine: "report: failed",
		}
	}
	if r.Verified {
		return prettyStatus{
			emojiLine: "\u2705 report verified",
			plainLine: "report: verified",
		}
	}
	return prettyStatus{
		emojiLine: "\u26a0\ufe0f report self-reported",
		plainLine: "report: self-reported",
	}
}

func prettyField(prefix, label, value string) string {
	return fmt.Sprintf("%s%-10s  %s", prefix, label, value)
}

func appendOptionalPrettyContext(lines []string, prefix string, r Report) []string {
	if strings.TrimSpace(r.WorkID) != "" {
		lines = append(lines, prettyField(prefix, "work_id", r.WorkID))
	}
	if r.Issue > 0 {
		lines = append(lines, prettyField(prefix, "issue", "#"+strconv.Itoa(r.Issue)))
	}
	if strings.TrimSpace(r.Branch) != "" {
		lines = append(lines, prettyField(prefix, "branch", r.Branch))
	}
	if strings.TrimSpace(r.Worktree) != "" {
		lines = append(lines, prettyField(prefix, "worktree", r.Worktree))
	}
	if r.Round > 0 {
		lines = append(lines, prettyField(prefix, "round", strconv.Itoa(r.Round)))
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
