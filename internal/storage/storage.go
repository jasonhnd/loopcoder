// Package storage provides the internal loopcoder runtime storage boundary.
package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/gitremote"
	"github.com/jasonhnd/loopcoder/internal/pathid"

	moderncsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// CurrentSchemaVersion is the newest SQLite schema version this binary can use.
	CurrentSchemaVersion = 33

	driverName = "sqlite"

	rollbackTimeout = 5 * time.Second

	defaultWriteTxRetryMaxAttempts = 8
	defaultWriteTxRetryMaxElapsed  = 6 * time.Second
)

// Store is the internal storage interface for v0.7 runtime state.
type Store interface {
	Close() error
	Path() string
	Now() time.Time
	Health(context.Context) (Health, error)
	WithTx(context.Context, func(Tx) error) error
	WithWriteTx(context.Context, func(Tx) error) error
}

// Tx is the storage transaction boundary exposed to internal callers.
type Tx interface {
	Exec(context.Context, string, ...any) (sql.Result, error)
	Query(context.Context, string, ...any) (*sql.Rows, error)
	QueryRow(context.Context, string, ...any) *sql.Row
}

// Options controls SQLite store opening.
type Options struct {
	Path string
	Now  func() time.Time

	BusyTimeout time.Duration

	WriteTxCommitHookForTest WriteTxCommitHookForTest
	WriteTxRetry             WriteTxRetryOptions

	deliveryV10BackupHookForTest          deliveryV10BackupHookForTest
	deliveryV10BackupBufferFactoryForTest func() []byte
}

// WriteTxCommitHookForTest lets tests inject deterministic failures at the
// final write transaction boundary without sharing mutable package state.
type WriteTxCommitHookForTest func(context.Context, Tx, func(context.Context) error) error

// WriteTxRetryOptions controls bounded whole-transaction retry for SQLite
// BUSY/LOCKED contention. Zero values use the production defaults.
type WriteTxRetryOptions struct {
	MaxAttempts int
	MaxElapsed  time.Duration
	Backoff     func(attempt int) time.Duration
	Clock       WriteTxRetryClock
}

// WriteTxRetryClock lets tests advance retry time without real sleeps.
type WriteTxRetryClock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type deliveryV10BackupPhase string

const (
	deliveryV10BackupPhaseBeforeVacuum deliveryV10BackupPhase = "before-vacuum"
	deliveryV10BackupPhaseAfterVacuum  deliveryV10BackupPhase = "after-vacuum"
	deliveryV10BackupPhaseAfterHash    deliveryV10BackupPhase = "after-hash"
	deliveryV10BackupPhaseBeforeRename deliveryV10BackupPhase = "before-rename"
)

// deliveryV10BackupHookForTest lets tests inject cancellation or faults at
// deterministic backup boundaries without package-global mutable state.
type deliveryV10BackupHookForTest func(context.Context, deliveryV10BackupPhase, string) error

// Health reports the local database state without exposing table internals.
type Health struct {
	Path          string `json:"path"`
	Exists        bool   `json:"exists"`
	SchemaVersion int    `json:"schema_version"`
	OK            bool   `json:"ok"`
	Message       string `json:"message"`
}

type sqliteStore struct {
	path                                  string
	db                                    *sql.DB
	now                                   func() time.Time
	busyTimeout                           time.Duration
	writeTxCommitHookForTest              WriteTxCommitHookForTest
	writeTxRetry                          writeTxRetryPolicy
	sourceExistedBeforeOpen               bool
	deliveryV10BackupHookForTest          deliveryV10BackupHookForTest
	deliveryV10BackupBufferFactoryForTest func() []byte
}

type sqlTx struct {
	tx *sql.Tx
}

type sqlConnTx struct {
	conn *sql.Conn
}

type writeTxRetryPolicy struct {
	maxAttempts        int
	maxElapsed         time.Duration
	backoff            func(attempt int) time.Duration
	clock              WriteTxRetryClock
	useAttemptDeadline bool
}

type realWriteTxRetryClock struct{}

var rollbackConnTxHookForTest func(*sql.Conn) error

