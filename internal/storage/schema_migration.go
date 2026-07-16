package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// SchemaMigrationContract identifies the machine-readable storage upgrade contract.
	SchemaMigrationContract = "loopcoder.storage_migration.v1"

	schemaMigrationStatusFresh           = "fresh"
	schemaMigrationStatusCurrent         = "current"
	schemaMigrationStatusUpgradeRequired = "upgrade-required"
)

// SchemaMigrationStep is one ordered SQLite schema transition.
type SchemaMigrationStep struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
}

// SchemaRollbackPlan describes whether and how an applied migration can be undone.
// Limitations contains stable codes so callers do not need to parse prose.
type SchemaRollbackPlan struct {
	Applicable      bool     `json:"applicable"`
	Supported       bool     `json:"supported"`
	Strategy        string   `json:"strategy,omitempty"`
	RequiresOffline bool     `json:"requires_offline"`
	BackupRequired  bool     `json:"backup_required"`
	BackupPath      string   `json:"backup_path,omitempty"`
	BackupSHA256    string   `json:"backup_sha256,omitempty"`
	Limitations     []string `json:"limitations"`
}

// SchemaMigrationPlan is a side-effect-free description of a storage upgrade.
type SchemaMigrationPlan struct {
	SchemaVersion       string                `json:"schema_version"`
	DatabasePath        string                `json:"database_path"`
	SourceExists        bool                  `json:"source_exists"`
	SourceSchemaVersion int                   `json:"source_schema_version"`
	TargetSchemaVersion int                   `json:"target_schema_version"`
	Status              string                `json:"status"`
	Steps               []SchemaMigrationStep `json:"steps"`
	BackupRequired      bool                  `json:"backup_required"`
	BackupDirectory     string                `json:"backup_directory,omitempty"`
	PlanFingerprint     string                `json:"plan_fingerprint"`
	Rollback            SchemaRollbackPlan    `json:"rollback"`
}

// SchemaMigrationBackup is the verified v0.7 recovery point recorded by v0.8.
type SchemaMigrationBackup struct {
	BackupID                 string `json:"backup_id"`
	SourcePath               string `json:"source_path"`
	SourceSchemaVersion      int    `json:"source_schema_version"`
	SHA256                   string `json:"sha256"`
	Path                     string `json:"path"`
	CreatedAt                string `json:"created_at"`
	MigrationPlanFingerprint string `json:"migration_plan_fingerprint"`
	Verified                 bool   `json:"verified"`
}

// SchemaMigrationResult reports a plan or a completed application of that plan.
type SchemaMigrationResult struct {
	SchemaVersion string                 `json:"schema_version"`
	Status        string                 `json:"status"`
	DryRun        bool                   `json:"dry_run"`
	Applied       bool                   `json:"applied"`
	Plan          SchemaMigrationPlan    `json:"plan"`
	Health        *Health                `json:"health,omitempty"`
	Backup        *SchemaMigrationBackup `json:"backup,omitempty"`
	Rollback      SchemaRollbackPlan     `json:"rollback"`
}

// SchemaMigrationOptions controls storage migration planning and application.
// Apply defaults to false so inspection cannot mutate local state accidentally.
type SchemaMigrationOptions struct {
	Path  string
	Apply bool
	Now   func() time.Time
}

// PlanSchemaMigration inspects a database without creating files or applying migrations.
func PlanSchemaMigration(ctx context.Context, path string) (SchemaMigrationPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return SchemaMigrationPlan{}, errors.New("plan storage migration: path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return SchemaMigrationPlan{}, fmt.Errorf("plan storage migration: resolve path: %w", err)
	}
	absolutePath = filepath.Clean(absolutePath)
	plan := SchemaMigrationPlan{
		SchemaVersion:       SchemaMigrationContract,
		DatabasePath:        absolutePath,
		TargetSchemaVersion: CurrentSchemaVersion,
		Steps:               []SchemaMigrationStep{},
		Rollback:            noSchemaRollbackPlan(),
	}

	info, err := os.Lstat(absolutePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return SchemaMigrationPlan{}, fmt.Errorf("plan storage migration: inspect %s: %w", absolutePath, err)
		}
		plan.Status = schemaMigrationStatusFresh
		plan.Steps = schemaMigrationStepsAfter(0)
		plan.PlanFingerprint = schemaMigrationFingerprint(plan)
		return plan, nil
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return SchemaMigrationPlan{}, fmt.Errorf("plan storage migration: database path %s must be a regular file, not %s", absolutePath, info.Mode().Type())
	}
	plan.SourceExists = true

	db, err := openReadOnlySQLite(ctx, absolutePath)
	if err != nil {
		return SchemaMigrationPlan{}, fmt.Errorf("plan storage migration: %w", err)
	}
	defer db.Close()

	version, err := schemaVersion(ctx, db)
	if err != nil {
		return SchemaMigrationPlan{}, fmt.Errorf("plan storage migration: inspect schema: %w", err)
	}
	plan.SourceSchemaVersion = version
	if version == 0 {
		return SchemaMigrationPlan{}, errors.New("plan storage migration: existing database has no migration history")
	}
	if version > CurrentSchemaVersion {
		return SchemaMigrationPlan{}, unsupportedVersionError(version)
	}
	if err := validateSchemaMigrationHistory(ctx, db, version); err != nil {
		return SchemaMigrationPlan{}, fmt.Errorf("plan storage migration: %w", err)
	}
	if err := integrityCheck(ctx, db); err != nil {
		return SchemaMigrationPlan{}, fmt.Errorf("plan storage migration: %w", err)
	}

	plan.Steps = schemaMigrationStepsAfter(version)
	if version == CurrentSchemaVersion {
		if err := checkRequiredTables(ctx, db); err != nil {
			return SchemaMigrationPlan{}, fmt.Errorf("plan storage migration: %w", err)
		}
		if err := checkDurableRunGraph(ctx, db); err != nil {
			return SchemaMigrationPlan{}, fmt.Errorf("plan storage migration: %w", err)
		}
		plan.Status = schemaMigrationStatusCurrent
	} else {
		plan.Status = schemaMigrationStatusUpgradeRequired
	}
	if version == 9 {
		plan.BackupRequired = true
		plan.BackupDirectory = filepath.Join(filepath.Dir(absolutePath), "backups")
		plan.Rollback = v07SchemaRollbackPlan()
	}
	plan.PlanFingerprint = schemaMigrationFingerprint(plan)
	return plan, nil
}

