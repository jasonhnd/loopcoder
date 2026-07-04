package agent

import (
	"reflect"
	"testing"
)

func TestMCPServersDoNotAffectProviderArgsYet(t *testing.T) {
	base := Invocation{
		WorktreePath: "wt",
		Prompt:       "review",
		LogPath:      "agent.log",
		ReadOnly:     true,
		OutputSchema: `{"type":"object"}`,
		Model:        "model",
		Effort:       "high",
	}
	withMCP := base
	withMCP.MCPServers = []MCPServer{{
		Name:      "shared-read",
		Transport: "stdio",
		Command:   "./tools/shared-read",
		Args:      []string{"--root", "."},
		Roles:     []string{"worker", "verifier"},
		ReadOnly:  true,
	}}

	if got, want := BuildCodexArgs(withMCP), BuildCodexArgs(base); !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildCodexArgs with MCP = %#v, want %#v", got, want)
	}
	if got, want := BuildClaudeArgs(withMCP), BuildClaudeArgs(base); !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildClaudeArgs with MCP = %#v, want %#v", got, want)
	}
	if got, want := BuildGeminiArgs(withMCP), BuildGeminiArgs(base); !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildGeminiArgs with MCP = %#v, want %#v", got, want)
	}
}
