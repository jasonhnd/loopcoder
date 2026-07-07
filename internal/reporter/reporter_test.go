package reporter

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCanonicalJSONShapeAndRoundTrip(t *testing.T) {
	record := validRecord()

	data, err := record.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}

	const want = `{"role":"worker","provider":"codex","model":"gpt-5.5","model_source":"parsed","effort":"xhigh","permission":"write","action":"implement issue #172","exit_code":0,"started_at":"2026-06-28T00:00:00Z","ended_at":"2026-06-28T00:00:42Z","duration_ms":42000,"usage":{"input_tokens":120,"output_tokens":34,"total_tokens":154},"verified":true}`
	if string(data) != want {
		t.Fatalf("canonical JSON = %s, want %s", string(data), want)
	}

	var roundTrip Report
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("round trip unmarshal returned error: %v", err)
	}
	if err := roundTrip.Validate(); err != nil {
		t.Fatalf("round trip Validate returned error: %v", err)
	}
	if roundTrip.Role != RoleWorker || roundTrip.Provider != "codex" || roundTrip.ModelSource != ModelSourceParsed {
		t.Fatalf("round trip identity fields = %#v", roundTrip)
	}
	if roundTrip.Usage.InputTokens == nil || *roundTrip.Usage.InputTokens != 120 {
		t.Fatalf("round trip usage = %#v", roundTrip.Usage)
	}
}

func TestHeaderFormatting(t *testing.T) {
	record := validRecord()

	const want = `[reporter] role=worker provider=codex model=gpt-5.5(parsed) effort=xhigh perm=write action="implement issue #172" exit=0 dur=42s tokens=120/34|154 verified=true`
	if got := record.Header(); got != want {
		t.Fatalf("Header() = %q, want %q", got, want)
	}
}

func TestValidateSuccess(t *testing.T) {
	tests := []struct {
		name   string
		record Report
	}{
		{
			name:   "split and total tokens",
			record: validRecord(),
		},
		{
			name: "total only tokens and empty effort",
			record: Report{
				Role:        RoleConductor,
				Provider:    "codex-cli",
				Model:       "gpt-5.5",
				ModelSource: ModelSourceSelfReported,
				Permission:  PermissionOrchestrate,
				Action:      "dispatch ready issue",
				ExitCode:    0,
				StartedAt:   "2026-06-28T00:00:00Z",
				EndedAt:     "2026-06-28T00:00:01Z",
				DurationMS:  1000,
				Usage: Usage{
					TotalTokens: int64Ptr(42),
				},
				Verified: false,
			},
		},
		{
			name: "split tokens only",
			record: Report{
				Role:        RoleVerifier,
				Provider:    "claude",
				Model:       "opus",
				ModelSource: ModelSourceParsed,
				Effort:      "high",
				Permission:  PermissionReadOnly,
				Action:      "review pull request",
				ExitCode:    0,
				StartedAt:   "2026-06-28T00:00:00Z",
				EndedAt:     "2026-06-28T00:00:02Z",
				DurationMS:  2000,
				Usage: Usage{
					InputTokens:  int64Ptr(10),
					OutputTokens: int64Ptr(20),
				},
				Verified: true,
			},
		},
		{
			name: "antigravity self-reported without usage",
			record: Report{
				Role:        RoleWorker,
				Provider:    "antigravity",
				Model:       "Gemini 3.1 Pro (High)",
				ModelSource: ModelSourceSelfReported,
				Effort:      "High",
				Permission:  PermissionWrite,
				Action:      "implement issue #559",
				ExitCode:    0,
				StartedAt:   "2026-06-28T00:00:00Z",
				EndedAt:     "2026-06-28T00:00:02Z",
				DurationMS:  2000,
				Verified:    true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.record.Validate(); err != nil {
				t.Fatalf("Validate returned error: %v", err)
			}
		})
	}
}

func TestValidateAllowsAntigravityAbsentUsageButNotCodex(t *testing.T) {
	payload := `{"role":"verifier","provider":"antigravity","model":"Gemini 3.1 Pro (High)","model_source":"self-reported","effort":"High","permission":"read-only","action":"review PR #559","exit_code":0,"started_at":"2026-06-28T00:00:00Z","ended_at":"2026-06-28T00:00:02Z","duration_ms":2000,"verified":true}`
	var record Report
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("Antigravity Validate returned error: %v", err)
	}

	record.Provider = "codex"
	record.ModelSource = ModelSourceParsed
	err := record.Validate()
	assertValidationErrorNamesField(t, err, "usage")
}

