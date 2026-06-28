package agent

import (
	"reflect"
	"slices"
	"testing"
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

func TestBuildClaudeReadOnlyVerifierArgs(t *testing.T) {
	schema := `{"type":"object"}`
	got := BuildClaudeArgs(Invocation{
		ReadOnly:     true,
		OutputSchema: schema,
	})
	want := []string{
		"--print",
		"--allowedTools", "Read Grep Glob",
		"--output-format", "json",
		"--json-schema", schema,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildClaudeArgs() = %#v, want %#v", got, want)
	}
	assertArgsDoNotContain(t, got, "dangerously-skip-permissions", "--permission-mode", "approval", "plan")
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

func TestParseClaudeInvocation(t *testing.T) {
	tests := []struct {
		name       string
		output     []byte
		inv        Invocation
		wantModel  string
		wantEffort string
		wantInput  *int64
		wantOutput *int64
	}{
		{
			name: "captured json result with model usage key and split tokens",
			output: []byte(`{
				"type": "result",
				"subtype": "success",
				"result": "done",
				"usage": {
					"input_tokens": 1234,
					"output_tokens": 567
				},
				"modelUsage": {
					"claude-opus-4-20250514": {
						"inputTokens": 1234,
						"outputTokens": 567
					}
				}
			}`),
			inv:        Invocation{Effort: "high"},
			wantModel:  "claude-opus-4-20250514",
			wantEffort: "high",
			wantInput:  testInt64Ptr(1234),
			wantOutput: testInt64Ptr(567),
		},
		{
			name:       "invalid json keeps invocation effort only",
			output:     []byte(`not json`),
			inv:        Invocation{Effort: "xhigh"},
			wantEffort: "xhigh",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseClaudeInvocation(tt.output, tt.inv)
			if got.Model != tt.wantModel {
				t.Fatalf("Model = %q, want %q", got.Model, tt.wantModel)
			}
			if got.Effort != tt.wantEffort {
				t.Fatalf("Effort = %q, want %q", got.Effort, tt.wantEffort)
			}
			if tt.wantInput == nil {
				assertNilInt64Ptr(t, got.Usage.InputTokens)
			} else {
				assertInt64Ptr(t, got.Usage.InputTokens, *tt.wantInput)
			}
			if tt.wantOutput == nil {
				assertNilInt64Ptr(t, got.Usage.OutputTokens)
			} else {
				assertInt64Ptr(t, got.Usage.OutputTokens, *tt.wantOutput)
			}
			assertNilInt64Ptr(t, got.Usage.TotalTokens)
		})
	}
}

func testInt64Ptr(value int64) *int64 {
	return &value
}
