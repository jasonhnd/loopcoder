package hostprofile_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/hostprofile"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

func TestResolvePrefersEnvOverConfigAndDetection(t *testing.T) {
	resolved, err := hostprofile.Resolve(hostprofile.Options{
		Profile: "claude-code",
		Getenv: mapGetenv(map[string]string{
			hostprofile.EnvName: "codex",
			"PASEO_AGENT_ID":    "agent-1",
		}),
	})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if resolved.Name != "codex-cli" || resolved.Source != hostprofile.SourceEnv || resolved.Selector != hostprofile.EnvName {
		t.Fatalf("resolved = %#v, want codex-cli from env", resolved)
	}
}

func TestResolveUsesConfigBeforeDetection(t *testing.T) {
	resolved, err := hostprofile.Resolve(hostprofile.Options{
		Profile: "claudecode",
		Getenv:  mapGetenv(map[string]string{"PASEO_HOST": "127.0.0.1:6768"}),
	})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if resolved.Name != "claude-code" || resolved.Source != hostprofile.SourceConfig || resolved.Selector != "host.profile" {
		t.Fatalf("resolved = %#v, want claude-code from config", resolved)
	}
}

func TestResolveDetectsKnownHosts(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "paseo", env: map[string]string{"PASEO_AGENT_ID": "agent-1"}, want: "paseo-style"},
		{name: "claude", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "session-1"}, want: "claude-code"},
		{name: "codex", env: map[string]string{"CODEX_THREAD_ID": "thread-1"}, want: "codex-cli"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := hostprofile.Resolve(hostprofile.Options{Getenv: mapGetenv(tt.env)})
			if err != nil {
				t.Fatalf("Resolve returned error: %v", err)
			}
			if resolved.Name != tt.want || resolved.Source != hostprofile.SourceDetection || len(resolved.DetectedBy) == 0 {
				t.Fatalf("resolved = %#v, want %s from detection", resolved, tt.want)
			}
		})
	}
}

func TestResolveFallsBackToGenericLocal(t *testing.T) {
	resolved, err := hostprofile.Resolve(hostprofile.Options{Getenv: mapGetenv(nil)})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if resolved.Name != "generic-local" || resolved.Source != hostprofile.SourceFallback {
		t.Fatalf("resolved = %#v, want generic-local fallback", resolved)
	}
}

func TestResolveRejectsUnknownExplicitProfile(t *testing.T) {
	_, err := hostprofile.Resolve(hostprofile.Options{
		Getenv: mapGetenv(map[string]string{hostprofile.EnvName: "made-up"}),
	})
	if err == nil {
		t.Fatal("Resolve returned nil error, want unknown profile")
	}
	for _, want := range []string{`unknown host profile "made-up"`, hostprofile.EnvName, "codex-cli", "claude-code", "paseo-style"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want containing %q", err.Error(), want)
		}
	}
}

func TestCodexOriginBindingRequestRedactsThreadBearerAndPaths(t *testing.T) {
	secret := "AKIA" + strings.Repeat("A", 16)
	req, ok := hostprofile.CodexOriginBindingRequest(hostprofile.OriginOptions{
		ProjectID:     "proj_codex",
		DeliveryRunID: "run_codex",
		CorrelationID: "corr_codex",
		Getenv: mapGetenv(map[string]string{
			"CODEX_THREAD_ID": "thread-" + secret,
			"CODEX_CLI":       "1",
			"PWD":             "/Users/alice/private/repo",
		}),
	})
	if !ok {
		t.Fatal("CodexOriginBindingRequest returned ok=false")
	}
	binding := runtimecap.BindHostRunOrigin(req)
	if !binding.Bound || binding.Code != runtimecap.HostOriginBound || binding.BindingID == "" {
		t.Fatalf("binding = %#v, want bound Codex origin", binding)
	}
	data, err := json.Marshal(binding)
	if err != nil {
		t.Fatalf("marshal binding: %v", err)
	}
	text := string(data)
	for _, forbidden := range []string{secret, "thread-", "/Users/alice", "CODEX_THREAD_ID\":\"thread"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("binding leaked %q: %s", forbidden, text)
		}
	}
	if !containsHostProfileString(binding.MetadataKeys, "env.CODEX_THREAD_ID") || !containsHostProfileString(binding.MetadataKeys, "env.CODEX_CLI") {
		t.Fatalf("metadata keys = %#v, want marker names only", binding.MetadataKeys)
	}
}

func TestCodexOriginBindingRequestAbsentMetadataIsNotCapabilityProof(t *testing.T) {
	if _, ok := hostprofile.CodexOriginBindingRequest(hostprofile.OriginOptions{
		ProjectID:     "proj_codex",
		DeliveryRunID: "run_codex",
		CorrelationID: "corr_codex",
		Getenv:        mapGetenv(map[string]string{"CODEX_CLI": "1"}),
	}); ok {
		t.Fatal("CodexOriginBindingRequest ok=true without thread/session metadata")
	}
}

