package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const ProviderExecutionAuthoritySchema = "loopcoder.provider_execution_authority.v1"

// Typed spawn phases durable on the authority row (same write as create).
// legacy_unknown is the DB/migration default for pre-v33 or untyped rows — never auto-recoverable.
const (
	SpawnPhaseLegacyUnknown      = "legacy_unknown"
	SpawnPhaseAuthorityPersisted = "authority_persisted"
	SpawnPhasePIDEventPersisted  = "pid_event_persisted"
	SpawnPhasePIDEventFailed     = "pid_event_failed"
)

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
	// SpawnPhase is written in the same row/tx as Persist when creating a new
	// authority (must be authority_persisted explicitly). Migration default for
	// pre-v33 rows is legacy_unknown — never promoted to recoverable by COALESCE.
	// Transitions: authority_persisted → pid_event_persisted | pid_event_failed.
	SpawnPhase    string
	CompletedAt   string
	TerminalState string
	CreatedAt     string
	UpdatedAt     string
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
	// Persist creates ONLY authority_persisted. Explicit legacy_unknown is rejected.
	// Advanced phases are reached solely via TransitionProviderExecutionSpawnPhase.
	phaseIn := strings.TrimSpace(authority.SpawnPhase)
	switch phaseIn {
	case "", SpawnPhaseAuthorityPersisted:
		authority.SpawnPhase = SpawnPhaseAuthorityPersisted
	case SpawnPhaseLegacyUnknown:
		return ProviderExecutionAuthority{}, fmt.Errorf("provider execution authority spawn_phase %q rejected on Persist (explicit legacy_unknown not creatable)", phaseIn)
	case SpawnPhasePIDEventPersisted, SpawnPhasePIDEventFailed:
		return ProviderExecutionAuthority{}, fmt.Errorf("provider execution authority spawn_phase %q rejected on Persist (only authority_persisted creatable; use Transition)", phaseIn)
	default:
		return ProviderExecutionAuthority{}, fmt.Errorf("provider execution authority spawn_phase %q not writable by Persist", phaseIn)
	}
	if err := validateProviderExecutionAuthority(authority); err != nil {
		return ProviderExecutionAuthority{}, err
	}
	// Exactly-once create: INSERT … DO NOTHING. Conflict never rewrites identity or phase.
	// Idempotent only when existing row is incomplete authority_persisted and every
	// immutable field is byte/exact equal (owner/gen/process/paths/schema/id).
	err := store.WithWriteTx(ctx, func(tx Tx) error {
		result, err := tx.Exec(ctx, `INSERT INTO provider_execution_authorities(
				authority_id, schema_version, record_version, project_id, run_id, attempt_id,
				provider_pid, provider_pgid, process_birth_identity, executable_identity, owner_id, claim_generation,
				started_at, heartbeat_at, worktree_path, log_path, identity_ambiguous, ambiguity_reason,
				spawn_phase, created_at, updated_at
			) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id, run_id, attempt_id) DO NOTHING`,
			authority.AuthorityID, authority.SchemaVersion, authority.ProjectID, authority.RunID, authority.AttemptID,
			authority.ProviderPID, authority.ProviderPGID, authority.ProcessBirthIdentity, authority.ExecutableIdentity,
			authority.OwnerID, authority.ClaimGeneration, authority.StartedAt, authority.HeartbeatAt,
			authority.WorktreePath, authority.LogPath, boolInt(authority.IdentityAmbiguous), authority.AmbiguityReason,
			authority.SpawnPhase, authority.CreatedAt, authority.UpdatedAt)
		if err != nil {
			return fmt.Errorf("persist provider execution authority: %w", err)
		}
		affected, aerr := result.RowsAffected()
		if aerr != nil {
			return fmt.Errorf("persist provider execution authority rows affected: %w", aerr)
		}
		if affected == 1 {
			return nil // created
		}
		// Conflict: load existing in same tx and compare for idempotent zero-mutation return.
		var existing ProviderExecutionAuthority
		var ambig int
		scanErr := tx.QueryRow(ctx, `SELECT
				authority_id, schema_version, record_version, project_id, run_id, attempt_id,
				provider_pid, provider_pgid, process_birth_identity, executable_identity, owner_id, claim_generation,
				started_at, heartbeat_at, worktree_path, log_path, identity_ambiguous, ambiguity_reason,
				COALESCE(spawn_phase, ''), COALESCE(completed_at, ''), terminal_state, created_at, updated_at
			FROM provider_execution_authorities
			WHERE project_id = ? AND run_id = ? AND attempt_id = ?`,
			authority.ProjectID, authority.RunID, authority.AttemptID).Scan(
			&existing.AuthorityID, &existing.SchemaVersion, &existing.RecordVersion, &existing.ProjectID, &existing.RunID, &existing.AttemptID,
			&existing.ProviderPID, &existing.ProviderPGID, &existing.ProcessBirthIdentity, &existing.ExecutableIdentity, &existing.OwnerID, &existing.ClaimGeneration,
			&existing.StartedAt, &existing.HeartbeatAt, &existing.WorktreePath, &existing.LogPath, &ambig, &existing.AmbiguityReason,
			&existing.SpawnPhase, &existing.CompletedAt, &existing.TerminalState, &existing.CreatedAt, &existing.UpdatedAt)
		if scanErr != nil {
			return fmt.Errorf("persist provider execution authority conflict load: %w", scanErr)
		}
		existing.IdentityAmbiguous = ambig != 0
		if strings.TrimSpace(existing.SpawnPhase) == "" {
			existing.SpawnPhase = SpawnPhaseLegacyUnknown
		}
		if err := authorityPersistConflictExactMatch(existing, authority); err != nil {
			return err
		}
		// Exact idempotent replay — zero mutation (do not UPDATE).
		return nil
	})
	if err != nil {
		return ProviderExecutionAuthority{}, err
	}
	return LoadProviderExecutionAuthority(ctx, store, authority.ProjectID, authority.RunID, authority.AttemptID)
}

