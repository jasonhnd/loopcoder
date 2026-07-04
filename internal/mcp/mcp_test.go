package mcp

import (
	"reflect"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/config"
)

func TestServersForInvocationFiltersByRoleAndReadOnly(t *testing.T) {
	cfg := config.MCP{
		Servers: []config.MCPServer{
			{
				Name:      "worker-index",
				Transport: "stdio",
				Command:   "./tools/worker-index",
				Args:      []string{"--root", "."},
				Roles:     []string{"worker"},
				ReadOnly:  false,
			},
			{
				Name:      "shared-read",
				Transport: "http",
				URL:       "https://mcp.example.com/shared",
				Auth: config.MCPAuth{
					Header: "Authorization",
					Env:    "SHARED_MCP_TOKEN",
				},
				Roles:    []string{"worker", "verifier"},
				ReadOnly: true,
			},
			{
				Name:      "verifier-write",
				Transport: "stdio",
				Command:   "./tools/verifier-write",
				Roles:     []string{"verifier"},
			},
		},
	}

	workerServers, err := ServersForInvocation(cfg, RoleWorker, false)
	if err != nil {
		t.Fatalf("ServersForInvocation worker returned error: %v", err)
	}
	wantWorker := []agent.MCPServer{
		{
			Name:      "worker-index",
			Transport: "stdio",
			Command:   "./tools/worker-index",
			Args:      []string{"--root", "."},
			Roles:     []string{"worker"},
		},
		{
			Name:      "shared-read",
			Transport: "http",
			URL:       "https://mcp.example.com/shared",
			Auth: agent.MCPAuth{
				Header: "Authorization",
				Env:    "SHARED_MCP_TOKEN",
			},
			Roles:    []string{"worker", "verifier"},
			ReadOnly: true,
		},
	}
	if !reflect.DeepEqual(workerServers, wantWorker) {
		t.Fatalf("worker MCP servers = %#v, want %#v", workerServers, wantWorker)
	}

	verifierServers, err := ServersForInvocation(cfg, RoleVerifier, true)
	if err != nil {
		t.Fatalf("ServersForInvocation verifier returned error: %v", err)
	}
	wantVerifier := []agent.MCPServer{wantWorker[1]}
	if !reflect.DeepEqual(verifierServers, wantVerifier) {
		t.Fatalf("verifier MCP servers = %#v, want %#v", verifierServers, wantVerifier)
	}
}

func TestServersForInvocationAllowsUnrestrictedRoles(t *testing.T) {
	cfg := config.MCP{
		Servers: []config.MCPServer{{
			Name:      "repo-index",
			Transport: "stdio",
			Command:   "./tools/repo-index",
			ReadOnly:  true,
		}},
	}

	servers, err := ServersForInvocation(cfg, RoleVerifier, true)
	if err != nil {
		t.Fatalf("ServersForInvocation returned error: %v", err)
	}
	if len(servers) != 1 || servers[0].Name != "repo-index" {
		t.Fatalf("servers = %#v, want unrestricted repo-index", servers)
	}
}

func TestServersForInvocationRejectsUnknownRoles(t *testing.T) {
	cfg := config.MCP{
		Servers: []config.MCPServer{{
			Name:  "future",
			Roles: []string{"planner"},
		}},
	}

	if _, err := ServersForInvocation(cfg, RoleWorker, false); err == nil {
		t.Fatal("ServersForInvocation returned nil error for unknown configured role")
	}
	if _, err := ServersForInvocation(config.MCP{}, "planner", false); err == nil {
		t.Fatal("ServersForInvocation returned nil error for unknown invocation role")
	}
}
