package preflight_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/preflight"
)

func t0() time.Time { return time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC) }

func fixtureDeps(t *testing.T) preflight.Deps {
	t.Helper()
	home := t.TempDir()
	return preflight.Deps{
		Now:    t0,
		GOOS:   "fixture",
		GOARCH: "arm64",
		LookPath: func(file string) (string, error) {
			if file == "git" || file == "true" || file == "codex" {
				return "/bin/" + file, nil
			}
			return "", errors.New("not found")
		},
		Stat: func(name string) (os.FileInfo, error) {
			return os.Stat(name)
		},
		UserHomeDir:  func() (string, error) { return home, nil },
		Getenv:       func(string) string { return "" },
		MkdirAll:     os.MkdirAll,
		BudgetFreeMB: func() (int64, error) { return 512, nil },
		ProviderPresent: func(provider string) (bool, string, error) {
			if provider == "codex" || provider == "fixture" {
				return true, provider, nil
			}
			return false, provider, nil
		},
		UICapable:  func() (bool, string) { return true, "ok" },
		QuotaKnown: func() (bool, string) { return false, "unknown" },
	}
}

func TestHealthyAllowsLaunch(t *testing.T) {
	deps := fixtureDeps(t)
	repo := t.TempDir()
	snap, err := preflight.Evaluate(context.Background(), preflight.Input{
		Repo: repo, Provider: "codex", Model: "m",
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.AllowLaunch || snap.Decision == preflight.StatusFail {
		t.Fatalf("%+v", snap)
	}
	if snap.Digest == "" || snap.Schema != preflight.SchemaSnapshot {
		t.Fatalf("%+v", snap)
	}
	// quota optional warn ok
	foundWarn := false
	for _, p := range snap.Probes {
		if p.Name == "quota_catalog" && p.Status == preflight.StatusWarn {
			foundWarn = true
		}
		if p.Kind == preflight.KindPrerequisite && p.Status == preflight.StatusFail {
			t.Fatalf("prereq fail: %+v", p)
		}
	}
	if !foundWarn {
		t.Fatal("expected optional quota warn")
	}
}

func TestFailuresBeforeLaunch(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*preflight.Deps, *preflight.Input)
		rem  string
	}{
		{"platform", func(d *preflight.Deps, in *preflight.Input) { d.GOOS = "windows" }, preflight.RemediationUnsupportedPlatform},
		{"home", func(d *preflight.Deps, in *preflight.Input) {
			d.UserHomeDir = func() (string, error) { return "relative", nil }
		}, preflight.RemediationUnsafeHome},
		{"repo", func(d *preflight.Deps, in *preflight.Input) { in.Repo = "" }, preflight.RemediationInvalidRepo},
		{"git", func(d *preflight.Deps, in *preflight.Input) {
			d.LookPath = func(string) (string, error) { return "", errors.New("no") }
		}, preflight.RemediationGitUnavailable},
		{"provider", func(d *preflight.Deps, in *preflight.Input) {
			d.ProviderPresent = func(string) (bool, string, error) { return false, "codex", nil }
		}, preflight.RemediationProviderMissing},
		{"budget", func(d *preflight.Deps, in *preflight.Input) {
			d.BudgetFreeMB = func() (int64, error) { return 1, nil }
			in.MinBudgetMB = 64
		}, preflight.RemediationBudgetInsufficient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := fixtureDeps(t)
			in := preflight.Input{Repo: t.TempDir(), Provider: "codex", Model: "m"}
			tc.mut(&deps, &in)
			snap, err := preflight.Evaluate(context.Background(), in, deps)
			if err != nil {
				t.Fatal(err)
			}
			if snap.AllowLaunch {
				t.Fatalf("should block: %+v", snap)
			}
			found := false
			for _, p := range snap.Probes {
				if p.Remediation == tc.rem && p.Status == preflight.StatusFail {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing rem %s in %+v", tc.rem, snap.Probes)
			}
			_, err = preflight.RequireLaunch(context.Background(), in, deps)
			var ge *preflight.GateError
			if !errors.As(err, &ge) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestOptionalUINotAuthFailure(t *testing.T) {
	deps := fixtureDeps(t)
	deps.UICapable = func() (bool, string) { return false, "no_ui" }
	snap, err := preflight.Evaluate(context.Background(), preflight.Input{
		Repo: t.TempDir(), Provider: "codex", Model: "m",
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.AllowLaunch {
		t.Fatal("ui optional must not block")
	}
	for _, p := range snap.Probes {
		if p.Name == "ui_capability" {
			if p.Status != preflight.StatusWarn || p.Remediation != preflight.RemediationUIOptional {
				t.Fatalf("%+v", p)
			}
			if p.Kind != preflight.KindOptional {
				t.Fatal("must be optional")
			}
		}
		if p.Name == "explicit_provider" && p.Status != preflight.StatusPass {
			t.Fatal("must not confuse with provider auth")
		}
	}
}

func TestEnsureLayoutIdempotentAndReportsChanged(t *testing.T) {
	deps := fixtureDeps(t)
	home, _ := deps.UserHomeDir()
	in := preflight.Input{Repo: "owner/repo", Provider: "codex", Model: "m", EnsureLayout: true}
	s1, err := preflight.Evaluate(context.Background(), in, deps)
	if err != nil || !s1.AllowLaunch {
		t.Fatalf("%+v err=%v", s1, err)
	}
	if len(s1.Changed) == 0 {
		t.Fatal("expected created dirs first time")
	}
	// second run no new creates
	s2, err := preflight.Evaluate(context.Background(), in, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Changed) != 0 {
		t.Fatalf("second ensure should be empty: %v", s2.Changed)
	}
	// layout exists under home
	if _, err := os.Stat(filepath.Join(home, ".loopcoder", "data")); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatSameDecision(t *testing.T) {
	deps := fixtureDeps(t)
	in := preflight.Input{Repo: "a/b", Provider: "fixture", Model: "m"}
	a, err := preflight.Evaluate(context.Background(), in, deps)
	if err != nil {
		t.Fatal(err)
	}
	b, err := preflight.Evaluate(context.Background(), in, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !preflight.SameDecision(a, b) {
		t.Fatalf("decisions differ a=%s b=%s", a.Decision, b.Decision)
	}
}

func TestNoSecretsInEvidence(t *testing.T) {
	deps := fixtureDeps(t)
	deps.ProviderPresent = func(string) (bool, string, error) {
		return false, "", errors.New("auth failed bearer ghp_SECRETKEY")
	}
	snap, _ := preflight.Evaluate(context.Background(), preflight.Input{
		Repo: "a/b", Provider: "codex", Model: "m",
	}, deps)
	for _, p := range snap.Probes {
		for k, v := range p.Evidence {
			if v == "ghp_SECRETKEY" || containsSecret(v) {
				t.Fatalf("secret leaked %s=%s", k, v)
			}
		}
	}
}

func containsSecret(s string) bool {
	return len(s) > 0 && (s == "ghp_SECRETKEY" || s == "bearer ghp_SECRETKEY")
}
