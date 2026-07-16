package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/reporter"
)

type commandOutputMode struct {
	Format  string
	Verbose bool
}

func normalizeCommandOutputMode(command, format string, verbose bool, stderr io.Writer) (commandOutputMode, bool) {
	return normalizeCommandOutputModeWithFormats(command, format, verbose, stderr, "text", "json")
}

func normalizeCommandOutputModeWithFormats(command, format string, verbose bool, stderr io.Writer, allowed ...string) (commandOutputMode, bool) {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "text"
	}
	for _, candidate := range allowed {
		if format == candidate {
			return commandOutputMode{Format: format, Verbose: verbose}, true
		}
	}
	fmt.Fprintf(stderr, "%s: invalid --format %q; want %s\n", command, format, strings.Join(allowed, ", "))
	return commandOutputMode{}, false
}

func writeJSONLine(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func commandWarningsWriter(mode commandOutputMode, stderr io.Writer) io.Writer {
	if mode.Format == "json" {
		return io.Discard
	}
	return stderr
}

func isTerminalWriter(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func shouldRenderPretty(suppress bool) bool {
	return !(suppress || envFlag("LOOPCODER_NO_PRETTY"))
}

func prettyModeForTarget(w io.Writer, deps Deps, forceEmoji bool) reporter.PrettyMode {
	if plainPrettyForced() {
		return reporter.PrettyModePlain
	}
	if forceEmoji || envFlag("LOOPCODER_PRETTY") {
		return reporter.PrettyModeEmoji
	}
	if prettyTargetInteractive(w, deps) {
		return reporter.PrettyModeEmoji
	}
	return reporter.PrettyModePlain
}

func renderPrettyReport(w io.Writer, record reporter.Report, mode reporter.PrettyMode) error {
	return renderPrettyReportWithOptions(w, record, reporter.PrettyOptions{Mode: mode})
}

func renderPrettyReportWithOptions(w io.Writer, record reporter.Report, options reporter.PrettyOptions) error {
	if strings.TrimSpace(options.DetailCommand) == "" {
		options.DetailCommand = detailReportCommand(record)
	}
	if strings.TrimSpace(options.RawJSONCommand) == "" {
		options.RawJSONCommand = rawJSONReportCommand(record)
	}
	_, err := fmt.Fprintln(w, record.Pretty(options))
	return err
}

func prettyReport(record reporter.Report, options reporter.PrettyOptions) string {
	if strings.TrimSpace(options.DetailCommand) == "" {
		options.DetailCommand = detailReportCommand(record)
	}
	if strings.TrimSpace(options.RawJSONCommand) == "" {
		options.RawJSONCommand = rawJSONReportCommand(record)
	}
	return record.Pretty(options)
}

func renderLoopreviewPrettyReport(w io.Writer, verdict loopreview.Verdict, mode reporter.PrettyMode) error {
	if verdict.Report == nil {
		return nil
	}
	_, err := fmt.Fprintln(w, loopreviewPrettyBlock(verdict, mode))
	return err
}

func loopreviewPrettyBlock(verdict loopreview.Verdict, mode reporter.PrettyMode) string {
	verdict = loopreview.NormalizeVerdict(verdict)
	record := *verdict.Report
	blocking := blockingFindingCount(verdict.Findings)
	next := []string{}
	if verdict.Verdict == loopreview.VerdictNeedsHuman || verdict.Verdict == loopreview.VerdictFail {
		next = append(next, verdict.NextAction)
	}
	return prettyReport(record, reporter.PrettyOptions{
		Mode:            mode,
		Status:          verdict.Verdict,
		PR:              prFromReport(record),
		BlockingDefects: &blocking,
		Reason:          verdict.Reason,
		SpecConformance: verdict.SpecConformance,
		Findings:        prettyFindings(verdict.Findings),
		Next:            next,
	})
}

func dispatchPrettyBlock(record reporter.Report, status, pr, reason string, mode reporter.PrettyMode) string {
	return prettyReport(record, reporter.PrettyOptions{
		Mode:   mode,
		Status: status,
		PR:     firstNonEmptyString(pr, prFromReport(record)),
		Reason: reason,
	})
}

func prettyFindings(findings []loopreview.Finding) []reporter.PrettyFinding {
	out := make([]reporter.PrettyFinding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, reporter.PrettyFinding{
			Severity: finding.Severity,
			File:     finding.File,
			Note:     finding.Note,
		})
	}
	return out
}

func blockingFindingCount(findings []loopreview.Finding) int {
	count := 0
	for _, finding := range findings {
		switch strings.ToLower(strings.TrimSpace(finding.Severity)) {
		case "critical", "error", "high", "blocking":
			count++
		}
	}
	return count
}

func prFromReport(record reporter.Report) string {
	action := strings.TrimSpace(record.Action)
	index := strings.LastIndex(action, "#")
	if index < 0 {
		return ""
	}
	numberText := strings.TrimSpace(action[index+1:])
	var digits strings.Builder
	for _, r := range numberText {
		if r < '0' || r > '9' {
			break
		}
		digits.WriteRune(r)
	}
	if digits.Len() == 0 {
		return ""
	}
	return "#" + digits.String()
}

func detailReportCommand(record reporter.Report) string {
	if strings.TrimSpace(record.WorkID) == "" {
		return "loopcoder report --verbose"
	}
	return "loopcoder report --work-id " + record.WorkID + " --verbose"
}

func rawJSONReportCommand(record reporter.Report) string {
	if strings.TrimSpace(record.WorkID) == "" {
		return "loopcoder report --format json"
	}
	return "loopcoder report --work-id " + record.WorkID + " --format json"
}

func firstReceiptLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	line, _, _ := strings.Cut(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	return line
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func formatPRNumber(number int) string {
	if number <= 0 {
		return ""
	}
	return "#" + strconv.Itoa(number)
}

func prettyTargetInteractive(w io.Writer, deps Deps) bool {
	if deps.IsTerminal == nil {
		deps.IsTerminal = DefaultDeps().IsTerminal
	}
	return deps.IsTerminal(w)
}

func plainPrettyForced() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return true
	}
	return envSet("LOOPCODER_NO_EMOJI") || envSet("LOOPCODER_PLAIN")
}

func envFlag(name string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envSet(name string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	switch value {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
