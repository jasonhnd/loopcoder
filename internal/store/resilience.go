package store

import (
	"time"
)

// Operational limits for machine and project compact stores (V090-011).
// Both roles share these foundation settings; domain schema lives elsewhere.
const (
	// DefaultBusyTimeout is how long SQLite waits on locks before SQLITE_BUSY.
	DefaultBusyTimeout = 5 * time.Second

	// MaxOpenConns is the hard connection-pool bound (single-writer policy).
	MaxOpenConns = 1

	// MaxIdleConns keeps at most one idle connection (matches MaxOpenConns).
	MaxIdleConns = 1

	// RollbackCleanupTimeout bounds rollback after caller cancellation.
	RollbackCleanupTimeout = 5 * time.Second

	// CloseCleanupTimeout bounds clean-close WAL checkpoint + close work.
	CloseCleanupTimeout = 10 * time.Second

	// DefaultWALAutocheckpoint is pages between automatic WAL checkpoints.
	DefaultWALAutocheckpoint = 1000
)

// JournalModeWAL is the required journal mode for v0.9 authority stores.
const JournalModeWAL = "wal"

// OpenReport describes recovery and operating mode after a successful Open.
type OpenReport struct {
	// Recovered is true when a previous process left an unclean-open marker
	// (abrupt termination). Committed data is preserved; uncommitted work is gone.
	Recovered bool
	// JournalMode is the active SQLite journal mode (expected "wal").
	JournalMode string
	// BusyTimeout is the configured lock wait.
	BusyTimeout time.Duration
	// MaxOpenConns is the pool upper bound.
	MaxOpenConns int
}

// LastOpenReport returns the recovery report from the most recent successful Open.
func (s *Store) LastOpenReport() OpenReport {
	if s == nil {
		return OpenReport{}
	}
	return s.openReport
}
