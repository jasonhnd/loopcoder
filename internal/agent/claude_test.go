package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
				"--output-format", "stream-json", "--verbose",
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
				"--output-format", "stream-json", "--verbose",
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
				"--output-format", "stream-json", "--verbose",
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
				"--safe-mode",
				"--no-session-persistence",
				"--allowedTools", "Read Grep Glob",
				"--output-format", "stream-json", "--verbose",
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
				"--output-format", "stream-json", "--verbose",
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

func TestBuildClaudeReadOnlyMCPArgs(t *testing.T) {
	logPath := filepath.Join("logs", "claude.log")
	got := BuildClaudeArgs(Invocation{
		LogPath:  logPath,
		ReadOnly: true,
		Role:     "verifier",
		MCPServers: []MCPServer{{
			Name:      "shared-read",
			Transport: "http",
			URL:       "https://mcp.example.com/shared",
			Auth: MCPAuth{
				Header: "Authorization",
				Env:    "SHARED_MCP_TOKEN",
			},
			Roles:    []string{"verifier"},
			ReadOnly: true,
		}},
	})
	want := []string{
		"--print",
		"--permission-mode", "plan",
		"--no-session-persistence",
		"--allowedTools", "Read Grep Glob mcp__shared-read__*",
		"--output-format", "stream-json", "--verbose",
		"--mcp-config", filepath.Join("logs", "claude-mcp.json"),
		"--strict-mcp-config",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildClaudeArgs() = %#v, want %#v", got, want)
	}
	assertArgsDoNotContain(t, got, "--safe-mode", "dangerously-skip-permissions")
}

