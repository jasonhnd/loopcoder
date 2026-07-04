package config

import (
	"context"
	"errors"
	"io"
	"os"
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
	if d.Resilience.Worker.HardCapSeconds != 2700 || d.Resilience.Worker.StallTimeoutSeconds != 300 {
		t.Fatalf("worker defaults = %d/%d, want 2700/300", d.Resilience.Worker.HardCapSeconds, d.Resilience.Worker.StallTimeoutSeconds)
	}
	if d.Resilience.Verifier.HardCapSeconds != 900 || d.Resilience.Verifier.StallTimeoutSeconds != 300 {
		t.Fatalf("verifier defaults = %d/%d, want 900/300", d.Resilience.Verifier.HardCapSeconds, d.Resilience.Verifier.StallTimeoutSeconds)
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
	if cfg.Resilience.Worker.StallTimeoutSeconds != 300 {
		t.Fatalf("worker stall = %d, want default 300", cfg.Resilience.Worker.StallTimeoutSeconds)
	}
	if cfg.Resilience.Verifier.StallTimeoutSeconds != 45 {
		t.Fatalf("verifier stall = %d, want 45", cfg.Resilience.Verifier.StallTimeoutSeconds)
	}
	if cfg.Resilience.Verifier.HardCapSeconds != 900 {
		t.Fatalf("verifier hard_cap = %d, want default 900", cfg.Resilience.Verifier.HardCapSeconds)
	}
}

func TestResilienceForRepoDefaultsWhenMissing(t *testing.T) {
	r, err := ResilienceForRepo(context.Background(), t.TempDir(), LoadOptions{
		BaseBranch: "main",
		ShowBaseConfig: func(context.Context, string, string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
	})
	if err != nil {
		t.Fatalf("ResilienceForRepo returned error: %v", err)
	}
	if r.Worker.HardCapSeconds != 2700 || r.Verifier.HardCapSeconds != 900 {
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
				return nil, os.ErrNotExist
			},
			wantHardCap: 2700,
		},
		{
			name: "base check real failure warns and defaults",
			show: func(context.Context, string, string) ([]byte, error) {
				return nil, errors.New("git show failed")
			},
			wantHardCap: 2700,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var warnings strings.Builder
			cfg, err := LoadForRepo(context.Background(), t.TempDir(), LoadOptions{
				BaseBranch:     "main",
				ConfigFromBase: tt.configFromBase,
				ShowBaseConfig: tt.show,
				Warnings:       &warnings,
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
			if tt.name == "base check real failure warns and defaults" {
				if !strings.Contains(warnings.String(), "base .delivery.yml consistency check could not run") || !strings.Contains(warnings.String(), "using defaults") {
					t.Fatalf("warning missing real git failure details:\n%s", warnings.String())
				}
			} else if warnings.Len() != 0 {
				t.Fatalf("warnings = %q, want none", warnings.String())
			}
		})
	}
}

func TestLoadForRepoBaseConfigAbsentDiscrimination(t *testing.T) {
	tests := []struct {
		name     string
		showErr  error
		wantWarn bool
	}{
		{
			name:    "path absent from valid base is silent",
			showErr: errors.New("git show main:.delivery.yml: exit status 128: fatal: Path '.delivery.yml' does not exist in 'main'"),
		},
		{
			name:    "path exists on disk but absent from valid base is silent",
			showErr: errors.New("git show main:.delivery.yml: exit status 128: fatal: .delivery.yml exists on disk, but not in 'main'"),
		},
		{
			name:     "bad base pathspec warns and defaults",
			showErr:  errors.New("git show main:.delivery.yml: exit status 128: error: pathspec 'main' did not match any file(s) known to git"),
			wantWarn: true,
		},
		{
			name:     "invalid object name warns and defaults",
			showErr:  errors.New("git show main:.delivery.yml: exit status 128: fatal: invalid object name 'main'"),
			wantWarn: true,
		},
		{
			name:     "ambiguous argument warns and defaults",
			showErr:  errors.New("git show main:.delivery.yml: exit status 128: fatal: ambiguous argument 'main:.delivery.yml': unknown revision or path not in the working tree"),
			wantWarn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var warnings strings.Builder
			cfg, err := LoadForRepo(context.Background(), t.TempDir(), LoadOptions{
				BaseBranch: "main",
				ShowBaseConfig: func(context.Context, string, string) ([]byte, error) {
					return nil, tt.showErr
				},
				Warnings: &warnings,
			})
			if err != nil {
				t.Fatalf("LoadForRepo returned error: %v", err)
			}
			if cfg.Resilience.Worker.HardCapSeconds != 2700 {
				t.Fatalf("worker hard cap = %d, want default 2700", cfg.Resilience.Worker.HardCapSeconds)
			}
			if tt.wantWarn {
				for _, want := range []string{"warning", "base .delivery.yml consistency check could not run", "using defaults"} {
					if !strings.Contains(warnings.String(), want) {
						t.Fatalf("warning missing %q:\n%s", want, warnings.String())
					}
				}
				return
			}
			if warnings.Len() != 0 {
				t.Fatalf("warnings = %q, want none", warnings.String())
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
				return nil, os.ErrNotExist
			},
			wantHardCap: 900,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resilience, err := ResilienceForRepo(context.Background(), t.TempDir(), LoadOptions{
				BaseBranch:     "main",
				ConfigFromBase: tt.configFromBase,
				ShowBaseConfig: tt.show,
				Warnings:       io.Discard,
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
