package orchestration

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTriggerCronInvokesOneTickWithExplicitRepo(t *testing.T) {
	repo := t.TempDir()
	called := 0

	report, err := RunTrigger(context.Background(), TriggerOptions{
		Kind:     TriggerKindCron,
		RepoPath: repo,
		Schedule: "0 * * * *",
		Clock: func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		},
		Tick: func(_ context.Context, opts TickOptions) (TickReport, error) {
			called++
			if opts.RepoPath != repo {
				t.Fatalf("tick RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			if opts.RunID == "" {
				t.Fatal("tick RunID is empty")
			}
			return triggerTickReport(opts, TickStatusSucceeded, TickStopCompleted), nil
		},
	})
	if err != nil {
		t.Fatalf("RunTrigger returned error: %v", err)
	}
	if called != 1 {
		t.Fatalf("tick calls = %d, want 1", called)
	}
	if report.Kind != TriggerKindCron || report.Schedule != "0 * * * *" || report.Status != TriggerStatusSucceeded {
		t.Fatalf("trigger report = %#v", report)
	}
	if report.Iterations != 1 || len(report.Ticks) != 1 {
		t.Fatalf("iterations=%d ticks=%d, want 1", report.Iterations, len(report.Ticks))
	}
}

func TestTriggerHookInvokesOneTickWithEvent(t *testing.T) {
	repo := t.TempDir()
	var gotEvent string

	report, err := RunTrigger(context.Background(), TriggerOptions{
		Kind:     TriggerKindHook,
		RepoPath: repo,
		Event:    "ci-failure",
		Tick: func(_ context.Context, opts TickOptions) (TickReport, error) {
			if opts.RepoPath != repo {
				t.Fatalf("tick RepoPath = %q, want %q", opts.RepoPath, repo)
			}
			gotEvent = "ci-failure"
			return triggerTickReport(opts, TickStatusSucceeded, TickStopCompleted), nil
		},
	})
	if err != nil {
		t.Fatalf("RunTrigger returned error: %v", err)
	}
	if gotEvent != "ci-failure" || report.Event != "ci-failure" || report.Status != TriggerStatusSucceeded {
		t.Fatalf("event/report = %q %#v", gotEvent, report)
	}
	if len(report.Ticks) != 1 {
		t.Fatalf("ticks = %d, want 1", len(report.Ticks))
	}
}

func TestTriggerGoalLoopStopsWhenGoalReached(t *testing.T) {
	repo := t.TempDir()
	var runIDs []string

	report, err := RunTrigger(context.Background(), TriggerOptions{
		Kind:          TriggerKindGoalLoop,
		RepoPath:      repo,
		MaxIterations: 5,
		Clock: func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		},
		Tick: func(_ context.Context, opts TickOptions) (TickReport, error) {
			runIDs = append(runIDs, opts.RunID)
			if len(runIDs) < 3 {
				return triggerTickReport(opts, TickStatusSucceeded, TickStopCompleted), nil
			}
			return triggerTickReport(opts, TickStatusNoReadyWork, TickStopNoReadyWork), nil
		},
	})
	if err != nil {
		t.Fatalf("RunTrigger returned error: %v", err)
	}
	if report.Status != TriggerStatusSucceeded || report.StopReason != TriggerStopGoalReached {
		t.Fatalf("status=%s stop=%s, want goal reached", report.Status, report.StopReason)
	}
	if len(runIDs) != 3 {
		t.Fatalf("tick calls = %d, want 3", len(runIDs))
	}
	if runIDs[0] == runIDs[1] || runIDs[1] == runIDs[2] {
		t.Fatalf("goal-loop reused run ids: %#v", runIDs)
	}
}

func TestTriggerGoalLoopMaxIterationsRoutesNeedsHuman(t *testing.T) {
	repo := t.TempDir()
	called := 0

	report, err := RunTrigger(context.Background(), TriggerOptions{
		Kind:          TriggerKindGoalLoop,
		RepoPath:      repo,
		MaxIterations: 2,
		Tick: func(_ context.Context, opts TickOptions) (TickReport, error) {
			called++
			return triggerTickReport(opts, TickStatusSucceeded, TickStopCompleted), nil
		},
	})
	if err != nil {
		t.Fatalf("RunTrigger returned error: %v", err)
	}
	if called != 2 {
		t.Fatalf("tick calls = %d, want 2", called)
	}
	if report.Status != TriggerStatusNeedsHuman || report.StopReason != TriggerStopMaxIterations {
		t.Fatalf("status=%s stop=%s, want needs-human max-iterations", report.Status, report.StopReason)
	}
	if TriggerExitCode(report) != 2 {
		t.Fatalf("exit code = %d, want 2", TriggerExitCode(report))
	}
}

