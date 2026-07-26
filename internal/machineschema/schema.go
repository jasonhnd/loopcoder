package machineschema

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
)

// Ensure applies machine-domain tables to an open machine store.
func Ensure(ctx context.Context, ms *authoritystore.MachineStore) error {
	if ms == nil || ms.Foundation() == nil {
		return fmt.Errorf("machineschema: nil machine store")
	}
	return ms.Foundation().WithDB(func(db *sql.DB) error {
		return ensure(ctx, db)
	})
}

func ensure(ctx context.Context, db *sql.DB) error {
	for _, stmt := range ddl {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("machineschema: %w", err)
		}
	}
	return nil
}

var ddl = []string{
	`CREATE TABLE IF NOT EXISTS provider_installations (
		provider_key TEXT PRIMARY KEY,
		install_path TEXT NOT NULL DEFAULT '',
		version TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL,
		observed_at TEXT NOT NULL,
		freshness TEXT NOT NULL DEFAULT 'unknown',
		confidence TEXT NOT NULL DEFAULT 'unknown',
		digest TEXT NOT NULL DEFAULT '',
		payload_json TEXT NOT NULL DEFAULT '{}' CHECK (length(payload_json) <= 8192)
	)`,
	`CREATE TABLE IF NOT EXISTS model_capabilities (
		provider_key TEXT NOT NULL,
		model_id TEXT NOT NULL,
		capabilities_json TEXT NOT NULL DEFAULT '{}' CHECK (length(capabilities_json) <= 8192),
		source TEXT NOT NULL,
		observed_at TEXT NOT NULL,
		freshness TEXT NOT NULL DEFAULT 'unknown',
		confidence TEXT NOT NULL DEFAULT 'unknown',
		digest TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (provider_key, model_id)
	)`,
	`CREATE TABLE IF NOT EXISTS quota_observations (
		observation_id TEXT PRIMARY KEY,
		provider_key TEXT NOT NULL,
		window_label TEXT NOT NULL DEFAULT '',
		remaining REAL,
		limit_value REAL,
		reset_at TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL,
		observed_at TEXT NOT NULL,
		freshness TEXT NOT NULL DEFAULT 'unknown',
		confidence TEXT NOT NULL DEFAULT 'unknown',
		digest TEXT NOT NULL DEFAULT '',
		payload_json TEXT NOT NULL DEFAULT '{}' CHECK (length(payload_json) <= 8192)
	)`,
	`CREATE TABLE IF NOT EXISTS provider_health (
		provider_key TEXT PRIMARY KEY,
		state TEXT NOT NULL CHECK (state IN ('healthy','degraded','cooldown','unknown')),
		cooldown_until TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL,
		observed_at TEXT NOT NULL,
		confidence TEXT NOT NULL DEFAULT 'unknown',
		digest TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS resource_reservations (
		reservation_id TEXT PRIMARY KEY,
		owner TEXT NOT NULL,
		budget_kind TEXT NOT NULL,
		units REAL NOT NULL,
		lease_until TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL CHECK (state IN ('active','released','expired')),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		digest TEXT NOT NULL DEFAULT ''
	)`,
}

// Observation is a provider installation fact.
type Observation struct {
	ProviderKey string
	InstallPath string
	Version     string
	Source      string
	ObservedAt  time.Time
	Freshness   string
	Confidence  string
	Digest      string
}

// PutInstallation inserts or replaces a provider installation observation.
func PutInstallation(ctx context.Context, ms *authoritystore.MachineStore, obs Observation) error {
	if containsForbidden(obs.InstallPath) || containsForbidden(obs.Source) {
		return fmt.Errorf("machineschema: forbidden field content")
	}
	if obs.ProviderKey == "" || obs.Source == "" {
		return fmt.Errorf("machineschema: provider_key and source required")
	}
	if obs.Freshness == "" {
		obs.Freshness = "unknown"
	}
	if obs.Confidence == "" {
		obs.Confidence = "unknown"
	}
	ts := obs.ObservedAt.UTC().Format(time.RFC3339Nano)
	return ms.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensure(ctx, db); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, `INSERT INTO provider_installations(
			provider_key, install_path, version, source, observed_at, freshness, confidence, digest, payload_json
		) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(provider_key) DO UPDATE SET
			install_path=excluded.install_path,
			version=excluded.version,
			source=excluded.source,
			observed_at=excluded.observed_at,
			freshness=excluded.freshness,
			confidence=excluded.confidence,
			digest=excluded.digest`,
			obs.ProviderKey, obs.InstallPath, obs.Version, obs.Source, ts, obs.Freshness, obs.Confidence, obs.Digest, "{}",
		)
		return err
	})
}

// GetInstallation reads a provider installation row.
func GetInstallation(ctx context.Context, ms *authoritystore.MachineStore, providerKey string) (Observation, bool, error) {
	var out Observation
	var ts string
	err := ms.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensure(ctx, db); err != nil {
			return err
		}
		return db.QueryRowContext(ctx, `SELECT provider_key, install_path, version, source, observed_at, freshness, confidence, digest
			FROM provider_installations WHERE provider_key=?`, providerKey).Scan(
			&out.ProviderKey, &out.InstallPath, &out.Version, &out.Source, &ts, &out.Freshness, &out.Confidence, &out.Digest,
		)
	})
	if err == sql.ErrNoRows {
		return Observation{}, false, nil
	}
	if err != nil {
		return Observation{}, false, err
	}
	out.ObservedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return out, true, nil
}

// Reservation is a resource reservation record.
type Reservation struct {
	ID         string
	Owner      string
	BudgetKind string
	Units      float64
	LeaseUntil time.Time
	State      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Digest     string
}

// PutReservation stores a reservation.
func PutReservation(ctx context.Context, ms *authoritystore.MachineStore, r Reservation) error {
	if r.ID == "" || r.Owner == "" || r.BudgetKind == "" || r.State == "" {
		return fmt.Errorf("machineschema: reservation fields required")
	}
	if containsForbidden(r.Owner) {
		return fmt.Errorf("machineschema: forbidden owner content")
	}
	return ms.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensure(ctx, db); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, `INSERT INTO resource_reservations(
			reservation_id, owner, budget_kind, units, lease_until, state, created_at, updated_at, digest
		) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(reservation_id) DO UPDATE SET
			state=excluded.state,
			updated_at=excluded.updated_at,
			lease_until=excluded.lease_until`,
			r.ID, r.Owner, r.BudgetKind, r.Units,
			formatTime(r.LeaseUntil), r.State, formatTime(r.CreatedAt), formatTime(r.UpdatedAt), r.Digest,
		)
		return err
	})
}

// AssertNoProjectDomainColumns fails if project-domain table names exist.
func AssertNoProjectDomainColumns(ctx context.Context, ms *authoritystore.MachineStore) error {
	forbidden := []string{"jobs", "attempts", "events", "work_items", "pull_requests", "run_events"}
	return ms.Foundation().WithDB(func(db *sql.DB) error {
		for _, name := range forbidden {
			var n int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("machineschema: forbidden project-domain table %s present", name)
			}
		}
		return nil
	})
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func containsForbidden(s string) bool {
	lower := strings.ToLower(s)
	for _, bad := range []string{"sk-", "ghp_", "password=", "api_key", "-----begin"} {
		if strings.Contains(lower, bad) {
			return true
		}
	}
	return false
}
