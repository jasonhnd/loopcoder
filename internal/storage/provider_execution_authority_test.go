package storage

import (
	"context"
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
	if err := CompleteProviderExecutionAuthority(ctx, store, stale, started.Add(3*time.Minute), "failed"); err == nil || !strings.Contains(err.Error(), "stale owner or generation") {
		t.Fatalf("stale completion error = %v, want stale owner/generation", err)
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
