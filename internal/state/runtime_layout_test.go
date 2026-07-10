package state

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/registry"
)

func TestResolveRuntimeLayoutRegisteredUsesGlobalPayloadRoot(t *testing.T) {
	repo, layout := registerRuntimeLayoutTestProject(t)

	resolved, err := ResolveRuntimeLayout(repo)
	if err != nil {
		t.Fatalf("ResolveRuntimeLayout: %v", err)
	}
	if resolved.Mode != RuntimeModeRegisteredGlobal || !resolved.Registered {
		t.Fatalf("mode/registered = %s/%v, want registered global", resolved.Mode, resolved.Registered)
	}
	if resolved.ProjectID == "" {
		t.Fatalf("ProjectID is empty")
	}
	wantRoot := layout.ProjectDir(resolved.ProjectID)
	if resolved.PayloadRoot != wantRoot || resolved.RunsRoot != filepath.Join(wantRoot, "runs") {
		t.Fatalf("payload roots = %#v, want %s", resolved, wantRoot)
	}

	runID := "run-20260710T000000Z-issue-711"
	if _, err := WriteAttempt(repo, runID, AttemptRecord{
		Version:        1,
		JobID:          "job-711-1",
		Issue:          711,
		Attempt:        1,
		Provider:       "codex",
		PID:            123,
		Phase:          "done",
		Status:         StatusSucceeded,
		StartedAt:      FormatTimestamp(time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)),
		HeartbeatAt:    FormatTimestamp(time.Date(2026, 7, 10, 0, 1, 0, 0, time.UTC)),
		LastProgressAt: FormatTimestamp(time.Date(2026, 7, 10, 0, 1, 0, 0, time.UTC)),
	}); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".loopcoder")); !os.IsNotExist(err) {
		t.Fatalf("registered WriteAttempt created repo-local .loopcoder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wantRoot, "runs", runID, "workers", "job-711-1.attempt.json")); err != nil {
		t.Fatalf("global attempt missing: %v", err)
	}
}

func TestResolveRuntimeLayoutUnregisteredIsExplicitRepoLocalFallback(t *testing.T) {
	repo := t.TempDir()
	homeDir := filepath.Join(t.TempDir(), "home")
	t.Setenv(home.EnvHome, homeDir)

	resolved, err := ResolveRuntimeLayout(repo)
	if err != nil {
		t.Fatalf("ResolveRuntimeLayout: %v", err)
	}
	if resolved.Mode != RuntimeModeUnregisteredRepoLocal || resolved.Registered {
		t.Fatalf("mode/registered = %s/%v, want unregistered repo-local", resolved.Mode, resolved.Registered)
	}
	if resolved.PayloadRoot != filepath.Join(repo, ".loopcoder") {
		t.Fatalf("PayloadRoot = %s, want repo-local .loopcoder", resolved.PayloadRoot)
	}
}

func TestResolveRuntimeLayoutSymlinkAliasSharesProjectHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping symlink test in short mode")
	}
	repo, _ := registerRuntimeLayoutTestProject(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	fromRepo, err := ResolveRuntimeLayout(repo)
	if err != nil {
		t.Fatalf("ResolveRuntimeLayout repo: %v", err)
	}
	fromAlias, err := ResolveRuntimeLayout(alias)
	if err != nil {
		t.Fatalf("ResolveRuntimeLayout alias: %v", err)
	}
	if fromRepo.ProjectID != fromAlias.ProjectID || fromRepo.PayloadRoot != fromAlias.PayloadRoot {
		t.Fatalf("alias resolved different history: repo=%#v alias=%#v", fromRepo, fromAlias)
	}
}

func TestRegisteredConcurrentWritesStayInGlobalPayloadRoot(t *testing.T) {
	repo, layout := registerRuntimeLayoutTestProject(t)
	resolved, err := ResolveRuntimeLayout(repo)
	if err != nil {
		t.Fatalf("ResolveRuntimeLayout: %v", err)
	}
	runID := "run-20260710T000000Z-issue-712"
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := WriteAttempt(repo, runID, AttemptRecord{
				Version:        1,
				JobID:          "job-712-" + string(rune('a'+i)),
				Issue:          712,
				Attempt:        i + 1,
				Provider:       "codex",
				PID:            100 + i,
				Phase:          "running",
				Status:         StatusRunning,
				StartedAt:      FormatTimestamp(time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)),
				HeartbeatAt:    FormatTimestamp(time.Date(2026, 7, 10, 0, 1, 0, 0, time.UTC)),
				LastProgressAt: FormatTimestamp(time.Date(2026, 7, 10, 0, 1, 0, 0, time.UTC)),
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("WriteAttempt: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".loopcoder")); !os.IsNotExist(err) {
		t.Fatalf("registered concurrent writes created repo-local .loopcoder: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(layout.ProjectDir(resolved.ProjectID), "runs", runID, "workers"))
	if err != nil {
		t.Fatalf("read global workers: %v", err)
	}
	if len(entries) != 8 {
		t.Fatalf("global attempts = %d, want 8", len(entries))
	}
}

func registerRuntimeLayoutTestProject(t *testing.T) (string, home.Layout) {
	t.Helper()
	repo := t.TempDir()
	homeDir := filepath.Join(t.TempDir(), "home")
	t.Setenv(home.EnvHome, homeDir)
	layout := home.New(homeDir)
	if _, err := registry.Register(context.Background(), registry.Options{
		RepoPath: repo,
		Now: func() time.Time {
			return time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
		},
	}, registry.DefaultDeps()); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	return repo, layout
}