func TestTriggerGoalLoopStopsOnTickNeedsHuman(t *testing.T) {
	repo := t.TempDir()
	called := 0

	report, err := RunTrigger(context.Background(), TriggerOptions{
		Kind:          TriggerKindGoalLoop,
		RepoPath:      repo,
		MaxIterations: 5,
		Tick: func(_ context.Context, opts TickOptions) (TickReport, error) {
			called++
			if called == 1 {
				return triggerTickReport(opts, TickStatusSucceeded, TickStopCompleted), nil
			}
			return triggerTickReport(opts, TickStatusNeedsHuman, TickStopReviewNeedsHuman), nil
		},
	})
	if err != nil {
		t.Fatalf("RunTrigger returned error: %v", err)
	}
	if called != 2 {
		t.Fatalf("tick calls = %d, want 2", called)
	}
	if report.Status != TriggerStatusNeedsHuman || report.StopReason != TriggerStopNeedsHuman {
		t.Fatalf("status=%s stop=%s, want tick-needs-human", report.Status, report.StopReason)
	}
}

func TestTriggerTickErrorReportsFailure(t *testing.T) {
	repo := t.TempDir()

	report, err := RunTrigger(context.Background(), TriggerOptions{
		Kind:     TriggerKindCron,
		RepoPath: repo,
		Schedule: "@hourly",
		Tick: func(context.Context, TickOptions) (TickReport, error) {
			return TickReport{}, errors.New("tick exploded")
		},
	})
	if err != nil {
		t.Fatalf("RunTrigger returned error: %v", err)
	}
	if report.Status != TriggerStatusFailed || report.StopReason != TriggerStopTickError || !strings.Contains(report.Error, "tick exploded") {
		t.Fatalf("report = %#v", report)
	}
	if TriggerExitCode(report) != 1 {
		t.Fatalf("exit code = %d, want 1", TriggerExitCode(report))
	}
}

func TestTriggerRequiresExplicitRepoPerStep(t *testing.T) {
	_, err := RunTrigger(context.Background(), TriggerOptions{
		Kind:     TriggerKindCron,
		Schedule: "@daily",
		Tick: func(context.Context, TickOptions) (TickReport, error) {
			t.Fatal("tick should not be called without explicit repo")
			return TickReport{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "repo path is required") {
		t.Fatalf("error = %v, want repo path required", err)
	}
}

func TestTriggerDependencySurfaceHasNoMergePromoteAuthority(t *testing.T) {
	triggerOptions := reflect.TypeOf(TriggerOptions{})
	var hits []string
	for i := 0; i < triggerOptions.NumField(); i++ {
		field := triggerOptions.Field(i)
		name := strings.ToLower(field.Name)
		typeName := strings.ToLower(field.Type.String())
		if strings.Contains(name, "main") || strings.Contains(typeName, "main") ||
			strings.Contains(name, "promote") || strings.Contains(typeName, "promote") {
			hits = append(hits, field.Name+" "+field.Type.String())
		}
		if strings.Contains(name, "merge") || strings.Contains(typeName, "merge") {
			hits = append(hits, field.Name+" "+field.Type.String())
		}
	}
	if len(hits) > 0 {
		t.Fatalf("trigger dependency surface exposes forbidden main/promote/merge names: %s", strings.Join(hits, ", "))
	}
}

func triggerTickReport(opts TickOptions, status, stopReason string) TickReport {
	return normalizeTickReport(TickReport{
		Version:       TickReportVersion,
		Repo:          "owner/repo",
		RepoPath:      opts.RepoPath,
		BaseBranch:    firstNonEmpty(opts.BaseBranch, "main"),
		PreProdBranch: firstNonEmpty(opts.PreProdBranch, "pre-prod"),
		RunID:         opts.RunID,
		Status:        status,
		StopReason:    stopReason,
		StartedAt:     "2026-07-02T12:00:00Z",
		FinishedAt:    "2026-07-02T12:00:00Z",
	})
}
