package observability

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/sanitize"
)

const RenderSchemaVersion = "loopcoder.observability_render.v1"

type Capabilities struct {
	Width      int
	Color      bool
	Unicode    bool
	Redirected bool
}

type Document struct {
	SchemaVersion string             `json:"schema_version"`
	Command       string             `json:"command"`
	Mode          string             `json:"mode"`
	Correlation   Correlation        `json:"correlation"`
	Items         []RenderItem       `json:"items"`
	Evidence      []Evidence         `json:"evidence"`
	Redaction     RedactionSummary   `json:"redaction"`
	Truncation    TruncationMetadata `json:"truncation"`
}

type Correlation struct {
	ProjectID     string `json:"project_id,omitempty"`
	DeliveryRunID string `json:"delivery_run_id,omitempty"`
	InvocationID  string `json:"invocation_id,omitempty"`
	Source        string `json:"source,omitempty"`
}

type RenderItem struct {
	ID         string             `json:"id"`
	Kind       string             `json:"kind"`
	Status     string             `json:"status"`
	Duration   string             `json:"duration"`
	Provider   string             `json:"provider"`
	Model      string             `json:"model"`
	Effort     string             `json:"effort,omitempty"`
	Permission string             `json:"permission,omitempty"`
	Usage      UsageSummary       `json:"usage"`
	Reason     string             `json:"reason,omitempty"`
	NextAction string             `json:"next_action,omitempty"`
	Confidence string             `json:"confidence"`
	Freshness  string             `json:"freshness"`
	SourceRefs []SourceRef        `json:"source_refs"`
	Redaction  RedactionSummary   `json:"redaction"`
	Truncation TruncationMetadata `json:"truncation"`
}

type UsageSummary struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	TotalTokens  *int64 `json:"total_tokens"`
}

type TruncationMetadata struct {
	Applied      bool     `json:"applied"`
	Fields       []string `json:"fields"`
	MaxRunes     int      `json:"max_runes,omitempty"`
	OmittedRunes int      `json:"omitted_runes,omitempty"`
}

func NewDocument(command string, correlation Correlation, items []RenderItem, evidence []Evidence) Document {
	doc := Document{
		SchemaVersion: RenderSchemaVersion,
		Command:       strings.TrimSpace(command),
		Mode:          "canonical",
		Correlation:   correlation,
		Items:         normalizeRenderItems(items),
		Evidence:      normalizeEvidence(evidence),
		Redaction:     projectionRedaction(),
		Truncation:    TruncationMetadata{Fields: []string{}},
	}
	return doc
}

func ItemFromReport(id, kind, status, reason, nextAction string, record reporter.Report, refs []SourceRef) RenderItem {
	return RenderItem{
		ID:         firstRenderNonEmpty(strings.TrimSpace(id), strings.TrimSpace(record.WorkID), strconv.Itoa(record.Issue)),
		Kind:       firstRenderNonEmpty(strings.TrimSpace(kind), string(record.Role)),
		Status:     strings.TrimSpace(status),
		Duration:   formatDuration(record.DurationMS),
		Provider:   strings.TrimSpace(record.Provider),
		Model:      strings.TrimSpace(record.Model),
		Effort:     strings.TrimSpace(record.Effort),
		Permission: strings.TrimSpace(string(record.Permission)),
		Usage: UsageSummary{
			InputTokens:  cloneInt64(record.Usage.InputTokens),
			OutputTokens: cloneInt64(record.Usage.OutputTokens),
			TotalTokens:  cloneInt64(record.Usage.TotalTokens),
		},
		Reason:     strings.TrimSpace(reason),
		NextAction: strings.TrimSpace(nextAction),
		Confidence: "exact",
		Freshness:  "durable",
		SourceRefs: normalizeSourceRefs(refs),
		Redaction:  projectionRedaction(),
		Truncation: TruncationMetadata{Fields: []string{}},
	}
}

