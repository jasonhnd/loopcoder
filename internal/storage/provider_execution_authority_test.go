package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderExecutionAuthorityFencesHeartbeatAndCompletion(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	seedAuthorityProject(t, ctx, store, "proj-authority")

	started := fixedNow()
	authority, err := PersistProviderExecutionAuthority(ctx, store, ProviderExecutionAuthority{
		ProjectID:            "proj-authority",
		RunID:                "run-authority",
		AttemptID:            "job-960-1",
		ProviderPID:          12345,
		ProviderPGID:         12345,
		ProcessBirthIdentity: "pid=12345 pgid=12345 lstart=Fri Jan 2 03:04:05 2026",
		ExecutableIdentity:   "/usr/bin/true",
		OwnerID:              "worker:run-authority:job-960-1:1",
		ClaimGeneration:      7,
		WorktreePath:         "/tmp/loopcoder/wt",
		LogPath:              "/tmp/loopcoder/logs/job-960-1.log",
		IdentityAmbiguous:    false,
	}, started)
	if err != nil {
		t.Fatalf("PersistProviderExecutionAuthority: %v", err)
	}
	if authority.IdentityAmbiguous {
		t.Fatalf("authority marked ambiguous: %#v", authority)
	}

	current := ProviderExecutionAuthorityFence{
		ProjectID:       authority.ProjectID,
		RunID:           authority.RunID,
		AttemptID:       authority.AttemptID,
		OwnerID:         authority.OwnerID,
		ClaimGeneration: authority.ClaimGeneration,
	}
	stale := current
	stale.ClaimGeneration--
	if err := HeartbeatProviderExecutionAuthority(ctx, store, stale, started.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "stale owner or generation") {
		t.Fatalf("stale heartbeat error = %v, want stale owner/generation", err)
	}
	loaded := mustLoadAuthority(t, ctx, store, current)
	if loaded.HeartbeatAt != authority.HeartbeatAt {
		t.Fatalf("stale heartbeat mutated heartbeat_at = %q, want %q", loaded.HeartbeatAt, authority.HeartbeatAt)
	}

	if err := HeartbeatProviderExecutionAuthority(ctx, store, current, started.Add(2*time.Minute)); err != nil {
		t.Fatalf("current heartbeat: %v", err)
	}
	// Normal complete requires pid_event_persisted.
	if err := TransitionProviderExecutionSpawnPhase(ctx, store, current, started.Add(150*time.Second), SpawnPhasePIDEventPersisted); err != nil {
		t.Fatalf("transition pid_event_persisted: %v", err)
	}
	if err := CompleteProviderExecutionAuthority(ctx, store, stale, started.Add(3*time.Minute), "failed"); err == nil {
		t.Fatal("stale completion must fail")
	} else if !strings.Contains(err.Error(), "stale owner or generation") &&
		!strings.Contains(err.Error(), "no rows") &&
		!strings.Contains(err.Error(), "spawn_phase") {
		t.Fatalf("stale completion error = %v, want fence/phase reject", err)
	}
	if err := CompleteProviderExecutionAuthority(ctx, store, current, started.Add(4*time.Minute), "succeeded"); err != nil {
		t.Fatalf("current completion: %v", err)
	}
	loaded = mustLoadAuthority(t, ctx, store, current)
	if loaded.TerminalState != "succeeded" || loaded.CompletedAt == "" {
		t.Fatalf("completed authority = %#v, want succeeded completed record", loaded)
	}
	if err := HeartbeatProviderExecutionAuthority(ctx, store, current, started.Add(5*time.Minute)); err == nil || !strings.Contains(err.Error(), "stale owner or generation") {
		t.Fatalf("heartbeat after completion error = %v, want stale owner/generation", err)
	}
}

