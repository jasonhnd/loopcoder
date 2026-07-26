// Package store implements the v0.9.0 Store open/schema foundation.
//
// This package creates and opens the compact SQLite store with schema version
// bootstrap, owner-only permissions, integrity checks, and close behavior.
// It intentionally does not write domain projections, events, or other later
// R1 tables; those arrive in subsequent slices.
//
// Product platform: darwin/arm64 only. Unsupported GOOS/GOARCH combinations
// fail closed with ErrUnsupportedPlatform; there is no Windows/Linux permission
// implementation on the v0.9 store path.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// CurrentSchemaVersion is the newest compact-store schema this binary can use.
	CurrentSchemaVersion = 1

	// CompatibilityFloor is the oldest compact-store schema this binary can open.
	CompatibilityFloor = 1

	// FormatIdentity is the durable local store format marker.
	FormatIdentity = "loopcoder.store.v1"

	driverName = "sqlite"

	foundationMigrationID   = "r1.3-store-open-schema-foundation"
	foundationMigrationName = "store open/schema foundation"

	defaultBusyTimeout = DefaultBusyTimeout
)

// Options controls compact-store opening.
type Options struct {
	Path string
	Now  func() time.Time

	BusyTimeout time.Duration

	// FormatIdentity overrides the durable format marker written and validated
	// in store_metadata. Empty uses FormatIdentity (generic foundation).
	// Machine and project authorities use distinct identities so one file cannot
	// silently serve both roles.
	FormatIdentity string

	// AuthorityRole is an optional diagnostic label (for example "machine" or
	// "project"). It does not change schema by itself; FormatIdentity does.
	AuthorityRole string

	// QuarantineDir is where integrity/schema failures are moved. Empty uses
	// <parent>/quarantine. Quarantine never auto-recreates a replacement DB
	// in the same Open call.
	QuarantineDir string

	// SkipQuarantine disables automatic quarantine on open integrity failure
	// (tests that assert the raw error). Default false.
	SkipQuarantine bool
}

// Metadata is the singleton store identity and schema state.
type Metadata struct {
	StoreID                 string
	FormatIdentity          string
	SchemaVersion           int
	CompatibilityFloor      int
	CreatedAt               time.Time
	LastSuccessfulMigration int
	LastMigrationAt         time.Time
}

// Store is an open compact SQLite store handle.
type Store struct {
	path           string
	db             *sql.DB
	now            func() time.Time
	formatIdentity string
	authorityRole  string
	openReport     OpenReport

	closeMu sync.Mutex
	closed  bool
}

// Open creates or opens the compact store at opts.Path, bootstraps the schema
// foundation when the path is fresh, enforces owner-only permissions, and runs
// fail-closed integrity validation before returning a handle.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if err := requireSupportedPlatform(); err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, errors.New("open store: path is required")
	}
	path = filepath.Clean(path)
	if path == "." {
		return nil, errors.New("open store: path is required")
	}

	formatIdentity := strings.TrimSpace(opts.FormatIdentity)
	if formatIdentity == "" {
		formatIdentity = FormatIdentity
	}
	authorityRole := strings.TrimSpace(opts.AuthorityRole)

	recovered := readUncleanMarker(path)
	busyTimeout := opts.BusyTimeout
	if busyTimeout <= 0 {
		busyTimeout = defaultBusyTimeout
	}

	var store *Store
	err := withOwnerOnlyUmask(func() error {
		if err := ensurePermissionsForOpen(path); err != nil {
			return fmt.Errorf("open store: secure permissions for %s: %w", path, err)
		}
		db, err := sql.Open(driverName, path)
		if err != nil {
			return fmt.Errorf("open store %s: %w", path, err)
		}
		// Single-writer policy: never grow a connection pool.
		db.SetMaxOpenConns(MaxOpenConns)
		db.SetMaxIdleConns(MaxIdleConns)
		db.SetConnMaxLifetime(0)
		opened := &Store{
			path:           path,
			db:             db,
			formatIdentity: formatIdentity,
			authorityRole:  authorityRole,
			now:            normalizeNow(opts.Now),
			openReport: OpenReport{
				Recovered:    recovered,
				BusyTimeout:  busyTimeout,
				MaxOpenConns: MaxOpenConns,
			},
		}
		if err := opened.configure(ctx, busyTimeout); err != nil {
			_ = db.Close()
			return maybeQuarantine(path, opts, err)
		}
		if err := opened.bootstrapOrValidate(ctx); err != nil {
			_ = db.Close()
			return maybeQuarantine(path, opts, err)
		}
		if err := hardenSQLiteSidecars(path); err != nil {
			_ = db.Close()
			return fmt.Errorf("open store: secure sqlite sidecars for %s: %w", path, err)
		}
		if err := opened.CheckIntegrity(ctx); err != nil {
			_ = db.Close()
			return maybeQuarantine(path, opts, err)
		}
		var journal string
		if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err == nil {
			opened.openReport.JournalMode = toLowerASCII(journal)
		}
		if err := writeOpenMarker(path, opened.now()); err != nil {
			_ = db.Close()
			return fmt.Errorf("open store: %w", err)
		}
		store = opened
		return nil
	})
	if err != nil {
		return nil, err
	}
	return store, nil
}

