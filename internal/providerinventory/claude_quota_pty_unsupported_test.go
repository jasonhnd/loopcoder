//go:build !darwin || !arm64

package providerinventory

import (
	"context"
	"errors"
	"testing"
)

func TestRunClaudeUsagePTYUnsupportedPlatform(t *testing.T) {
	result, err := runClaudeUsagePTY(context.Background(), ClaudePTYRequest{
		Argv: []string{"claude"},
	})
	if !errors.Is(err, ErrClaudeQuotaPTYUnsupported) {
		t.Fatalf("err = %v, want ErrClaudeQuotaPTYUnsupported", err)
	}
	if result.ExitCode != -1 || result.Output != "" {
		t.Fatalf("result = %#v, want typed unavailable result", result)
	}
}
