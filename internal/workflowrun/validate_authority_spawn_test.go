package workflowrun_test

import (
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// TestValidateAuthorityMatchesSpawn_PrePIDStrict rejects pre-PID rows that are
// not a fresh authority_persisted create at RecordVersion exactly 1, and rows
// with contradictory TerminalState. Used before PID append.
func TestValidateAuthorityMatchesSpawn_PrePIDStrict(t *testing.T) {
	ps := workflowrun.ProcessStart{
		PID:                  4242,
		PGID:                 4242,
		ProcessBirthIdentity: "birth-v",
		ExecutableIdentity:   "/bin/sleep",
		WorktreePath:         "/tmp/wt-v",
		LogPath:              "/tmp/log-v",
	}
	base := storage.ProviderExecutionAuthority{
		AuthorityID:          "pauth_validate_spawn",
		SchemaVersion:        storage.ProviderExecutionAuthoritySchema,
		RecordVersion:        1,
		ProjectID:            "proj-v",
		RunID:                "run-v",
		AttemptID:            "att-v",
		ProviderPID:          4242,
		ProviderPGID:         4242,
		ProcessBirthIdentity: "birth-v",
		ExecutableIdentity:   "/bin/sleep",
		OwnerID:              "owner-v",
		ClaimGeneration:      1,
		StartedAt:            "2026-01-01T00:00:00Z",
		WorktreePath:         "/tmp/wt-v",
		LogPath:              "/tmp/log-v",
		SpawnPhase:           storage.SpawnPhaseAuthorityPersisted,
	}
	if err := workflowrun.ValidateAuthorityMatchesSpawn(base, ps, 1, "att-v", "owner-v"); err != nil {
		t.Fatalf("want accept clean pre-PID: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*storage.ProviderExecutionAuthority)
		wantSub string
	}{
		{
			name: "record_version_2",
			mutate: func(a *storage.ProviderExecutionAuthority) {
				a.RecordVersion = 2
			},
			wantSub: "record_version",
		},
		{
			name: "record_version_0",
			mutate: func(a *storage.ProviderExecutionAuthority) {
				a.RecordVersion = 0
			},
			wantSub: "record_version",
		},
		{
			name: "terminal_state_nonempty",
			mutate: func(a *storage.ProviderExecutionAuthority) {
				a.TerminalState = "failed"
			},
			wantSub: "TerminalState",
		},
		{
			name: "spawn_phase_advanced",
			mutate: func(a *storage.ProviderExecutionAuthority) {
				a.SpawnPhase = storage.SpawnPhasePIDEventPersisted
			},
			wantSub: "spawn_phase",
		},
		{
			name: "legacy_unknown",
			mutate: func(a *storage.ProviderExecutionAuthority) {
				a.SpawnPhase = storage.SpawnPhaseLegacyUnknown
			},
			wantSub: "legacy_unknown",
		},
		{
			name: "completed_at",
			mutate: func(a *storage.ProviderExecutionAuthority) {
				a.CompletedAt = "2026-01-01T00:01:00Z"
			},
			wantSub: "completed",
		},
		{
			name: "identity_ambiguous",
			mutate: func(a *storage.ProviderExecutionAuthority) {
				a.IdentityAmbiguous = true
				a.AmbiguityReason = "note"
			},
			wantSub: "ambiguous",
		},
		{
			name: "pid_mismatch",
			mutate: func(a *storage.ProviderExecutionAuthority) {
				a.ProviderPID = 9999
			},
			wantSub: "pid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			tc.mutate(&a)
			err := workflowrun.ValidateAuthorityMatchesSpawn(a, ps, 1, "att-v", "owner-v")
			if err == nil {
				t.Fatal("want reject")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}
