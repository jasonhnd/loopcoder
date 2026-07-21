package admission

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
)

var ddl = []string{
	`CREATE TABLE IF NOT EXISTS admission_reservations (
		reservation_id TEXT PRIMARY KEY,
		generation INTEGER NOT NULL,
		project_id TEXT NOT NULL,
		job_id TEXT NOT NULL DEFAULT '',
		attempt_id TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL CHECK (role IN ('worker','verifier','local_test')),
		state TEXT NOT NULL CHECK (state IN ('active','released','expired','attention_required')),
		processes INTEGER NOT NULL DEFAULT 0,
		rss_bytes INTEGER NOT NULL DEFAULT 0,
		cpu_rate REAL NOT NULL DEFAULT 0,
		lease_until TEXT NOT NULL,
		idempotency_key TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		attention_reason TEXT NOT NULL DEFAULT '',
		UNIQUE(idempotency_key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_admission_reservations_state
		ON admission_reservations(state, role, lease_until)`,
	`CREATE TABLE IF NOT EXISTS admission_enforcement_requests (
		request_id TEXT PRIMARY KEY,
		reservation_id TEXT NOT NULL,
		transition_key TEXT NOT NULL,
		metric TEXT NOT NULL,
		observed REAL NOT NULL,
		threshold REAL NOT NULL,
		created_at TEXT NOT NULL,
		UNIQUE(reservation_id, transition_key)
	)`,
}

// Ensure creates admission tables on an open machine store.
func Ensure(ctx context.Context, ms *authoritystore.MachineStore) error {
	if ms == nil || ms.Foundation() == nil {
		return fmt.Errorf("admission: nil machine store")
	}
	return ms.Foundation().WithDB(func(db *sql.DB) error {
		return ensure(ctx, db)
	})
}

func ensure(ctx context.Context, db *sql.DB) error {
	for _, stmt := range ddl {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("admission ensure: %w", err)
		}
	}
	return nil
}
