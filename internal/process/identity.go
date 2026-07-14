package process

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Identity struct {
	PID                  int
	PGID                 int
	ProcessBirthIdentity string
	ExecutableIdentity   string
	ObservedAt           time.Time
	Ambiguous            bool
	AmbiguityReason      string
}

func Snapshot(pid int, observedAt time.Time) (Identity, error) {
	if pid <= 0 {
		return Identity{}, fmt.Errorf("pid must be positive")
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	identity := Identity{
		PID:        pid,
		ObservedAt: observedAt.UTC(),
	}
	var reasons []string
	if pgid, ok := processGroup(pid); ok && pgid > 0 {
		identity.PGID = pgid
	} else {
		reasons = append(reasons, "process-group-unavailable")
	}
	if birth := strings.TrimSpace(processBirthIdentity(pid)); birth != "" {
		identity.ProcessBirthIdentity = birth
	} else {
		reasons = append(reasons, "process-birth-identity-unavailable")
	}
	if executable := strings.TrimSpace(processExecutableIdentity(pid)); executable != "" {
		identity.ExecutableIdentity = executable
	} else {
		reasons = append(reasons, "executable-identity-unavailable")
	}
	identity.Ambiguous = len(reasons) > 0
	identity.AmbiguityReason = strings.Join(reasons, ",")
	return identity, nil
}

func VerifySnapshot(identity Identity) error {
	if identity.PID <= 0 {
		return fmt.Errorf("process authority pid is missing")
	}
	if identity.PGID <= 0 || strings.TrimSpace(identity.ProcessBirthIdentity) == "" || strings.TrimSpace(identity.ExecutableIdentity) == "" || identity.Ambiguous {
		return fmt.Errorf("process authority identity is ambiguous")
	}
	if !Alive(identity.PID) {
		return fmt.Errorf("process is not alive")
	}
	if pgid, ok := processGroup(identity.PID); !ok || pgid != identity.PGID {
		return fmt.Errorf("process authority group mismatch")
	}
	if birth := strings.TrimSpace(processBirthIdentity(identity.PID)); birth == "" || birth != identity.ProcessBirthIdentity {
		return fmt.Errorf("process authority birth identity mismatch")
	}
	if executable := strings.TrimSpace(processExecutableIdentity(identity.PID)); executable == "" || executable != identity.ExecutableIdentity {
		return fmt.Errorf("process authority executable mismatch")
	}
	return nil
}

func psIdentityField(pid int, args ...string) string {
	out, err := runPSIdentityCommand(append(args, "-p", strconv.Itoa(pid))...)
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}