func TestWriteClaudeMCPConfigUsesEnvPlaceholders(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "claude.log")
	err := writeClaudeMCPConfig(logPath, []MCPServer{
		{
			Name:      "local-index",
			Transport: "stdio",
			Command:   "./tools/local-index",
			Args:      []string{"--root", "."},
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
		t.Fatalf("writeClaudeMCPConfig returned error: %v", err)
	}
	data, err := os.ReadFile(claudeMCPConfigPath(logPath))
	if err != nil {
		t.Fatalf("read claude MCP config: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`"local-index"`,
		`"type": "stdio"`,
		`"command": "./tools/local-index"`,
		`"shared-read"`,
		`"type": "http"`,
		`"url": "https://mcp.example.com/shared"`,
		`"Authorization": "Bearer ${SHARED_MCP_TOKEN}"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("claude MCP config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "secret") {
		t.Fatalf("claude MCP config should not contain hardcoded secrets:\n%s", text)
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
		"--safe-mode",
		"--no-session-persistence",
		"--allowedTools", "Read Grep Glob",
		"--output-format", "stream-json", "--verbose",
		"--json-schema", schema,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildClaudeArgs() = %#v, want %#v", got, want)
	}
	assertArgsDoNotContain(t, got, "dangerously-skip-permissions", "--permission-mode", "approval", "plan")
}

func TestParseClaudeStreamJSONOutput(t *testing.T) {
	tests := []struct {
		name        string
		output      []byte
		inv         Invocation
		wantSummary string
		wantModel   string
		wantEffort  string
		wantInput   *int64
		wantOutput  *int64
		wantTotal   *int64
	}{
		{
			name: "structured output result event with usage and primary model",
			output: []byte(`{"type":"system","subtype":"init","session_id":"abc123"}
{"type":"system","subtype":"hook_started","hook":"SessionStart"}
{"type":"system","subtype":"hook_response","hook":"SessionStart","result":"ok"}
{"type":"assistant","message":{"content":[{"type":"text","text":"reviewing"}]}}
{"type":"user","message":{"content":[{"type":"text","text":"continue"}]}}
{"type":"result","subtype":"success","result":"Pass. Structured output contains the verdict.","structured_output":{"verdict":"pass","findings":[],"evidence":"streamed final event","spec_conformance":"pass"},"usage":{"input_tokens":33346,"output_tokens":4,"total_tokens":33350},"modelUsage":{"claude-opus-4-8[1m]":{"inputTokens":33346,"outputTokens":4},"claude-haiku-4-5-20251001":{"inputTokens":10,"outputTokens":1}}}
{"type":"assistant"`),
			inv:         Invocation{Effort: "max"},
			wantSummary: `{"verdict":"pass","findings":[],"evidence":"streamed final event","spec_conformance":"pass"}`,
			wantModel:   "claude-opus-4-8[1m]",
			wantEffort:  "max",
			wantInput:   testInt64Ptr(33346),
			wantOutput:  testInt64Ptr(4),
			wantTotal:   testInt64Ptr(33350),
		},
		{
			name: "result event without structured output returns result text",
			output: []byte(`{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}
{"type":"result","subtype":"success","result":"Implemented the adapter and tests.\nVerification passed.","usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15},"modelUsage":{"claude-sonnet-4-20250514":{"inputTokens":12,"outputTokens":3}}}`),
			wantSummary: "Implemented the adapter and tests.\nVerification passed.",
			wantModel:   "claude-sonnet-4-20250514",
			wantInput:   testInt64Ptr(12),
			wantOutput:  testInt64Ptr(3),
			wantTotal:   testInt64Ptr(15),
		},
		{
			name: "noise stream without result event returns empty summary",
			output: []byte(`not-json
{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"still working"}]}}
{"type":"result"`),
			inv:         Invocation{Effort: "xhigh"},
			wantSummary: "",
			wantEffort:  "xhigh",
		},
		{
			name: "last result event wins",
			output: []byte(`{"type":"result","result":"first","usage":{"input_tokens":1},"modelUsage":{"claude-a":{"inputTokens":1,"outputTokens":0}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"more"}]}}
{"type":"result","result":"second","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"modelUsage":{"claude-b":{"inputTokens":2,"outputTokens":3}}}`),
			wantSummary: "second",
			wantModel:   "claude-b",
			wantInput:   testInt64Ptr(2),
			wantOutput:  testInt64Ptr(3),
			wantTotal:   testInt64Ptr(5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSummary := parseClaudeSummary(tt.output)
			if gotSummary != tt.wantSummary {
				t.Fatalf("parseClaudeSummary() = %q, want %q", gotSummary, tt.wantSummary)
			}

			gotInvocation := parseClaudeInvocation(tt.output, tt.inv)
			if gotInvocation.Model != tt.wantModel {
				t.Fatalf("Model = %q, want %q", gotInvocation.Model, tt.wantModel)
			}
			if gotInvocation.Effort != tt.wantEffort {
				t.Fatalf("Effort = %q, want %q", gotInvocation.Effort, tt.wantEffort)
			}
			if tt.wantInput == nil {
				assertNilInt64Ptr(t, gotInvocation.Usage.InputTokens)
			} else {
				assertInt64Ptr(t, gotInvocation.Usage.InputTokens, *tt.wantInput)
			}
			if tt.wantOutput == nil {
				assertNilInt64Ptr(t, gotInvocation.Usage.OutputTokens)
			} else {
				assertInt64Ptr(t, gotInvocation.Usage.OutputTokens, *tt.wantOutput)
			}
			if tt.wantTotal == nil {
				assertNilInt64Ptr(t, gotInvocation.Usage.TotalTokens)
			} else {
				assertInt64Ptr(t, gotInvocation.Usage.TotalTokens, *tt.wantTotal)
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

func TestParseClaudeSummaryPrefersStructuredOutput(t *testing.T) {
	payload := []byte(`{
		"type": "result",
		"subtype": "success",
		"result": "Pass. The structured output contains the verdict.",
		"structured_output": {
			"verdict": "pass",
			"findings": [],
			"evidence": "bounded packet complete",
			"spec_conformance": "pass"
		}
	}`)

	got := parseClaudeSummary(payload)
	want := `{"verdict":"pass","findings":[],"evidence":"bounded packet complete","spec_conformance":"pass"}`
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
		wantTotal  *int64
	}{
		{
			name: "captured json result with model usage key and split plus total tokens",
			output: []byte(`{
				"type": "result",
				"subtype": "success",
				"result": "done",
				"usage": {
					"input_tokens": 1234,
					"output_tokens": 567,
					"total_tokens": 1801
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
			wantTotal:  testInt64Ptr(1801),
		},
		{
			name: "split without total keeps input and output only",
			output: []byte(`{
				"type": "result",
				"subtype": "success",
				"result": "done",
				"usage": {
					"input_tokens": 2447,
					"output_tokens": 71
				},
				"modelUsage": {
					"claude-haiku-4-5-20251001": {
						"inputTokens": 446,
						"outputTokens": 11
					},
					"claude-opus-4-8[1m]": {
						"inputTokens": 2447,
						"outputTokens": 71
					}
				}
			}`),
			wantModel:  "claude-opus-4-8[1m]",
			wantInput:  testInt64Ptr(2447),
			wantOutput: testInt64Ptr(71),
		},
		{
			name: "pinned model present wins over higher token auxiliary",
			output: []byte(`{
				"type": "result",
				"subtype": "success",
				"result": "done",
				"modelUsage": {
					"claude-haiku-4-5-20251001": {
						"inputTokens": 1000,
						"outputTokens": 200
					},
					"claude-opus-4-8[1m]": {
						"inputTokens": 100,
						"outputTokens": 10
					}
				}
			}`),
			inv:       Invocation{Model: "claude-opus-4-8[1m]"},
			wantModel: "claude-opus-4-8[1m]",
		},
		{
			name: "pinned model absent falls back to primary model",
			output: []byte(`{
				"type": "result",
				"subtype": "success",
				"result": "done",
				"modelUsage": {
					"claude-haiku-4-5-20251001": {
						"inputTokens": 1000,
						"outputTokens": 200
					}
				}
			}`),
			inv:       Invocation{Model: "claude-opus-4-8[1m]"},
			wantModel: "claude-haiku-4-5-20251001",
		},
		{
			name: "unset model uses primary model",
			output: []byte(`{
				"type": "result",
				"subtype": "success",
				"result": "done",
				"modelUsage": {
					"claude-haiku-4-5-20251001": {
						"inputTokens": 1000,
						"outputTokens": 200
					},
					"claude-opus-4-8[1m]": {
						"inputTokens": 100,
						"outputTokens": 10
					}
				}
			}`),
			wantModel: "claude-haiku-4-5-20251001",
		},
		{
			name: "total only leaves split absent",
			output: []byte(`{
				"type": "result",
				"subtype": "success",
				"result": "done",
				"usage": {
					"total_tokens": 42
				},
				"modelUsage": {
					"claude-opus-4-20250514": {}
				}
			}`),
			wantModel: "claude-opus-4-20250514",
			wantTotal: testInt64Ptr(42),
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
			if tt.wantTotal == nil {
				assertNilInt64Ptr(t, got.Usage.TotalTokens)
			} else {
				assertInt64Ptr(t, got.Usage.TotalTokens, *tt.wantTotal)
			}
		})
	}
}

func testInt64Ptr(value int64) *int64 {
	return &value
}