// RunSchemaMigration returns a plan unless Apply is explicitly true. Applying
// delegates to Open so there is only one production migration engine.
func RunSchemaMigration(ctx context.Context, opts SchemaMigrationOptions) (SchemaMigrationResult, error) {
	plan, err := PlanSchemaMigration(ctx, opts.Path)
	if err != nil {
		return SchemaMigrationResult{}, err
	}
	result := SchemaMigrationResult{
		SchemaVersion: SchemaMigrationContract,
		Status:        "planned",
		DryRun:        !opts.Apply,
		Plan:          plan,
		Rollback:      plan.Rollback,
	}
	if !opts.Apply {
		return result, nil
	}

	store, err := Open(ctx, Options{Path: plan.DatabasePath, Now: opts.Now})
	if err != nil {
		return SchemaMigrationResult{}, fmt.Errorf("apply storage migration: %w", err)
	}
	health, healthErr := store.Health(ctx)
	if healthErr != nil {
		_ = store.Close()
		return SchemaMigrationResult{}, fmt.Errorf("apply storage migration: verify target: %w", healthErr)
	}
	if !health.OK || health.SchemaVersion != CurrentSchemaVersion {
		_ = store.Close()
		return SchemaMigrationResult{}, fmt.Errorf("apply storage migration: target health is not schema %d: %+v", CurrentSchemaVersion, health)
	}

	if plan.SourceExists && plan.SourceSchemaVersion == 9 {
		backup, verifyErr := loadVerifiedV07SchemaBackup(ctx, store, plan.DatabasePath)
		if verifyErr != nil {
			_ = store.Close()
			return SchemaMigrationResult{}, fmt.Errorf("apply storage migration: verify v0.7 backup: %w", verifyErr)
		}
		result.Backup = &backup
		result.Rollback.BackupPath = backup.Path
		result.Rollback.BackupSHA256 = backup.SHA256
	}
	if err := store.Close(); err != nil {
		return SchemaMigrationResult{}, fmt.Errorf("apply storage migration: close target: %w", err)
	}

	result.Applied = true
	result.DryRun = false
	result.Health = &health
	switch {
	case !plan.SourceExists:
		result.Status = "created"
	case plan.SourceSchemaVersion == CurrentSchemaVersion:
		result.Status = "no-op"
	default:
		result.Status = "migrated"
	}
	return result, nil
}

func openReadOnlySQLite(ctx context.Context, path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("mode", "ro")
	u.RawQuery = query.Encode()
	db, err := sql.Open(driverName, u.String())
	if err != nil {
		return nil, fmt.Errorf("open read-only database %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable read-only mode for %s: %w", path, err)
	}
	return db, nil
}