// authorityPersistConflictExactMatch allows DO NOTHING conflict only when the
// existing row is incomplete authority_persisted at RecordVersion==1 and every
// immutable field matches. Timestamps are not compared (replay-safe).
func authorityPersistConflictExactMatch(existing, want ProviderExecutionAuthority) error {
	if strings.TrimSpace(existing.CompletedAt) != "" {
		return fmt.Errorf("provider execution authority already completed (fail closed on Persist conflict)")
	}
	if strings.TrimSpace(existing.TerminalState) != "" {
		return fmt.Errorf("provider execution authority conflict with nonempty TerminalState %q (fail closed)", existing.TerminalState)
	}
	if existing.RecordVersion != 1 {
		return fmt.Errorf("provider execution authority conflict record_version %d want 1 for pre-PID authority_persisted (fail closed)", existing.RecordVersion)
	}
	phase := strings.TrimSpace(existing.SpawnPhase)
	if phase == "" {
		phase = SpawnPhaseLegacyUnknown
	}
	if phase == SpawnPhaseLegacyUnknown {
		return fmt.Errorf("provider execution authority conflict with legacy_unknown row (fail closed; never rewrite)")
	}
	if phase != SpawnPhaseAuthorityPersisted {
		return fmt.Errorf("provider execution authority conflict with spawn_phase %q (fail closed; never rewrite advanced phase)", phase)
	}
	if strings.TrimSpace(existing.OwnerID) != strings.TrimSpace(want.OwnerID) ||
		existing.ClaimGeneration != want.ClaimGeneration {
		return fmt.Errorf("provider execution authority conflict owner/gen mismatch (fail closed)")
	}
	if strings.TrimSpace(existing.AuthorityID) != strings.TrimSpace(want.AuthorityID) {
		return fmt.Errorf("provider execution authority conflict authority_id mismatch (fail closed)")
	}
	if strings.TrimSpace(existing.SchemaVersion) != strings.TrimSpace(want.SchemaVersion) {
		return fmt.Errorf("provider execution authority conflict schema_version mismatch (fail closed)")
	}
	if existing.ProviderPID != want.ProviderPID || existing.ProviderPGID != want.ProviderPGID {
		return fmt.Errorf("provider execution authority conflict pid/pgid mismatch (fail closed)")
	}
	if strings.TrimSpace(existing.ProcessBirthIdentity) != strings.TrimSpace(want.ProcessBirthIdentity) ||
		strings.TrimSpace(existing.ExecutableIdentity) != strings.TrimSpace(want.ExecutableIdentity) {
		return fmt.Errorf("provider execution authority conflict process identity mismatch (fail closed)")
	}
	if strings.TrimSpace(existing.WorktreePath) != strings.TrimSpace(want.WorktreePath) ||
		strings.TrimSpace(existing.LogPath) != strings.TrimSpace(want.LogPath) {
		return fmt.Errorf("provider execution authority conflict worktree/log mismatch (fail closed)")
	}
	if existing.IdentityAmbiguous != want.IdentityAmbiguous {
		return fmt.Errorf("provider execution authority conflict identity_ambiguous mismatch (fail closed)")
	}
	// AmbiguityReason compared always (including when IdentityAmbiguous is false).
	if strings.TrimSpace(existing.AmbiguityReason) != strings.TrimSpace(want.AmbiguityReason) {
		return fmt.Errorf("provider execution authority conflict ambiguity_reason mismatch (fail closed)")
	}
	return nil
}

