package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInjectGrokCLIAuthAsAPIKeyFromAccountKeyedAuth(t *testing.T) {
	home := t.TempDir()
	authDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"https://auth.x.ai::acct":{"key":"test-token-xyz-not-for-logs"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", "")
	// Clear any ambient API key.
	t.Setenv("XAI_API_KEY", "")
	out := injectGrokCLIAuthAsAPIKey([]string{"PATH=/bin", "CI=1"})
	found := false
	for _, e := range out {
		if strings.HasPrefix(e, "XAI_API_KEY=") {
			found = true
			if e != "XAI_API_KEY=test-token-xyz-not-for-logs" {
				t.Fatalf("unexpected injection %q", e)
			}
		}
	}
	if !found {
		t.Fatal("expected XAI_API_KEY injection")
	}
	// Does not override existing key.
	out2 := injectGrokCLIAuthAsAPIKey([]string{"XAI_API_KEY=already-set"})
	if len(out2) != 1 || out2[0] != "XAI_API_KEY=already-set" {
		t.Fatalf("override existing: %#v", out2)
	}
}
