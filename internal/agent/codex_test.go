package agent

import (
	"reflect"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/attestation"
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

func TestParseCodexMetadata(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantModel  string
		wantEffort string
		wantUsage  attestation.Usage
	}{
		{
			name: "stdout header and token footer",
			output: `OpenAI Codex v0.142.2
--------
workdir: C:\repo
model: gpt-5-codex
provider: openai
approval: never
sandbox: danger-full-access
reasoning effort: high
--------
user
do the work

tokens used: 12,345
`,
			wantModel:  "gpt-5-codex",
			wantEffort: "high",
			wantUsage: attestation.Usage{
				TotalTokens: int64Ptr(12345),
			},
		},
		{
			name:      "missing fields stay empty",
			output:    "plain log without header",
			wantUsage: attestation.Usage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCodexMetadata([]byte(tt.output))
			if got.Model != tt.wantModel || got.Effort != tt.wantEffort || !reflect.DeepEqual(got.Usage, tt.wantUsage) {
				t.Fatalf("parseCodexMetadata() = %#v, want model=%q effort=%q usage=%#v", got, tt.wantModel, tt.wantEffort, tt.wantUsage)
			}
		})
	}
}