// maybeQuarantine moves a failed store aside for integrity/version/authority
// failures. It never creates a replacement database in the same call.
func maybeQuarantine(path string, opts Options, cause error) error {
	if opts.SkipQuarantine || cause == nil {
		return cause
	}
	if !shouldQuarantine(cause) {
		return cause
	}
	// Close any leftover handles before rename.
	res, qerr := QuarantineDatabase(path, opts.QuarantineDir, time.Now().UTC())
	if qerr != nil {
		return fmt.Errorf("%w: %v (quarantine failed: %v)", ErrQuarantined, cause, qerr)
	}
	return fmt.Errorf("%w: %v; moved to %s", ErrQuarantined, cause, RedactPath(res.QuarantinePath))
}

func shouldQuarantine(err error) bool {
	if err == nil {
		return false
	}
	// Quarantine only corruption / true schema-version failures.
	// Format-identity (wrong role) must not move the file — operator can reopen
	// with the correct role without recovery.
	if isCorrupt(err) || errors.Is(err, ErrCorrupt) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unsupported store schema version") {
		return true
	}
	if errors.Is(err, ErrIncompatibleVersion) && !strings.Contains(msg, "format identity") {
		return true
	}
	return false
}

// Close releases the store handle. It is idempotent and safe to call more than
// once; subsequent calls return nil without re-closing the database.
// Clean close clears the unclean-open marker and attempts a bounded WAL checkpoint.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.db == nil {
		_ = clearOpenMarker(s.path)
		return nil
	}
	// Independent cleanup so a cancelled ambient context cannot block close.
	ctx, cancel := context.WithTimeout(context.Background(), CloseCleanupTimeout)
	defer cancel()
	_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	err := s.db.Close()
	s.db = nil
	if clearErr := clearOpenMarker(s.path); clearErr != nil && err == nil {
		err = clearErr
	}
	return err
}

// Path returns the on-disk database path.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// ExpectedFormatIdentity returns the format identity this handle was opened with.
func (s *Store) ExpectedFormatIdentity() string {
	if s == nil {
		return ""
	}
	return s.formatIdentity
}

// AuthorityRole returns the optional authority role label for this handle.
func (s *Store) AuthorityRole() string {
	if s == nil {
		return ""
	}
	return s.authorityRole
}

// SchemaVersion returns the durable schema version recorded in store metadata.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	meta, err := s.Metadata(ctx)
	if err != nil {
		return 0, err
	}
	return meta.SchemaVersion, nil
}

