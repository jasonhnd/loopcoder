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
