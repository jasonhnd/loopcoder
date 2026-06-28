package agent

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/attestation"
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
		{
			name: "read-only argv",
			inv: Invocation{
				Prompt:   "do the work",
				LogPath:  "gemini.log",
				ReadOnly: true,
			},
			want: []string{
				"--prompt", "do the work",
				"--skip-trust",
				"--extensions", "none",
				"--output-format", "json",
			},
		},
		{
			name: "output schema uses json output only",
			inv: Invocation{
				Prompt:       "do the work",
				OutputSchema: `{"type":"object"}`,
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

func TestGeminiReadOnlySettingsDisableTools(t *testing.T) {
	if !strings.Contains(geminiReadOnlySettings, `"tools":{"core":[]}`) {
		t.Fatalf("geminiReadOnlySettings does not disable tools:\n%s", geminiReadOnlySettings)
	}
	for _, forbidden := range []string{"write_file", "replace", "run_shell_command", "read_file", "glob", "grep_search"} {
		if strings.Contains(geminiReadOnlySettings, forbidden) {
			t.Fatalf("geminiReadOnlySettings should not enable %q:\n%s", forbidden, geminiReadOnlySettings)
		}
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

func TestParseGeminiMetadata(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		wantModel string
		wantUsage attestation.Usage
	}{
		{
			name: "usage metadata",
			output: `{
				"response": "done",
				"model": "gemini-2.5-pro",
				"usageMetadata": {
					"promptTokenCount": 321,
					"candidatesTokenCount": 76,
					"totalTokenCount": 397
				}
			}`,
			wantModel: "gemini-2.5-pro",
			wantUsage: attestation.Usage{
				InputTokens:  int64Ptr(321),
				OutputTokens: int64Ptr(76),
				TotalTokens:  int64Ptr(397),
			},
		},
		{
			name: "stats model key and tokens",
			output: `{
				"response": "done",
				"stats": {
					"models": {
						"gemini-2.5-flash": {
							"tokens": {
								"input": 12,
								"output": 34,
								"total": 46
							}
						}
					}
				}
			}`,
			wantModel: "gemini-2.5-flash",
			wantUsage: attestation.Usage{
				InputTokens:  int64Ptr(12),
				OutputTokens: int64Ptr(34),
				TotalTokens:  int64Ptr(46),
			},
		},
		{
			name:      "auth failure text leaves metadata empty",
			output:    `You are not logged in. Run gemini auth.`,
			wantUsage: attestation.Usage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseGeminiMetadata([]byte(tt.output))
			if got.Model != tt.wantModel || got.Effort != "" || !reflect.DeepEqual(got.Usage, tt.wantUsage) {
				t.Fatalf("parseGeminiMetadata() = %#v, want model=%q effort empty usage=%#v", got, tt.wantModel, tt.wantUsage)
			}
		})
	}
}
