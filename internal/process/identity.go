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

// VerifyClass is a typed process-proof result. Callers must not parse error strings.
type VerifyClass int

const (
	// VerifyExactLive: pid alive and birth/pgid/executable match authority.
	VerifyExactLive VerifyClass = iota
	// VerifyDead: pid is not alive.
	VerifyDead
	// VerifyMismatch: pid alive but birth/pgid/executable explicitly differs (observable reuse).
	VerifyMismatch
	// VerifyUnobservable: alive (or unknown) but permission/read/parse/ambiguous — fail closed.
	VerifyUnobservable
)

// VerifyError carries a typed classification for VerifySnapshot failures.
type VerifyError struct {
	Class VerifyClass
	Msg   string
}

func (e *VerifyError) Error() string {
	if e == nil {
		return "process verify error"
	}
	return e.Msg
}

// ClassifySnapshot returns a typed proof class without string parsing.
func ClassifySnapshot(identity Identity) VerifyClass {
	if identity.PID <= 0 {
		return VerifyUnobservable
	}
	if identity.PGID <= 0 ||
		strings.TrimSpace(identity.ProcessBirthIdentity) == "" ||
		strings.TrimSpace(identity.ExecutableIdentity) == "" ||
		identity.Ambiguous {
		return VerifyUnobservable
	}
	if !Alive(identity.PID) {
		return VerifyDead
	}
	pgid, ok := processGroup(identity.PID)
	if !ok {
		// Cannot read process group while pid appears alive → unobservable.
		return VerifyUnobservable
	}
	if pgid != identity.PGID {
		return VerifyMismatch
	}
	birth := strings.TrimSpace(processBirthIdentity(identity.PID))
	if birth == "" {
		return VerifyUnobservable
	}
	if birth != strings.TrimSpace(identity.ProcessBirthIdentity) {
		return VerifyMismatch
	}
	executable := strings.TrimSpace(processExecutableIdentity(identity.PID))
	if executable == "" {
		return VerifyUnobservable
	}
	if executable != strings.TrimSpace(identity.ExecutableIdentity) {
		return VerifyMismatch
	}
	return VerifyExactLive
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
	switch ClassifySnapshot(identity) {
	case VerifyExactLive:
		return nil
	case VerifyDead:
		return &VerifyError{Class: VerifyDead, Msg: "process is not alive"}
	case VerifyMismatch:
		return &VerifyError{Class: VerifyMismatch, Msg: "process authority identity mismatch"}
	default:
		return &VerifyError{Class: VerifyUnobservable, Msg: "process authority identity is ambiguous or unobservable"}
	}
}

func psIdentityField(pid int, args ...string) string {
	out, err := runPSIdentityCommand(append(args, "-p", strconv.Itoa(pid))...)
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), " ")
}
