package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const ProviderExecutionAuthoritySchema = "loopcoder.provider_execution_authority.v1"

type ProviderExecutionAuthority struct {
	AuthorityID          string
	SchemaVersion        string
	RecordVersion        int
	ProjectID            string
	RunID                string
	AttemptID            string
	ProviderPID          int
	ProviderPGID         int
	ProcessBirthIdentity string
	ExecutableIdentity   string
	OwnerID              string
	ClaimGeneration      int64
	StartedAt            string
	HeartbeatAt          string
	WorktreePath         string
	LogPath              string
	IdentityAmbiguous    bool
	AmbiguityReason      string
	CompletedAt          string
	TerminalState        string
	CreatedAt            string
	UpdatedAt            string
}

type ProviderExecutionAuthorityFence struct {
	ProjectID       string
	RunID           string
	AttemptID       string
	OwnerID         string
	ClaimGeneration int64
}

func PersistProviderExecutionAuthority(ctx context.Context, store Store, authority ProviderExecutionAuthority, at time.Time) (ProviderExecutionAuthority, error) {
	if store == nil {
		return ProviderExecutionAuthority{}, fmt.Errorf("provider execution authority store is required")
	}
	authority = normalizeProviderExecutionAuthority(authority, at)
	if err := validateProviderExecutionAuthority(authority); err != nil {
		return ProviderExecutionAuthority{}, err
	}
	err := store.WithWriteTx(ctx, func(tx Tx) error {
		result, err := tx.Exec(ctx, `INSERT INTO provider_execution_authorities(
				authority_id, schema_version, record_version, project_id, run_id, attempt_id,
				provider_pid, provider_pgid, process_birth_identity, executable_identity, owner_id, claim_generation,
				started_at, heartbeat_at, worktree_path, log_path, identity_ambiguous, ambiguity_reason, created_at, updated_at
			) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id, run_id, attempt_id) DO UPDATE SET
				record_version = provider_execution_authorities.record_version + 1,
				provider_pid = excluded.provider_pid,
				provider_pgid = excluded.provider_pgid,
				process_birth_identity = excluded.process_birth_identity,
				executable_identity = excluded.executable_identity,
				started_at = excluded.started_at,
				heartbeat_at = excluded.heartbeat_at,
				worktree_path = excluded.worktree_path,
				log_path = excluded.log_path,
				identity_ambiguous = excluded.identity_ambiguous,
				ambiguity_reason = excluded.ambiguity_reason,
				updated_at = excluded.updated_at
			WHERE provider_execution_authorities.owner_id = excluded.owner_id
				AND provider_execution_authorities.claim_generation = excluded.claim_generation
				AND provider_execution_authorities.completed_at IS NULL`,
			authority.AuthorityID, authority.SchemaVersion, authority.ProjectID, authority.RunID, authority.AttemptID,
			authority.ProviderPID, authority.ProviderPGID, authority.ProcessBirthIdentity, authority.ExecutableIdentity,
			authority.OwnerID, authority.ClaimGeneration, authority.StartedAt, authority.HeartbeatAt,
			authority.WorktreePath, authority.LogPath, boolInt(authority.IdentityAmbiguous), authority.AmbiguityReason,
			authority.CreatedAt, authority.UpdatedAt)
		if err != nil {
			return fmt.Errorf("persist provider execution authority: %w", err)
		}
		return requireProviderAuthorityRowsAffected(result, "persist provider execution authority")
	})
	if err != nil {
		return ProviderExecutionAuthority{}, err
	}
	return LoadProviderExecutionAuthority(ctx, store, authority.ProjectID, authority.RunID, authority.AttemptID)
}

func HeartbeatProviderExecutionAuthority(ctx context.Context, store Store, fence ProviderExecutionAuthorityFence, at time.Time) error {
	return updateProviderExecutionAuthorityFenced(ctx, store, fence, at, "", false)
}

func CompleteProviderExecutionAuthority(ctx context.Context, store Store, fence ProviderExecutionAuthorityFence, at time.Time, terminalState string) error {
	terminalState = strings.TrimSpace(terminalState)
	if terminalState == "" {
		return fmt.Errorf("provider execution authority terminal state is required")
	}
	return updateProviderExecutionAuthorityFenced(ctx, store, fence, at, terminalState, true)
}

