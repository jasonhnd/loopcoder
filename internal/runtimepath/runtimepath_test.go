package runtimepath_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/relay"
	"github.com/jasonhnd/loopcoder/internal/relaygate"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/reportquery"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/state"
)

func TestRegisteredRuntimePayloadsRemainDiscoverableAfterCheckoutDeleted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOOPCODER_HOME", home)
	repo := t.TempDir()
	ctx := context.Background()

	registered, err := registry.Register(ctx, registry.Options{RepoPath: repo}, registry.DefaultDeps())
	if err != nil {
		t.Fatalf("register project: %v", err)
	}
	runID := state.RunIDForIssue(711, time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC))
	reportRecord := reporter.Report{
		WorkID:      runID,
		Issue:       711,
		Branch:      "loop/issue-711",
		Role:        reporter.RoleWorker,
		Provider:    "codex",
		Model:       "gpt-5",
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "high",
		Permission:  reporter.PermissionWrite,
		Action:      "implement issue #711",
		ExitCode:    0,
		StartedAt:   "2026-07-10T00:00:00Z",
		EndedAt:     "2026-07-10T00:00:01Z",
		DurationMS:  1000,
		Usage: reporter.Usage{
			TotalTokens: int64Ptr(711),
		},
		Verified: true,
	}

	if _, err := state.WriteAttempt(repo, runID, state.AttemptRecord{
		Version:        1,
		JobID:          "job-711-1",
		Issue:          711,
		Attempt:        1,
		Provider:       "codex",
		Phase:          "pr_opened",
		Status:         state.StatusSucceeded,
		StartedAt:      "2026-07-10T00:00:00Z",
		HeartbeatAt:    "2026-07-10T00:00:01Z",
		LastProgressAt: "2026-07-10T00:00:01Z",
		Report:         &reportRecord,
	}); err != nil {
		t.Fatalf("write attempt: %v", err)
	}
	if err := state.AppendEvent(repo, runID, state.Event{Timestamp: "2026-07-10T00:00:01Z", RunID: runID, JobID: "job-711-1", Issue: 711, Phase: "done", Status: state.StatusSucceeded}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{Timestamp: "2026-07-10T00:00:00Z", RunID: runID, State: state.StatePlanned}); err != nil {
		t.Fatalf("append planned lifecycle: %v", err)
	}
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{Timestamp: "2026-07-10T00:00:00Z", RunID: runID, State: state.StateRunning}); err != nil {
		t.Fatalf("append running lifecycle: %v", err)
	}
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{Timestamp: "2026-07-10T00:00:01Z", RunID: runID, State: state.StateSucceeded}); err != nil {
		t.Fatalf("append succeeded lifecycle: %v", err)
	}
	briefPath := state.RecoveryBriefPath(repo, runID, "job-711-1")
	if err := os.MkdirAll(filepath.Dir(briefPath), 0o700); err != nil {
		t.Fatalf("create recovery directory: %v", err)
	}
	if err := os.WriteFile(briefPath, []byte("global recovery brief"), 0o600); err != nil {
		t.Fatalf("write recovery brief: %v", err)
	}
	if _, err := relay.Write(relay.Entry{
		RepoPath:     repo,
		RunID:        runID,
		InvocationID: "job-711-1",
		Command:      "dispatch",
		Role:         reporter.RoleWorker,
		Issue:        711,
		CreatedAt:    time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC),
		Header:       reportRecord.Header(),
		Pretty:       reportRecord.Pretty(reporter.PrettyOptions{}),
		Report:       &reportRecord,
	}); err != nil {
		t.Fatalf("write relay ledger: %v", err)
	}
	if _, err := relaygate.Write(relaygate.WriteOptions{RepoPath: repo, RunID: runID, Role: string(reporter.RoleWorker), Block: reportRecord.Pretty(reporter.PrettyOptions{}), Report: &reportRecord}); err != nil {
		t.Fatalf("write pending relay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".loopcoder")); !os.IsNotExist(err) {
		t.Fatalf("registered runtime created repo-local .loopcoder: %v", err)
	}

	roots, err := runtimepath.Resolve(ctx, repo)
	if err != nil {
		t.Fatalf("resolve runtime roots: %v", err)
	}
	if !roots.Registered || roots.ProjectID != registered.Project.ProjectID {
		t.Fatalf("roots registered/project = %v/%s, want %s", roots.Registered, roots.ProjectID, registered.Project.ProjectID)
	}
	if !strings.HasPrefix(filepath.Clean(briefPath), filepath.Clean(roots.RecoveryRoot)+string(filepath.Separator)) {
		t.Fatalf("recovery brief path %s is not under recovery root %s", briefPath, roots.RecoveryRoot)
	}
	if strings.HasPrefix(filepath.Clean(briefPath), filepath.Clean(roots.RunsRoot)+string(filepath.Separator)) {
		t.Fatalf("recovery brief path stayed under runs root: %s", briefPath)
	}
	for _, path := range []string{state.AttemptPath(repo, runID, "job-711-1"), state.EventsPath(repo, runID), state.LifecyclePath(repo, runID), briefPath} {
		if strings.HasPrefix(filepath.Clean(path), filepath.Clean(repo)+string(filepath.Separator)) {
			t.Fatalf("registered payload path stayed under repo: %s", path)
		}
	}

	if err := os.RemoveAll(repo); err != nil {
		t.Fatalf("delete checkout: %v", err)
	}
	attempts, err := state.LoadAttempts(repo, runID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("load attempts after delete = %d, %v", len(attempts), err)
	}
	lifecycle, err := state.LoadLifecycle(repo, runID)
	if err != nil || lifecycle.State != state.StateSucceeded {
		t.Fatalf("load lifecycle after delete = %#v, %v", lifecycle, err)
	}
	if data, err := os.ReadFile(state.RecoveryBriefPath(repo, runID, "job-711-1")); err != nil || string(data) != "global recovery brief" {
		t.Fatalf("read recovery after delete = %q, %v", data, err)
	}
	if pending := relaygate.List(repo); len(pending) != 1 {
		t.Fatalf("pending relay after delete = %d, want 1", len(pending))
	}
	reports, err := reportquery.List(reportquery.Options{RepoPath: repo, WorkID: runID})
	if err != nil || len(reports) == 0 {
		t.Fatalf("report query after delete = %d, %v", len(reports), err)
	}
}

