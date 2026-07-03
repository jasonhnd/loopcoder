package config

import (
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
	r := ResilienceForRepo(t.TempDir())
	if r.Worker.HardCapSeconds != 1800 || r.Verifier.HardCapSeconds != 600 {
		t.Fatalf("missing-config resilience = worker %d / verifier %d, want defaults", r.Worker.HardCapSeconds, r.Verifier.HardCapSeconds)
	}
}
