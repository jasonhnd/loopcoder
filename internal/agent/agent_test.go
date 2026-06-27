package agent

import (
	"strings"
	"testing"
)

func TestLookup(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		wantErr     bool
		errContains []string
	}{
		{
			name:     "codex resolves",
			provider: "codex",
		},
		{
			name:     "unknown lists supported providers",
			provider: "claude",
			wantErr:  true,
			errContains: []string{
				`unknown provider "claude"`,
				"supported providers: codex",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner, err := Lookup(tt.provider)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Lookup returned nil error, want failure")
				}
				for _, want := range tt.errContains {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("Lookup error = %q, want substring %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Lookup returned error: %v", err)
			}
			if runner == nil {
				t.Fatal("Lookup returned nil runner")
			}
		})
	}
}