func RenderJSON(w io.Writer, doc Document) error {
	data, err := delivery.CanonicalJSON(normalizeDocument(doc))
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func RenderJSONL(w io.Writer, doc Document) error {
	doc = normalizeDocument(doc)
	for _, item := range doc.Items {
		record := struct {
			SchemaVersion string           `json:"schema_version"`
			Command       string           `json:"command"`
			Correlation   Correlation      `json:"correlation"`
			Item          RenderItem       `json:"item"`
			Redaction     RedactionSummary `json:"redaction"`
		}{
			SchemaVersion: RenderSchemaVersion,
			Command:       doc.Command,
			Correlation:   doc.Correlation,
			Item:          item,
			Redaction:     doc.Redaction,
		}
		data, err := delivery.CanonicalJSON(record)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func RenderHuman(doc Document, caps Capabilities) string {
	doc = normalizeDocument(doc)
	width := caps.Width
	if width <= 0 {
		width = 80
	}
	if width < 40 {
		width = 40
	}
	markerOK, markerWarn := "OK", "!"
	if caps.Unicode {
		markerOK, markerWarn = "✓", "!"
	}
	var out bytes.Buffer
	title := "OBSERVABILITY"
	if doc.Command != "" {
		title += " " + doc.Command
	}
	fmt.Fprintln(&out, title)
	if doc.Correlation.DeliveryRunID != "" || doc.Correlation.ProjectID != "" {
		fmt.Fprintln(&out, boundForWidth(fmt.Sprintf("scope: project=%s run=%s source=%s", display(doc.Correlation.ProjectID), display(doc.Correlation.DeliveryRunID), display(doc.Correlation.Source)), width))
	}
	if len(doc.Items) == 0 {
		fmt.Fprintln(&out, "- none")
		return out.String()
	}
	for _, item := range doc.Items {
		marker := markerOK
		if item.Status == "" || strings.Contains(item.Status, "fail") || strings.Contains(item.Status, "human") || strings.Contains(item.Status, "partial") {
			marker = markerWarn
		}
		head := fmt.Sprintf("%s %s %s", marker, display(item.ID), display(item.Status))
		fmt.Fprintln(&out, boundForWidth(head, width))
		line := fmt.Sprintf("  %s/%s duration=%s usage=%s confidence=%s freshness=%s", display(item.Provider), display(item.Model), display(item.Duration), renderUsage(item.Usage), display(item.Confidence), display(item.Freshness))
		fmt.Fprintln(&out, boundForWidth(line, width))
		if item.Reason != "" {
			fmt.Fprintln(&out, boundForWidth("  reason: "+item.Reason, width))
		}
		if item.NextAction != "" {
			fmt.Fprintln(&out, boundForWidth("  next: "+item.NextAction, width))
		}
	}
	return out.String()
}

func BoundedText(value string, maxRunes int) (string, TruncationMetadata) {
	redacted := sanitize.Text(strings.TrimSpace(value))
	if maxRunes <= 0 {
		return redacted, TruncationMetadata{Fields: []string{}}
	}
	runes := []rune(redacted)
	if len(runes) <= maxRunes {
		return redacted, TruncationMetadata{Fields: []string{}}
	}
	omitted := len(runes) - maxRunes
	if strings.Contains(redacted, "[REDACTED") {
		preserved := "[REDACTED] [TRUNCATED]"
		if len([]rune(preserved)) <= maxRunes {
			return preserved, TruncationMetadata{Applied: true, Fields: []string{"text"}, MaxRunes: maxRunes, OmittedRunes: omitted}
		}
		return "[REDACTED]", TruncationMetadata{Applied: true, Fields: []string{"text"}, MaxRunes: maxRunes, OmittedRunes: omitted}
	}
	if maxRunes > 14 {
		redacted = string(runes[:maxRunes-14]) + " [TRUNCATED]"
	} else {
		redacted = string(runes[:maxRunes])
	}
	return redacted, TruncationMetadata{Applied: true, Fields: []string{"text"}, MaxRunes: maxRunes, OmittedRunes: omitted}
}

func normalizeDocument(doc Document) Document {
	if doc.SchemaVersion == "" {
		doc.SchemaVersion = RenderSchemaVersion
	}
	doc.Command = strings.TrimSpace(doc.Command)
	doc.Mode = firstRenderNonEmpty(strings.TrimSpace(doc.Mode), "canonical")
	doc.Items = normalizeRenderItems(doc.Items)
	doc.Evidence = normalizeEvidence(doc.Evidence)
	doc.Redaction = projectionRedaction()
	if doc.Truncation.Fields == nil {
		doc.Truncation.Fields = []string{}
	}
	return doc
}

func normalizeRenderItems(items []RenderItem) []RenderItem {
	if items == nil {
		items = []RenderItem{}
	}
	out := make([]RenderItem, 0, len(items))
	for _, item := range items {
		item.ID = strings.TrimSpace(item.ID)
		item.Kind = strings.TrimSpace(item.Kind)
		item.Status = strings.TrimSpace(item.Status)
		item.Duration = strings.TrimSpace(item.Duration)
		item.Provider = strings.TrimSpace(item.Provider)
		item.Model = strings.TrimSpace(item.Model)
		item.Reason = sanitize.Text(strings.TrimSpace(item.Reason))
		item.NextAction = sanitize.Text(strings.TrimSpace(item.NextAction))
		item.Confidence = firstRenderNonEmpty(strings.TrimSpace(item.Confidence), "unknown")
		item.Freshness = firstRenderNonEmpty(strings.TrimSpace(item.Freshness), "unknown")
		item.SourceRefs = normalizeSourceRefs(item.SourceRefs)
		item.Redaction = projectionRedaction()
		if item.Truncation.Fields == nil {
			item.Truncation.Fields = []string{}
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return renderItemKey(out[i]) < renderItemKey(out[j])
	})
	return out
}

func renderItemKey(item RenderItem) string {
	ref := ""
	if len(item.SourceRefs) > 0 {
		ref = item.SourceRefs[0].Table + "\x00" + item.SourceRefs[0].RecordID
	}
	return ref + "\x00" + item.Kind + "\x00" + item.ID
}

func normalizeSourceRefs(refs []SourceRef) []SourceRef {
	if len(refs) == 0 {
		return []SourceRef{}
	}
	out := append([]SourceRef(nil), refs...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Table+"\x00"+out[i].RecordID+"\x00"+out[i].Field < out[j].Table+"\x00"+out[j].RecordID+"\x00"+out[j].Field
	})
	return out
}

func normalizeEvidence(evidence []Evidence) []Evidence {
	if len(evidence) == 0 {
		return []Evidence{}
	}
	out := append([]Evidence(nil), evidence...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Severity+"\x00"+out[i].Code+"\x00"+out[i].Message < out[j].Severity+"\x00"+out[j].Code+"\x00"+out[j].Message
	})
	return out
}

func formatDuration(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return (time.Duration(ms) * time.Millisecond).String()
}

func renderUsage(usage UsageSummary) string {
	parts := []string{}
	if usage.InputTokens != nil {
		parts = append(parts, "in="+strconv.FormatInt(*usage.InputTokens, 10))
	}
	if usage.OutputTokens != nil {
		parts = append(parts, "out="+strconv.FormatInt(*usage.OutputTokens, 10))
	}
	if usage.TotalTokens != nil {
		parts = append(parts, "total="+strconv.FormatInt(*usage.TotalTokens, 10))
	}
	if len(parts) == 0 {
		return "not-reported"
	}
	return strings.Join(parts, ",")
}

func boundForWidth(value string, width int) string {
	bounded, _ := BoundedText(value, max(width, 40))
	return bounded
}

func display(value string) string {
	if strings.TrimSpace(value) == "" {
		return "not-reported"
	}
	return strings.TrimSpace(value)
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func firstRenderNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
