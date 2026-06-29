package attestation

import (
	"fmt"
	"strconv"
	"strings"
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
		prettyField(prefix, "provider", prettyValue(r.Provider)),
		prettyField(prefix, "model", fmt.Sprintf("%s (source=%s)", prettyValue(r.Model), prettyValue(string(r.ModelSource)))),
		prettyField(prefix, "effort", prettyValue(r.Effort)),
		prettyField(prefix, "permission", prettyValue(string(r.Permission))),
		prettyField(prefix, "action", strconv.Quote(r.Action)),
		prettyField(prefix, "exit", strconv.Itoa(r.ExitCode)),
		prettyField(prefix, "duration", fmt.Sprintf("%s (%d ms)", formatDuration(r.DurationMS), r.DurationMS)),
		prettyField(prefix, "started", prettyValue(r.StartedAt)),
		prettyField(prefix, "ended", prettyValue(r.EndedAt)),
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

func formatPrettyUsage(usage Usage) string {
	var parts []string
	hasSplit := usage.InputTokens != nil || usage.OutputTokens != nil
	if usage.InputTokens != nil {
		parts = append(parts, fmt.Sprintf("input=%d", *usage.InputTokens))
	}
	if usage.OutputTokens != nil {
		parts = append(parts, fmt.Sprintf("output=%d", *usage.OutputTokens))
	}
	if usage.TotalTokens != nil {
		parts = append(parts, fmt.Sprintf("total=%d", *usage.TotalTokens))
	} else if hasSplit {
		parts = append(parts, "total=unset")
	}
	if len(parts) == 0 {
		return "unset"
	}
	return strings.Join(parts, " ")
}