func TestValidateReportsMissingRequiredFields(t *testing.T) {
	for _, field := range []string{
		"role",
		"provider",
		"model",
		"model_source",
		"permission",
		"action",
		"exit_code",
		"started_at",
		"ended_at",
		"duration_ms",
		"usage",
		"verified",
	} {
		t.Run(field, func(t *testing.T) {
			record := recordWithoutField(t, field)
			assertValidationErrorNamesField(t, record.Validate(), field)
		})
	}
}

func TestValidateReportsInvalidEnums(t *testing.T) {
	tests := []struct {
		name   string
		update func(*Report)
		field  string
	}{
		{
			name: "role",
			update: func(record *Report) {
				record.Role = "builder"
			},
			field: "role",
		},
		{
			name: "model_source",
			update: func(record *Report) {
				record.ModelSource = "guessed"
			},
			field: "model_source",
		},
		{
			name: "permission",
			update: func(record *Report) {
				record.Permission = "admin"
			},
			field: "permission",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := validRecord()
			tt.update(&record)
			assertValidationErrorNamesField(t, record.Validate(), tt.field)
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	var record Report
	err := record.Validate()
	for _, field := range []string{
		"role",
		"provider",
		"model",
		"model_source",
		"permission",
		"action",
		"started_at",
		"ended_at",
		"usage",
	} {
		assertValidationErrorNamesField(t, err, field)
	}
}

func TestValidateReportsInvalidScalarsAndUsage(t *testing.T) {
	tests := []struct {
		name   string
		update func(*Report)
		field  string
	}{
		{
			name: "negative exit code",
			update: func(record *Report) {
				record.ExitCode = -1
			},
			field: "exit_code",
		},
		{
			name: "invalid started_at",
			update: func(record *Report) {
				record.StartedAt = "not-a-time"
			},
			field: "started_at",
		},
		{
			name: "invalid ended_at",
			update: func(record *Report) {
				record.EndedAt = "not-a-time"
			},
			field: "ended_at",
		},
		{
			name: "negative duration",
			update: func(record *Report) {
				record.DurationMS = -1
			},
			field: "duration_ms",
		},
		{
			name: "missing token counts",
			update: func(record *Report) {
				record.Usage = Usage{}
			},
			field: "usage",
		},
		{
			name: "negative token count",
			update: func(record *Report) {
				record.Usage.InputTokens = int64Ptr(-1)
			},
			field: "usage.input_tokens",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := validRecord()
			tt.update(&record)
			assertValidationErrorNamesField(t, record.Validate(), tt.field)
		})
	}
}

func validRecord() Report {
	return Report{
		Role:        RoleWorker,
		Provider:    "codex",
		Model:       "gpt-5.5",
		ModelSource: ModelSourceParsed,
		Effort:      "xhigh",
		Permission:  PermissionWrite,
		Action:      "implement issue #172",
		ExitCode:    0,
		StartedAt:   "2026-06-28T00:00:00Z",
		EndedAt:     "2026-06-28T00:00:42Z",
		DurationMS:  42000,
		Usage: Usage{
			InputTokens:  int64Ptr(120),
			OutputTokens: int64Ptr(34),
			TotalTokens:  int64Ptr(154),
		},
		Verified: true,
	}
}

func recordWithoutField(t *testing.T, field string) Report {
	t.Helper()
	data, err := validRecord().CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal payload returned error: %v", err)
	}
	delete(payload, field)
	data, err = json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload returned error: %v", err)
	}
	var record Report
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("unmarshal record returned error: %v", err)
	}
	return record
}

func assertValidationErrorNamesField(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Validate returned nil error, want field %q", field)
	}
	var validationErr ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Validate error type = %T, want ValidationError", err)
	}
	if !strings.Contains(err.Error(), field) {
		t.Fatalf("Validate error = %q, want field %q", err.Error(), field)
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}
