// Package hostprofile resolves the agent host profile that invokes loopcoder.
package hostprofile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/runtimecap"
)

const EnvName = "LOOPCODER_HOST"

type Source string

const (
	SourceEnv       Source = "env"
	SourceConfig    Source = "config"
	SourceDetection Source = "detection"
	SourceFallback  Source = "fallback"
)

type Options struct {
	Profile  string
	Getenv   func(string) string
	Contract runtimecap.Contract
}

type Resolved struct {
	Name       string
	Source     Source
	Selector   string
	DetectedBy []string
	Runtime    runtimecap.HostRuntime
}

func Resolve(opts Options) (Resolved, error) {
	contract := opts.Contract
	if len(contract.Hosts) == 0 {
		contract = runtimecap.DefaultContract()
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	if raw := strings.TrimSpace(getenv(EnvName)); raw != "" {
		return resolveExplicit(contract, raw, SourceEnv, EnvName)
	}
	if raw := strings.TrimSpace(opts.Profile); raw != "" {
		return resolveExplicit(contract, raw, SourceConfig, "host.profile")
	}
	if detected := detect(contract, getenv); detected.Name != "" {
		return detected, nil
	}
	return resolveExplicit(contract, "generic-local", SourceFallback, "fallback")
}

func NormalizeName(name string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	key = strings.ReplaceAll(key, "_", "-")
	key = strings.ReplaceAll(key, " ", "-")
	key = strings.TrimSuffix(key, "-style")
	switch key {
	case "codex", "codex-cli", "codexcli":
		return "codex-cli", true
	case "claude", "claude-code", "claudecode", "claude-cli":
		return "claude-code", true
	case "paseo":
		return "paseo-style", true
	case "generic", "generic-local", "local":
		return "generic-local", true
	default:
		return "", false
	}
}

func KnownNames() []string {
	return runtimecap.HostNames()
}

func resolveExplicit(contract runtimecap.Contract, raw string, source Source, selector string) (Resolved, error) {
	name, ok := NormalizeName(raw)
	if !ok {
		return Resolved{}, unknownProfileError(raw, selector, contract)
	}
	host, ok := contract.LookupHost(name)
	if !ok {
		return Resolved{}, unknownProfileError(raw, selector, contract)
	}
	return Resolved{
		Name:     host.Name,
		Source:   source,
		Selector: selector,
		Runtime:  host,
	}, nil
}

func detect(contract runtimecap.Contract, getenv func(string) string) Resolved {
	candidates := []struct {
		name string
		vars []string
	}{
		{name: "paseo-style", vars: []string{"PASEO_AGENT_ID", "PASEO_HOST"}},
		{name: "claude-code", vars: []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT"}},
		{name: "codex-cli", vars: []string{"CODEX_CLI", "CODEX_THREAD_ID"}},
	}
	for _, candidate := range candidates {
		var matched []string
		for _, envName := range candidate.vars {
			if strings.TrimSpace(getenv(envName)) != "" {
				matched = append(matched, envName)
			}
		}
		if len(matched) == 0 {
			continue
		}
		host, ok := contract.LookupHost(candidate.name)
		if !ok {
			continue
		}
		return Resolved{
			Name:       host.Name,
			Source:     SourceDetection,
			Selector:   strings.Join(matched, ","),
			DetectedBy: matched,
			Runtime:    host,
		}
	}
	return Resolved{}
}

func unknownProfileError(raw, selector string, contract runtimecap.Contract) error {
	names := contract.HostNames()
	if len(names) == 0 {
		names = KnownNames()
	}
	sort.Strings(names)
	return fmt.Errorf("unknown host profile %q from %s (known profiles: %s)", raw, selector, strings.Join(names, ", "))
}