func TestProviderExecutionAuthoritySpawnPhaseSameWriteAndTransition(t *testing.T) {
	// SpawnPhase must land in the same Persist write as authority create, then
	// transition only via fenced row update — never a later event marker.
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	seedAuthorityProject(t, ctx, store, "proj-spawn-phase")

	started := fixedNow()
	// Empty SpawnPhase on input → Persist explicitly writes authority_persisted.
	authority, err := PersistProviderExecutionAuthority(ctx, store, ProviderExecutionAuthority{
		ProjectID:            "proj-spawn-phase",
		RunID:                "run-spawn",
		AttemptID:            "job-spawn-1",
		ProviderPID:          4242,
		ProviderPGID:         4242,
		ProcessBirthIdentity: "birth-spawn",
		ExecutableIdentity:   "/bin/sleep",
		OwnerID:              "worker:run-spawn:job-spawn-1:1",
		ClaimGeneration:      1,
		WorktreePath:         "/tmp/wt",
		LogPath:              "/tmp/log",
	}, started)
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if authority.SpawnPhase != SpawnPhaseAuthorityPersisted {
		t.Fatalf("SpawnPhase after Persist = %q want %s (same write, explicit)", authority.SpawnPhase, SpawnPhaseAuthorityPersisted)
	}
	fence := ProviderExecutionAuthorityFence{
		ProjectID: authority.ProjectID, RunID: authority.RunID, AttemptID: authority.AttemptID,
		OwnerID: authority.OwnerID, ClaimGeneration: authority.ClaimGeneration,
	}
	if err := TransitionProviderExecutionSpawnPhase(ctx, store, fence, started.Add(time.Second), SpawnPhasePIDEventPersisted); err != nil {
		t.Fatalf("transition to pid_event_persisted: %v", err)
	}
	loaded := mustLoadAuthority(t, ctx, store, fence)
	if loaded.SpawnPhase != SpawnPhasePIDEventPersisted {
		t.Fatalf("SpawnPhase = %q want %s", loaded.SpawnPhase, SpawnPhasePIDEventPersisted)
	}
	// Cannot transition to authority_persisted as target.
	if err := TransitionProviderExecutionSpawnPhase(ctx, store, fence, started.Add(2*time.Second), SpawnPhaseAuthorityPersisted); err == nil {
		t.Fatal("expected illegal transition target error")
	}
	// Fresh persist: authority_persisted → pid_event_failed.
	auth2, err := PersistProviderExecutionAuthority(ctx, store, ProviderExecutionAuthority{
		ProjectID: "proj-spawn-phase", RunID: "run-spawn", AttemptID: "job-spawn-2",
		ProviderPID: 5252, ProviderPGID: 5252,
		ProcessBirthIdentity: "birth-2", ExecutableIdentity: "/bin/sleep",
		OwnerID: "worker:run-spawn:job-spawn-2:1", ClaimGeneration: 1,
		WorktreePath: "/tmp/wt2", LogPath: "/tmp/log2",
	}, started)
	if err != nil {
		t.Fatalf("Persist 2: %v", err)
	}
	fence2 := ProviderExecutionAuthorityFence{
		ProjectID: auth2.ProjectID, RunID: auth2.RunID, AttemptID: auth2.AttemptID,
		OwnerID: auth2.OwnerID, ClaimGeneration: auth2.ClaimGeneration,
	}
	if err := TransitionProviderExecutionSpawnPhase(ctx, store, fence2, started.Add(time.Second), SpawnPhasePIDEventFailed); err != nil {
		t.Fatalf("transition to pid_event_failed: %v", err)
	}
	loaded2 := mustLoadAuthority(t, ctx, store, fence2)
	if loaded2.SpawnPhase != SpawnPhasePIDEventFailed {
		t.Fatalf("SpawnPhase = %q want %s", loaded2.SpawnPhase, SpawnPhasePIDEventFailed)
	}
}

func TestProviderExecutionAuthorityLegacyUnknownNotRecoverable(t *testing.T) {
	// pre-v33 / migration default rows stay legacy_unknown; transitions fail closed;
	// Load never promotes empty to authority_persisted.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	seedAuthorityProject(t, ctx, store, "proj-legacy")

	// Simulate a pre-v33 row: insert with only identity, force spawn_phase=legacy_unknown.
	started := fixedNow()
	ts := started.UTC().Format(time.RFC3339Nano)
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO provider_execution_authorities(
			authority_id, schema_version, record_version, project_id, run_id, attempt_id,
			provider_pid, provider_pgid, process_birth_identity, executable_identity, owner_id, claim_generation,
			started_at, heartbeat_at, worktree_path, log_path, identity_ambiguous, ambiguity_reason,
			spawn_phase, created_at, updated_at
		) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?, ?)`,
			"pauth_legacy1", ProviderExecutionAuthoritySchema,
			"proj-legacy", "run-legacy", "job-legacy-1",
			9991, 9991, "birth-legacy", "/bin/sleep", "owner-legacy-1", int64(1),
			ts, ts, "/tmp/wt-leg", "/tmp/log-leg",
			SpawnPhaseLegacyUnknown, ts, ts)
		return err
	}); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	loaded, err := LoadProviderExecutionAuthority(ctx, store, "proj-legacy", "run-legacy", "job-legacy-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.SpawnPhase != SpawnPhaseLegacyUnknown {
		t.Fatalf("SpawnPhase=%q want legacy_unknown (must not promote)", loaded.SpawnPhase)
	}
	fence := ProviderExecutionAuthorityFence{
		ProjectID: "proj-legacy", RunID: "run-legacy", AttemptID: "job-legacy-1",
		OwnerID: "owner-legacy-1", ClaimGeneration: 1,
	}
	if err := TransitionProviderExecutionSpawnPhase(ctx, store, fence, started.Add(time.Second), SpawnPhasePIDEventFailed); err == nil {
		t.Fatal("transition from legacy_unknown must fail closed")
	}
	// Empty spawn_phase on load → legacy_unknown
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_execution_authorities SET spawn_phase = '' WHERE attempt_id = ?`, "job-legacy-1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	loaded2, err := LoadProviderExecutionAuthority(ctx, store, "proj-legacy", "run-legacy", "job-legacy-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded2.SpawnPhase != SpawnPhaseLegacyUnknown {
		t.Fatalf("empty load SpawnPhase=%q want legacy_unknown", loaded2.SpawnPhase)
	}
}

