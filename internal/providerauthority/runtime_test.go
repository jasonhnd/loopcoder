package providerauthority

import (
	"os"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestInspectClassifiesProviderAuthorityStates(t *testing.T) {
	identity, err := process.Snapshot(os.Getpid(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	base := storage.ProviderExecutionAuthority{
		RunID:                "run-state",
		AttemptID:            "job-state",
		ProviderPID:          identity.PID,
		ProviderPGID:         identity.PGID,
		ProcessBirthIdentity: identity.ProcessBirthIdentity,
		ExecutableIdentity:   identity.ExecutableIdentity,
		OwnerID:              "owner-state",
		ClaimGeneration:      1,
	}

	active := Inspect(base)
	if active.State != StateActive || !active.Verified {
		t.Fatalf("active = %#v, want verified active", active)
	}

	terminalRow := base
	terminalRow.CompletedAt = "2026-01-01T00:01:00Z"
	terminalRow.TerminalState = "succeeded"
	if got := Inspect(terminalRow); got.State != StateTerminal || got.Verified {
		t.Fatalf("terminal = %#v", got)
	}

	ambiguousRow := base
	ambiguousRow.ProcessBirthIdentity = ""
	ambiguousRow.IdentityAmbiguous = true
	if got := Inspect(ambiguousRow); got.State != StateAmbiguous || got.Verified {
		t.Fatalf("ambiguous = %#v", got)
	}

	staleRow := base
	staleRow.ProviderPID = 2147480000
	if got := Inspect(staleRow); got.State != StateStale || got.Verified {
		t.Fatalf("stale = %#v", got)
	}

	mismatchRow := base
	mismatchRow.ProcessBirthIdentity = "different-birth-identity"
	if got := Inspect(mismatchRow); got.State != StateIdentityMismatch || got.Verified {
		t.Fatalf("mismatch = %#v", got)
	}
}