func TestRegisteredRecoveryPathsUseProjectRecoveryRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOOPCODER_HOME", home)
	repo := t.TempDir()
	ctx := context.Background()
	if _, err := registry.Register(ctx, registry.Options{RepoPath: repo}, registry.DefaultDeps()); err != nil {
		t.Fatalf("register project: %v", err)
	}

	roots, err := runtimepath.Resolve(ctx, repo)
	if err != nil {
		t.Fatalf("resolve roots: %v", err)
	}
	runID := "run-20260710T000000Z-issue-711"
	recoveryPath := state.RecoveryPath(repo, runID)
	briefPath := state.RecoveryBriefPath(repo, runID, "job-711-2")

	if want := filepath.Join(roots.RecoveryRoot, runID); recoveryPath != want {
		t.Fatalf("RecoveryPath = %s, want %s", recoveryPath, want)
	}
	if !strings.HasPrefix(filepath.Clean(briefPath), filepath.Clean(roots.RecoveryRoot)+string(filepath.Separator)) {
		t.Fatalf("RecoveryBriefPath = %s, want under %s", briefPath, roots.RecoveryRoot)
	}
	if strings.Contains(filepath.ToSlash(briefPath), "/runs/"+runID+"/recovery/") {
		t.Fatalf("RecoveryBriefPath used run payload recovery location: %s", briefPath)
	}
}

func TestRegisteredRecoveryReadFallsBackToLegacyLoopcoderRunRecovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOOPCODER_HOME", home)
	repo := t.TempDir()
	ctx := context.Background()
	if _, err := registry.Register(ctx, registry.Options{RepoPath: repo}, registry.DefaultDeps()); err != nil {
		t.Fatalf("register project: %v", err)
	}

	runID := "run-20260710T000000Z-issue-711"
	jobID := "job-711-legacy"
	legacyDir := filepath.Join(repo, ".loopcoder", "runs", runID, "recovery")
	legacyBrief := filepath.Join(legacyDir, jobID+"-context.md")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("create legacy recovery dir: %v", err)
	}
	if err := os.WriteFile(legacyBrief, []byte("legacy recovery"), 0o644); err != nil {
		t.Fatalf("write legacy recovery brief: %v", err)
	}

	globalBrief := state.RecoveryBriefPath(repo, runID, jobID)
	if globalBrief == legacyBrief {
		t.Fatalf("write path unexpectedly points at legacy recovery: %s", globalBrief)
	}
	if got := state.RecoveryPathForRead(repo, runID); got != legacyDir {
		t.Fatalf("RecoveryPathForRead = %s, want %s", got, legacyDir)
	}
	if got := state.RecoveryBriefPathForRead(repo, runID, jobID); got != legacyBrief {
		t.Fatalf("RecoveryBriefPathForRead = %s, want %s", got, legacyBrief)
	}
	data, err := os.ReadFile(state.RecoveryBriefPathForRead(repo, runID, jobID))
	if err != nil || string(data) != "legacy recovery" {
		t.Fatalf("read fallback recovery = %q, %v", data, err)
	}
}

func TestRegisteredRemoteCheckoutsShareProjectPayloadRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("LOOPCODER_HOME", home)
	ctx := context.Background()
	first := t.TempDir()
	second := t.TempDir()
	initRepoWithOrigin(t, first, "https://github.com/Owner/Shared.git")
	initRepoWithOrigin(t, second, "git@github.com:owner/shared.git")

	registered, err := registry.Register(ctx, registry.Options{RepoPath: first}, registry.DefaultDeps())
	if err != nil {
		t.Fatalf("register first checkout: %v", err)
	}
	firstRoots, err := runtimepath.Resolve(ctx, first)
	if err != nil {
		t.Fatalf("resolve first roots: %v", err)
	}
	secondRoots, err := runtimepath.Resolve(ctx, second)
	if err != nil {
		t.Fatalf("resolve second roots: %v", err)
	}
	if !secondRoots.Registered || secondRoots.ProjectID != registered.Project.ProjectID {
		t.Fatalf("second checkout project = registered=%v id=%s, want %s", secondRoots.Registered, secondRoots.ProjectID, registered.Project.ProjectID)
	}
	if firstRoots.ProjectRoot != secondRoots.ProjectRoot {
		t.Fatalf("project roots differ: %s vs %s", firstRoots.ProjectRoot, secondRoots.ProjectRoot)
	}
}

func initRepoWithOrigin(t *testing.T, repo, remote string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"remote", "add", "origin", remote},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}