// TransitionProviderExecutionSpawnPhase advances spawn_phase under owner/gen fence.
// Allowed: authority_persisted → pid_event_persisted | pid_event_failed;
// pid_event_failed / pid_event_persisted → same only (idempotent).
// Transitions FROM legacy_unknown or empty always fail closed.
func TransitionProviderExecutionSpawnPhase(ctx context.Context, store Store, fence ProviderExecutionAuthorityFence, at time.Time, toPhase string) error {
	if store == nil {
		return fmt.Errorf("provider execution authority store is required")
	}
	fence = normalizeProviderExecutionAuthorityFence(fence)
	if err := validateProviderExecutionAuthorityFence(fence); err != nil {
		return err
	}
	toPhase = strings.TrimSpace(toPhase)
	if toPhase != SpawnPhasePIDEventPersisted && toPhase != SpawnPhasePIDEventFailed {
		return fmt.Errorf("provider execution authority spawn_phase transition target %q invalid", toPhase)
	}
	now := storageTimestamp(at)
	return store.WithWriteTx(ctx, func(tx Tx) error {
		var current string
		err := tx.QueryRow(ctx, `SELECT COALESCE(spawn_phase, '') FROM provider_execution_authorities
			WHERE project_id = ? AND run_id = ? AND attempt_id = ? AND owner_id = ? AND claim_generation = ? AND completed_at IS NULL`,
			fence.ProjectID, fence.RunID, fence.AttemptID, fence.OwnerID, fence.ClaimGeneration).Scan(&current)
		if err != nil {
			return fmt.Errorf("load spawn_phase: %w", err)
		}
		current = strings.TrimSpace(current)
		if current == "" {
			current = SpawnPhaseLegacyUnknown
		}
		if current == SpawnPhaseLegacyUnknown {
			return fmt.Errorf("spawn_phase transition from legacy_unknown not allowed (fail closed)")
		}
		if current == toPhase {
			return nil // idempotent same-phase
		}
		// Legal transitions only from authority_persisted.
		if current != SpawnPhaseAuthorityPersisted {
			return fmt.Errorf("spawn_phase transition %q → %q not allowed", current, toPhase)
		}
		result, err := tx.Exec(ctx, `UPDATE provider_execution_authorities
			SET spawn_phase = ?, heartbeat_at = ?, updated_at = ?,
				record_version = record_version + 1
			WHERE project_id = ? AND run_id = ? AND attempt_id = ? AND owner_id = ? AND claim_generation = ?
				AND completed_at IS NULL AND spawn_phase = ?`,
			toPhase, now, now,
			fence.ProjectID, fence.RunID, fence.AttemptID, fence.OwnerID, fence.ClaimGeneration,
			current)
		if err != nil {
			return fmt.Errorf("transition spawn_phase: %w", err)
		}
		return requireProviderAuthorityRowsAffected(result, "transition spawn_phase")
	})
}

