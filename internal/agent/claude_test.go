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
				"--permission-mode", "plan",
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
