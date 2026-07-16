package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/observability"
)

func RenderJSON(w io.Writer, result Result) error {
	result = Finalize(result)
	payload := struct {
		Observability observability.Document `json:"observability"`
		Result
	}{
		Observability: ObservabilityDocument(result),
		Result:        result,
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}

func ObservabilityDocument(result Result) observability.Document {
	items := make([]observability.RenderItem, 0, 1)
	if result.Report != nil {
		receipt := DecisionReceipt(result)
		items = append(items, observability.ItemFromReport("audit", "audit", result.Verdict, receipt.Reason, receipt.NextAction, *result.Report, []observability.SourceRef{{
			Table:      "audit",
			RecordID:   "audit-result",
			Provenance: "audit-result",
		}}))
	} else {
		receipt := DecisionReceipt(result)
		items = append(items, observability.RenderItem{
			ID:         "audit",
			Kind:       "audit",
			Status:     result.Verdict,
			Reason:     receipt.Reason,
			NextAction: receipt.NextAction,
			Confidence: "exact",
			Freshness:  "durable",
			SourceRefs: []observability.SourceRef{{Table: "audit", RecordID: "audit-result", Provenance: "audit-result"}},
		})
	}
	return observability.NewDocument("audit", observability.Correlation{Source: "audit"}, items, nil)
}

func RenderText(w io.Writer, result Result) error {
	result = Finalize(result)
	if _, err := fmt.Fprintln(w, "AUDIT RESULT"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "repo: %s\n", result.Repo); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "layers: %s\n", strings.Join(result.Layers, ",")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "threshold: %s\n", result.Threshold); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "verdict: %s\n", result.Verdict); err != nil {
		return err
	}
	receipt := DecisionReceipt(result)
	if _, err := fmt.Fprintf(w, "reason: %s\n", receipt.Reason); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "next_action: %s\n", receipt.NextAction); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "findings: %d\n", len(result.Findings)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "gate_findings: %d\n", result.Summary.GateFindings); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "warnings: %d\n", result.Summary.Warnings); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "waived_findings: %d\n", result.Summary.WaivedFindings); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "needs_human_count: %d\n", result.Summary.NeedsHuman); err != nil {
		return err
	}
	for _, finding := range result.Findings {
		location := finding.File
		if location == "" {
			location = "(no file)"
		}
		if finding.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, finding.Line)
		}
		waiver := ""
		if finding.Waived {
			waiver = fmt.Sprintf(" waived=true waiver_id=%s", finding.WaiverID)
		}
		metadata := findingMetadataText(finding)
		if _, err := fmt.Fprintf(w, "- %s %s %s %s %s%s%s\n", finding.Severity, finding.Tool, finding.Rule, location, finding.Message, metadata, waiver); err != nil {
			return err
		}
		if strings.TrimSpace(finding.Evidence) != "" {
			if _, err := fmt.Fprintf(w, "  evidence: %s\n", finding.Evidence); err != nil {
				return err
			}
		}
	}
	if len(result.NeedsHuman) > 0 {
		if _, err := fmt.Fprintln(w, "needs_human:"); err != nil {
			return err
		}
		for _, item := range result.NeedsHuman {
			if _, err := fmt.Fprintf(w, "- %s: %s\n", item.Layer, item.Reason); err != nil {
				return err
			}
		}
	}
	if len(result.BaselineNotices) > 0 {
		if _, err := fmt.Fprintln(w, "baseline_notices:"); err != nil {
			return err
		}
		for _, item := range result.BaselineNotices {
			if _, err := fmt.Fprintf(w, "- %s %s: %s\n", item.Status, item.ID, item.Reason); err != nil {
				return err
			}
		}
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
			if _, err := fmt.Fprintf(w, "- %s exit=%d parser=%s status=%s truncated=%t\n", tool.ID, tool.ExitCode, tool.Parser, status, tool.OutputTruncated); err != nil {
				return err
			}
			if tool.Error != "" {
				if _, err := fmt.Fprintf(w, "  error: %s\n", tool.Error); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func findingMetadataText(finding Finding) string {
	parts := []string{}
	if strings.TrimSpace(finding.Tier) != "" {
		parts = append(parts, "tier="+finding.Tier)
	}
	if strings.TrimSpace(finding.Gate) != "" {
		parts = append(parts, "gate="+finding.Gate)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " ") + ")"
}