func TestMigrationV33SpawnPhaseDefaultsToLegacyUnknown(t *testing.T) {
	// Rewind past v33, drop column, re-migrate: existing rows get legacy_unknown, not authority_persisted.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder-v33.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedAuthorityProject(t, ctx, store, "proj-mig33")
	// Insert a row, then rewind schema as if pre-v33 (drop spawn_phase, delete migration 33).
	ts := fixedNow().UTC().Format(time.RFC3339Nano)
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		// Ensure we can recreate pre-v33 shape: drop column if present, delete migration 33.
		// SQLite cannot DROP COLUMN on all versions — simulate by: delete migration 33,
		// rebuild table without spawn_phase is heavy. Instead: set all to a sentinel then
		// re-run migrate after drop via recreate.
		if _, err := tx.Exec(ctx, `DELETE FROM migrations WHERE version = 33`); err != nil {
			return err
		}
		// If column exists, leave it; apply() is idempotent when column present.
		// Force re-add path: recreate table without spawn_phase is complex.
		// Insert a second approach: use raw SQL to verify DEFAULT on ADD COLUMN path.
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	// Fresh DB: open to v32 only by deleting migration 33 after full open and dropping column
	// via recreate. Simpler path: open new store, manually create table without column, apply migrate.
	path2 := filepath.Join(t.TempDir(), "loopcoder-pre33.db")
	store2, err := Open(ctx, Options{Path: path2, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	// Rewind to v32: delete migration 33; if spawn_phase exists, we need migrate to leave it.
	// Build isolated pre-v33 table state:
	if err := store2.WithWriteTx(ctx, func(tx Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM migrations WHERE version >= 33`); err != nil {
			return err
		}
		// Recreate authority table without spawn_phase (pre-v33 shape).
		if _, err := tx.Exec(ctx, `DROP TABLE IF EXISTS provider_execution_authorities`); err != nil {
			return err
		}
		for _, stmt := range providerExecutionAuthoritySchemaStatements {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at, local_path_canonical, git_root, identity_source)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "proj-mig33b", "/tmp/p", ts, ts, "/tmp/p", "/tmp/p", "test"); err != nil {
			// project may already exist
			_ = err
		}
		_, err := tx.Exec(ctx, `INSERT INTO provider_execution_authorities(
			authority_id, schema_version, record_version, project_id, run_id, attempt_id,
			provider_pid, provider_pgid, process_birth_identity, executable_identity, owner_id, claim_generation,
			started_at, heartbeat_at, worktree_path, log_path, identity_ambiguous, ambiguity_reason,
			created_at, updated_at
		) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)`,
			"pauth_pre33", ProviderExecutionAuthoritySchema,
			"proj-mig33b", "run-pre33", "job-pre33",
			7777, 7777, "birth-pre", "/bin/true", "owner-pre33", int64(1),
			ts, ts, "/tmp/wt", "/tmp/log", ts, ts)
		return err
	}); err != nil {
		t.Fatalf("build pre-v33: %v", err)
	}
	_ = store2.Close()

	// Reopen → applies migration 33 with DEFAULT legacy_unknown.
	store3, err := Open(ctx, Options{Path: path2, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen migrate v33: %v", err)
	}
	defer store3.Close()
	loaded, err := LoadProviderExecutionAuthority(ctx, store3, "proj-mig33b", "run-pre33", "job-pre33")
	if err != nil {
		t.Fatalf("Load migrated: %v", err)
	}
	if loaded.SpawnPhase != SpawnPhaseLegacyUnknown {
		t.Fatalf("migrated SpawnPhase=%q want legacy_unknown (must not auto authority_persisted)", loaded.SpawnPhase)
	}
	fence := ProviderExecutionAuthorityFence{
		ProjectID: "proj-mig33b", RunID: "run-pre33", AttemptID: "job-pre33",
		OwnerID: "owner-pre33", ClaimGeneration: 1,
	}
	if err := TransitionProviderExecutionSpawnPhase(ctx, store3, fence, fixedNow(), SpawnPhasePIDEventPersisted); err == nil {
		t.Fatal("legacy_unknown must not transition")
	}
}

// Fenced negative tests for spawn-phase state machine (Gate 2B-2).
func TestProviderExecutionAuthoritySpawnPhaseCompleteRules(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	seedAuthorityProject(t, ctx, store, "proj-spawn-neg")
	started := fixedNow()

	// 1) Persist rejects caller-supplied pid_event_persisted / pid_event_failed.
	for _, bad := range []string{SpawnPhasePIDEventPersisted, SpawnPhasePIDEventFailed} {
		_, err := PersistProviderExecutionAuthority(ctx, store, ProviderExecutionAuthority{
			ProjectID: "proj-spawn-neg", RunID: "run-neg", AttemptID: "job-bad-" + bad,
			ProviderPID: 1001, ProviderPGID: 1001,
			ProcessBirthIdentity: "birth", ExecutableIdentity: "/bin/true",
			OwnerID: "owner-bad", ClaimGeneration: 1,
			WorktreePath: "/tmp/wt", LogPath: "/tmp/log",
			SpawnPhase: bad,
		}, started)
		if err == nil || !strings.Contains(err.Error(), "rejected on Persist") {
			t.Fatalf("Persist with %s: err=%v", bad, err)
		}
	}

	// 2) Normal complete rejected while still authority_persisted.
	auth, err := PersistProviderExecutionAuthority(ctx, store, ProviderExecutionAuthority{
		ProjectID: "proj-spawn-neg", RunID: "run-neg", AttemptID: "job-auth-only",
		ProviderPID: 2002, ProviderPGID: 2002,
		ProcessBirthIdentity: "birth2", ExecutableIdentity: "/bin/true",
		OwnerID: "owner-auth", ClaimGeneration: 1,
		WorktreePath: "/tmp/wt2", LogPath: "/tmp/log2",
	}, started)
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	fence := ProviderExecutionAuthorityFence{
		ProjectID: auth.ProjectID, RunID: auth.RunID, AttemptID: auth.AttemptID,
		OwnerID: auth.OwnerID, ClaimGeneration: auth.ClaimGeneration,
	}
	if err := CompleteProviderExecutionAuthority(ctx, store, fence, started.Add(time.Second), "failed"); err == nil {
		t.Fatal("normal complete from authority_persisted must fail")
	}
	// Pre-PID recovery complete allowed from authority_persisted.
	if err := CompleteProviderExecutionAuthorityPrePIDRecovery(ctx, store, fence, started.Add(2*time.Second), "failed"); err != nil {
		t.Fatalf("pre-PID complete from authority_persisted: %v", err)
	}

	// 3) Pre-PID complete rejected from pid_event_persisted; normal complete OK.
	auth2, err := PersistProviderExecutionAuthority(ctx, store, ProviderExecutionAuthority{
		ProjectID: "proj-spawn-neg", RunID: "run-neg", AttemptID: "job-pid-ok",
		ProviderPID: 3003, ProviderPGID: 3003,
		ProcessBirthIdentity: "birth3", ExecutableIdentity: "/bin/true",
		OwnerID: "owner-pid", ClaimGeneration: 1,
		WorktreePath: "/tmp/wt3", LogPath: "/tmp/log3",
	}, started)
	if err != nil {
		t.Fatalf("Persist2: %v", err)
	}
	fence2 := ProviderExecutionAuthorityFence{
		ProjectID: auth2.ProjectID, RunID: auth2.RunID, AttemptID: auth2.AttemptID,
		OwnerID: auth2.OwnerID, ClaimGeneration: auth2.ClaimGeneration,
	}
	if err := TransitionProviderExecutionSpawnPhase(ctx, store, fence2, started.Add(time.Second), SpawnPhasePIDEventPersisted); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := CompleteProviderExecutionAuthorityPrePIDRecovery(ctx, store, fence2, started.Add(2*time.Second), "failed"); err == nil {
		t.Fatal("pre-PID complete from pid_event_persisted must fail")
	}
	if err := CompleteProviderExecutionAuthority(ctx, store, fence2, started.Add(3*time.Second), "succeeded"); err != nil {
		t.Fatalf("normal complete from pid_event_persisted: %v", err)
	}

	// 4) legacy_unknown never completes (generic or pre-PID).
	ts := storageTimestamp(started)
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO provider_execution_authorities(
			authority_id, schema_version, record_version, project_id, run_id, attempt_id,
			provider_pid, provider_pgid, process_birth_identity, executable_identity, owner_id, claim_generation,
			started_at, heartbeat_at, worktree_path, log_path, identity_ambiguous, ambiguity_reason,
			spawn_phase, created_at, updated_at
		) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?, ?)`,
			"pauth_legacy_neg", ProviderExecutionAuthoritySchema,
			"proj-spawn-neg", "run-neg", "job-legacy",
			4004, 4004, "birth-l", "/bin/true", "owner-legacy", int64(1),
			ts, ts, "/tmp/wtl", "/tmp/logl",
			SpawnPhaseLegacyUnknown, ts, ts)
		return err
	}); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	fenceL := ProviderExecutionAuthorityFence{
		ProjectID: "proj-spawn-neg", RunID: "run-neg", AttemptID: "job-legacy",
		OwnerID: "owner-legacy", ClaimGeneration: 1,
	}
	if err := CompleteProviderExecutionAuthority(ctx, store, fenceL, started.Add(time.Second), "failed"); err == nil {
		t.Fatal("legacy normal complete must fail")
	}
	if err := CompleteProviderExecutionAuthorityPrePIDRecovery(ctx, store, fenceL, started.Add(2*time.Second), "failed"); err == nil {
		t.Fatal("legacy pre-PID complete must fail")
	}
	// Pre-PID complete allowed from pid_event_failed.
	auth3, err := PersistProviderExecutionAuthority(ctx, store, ProviderExecutionAuthority{
		ProjectID: "proj-spawn-neg", RunID: "run-neg", AttemptID: "job-pfail",
		ProviderPID: 5005, ProviderPGID: 5005,
		ProcessBirthIdentity: "birth5", ExecutableIdentity: "/bin/true",
		OwnerID: "owner-pfail", ClaimGeneration: 1,
		WorktreePath: "/tmp/wt5", LogPath: "/tmp/log5",
	}, started)
	if err != nil {
		t.Fatalf("Persist3: %v", err)
	}
	fence3 := ProviderExecutionAuthorityFence{
		ProjectID: auth3.ProjectID, RunID: auth3.RunID, AttemptID: auth3.AttemptID,
		OwnerID: auth3.OwnerID, ClaimGeneration: auth3.ClaimGeneration,
	}
	if err := TransitionProviderExecutionSpawnPhase(ctx, store, fence3, started.Add(time.Second), SpawnPhasePIDEventFailed); err != nil {
		t.Fatalf("to pid_event_failed: %v", err)
	}
	if err := CompleteProviderExecutionAuthority(ctx, store, fence3, started.Add(2*time.Second), "failed"); err == nil {
		t.Fatal("normal complete from pid_event_failed must fail")
	}
	if err := CompleteProviderExecutionAuthorityPrePIDRecovery(ctx, store, fence3, started.Add(3*time.Second), "failed"); err != nil {
		t.Fatalf("pre-PID complete from pid_event_failed: %v", err)
	}
}

