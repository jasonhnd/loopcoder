package cli

import (
	"io"
	"strconv"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/detachedrun"
	"github.com/jasonhnd/loopcoder/internal/observability"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func renderCanonicalJSONLine(w io.Writer, value any) error {
	data, err := delivery.CanonicalJSON(value)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func detachedRecoverJSONPayload(result detachedrun.StatusResult) any {
	return struct {
		SchemaVersion string                 `json:"schema_version"`
		Observability observability.Document `json:"observability"`
		detachedrun.StatusResult
	}{
		SchemaVersion: "loopcoder.recover_detached_result.v1",
		Observability: observability.NewDocument("recover", observability.Correlation{
			DeliveryRunID: result.Record.RunID,
			Source:        "recover",
		}, []observability.RenderItem{{
			ID:         result.Record.RunID,
			Kind:       "detached-recovery",
			Status:     result.Record.Status,
			Reason:     result.Reason,
			NextAction: result.ReplayAction,
			Confidence: "exact",
			Freshness:  "durable",
			SourceRefs: []observability.SourceRef{{
				Table:         "detached_runs",
				RecordID:      result.Record.RunID,
				DeliveryRunID: result.Record.RunID,
				Provenance:    "detached-recover",
			}},
		}}, nil),
		StatusResult: result,
	}
}

func dispatchJSONPayload(result worker.Result) any {
	return struct {
		SchemaVersion string                 `json:"schema_version"`
		Observability observability.Document `json:"observability"`
		worker.Result
	}{
		SchemaVersion: "loopcoder.dispatch_result.v1",
		Observability: observability.NewDocument("dispatch", observability.Correlation{
			DeliveryRunID: result.RunID,
			Source:        "dispatch",
		}, dispatchObservabilityItems(result), nil),
		Result: result,
	}
}

func dispatchObservabilityItems(result worker.Result) []observability.RenderItem {
	if result.Report == nil {
		return []observability.RenderItem{}
	}
	return []observability.RenderItem{
		observability.ItemFromReport("issue-"+strconv.Itoa(result.Issue), "worker", result.Status, result.Reason, result.NextAction, *result.Report, []observability.SourceRef{{
			Table:         "worker_attempts",
			RecordID:      result.AttemptPath,
			DeliveryRunID: result.RunID,
			Provenance:    "durable-local-attempt",
		}}),
	}
}

func nestedJSONPayload(report orchestration.NestedScheduleReport) any {
	items := make([]observability.RenderItem, 0, len(report.Children))
	for _, child := range report.Children {
		if child.Report != nil {
			items = append(items, observability.ItemFromReport(child.RunID, "nested-child", child.Status, child.Reason, child.NextAction, *child.Report, []observability.SourceRef{{
				Table:         "run_edges",
				RecordID:      child.ID,
				DeliveryRunID: child.RunID,
				Provenance:    "durable-nested-run",
			}}))
			continue
		}
		items = append(items, observability.RenderItem{
			ID:         child.RunID,
			Kind:       "nested-child",
			Status:     child.Status,
			Reason:     child.Reason,
			NextAction: child.NextAction,
			Confidence: "exact",
			Freshness:  "durable",
			SourceRefs: []observability.SourceRef{{
				Table:         "run_edges",
				RecordID:      child.ID,
				DeliveryRunID: child.RunID,
				Provenance:    "durable-nested-run",
			}},
		})
	}
	return struct {
		SchemaVersion string                 `json:"schema_version"`
		Observability observability.Document `json:"observability"`
		orchestration.NestedScheduleReport
	}{
		SchemaVersion: "loopcoder.nested_run_result.v1",
		Observability: observability.NewDocument("nested", observability.Correlation{
			DeliveryRunID: report.ParentRunID,
			Source:        "nested",
		}, items, nil),
		NestedScheduleReport: report,
	}
}
