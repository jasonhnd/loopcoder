package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/routing"
)

func TestLoopreviewUnpinnedUsesIndependentRouteDecision(t *testing.T) {
	repo := t.TempDir()
	writeVerifierDeliveryYML(t, repo, "codex", "claude")
	var gotProvider, gotModel string
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "99",
	}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
		ResolveVerifierDispatchRoute: func(_ context.Context, input VerifierDispatchRouteInput) (VerifierDispatchRouteResult, error) {
			if input.WorkerProvider != "codex" {
				t.Fatalf("worker provider = %q, want codex from config", input.WorkerProvider)
			}
			if strings.TrimSpace(input.ExplicitProvider) != "" {
				t.Fatalf("expected unpinned verifier, got pin %q", input.ExplicitProvider)
			}
			return VerifierDispatchRouteResult{
				Provider:          "claude",
				Model:             "claude-opus-4-8[1m]",
				Effort:            "max",
				RoutingDecisionID: "rd-verifier-99",
				Outcome:           routing.RouteOutcomeSelected,
			}, nil
		},
		Loopreview: func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
			gotProvider = opts.Provider
			gotModel = opts.Model
			return loopreview.Result{
				Verdict: loopreview.Verdict{Verdict: "pass"},
			}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr.String())
	}
	if gotProvider != "claude" || gotModel != "claude-opus-4-8[1m]" {
		t.Fatalf("selection = %s/%s", gotProvider, gotModel)
	}
}

func TestLoopreviewNoIndependentVerifierReturnsNeedsHuman(t *testing.T) {
	repo := t.TempDir()
	writeVerifierDeliveryYML(t, repo, "codex", "codex")
	var calls int
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "7",
	}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
		ResolveVerifierDispatchRoute: func(context.Context, VerifierDispatchRouteInput) (VerifierDispatchRouteResult, error) {
			return VerifierDispatchRouteResult{
				Outcome:           routing.RouteOutcomeNoRoute,
				RoutingDecisionID: "rd-no-independent",
			}, fmt.Errorf("no_route: no independent read-only verifier")
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			calls++
			return loopreview.Result{}, fmt.Errorf("must not launch")
		},
	})
	if exit != 2 {
		t.Fatalf("exit = %d, want 2 needs-human; stderr=%s", exit, stderr.String())
	}
	if calls != 0 {
		t.Fatalf("loopreview calls = %d, want 0", calls)
	}
	if !strings.Contains(stderr.String(), "needs-human") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestLoopreviewRejectsSameProviderAsWorker(t *testing.T) {
	repo := t.TempDir()
	writeVerifierDeliveryYML(t, repo, "codex", "claude")
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "3",
	}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
		ResolveVerifierDispatchRoute: func(context.Context, VerifierDispatchRouteInput) (VerifierDispatchRouteResult, error) {
			return VerifierDispatchRouteResult{
				Provider:          "codex",
				Model:             "gpt-5.5",
				Effort:            "high",
				RoutingDecisionID: "rd-same",
				Outcome:           routing.RouteOutcomeSelected,
			}, nil
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			t.Fatal("must not launch non-independent verifier")
			return loopreview.Result{}, nil
		},
	})
	if exit != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not independent") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestLoopreviewPartialDepsRequiresExplicitProvider(t *testing.T) {
	repo := t.TempDir()
	writeVerifierDeliveryYML(t, repo, "codex", "claude")
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"loopreview",
		"--repo", repo,
		"--pr-number", "1",
	}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			t.Fatal("must not run")
			return loopreview.Result{}, nil
		},
	})
	if exit != 3 {
		t.Fatalf("exit = %d, want 3; stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unpinned verifier requires a route decision") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func writeVerifierDeliveryYML(t *testing.T, repo, worker, verifier string) {
	t.Helper()
	body := fmt.Sprintf("version: 1\nadapters:\n  worker: %s\n  verifier: %s\n", worker, verifier)
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write delivery yml: %v", err)
	}
}
