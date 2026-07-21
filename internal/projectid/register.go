package projectid

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
	"github.com/jasonhnd/loopcoder/internal/home"
)

// Registration is a persisted registry row plus aliases.
type Registration struct {
	Identity
	Created bool
	Updated bool
	Aliases []string
}

// AutoRegister resolves repoPath, ensures project layout, and upserts machine registry.
func AutoRegister(ctx context.Context, layout home.V09Layout, repoPath string, now func() time.Time) (Registration, error) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	id, err := Resolve(ctx, repoPath)
	if err != nil {
		return Registration{}, err
	}
	if _, err := EnsureLayoutPath(layout, id.ProjectID); err != nil {
		return Registration{}, err
	}
	ms, err := layout.OpenMachine(ctx, now)
	if err != nil {
		return Registration{}, err
	}
	defer ms.Close()
	return upsertRegistration(ctx, ms, id, now())
}

func upsertRegistration(ctx context.Context, ms *authoritystore.MachineStore, id Identity, at time.Time) (Registration, error) {
	foundation := ms.Foundation()
	if foundation == nil {
		return Registration{}, fmt.Errorf("projectid: nil foundation")
	}
	var reg Registration
	err := foundation.WithDB(func(sqlDB *sql.DB) error {
		if err := ensureRegistrySchema(ctx, sqlDB); err != nil {
			return err
		}
		var err error
		reg, err = upsertRegistrationDB(ctx, sqlDB, id, at)
		return err
	})
	return reg, err
}

func upsertRegistrationDB(ctx context.Context, sqlDB *sql.DB, id Identity, at time.Time) (Registration, error) {
	ts := at.UTC().Format(time.RFC3339Nano)
	var existing string
	err := sqlDB.QueryRowContext(ctx, `SELECT project_id FROM project_registry WHERE project_id = ?`, id.ProjectID).Scan(&existing)
	created := false
	updated := false
	switch {
	case err == sql.ErrNoRows:
		_, err = sqlDB.ExecContext(ctx, `INSERT INTO project_registry(
			project_id, display_name, identity_source, identity_key, remote_url,
			github_owner, github_name, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?)`,
			id.ProjectID, id.DisplayName, string(id.Source), id.IdentityKey, id.RemoteURL,
			id.GitHubOwner, id.GitHubName, ts, ts,
		)
		if err != nil {
			return Registration{}, err
		}
		created = true
	case err != nil:
		return Registration{}, err
	default:
		var key string
		if err := sqlDB.QueryRowContext(ctx, `SELECT identity_key FROM project_registry WHERE project_id = ?`, id.ProjectID).Scan(&key); err != nil {
			return Registration{}, err
		}
		if key != id.IdentityKey {
			return Registration{}, fmt.Errorf("projectid: project_id collision for distinct identity keys")
		}
		_, err = sqlDB.ExecContext(ctx, `UPDATE project_registry SET display_name=?, remote_url=?, github_owner=?, github_name=?, updated_at=? WHERE project_id=?`,
			id.DisplayName, id.RemoteURL, id.GitHubOwner, id.GitHubName, ts, id.ProjectID,
		)
		if err != nil {
			return Registration{}, err
		}
		updated = true
	}

	if id.CanonicalPath != "" {
		if err := upsertAlias(ctx, sqlDB, id.ProjectID, "path", id.CanonicalPath, ts); err != nil {
			return Registration{}, err
		}
	}
	if id.LocalPath != "" && id.LocalPath != id.CanonicalPath {
		if err := upsertAlias(ctx, sqlDB, id.ProjectID, "path", id.LocalPath, ts); err != nil {
			return Registration{}, err
		}
	}
	aliases, err := listAliases(ctx, sqlDB, id.ProjectID)
	if err != nil {
		return Registration{}, err
	}
	return Registration{Identity: id, Created: created, Updated: updated, Aliases: aliases}, nil
}

// LookupByPath finds a project_id by path alias.
func LookupByPath(ctx context.Context, ms *authoritystore.MachineStore, path string) (string, bool, error) {
	foundation := ms.Foundation()
	if foundation == nil {
		return "", false, fmt.Errorf("projectid: nil foundation")
	}
	var id string
	err := foundation.WithDB(func(db *sql.DB) error {
		if err := ensureRegistrySchema(ctx, db); err != nil {
			return err
		}
		return db.QueryRowContext(ctx, `SELECT project_id FROM project_aliases WHERE alias_kind='path' AND alias_value=?`, path).Scan(&id)
	})
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

func upsertAlias(ctx context.Context, db *sql.DB, projectID, kind, value, ts string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var other string
	err := db.QueryRowContext(ctx, `SELECT project_id FROM project_aliases WHERE alias_kind=? AND alias_value=?`, kind, value).Scan(&other)
	if err == nil && other != projectID {
		return fmt.Errorf("projectid: alias %s already registered to %s", kind, other)
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO project_aliases(project_id, alias_kind, alias_value, created_at)
		VALUES(?,?,?,?)
		ON CONFLICT(alias_kind, alias_value) DO UPDATE SET project_id=excluded.project_id`,
		projectID, kind, value, ts,
	)
	return err
}

func listAliases(ctx context.Context, db *sql.DB, projectID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT alias_value FROM project_aliases WHERE project_id=? ORDER BY alias_value`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func ensureRegistrySchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS project_registry (
			project_id TEXT PRIMARY KEY,
			display_name TEXT NOT NULL,
			identity_source TEXT NOT NULL,
			identity_key TEXT NOT NULL UNIQUE,
			remote_url TEXT NOT NULL DEFAULT '',
			github_owner TEXT NOT NULL DEFAULT '',
			github_name TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS project_aliases (
			project_id TEXT NOT NULL,
			alias_kind TEXT NOT NULL,
			alias_value TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (alias_kind, alias_value),
			FOREIGN KEY (project_id) REFERENCES project_registry(project_id)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("projectid: ensure schema: %w", err)
		}
	}
	return nil
}