func LoadProviderExecutionAuthority(ctx context.Context, store Store, projectID, runID, attemptID string) (ProviderExecutionAuthority, error) {
	if store == nil {
		return ProviderExecutionAuthority{}, fmt.Errorf("provider execution authority store is required")
	}
	var authority ProviderExecutionAuthority
	err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT
				authority_id, schema_version, record_version, project_id, run_id, attempt_id,
				provider_pid, provider_pgid, process_birth_identity, executable_identity, owner_id, claim_generation,
				started_at, heartbeat_at, worktree_path, log_path, identity_ambiguous, ambiguity_reason,
				COALESCE(completed_at, ''), terminal_state, created_at, updated_at
			FROM provider_execution_authorities
			WHERE project_id = ? AND run_id = ? AND attempt_id = ?`,
			strings.TrimSpace(projectID), strings.TrimSpace(runID), strings.TrimSpace(attemptID)).Scan(
			&authority.AuthorityID, &authority.SchemaVersion, &authority.RecordVersion, &authority.ProjectID, &authority.RunID, &authority.AttemptID,
			&authority.ProviderPID, &authority.ProviderPGID, &authority.ProcessBirthIdentity, &authority.ExecutableIdentity, &authority.OwnerID, &authority.ClaimGeneration,
			&authority.StartedAt, &authority.HeartbeatAt, &authority.WorktreePath, &authority.LogPath, &authority.IdentityAmbiguous, &authority.AmbiguityReason,
			&authority.CompletedAt, &authority.TerminalState, &authority.CreatedAt, &authority.UpdatedAt)
	})
	if err != nil {
		return ProviderExecutionAuthority{}, err
	}
	return authority, nil
}

func ListProviderExecutionAuthorities(ctx context.Context, store Store, projectID, runID string) ([]ProviderExecutionAuthority, error) {
	if store == nil {
		return nil, fmt.Errorf("provider execution authority store is required")
	}
	projectID = strings.TrimSpace(projectID)
	runID = strings.TrimSpace(runID)
	if projectID == "" {
		return nil, fmt.Errorf("provider execution authority project_id is required")
	}
	query := `SELECT
			authority_id, schema_version, record_version, project_id, run_id, attempt_id,
			provider_pid, provider_pgid, process_birth_identity, executable_identity, owner_id, claim_generation,
			started_at, heartbeat_at, worktree_path, log_path, identity_ambiguous, ambiguity_reason,
			COALESCE(completed_at, ''), terminal_state, created_at, updated_at
		FROM provider_execution_authorities
		WHERE project_id = ?`
	args := []any{projectID}
	if runID != "" {
		query += ` AND run_id = ?`
		args = append(args, runID)
	}
	query += ` ORDER BY started_at, run_id, attempt_id`

	var authorities []ProviderExecutionAuthority
	err := store.WithTx(ctx, func(tx Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var authority ProviderExecutionAuthority
			if err := rows.Scan(
				&authority.AuthorityID, &authority.SchemaVersion, &authority.RecordVersion, &authority.ProjectID, &authority.RunID, &authority.AttemptID,
				&authority.ProviderPID, &authority.ProviderPGID, &authority.ProcessBirthIdentity, &authority.ExecutableIdentity, &authority.OwnerID, &authority.ClaimGeneration,
				&authority.StartedAt, &authority.HeartbeatAt, &authority.WorktreePath, &authority.LogPath, &authority.IdentityAmbiguous, &authority.AmbiguityReason,
				&authority.CompletedAt, &authority.TerminalState, &authority.CreatedAt, &authority.UpdatedAt,
			); err != nil {
				return err
			}
			authorities = append(authorities, authority)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return authorities, nil
}

func ValidateProviderExecutionAuthorityOwner(ctx context.Context, store Store, fence ProviderExecutionAuthorityFence, at time.Time) error {
	if store == nil {
		return fmt.Errorf("provider execution authority store is required")
	}
	fence = normalizeProviderExecutionAuthorityFence(fence)
	if err := validateProviderExecutionAuthorityFence(fence); err != nil {
		return err
	}
	now := storageTimestamp(at)
	return store.WithTx(ctx, func(tx Tx) error {
		var active int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*)
			FROM agent_ownership_locks
			WHERE project_id = ? AND delivery_run_id = ? AND run_id = ? AND child_agent_id = ?
				AND claim_generation = ? AND state = ? AND lease_expires_at > ?`,
			fence.ProjectID, fence.RunID, fence.RunID, fence.OwnerID, fence.ClaimGeneration, OwnershipStateHeld, now).Scan(&active); err != nil {
			return fmt.Errorf("validate provider execution authority owner: %w", err)
		}
		if active == 0 {
			return ErrOwnershipStale
		}
		return nil
	})
}