func TestProviderExecutionAuthorityWithoutBirthIdentityIsAmbiguous(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	seedAuthorityProject(t, ctx, store, "proj-ambiguous")

	authority, err := PersistProviderExecutionAuthority(ctx, store, ProviderExecutionAuthority{
		ProjectID:       "proj-ambiguous",
		RunID:           "run-ambiguous",
		AttemptID:       "job-ambiguous",
		ProviderPID:     2222,
		OwnerID:         "worker:run-ambiguous:job-ambiguous:1",
		ClaimGeneration: 1,
		WorktreePath:    "/tmp/loopcoder/wt",
		LogPath:         "/tmp/loopcoder/log",
	}, fixedNow())
	if err != nil {
		t.Fatalf("PersistProviderExecutionAuthority ambiguous: %v", err)
	}
	if !authority.IdentityAmbiguous || authority.AmbiguityReason == "" {
		t.Fatalf("authority = %#v, want ambiguous with reason", authority)
	}
}

func seedAuthorityProject(t *testing.T, ctx context.Context, store Store, projectID string) {
	t.Helper()
	err := store.WithWriteTx(ctx, func(tx Tx) error {
		projectPath := t.TempDir()
		_, err := tx.Exec(ctx, `INSERT INTO projects(
				id, local_path, created_at, updated_at, local_path_canonical, git_root, identity_source
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			projectID, projectPath, formatTimestamp(fixedNow()), formatTimestamp(fixedNow()), projectPath, projectPath, "test")
		return err
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func mustLoadAuthority(t *testing.T, ctx context.Context, store Store, fence ProviderExecutionAuthorityFence) ProviderExecutionAuthority {
	t.Helper()
	authority, err := LoadProviderExecutionAuthority(ctx, store, fence.ProjectID, fence.RunID, fence.AttemptID)
	if err != nil {
		t.Fatalf("LoadProviderExecutionAuthority: %v", err)
	}
	return authority
}

func TestPersistProviderExecutionAuthority_ExactlyOnceConflict(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedAuthorityProject(t, ctx, store, "proj-conflict")
	started := fixedNow()
	base := ProviderExecutionAuthority{
		ProjectID: "proj-conflict", RunID: "run-c", AttemptID: "job-c1",
		ProviderPID: 4242, ProviderPGID: 4242,
		ProcessBirthIdentity: "birth-exact", ExecutableIdentity: "/bin/sleep",
		OwnerID: "owner-c1", ClaimGeneration: 1,
		WorktreePath: "/tmp/wt-c", LogPath: "/tmp/log-c",
	}
	a1, err := PersistProviderExecutionAuthority(ctx, store, base, started)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a1.SpawnPhase != SpawnPhaseAuthorityPersisted || a1.RecordVersion != 1 {
		t.Fatalf("create phase/ver=%q/%d", a1.SpawnPhase, a1.RecordVersion)
	}
	// Snapshot DB bytes
	dbBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Exact replay → zero mutation
	a2, err := PersistProviderExecutionAuthority(ctx, store, base, started.Add(time.Second))
	if err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	if a2.RecordVersion != a1.RecordVersion {
		t.Fatalf("record_version mutated %d → %d", a1.RecordVersion, a2.RecordVersion)
	}
	dbAfter, _ := os.ReadFile(path)
	if string(dbBefore) != string(dbAfter) {
		t.Fatal("DB bytes mutated on exact idempotent Persist")
	}
	// Divergent PID → fail closed, no mutation
	div := base
	div.ProviderPID = 9999
	if _, err := PersistProviderExecutionAuthority(ctx, store, div, started.Add(2*time.Second)); err == nil {
		t.Fatal("want divergent PID fail")
	}
	db2, _ := os.ReadFile(path)
	if string(dbBefore) != string(db2) {
		t.Fatal("DB mutated on divergent conflict")
	}
	// Explicit legacy_unknown rejected
	leg := base
	leg.AttemptID = "job-c2"
	leg.SpawnPhase = SpawnPhaseLegacyUnknown
	if _, err := PersistProviderExecutionAuthority(ctx, store, leg, started); err == nil || !strings.Contains(err.Error(), "legacy_unknown") {
		t.Fatalf("explicit legacy: %v", err)
	}
	// Advanced existing row: create authority_persisted then transition; Persist conflict must fail
	adv := base
	adv.AttemptID = "job-c3"
	adv.OwnerID = "owner-c3"
	adv.ProviderPID = 4343
	adv.ProviderPGID = 4343
	adv.ProcessBirthIdentity = "birth-adv"
	a3, err := PersistProviderExecutionAuthority(ctx, store, adv, started)
	if err != nil {
		t.Fatal(err)
	}
	fence := ProviderExecutionAuthorityFence{
		ProjectID: a3.ProjectID, RunID: a3.RunID, AttemptID: a3.AttemptID,
		OwnerID: a3.OwnerID, ClaimGeneration: a3.ClaimGeneration,
	}
	if err := TransitionProviderExecutionSpawnPhase(ctx, store, fence, started.Add(time.Second), SpawnPhasePIDEventPersisted); err != nil {
		t.Fatal(err)
	}
	dbAdv, _ := os.ReadFile(path)
	if _, err := PersistProviderExecutionAuthority(ctx, store, adv, started.Add(2*time.Second)); err == nil {
		t.Fatal("want fail on advanced phase conflict")
	}
	dbAdv2, _ := os.ReadFile(path)
	if string(dbAdv) != string(dbAdv2) {
		t.Fatal("DB mutated on advanced conflict")
	}
	// Legacy existing row: insert with legacy_unknown, Persist must fail closed
	ts := storageTimestamp(started)
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO provider_execution_authorities(
			authority_id, schema_version, record_version, project_id, run_id, attempt_id,
			provider_pid, provider_pgid, process_birth_identity, executable_identity, owner_id, claim_generation,
			started_at, heartbeat_at, worktree_path, log_path, identity_ambiguous, ambiguity_reason,
			spawn_phase, created_at, updated_at
		) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?, ?)`,
			"pauth_leg_conflict", ProviderExecutionAuthoritySchema,
			"proj-conflict", "run-c", "job-leg",
			5005, 5005, "birth-leg", "/bin/true", "owner-leg", int64(1),
			ts, ts, "/tmp/wtl", "/tmp/logl",
			SpawnPhaseLegacyUnknown, ts, ts)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dbLeg, _ := os.ReadFile(path)
	legReplay := ProviderExecutionAuthority{
		ProjectID: "proj-conflict", RunID: "run-c", AttemptID: "job-leg",
		ProviderPID: 5005, ProviderPGID: 5005,
		ProcessBirthIdentity: "birth-leg", ExecutableIdentity: "/bin/true",
		OwnerID: "owner-leg", ClaimGeneration: 1,
		WorktreePath: "/tmp/wtl", LogPath: "/tmp/logl",
		AuthorityID: "pauth_leg_conflict", SchemaVersion: ProviderExecutionAuthoritySchema,
	}
	if _, err := PersistProviderExecutionAuthority(ctx, store, legReplay, started); err == nil {
		t.Fatal("want fail on legacy existing conflict")
	}
	dbLeg2, _ := os.ReadFile(path)
	if string(dbLeg) != string(dbLeg2) {
		t.Fatal("DB mutated on legacy conflict")
	}
}

// TestPersistProviderExecutionAuthority_ConflictTampers_ZeroMutation proves
// conflict idempotency rejects RecordVersion!=1, contradictory TerminalState,
// and AmbiguityReason mismatch (when IdentityAmbiguous), each with zero DB mutation.
// Replay does not depend on regenerated timestamps.
func TestPersistProviderExecutionAuthority_ConflictTampers_ZeroMutation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedAuthorityProject(t, ctx, store, "proj-tamper")
	started := fixedNow()

	// --- RecordVersion != 1 ---
	baseRV := ProviderExecutionAuthority{
		ProjectID: "proj-tamper", RunID: "run-t", AttemptID: "job-rv",
		ProviderPID: 6101, ProviderPGID: 6101,
		ProcessBirthIdentity: "birth-rv", ExecutableIdentity: "/bin/sleep",
		OwnerID: "owner-rv", ClaimGeneration: 1,
		WorktreePath: "/tmp/wt-rv", LogPath: "/tmp/log-rv",
	}
	if _, err := PersistProviderExecutionAuthority(ctx, store, baseRV, started); err != nil {
		t.Fatalf("create rv: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_execution_authorities SET record_version = 2
			WHERE project_id = ? AND run_id = ? AND attempt_id = ?`,
			"proj-tamper", "run-t", "job-rv")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dbRV, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PersistProviderExecutionAuthority(ctx, store, baseRV, started.Add(time.Hour)); err == nil {
		t.Fatal("want fail on RecordVersion!=1 conflict")
	} else if !strings.Contains(err.Error(), "record_version") {
		t.Fatalf("want record_version error, got %v", err)
	}
	dbRV2, _ := os.ReadFile(path)
	if string(dbRV) != string(dbRV2) {
		t.Fatal("DB mutated on RecordVersion!=1 conflict")
	}
	loadedRV := mustLoadAuthority(t, ctx, store, ProviderExecutionAuthorityFence{
		ProjectID: "proj-tamper", RunID: "run-t", AttemptID: "job-rv",
		OwnerID: "owner-rv", ClaimGeneration: 1,
	})
	if loadedRV.RecordVersion != 2 {
		t.Fatalf("record_version after reject=%d want 2 (unchanged)", loadedRV.RecordVersion)
	}

	// --- nonempty TerminalState while still authority_persisted / incomplete ---
	baseTS := ProviderExecutionAuthority{
		ProjectID: "proj-tamper", RunID: "run-t", AttemptID: "job-ts",
		ProviderPID: 6102, ProviderPGID: 6102,
		ProcessBirthIdentity: "birth-ts", ExecutableIdentity: "/bin/sleep",
		OwnerID: "owner-ts", ClaimGeneration: 1,
		WorktreePath: "/tmp/wt-ts", LogPath: "/tmp/log-ts",
	}
	if _, err := PersistProviderExecutionAuthority(ctx, store, baseTS, started); err != nil {
		t.Fatalf("create ts: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_execution_authorities SET terminal_state = 'failed'
			WHERE project_id = ? AND run_id = ? AND attempt_id = ?`,
			"proj-tamper", "run-t", "job-ts")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dbTS, _ := os.ReadFile(path)
	if _, err := PersistProviderExecutionAuthority(ctx, store, baseTS, started.Add(2*time.Hour)); err == nil {
		t.Fatal("want fail on TerminalState conflict")
	} else if !strings.Contains(err.Error(), "TerminalState") && !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("want TerminalState error, got %v", err)
	}
	dbTS2, _ := os.ReadFile(path)
	if string(dbTS) != string(dbTS2) {
		t.Fatal("DB mutated on TerminalState conflict")
	}
	loadedTS := mustLoadAuthority(t, ctx, store, ProviderExecutionAuthorityFence{
		ProjectID: "proj-tamper", RunID: "run-t", AttemptID: "job-ts",
		OwnerID: "owner-ts", ClaimGeneration: 1,
	})
	if loadedTS.TerminalState != "failed" {
		t.Fatalf("terminal_state after reject=%q want failed (unchanged)", loadedTS.TerminalState)
	}

	// --- IdentityAmbiguous + AmbiguityReason mismatch ---
	baseAR := ProviderExecutionAuthority{
		ProjectID: "proj-tamper", RunID: "run-t", AttemptID: "job-ar",
		ProviderPID: 6103, ProviderPGID: 0, // forces IdentityAmbiguous
		ProcessBirthIdentity: "", ExecutableIdentity: "",
		OwnerID: "owner-ar", ClaimGeneration: 1,
		WorktreePath: "/tmp/wt-ar", LogPath: "/tmp/log-ar",
		IdentityAmbiguous: true, AmbiguityReason: "reason-original",
	}
	aAR, err := PersistProviderExecutionAuthority(ctx, store, baseAR, started)
	if err != nil {
		t.Fatalf("create ar: %v", err)
	}
	if !aAR.IdentityAmbiguous || aAR.AmbiguityReason != "reason-original" {
		// normalize may overwrite empty birth with incomplete-process-identity if we passed empty reason
		// We passed reason-original with incomplete identity — should stick.
		if !aAR.IdentityAmbiguous {
			t.Fatalf("want IdentityAmbiguous: %#v", aAR)
		}
	}
	// Exact replay with same reason must be idempotent (timestamp independent).
	dbARExact, _ := os.ReadFile(path)
	if _, err := PersistProviderExecutionAuthority(ctx, store, baseAR, started.Add(3*time.Hour)); err != nil {
		t.Fatalf("exact ambiguous replay: %v", err)
	}
	dbARExact2, _ := os.ReadFile(path)
	if string(dbARExact) != string(dbARExact2) {
		t.Fatal("DB mutated on exact IdentityAmbiguous replay")
	}
	// Tamper reason only.
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_execution_authorities SET ambiguity_reason = 'reason-tampered'
			WHERE project_id = ? AND run_id = ? AND attempt_id = ?`,
			"proj-tamper", "run-t", "job-ar")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dbAR, _ := os.ReadFile(path)
	if _, err := PersistProviderExecutionAuthority(ctx, store, baseAR, started.Add(4*time.Hour)); err == nil {
		t.Fatal("want fail on AmbiguityReason mismatch")
	} else if !strings.Contains(err.Error(), "ambiguity_reason") {
		t.Fatalf("want ambiguity_reason error, got %v", err)
	}
	dbAR2, _ := os.ReadFile(path)
	if string(dbAR) != string(dbAR2) {
		t.Fatal("DB mutated on AmbiguityReason conflict")
	}
	loadedAR := mustLoadAuthority(t, ctx, store, ProviderExecutionAuthorityFence{
		ProjectID: "proj-tamper", RunID: "run-t", AttemptID: "job-ar",
		OwnerID: "owner-ar", ClaimGeneration: 1,
	})
	if loadedAR.AmbiguityReason != "reason-tampered" {
		t.Fatalf("ambiguity_reason after reject=%q want reason-tampered", loadedAR.AmbiguityReason)
	}

	// --- RecordVersion 0 (zero) also rejected ---
	baseZ := ProviderExecutionAuthority{
		ProjectID: "proj-tamper", RunID: "run-t", AttemptID: "job-z",
		ProviderPID: 6104, ProviderPGID: 6104,
		ProcessBirthIdentity: "birth-z", ExecutableIdentity: "/bin/true",
		OwnerID: "owner-z", ClaimGeneration: 1,
		WorktreePath: "/tmp/wt-z", LogPath: "/tmp/log-z",
	}
	if _, err := PersistProviderExecutionAuthority(ctx, store, baseZ, started); err != nil {
		t.Fatalf("create z: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_execution_authorities SET record_version = 0
			WHERE project_id = ? AND run_id = ? AND attempt_id = ?`,
			"proj-tamper", "run-t", "job-z")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dbZ, _ := os.ReadFile(path)
	if _, err := PersistProviderExecutionAuthority(ctx, store, baseZ, started.Add(5*time.Hour)); err == nil {
		t.Fatal("want fail on RecordVersion=0")
	}
	dbZ2, _ := os.ReadFile(path)
	if string(dbZ) != string(dbZ2) {
		t.Fatal("DB mutated on RecordVersion=0 conflict")
	}
}