func validateSchemaMigrationHistory(ctx context.Context, db *sql.DB, version int) error {
	rows, err := db.QueryContext(ctx, `SELECT version, name FROM migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read migration history: %w", err)
	}
	defer rows.Close()
	expected := 1
	for rows.Next() {
		var gotVersion int
		var name string
		if err := rows.Scan(&gotVersion, &name); err != nil {
			return fmt.Errorf("read migration history: %w", err)
		}
		if gotVersion != expected {
			return fmt.Errorf("migration history is incomplete: got version %d, want %d", gotVersion, expected)
		}
		if gotVersion <= len(migrations) && name != migrations[gotVersion-1].name {
			return fmt.Errorf("migration history version %d has name %q, want %q", gotVersion, name, migrations[gotVersion-1].name)
		}
		expected++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read migration history: %w", err)
	}
	if expected-1 != version {
		return fmt.Errorf("migration history ends at version %d, reported version %d", expected-1, version)
	}
	return nil
}

func schemaMigrationStepsAfter(version int) []SchemaMigrationStep {
	steps := make([]SchemaMigrationStep, 0, len(migrations))
	for _, migration := range migrations {
		if migration.version > version {
			steps = append(steps, SchemaMigrationStep{Version: migration.version, Name: migration.name})
		}
	}
	return steps
}

func schemaMigrationFingerprint(plan SchemaMigrationPlan) string {
	parts := []string{
		SchemaMigrationContract,
		plan.DatabasePath,
		strconv.Itoa(plan.SourceSchemaVersion),
		strconv.Itoa(plan.TargetSchemaVersion),
	}
	for _, step := range plan.Steps {
		parts = append(parts, strconv.Itoa(step.Version), step.Name)
	}
	return "sha256:" + hashStringsStorage(parts...)
}

func noSchemaRollbackPlan() SchemaRollbackPlan {
	return SchemaRollbackPlan{Limitations: []string{}}
}

func v07SchemaRollbackPlan() SchemaRollbackPlan {
	return SchemaRollbackPlan{
		Applicable:      true,
		Supported:       true,
		Strategy:        "offline-copy-verified-v0.7-backup",
		RequiresOffline: true,
		BackupRequired:  true,
		Limitations: []string{
			"requires-all-loopcoder-processes-stopped",
			"restore-copy-never-mutate-backup",
			"discards-v0.8-only-state",
			"v0.7-cannot-open-v0.8-schema",
		},
	}
}

func loadVerifiedV07SchemaBackup(ctx context.Context, store Store, sourcePath string) (SchemaMigrationBackup, error) {
	var backup SchemaMigrationBackup
	err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT backup_id, source_db_path, source_schema_version, source_db_hash, backup_path, created_at, migration_plan_fingerprint
			FROM delivery_migration_backups
			WHERE source_db_path = ? AND source_schema_version = 9
			ORDER BY created_at DESC, backup_id DESC
			LIMIT 1`, sourcePath).Scan(
			&backup.BackupID,
			&backup.SourcePath,
			&backup.SourceSchemaVersion,
			&backup.SHA256,
			&backup.Path,
			&backup.CreatedAt,
			&backup.MigrationPlanFingerprint,
		)
	})
	if err != nil {
		return SchemaMigrationBackup{}, err
	}
	if backup.BackupID == "" || backup.SHA256 == "" || backup.MigrationPlanFingerprint == "" {
		return SchemaMigrationBackup{}, errors.New("backup metadata is incomplete")
	}
	if filepath.Clean(backup.SourcePath) != filepath.Clean(sourcePath) {
		return SchemaMigrationBackup{}, fmt.Errorf("backup source path %q does not match %q", backup.SourcePath, sourcePath)
	}
	if err := verifyV07SchemaBackup(ctx, backup, sourcePath); err != nil {
		return SchemaMigrationBackup{}, err
	}
	backup.Verified = true
	return backup, nil
}

func verifyV07SchemaBackup(ctx context.Context, backup SchemaMigrationBackup, sourcePath string) error {
	expectedDir := filepath.Join(filepath.Dir(sourcePath), "backups")
	backupPath := filepath.Clean(backup.Path)
	if filepath.Dir(backupPath) != expectedDir || !filepath.IsLocal(filepath.Base(backupPath)) {
		return fmt.Errorf("backup path %q is outside %q", backup.Path, expectedDir)
	}
	info, err := os.Lstat(backupPath)
	if err != nil {
		return fmt.Errorf("inspect backup: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("backup path %q is not a regular file", backupPath)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("backup path %q is not owner-only: mode %04o", backupPath, info.Mode().Perm())
	}

	root, err := os.OpenRoot(expectedDir)
	if err != nil {
		return fmt.Errorf("open backup directory: %w", err)
	}
	actualHash, hashErr := fileSHA256(ctx, root, filepath.Base(backupPath), nil)
	closeErr := root.Close()
	if hashErr != nil {
		return fmt.Errorf("hash backup: %w", hashErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close backup directory: %w", closeErr)
	}
	if actualHash != backup.SHA256 {
		return fmt.Errorf("backup checksum %s does not match recorded checksum %s", actualHash, backup.SHA256)
	}

	db, err := openReadOnlySQLite(ctx, backupPath)
	if err != nil {
		return err
	}
	defer db.Close()
	version, err := schemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("inspect backup schema: %w", err)
	}
	if version != 9 {
		return fmt.Errorf("backup schema version = %d, want 9", version)
	}
	if err := validateSchemaMigrationHistory(ctx, db, version); err != nil {
		return fmt.Errorf("inspect backup migration history: %w", err)
	}
	if err := integrityCheck(ctx, db); err != nil {
		return fmt.Errorf("inspect backup integrity: %w", err)
	}
	return nil
}
