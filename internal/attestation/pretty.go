package attestation

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PrettyMode selects the human-oriented attestation rendering style.
type PrettyMode int

const (
	// PrettyModeEmoji renders the preferred human form with a status emoji.
	PrettyModeEmoji PrettyMode = iota
	// PrettyModePlain renders the plain ASCII fallback with no emoji or ANSI.
	PrettyModePlain
)

// PrettyOptions configures human-oriented attestation rendering.
type PrettyOptions struct {
	Mode PrettyMode
}

// Pretty renders a human-oriented, multi-line attestation summary.
//
// Pretty output is not a machine parse target. Use CanonicalJSON or Header for
// durable machine or greppable output.
func (r AttestationRecord) Pretty(options PrettyOptions) string {
	status := r.prettyStatus()
	prefix := "   "
	statusLine := status.emojiLine
	if options.Mode == PrettyModePlain {
		prefix = "  "
		statusLine = status.plainLine
	}

	lines := []string{
		statusLine,
		prettyField(prefix, "role", prettyValue(string(r.Role))),
		prettyField(prefix, "provider", prettyValue(prettyProviderVendor(r.Provider))),
		prettyField(prefix, "tool", prettyValue(r.Provider)),
		prettyField(prefix, "model", formatPrettyModel(r.Model, r.ModelSource)),
		prettyField(prefix, "effort", prettyValue(r.Effort)),
		prettyField(prefix, "permission", prettyValue(string(r.Permission))),
		prettyField(prefix, "action", strconv.Quote(r.Action)),
		prettyField(prefix, "exit", strconv.Itoa(r.ExitCode)),
		prettyField(prefix, "started", formatPrettyTimestamp(r.StartedAt)),
		prettyField(prefix, "ended", formatPrettyTimestamp(r.EndedAt)),
		prettyField(prefix, "duration", formatPrettyDuration(r.DurationMS)),
		prettyField(prefix, "tokens", formatPrettyUsage(r.Usage)),
		prettyField(prefix, "verified", strconv.FormatBool(r.Verified)),
	}

	return strings.Join(lines, "\n")
}

type prettyStatus struct {
	emojiLine string
	plainLine string
}

func (r AttestationRecord) prettyStatus() prettyStatus {
	if r.ExitCode != 0 {
		return prettyStatus{
			emojiLine: "❌ attestation failed",
			plainLine: "attestation: failed",
		}
	}
	if r.Verified {
		return prettyStatus{
			emojiLine: "✅ attestation verified",
			plainLine: "attestation: verified",
		}
	}
	return prettyStatus{
		emojiLine: "⚠️ attestation self-reported",
		plainLine: "attestation: self-reported",
	}
}

func prettyField(prefix, label, value string) string {
	return fmt.Sprintf("%s%-10s  %s", prefix, label, value)
}

func prettyValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unset"
	}
	return value
}

func prettyProviderVendor(provider string) string {
	switch provider {
	case "codex":
		return "OpenAI"
	case "claude":
		return "Anthropic"
	case "gemini":
		return "Google"
	default:
		return provider
	}
}

func formatPrettyModel(model string, source ModelSource) string {
	suffix := prettyValue(string(source))
	switch source {
	case ModelSourceParsed:
		suffix = "detected"
	case ModelSourceSelfReported:
		suffix = "self-reported"
	}
	return fmt.Sprintf("%s (%s)", prettyValue(model), suffix)
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
