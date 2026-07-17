package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

// ProviderCallRefusedError marks a deliberate pre-launch refusal such as a
// per-run cost cap. It is safe to inspect through wrapping and must not be
// treated as a provider execution failure or retried automatically.
type ProviderCallRefusedError struct {
	Err error
}

func (e ProviderCallRefusedError) Error() string {
	if e.Err == nil {
		return "provider call refused"
	}
	return e.Err.Error()
}

func (e ProviderCallRefusedError) Unwrap() error { return e.Err }

func IsProviderCallRefused(err error) bool {
	var target ProviderCallRefusedError
	return errors.As(err, &target)
}

type Invocation struct {
	WorktreePath string
	Prompt       string
	Model        string
	Effort       string
	ReadOnly     bool
	// BoundedWrite selects a provider mode that may modify only the supplied
	// workspace and must not inherit mutation-capable user configuration.
	BoundedWrite bool
	// DisableDelegation is mandatory for LoopCoder-managed nested roles. It is
	// set by the scheduler boundary and converted to provider-specific hard
	// controls; prompt or environment text cannot unset it.
	DisableDelegation bool
	OutputSchema      string
	LogPath           string
	Stderr            io.Writer
	HardCap           time.Duration
	StallTimeout      time.Duration
	LivenessMode      string
	LivenessCommand   string
	Guardian          supervisedexec.GuardianOptions
	// RunID and Role tag the spawned provider process as loopcoder-managed and
	// place it in a per-run kill-group (spec 0390, Decision 11).
	RunID string
	Role  string
	// ProviderKey is loopcoder's durable idempotency key for the logical child
	// operation. Runners may pass it to providers with native support; providers
	// without native support receive it only as loopcoder metadata.
	ProviderKey      string
	OnProviderLaunch func(pid int)
	OnProviderStart  func(ProviderProcess) error
	// MCPServers carries provider-neutral MCP declarations. Provider-specific
	// flags and config files are still owned by each runner.
	MCPServers []MCPServer
	// Environment contains trusted per-invocation overrides applied after the
	// runner's normal environment isolation.
	Environment map[string]string
}

func environmentWithOverrides(environ []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return append([]string(nil), environ...)
	}
	cleaned := make([]string, 0, len(environ)+len(overrides))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, replaced := overrides[key]; !replaced {
			cleaned = append(cleaned, entry)
		}
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cleaned = append(cleaned, key+"="+overrides[key])
	}
	return cleaned
}

func mcpInvocationRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "nested-read-only":
		return "verifier"
	case "nested-bounded-write":
		return "worker"
	default:
		return role
	}
}

type MCPServer = config.MCPServer

type MCPAuth = config.MCPAuth

type Result struct {
	ExitCode           int
	Summary            string
	Model              string
	Effort             string
	Usage              reporter.Usage
	StartedAt          string
	EndedAt            string
	DurationMS         int64
	Hung               bool
	HungReason         string
	AdapterVersion     string
	ExternalSessionRef string
}

type ProviderProcess struct {
	PID                   int
	PGID                  int
	ProcessBirthIdentity  string
	ExecutableIdentity    string
	ObservedAt            time.Time
	IdentityAmbiguous     bool
	IdentityAmbiguityNote string
}

type Runner interface {
	Run(ctx context.Context, inv Invocation) (Result, error)
}

func validateNestedDelegationBoundary(inv Invocation) error {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(inv.Role)), "nested-") && !inv.DisableDelegation {
		return errors.New("nested provider invocation must disable provider-native delegation")
	}
	return nil
}

var registry = map[string]Runner{
	"codex": ExecCodexRunner{},
}

func Lookup(provider string) (Runner, error) {
	if runner, ok := registry[provider]; ok {
		return runner, nil
	}
	return nil, fmt.Errorf("unknown provider %q (supported providers: %s)", provider, strings.Join(SupportedProviders(), ", "))
}

func SupportedProviders() []string {
	providers := make([]string, 0, len(registry))
	for provider := range registry {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers
}
