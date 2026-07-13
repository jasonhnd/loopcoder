//go:build !darwin || !arm64

package providerinventory

import (
	"context"
	"strings"
)

func runClaudeUsagePTY(ctx context.Context, req ClaudePTYRequest) (ClaudePTYResult, error) {
	_ = ctx
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return ClaudePTYResult{ExitCode: -1}, ErrClaudeQuotaPTYUnsupported
	}
	return ClaudePTYResult{
		ExitCode: -1,
		Stderr:   "claude usage PTY collection is unavailable on this platform",
	}, ErrClaudeQuotaPTYUnsupported
}