func TestClaudeOriginBindingRequestRedactsSessionAndPaths(t *testing.T) {
	secret := "AKIA" + strings.Repeat("B", 16)
	req, ok := hostprofile.ClaudeOriginBindingRequest(hostprofile.OriginOptions{
		ProjectID:     "proj_claude",
		DeliveryRunID: "run_claude",
		CorrelationID: "corr_claude",
		Getenv: mapGetenv(map[string]string{
			"CLAUDE_CODE_SESSION_ID": "session-" + secret,
			"CLAUDECODE":             "1",
			"CLAUDE_CODE_ENTRYPOINT": "/Users/alice/.claude/local",
			"PWD":                    "/Users/alice/private/repo",
		}),
	})
	if !ok {
		t.Fatal("ClaudeOriginBindingRequest returned ok=false")
	}
	binding := runtimecap.BindHostRunOrigin(req)
	if !binding.Bound || binding.Code != runtimecap.HostOriginBound || binding.BindingID == "" {
		t.Fatalf("binding = %#v, want bound Claude origin", binding)
	}
	data, err := json.Marshal(binding)
	if err != nil {
		t.Fatalf("marshal binding: %v", err)
	}
	text := string(data)
	for _, forbidden := range []string{secret, "session-", "/Users/alice", "CLAUDE_CODE_SESSION_ID\":\"session"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("binding leaked %q: %s", forbidden, text)
		}
	}
	for _, want := range []string{"env.CLAUDE_CODE_SESSION_ID", "env.CLAUDECODE", "env.CLAUDE_CODE_ENTRYPOINT"} {
		if !containsHostProfileString(binding.MetadataKeys, want) {
			t.Fatalf("metadata keys = %#v, want marker %s", binding.MetadataKeys, want)
		}
	}
}

func TestClaudeOriginBindingRequestAbsentMetadataIsNotCapabilityProof(t *testing.T) {
	if _, ok := hostprofile.ClaudeOriginBindingRequest(hostprofile.OriginOptions{
		ProjectID:     "proj_claude",
		DeliveryRunID: "run_claude",
		CorrelationID: "corr_claude",
		Getenv:        mapGetenv(map[string]string{"CLAUDECODE": "1"}),
	}); ok {
		t.Fatal("ClaudeOriginBindingRequest ok=true without session metadata")
	}
}

func TestPaseoOriginBindingRequestRedactsAgentIDAndMarkers(t *testing.T) {
	secret := "AKIA" + strings.Repeat("C", 16)
	req, ok := hostprofile.PaseoOriginBindingRequest(hostprofile.OriginOptions{
		ProjectID:     "proj_paseo",
		DeliveryRunID: "run_paseo",
		CorrelationID: "corr_paseo",
		Getenv: mapGetenv(map[string]string{
			"PASEO_AGENT_ID": "agent-" + secret,
			"PASEO_HOST":     "127.0.0.1:6767",
			"PWD":            "/Users/alice/private/repo",
		}),
	})
	if !ok {
		t.Fatal("PaseoOriginBindingRequest returned ok=false")
	}
	binding := runtimecap.BindHostRunOrigin(req)
	if !binding.Bound || binding.Code != runtimecap.HostOriginBound || binding.BindingID == "" {
		t.Fatalf("binding = %#v, want bound Paseo origin", binding)
	}
	data, err := json.Marshal(binding)
	if err != nil {
		t.Fatalf("marshal binding: %v", err)
	}
	text := string(data)
	for _, forbidden := range []string{secret, "agent-", "127.0.0.1", "/Users/alice", "PASEO_AGENT_ID\":\"agent"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("binding leaked %q: %s", forbidden, text)
		}
	}
	for _, want := range []string{"env.PASEO_AGENT_ID", "env.PASEO_HOST"} {
		if !containsHostProfileString(binding.MetadataKeys, want) {
			t.Fatalf("metadata keys = %#v, want marker %s", binding.MetadataKeys, want)
		}
	}
}

func TestPaseoHostMarkerWithoutAgentIDIsNotOriginOrWakeProof(t *testing.T) {
	if _, ok := hostprofile.PaseoOriginBindingRequest(hostprofile.OriginOptions{
		ProjectID:     "proj_paseo",
		DeliveryRunID: "run_paseo",
		CorrelationID: "corr_paseo",
		Getenv:        mapGetenv(map[string]string{"PASEO_HOST": "127.0.0.1:6767"}),
	}); ok {
		t.Fatal("PaseoOriginBindingRequest ok=true without PASEO_AGENT_ID")
	}
}

func mapGetenv(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}

func containsHostProfileString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
