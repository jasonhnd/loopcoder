package runstatus

import (
	"strconv"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/observability"
)

func runStatusObservability(report Report) observability.Document {
	items := make([]observability.RenderItem, 0, len(report.Rows)+len(report.RunTree.Nodes))
	for _, row := range report.Rows {
		id := row.WorkerJob
		if strings.TrimSpace(id) == "" || id == NotReported {
			id = row.Issue
		}
		items = append(items, observability.RenderItem{
			ID:         id,
			Kind:       "worker-status",
			Status:     row.Status,
			Duration:   row.WorkerDuration,
			Provider:   row.WorkerProvider,
			Model:      row.WorkerModel,
			Effort:     row.WorkerEffort,
			Permission: row.WorkerPermission,
			Usage: observability.UsageSummary{
				InputTokens:  parseTokenPtr(row.WorkerInputTokens),
				OutputTokens: parseTokenPtr(row.WorkerOutputTokens),
				TotalTokens:  parseTokenPtr(row.WorkerTotalTokens),
			},
			Confidence: "exact",
			Freshness:  "durable",
			SourceRefs: []observability.SourceRef{{
				Table:         "worker_attempts",
				RecordID:      row.WorkerJob,
				DeliveryRunID: report.RunID,
				Provenance:    "local-run-status",
			}},
		})
		if row.VerifierVerdict != "" && row.VerifierVerdict != NotReported {
			items = append(items, observability.RenderItem{
				ID:         row.WorkerJob + "/verifier",
				Kind:       "verifier-status",
				Status:     row.VerifierVerdict,
				Duration:   row.VerifierDuration,
				Provider:   row.VerifierProvider,
				Model:      row.VerifierModel,
				Effort:     row.VerifierEffort,
				Permission: row.VerifierPermission,
				Usage: observability.UsageSummary{
					InputTokens:  parseTokenPtr(row.VerifierInputTokens),
					OutputTokens: parseTokenPtr(row.VerifierOutputTokens),
					TotalTokens:  parseTokenPtr(row.VerifierTotalTokens),
				},
				Confidence: "exact",
				Freshness:  "durable",
				SourceRefs: []observability.SourceRef{{
					Table:         "verifier_reports",
					RecordID:      row.verifierRecordSortKey,
					DeliveryRunID: report.RunID,
					Provenance:    "local-run-status",
				}},
			})
		}
	}
	for _, node := range report.RunTree.Nodes {
		items = append(items, observability.RenderItem{
			ID:         node.RunID,
			Kind:       "run-tree-node",
			Status:     node.LifecycleStatus,
			Provider:   node.Provider,
			Model:      node.Model,
			Effort:     node.Effort,
			Permission: node.Permission,
			Reason:     node.LastError,
			NextAction: node.ClaimOutcome,
			Confidence: "exact",
			Freshness:  "durable",
			SourceRefs: []observability.SourceRef{{
				Table:         "run_tree",
				RecordID:      node.RunID,
				ProjectID:     node.ProjectID,
				DeliveryRunID: node.RunID,
				Provenance:    "local-run-tree",
			}},
		})
	}
	return observability.NewDocument("status", observability.Correlation{
		ProjectID:     report.Project.ProjectID,
		DeliveryRunID: report.RunID,
		Source:        "status",
	}, items, nil)
}

func parseTokenPtr(value string) *int64 {
	value = strings.TrimSpace(value)
	if value == "" || value == NotReported {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil
	}
	return &parsed
}
