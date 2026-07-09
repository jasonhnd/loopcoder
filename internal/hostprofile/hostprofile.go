// Package hostprofile resolves the agent host that invoked loopcoder.
package hostprofile

import (
	"fmt"
	"strings"
)

const EnvHost = "LOOPCODER_HOST"

type Profile struct {
	Name             string `json:"name"`
	Source           string `json:"source"`
	CWDPolicy        string `json:"cwd_policy"`
	RepoPathPolicy   string `json:"repo_path_policy"`
	EnvPolicy        string `json:"env_policy"`
	OutputPolicy     string `json:"output_policy"`
	NestedSubagents  bool   `json:"nested_subagents"`
	PrivateAPI       bool   `json:"private_api"`
	Hooks            string `json:"hooks"`
	MachineReadable  bool   `json:"machine_readable"`
	HumanDescription string `json:"human_description"`
}

type EnvFunc func(string) string

func Resolve(configProfile string, getenv EnvFunc) (Profile, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if value := strings.TrimSpace(getenv(EnvHost)); value != "" {
		return profileFromExplicit(value, "env:"+EnvHost)
	}
	if value := strings.TrimSpace(configProfile); value != "" && !strings.EqualFold(value, "auto") {
		return profileFromExplicit(value, "config:host.profile")
	}
	if detected, source := detect(getenv); detected != "" {
		return profile(detected, source), nil
	}
	return profile("generic", "default"), nil
}

func ValidateName(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.EqualFold(trimmed, "auto") {
		return nil
	}
	if _, ok := canonicalName(trimmed); ok {
		return nil
	}
	return fmt.Errorf("host.profile %q is not one of auto, generic, codex, claude, claudecode, claude-code, paseo", value)
}

func profileFromExplicit(value, source string) (Profile, error) {
	name, ok := canonicalName(value)
	if !ok {
		return Profile{}, fmt.Errorf("%s %q is not one of generic, codex, claude, claudecode, claude-code, paseo", source, value)
	}
	return profile(name, source), nil
}

func canonicalName(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "generic", "unknown":
		return "generic", true
	case "codex", "codex-cli":
		return "codex", true
	case "claude", "claudecode", "claude-code", "claude_code":
		return "claude", true
	case "paseo":
		return "paseo", true
	default:
		return "", false
	}
}

func detect(getenv EnvFunc) (string, string) {
	for _, candidate := range []struct {
		name string
		keys []string
	}{
		{name: "paseo", keys: []string{"PASEO_AGENT_ID", "PASEO_HOST", "PASEO_WORKTREE"}},
		{name: "codex", keys: []string{"CODEX_THREAD_ID", "CODEX_CLI", "CODEX_SANDBOX"}},
		{name: "claude", keys: []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT"}},
	} {
		for _, key := range candidate.keys {
			if strings.TrimSpace(getenv(key)) != "" {
				return candidate.name, "detected:" + key
			}
		}
	}
	return "", ""
}

func profile(name, source string) Profile {
	p := Profile{
		Name:            name,
		Source:          source,
		CWDPolicy:       "resolve cwd before command execution",
		RepoPathPolicy:  "normalize --repo to an absolute directory path",
		EnvPolicy:       "preserve caller environment; LOOPCODER_* variables are loopcoder-owned",
		OutputPolicy:    "human output on stdout, diagnostics on stderr, JSON formats emit only JSON on stdout",
		NestedSubagents: true,
		PrivateAPI:      false,
		Hooks:           "none",
		MachineReadable: true,
	}
	switch name {
	case "codex":
		p.Hooks = "best-effort"
		p.HumanDescription = "Codex-style host"
	case "claude":
		p.Hooks = "claude-code settings"
		p.HumanDescription = "Claude Code-style host"
	case "paseo":
		p.Hooks = "host-managed"
		p.HumanDescription = "Paseo-style host"
	default:
		p.NestedSubagents = false
		p.Hooks = "unknown"
		p.HumanDescription = "generic host"
	}
	return p
}
