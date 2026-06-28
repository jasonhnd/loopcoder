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
			provider: "no-such-provider",
			wantErr:  true,
			errContains: []string{
				`unknown provider "no-such-provider"`,
				"supported providers: claude, codex",
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

func assertInt64Ptr(t *testing.T, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("value = nil, want %d", want)
	}
	if *got != want {
		t.Fatalf("value = %d, want %d", *got, want)
	}
}

func assertNilInt64Ptr(t *testing.T, got *int64) {
	t.Helper()
	if got != nil {
		t.Fatalf("value = %d, want nil", *got)
	}
}
