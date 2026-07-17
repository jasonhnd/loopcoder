package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/config"
	"gopkg.in/yaml.v3"
)

func TestMCPValidationMatrixSharedByParseAndAgent(t *testing.T) {
	tests := []struct {
		name         string
		serversYAML  string
		role         string
		readOnly     bool
		wantParseErr bool
		wantAgentErr bool
		wantErr      string
		wantNames    []string
	}{
		{
			name: "valid stdio explicit",
			serversYAML: `
    - name: local-index
      transport: stdio
      command: ./tools/local-index
      args: ["--root", "."]
      roles: [worker]
`,
			role:      "worker",
			wantNames: []string{"local-index"},
		},
		{
			name: "valid http auth",
			serversYAML: `
    - name: remote-read
      transport: http
      url: https://mcp.example.com/read
      auth:
        header: Authorization
        env: REMOTE_MCP_TOKEN
      roles: [verifier]
      read_only: true
`,
			role:      "verifier",
			readOnly:  true,
			wantNames: []string{"remote-read"},
		},
		{
			name: "valid inferred transports",
			serversYAML: `
    - name: inferred-stdio
      command: ./tools/local-index
      roles: [worker]
    - name: inferred-http
      url: http://mcp.example.com/read
      roles: [worker]
      read_only: true
`,
			role:      "worker",
			wantNames: []string{"inferred-stdio", "inferred-http"},
		},
		{
			name: "stdio empty arg remains accepted",
			serversYAML: `
    - name: local-index
      transport: stdio
      command: ./tools/local-index
      args: [""]
`,
			wantNames: []string{"local-index"},
		},
		{
			name: "invalid role",
			serversYAML: `
    - name: future
      command: ./tools/future
      roles: [planner]
`,
			role:         "worker",
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "unknown role",
		},
		{
			name: "invalid name",
			serversYAML: `
    - name: bad/name
      command: ./tools/local-index
`,
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "safe provider MCP server name",
		},
		{
			name: "stdio missing command",
			serversYAML: `
    - name: local-index
      transport: stdio
      roles: [worker]
`,
			role:         "worker",
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "stdio transport requires command",
		},
		{
			name: "stdio cannot include url",
			serversYAML: `
    - name: local-index
      transport: stdio
      command: ./tools/local-index
      url: https://mcp.example.com/read
`,
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "stdio transport cannot include url",
		},
		{
			name: "stdio cannot use auth",
			serversYAML: `
    - name: local-index
      transport: stdio
      command: ./tools/local-index
      auth:
        header: Authorization
        env: LOCAL_MCP_TOKEN
`,
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "stdio transport cannot use HTTP auth",
		},
		{
			name: "http missing url",
			serversYAML: `
    - name: remote
      transport: http
      roles: [worker]
`,
			role:         "worker",
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "http transport requires url",
		},
		{
			name: "http cannot include command",
			serversYAML: `
    - name: remote
      transport: http
      command: ./tools/local-index
      url: https://mcp.example.com/read
`,
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "http transport cannot include command or args",
		},
		{
			name: "invalid url",
			serversYAML: `
    - name: remote
      transport: http
      url: ftp://mcp.example.com/read
`,
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "http transport requires an http or https url",
		},
		{
			name: "auth missing env",
			serversYAML: `
    - name: remote
      transport: http
      url: https://mcp.example.com/read
      auth:
        header: Authorization
`,
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "http auth requires both header and env",
		},
		{
			name: "invalid header",
			serversYAML: `
    - name: remote
      transport: http
      url: https://mcp.example.com/read
      auth:
        header: "Authorization: Bearer token"
        env: REMOTE_MCP_TOKEN
`,
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "http auth header",
		},
		{
			name: "invalid env",
			serversYAML: `
    - name: remote
      transport: http
      url: https://mcp.example.com/read
      auth:
        header: Authorization
        env: "TOKEN VALUE"
`,
			wantParseErr: true,
			wantAgentErr: true,
			wantErr:      "http auth env",
		},
		{
			name: "role filtering",
			serversYAML: `
    - name: worker-index
      transport: stdio
      command: ./tools/worker-index
      roles: [worker]
    - name: verifier-read
      transport: http
      url: https://mcp.example.com/verifier
      roles: [verifier]
      read_only: true
`,
			role:      "worker",
			wantNames: []string{"worker-index"},
		},
		{
			name: "read-only selected write server",
			serversYAML: `
    - name: verifier-write
      transport: stdio
      command: ./tools/verifier-write
      roles: [verifier]
`,
			role:         "verifier",
			readOnly:     true,
			wantAgentErr: true,
			wantErr:      "not locally classified read-only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "mcp:\n  servers:\n" + strings.TrimPrefix(tt.serversYAML, "\n")
			_, parseErr := config.Parse([]byte(body))
			if tt.wantParseErr {
				if parseErr == nil {
					t.Fatal("Parse returned nil error, want MCP validation error")
				}
				if !strings.Contains(parseErr.Error(), "mcp.servers[0]") || !strings.Contains(parseErr.Error(), tt.wantErr) {
					t.Fatalf("Parse error = %v, want path and %q", parseErr, tt.wantErr)
				}
			} else if parseErr != nil {
				t.Fatalf("Parse returned error: %v", parseErr)
			}

			var decoded struct {
				MCP config.MCP `yaml:"mcp"`
			}
			if err := yaml.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("yaml.Unmarshal returned error: %v", err)
			}
			got, agentErr := mcpServersForInvocation(Invocation{
				Role:       tt.role,
				ReadOnly:   tt.readOnly,
				MCPServers: decoded.MCP.Servers,
			})
			if tt.wantAgentErr {
				if agentErr == nil {
					t.Fatal("mcpServersForInvocation returned nil error, want MCP validation error")
				}
				if !strings.Contains(agentErr.Error(), "mcp.servers[0]") || !strings.Contains(agentErr.Error(), tt.wantErr) {
					t.Fatalf("agent error = %v, want path and %q", agentErr, tt.wantErr)
				}
				return
			}
			if agentErr != nil {
				t.Fatalf("mcpServersForInvocation returned error: %v", agentErr)
			}
			if gotNames := mcpServerNames(got); !reflect.DeepEqual(gotNames, tt.wantNames) {
				t.Fatalf("selected MCP servers = %v, want %v", gotNames, tt.wantNames)
			}
		})
	}
}

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

func mcpServerNames(servers []MCPServer) []string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, strings.TrimSpace(server.Name))
	}
	return names
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

func TestNestedInvocationRolesMapToRegisteredMCPRoles(t *testing.T) {
	if got := mcpInvocationRole("nested-read-only"); got != "verifier" {
		t.Fatalf("nested read-only role = %q, want verifier", got)
	}
	if got := mcpInvocationRole("nested-bounded-write"); got != "worker" {
		t.Fatalf("nested bounded-write role = %q, want worker", got)
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
