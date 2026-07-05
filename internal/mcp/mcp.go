package mcp

import (
	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/config"
)

const (
	RoleWorker   = "worker"
	RoleVerifier = "verifier"
)

// ServersForInvocation selects configured MCP servers for a provider-neutral
// agent invocation. Empty role lists are unrestricted; read-only invocations
// receive only servers locally classified as read-only.
func ServersForInvocation(cfg config.MCP, role string, readOnly bool) ([]agent.MCPServer, error) {
	return config.MCPServersForInvocation(cfg, config.MCPInvocationOptions{
		Role:           role,
		ReadOnly:       readOnly,
		ReadOnlyPolicy: config.MCPReadOnlyFilter,
		RequireRole:    true,
		ErrorPrefix:    "invalid delivery config: ",
	})
}