func updateProviderExecutionAuthorityFenced(ctx context.Context, store Store, fence ProviderExecutionAuthorityFence, at time.Time, terminalState string, complete bool) error {
	if store == nil {
		return fmt.Errorf("provider execution authority store is required")
	}
	fence = normalizeProviderExecutionAuthorityFence(fence)
	if err := validateProviderExecutionAuthorityFence(fence); err != nil {
		return err
	}
	now := storageTimestamp(at)
	return store.WithWriteTx(ctx, func(tx Tx) error {
		query := `UPDATE provider_execution_authorities
			SET heartbeat_at = ?, updated_at = ?
			WHERE project_id = ? AND run_id = ? AND attempt_id = ? AND owner_id = ? AND claim_generation = ? AND completed_at IS NULL`
		args := []any{now, now, fence.ProjectID, fence.RunID, fence.AttemptID, fence.OwnerID, fence.ClaimGeneration}
		if complete {
			query = `UPDATE provider_execution_authorities
				SET heartbeat_at = ?, completed_at = ?, terminal_state = ?, updated_at = ?
				WHERE project_id = ? AND run_id = ? AND attempt_id = ? AND owner_id = ? AND claim_generation = ? AND completed_at IS NULL`
			args = []any{now, now, strings.TrimSpace(terminalState), now, fence.ProjectID, fence.RunID, fence.AttemptID, fence.OwnerID, fence.ClaimGeneration}
		}
		result, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("update provider execution authority: %w", err)
		}
		return requireProviderAuthorityRowsAffected(result, "update provider execution authority")
	})
}

func normalizeProviderExecutionAuthority(authority ProviderExecutionAuthority, at time.Time) ProviderExecutionAuthority {
	now := storageTimestamp(at)
	authority.SchemaVersion = firstNonEmptyStorage(authority.SchemaVersion, ProviderExecutionAuthoritySchema)
	authority.ProjectID = strings.TrimSpace(authority.ProjectID)
	authority.RunID = strings.TrimSpace(authority.RunID)
	authority.AttemptID = strings.TrimSpace(authority.AttemptID)
	authority.ProcessBirthIdentity = strings.TrimSpace(authority.ProcessBirthIdentity)
	authority.ExecutableIdentity = strings.TrimSpace(authority.ExecutableIdentity)
	authority.OwnerID = strings.TrimSpace(authority.OwnerID)
	authority.StartedAt = firstNonEmptyStorage(strings.TrimSpace(authority.StartedAt), now)
	authority.HeartbeatAt = firstNonEmptyStorage(strings.TrimSpace(authority.HeartbeatAt), authority.StartedAt)
	authority.WorktreePath = strings.TrimSpace(authority.WorktreePath)
	authority.LogPath = strings.TrimSpace(authority.LogPath)
	authority.AmbiguityReason = strings.TrimSpace(authority.AmbiguityReason)
	authority.CreatedAt = firstNonEmptyStorage(strings.TrimSpace(authority.CreatedAt), now)
	authority.UpdatedAt = firstNonEmptyStorage(strings.TrimSpace(authority.UpdatedAt), now)
	if authority.AuthorityID == "" {
		authority.AuthorityID = "pauth_" + hashStringsStorage(authority.ProjectID, authority.RunID, authority.AttemptID)[:24]
	}
	if authority.ProviderPGID <= 0 || authority.ProcessBirthIdentity == "" || authority.ExecutableIdentity == "" {
		authority.IdentityAmbiguous = true
		if authority.AmbiguityReason == "" {
			authority.AmbiguityReason = "incomplete-process-identity"
		}
	}
	return authority
}

func normalizeProviderExecutionAuthorityFence(fence ProviderExecutionAuthorityFence) ProviderExecutionAuthorityFence {
	fence.ProjectID = strings.TrimSpace(fence.ProjectID)
	fence.RunID = strings.TrimSpace(fence.RunID)
	fence.AttemptID = strings.TrimSpace(fence.AttemptID)
	fence.OwnerID = strings.TrimSpace(fence.OwnerID)
	return fence
}

func validateProviderExecutionAuthority(authority ProviderExecutionAuthority) error {
	if authority.ProjectID == "" || authority.RunID == "" || authority.AttemptID == "" || authority.OwnerID == "" {
		return fmt.Errorf("provider execution authority project_id, run_id, attempt_id, and owner_id are required")
	}
	if authority.ProviderPID <= 0 {
		return fmt.Errorf("provider execution authority provider_pid must be positive")
	}
	if authority.ClaimGeneration <= 0 {
		return fmt.Errorf("provider execution authority claim_generation must be positive")
	}
	if authority.WorktreePath == "" || authority.LogPath == "" {
		return fmt.Errorf("provider execution authority worktree_path and log_path are required")
	}
	return nil
}

func validateProviderExecutionAuthorityFence(fence ProviderExecutionAuthorityFence) error {
	if fence.ProjectID == "" || fence.RunID == "" || fence.AttemptID == "" || fence.OwnerID == "" || fence.ClaimGeneration <= 0 {
		return fmt.Errorf("complete provider execution authority fence is required")
	}
	return nil
}

func requireProviderAuthorityRowsAffected(result sql.Result, operation string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", operation, err)
	}
	if affected != 1 {
		return fmt.Errorf("%s stale owner or generation", operation)
	}
	return nil
}

func storageTimestamp(at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func firstNonEmptyStorage(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
