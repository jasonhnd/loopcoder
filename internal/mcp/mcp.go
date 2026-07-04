package mcp

import (
	"fmt"
	"strings"

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
	role = normalizeRole(role)
	if !validRole(role) {
		return nil, fmt.Errorf("invalid MCP invocation role %q", role)
	}
	if len(cfg.Servers) == 0 {
		return nil, nil
	}

	servers := make([]agent.MCPServer, 0, len(cfg.Servers))
	for index, server := range cfg.Servers {
		if err := validateServerRoles(index, server.Roles); err != nil {
			return nil, err
		}
		if !roleAllowed(server.Roles, role) {
			continue
		}
		if readOnly && !server.ReadOnly {
			continue
		}
		servers = append(servers, copyServer(server))
	}
	if len(servers) == 0 {
		return nil, nil
	}
	return servers, nil
}

func validateServerRoles(index int, roles []string) error {
	for _, role := range roles {
		normalized := normalizeRole(role)
		if !validRole(normalized) {
			return fmt.Errorf("invalid delivery config: mcp.servers[%d].roles contains unknown role %q", index, role)
		}
	}
	return nil
}

func roleAllowed(roles []string, role string) bool {
	if len(roles) == 0 {
		return true
	}
	for _, candidate := range roles {
		if normalizeRole(candidate) == role {
			return true
		}
	}
	return false
}

func validRole(role string) bool {
	switch role {
	case RoleWorker, RoleVerifier:
		return true
	default:
		return false
	}
}

func normalizeRole(role string) string {
	return strings.ToLower(strings.TrimSpace(role))
}

func copyServer(server config.MCPServer) agent.MCPServer {
	return agent.MCPServer{
		Name:      server.Name,
		Transport: server.Transport,
		Command:   server.Command,
		Args:      append([]string(nil), server.Args...),
		URL:       server.URL,
		Auth: agent.MCPAuth{
			Header: server.Auth.Header,
			Env:    server.Auth.Env,
		},
		Roles:    append([]string(nil), server.Roles...),
		ReadOnly: server.ReadOnly,
	}
}