// Metadata returns the singleton store metadata row.
func (s *Store) Metadata(ctx context.Context) (Metadata, error) {
	db, _, err := s.openHandle()
	if err != nil {
		return Metadata{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return readMetadata(ctx, db)
}

// CheckIntegrity fails closed on SQLite corruption, permission violations, and
// schema/migration-ledger inconsistencies.
func (s *Store) CheckIntegrity(ctx context.Context) error {
	db, path, err := s.openHandle()
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return checkIntegrity(ctx, path, db)
}

// WithDB runs fn with the live database handle. The callback must not close the
// handle; Close owns lifetime. Used by authority schema packages.
func (s *Store) WithDB(fn func(*sql.DB) error) error {
	if fn == nil {
		return errors.New("store: WithDB callback is required")
	}
	db, _, err := s.openHandle()
	if err != nil {
		return err
	}
	return fn(db)
}

// openHandle returns the live database handle and path, or an error if the
// store is nil or already closed. The returned *sql.DB must not be closed by
// callers; Close owns handle lifetime.
func (s *Store) openHandle() (*sql.DB, string, error) {
	if s == nil {
		return nil, "", errors.New("store is nil")
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed || s.db == nil {
		return nil, "", errors.New("store is closed")
	}
	return s.db, s.path, nil
}

func (s *Store) configure(ctx context.Context, busyTimeout time.Duration) error {
	if busyTimeout <= 0 {
		busyTimeout = defaultBusyTimeout
	}
	// journal_mode must run alone and returns the mode name.
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("open store %s: enable wal: %w", s.path, err)
	}
	s.openReport.JournalMode = toLowerASCII(mode)
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA synchronous = NORMAL`,
		fmt.Sprintf(`PRAGMA busy_timeout = %d`, busyTimeout.Milliseconds()),
		fmt.Sprintf(`PRAGMA wal_autocheckpoint = %d`, DefaultWALAutocheckpoint),
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("open store %s: configure sqlite: %w", s.path, err)
		}
	}
	return nil
}

func (s *Store) bootstrapOrValidate(ctx context.Context) error {
	version, err := schemaVersion(ctx, s.db)
	if err != nil {
		return fmt.Errorf("open store: inspect schema version: %w", err)
	}
	switch {
	case version == 0:
		return s.bootstrapFoundation(ctx)
	case version > CurrentSchemaVersion:
		return unsupportedNewVersionError(version)
	case version < CompatibilityFloor:
		return unsupportedOldVersionError(version)
	case version < CurrentSchemaVersion:
		// Later slices own explicit upgrade apply. This foundation binary only
		// opens databases already at its target version.
		return fmt.Errorf("open store: schema version %d requires migration to %d; explicit migration apply is not part of the open foundation", version, CurrentSchemaVersion)
	default:
		return s.validateCurrentSchema(ctx, version)
	}
}

func (s *Store) bootstrapFoundation(ctx context.Context) error {
	// Reject non-empty unknown databases that lack foundation tables.
	var userTables int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&userTables); err != nil {
		return fmt.Errorf("open store: inspect existing tables: %w", err)
	}
	if userTables > 0 {
		return errors.New("open store: database exists but is missing compact store foundation tables; refusing to mutate foreign schema")
	}

	now := formatTimestamp(s.now())
	storeID, err := newStoreID()
	if err != nil {
		return fmt.Errorf("open store: generate store id: %w", err)
	}
	checksum := foundationChecksum()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("open store: begin foundation bootstrap: %w", err)
	}
	defer tx.Rollback()

	for _, statement := range foundationSchemaStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("open store: create foundation schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO store_metadata(
		id, store_id, format_identity, schema_version, compatibility_floor,
		created_at, last_successful_migration, last_migration_at
	) VALUES (1, ?, ?, ?, ?, ?, ?, ?)`,
		storeID,
		s.formatIdentity,
		CurrentSchemaVersion,
		CompatibilityFloor,
		now,
		CurrentSchemaVersion,
		now,
	); err != nil {
		return fmt.Errorf("open store: insert store metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO migration_ledger(
		version, migration_id, name, checksum, applied_at,
		source_version, target_version, backup_manifest_pointer, verification_result
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		CurrentSchemaVersion,
		foundationMigrationID,
		foundationMigrationName,
		checksum,
		now,
		0,
		CurrentSchemaVersion,
		"",
		"verified",
	); err != nil {
		return fmt.Errorf("open store: insert migration ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("open store: commit foundation bootstrap: %w", err)
	}
	return nil
}

func (s *Store) validateCurrentSchema(ctx context.Context, version int) error {
	meta, err := readMetadata(ctx, s.db)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if meta.FormatIdentity != s.formatIdentity {
		// Wrong role / identity: fail closed without quarantine (file remains).
		return fmt.Errorf("open store: unsupported store format identity %q; want %q", meta.FormatIdentity, s.formatIdentity)
	}
	if meta.SchemaVersion != version {
		return fmt.Errorf("open store: schema metadata version %d does not match migration ledger version %d", meta.SchemaVersion, version)
	}
	if meta.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("open store: schema version %d is not current (%d)", meta.SchemaVersion, CurrentSchemaVersion)
	}
	if meta.CompatibilityFloor < 1 {
		return errors.New("open store: invalid compatibility floor in store metadata")
	}
	if meta.LastSuccessfulMigration != meta.SchemaVersion {
		return fmt.Errorf("open store: last successful migration %d does not match schema version %d", meta.LastSuccessfulMigration, meta.SchemaVersion)
	}
	if err := validateMigrationLedger(ctx, s.db, version); err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	return nil
}

func checkIntegrity(ctx context.Context, path string, db *sql.DB) error {
	permissions, err := CheckPermissions(path)
	if err != nil {
		return fmt.Errorf("store integrity: inspect permissions for %s: %w", path, err)
	}
	if unsafe := firstUnsafePermissionItem(permissions); unsafe != nil {
		return fmt.Errorf("store integrity: unsafe storage path %s: %s", unsafe.Path, unsafe.Message)
	}
	if !permissions.Supported {
		// Platforms without owner-only enforcement cannot satisfy the
		// foundation integrity contract. Fail closed instead of proceeding
		// with Supported=false/Secure=false.
		msg := permissions.Message
		if strings.TrimSpace(msg) == "" {
			msg = "owner-only store permission enforcement is unsupported on this platform"
		}
		return fmt.Errorf("store integrity: %s", msg)
	}
	if !permissions.Secure {
		for _, item := range permissions.Items {
			if item.Exists && !item.Secure {
				return fmt.Errorf("store integrity: insecure %s %s: %s", item.Kind, item.Path, item.Message)
			}
		}
		return fmt.Errorf("store integrity: storage permissions for %s are not secure", path)
	}

	if err := sqliteIntegrityCheck(ctx, db); err != nil {
		return fmt.Errorf("%w: store integrity: %v", ErrCorrupt, err)
	}
	if err := checkRequiredFoundationTables(ctx, db); err != nil {
		return fmt.Errorf("store integrity: %w", err)
	}

	version, err := schemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("store integrity: %w", err)
	}
	if version == 0 {
		return errors.New("store integrity: migration ledger is missing or empty")
	}
	if version > CurrentSchemaVersion {
		return unsupportedNewVersionError(version)
	}
	if version < CompatibilityFloor {
		return unsupportedOldVersionError(version)
	}

	meta, err := readMetadata(ctx, db)
	if err != nil {
		return fmt.Errorf("store integrity: %w", err)
	}
	// format identity is validated at open against the handle's expected identity
	if meta.FormatIdentity == "" {
		return fmt.Errorf("store integrity: missing store format identity")
	}
	if meta.SchemaVersion != version {
		return fmt.Errorf("store integrity: schema metadata version %d does not match migration ledger version %d", meta.SchemaVersion, version)
	}
	if meta.LastSuccessfulMigration != version {
		return fmt.Errorf("store integrity: last successful migration %d does not match ledger version %d", meta.LastSuccessfulMigration, version)
	}
	if err := validateMigrationLedger(ctx, db, version); err != nil {
		return fmt.Errorf("store integrity: %w", err)
	}
	return nil
}

func sqliteIntegrityCheck(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("%w: integrity_check failed: %s", ErrCorrupt, result)
	}
	return nil
}

