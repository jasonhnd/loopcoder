package hostprofile

import "testing"

func TestResolveExplicitEnvOverridesConfig(t *testing.T) {
	profile, err := Resolve("claude", mapEnv(map[string]string{EnvHost: "codex"}))
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if profile.Name != "codex" || profile.Source != "env:"+EnvHost {
		t.Fatalf("profile = %#v, want codex from env", profile)
	}
}

func TestResolveConfigAlias(t *testing.T) {
	profile, err := Resolve("claudecode", nil)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if profile.Name != "claude" || profile.Source != "config:host.profile" {
		t.Fatalf("profile = %#v, want claude from config", profile)
	}
}

func TestResolveDetectsKnownHosts(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "paseo", env: map[string]string{"PASEO_AGENT_ID": "agent-1", "CODEX_THREAD_ID": "thread-1"}, want: "paseo"},
		{name: "codex", env: map[string]string{"CODEX_THREAD_ID": "thread-1", "CLAUDECODE": "1"}, want: "codex"},
		{name: "claude", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "session-1"}, want: "claude"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile, err := Resolve("auto", mapEnv(tt.env))
			if err != nil {
				t.Fatalf("Resolve returned error: %v", err)
			}
			if profile.Name != tt.want || profile.Source == "" {
				t.Fatalf("profile = %#v, want %s with source", profile, tt.want)
			}
		})
	}
}

func TestResolveRejectsUnknownExplicitProfile(t *testing.T) {
	if _, err := Resolve("spaceship", nil); err == nil {
		t.Fatal("Resolve returned nil error for unknown config profile")
	}
	if err := ValidateName("spaceship"); err == nil {
		t.Fatal("ValidateName returned nil error for unknown profile")
	}
}

func mapEnv(values map[string]string) EnvFunc {
	return func(key string) string {
		return values[key]
	}
}
