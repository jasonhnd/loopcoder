package agent

import (
	"reflect"
	"testing"
)

func TestBuildCodexArgs(t *testing.T) {
	tests := []struct {
		name string
		inv  Invocation
		want []string
	}{
		{
			name: "base argv",
			inv: Invocation{
				WorktreePath: "wt",
				LogPath:      "codex.log",
			},
			want: []string{
				"exec",
				"--cd", "wt",
				"--dangerously-bypass-approvals-and-sandbox",
				"--skip-git-repo-check",
				"-o", "summary.txt",
				"-",
			},
		},
		{
			name: "model and effort",
			inv: Invocation{
				WorktreePath: "wt",
				LogPath:      "codex.log",
				Model:        "gpt-5",
				Effort:       "high",
			},
			want: []string{
				"exec",
				"--cd", "wt",
				"--dangerously-bypass-approvals-and-sandbox",
				"--skip-git-repo-check",
				"-m", "gpt-5",
				"-c", "model_reasoning_effort=high",
				"-o", "summary.txt",
				"-",
			},
		},
		{
			name: "read-only argv",
			inv: Invocation{
				WorktreePath: "wt",
				LogPath:      "codex.log",
				ReadOnly:     true,
			},
			want: []string{
				"exec",
				"--cd", "wt",
				"-s", "read-only",
				"--skip-git-repo-check",
				"-o", "summary.txt",
				"-",
			},
		},
		{
			name: "with output schema",
			inv: Invocation{
				WorktreePath: "wt",
				LogPath:      "codex.log",
				OutputSchema: "schema.json",
			},
			want: []string{
				"exec",
				"--cd", "wt",
				"--dangerously-bypass-approvals-and-sandbox",
				"--skip-git-repo-check",
				"--output-schema", "schema.json",
				"-o", "summary.txt",
				"-",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildCodexArgs(tt.inv)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuildCodexArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestBuildCodexReadOnlyVerifierArgs(t *testing.T) {
	schema := `{"type":"object"}`
	got := BuildCodexArgs(Invocation{
		WorktreePath: "wt",
		LogPath:      "codex.log",
		ReadOnly:     true,
		OutputSchema: schema,
	})
	want := []string{
		"exec",
		"--cd", "wt",
		"-s", "read-only",
		"--skip-git-repo-check",
		"--output-schema", "schema.json",
		"-o", "summary.txt",
		"-",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildCodexArgs() = %#v, want %#v", got, want)
	}
	assertArgsDoNotContain(t, got, "dangerously-bypass-approvals-and-sandbox", "approval", "plan")
}

func TestParseCodexInvocation(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantModel  string
		wantEffort string
		wantTotal  *int64
	}{
		{
			name: "real two-line token total",
			output: `model: gpt-5.5
provider: openai
reasoning effort: xhigh

tokens used
15,988
`,
			wantModel:  "gpt-5.5",
			wantEffort: "xhigh",
			wantTotal:  testInt64Ptr(15988),
		},
		{
			name: "inline token total",
			output: `model: gpt-5
reasoning effort: high
tokens used: 1,234
`,
			wantModel:  "gpt-5",
			wantEffort: "high",
			wantTotal:  testInt64Ptr(1234),
		},
		{
			name:   "missing header fields",
			output: "raw output without attestation header",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCodexInvocation([]byte(tt.output))
			if got.Model != tt.wantModel {
				t.Fatalf("Model = %q, want %q", got.Model, tt.wantModel)
			}
			if got.Effort != tt.wantEffort {
				t.Fatalf("Effort = %q, want %q", got.Effort, tt.wantEffort)
			}
			assertNilInt64Ptr(t, got.Usage.InputTokens)
			assertNilInt64Ptr(t, got.Usage.OutputTokens)
			if tt.wantTotal == nil {
				assertNilInt64Ptr(t, got.Usage.TotalTokens)
			} else {
				assertInt64Ptr(t, got.Usage.TotalTokens, *tt.wantTotal)
			}
		})
	}
}
