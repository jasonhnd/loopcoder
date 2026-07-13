package agent

import (
	"os"
	"runtime"
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
				"supported providers: antigravity, claude, codex, gemini, grok",
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

func assertArgsDoNotContain(t *testing.T, args []string, forbidden ...string) {
	t.Helper()
	for _, arg := range args {
		lowerArg := strings.ToLower(arg)
		for _, value := range forbidden {
			if strings.Contains(lowerArg, strings.ToLower(value)) {
				t.Fatalf("args %#v contain forbidden value %q in arg %q", args, value, arg)
			}
		}
	}
}

func assertPrivateFileMode(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.IsDir() {
		t.Fatalf("%s is a directory, want file", path)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %#o, want %#o", path, got, os.FileMode(0o600))
	}
}
