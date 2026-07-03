package config

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDurationSeconds(t *testing.T) {
	if got := DurationSeconds(30, time.Minute); got != 30*time.Second {
		t.Fatalf("DurationSeconds(30) = %s, want 30s", got)
	}
	if got := DurationSeconds(0, time.Minute); got != time.Minute {
		t.Fatalf("DurationSeconds(0) = %s, want default 1m", got)
	}
	if got := DurationSeconds(-5, time.Minute); got != time.Minute {
		t.Fatalf("DurationSeconds(-5) = %s, want default 1m", got)
	}
}

func TestDefaultWatchdogThresholds(t *testing.T) {
	d := Default()
	if d.Resilience.Worker.HardCapSeconds != 1800 || d.Resilience.Worker.StallTimeoutSeconds != 120 {
		t.Fatalf("worker defaults = %d/%d, want 1800/120", d.Resilience.Worker.HardCapSeconds, d.Resilience.Worker.StallTimeoutSeconds)
	}
	if d.Resilience.Verifier.HardCapSeconds != 600 || d.Resilience.Verifier.StallTimeoutSeconds != 120 {
		t.Fatalf("verifier defaults = %d/%d, want 600/120", d.Resilience.Verifier.HardCapSeconds, d.Resilience.Verifier.StallTimeoutSeconds)
	}
}

func TestParseOverridesWatchdogThresholds(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nresilience:\n  worker:\n    hard_cap_seconds: 60\n  verifier:\n    stall_timeout_seconds: 45\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resilience.Worker.HardCapSeconds != 60 {
		t.Fatalf("worker hard_cap = %d, want 60", cfg.Resilience.Worker.HardCapSeconds)
	}
	// Unspecified fields keep their defaults.
	if cfg.Resilience.Worker.StallTimeoutSeconds != 120 {
		t.Fatalf("worker stall = %d, want default 120", cfg.Resilience.Worker.StallTimeoutSeconds)
	}
	if cfg.Resilience.Verifier.StallTimeoutSeconds != 45 {
		t.Fatalf("verifier stall = %d, want 45", cfg.Resilience.Verifier.StallTimeoutSeconds)
	}
	if cfg.Resilience.Verifier.HardCapSeconds != 600 {
		t.Fatalf("verifier hard_cap = %d, want default 600", cfg.Resilience.Verifier.HardCapSeconds)
	}
}

func TestResilienceForRepoDefaultsWhenMissing(t *testing.T) {
	r, err := ResilienceForRepo(context.Background(), t.TempDir(), LoadOptions{
		BaseBranch: "main",
		ShowBaseConfig: func(context.Context, string, string) ([]byte, error) {
			return nil, errors.New("not found")
		},
	})
	if err != nil {
		t.Fatalf("ResilienceForRepo returned error: %v", err)
	}
	if r.Worker.HardCapSeconds != 1800 || r.Verifier.HardCapSeconds != 600 {
		t.Fatalf("missing-config resilience = worker %d / verifier %d, want defaults", r.Worker.HardCapSeconds, r.Verifier.HardCapSeconds)
	}
}

func TestLoadForRepoLoudConfigResolution(t *testing.T) {
	baseConfig := []byte("version: 1\nworker:\n  base_branch: trunk\nresilience:\n  worker:\n    hard_cap_seconds: 42\n")
	tests := []struct {
		name           string
		configFromBase bool
		show           ShowBaseConfigFunc
		wantErr        bool
		wantBaseBranch string
		wantHardCap    int
	}{
		{
			name: "cwd lacks and base has errors loud",
			show: func(context.Context, string, string) ([]byte, error) {
				return baseConfig, nil
			},
			wantErr: true,
		},
		{
			name:           "config-from-base reads base config",
			configFromBase: true,
			show: func(context.Context, string, string) ([]byte, error) {
				return baseConfig, nil
			},
			wantBaseBranch: "trunk",
			wantHardCap:    42,
		},
		{
			name: "no config anywhere uses defaults",
			show: func(context.Context, string, string) ([]byte, error) {
				return nil, errors.New("not found")
			},
			wantHardCap: 1800,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadForRepo(context.Background(), t.TempDir(), LoadOptions{
				BaseBranch:     "main",
				ConfigFromBase: tt.configFromBase,
				ShowBaseConfig: tt.show,
			})
			if tt.wantErr {
				var mismatch ConfigMismatchError
				if !errors.As(err, &mismatch) {
					t.Fatalf("err = %v, want ConfigMismatchError", err)
				}
				if !strings.Contains(err.Error(), "probably the wrong branch") || !strings.Contains(err.Error(), "--config-from-base") {
					t.Fatalf("mismatch error message = %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadForRepo returned error: %v", err)
			}
			if tt.wantBaseBranch != "" && cfg.Worker.BaseBranch != tt.wantBaseBranch {
				t.Fatalf("Worker.BaseBranch = %q, want %q", cfg.Worker.BaseBranch, tt.wantBaseBranch)
			}
			if cfg.Resilience.Worker.HardCapSeconds != tt.wantHardCap {
				t.Fatalf("worker hard cap = %d, want %d", cfg.Resilience.Worker.HardCapSeconds, tt.wantHardCap)
			}
		})
	}
}

func TestResilienceForRepoLoudConfigResolution(t *testing.T) {
	baseConfig := []byte("version: 1\nresilience:\n  verifier:\n    hard_cap_seconds: 44\n")
	tests := []struct {
		name           string
		configFromBase bool
		show           ShowBaseConfigFunc
		wantErr        bool
		wantHardCap    int
	}{
		{
			name: "cwd lacks and base has errors loud",
			show: func(context.Context, string, string) ([]byte, error) {
				return baseConfig, nil
			},
			wantErr: true,
		},
		{
			name:           "config-from-base reads base resilience",
			configFromBase: true,
			show: func(context.Context, string, string) ([]byte, error) {
				return baseConfig, nil
			},
			wantHardCap: 44,
		},
		{
			name: "no config anywhere uses defaults",
			show: func(context.Context, string, string) ([]byte, error) {
				return nil, errors.New("not found")
			},
			wantHardCap: 600,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resilience, err := ResilienceForRepo(context.Background(), t.TempDir(), LoadOptions{
				BaseBranch:     "main",
				ConfigFromBase: tt.configFromBase,
				ShowBaseConfig: tt.show,
			})
			if tt.wantErr {
				var mismatch ConfigMismatchError
				if !errors.As(err, &mismatch) {
					t.Fatalf("err = %v, want ConfigMismatchError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResilienceForRepo returned error: %v", err)
			}
			if resilience.Verifier.HardCapSeconds != tt.wantHardCap {
				t.Fatalf("verifier hard cap = %d, want %d", resilience.Verifier.HardCapSeconds, tt.wantHardCap)
			}
		})
	}
}