func checkRequiredFoundationTables(ctx context.Context, q schemaQuerier) error {
	for _, table := range []string{"store_metadata", "migration_ledger"} {
		var count int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			return fmt.Errorf("inspect required table %s: %w", table, err)
		}
		if count != 1 {
			return fmt.Errorf("missing required foundation table %s", table)
		}
	}
	return nil
}

func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'migration_ledger'`).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, nil
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM migration_ledger`).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

func readMetadata(ctx context.Context, q schemaQuerier) (Metadata, error) {
	var meta Metadata
	var createdAt, lastMigrationAt string
	err := q.QueryRowContext(ctx, `SELECT store_id, format_identity, schema_version, compatibility_floor, created_at, last_successful_migration, last_migration_at
		FROM store_metadata WHERE id = 1`).Scan(
		&meta.StoreID,
		&meta.FormatIdentity,
		&meta.SchemaVersion,
		&meta.CompatibilityFloor,
		&createdAt,
		&meta.LastSuccessfulMigration,
		&lastMigrationAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Metadata{}, errors.New("store metadata row is missing")
		}
		return Metadata{}, fmt.Errorf("read store metadata: %w", err)
	}
	meta.CreatedAt, err = parseTimestamp(createdAt)
	if err != nil {
		return Metadata{}, fmt.Errorf("parse store metadata created_at: %w", err)
	}
	meta.LastMigrationAt, err = parseTimestamp(lastMigrationAt)
	if err != nil {
		return Metadata{}, fmt.Errorf("parse store metadata last_migration_at: %w", err)
	}
	return meta, nil
}

