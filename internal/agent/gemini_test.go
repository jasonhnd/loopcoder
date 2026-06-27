package agent

import (
	"reflect"
	"slices"
	"testing"
)

func TestGeminiRegistration(t *testing.T) {
	runner, err := Lookup("gemini")
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if runner == nil {
		t.Fatal("Lookup returned nil runner")
	}
	if !slices.Contains(SupportedProviders(), "gemini") {
		t.Fatalf("SupportedProviders() = %#v, want gemini", SupportedProviders())
	}
}

func TestBuildGeminiArgs(t *testing.T) {
	tests := []struct {
		name string
		inv  Invocation
		want []string
	}{
		{
			name: "base argv",
			inv: Invocation{
				Prompt: "do the work",
			},
			want: []string{
				"--prompt", "do the work",
				"--yolo",
				"--output-format", "json",
			},
		},
		{
			name: "with model",
			inv: Invocation{
				Prompt: "do the work",
				Model:  "gemini-2.5-pro",
			},
			want: []string{
				"--prompt", "do the work",
				"--yolo",
				"-m", "gemini-2.5-pro",
				"--output-format", "json",
			},
		},
		{
			name: "effort is ignored",
			inv: Invocation{
				Prompt: "do the work",
				Effort: "high",
			},
			want: []string{
				"--prompt", "do the work",
				"--yolo",
				"--output-format", "json",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildGeminiArgs(tt.inv)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuildGeminiArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseGeminiSummary(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "response field",
			in:   `{"response":"Implemented Gemini adapter.","stats":{"models":{}}}`,
			want: "Implemented Gemini adapter.",
		},
		{
			name: "stats only",
			in:   `{"stats":{"models":{}}}`,
			want: "",
		},
		{
			name: "non json fallback",
			in:   "raw final message\n",
			want: "raw final message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseGeminiSummary([]byte(tt.in))
			if got != tt.want {
				t.Fatalf("parseGeminiSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}
