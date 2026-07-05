package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func Render(w io.Writer, result Result, format string) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		return RenderText(w, result)
	case "json":
		return RenderJSON(w, result)
	default:
		return fmt.Errorf("unsupported audit output format %q", format)
	}
}

func RenderJSON(w io.Writer, result Result) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w)
	return err
}

func RenderText(w io.Writer, result Result) error {
	if _, err := fmt.Fprintln(w, "AUDIT SUMMARY"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "verdict: %s\n", result.Verdict); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "threshold: %s\n", result.Threshold); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "layers: %s\n", strings.Join(result.Layers, ",")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "findings: %d\n", len(result.Findings)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "needs_human: %d\n", len(result.NeedsHuman)); err != nil {
		return err
	}
	if len(result.RuntimeFailures) > 0 {
		if _, err := fmt.Fprintln(w, "runtime_failures:"); err != nil {
			return err
		}
		for _, failure := range result.RuntimeFailures {
			if _, err := fmt.Fprintf(w, "- %s\n", failure); err != nil {
				return err
			}
		}
	}
	if len(result.ToolResults) > 0 {
		if _, err := fmt.Fprintln(w, "tool_results:"); err != nil {
			return err
		}
		for _, tool := range result.ToolResults {
			status := tool.ParseStatus
			if status == "" {
				status = "unknown"
			}
			line := fmt.Sprintf("- %s: %s exit=%d findings=%d", tool.ID, status, tool.ExitStatus, tool.FindingCount)
			if tool.Error != "" {
				line += " error=" + tool.Error
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}
	if len(result.Findings) > 0 {
		if _, err := fmt.Fprintln(w, "findings:"); err != nil {
			return err
		}
		for _, finding := range result.Findings {
			location := finding.File
			if location == "" {
				location = "(global)"
			}
			if finding.Line > 0 {
				location = fmt.Sprintf("%s:%d", location, finding.Line)
			}
			if _, err := fmt.Fprintf(w, "- %s %s %s %s: %s\n", finding.Severity, location, finding.Tool, finding.Rule, finding.Message); err != nil {
				return err
			}
			if strings.TrimSpace(finding.Evidence) != "" {
				if _, err := fmt.Fprintf(w, "  evidence: %s\n", finding.Evidence); err != nil {
					return err
				}
			}
		}
	}
	if len(result.NeedsHuman) > 0 {
		if _, err := fmt.Fprintln(w, "needs_human:"); err != nil {
			return err
		}
		for _, item := range result.NeedsHuman {
			if _, err := fmt.Fprintf(w, "- %s %s: %s\n", item.Layer, item.Reason, item.Message); err != nil {
				return err
			}
		}
	}
	return nil
}
