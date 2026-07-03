package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
)

type Invocation struct {
	WorktreePath string
	Prompt       string
	Model        string
	Effort       string
	ReadOnly     bool
	OutputSchema string
	LogPath      string
	HardCap      time.Duration
	StallTimeout time.Duration
}

type Result struct {
	ExitCode   int
	Summary    string
	Model      string
	Effort     string
	Usage      attestation.Usage
	StartedAt  string
	EndedAt    string
	DurationMS int64
	Hung       bool
	HungReason string
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
