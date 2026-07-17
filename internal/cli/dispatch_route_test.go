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

	"github.com/jasonhnd/loopcoder/internal/routing"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func TestDispatchUnpinnedUsesRouteDecisionNotCodexDefault(t *testing.T) {
	repo := t.TempDir()
	writeMinimalDeliveryYML(t, repo)
	var got worker.Options
	var dispatchCalls int
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "1012",
		"--issue-title", "Route unpinned worker",
		"--foreground",
	}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
		ResolveWorkerDispatchRoute: func(_ context.Context, input WorkerDispatchRouteInput) (WorkerDispatchRouteResult, error) {
			if strings.TrimSpace(input.ExplicitProvider) != "" {
				t.Fatalf("expected unpinned route request, got pin %q", input.ExplicitProvider)
			}
			return WorkerDispatchRouteResult{
				Provider:          "grok",
				Model:             "grok-4",
				Effort:            "high",
				RoutingDecisionID: "rd-unpinned-1012",
				DecisionKey:       "worker:issue-1012:attempt-1",
				Outcome:           routing.RouteOutcomeSelected,
				DeliveryRunID:     "run-1012",
			}, nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			dispatchCalls++
			got = opts
			record := validDispatchReport()
			record.Provider = opts.Provider
			record.Model = opts.Model
			record.Effort = opts.Effort
			return worker.Result{
				OK: true, Issue: opts.IssueNumber, RunID: opts.RunID, Status: "succeeded",
				Report: &record,
			}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr.String())
	}
	if dispatchCalls != 1 {
		t.Fatalf("dispatch calls = %d, want 1", dispatchCalls)
	}
	if got.Provider != "grok" || got.Model != "grok-4" || got.Effort != "high" {
		t.Fatalf("worker selection = %s/%s/%s, want grok/grok-4/high", got.Provider, got.Model, got.Effort)
	}
	if got.RoutingDecisionID != "rd-unpinned-1012" {
		t.Fatalf("RoutingDecisionID = %q, want rd-unpinned-1012", got.RoutingDecisionID)
	}
}

func TestDispatchNoRouteLaunchesZeroProviders(t *testing.T) {
	repo := t.TempDir()
	writeMinimalDeliveryYML(t, repo)
	var dispatchCalls int
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "42",
		"--issue-title", "No route",
		"--foreground",
	}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
		ResolveWorkerDispatchRoute: func(context.Context, WorkerDispatchRouteInput) (WorkerDispatchRouteResult, error) {
			return WorkerDispatchRouteResult{
				Outcome:              routing.RouteOutcomeNoRoute,
				RoutingDecisionID:    "rd-no-route",
				DecisionKey:          "worker:issue-42:attempt-1",
				ZeroProviderLaunches: true,
			}, fmt.Errorf("no_route: no eligible worker route")
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			dispatchCalls++
			return worker.Result{}, fmt.Errorf("Dispatch must not be called")
		},
	})
	if exit != 20 {
		t.Fatalf("exit = %d, want 20; stderr=%s", exit, stderr.String())
	}
	if dispatchCalls != 0 {
		t.Fatalf("dispatch calls = %d, want 0", dispatchCalls)
	}
	if !strings.Contains(stderr.String(), "no_route") {
		t.Fatalf("stderr = %q, want no_route", stderr.String())
	}
}

func TestDispatchExplicitPinGoesThroughRouteResolver(t *testing.T) {
	repo := t.TempDir()
	writeMinimalDeliveryYML(t, repo)
	var seenPin string
	var got worker.Options
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "7",
		"--issue-title", "Pinned claude",
		"--provider", "claude",
		"--model", "claude-opus-4-8[1m]",
		"--effort", "max",
		"--foreground",
	}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
		ResolveWorkerDispatchRoute: func(_ context.Context, input WorkerDispatchRouteInput) (WorkerDispatchRouteResult, error) {
			seenPin = input.ExplicitProvider
			if input.ExplicitModel != "claude-opus-4-8[1m]" || input.ExplicitEffort != "max" {
				t.Fatalf("pin model/effort = %s/%s", input.ExplicitModel, input.ExplicitEffort)
			}
			return WorkerDispatchRouteResult{
				Provider:          "claude",
				Model:             "claude-opus-4-8[1m]",
				Effort:            "max",
				RoutingDecisionID: "rd-pin-claude",
				Outcome:           routing.RouteOutcomeSelected,
			}, nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			got = opts
			record := validDispatchReport()
			record.Provider = opts.Provider
			record.Model = opts.Model
			record.Effort = opts.Effort
			return worker.Result{
				OK: true, Issue: opts.IssueNumber, RunID: opts.RunID, Status: "succeeded",
				Report: &record,
			}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr.String())
	}
	if seenPin != "claude" {
		t.Fatalf("pin provider = %q, want claude", seenPin)
	}
	if got.Provider != "claude" || got.Model != "claude-opus-4-8[1m]" || got.Effort != "max" {
		t.Fatalf("selection = provider=%s model=%s effort=%s", got.Provider, got.Model, got.Effort)
	}
	if got.RoutingDecisionID != "rd-pin-claude" {
		t.Fatalf("RoutingDecisionID = %q", got.RoutingDecisionID)
	}
}

func TestDispatchPartialDepsWithoutRouteRequiresExplicitProvider(t *testing.T) {
	repo := t.TempDir()
	writeMinimalDeliveryYML(t, repo)
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"dispatch",
		"--repo", repo,
		"--issue-number", "9",
		"--issue-title", "No default codex",
		"--foreground",
	}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) },
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			t.Fatal("Dispatch must not run without provider or route")
			return worker.Result{}, nil
		},
	})
	if exit != 2 {
		t.Fatalf("exit = %d, want 2; stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unpinned worker requires a route decision") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestMapInvocationProfileToEffort(t *testing.T) {
	if got := mapInvocationProfileToEffort("deep", ""); got != "xhigh" {
		t.Fatalf("deep -> %q", got)
	}
	if got := mapInvocationProfileToEffort("fast", ""); got != "low" {
		t.Fatalf("fast -> %q", got)
	}
	if got := mapInvocationProfileToEffort("default", "medium"); got != "medium" {
		t.Fatalf("explicit wins: %q", got)
	}
}

func TestSelectedCandidateExport(t *testing.T) {
	decision := routing.RoutingDecision{
		ChosenCandidateID: "cand-1",
		EligibleCandidates: []routing.Candidate{{
			RoutingCandidateID:   "cand-1",
			AdapterID:            "grok",
			CanonicalModelID:     "grok-4",
			InvocationProfileKey: "default",
		}},
	}
	got := routing.SelectedCandidate(decision)
	if got.AdapterID != "grok" || got.CanonicalModelID != "grok-4" {
		t.Fatalf("SelectedCandidate = %#v", got)
	}
}

func writeMinimalDeliveryYML(t *testing.T, repo string) {
	t.Helper()
	body := "version: 1\nadapters:\n  worker: codex\n  verifier: claude\n"
	if err := os.WriteFile(filepath.Join(repo, ".delivery.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write .delivery.yml: %v", err)
	}
}