func validateMigrationLedger(ctx context.Context, q schemaQuerier, currentVersion int) error {
	rows, err := q.QueryContext(ctx, `SELECT version, migration_id, checksum, source_version, target_version, verification_result
		FROM migration_ledger ORDER BY version ASC`)
	if err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()

	expected := 1
	seenIDs := map[string]struct{}{}
	var lastTarget int
	for rows.Next() {
		var (
			version            int
			migrationID        string
			checksum           string
			sourceVersion      int
			targetVersion      int
			verificationResult string
		)
		if err := rows.Scan(&version, &migrationID, &checksum, &sourceVersion, &targetVersion, &verificationResult); err != nil {
			return fmt.Errorf("scan migration ledger: %w", err)
		}
		if version != expected {
			return fmt.Errorf("migration ledger is not contiguous: expected version %d, found %d", expected, version)
		}
		if strings.TrimSpace(migrationID) == "" {
			return fmt.Errorf("migration ledger version %d has empty migration id", version)
		}
		if _, exists := seenIDs[migrationID]; exists {
			return fmt.Errorf("migration ledger contains duplicate migration id %q", migrationID)
		}
		seenIDs[migrationID] = struct{}{}
		if strings.TrimSpace(checksum) == "" {
			return fmt.Errorf("migration ledger version %d has empty checksum", version)
		}
		if targetVersion != version {
			return fmt.Errorf("migration ledger version %d target_version %d mismatch", version, targetVersion)
		}
		if sourceVersion != version-1 {
			return fmt.Errorf("migration ledger version %d source_version %d is not contiguous", version, sourceVersion)
		}
		if verificationResult != "verified" {
			return fmt.Errorf("migration ledger version %d is not verified: %q", version, verificationResult)
		}
		if version == 1 {
			if migrationID != foundationMigrationID {
				return fmt.Errorf("foundation migration id = %q, want %q", migrationID, foundationMigrationID)
			}
			if checksum != foundationChecksum() {
				return fmt.Errorf("foundation migration checksum drift for version 1")
			}
		}
		lastTarget = targetVersion
		expected++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate migration ledger: %w", err)
	}
	if lastTarget != currentVersion {
		return fmt.Errorf("migration ledger ends at version %d, want %d", lastTarget, currentVersion)
	}
	return nil
}

type schemaQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func foundationChecksum() string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{
		foundationMigrationID,
		foundationMigrationName,
		fmt.Sprintf("target=%d", CurrentSchemaVersion),
	}, foundationSchemaStatements...), "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newStoreID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func normalizeNow(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTimestamp(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func unsupportedNewVersionError(version int) error {
	return fmt.Errorf("%w: unsupported store schema version %d; this binary supports up to schema version %d", ErrIncompatibleVersion, version, CurrentSchemaVersion)
}

func unsupportedOldVersionError(version int) error {
	return fmt.Errorf("%w: unsupported store schema version %d; compatibility floor is %d", ErrIncompatibleVersion, version, CompatibilityFloor)
}
