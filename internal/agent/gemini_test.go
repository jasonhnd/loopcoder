package agent

import (
	"reflect"
	"slices"
	"strings"
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

func TestGeminiEffortAdvisory(t *testing.T) {
	got := geminiEffortAdvisory(" high ")
	for _, want := range []string{"advisory", "gemini ignores effort \"high\"", "no reasoning-effort knob"} {
		if !strings.Contains(got, want) {
			t.Fatalf("advisory missing %q: %q", want, got)
		}
	}
	if empty := geminiEffortAdvisory(" "); empty != "" {
		t.Fatalf("empty effort advisory = %q, want empty", empty)
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

func TestParseGeminiInvocation(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantModel  string
		wantInput  *int64
		wantOutput *int64
		wantTotal  *int64
	}{
		{
			name: "stats models key with token fields",
			output: `{
				"response": "done",
				"stats": {
					"models": {
						"gemini-2.5-pro": {
							"inputTokens": 321,
							"outputTokens": 45,
							"totalTokens": 366
						}
					}
				}
			}`,
			wantModel:  "gemini-2.5-pro",
			wantInput:  testInt64Ptr(321),
			wantOutput: testInt64Ptr(45),
			wantTotal:  testInt64Ptr(366),
		},
		{
			name: "top level model and snake case usage",
			output: `{
				"model": "gemini-2.0-flash",
				"usage": {
					"input_tokens": "1,000",
					"output_tokens": 25,
					"total_tokens": 1025
				}
			}`,
			wantModel:  "gemini-2.0-flash",
			wantInput:  testInt64Ptr(1000),
			wantOutput: testInt64Ptr(25),
			wantTotal:  testInt64Ptr(1025),
		},
		{
			name: "total only leaves split absent",
			output: `{
				"model": "gemini-2.5-pro",
				"usage": {
					"totalTokenCount": 2048
				}
			}`,
			wantModel: "gemini-2.5-pro",
			wantTotal: testInt64Ptr(2048),
		},
		{
			name: "split without total keeps input and output only",
			output: `{
				"model": "gemini-2.5-pro",
				"usage": {
					"promptTokenCount": 90,
					"candidatesTokenCount": 12
				}
			}`,
			wantModel:  "gemini-2.5-pro",
			wantInput:  testInt64Ptr(90),
			wantOutput: testInt64Ptr(12),
		},
		{
			name:   "non json output yields empty metadata",
			output: "auth required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseGeminiInvocation([]byte(tt.output))
			if got.Model != tt.wantModel {
				t.Fatalf("Model = %q, want %q", got.Model, tt.wantModel)
			}
			if got.Effort != "" {
				t.Fatalf("Effort = %q, want empty", got.Effort)
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
			if tt.wantTotal == nil {
				assertNilInt64Ptr(t, got.Usage.TotalTokens)
			} else {
				assertInt64Ptr(t, got.Usage.TotalTokens, *tt.wantTotal)
			}
		})
	}
}
