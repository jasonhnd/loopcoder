package agent

import (
	"reflect"
	"slices"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/attestation"
)

func TestClaudeRegistration(t *testing.T) {
	runner, err := Lookup("claude")
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if runner == nil {
		t.Fatal("Lookup returned nil runner")
	}

	providers := SupportedProviders()
	if !slices.Contains(providers, "claude") {
		t.Fatalf("SupportedProviders() = %#v, want claude registered", providers)
	}
}

func TestBuildClaudeArgs(t *testing.T) {
	tests := []struct {
		name string
		inv  Invocation
		want []string
	}{
		{
			name: "base argv",
			want: []string{
				"--print",
				"--dangerously-skip-permissions",
				"--output-format", "json",
			},
		},
		{
			name: "with model",
			inv: Invocation{
				Model: "claude-opus-4-20250514",
			},
			want: []string{
				"--print",
				"--dangerously-skip-permissions",
				"--output-format", "json",
				"--model", "claude-opus-4-20250514",
			},
		},
		{
			name: "with effort",
			inv: Invocation{
				Effort: "high",
			},
			want: []string{
				"--print",
				"--dangerously-skip-permissions",
				"--output-format", "json",
				"--effort", "high",
			},
		},
		{
			name: "read-only argv",
			inv: Invocation{
				ReadOnly: true,
			},
			want: []string{
				"--print",
				"--allowedTools", "Read Grep Glob",
				"--output-format", "json",
			},
		},
		{
			name: "with output schema",
			inv: Invocation{
				OutputSchema: `{"type":"object"}`,
			},
			want: []string{
				"--print",
				"--dangerously-skip-permissions",
				"--output-format", "json",
				"--json-schema", `{"type":"object"}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildClaudeArgs(tt.inv)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuildClaudeArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseClaudeSummary(t *testing.T) {
	payload := []byte(`{
		"type": "result",
		"subtype": "success",
		"is_error": false,
		"result": "Implemented the adapter and tests.\nVerification passed."
	}`)

	got := parseClaudeSummary(payload)
	want := "Implemented the adapter and tests.\nVerification passed."
	if got != want {
		t.Fatalf("parseClaudeSummary() = %q, want %q", got, want)
	}
}

func TestParseClaudeMetadata(t *testing.T) {
	tests := []struct {
		name       string
		output     []byte
		inv        Invocation
		wantModel  string
		wantEffort string
		wantUsage  attestation.Usage
	}{
		{
			name: "modelUsage key and usage tokens",
			output: []byte(`{
				"type": "result",
				"result": "done",
				"modelUsage": {
					"claude-sonnet-4-20250514": {
						"input_tokens": 120,
						"output_tokens": 45
					}
				},
				"usage": {
					"input_tokens": 120,
					"output_tokens": 45
				}
			}`),
			inv:        Invocation{Effort: "high"},
			wantModel:  "claude-sonnet-4-20250514",
			wantEffort: "high",
			wantUsage: attestation.Usage{
				InputTokens:  int64Ptr(120),
				OutputTokens: int64Ptr(45),
			},
		},
		{
			name:       "invalid json leaves parse fields empty but preserves invocation effort",
			output:     []byte(`not json`),
			inv:        Invocation{Effort: "low"},
			wantEffort: "low",
			wantUsage:  attestation.Usage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseClaudeMetadata(tt.output, tt.inv)
			if got.Model != tt.wantModel || got.Effort != tt.wantEffort || !reflect.DeepEqual(got.Usage, tt.wantUsage) {
				t.Fatalf("parseClaudeMetadata() = %#v, want model=%q effort=%q usage=%#v", got, tt.wantModel, tt.wantEffort, tt.wantUsage)
			}
		})
	}
}