func HeartbeatProviderExecutionAuthority(ctx context.Context, store Store, fence ProviderExecutionAuthorityFence, at time.Time) error {
	return updateProviderExecutionAuthorityFenced(ctx, store, fence, at, "", false)
}

// CompleteProviderExecutionAuthority is the normal completion API.
// Requires spawn_phase=pid_event_persisted. legacy_unknown never completes.
// Pre-PID recovery must use CompleteProviderExecutionAuthorityPrePIDRecovery.
func CompleteProviderExecutionAuthority(ctx context.Context, store Store, fence ProviderExecutionAuthorityFence, at time.Time, terminalState string) error {
	terminalState = strings.TrimSpace(terminalState)
	if terminalState == "" {
		return fmt.Errorf("provider execution authority terminal state is required")
	}
	return completeProviderExecutionAuthorityFenced(ctx, store, fence, at, terminalState, []string{SpawnPhasePIDEventPersisted})
}

// CompleteProviderExecutionAuthorityPrePIDRecovery is the sole typed pre-PID
// completion path for authority_persisted or pid_event_failed (pid never landed).
// Never callable as a generic complete; rejects pid_event_persisted and legacy_unknown.
func CompleteProviderExecutionAuthorityPrePIDRecovery(ctx context.Context, store Store, fence ProviderExecutionAuthorityFence, at time.Time, terminalState string) error {
	terminalState = strings.TrimSpace(terminalState)
	if terminalState == "" {
		return fmt.Errorf("provider execution authority terminal state is required")
	}
	return completeProviderExecutionAuthorityFenced(ctx, store, fence, at, terminalState, []string{SpawnPhaseAuthorityPersisted, SpawnPhasePIDEventFailed})
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
				COALESCE(spawn_phase, ''),
				COALESCE(completed_at, ''), terminal_state, created_at, updated_at
			FROM provider_execution_authorities
			WHERE project_id = ? AND run_id = ? AND attempt_id = ?`,
			strings.TrimSpace(projectID), strings.TrimSpace(runID), strings.TrimSpace(attemptID)).Scan(
			&authority.AuthorityID, &authority.SchemaVersion, &authority.RecordVersion, &authority.ProjectID, &authority.RunID, &authority.AttemptID,
			&authority.ProviderPID, &authority.ProviderPGID, &authority.ProcessBirthIdentity, &authority.ExecutableIdentity, &authority.OwnerID, &authority.ClaimGeneration,
			&authority.StartedAt, &authority.HeartbeatAt, &authority.WorktreePath, &authority.LogPath, &authority.IdentityAmbiguous, &authority.AmbiguityReason,
			&authority.SpawnPhase,
			&authority.CompletedAt, &authority.TerminalState, &authority.CreatedAt, &authority.UpdatedAt)
	})
	if err != nil {
		return ProviderExecutionAuthority{}, err
	}
	// Empty/null spawn_phase on load is legacy_unknown — never promote to recoverable.
	if strings.TrimSpace(authority.SpawnPhase) == "" {
		authority.SpawnPhase = SpawnPhaseLegacyUnknown
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
			COALESCE(spawn_phase, ''),
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
				&authority.SpawnPhase,
				&authority.CompletedAt, &authority.TerminalState, &authority.CreatedAt, &authority.UpdatedAt,
			); err != nil {
				return err
			}
			if strings.TrimSpace(authority.SpawnPhase) == "" {
				authority.SpawnPhase = SpawnPhaseLegacyUnknown
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
			// Heartbeat-only path never completes; completion goes through completeProviderExecutionAuthorityFenced.
			return fmt.Errorf("updateProviderExecutionAuthorityFenced: complete=true is unsupported (use Complete APIs)")
		}
		result, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("update provider execution authority: %w", err)
		}
		return requireProviderAuthorityRowsAffected(result, "update provider execution authority")
	})
}

// completeProviderExecutionAuthorityFenced completes only when current spawn_phase
// is one of allowedPhases. legacy_unknown and empty never complete.
func completeProviderExecutionAuthorityFenced(ctx context.Context, store Store, fence ProviderExecutionAuthorityFence, at time.Time, terminalState string, allowedPhases []string) error {
	if store == nil {
		return fmt.Errorf("provider execution authority store is required")
	}
	fence = normalizeProviderExecutionAuthorityFence(fence)
	if err := validateProviderExecutionAuthorityFence(fence); err != nil {
		return err
	}
	if len(allowedPhases) == 0 {
		return fmt.Errorf("provider execution authority complete: allowed phases required")
	}
	now := storageTimestamp(at)
	return store.WithWriteTx(ctx, func(tx Tx) error {
		var currentPhase string
		err := tx.QueryRow(ctx, `SELECT COALESCE(spawn_phase, '') FROM provider_execution_authorities
			WHERE project_id = ? AND run_id = ? AND attempt_id = ? AND owner_id = ? AND claim_generation = ? AND completed_at IS NULL`,
			fence.ProjectID, fence.RunID, fence.AttemptID, fence.OwnerID, fence.ClaimGeneration).Scan(&currentPhase)
		if err != nil {
			return fmt.Errorf("load spawn_phase for complete: %w", err)
		}
		currentPhase = strings.TrimSpace(currentPhase)
		if currentPhase == "" {
			currentPhase = SpawnPhaseLegacyUnknown
		}
		if currentPhase == SpawnPhaseLegacyUnknown {
			return fmt.Errorf("provider execution authority complete from legacy_unknown not allowed (fail closed)")
		}
		allowed := false
		for _, p := range allowedPhases {
			if currentPhase == p {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("provider execution authority complete rejected for spawn_phase %q (allowed %v)", currentPhase, allowedPhases)
		}
		result, err := tx.Exec(ctx, `UPDATE provider_execution_authorities
			SET heartbeat_at = ?, completed_at = ?, terminal_state = ?, updated_at = ?,
				record_version = record_version + 1
			WHERE project_id = ? AND run_id = ? AND attempt_id = ? AND owner_id = ? AND claim_generation = ?
				AND completed_at IS NULL AND spawn_phase = ?`,
			now, now, strings.TrimSpace(terminalState), now,
			fence.ProjectID, fence.RunID, fence.AttemptID, fence.OwnerID, fence.ClaimGeneration,
			currentPhase)
		if err != nil {
			return fmt.Errorf("complete provider execution authority: %w", err)
		}
		return requireProviderAuthorityRowsAffected(result, "complete provider execution authority")
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
	// Do NOT default empty SpawnPhase to authority_persisted here — that would
	// promote legacy rows. Persist sets authority_persisted explicitly after normalize.
	authority.SpawnPhase = strings.TrimSpace(authority.SpawnPhase)
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
	// Persist validates after force-assigning authority_persisted only.
	switch authority.SpawnPhase {
	case SpawnPhaseAuthorityPersisted:
		// ok
	case "":
		// ok until Persist assigns authority_persisted
	default:
		return fmt.Errorf("provider execution authority spawn_phase %q invalid on Persist create", authority.SpawnPhase)
	}
	if strings.TrimSpace(authority.AuthorityID) == "" {
		return fmt.Errorf("provider execution authority authority_id is required")
	}
	if strings.TrimSpace(authority.SchemaVersion) != ProviderExecutionAuthoritySchema {
		return fmt.Errorf("provider execution authority schema_version must be %q", ProviderExecutionAuthoritySchema)
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
