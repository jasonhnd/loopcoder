package store

import (
	"context"
	"errors"
	"os"
	"strings"

	moderncsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Typed resilience errors. Callers should use errors.Is / FailureClassOf.
var (
	// ErrBusy is a retryable lock contention result (SQLITE_BUSY / LOCKED).
	ErrBusy = errors.New("store: busy")

	// ErrDiskFull indicates ENOSPC or SQLite full conditions.
	ErrDiskFull = errors.New("store: disk full")

	// ErrPermission indicates filesystem permission denial.
	ErrPermission = errors.New("store: permission denied")

	// ErrCorrupt indicates integrity_check failure or unreadable schema authority.
	ErrCorrupt = errors.New("store: corrupt")

	// ErrIncompatibleVersion is schema newer/older than this binary supports.
	ErrIncompatibleVersion = errors.New("store: incompatible schema version")

	// ErrQuarantined is returned when a failed open moved the database aside.
	ErrQuarantined = errors.New("store: database quarantined")
)

// FailureClass is a redacted operator-facing category.
type FailureClass string

const (
	ClassOK                  FailureClass = "ok"
	ClassBusy                FailureClass = "busy_exhausted"
	ClassDiskFull            FailureClass = "disk_full"
	ClassPermission          FailureClass = "permission"
	ClassCorrupt             FailureClass = "corruption"
	ClassIncompatibleVersion FailureClass = "incompatible_version"
	ClassCancelled           FailureClass = "cancelled"
	ClassOther               FailureClass = "other"
)

// Diagnostic is a redacted failure report suitable for operators.
// It never includes event payloads or full home-directory paths when
// RedactPath is applied by callers to path fields.
type Diagnostic struct {
	Class          FailureClass `json:"class"`
	Message        string       `json:"message"`
	OperatorAction string       `json:"operator_action"`
	// Retryable is true for busy/contention only.
	Retryable bool `json:"retryable"`
}

// Classify maps an error to a redacted Diagnostic.
func Classify(err error) Diagnostic {
	if err == nil {
		return Diagnostic{Class: ClassOK, Message: "ok"}
	}
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return Diagnostic{
			Class:          ClassCancelled,
			Message:        "operation cancelled",
			OperatorAction: "retry the operation; no store repair needed",
		}
	case IsBusy(err) || errors.Is(err, ErrBusy):
		return Diagnostic{
			Class:          ClassBusy,
			Message:        "store lock busy or exhausted",
			OperatorAction: "retry with backoff; ensure only one writer process holds the store",
			Retryable:      true,
		}
	case errors.Is(err, ErrDiskFull) || isDiskFull(err):
		return Diagnostic{
			Class:          ClassDiskFull,
			Message:        "disk full or SQLite full",
			OperatorAction: "free disk space; do not delete live store files; retry after space is available",
		}
	case errors.Is(err, ErrPermission) || errors.Is(err, os.ErrPermission) || isPermission(err):
		return Diagnostic{
			Class:          ClassPermission,
			Message:        "permission denied on store path",
			OperatorAction: "fix owner-only permissions (0600/0700) and ownership; do not chmod world-writable",
		}
	case errors.Is(err, ErrCorrupt) || isCorrupt(err):
		return Diagnostic{
			Class:          ClassCorrupt,
			Message:        "integrity or schema authority check failed",
			OperatorAction: "stop writers; run quarantine if not already; restore from a known-good backup into a new path",
		}
	case errors.Is(err, ErrIncompatibleVersion) || isIncompatibleVersion(err):
		return Diagnostic{
			Class:          ClassIncompatibleVersion,
			Message:        "store schema version incompatible with this binary",
			OperatorAction: "upgrade/downgrade the binary to a compatible release; never auto-recreate over the file",
		}
	case errors.Is(err, ErrQuarantined):
		return Diagnostic{
			Class:          ClassCorrupt,
			Message:        "database moved to quarantine after failed open checks",
			OperatorAction: "inspect quarantine copy; restore from backup to a separate path; do not overwrite quarantine",
		}
	default:
		return Diagnostic{
			Class:          ClassOther,
			Message:        "store operation failed",
			OperatorAction: "inspect redacted logs; if integrity fails, quarantine and restore from backup",
		}
	}
}

// IsBusy reports whether err is SQLite busy/locked or wrapped ErrBusy.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBusy) {
		return true
	}
	var sqliteErr *moderncsqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}

// FailureClassOf returns the class for err.
func FailureClassOf(err error) FailureClass {
	return Classify(err).Class
}

// RedactPath replaces home-like prefixes for operator diagnostics.
func RedactPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(path, home) {
		return "$HOME" + strings.TrimPrefix(path, home)
	}
	if i := strings.LastIndex(path, "/"); i >= 0 && i+1 < len(path) {
		return "…/" + path[i+1:]
	}
	return path
}

func isDiskFull(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *moderncsqlite.Error
	if errors.As(err, &sqliteErr) {
		if sqliteErr.Code()&0xff == sqlite3.SQLITE_FULL {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no space left") ||
		strings.Contains(msg, "disk full") ||
		strings.Contains(msg, "database or disk is full")
}

func isPermission(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") || strings.Contains(msg, "operation not permitted")
}

func isCorrupt(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "integrity_check") ||
		strings.Contains(msg, "database disk image is malformed") ||
		strings.Contains(msg, "file is not a database") ||
		strings.Contains(msg, "corrupt")
}

func isIncompatibleVersion(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unsupported store schema version") ||
		strings.Contains(msg, "requires migration") ||
		strings.Contains(msg, "unsupported store format identity")
}
