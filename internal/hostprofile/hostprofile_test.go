package hostprofile_test

import (
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/hostprofile"
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

func mapGetenv(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}
