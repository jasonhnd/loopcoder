package agent

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

type Invocation struct {
	WorktreePath    string
	Prompt          string
	Model           string
	Effort          string
	ReadOnly        bool
	OutputSchema    string
	LogPath         string
	Stderr          io.Writer
	HardCap         time.Duration
	StallTimeout    time.Duration
	LivenessMode    string
	LivenessCommand string
	Guardian        supervisedexec.GuardianOptions
	// RunID and Role tag the spawned provider process as loopcoder-managed and
	// place it in a per-run kill-group (spec 0390, Decision 11).
	RunID string
	Role  string
	// ProviderKey is loopcoder's durable idempotency key for the logical child
	// operation. Runners may pass it to providers with native support; providers
	// without native support receive it only as loopcoder metadata.
	ProviderKey     string
	OnProviderStart func(ProviderProcess) error
	// MCPServers carries provider-neutral MCP declarations. Provider-specific
	// flags and config files are still owned by each runner.
	MCPServers []MCPServer
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
