package agent

import (
	"strings"
	"testing"
)

func TestMCPServersForInvocationFiltersByRole(t *testing.T) {
	got, err := mcpServersForInvocation(Invocation{
		Role:     "verifier",
		ReadOnly: true,
		MCPServers: []MCPServer{
			{
				Name:      "worker-write",
				Transport: "stdio",
				Command:   "./tools/worker-write",
				Roles:     []string{"worker"},
			},
			{
				Name:      "verifier-read",
				Transport: "http",
				URL:       "https://mcp.example.com/verifier",
				Roles:     []string{"verifier"},
				ReadOnly:  true,
			},
		},
	})
	if err != nil {
		t.Fatalf("mcpServersForInvocation returned error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "verifier-read" {
		t.Fatalf("mcpServersForInvocation() = %#v, want only verifier-read", got)
	}
}

func TestMCPServersForInvocationRejectsWriteServerForReadOnly(t *testing.T) {
	_, err := mcpServersForInvocation(Invocation{
		Role:     "verifier",
		ReadOnly: true,
		MCPServers: []MCPServer{{
			Name:      "verifier-write",
			Transport: "stdio",
			Command:   "./tools/verifier-write",
			Roles:     []string{"verifier"},
		}},
	})
	if err == nil {
		t.Fatal("mcpServersForInvocation returned nil error for write-capable verifier MCP server")
	}
	if !strings.Contains(err.Error(), "not locally classified read-only") {
		t.Fatalf("error = %v, want local read-only classification failure", err)
	}
}

func TestMCPServersForInvocationRejectsUnknownRoles(t *testing.T) {
	_, err := mcpServersForInvocation(Invocation{
		Role: "worker",
		MCPServers: []MCPServer{{
			Name:      "future",
			Transport: "stdio",
			Command:   "./tools/future",
			Roles:     []string{"planner"},
		}},
	})
	if err == nil {
		t.Fatal("mcpServersForInvocation returned nil error for unknown MCP role")
	}
	if !strings.Contains(err.Error(), "unknown role") || !strings.Contains(err.Error(), "planner") {
		t.Fatalf("error = %v, want unknown role context", err)
	}
}

func TestMCPServersForInvocationRejectsUnsafeHTTPAuth(t *testing.T) {
	tests := []struct {
		name    string
		auth    MCPAuth
		wantErr string
	}{
		{
			name:    "missing env",
			auth:    MCPAuth{Header: "Authorization"},
			wantErr: "requires both header and env",
		},
		{
			name:    "header contains value",
			auth:    MCPAuth{Header: "Authorization: Bearer token", Env: "TOKEN"},
			wantErr: "auth header",
		},
		{
			name:    "invalid env",
			auth:    MCPAuth{Header: "Authorization", Env: "TOKEN VALUE"},
			wantErr: "auth env",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mcpServersForInvocation(Invocation{
				Role: "worker",
				MCPServers: []MCPServer{{
					Name:      "remote",
					Transport: "http",
					URL:       "https://mcp.example.com/remote",
					Auth:      tt.auth,
					Roles:     []string{"worker"},
				}},
			})
			if err == nil {
				t.Fatal("mcpServersForInvocation returned nil error for unsafe HTTP auth")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