var migrations = []migration{
	{
		version: 1,
		name:    "initial runtime schema",
		statements: []string{
			`CREATE TABLE IF NOT EXISTS migrations (
				version INTEGER PRIMARY KEY,
				name TEXT NOT NULL,
				applied_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS projects (
				id TEXT PRIMARY KEY,
				remote_url TEXT NOT NULL DEFAULT '',
				github_owner TEXT NOT NULL DEFAULT '',
				github_name TEXT NOT NULL DEFAULT '',
				local_path TEXT NOT NULL,
				default_branch TEXT NOT NULL DEFAULT '',
				display_name TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_projects_local_path ON projects(local_path)`,
			`CREATE TABLE IF NOT EXISTS runs (
				id TEXT PRIMARY KEY,
				project_id TEXT REFERENCES projects(id) ON DELETE SET NULL,
				parent_run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
				issue_number INTEGER,
				status TEXT NOT NULL DEFAULT '',
				started_at TEXT,
				ended_at TEXT,
				updated_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_runs_project_id ON runs(project_id)`,
			`CREATE INDEX IF NOT EXISTS idx_runs_parent_run_id ON runs(parent_run_id)`,
			`CREATE TABLE IF NOT EXISTS run_events (
				id TEXT PRIMARY KEY,
				run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
				sequence INTEGER NOT NULL,
				ts TEXT NOT NULL,
				event_type TEXT NOT NULL,
				payload_json TEXT NOT NULL DEFAULT '{}',
				UNIQUE(run_id, sequence)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_run_events_run_id_sequence ON run_events(run_id, sequence)`,
			`CREATE TABLE IF NOT EXISTS run_edges (
				parent_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
				child_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
				edge_type TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				PRIMARY KEY(parent_run_id, child_run_id)
			)`,
			`CREATE TABLE IF NOT EXISTS reports (
				id TEXT PRIMARY KEY,
				run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
				role TEXT NOT NULL,
				provider TEXT NOT NULL DEFAULT '',
				model TEXT NOT NULL DEFAULT '',
				started_at TEXT,
				ended_at TEXT,
				payload_json TEXT NOT NULL,
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_reports_run_id ON reports(run_id)`,
		},
	},
	{
		version: 2,
		name:    "project identity fields",
		statements: []string{
			`ALTER TABLE projects ADD COLUMN local_path_canonical TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE projects ADD COLUMN git_root TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE projects ADD COLUMN remote_url_normalized TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE projects ADD COLUMN identity_source TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_projects_local_path_canonical ON projects(local_path_canonical)`,
			`CREATE INDEX IF NOT EXISTS idx_projects_remote_url_normalized ON projects(remote_url_normalized)`,
		},
	},
	{
		version: 3,
		name:    "legacy local state import metadata",
		statements: []string{
			`ALTER TABLE reports ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE reports ADD COLUMN source_path TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE reports ADD COLUMN source_hash TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE reports ADD COLUMN source_kind TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_reports_project_id ON reports(project_id)`,
			`CREATE INDEX IF NOT EXISTS idx_reports_source_hash ON reports(source_hash)`,
			`CREATE TABLE IF NOT EXISTS legacy_import_records (
				id TEXT PRIMARY KEY,
				project_id TEXT NOT NULL REFERENCES projects(id),
				run_id TEXT,
				record_type TEXT NOT NULL,
				source_path TEXT NOT NULL,
				source_line INTEGER NOT NULL DEFAULT 0,
				source_hash TEXT NOT NULL,
				payload_json TEXT NOT NULL DEFAULT '{}',
				imported_at TEXT NOT NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_legacy_import_records_source ON legacy_import_records(project_id, record_type, source_path, source_line, source_hash)`,
			`CREATE INDEX IF NOT EXISTS idx_legacy_import_records_project ON legacy_import_records(project_id)`,
			`CREATE TABLE IF NOT EXISTS legacy_import_status (
				project_id TEXT PRIMARY KEY REFERENCES projects(id),
				repo_path TEXT NOT NULL,
				started_at TEXT NOT NULL,
				completed_at TEXT NOT NULL,
				status TEXT NOT NULL,
				scanned_count INTEGER NOT NULL DEFAULT 0,
				imported_count INTEGER NOT NULL DEFAULT 0,
				skipped_count INTEGER NOT NULL DEFAULT 0,
				malformed_count INTEGER NOT NULL DEFAULT 0,
				message TEXT NOT NULL DEFAULT ''
			)`,
		},
	},
	{
		version: 4,
		name:    "scrub project remote urls",
		apply:   scrubProjectRemoteURLs,
	},
	{
		version: 5,
		name:    "preserve project history on registry removal",
		statements: []string{
			`ALTER TABLE projects ADD COLUMN detached_at TEXT NOT NULL DEFAULT ''`,
		},
		apply: rebuildLegacyImportTablesWithoutCascade,
	},
	{
		version: 6,
		name:    "reconcile physical project identities",
		apply:   reconcilePhysicalProjectIdentities,
	},
	{
		version: 7,
		name:    "nested child plans and durable run graph",
		statements: []string{
			`ALTER TABLE runs ADD COLUMN root_run_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE runs ADD COLUMN depth INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE runs ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE runs ADD COLUMN created_at TEXT NOT NULL DEFAULT ''`,
			`UPDATE runs SET root_run_id = CASE WHEN parent_run_id IS NULL OR parent_run_id = '' THEN id ELSE parent_run_id END WHERE root_run_id = ''`,
			`UPDATE runs SET created_at = COALESCE(NULLIF(started_at, ''), updated_at) WHERE created_at = ''`,
			`CREATE INDEX IF NOT EXISTS idx_runs_root_run_id ON runs(root_run_id)`,
			`CREATE INDEX IF NOT EXISTS idx_runs_depth ON runs(depth)`,
			`CREATE TABLE IF NOT EXISTS child_plans (
				plan_id TEXT PRIMARY KEY,
				parent_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
				root_run_id TEXT NOT NULL,
				schema_version TEXT NOT NULL,
				max_depth INTEGER NOT NULL,
				max_concurrency INTEGER NOT NULL,
				plan_json TEXT NOT NULL,
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_child_plans_parent_run_id ON child_plans(parent_run_id)`,
			`CREATE INDEX IF NOT EXISTS idx_child_plans_root_run_id ON child_plans(root_run_id)`,
			`ALTER TABLE run_edges ADD COLUMN root_run_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE run_edges ADD COLUMN plan_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE run_edges ADD COLUMN child_key TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE run_edges ADD COLUMN depth INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE run_edges ADD COLUMN ordinal INTEGER NOT NULL DEFAULT -1`,
			`ALTER TABLE run_edges ADD COLUMN scope_json TEXT NOT NULL DEFAULT '{}'`,
			`ALTER TABLE run_edges ADD COLUMN permission TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE run_edges ADD COLUMN aggregation_json TEXT NOT NULL DEFAULT '{}'`,
			`ALTER TABLE run_edges ADD COLUMN status TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE run_edges ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''`,
			`UPDATE run_edges SET updated_at = created_at WHERE updated_at = ''`,
			`CREATE INDEX IF NOT EXISTS idx_run_edges_root_run_id ON run_edges(root_run_id)`,
			`CREATE INDEX IF NOT EXISTS idx_run_edges_plan_id ON run_edges(plan_id)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_run_edges_plan_child_key ON run_edges(plan_id, child_key) WHERE plan_id <> '' AND child_key <> ''`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_run_edges_parent_ordinal ON run_edges(parent_run_id, ordinal) WHERE ordinal >= 0`,
		},
	},
	{
		version: 8,
		name:    "nested child execution claims",
		statements: []string{
			`CREATE TABLE IF NOT EXISTS run_claims (
				run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
				executor_id TEXT NOT NULL,
				claim_generation INTEGER NOT NULL,
				claimed_at TEXT NOT NULL,
				lease_expires_at TEXT NOT NULL,
				heartbeat_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_run_claims_executor_id ON run_claims(executor_id)`,
			`CREATE INDEX IF NOT EXISTS idx_run_claims_lease_expires_at ON run_claims(lease_expires_at)`,
		},
	},
	{
		version: 9,
		name:    "nested child claim lifecycle phase",
		statements: []string{
			`ALTER TABLE run_claims ADD COLUMN phase TEXT NOT NULL DEFAULT 'claimed'`,
			`ALTER TABLE run_claims ADD COLUMN provider_idempotency_key TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE run_claims ADD COLUMN provider_receipt TEXT NOT NULL DEFAULT ''`,
			`UPDATE run_claims
				SET phase = CASE
					WHEN EXISTS (
						SELECT 1 FROM runs r
						WHERE r.id = run_claims.run_id
							AND LOWER(TRIM(COALESCE(r.status, ''))) IN ('succeeded', 'succeeded_with_optional_failures', 'failed', 'cancelled', 'timed_out', 'abandoned', 'needs-human', 'skipped', 'hung', 'idle', 'blocked')
					) THEN 'completed'
					ELSE 'executing'
				END
				WHERE phase = '' OR phase = 'claimed'`,
		},
	},
	{
		version: 10,
		name:    "delivery run contracts",
	},
	{
		version:    11,
		name:       "provider inventory",
		statements: providerInventorySchemaStatements,
	},
	{
		version:    12,
		name:       "quota telemetry",
		statements: quotaTelemetrySchemaStatements,
	},
	{
		version:    13,
		name:       "usage ledger",
		statements: usageLedgerSchemaStatements,
	},
	{
		version:    14,
		name:       "hierarchical budget accounting",
		statements: budgetSchemaStatements,
	},
	{
		version:    15,
		name:       "scoped budget reservation idempotency",
		statements: budgetSchemaV15Statements,
	},
	{
		version:    16,
		name:       "task requirement classification",
		statements: taskRequirementSchemaStatements,
	},
	{
		version:    17,
		name:       "agent federation registration",
		statements: agentFederationSchemaStatements,
	},
	{
		version:    18,
		name:       "availability read model",
		statements: availabilitySchemaStatements,
	},
	{
		version: 19,
		name:    "bounded circuit breaker recovery",
		apply:   migrateCircuitBreakerRecovery,
	},
	{
		version:    20,
		name:       "accepted task graph versions",
		statements: acceptedTaskGraphSchemaStatements,
	},
	{
		version:    21,
		name:       "explainable routing decisions",
		statements: routingDecisionSchemaStatements,
	},
	{
		version:    22,
		name:       "versioned routing policy profiles",
		statements: routingPolicyProfileSchemaStatements,
	},
	{
		version:    23,
		name:       "fallback and replan decisions",
		statements: fallbackReplanSchemaStatements,
	},
	{
		version:    24,
		name:       "independent verification decisions",
		statements: verificationDecisionSchemaStatements,
	},
	{
		version:    25,
		name:       "canonical progress receipts",
		statements: progressReceiptSchemaStatements,
	},
	{
		version:    26,
		name:       "durable handoff transactions",
		statements: handoffSchemaStatements,
	},
	{
		version:    27,
		name:       "nested scheduler resource reservations",
		statements: nestedSchedulerReservationSchemaStatements,
	},
	{
		version:    28,
		name:       "progress delivery outbox",
		statements: progressDeliveryOutboxSchemaStatements,
	},
	{
		version:    29,
		name:       "detached run supervisors",
		statements: detachedRunSupervisorSchemaStatements,
	},
	{
		version:    30,
		name:       "provider execution authority",
		statements: providerExecutionAuthoritySchemaStatements,
	},
	{
		version:    31,
		name:       "versioned child execution requests",
		statements: childExecutionRequestSchemaStatements,
	},
	{
		version:    32,
		name:       "v0.9 work graph versions and items",
		statements: workGraphSchemaStatements,
	},
	{
		version: 33,
		name:    "provider execution authority spawn_phase",
		apply:   migrateProviderExecutionSpawnPhase,
	},
}

var requiredTables = []string{
	"migrations",
	"projects",
	"runs",
	"run_events",
	"run_edges",
	"reports",
	"child_plans",
	"run_claims",
	"legacy_import_records",
	"legacy_import_status",
	"delivery_runs",
	"delivery_tasks",
	"delivery_dependency_edges",
	"delivery_attempts",
	"delivery_decisions",
	"delivery_approvals",
	"delivery_overrides",
	"delivery_plan_fingerprints",
	"delivery_idempotency",
	"delivery_migration_backups",
	"delivery_legacy_imports",
	"adapter_declarations",
	"provider_installations",
	"provider_probe_results",
	"account_profiles",
	"auth_readiness",
	"model_catalog_snapshots",
	"model_capabilities",
	"quota_telemetry_sources",
	"quota_snapshots",
	"usage_records",
	"usage_reconciliations",
	"budget_policies",
	"budget_reservations",
	"budget_aggregates",
	"quota_budget_events",
	"inventory_events",
	"task_requirements",
	"task_requirement_overrides",
	"task_graph_validations",
	"accepted_task_graph_versions",
	"routing_decisions",
	"role_definitions",
	"routing_policy_profiles",
	"routing_policy_inputs",
	"routing_legacy_model_mappings",
	"routing_events",
	"fallback_decisions",
	"replan_decisions",
	"verification_decisions",
	"progress_receipts",
	"progress_delivery_obligations",
	"progress_delivery_attempts",
	"progress_delivery_attempt_results",
	"progress_delivery_acknowledgments",
	"progress_delivery_replay_cursors",
	"detached_run_supervisors",
	"provider_execution_authorities",
	"child_execution_requests",
	"handoff_transactions",
	"nested_scheduler_resource_reservations",
	"agent_scope_grants",
	"agent_registrations",
	"agent_ownership_locks",
	"agent_budget_bindings",
	"agent_events",
	"availability_observations",
	"availability_scores",
	"circuit_breakers",
	"workgraph_versions",
	"workgraph_items",
	"workgraph_dependencies",
	"workgraph_events",
}

type migration struct {
	version    int
	name       string
	statements []string
	apply      func(context.Context, *sql.Tx) error
}

// Open opens or creates the SQLite-backed store and applies supported migrations.
func Open(ctx context.Context, opts Options) (Store, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, errors.New("open storage: path is required")
	}
	path = filepath.Clean(path)
	sourceExistedBeforeOpen := true
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			sourceExistedBeforeOpen = false
		} else {
			return nil, fmt.Errorf("open storage: inspect source database %s: %w", path, err)
		}
	}

	var store *sqliteStore
	err := withOwnerOnlyUmask(func() error {
		if err := ensurePermissionsForOpen(path); err != nil {
			return fmt.Errorf("open storage: secure permissions for %s: %w", path, err)
		}
		db, err := sql.Open(driverName, path)
		if err != nil {
			return fmt.Errorf("open storage %s: %w", path, err)
		}
		db.SetMaxOpenConns(1)
		opened := &sqliteStore{
			path:                                  path,
			db:                                    db,
			now:                                   normalizeNow(opts.Now),
			busyTimeout:                           opts.BusyTimeout,
			writeTxCommitHookForTest:              opts.WriteTxCommitHookForTest,
			writeTxRetry:                          normalizeWriteTxRetryPolicy(opts.WriteTxRetry),
			sourceExistedBeforeOpen:               sourceExistedBeforeOpen,
			deliveryV10BackupHookForTest:          opts.deliveryV10BackupHookForTest,
			deliveryV10BackupBufferFactoryForTest: opts.deliveryV10BackupBufferFactoryForTest,
		}
		if err := opened.configure(ctx); err != nil {
			_ = db.Close()
			return err
		}
		if err := opened.migrate(ctx); err != nil {
			_ = db.Close()
			return err
		}
		if err := hardenSQLiteSidecars(path); err != nil {
			_ = db.Close()
			return fmt.Errorf("open storage: secure sqlite sidecars for %s: %w", path, err)
		}
		store = opened
		return nil
	})
	if err != nil {
		return nil, err
	}
	return store, nil
}

// CheckHealth inspects an existing database path without creating or migrating it.
func CheckHealth(ctx context.Context, path string) (Health, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path = filepath.Clean(strings.TrimSpace(path))
	health := Health{Path: path}
	if path == "." || path == "" {
		return health, errors.New("storage health: path is required")
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			health.Message = "database has not been created"
			return health, nil
		}
		return health, fmt.Errorf("storage health: inspect %s: %w", path, err)
	}
	health.Exists = true
	permissions, err := CheckPermissions(path)
	if err != nil {
		return health, fmt.Errorf("storage health: inspect permissions for %s: %w", path, err)
	}
	if unsafe := firstUnsafePermissionItem(permissions); unsafe != nil {
		return health, fmt.Errorf("storage health: unsafe storage path %s: %s", unsafe.Path, unsafe.Message)
	}

	db, err := sql.Open(driverName, path)
	if err != nil {
		return health, fmt.Errorf("storage health: open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return health, fmt.Errorf("storage health: enable foreign keys: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		return health, fmt.Errorf("storage health: enable read-only mode: %w", err)
	}
	version, err := schemaVersion(ctx, db)
	if err != nil {
		return health, fmt.Errorf("storage health: %w", err)
	}
	health.SchemaVersion = version
	if version > CurrentSchemaVersion {
		return health, unsupportedVersionError(version)
	}
	if version == 0 {
		return health, errors.New("storage health: migrations table is missing or empty; run a newer loopcoder storage migration")
	}
	if err := checkRequiredTables(ctx, db); err != nil {
		return health, fmt.Errorf("storage health: %w", err)
	}
	if err := integrityCheck(ctx, db); err != nil {
		return health, fmt.Errorf("storage health: %w", err)
	}
	if err := checkDurableRunGraph(ctx, db); err != nil {
		return health, fmt.Errorf("storage health: %w", err)
	}
	health.OK = true
	health.Message = "storage database is healthy"
	return health, nil
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

func (s *sqliteStore) Path() string {
	return s.path
}

func (s *sqliteStore) Now() time.Time {
	return s.now().UTC()
}

func (s *sqliteStore) Health(ctx context.Context) (Health, error) {
	return CheckHealth(ctx, s.path)
}

func (s *sqliteStore) WithTx(ctx context.Context, fn func(Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return errors.New("storage transaction: callback is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage transaction: begin: %w", err)
	}
	wrapped := sqlTx{tx: tx}
	if err := fn(wrapped); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return fmt.Errorf("storage transaction: rollback after %v: %w", err, rollbackErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage transaction: commit: %w", err)
	}
	return nil
}

func (s *sqliteStore) WithWriteTx(ctx context.Context, fn func(Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return errors.New("storage write transaction: callback is required")
	}
	return retryWriteTx(ctx, s.writeTxRetry, func(attemptCtx context.Context) error {
		return s.withWriteTxOnce(attemptCtx, fn)
	})
}

func (s *sqliteStore) withWriteTxOnce(ctx context.Context, fn func(Tx) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("storage write transaction: connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("storage write transaction: begin immediate: %w", err)
	}
	wrapped := sqlConnTx{conn: conn}
	if err := fn(wrapped); err != nil {
		if rollbackErr := rollbackConnTx(conn); rollbackErr != nil {
			_ = discardConn(conn)
			return fmt.Errorf("storage write transaction: rollback after %v: %w", err, rollbackErr)
		}
		return err
	}
	commit := func(commitCtx context.Context) error {
		_, err := conn.ExecContext(commitCtx, `COMMIT`)
		return err
	}
	var commitErr error
	if s.writeTxCommitHookForTest != nil {
		commitErr = s.writeTxCommitHookForTest(ctx, wrapped, commit)
	} else {
		commitErr = commit(ctx)
	}
	if commitErr != nil {
		if rollbackErr := rollbackConnTx(conn); rollbackErr != nil {
			_ = discardConn(conn)
		}
		return fmt.Errorf("storage write transaction: commit: %w", commitErr)
	}
	return nil
}

func normalizeWriteTxRetryPolicy(opts WriteTxRetryOptions) writeTxRetryPolicy {
	policy := writeTxRetryPolicy{
		maxAttempts: opts.MaxAttempts,
		maxElapsed:  opts.MaxElapsed,
		backoff:     opts.Backoff,
		clock:       opts.Clock,
	}
	if policy.maxAttempts <= 0 {
		policy.maxAttempts = defaultWriteTxRetryMaxAttempts
	}
	if policy.maxElapsed <= 0 {
		policy.maxElapsed = defaultWriteTxRetryMaxElapsed
	}
	if policy.backoff == nil {
		policy.backoff = defaultWriteTxRetryBackoff
	}
	if policy.clock == nil {
		policy.clock = realWriteTxRetryClock{}
		policy.useAttemptDeadline = true
	}
	return policy
}

func defaultWriteTxRetryBackoff(attempt int) time.Duration {
	return time.Duration(attempt) * 25 * time.Millisecond
}

func retryWriteTx(ctx context.Context, policy writeTxRetryPolicy, op func(context.Context) error) error {
	var err error
	var lastBusyErr error
	var deadline time.Time
	retryWindowStarted := false
	for attempt := 1; attempt <= policy.maxAttempts; attempt++ {
		if retryWindowStarted && policy.maxElapsed > 0 && !policy.clock.Now().Before(deadline) {
			return lastBusyErr
		}
		attemptCtx := ctx
		cancel := func() {}
		attemptHasInternalDeadline := false
		if retryWindowStarted && policy.useAttemptDeadline && policy.maxElapsed > 0 {
			attemptCtx, cancel = context.WithDeadline(ctx, deadline)
			attemptHasInternalDeadline = true
		}
		err = op(attemptCtx)
		attemptCtxErr := attemptCtx.Err()
		cancel()
		internalAttemptDeadlineExpired := attemptHasInternalDeadline && errors.Is(attemptCtxErr, context.DeadlineExceeded)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && lastBusyErr != nil && internalAttemptDeadlineExpired {
			return lastBusyErr
		}
		if err == nil || !IsBusy(err) {
			return err
		}
		lastBusyErr = err
		if !retryWindowStarted {
			retryWindowStarted = true
			if policy.maxElapsed > 0 {
				deadline = policy.clock.Now().Add(policy.maxElapsed)
			}
		}
		if attempt == policy.maxAttempts {
			return err
		}
		if policy.maxElapsed > 0 && !policy.clock.Now().Before(deadline) {
			return err
		}
		delay := policy.backoff(attempt)
		if delay < 0 {
			delay = 0
		}
		if policy.maxElapsed > 0 {
			remaining := deadline.Sub(policy.clock.Now())
			if remaining <= 0 {
				return err
			}
			if delay > remaining {
				delay = remaining
			}
		}
		if delay == 0 {
			continue
		}
		if sleepErr := policy.clock.Sleep(ctx, delay); sleepErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return sleepErr
		}
	}
	return err
}

func (realWriteTxRetryClock) Now() time.Time {
	return time.Now()
}

func (realWriteTxRetryClock) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func rollbackConnTx(conn *sql.Conn) error {
	if rollbackConnTxHookForTest != nil {
		return rollbackConnTxHookForTest(conn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	_, err := conn.ExecContext(ctx, `ROLLBACK`)
	return err
}

func discardConn(conn *sql.Conn) error {
	return conn.Raw(func(any) error {
		return driver.ErrBadConn
	})
}

func withRetry(ctx context.Context, op func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		err = op()
		if err == nil || !IsBusy(err) {
			return err
		}
		delay := time.Duration(attempt+1) * 25 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return err
}

func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *moderncsqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		default:
			return false
		}
	}
	return false
}

func IsConstraint(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *moderncsqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code()&0xff == sqlite3.SQLITE_CONSTRAINT
	}
	return false
}

func (tx sqlTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.tx.ExecContext(ctx, query, args...)
}

func (tx sqlTx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.tx.QueryContext(ctx, query, args...)
}

func (tx sqlTx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.tx.QueryRowContext(ctx, query, args...)
}

func (tx sqlConnTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(ctx, query, args...)
}

func (tx sqlConnTx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(ctx, query, args...)
}

func (tx sqlConnTx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(ctx, query, args...)
}

func (s *sqliteStore) configure(ctx context.Context) error {
	busyTimeout := s.busyTimeout
	if busyTimeout <= 0 {
		busyTimeout = 5 * time.Second
	}
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		fmt.Sprintf(`PRAGMA busy_timeout = %d`, busyTimeout.Milliseconds()),
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("open storage %s: configure sqlite: %w", s.path, err)
		}
	}
	return nil
}

func (s *sqliteStore) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("migrate storage: initialize migrations table: %w", err)
	}
	version, err := schemaVersion(ctx, s.db)
	if err != nil {
		return fmt.Errorf("migrate storage: %w", err)
	}
	if version > CurrentSchemaVersion {
		return unsupportedVersionError(version)
	}
	var deliveryV10Backup *deliveryMigrationBackup
	if CurrentSchemaVersion >= 10 {
		switch {
		case version == 9:
			deliveryV10Backup, err = prepareDeliveryV10Backup(ctx, s.db, s.path, formatTimestamp(s.now()), deliveryV10BackupOptions{
				hook:          s.deliveryV10BackupHookForTest,
				bufferFactory: s.deliveryV10BackupBufferFactoryForTest,
			})
		case version < 10 && !s.sourceExistedBeforeOpen:
			deliveryV10Backup = prepareDeliveryV10NoSourceMetadata(s.path, formatTimestamp(s.now()))
		}
		if err != nil {
			return err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate storage: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("migrate storage: initialize migrations table: %w", err)
	}
	version, err = txSchemaVersion(ctx, tx)
	if err != nil {
		return fmt.Errorf("migrate storage: %w", err)
	}
	if version > CurrentSchemaVersion {
		return unsupportedVersionError(version)
	}

	for _, migration := range migrations {
		if migration.version <= version {
			continue
		}
		for _, statement := range migration.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate storage to version %d: %w", migration.version, err)
			}
		}
		if migration.version == 10 {
			if err := migrateDeliveryRunContracts(ctx, tx, deliveryV10Backup, formatTimestamp(s.now())); err != nil {
				return fmt.Errorf("migrate storage to version %d: %w", migration.version, err)
			}
		}
		if migration.apply != nil {
			if err := migration.apply(ctx, tx); err != nil {
				return fmt.Errorf("migrate storage to version %d: %w", migration.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO migrations(version, name, applied_at) VALUES (?, ?, ?)`,
			migration.version, migration.name, formatTimestamp(s.now())); err != nil {
			return fmt.Errorf("record storage migration %d: %w", migration.version, err)
		}
	}
	if err := checkRequiredTables(ctx, tx); err != nil {
		return fmt.Errorf("migrate storage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate storage: commit: %w", err)
	}
	return nil
}

func scrubProjectRemoteURLs(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, remote_url, remote_url_normalized FROM projects`)
	if err != nil {
		return err
	}
	type projectRemote struct {
		id         string
		display    string
		normalized string
	}
	var updates []projectRemote
	for rows.Next() {
		var current projectRemote
		if err := rows.Scan(&current.id, &current.display, &current.normalized); err != nil {
			if closeErr := rows.Close(); closeErr != nil {
				return fmt.Errorf("close project remote rows after scan error: %v: %w", closeErr, err)
			}
			return err
		}
		nextDisplay, _ := gitremote.SanitizeDisplayURL(current.display)
		nextNormalized, _, _, normalizedOK := gitremote.NormalizeURL(current.normalized)
		if !normalizedOK {
			nextNormalized, _, _, normalizedOK = gitremote.NormalizeURL(nextDisplay)
		}
		if !normalizedOK {
			nextNormalized = ""
		}
		if current.display != nextDisplay || current.normalized != nextNormalized {
			updates = append(updates, projectRemote{
				id:         current.id,
				display:    nextDisplay,
				normalized: nextNormalized,
			})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET remote_url = ?, remote_url_normalized = ? WHERE id = ?`, update.display, update.normalized, update.id); err != nil {
			return err
		}
	}
	return nil
}

