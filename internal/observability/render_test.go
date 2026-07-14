package observability

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/reporter"
)

func TestRenderHumanCapabilitiesNarrowWideUnicodeASCII(t *testing.T) {
	total := int64(1234)
	doc := NewDocument("status", Correlation{ProjectID: "proj-a", DeliveryRunID: "run-a", Source: "status"}, []RenderItem{
		ItemFromReport("run-a#101", "worker", "succeeded", "completed implementation", "review PR", reporter.Report{
			WorkID:      "run-a",
			Issue:       101,
			Role:        reporter.RoleWorker,
			Provider:    "codex",
			Model:       "gpt-5.5",
			ModelSource: reporter.ModelSourceParsed,
			Effort:      "high",
			Permission:  reporter.PermissionWrite,
			DurationMS:  42000,
			Usage:       reporter.Usage{TotalTokens: &total},
		}, []SourceRef{{Table: "reports", RecordID: "report-101", Provenance: "durable"}}),
	}, nil)

	wide := RenderHuman(doc, Capabilities{Width: 120, Unicode: true})
	for _, want := range []string{"OBSERVABILITY status", "✓ run-a#101 succeeded", "codex/gpt-5.5", "duration=42s", "usage=total=1234", "next: review PR"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide unicode output missing %q:\n%s", want, wide)
		}
	}

	narrowASCII := RenderHuman(doc, Capabilities{Width: 44, Unicode: false, Redirected: true})
	if strings.Contains(narrowASCII, "✓") {
		t.Fatalf("ASCII output used unicode marker:\n%s", narrowASCII)
	}
	for _, line := range strings.Split(strings.TrimSpace(narrowASCII), "\n") {
		if len([]rune(line)) > 44 {
			t.Fatalf("narrow line length = %d, want <= 44: %q\n%s", len([]rune(line)), line, narrowASCII)
		}
	}
}

func TestRenderHumanColorAndStableRecordIDPathBehavior(t *testing.T) {
	attemptPath := filepath.Join(string(os.PathSeparator), "tmp", "repo", ".loopcoder", "runs", "run-color", "workers", "job-1.attempt.json")
	doc := NewDocument("dispatch", Correlation{DeliveryRunID: "run-color"}, []RenderItem{{
		ID:         "job-1",
		Kind:       "worker",
		Status:     "succeeded",
		Confidence: "exact",
		Freshness:  "durable",
		SourceRefs: []SourceRef{{Table: "worker_attempts", RecordID: attemptPath, Provenance: "durable"}},
	}}, nil)

	colored := RenderHuman(doc, Capabilities{Width: 100, Unicode: true, Color: true})
	if !strings.Contains(colored, "\x1b[32m✓") || !strings.Contains(colored, "\x1b[0m") {
		t.Fatalf("color-capable output did not include ANSI success marker:\n%q", colored)
	}
	redirected := RenderHuman(doc, Capabilities{Width: 100, Unicode: true, Color: true, Redirected: true})
	if strings.Contains(redirected, "\x1b[") {
		t.Fatalf("redirected output used ANSI color:\n%q", redirected)
	}

	if got := StableRecordID(attemptPath); got != "job-1" {
		t.Fatalf("known attempt path id = %q, want job-1", got)
	}
	if a, b := StableRecordID("tenant/a/run"), StableRecordID("tenant/b/run"); a == b || a == "run" || b == "run" {
		t.Fatalf("slash durable ids collapsed: a=%q b=%q", a, b)
	}
}

func TestRenderJSONAndJSONLStableOrderingAndNormalizedCollections(t *testing.T) {
	doc := NewDocument("dispatch-wave", Correlation{DeliveryRunID: "run-wave"}, []RenderItem{
		{ID: "b", Kind: "worker", Status: "failed", SourceRefs: []SourceRef{{Table: "workers", RecordID: "b", Provenance: "durable"}}},
		{ID: "a", Kind: "worker", Status: "succeeded", SourceRefs: []SourceRef{{Table: "workers", RecordID: "a", Provenance: "durable"}}},
	}, nil)

	var first bytes.Buffer
	if err := RenderJSON(&first, doc); err != nil {
		t.Fatalf("RenderJSON first: %v", err)
	}
	var second bytes.Buffer
	if err := RenderJSON(&second, doc); err != nil {
		t.Fatalf("RenderJSON second: %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("JSON rendering is unstable:\nfirst=%s\nsecond=%s", first.String(), second.String())
	}
	if !strings.Contains(first.String(), `"schema_version":"loopcoder.observability_render.v1"`) || !strings.Contains(first.String(), `"evidence":[]`) {
		t.Fatalf("JSON missing schema or normalized empty arrays:\n%s", first.String())
	}
	if strings.Index(first.String(), `"id":"a"`) > strings.Index(first.String(), `"id":"b"`) {
		t.Fatalf("items were not ordered by durable source:\n%s", first.String())
	}

	var jsonl bytes.Buffer
	if err := RenderJSONL(&jsonl, doc); err != nil {
		t.Fatalf("RenderJSONL: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(jsonl.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("jsonl line count = %d, want 2:\n%s", len(lines), jsonl.String())
	}
	for _, line := range lines {
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("jsonl line did not parse: %v\n%s", err, line)
		}
		if payload["schema_version"] != RenderSchemaVersion {
			t.Fatalf("jsonl schema = %#v", payload["schema_version"])
		}
	}
}

func TestBoundedTextRedactsBeforeTruncating(t *testing.T) {
	canary := "sk-" + strings.Repeat("A1", 24)
	got, meta := BoundedText("prefix "+canary+" suffix", 24)
	if strings.Contains(got, canary) || strings.Contains(got, "sk-") || strings.Contains(got, "A1A1") {
		t.Fatalf("bounded text leaked secret fragment: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("bounded text did not redact first: %q", got)
	}
	if !meta.Applied || meta.OmittedRunes == 0 {
		t.Fatalf("truncation metadata = %#v, want applied", meta)
	}
}
