package reportquery

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/jasonhnd/loopcoder/internal/observability"
)

func ObservabilityDocument(records []Record) observability.Document {
	items := make([]observability.RenderItem, 0, len(records))
	for _, record := range records {
		r := record.Report
		status := "succeeded"
		if r.ExitCode != 0 {
			status = "failed"
		}
		items = append(items, observability.ItemFromReport(reportItemID(record), string(r.Role), status, "", "", r, []observability.SourceRef{{
			Table:         "reports",
			RecordID:      reportItemID(record),
			DeliveryRunID: record.RunID,
			Field:         record.Source,
			Provenance:    "report-query",
		}}))
	}
	return observability.NewDocument("report", observability.Correlation{Source: "report"}, items, nil)
}

func reportItemID(record Record) string {
	if record.Report.WorkID != "" {
		return record.Report.WorkID
	}
	if record.RunID != "" {
		return record.RunID
	}
	if record.Report.Issue > 0 {
		return "issue-" + strconv.Itoa(record.Report.Issue)
	}
	if record.Source != "" {
		return "report-" + observability.StableRecordID(record.Source)
	}
	if record.Path != "" {
		sum := sha256.Sum256([]byte(record.Path))
		return "report-path-" + hex.EncodeToString(sum[:])[:16]
	}
	return "report-unknown"
}