func rebuildLegacyImportTablesWithoutCascade(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE legacy_import_records_v5 (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id),
			run_id TEXT,
			record_type TEXT NOT NULL,
			source_path TEXT NOT NULL,
			source_line INTEGER NOT NULL DEFAULT 0,
			source_hash TEXT NOT NULL,
			payload_json TEXT NOT NULL DEFAULT '{}',
			imported_at TEXT NOT NULL
		)`,
		`INSERT INTO legacy_import_records_v5(id, project_id, run_id, record_type, source_path, source_line, source_hash, payload_json, imported_at)
			SELECT id, project_id, run_id, record_type, source_path, source_line, source_hash, payload_json, imported_at FROM legacy_import_records`,
		`DROP TABLE legacy_import_records`,
		`ALTER TABLE legacy_import_records_v5 RENAME TO legacy_import_records`,
		`CREATE UNIQUE INDEX idx_legacy_import_records_source ON legacy_import_records(project_id, record_type, source_path, source_line, source_hash)`,
		`CREATE INDEX idx_legacy_import_records_project ON legacy_import_records(project_id)`,
		`CREATE TABLE legacy_import_status_v5 (
			project_id TEXT PRIMARY KEY REFERENCES projects(id),
			repo_path TEXT NOT NULL,
			started_at TEXT NOT NULL,
			completed_at TEXT NOT NULL,
			status TEXT NOT NULL,
			scanned_count INTEGER NOT NULL DEFAULT 0,
			imported_count INTEGER NOT NULL DEFAULT 0,
			skipped_count INTEGER NOT NULL DEFAULT 0,
			malformed_count INTEGER NOT NULL DEFAULT 0,
			message TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO legacy_import_status_v5(project_id, repo_path, started_at, completed_at, status, scanned_count, imported_count, skipped_count, malformed_count, message)
			SELECT project_id, repo_path, started_at, completed_at, status, scanned_count, imported_count, skipped_count, malformed_count, message FROM legacy_import_status`,
		`DROP TABLE legacy_import_status`,
		`ALTER TABLE legacy_import_status_v5 RENAME TO legacy_import_status`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

type storedProjectIdentity struct {
	id         string
	canonical  string
	localPath  string
	gitRoot    string
	createdAt  string
	detachedAt string
}

func reconcilePhysicalProjectIdentities(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, local_path_canonical, local_path, git_root, created_at, detached_at FROM projects ORDER BY id`)
	if err != nil {
		return err
	}
	var projects []storedProjectIdentity
	for rows.Next() {
		var project storedProjectIdentity
		if err := rows.Scan(&project.id, &project.canonical, &project.localPath, &project.gitRoot, &project.createdAt, &project.detachedAt); err != nil {
			if closeErr := rows.Close(); closeErr != nil {
				return fmt.Errorf("close project identity rows after scan error: %v: %w", closeErr, err)
			}
			return err
		}
		physical := physicalProjectCanonical(firstNonEmpty(project.gitRoot, project.canonical, project.localPath))
		if physical != "" {
			project.canonical = physical
			projects = append(projects, project)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}

	byCanonical := map[string][]storedProjectIdentity{}
	for _, project := range projects {
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET local_path_canonical = ? WHERE id = ?`, project.canonical, project.id); err != nil {
			return err
		}
		byCanonical[project.canonical] = append(byCanonical[project.canonical], project)
	}
	for canonical, group := range byCanonical {
		if len(group) < 2 {
			continue
		}
		sortStoredProjects(group)
		survivor := group[0]
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET local_path_canonical = ?, detached_at = '' WHERE id = ?`, canonical, survivor.id); err != nil {
			return err
		}
		for _, duplicate := range group[1:] {
			if err := mergeStoredProjectHistory(ctx, tx, duplicate.id, survivor.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func physicalProjectCanonical(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	identity, err := pathid.Identity(path)
	if err != nil {
		clean := filepath.Clean(path)
		if abs, absErr := filepath.Abs(clean); absErr == nil {
			clean = filepath.Clean(abs)
		}
		return clean
	}
	return identity
}

func sortStoredProjects(projects []storedProjectIdentity) {
	sort.Slice(projects, func(i, j int) bool {
		leftDetached := strings.TrimSpace(projects[i].detachedAt) != ""
		rightDetached := strings.TrimSpace(projects[j].detachedAt) != ""
		if leftDetached != rightDetached {
			return !leftDetached
		}
		if projects[i].createdAt != projects[j].createdAt {
			return projects[i].createdAt < projects[j].createdAt
		}
		return projects[i].id < projects[j].id
	})
}

func mergeStoredProjectHistory(ctx context.Context, tx *sql.Tx, fromID, toID string) error {
	if fromID == toID {
		return nil
	}
	for _, statement := range []string{
		`UPDATE runs SET project_id = ? WHERE project_id = ?`,
		`UPDATE reports SET project_id = ? WHERE project_id = ?`,
		`UPDATE OR IGNORE legacy_import_records SET project_id = ? WHERE project_id = ?`,
		`DELETE FROM legacy_import_records WHERE project_id = ?`,
	} {
		args := []any{toID, fromID}
		if strings.HasPrefix(statement, `DELETE`) {
			args = []any{fromID}
		}
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			return err
		}
	}
	if err := mergeStoredLegacyImportStatus(ctx, tx, fromID, toID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, fromID); err != nil {
		return err
	}
	return nil
}

func mergeStoredLegacyImportStatus(ctx context.Context, tx *sql.Tx, fromID, toID string) error {
	var survivorCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM legacy_import_status WHERE project_id = ?`, toID).Scan(&survivorCount); err != nil {
		return err
	}
	if survivorCount == 0 {
		_, err := tx.ExecContext(ctx, `UPDATE legacy_import_status SET project_id = ? WHERE project_id = ?`, toID, fromID)
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE legacy_import_status SET
		scanned_count = scanned_count + COALESCE((SELECT scanned_count FROM legacy_import_status WHERE project_id = ?), 0),
		imported_count = imported_count + COALESCE((SELECT imported_count FROM legacy_import_status WHERE project_id = ?), 0),
		skipped_count = skipped_count + COALESCE((SELECT skipped_count FROM legacy_import_status WHERE project_id = ?), 0),
		malformed_count = malformed_count + COALESCE((SELECT malformed_count FROM legacy_import_status WHERE project_id = ?), 0),
		message = CASE WHEN message = '' THEN 'reconciled duplicate physical project identity' ELSE message || '; reconciled duplicate physical project identity' END
		WHERE project_id = ?`, fromID, fromID, fromID, fromID, toID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM legacy_import_status WHERE project_id = ?`, fromID)
	return err
}

func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'migrations'`).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, nil
	}
	return querySchemaVersion(ctx, db)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func txSchemaVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	return querySchemaVersion(ctx, tx)
}

type schemaVersionQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func querySchemaVersion(ctx context.Context, q schemaVersionQuerier) (int, error) {
	var version int
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

func checkRequiredTables(ctx context.Context, q schemaVersionQuerier) error {
	for _, table := range requiredTables {
		var count int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			return fmt.Errorf("inspect required table %s: %w", table, err)
		}
		if count != 1 {
			return fmt.Errorf("schema version %d is missing required table %s", CurrentSchemaVersion, table)
		}
	}
	return nil
}

func integrityCheck(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check failed: %s", result)
	}
	return nil
}

func checkDurableRunGraph(ctx context.Context, db *sql.DB) error {
	var selfEdges int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_edges WHERE parent_run_id = child_run_id`).Scan(&selfEdges); err != nil {
		return fmt.Errorf("inspect durable run graph: %w", err)
	}
	if selfEdges > 0 {
		return fmt.Errorf("durable run graph has %d self-edge(s)", selfEdges)
	}

	var cycleParent, cycleChild string
	err := db.QueryRowContext(ctx, `WITH RECURSIVE walk(root_parent, current_child, path) AS (
		SELECT parent_run_id, child_run_id, parent_run_id || char(0) || child_run_id FROM run_edges
		UNION
		SELECT walk.root_parent, run_edges.child_run_id, walk.path || char(0) || run_edges.child_run_id
			FROM run_edges
			JOIN walk ON run_edges.parent_run_id = walk.current_child
			WHERE instr(walk.path || char(0), char(0) || run_edges.child_run_id || char(0)) = 0
	) SELECT root_parent, current_child FROM walk WHERE root_parent = current_child LIMIT 1`).Scan(&cycleParent, &cycleChild)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("inspect durable run graph: %w", err)
	}
	if err == nil {
		return fmt.Errorf("durable run graph contains a cycle involving %s and %s", cycleParent, cycleChild)
	}

	var mismatchCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM run_edges e
		JOIN runs p ON p.id = e.parent_run_id
		JOIN runs c ON c.id = e.child_run_id
		WHERE e.root_run_id <> ''
			AND (p.root_run_id <> e.root_run_id OR c.root_run_id <> e.root_run_id OR c.depth <> e.depth)`).Scan(&mismatchCount); err != nil {
		return fmt.Errorf("inspect durable run graph: %w", err)
	}
	if mismatchCount > 0 {
		return fmt.Errorf("durable run graph has %d root/depth mismatch(es)", mismatchCount)
	}

	var missingEdgeCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM runs r
		WHERE r.parent_run_id IS NOT NULL AND r.parent_run_id <> ''
			AND r.root_run_id <> ''
			AND NOT EXISTS (
				SELECT 1 FROM run_edges e WHERE e.parent_run_id = r.parent_run_id AND e.child_run_id = r.id
			)`).Scan(&missingEdgeCount); err != nil {
		return fmt.Errorf("inspect durable run graph: %w", err)
	}
	if missingEdgeCount > 0 {
		return fmt.Errorf("durable run graph has %d child run(s) without matching edge", missingEdgeCount)
	}
	return nil
}

func unsupportedVersionError(version int) error {
	return fmt.Errorf("unsupported storage schema version %d; selected loopcoder supports schema version %d", version, CurrentSchemaVersion)
}

func firstUnsafePermissionItem(report PermissionReport) *PermissionItem {
	for i := range report.Items {
		if report.Items[i].Unsafe {
			return &report.Items[i]
		}
	}
	return nil
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
