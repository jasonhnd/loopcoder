package agent

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
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
				"--prompt", "",
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
				"--prompt", "",
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
				"--prompt", "",
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
				"--prompt", "",
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
				"--prompt", "",
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
			assertArgsDoNotContain(t, got, tt.inv.Prompt)
		})
	}
}

func TestBuildGeminiArgsWithMCPServers(t *testing.T) {
	got := BuildGeminiArgs(Invocation{
		Prompt: "do the work",
		Role:   "worker",
		MCPServers: []MCPServer{
			{
				Name:      "worker-index",
				Transport: "stdio",
				Command:   "./tools/worker-index",
				Args:      []string{"--root", "."},
				Roles:     []string{"worker"},
			},
			{
				Name:      "shared-read",
				Transport: "http",
				URL:       "https://mcp.example.com/shared",
				Roles:     []string{"worker", "verifier"},
				ReadOnly:  true,
			},
		},
	})
	want := []string{
		"--prompt", "",
		"--yolo",
		"--allowed-mcp-server-names", "worker-index",
		"--allowed-mcp-server-names", "shared-read",
		"--output-format", "json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildGeminiArgs() = %#v, want %#v", got, want)
	}
	assertArgsDoNotContain(t, got, "do the work")
}

func TestGeminiRunnerFeedsPromptOnStdinAndCreatesSensitiveFilesPrivate(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "gemini.log")
	prompt := "do the sensitive work"
	restore := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		stdin, err := io.ReadAll(cmd.Stdin)
		if err != nil {
			t.Fatalf("read stdin: %v", err)
		}
		if string(stdin) != prompt {
			t.Fatalf("stdin = %q, want prompt %q", string(stdin), prompt)
		}
		assertArgsDoNotContain(t, cmd.Args, prompt)
		_, _ = io.WriteString(cmd.Stdout, `{"response":"done","model":"gemini-2.5-pro","usage":{"totalTokens":42}}`)
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restore()

	result, err := GeminiRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       prompt,
		LogPath:      logPath,
		ReadOnly:     true,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	assertPrivateFileMode(t, logPath)
	assertPrivateFileMode(t, geminiReadOnlySettingsPath(logPath))
}

func TestGeminiSettingsJSONWithReadOnlyMCP(t *testing.T) {
	data, err := geminiSettingsJSON(true, []MCPServer{
		{
			Name:      "local-index",
			Transport: "stdio",
			Command:   "./tools/local-index",
			Args:      []string{"--root", "."},
			ReadOnly:  true,
		},
		{
			Name:      "shared-read",
			Transport: "http",
			URL:       "https://mcp.example.com/shared",
			Auth: MCPAuth{
				Header: "Authorization",
				Env:    "SHARED_MCP_TOKEN",
			},
			ReadOnly: true,
		},
	})
	if err != nil {
		t.Fatalf("geminiSettingsJSON returned error: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`"tools": {`,
		`"core": []`,
		`"mcpServers": {`,
		`"local-index"`,
		`"command": "./tools/local-index"`,
		`"shared-read"`,
		`"type": "http"`,
		`"httpUrl": "https://mcp.example.com/shared"`,
		`"Authorization": "Bearer ${SHARED_MCP_TOKEN}"`,
		`"allowed": [`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("gemini settings missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "secret") {
		t.Fatalf("gemini settings should not contain hardcoded secrets:\n%s", text)
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
