package audit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/attestation"
)

func TestNormalizeLayersAllowsLLMAndAll(t *testing.T) {
	got, err := NormalizeLayers([]string{"all"})
	if err != nil {
		t.Fatalf("NormalizeLayers returned error: %v", err)
	}
	if want := []string{LayerSAST, LayerLLM}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeLayers(all) = %#v, want %#v", got, want)
	}

	got, err = NormalizeLayers([]string{"llm,sast,llm"})
	if err != nil {
		t.Fatalf("NormalizeLayers returned error: %v", err)
	}
	if want := []string{LayerLLM, LayerSAST}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeLayers(llm,sast,llm) = %#v, want %#v", got, want)
	}
}

func TestLLMReviewBuildsPromptAndReadOnlyInvocation(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs", "security"), 0o700); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "security", "audit-rubric.md"), []byte("CUSTOM RUBRIC: check shared-host prompt disclosure."), 0o600); err != nil {
		t.Fatalf("write rubric: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	writeAuditConfig(t, repo, `
adapters:
  verifier: codex
verifier:
  model: review-model
  reasoning_effort: high
audit:
  review:
    rubric_path: docs/security/audit-rubric.md
mcp:
  servers:
    - name: read-index
      command: ./read-index
      roles: [verifier]
      read_only: true
    - name: write-index
      command: ./write-index
      roles: [verifier]
      read_only: false
`)

	var captured agent.Invocation
	result, err := Run(context.Background(), Options{
		RepoPath: repo,
		Layers:   []string{LayerLLM},
	}, Deps{
		AgentLookup: func(provider string) (agent.Runner, error) {
			if provider != "codex" {
				t.Fatalf("provider = %q, want codex", provider)
			}
			return fakeAgentRunner(func(_ context.Context, inv agent.Invocation) (agent.Result, error) {
				captured = inv
				return validAgentResult(`{"findings":[{"id":"llm:shared-host:main.go","layer":"llm","severity":"high","file":"main.go","line":1,"rule":"llm:shared-host","category":"shared-host-disclosure","message":"Prompt material may be disclosed.","evidence":"main.go is representative evidence in this fake review."}],"evidence":"reviewed bounded packet"}`), nil
			}), nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if !captured.ReadOnly {
		t.Fatal("Invocation.ReadOnly = false, want true")
	}
	if captured.Role != "verifier" {
		t.Fatalf("Invocation.Role = %q, want verifier", captured.Role)
	}
	if captured.Model != "review-model" || captured.Effort != "high" {
		t.Fatalf("model/effort = %q/%q, want review-model/high", captured.Model, captured.Effort)
	}
	if captured.OutputSchema != AuditReviewJSONSchema {
		t.Fatal("Invocation.OutputSchema did not use audit review schema")
	}
	if len(captured.MCPServers) != 1 || captured.MCPServers[0].Name != "read-index" {
		t.Fatalf("MCPServers = %#v, want only read-index", captured.MCPServers)
	}
	for _, want := range []string{"Operator-trusted inputs", "Untrusted inputs", "supply-chain integrity", "CUSTOM RUBRIC", "main.go"} {
		if !strings.Contains(captured.Prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, captured.Prompt)
		}
	}

	if result.Verdict != VerdictFindings || ExitCode(result) != 1 {
		t.Fatalf("verdict/exit = %s/%d, want findings/1", result.Verdict, ExitCode(result))
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings len = %d, want 1", len(result.Findings))
	}
	finding := result.Findings[0]
	if finding.Layer != LayerLLM || finding.Tool != auditReviewTool || finding.Severity != SeverityHigh || finding.Rule != "llm:shared-host" {
		t.Fatalf("finding not normalized: %#v", finding)
	}
	if result.Attestation == nil {
		t.Fatal("Attestation = nil, want verifier attestation")
	}
	if err := result.Attestation.Validate(); err != nil {
		t.Fatalf("attestation did not validate: %v", err)
	}
	if result.Attestation.Role != attestation.RoleVerifier || result.Attestation.Permission != attestation.PermissionReadOnly || !result.Attestation.Verified {
		t.Fatalf("attestation semantics = %#v", result.Attestation)
	}
}

func TestLLMReviewExplicitProviderModelEffortOverridesConfig(t *testing.T) {
	repo := t.TempDir()
	writeAuditConfig(t, repo, `
adapters:
  verifier: claude
verifier:
  model: config-model
  reasoning_effort: low
`)

	var got agent.Invocation
	result, err := Run(context.Background(), Options{
		RepoPath:          repo,
		Layers:            []string{LayerLLM},
		Provider:          "codex",
		Model:             "override-model",
		Effort:            "xhigh",
		ThresholdOverride: SeverityLow,
	}, Deps{
		AgentLookup: func(provider string) (agent.Runner, error) {
			if provider != "codex" {
				t.Fatalf("provider = %q, want explicit codex", provider)
			}
			return fakeAgentRunner(func(_ context.Context, inv agent.Invocation) (agent.Result, error) {
				got = inv
				return validAgentResult(`{"findings":[],"evidence":"clean fake review"}`), nil
			}), nil
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got.Model != "override-model" || got.Effort != "xhigh" {
		t.Fatalf("model/effort = %q/%q, want explicit override-model/xhigh", got.Model, got.Effort)
	}
	if result.Verdict != VerdictClean {
		t.Fatalf("verdict = %s, want clean", result.Verdict)
	}
}

func TestLLMReviewNeedsHumanFallbacks(t *testing.T) {
	tests := []struct {
		name          string
		config        string
		opts          Options
		lookup        func(*testing.T) func(string) (agent.Runner, error)
		wantReason    string
		wantRunnerRun bool
	}{
		{
			name:   "provider lookup error",
			config: "",
			opts:   Options{Provider: "missing"},
			lookup: func(t *testing.T) func(string) (agent.Runner, error) {
				return func(string) (agent.Runner, error) {
					return nil, errors.New("no such provider")
				}
			},
			wantReason: "resolve verifier provider",
		},
		{
			name: "provider infrastructure error",
			opts: Options{Provider: "codex"},
			lookup: func(t *testing.T) func(string) (agent.Runner, error) {
				return func(string) (agent.Runner, error) {
					return fakeAgentRunner(func(context.Context, agent.Invocation) (agent.Result, error) {
						return validAgentResult(`{"findings":[],"evidence":"unused"}`), errors.New("provider crashed")
					}), nil
				}
			},
			wantReason:    "audit review failed",
			wantRunnerRun: true,
		},
		{
			name: "provider timeout",
			opts: Options{Provider: "codex"},
			lookup: func(t *testing.T) func(string) (agent.Runner, error) {
				return func(string) (agent.Runner, error) {
					return fakeAgentRunner(func(context.Context, agent.Invocation) (agent.Result, error) {
						result := validAgentResult(`{"findings":[],"evidence":"unused"}`)
						result.Hung = true
						result.HungReason = agent.HungReasonDeadline
						return result, nil
					}), nil
				}
			},
			wantReason:    "timed out",
			wantRunnerRun: true,
		},
		{
			name: "nonzero provider exit",
			opts: Options{Provider: "codex"},
			lookup: func(t *testing.T) func(string) (agent.Runner, error) {
				return func(string) (agent.Runner, error) {
					return fakeAgentRunner(func(context.Context, agent.Invocation) (agent.Result, error) {
						result := validAgentResult(`{"findings":[],"evidence":"unused"}`)
						result.ExitCode = 7
						return result, nil
					}), nil
				}
			},
			wantReason:    "exited with code 7",
			wantRunnerRun: true,
		},
		{
			name: "unreadable configured rubric",
			config: `
audit:
  review:
    rubric_path: docs/missing.md
`,
			opts: Options{Provider: "codex"},
			lookup: func(t *testing.T) func(string) (agent.Runner, error) {
				return func(string) (agent.Runner, error) {
					return fakeAgentRunner(func(context.Context, agent.Invocation) (agent.Result, error) {
						t.Fatal("runner should not be called when rubric is unreadable")
						return agent.Result{}, nil
					}), nil
				}
			},
			wantReason: "read audit.review.rubric_path",
		},
		{
			name: "malformed JSON",
			opts: Options{Provider: "codex"},
			lookup: func(t *testing.T) func(string) (agent.Runner, error) {
				return func(string) (agent.Runner, error) {
					return fakeAgentRunner(func(context.Context, agent.Invocation) (agent.Result, error) {
						return validAgentResult(`not json`), nil
					}), nil
				}
			},
			wantReason:    "structured audit review parse failed",
			wantRunnerRun: true,
		},
		{
			name: "schema violation",
			opts: Options{Provider: "codex"},
			lookup: func(t *testing.T) func(string) (agent.Runner, error) {
				return func(string) (agent.Runner, error) {
					return fakeAgentRunner(func(context.Context, agent.Invocation) (agent.Result, error) {
						return validAgentResult(`{"findings":[{"layer":"llm","severity":"medium","file":"","rule":"llm:test","category":"bounded-io","message":"missing id","evidence":"missing id"}],"evidence":"reviewed"}`), nil
					}), nil
				}
			},
			wantReason:    "missing id",
			wantRunnerRun: true,
		},
		{
			name: "missing attestation metadata",
			opts: Options{Provider: "codex"},
			lookup: func(t *testing.T) func(string) (agent.Runner, error) {
				return func(string) (agent.Runner, error) {
					return fakeAgentRunner(func(context.Context, agent.Invocation) (agent.Result, error) {
						return agent.Result{
							ExitCode: 0,
							Summary:  `{"findings":[],"evidence":"clean fake review"}`,
						}, nil
					}), nil
				}
			},
			wantReason:    "incomplete verifier attestation",
			wantRunnerRun: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			if strings.TrimSpace(tt.config) != "" {
				writeAuditConfig(t, repo, tt.config)
			} else {
				writeAuditConfig(t, repo, "{}\n")
			}
			lookup := tt.lookup(t)
			runs := 0
			result, err := Run(context.Background(), Options{
				RepoPath: repo,
				Layers:   []string{LayerLLM},
				Provider: tt.opts.Provider,
			}, Deps{
				AgentLookup: func(provider string) (agent.Runner, error) {
					runner, err := lookup(provider)
					if runner == nil || err != nil {
						return runner, err
					}
					return fakeAgentRunner(func(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
						runs++
						return runner.Run(ctx, inv)
					}), nil
				},
			})
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if result.Verdict != VerdictNeedsHuman || ExitCode(result) != 2 {
				t.Fatalf("verdict/exit = %s/%d, want needs-human/2 result=%#v", result.Verdict, ExitCode(result), result)
			}
			if !needsHumanContains(result.NeedsHuman, tt.wantReason) {
				t.Fatalf("needs_human missing %q: %#v", tt.wantReason, result.NeedsHuman)
			}
			if tt.wantRunnerRun && runs == 0 {
				t.Fatal("runner was not called")
			}
			if !tt.wantRunnerRun && runs != 0 {
				t.Fatalf("runner called %d times, want 0", runs)
			}
		})
	}
}

func validAgentResult(summary string) agent.Result {
	started := time.Date(2026, 7, 5, 1, 2, 3, 0, time.UTC)
	ended := started.Add(2 * time.Second)
	total := int64(123)
	return agent.Result{
		ExitCode:   0,
		Summary:    summary,
		Model:      "resolved-review-model",
		Effort:     "resolved-effort",
		StartedAt:  started.Format(time.RFC3339Nano),
		EndedAt:    ended.Format(time.RFC3339Nano),
		DurationMS: int64((2 * time.Second).Milliseconds()),
		Usage: attestation.Usage{
			TotalTokens: &total,
		},
	}
}

type fakeAgentRunner func(context.Context, agent.Invocation) (agent.Result, error)

func (f fakeAgentRunner) Run(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
	return f(ctx, inv)
}

func needsHumanContains(items []NeedsHuman, want string) bool {
	for _, item := range items {
		if strings.Contains(item.Reason, want) {
			return true
		}
	}
	return false
}
