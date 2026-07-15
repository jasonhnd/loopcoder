package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func writeAttemptSidecar(t *testing.T, repo, runID, job, body string) {
	t.Helper()
	workers := filepath.Join(repo, ".loopcoder", "runs", runID, "workers")
	if err := os.MkdirAll(workers, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workers, job+".attempt.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadManagedProcessesReadsRunningAttempt(t *testing.T) {
	repo := t.TempDir()
	pid := os.Getpid() // this test process is guaranteed alive
	body := fmt.Sprintf(`{"version":1,"job_id":"job-42-1","issue":42,"provider":"codex","pid":%d,"status":"running","started_at":"2026-01-01T00:00:00Z"}`, pid)
	writeAttemptSidecar(t, repo, "run-20260101T000000Z-issue-42", "job-42-1", body)
	seedProviderAuthority(t, repo, "run-20260101T000000Z-issue-42", "job-42-1", pid)

	rows, err := loadManagedProcesses(repo, fixedManageNow)
	if err != nil {
		t.Fatalf("loadManagedProcesses: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].PID != pid || rows[0].Issue != 42 || rows[0].Provider != "codex" || rows[0].Status != "active" || !rows[0].Verified {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestLoadManagedProcessesIgnoresAttemptSidecarLiveness(t *testing.T) {
	repo := t.TempDir()
	// running but a pid that cannot be alive
	writeAttemptSidecar(t, repo, "run-20260101T000000Z-issue-9", "dead",
		`{"issue":9,"pid":2147480000,"status":"running","started_at":"t"}`)
	// alive pid but already succeeded
	writeAttemptSidecar(t, repo, "run-20260101T000000Z-issue-9", "done",
		fmt.Sprintf(`{"issue":9,"pid":%d,"status":"succeeded","started_at":"t"}`, os.Getpid()))

	rows, err := loadManagedProcesses(repo, fixedManageNow)
	if err != nil {
		t.Fatalf("loadManagedProcesses: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
}

func TestRunPsEmptyRepo(t *testing.T) {
	var out, errb bytes.Buffer
	code := runPs([]string{"--repo", t.TempDir()}, &out, &errb, Deps{})
	if code != 0 {
		t.Fatalf("runPs exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "no loopcoder-managed provider authorities found") {
		t.Fatalf("runPs output = %q", out.String())
	}
}

func TestRunPsJSONRendersDurableAuthority(t *testing.T) {
	repo := t.TempDir()
	runID := "run-20260101T000000Z-issue-11"
	writeAttemptSidecar(t, repo, runID, "job-11-1",
		fmt.Sprintf(`{"issue":11,"provider":"codex","pid":%d,"status":"running","started_at":"2026-01-01T00:00:00Z"}`, os.Getpid()))
	pgid := seedProviderAuthority(t, repo, runID, "job-11-1", os.Getpid())

	var out, errb bytes.Buffer
	code := runPs([]string{"--repo", repo, "--format", "json"}, &out, &errb, Deps{Now: fixedManageNow})
	if code != 0 {
		t.Fatalf("runPs json exit=%d stderr=%q", code, errb.String())
	}
	var payload struct {
		SchemaVersion string       `json:"schema_version"`
		Rows          []managedRow `json:"rows"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal ps json: %v\n%s", err, out.String())
	}
	if payload.SchemaVersion != "loopcoder.provider_processes.v1" || len(payload.Rows) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
	row := payload.Rows[0]
	if row.Run != runID || row.Attempt != "job-11-1" || row.Issue != 11 || row.Provider != "codex" || row.PGID != pgid || row.Status != "active" || !row.Verified {
		t.Fatalf("row = %#v", row)
	}
}

func TestRunKillRequiresScope(t *testing.T) {
	var out, errb bytes.Buffer
	code := runKill([]string{"--repo", t.TempDir()}, &out, &errb, Deps{})
	if code != 2 {
		t.Fatalf("runKill without --run/--all exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "--run") {
		t.Fatalf("runKill error = %q", errb.String())
	}
}

func TestRunKillRunFilterMatchesNothingIsSafe(t *testing.T) {
	repo := t.TempDir()
	// A running attempt under a DIFFERENT run id must not be touched when killing
	// a non-matching run (this test process's own pid).
	body := fmt.Sprintf(`{"issue":7,"pid":%d,"status":"running","started_at":"t"}`, os.Getpid())
	writeAttemptSidecar(t, repo, "run-20260101T000000Z-issue-7", "job-7-1", body)
	seedProviderAuthority(t, repo, "run-20260101T000000Z-issue-7", "job-7-1", os.Getpid())

	var out, errb bytes.Buffer
	killed := 0
	code := runKill([]string{"--repo", repo, "--run", "run-20990101T000000Z-issue-999"}, &out, &errb, Deps{
		Now: fixedManageNow,
		KillProcessGroup: func(int) error {
			killed++
			return nil
		},
	})
	if code != 0 {
		t.Fatalf("runKill exit = %d, want 0", code)
	}
	if killed != 0 {
		t.Fatalf("kill was called %d times, want 0", killed)
	}
	if !strings.Contains(out.String(), "terminated 0 loopcoder-managed provider group(s)") {
		t.Fatalf("runKill should have matched nothing: %q", out.String())
	}
}

func TestRunKillTerminatesMatchingRun(t *testing.T) {
	repo := t.TempDir()
	runID := "run-20260101T000000Z-issue-5"
	pid := os.Getpid()
	body := fmt.Sprintf(`{"issue":5,"provider":"codex","pid":%d,"status":"running","started_at":"t"}`, pid)
	writeAttemptSidecar(t, repo, runID, "job-5-1", body)
	pgid := seedProviderAuthority(t, repo, runID, "job-5-1", pid)

	var out, errb bytes.Buffer
	var killedPGID int
	if code := runKill([]string{"--repo", repo, "--run", runID}, &out, &errb, Deps{
		Now: fixedManageNow,
		KillProcessGroup: func(got int) error {
			killedPGID = got
			return nil
		},
	}); code != 0 {
		t.Fatalf("runKill = %d, stderr=%q", code, errb.String())
	}
	if killedPGID != pgid {
		t.Fatalf("killed pgid = %d, want %d", killedPGID, pgid)
	}
	if !strings.Contains(out.String(), "terminated 1 loopcoder-managed provider group(s)") {
		t.Fatalf("runKill output = %q", out.String())
	}
}

func TestRunKillRefusesStaleOwnershipGeneration(t *testing.T) {
	repo := t.TempDir()
	runID := "run-20260101T000000Z-issue-6"
	pid := os.Getpid()
	writeAttemptSidecar(t, repo, runID, "job-6-1",
		fmt.Sprintf(`{"issue":6,"provider":"codex","pid":%d,"status":"running","started_at":"t"}`, pid))
	seedProviderAuthority(t, repo, runID, "job-6-1", pid)
	store := openManageStore(t)
	defer store.Close()
	if err := store.WithWriteTx(context.Background(), func(tx storage.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE agent_ownership_locks SET claim_generation = claim_generation + 1 WHERE run_id = ?`, runID)
		return err
	}); err != nil {
		t.Fatalf("stale ownership mutation: %v", err)
	}

	var out, errb bytes.Buffer
	killed := 0
	code := runKill([]string{"--repo", repo, "--run", runID}, &out, &errb, Deps{
		Now: fixedManageNow,
		KillProcessGroup: func(int) error {
			killed++
			return nil
		},
	})
	if code != 1 {
		t.Fatalf("runKill exit=%d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if killed != 0 {
		t.Fatalf("kill called %d times, want 0", killed)
	}
	if !strings.Contains(errb.String(), "ownership fence is stale") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func fixedManageNow() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

func seedProviderAuthority(t *testing.T, repo, runID, jobID string, pid int) int {
	t.Helper()
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), "home"))
	ctx := context.Background()
	reg, err := registry.Register(ctx, registry.Options{RepoPath: repo, Now: fixedManageNow}, registry.DefaultDeps())
	if err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	store := openManageStore(t)
	defer store.Close()
	lease, err := storage.AcquireAgentOwnershipLease(ctx, store, storage.AgentOwnershipLeaseRequest{
		ProjectID:     reg.Project.ProjectID,
		DeliveryRunID: runID,
		RunID:         runID,
		OwnerID:       "worker:" + runID + ":" + jobID,
		Now:           fixedManageNow(),
		LeaseUntil:    fixedManageNow().Add(time.Hour),
		Resources: []storage.AgentOwnershipResource{
			{ResourceKind: "runtime-run", ResourceKey: runID},
		},
	})
	if err != nil {
		t.Fatalf("AcquireAgentOwnershipLease: %v", err)
	}
	identity, err := process.Snapshot(pid, fixedManageNow())
	if err != nil {
		t.Fatalf("process.Snapshot: %v", err)
	}
	_, err = storage.PersistProviderExecutionAuthority(ctx, store, storage.ProviderExecutionAuthority{
		ProjectID:            reg.Project.ProjectID,
		RunID:                runID,
		AttemptID:            jobID,
		ProviderPID:          identity.PID,
		ProviderPGID:         identity.PGID,
		ProcessBirthIdentity: identity.ProcessBirthIdentity,
		ExecutableIdentity:   identity.ExecutableIdentity,
		OwnerID:              lease.OwnerID,
		ClaimGeneration:      lease.ClaimGeneration,
		StartedAt:            "2026-01-01T00:00:00Z",
		HeartbeatAt:          "2026-01-01T00:00:00Z",
		WorktreePath:         filepath.Join(repo, "worktree"),
		LogPath:              filepath.Join(repo, "worker.log"),
		IdentityAmbiguous:    identity.Ambiguous,
		AmbiguityReason:      identity.AmbiguityReason,
	}, fixedManageNow())
	if err != nil {
		t.Fatalf("PersistProviderExecutionAuthority: %v", err)
	}
	return identity.PGID
}

func openManageStore(t *testing.T) storage.Store {
	t.Helper()
	layout, err := home.Resolve(home.DefaultDeps())
	if err != nil {
		t.Fatalf("home.Resolve: %v", err)
	}
	store, err := storage.Open(context.Background(), storage.Options{Path: layout.DatabasePath(), Now: fixedManageNow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	return store
}
