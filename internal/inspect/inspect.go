// Package inspect provides read-only project and DeliveryRun inspection.
package inspect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/storage"

	_ "modernc.org/sqlite"
)

const (
	SchemaVersion = "loopcoder.inspect.v1"

	DefaultProjectLimit = 50
	DefaultRunLimit     = 20
	DefaultTaskLimit    = 50
	DefaultAttemptLimit = 20
	MaxLimit            = 200
)

type Options struct {
	DatabasePath    string
	ProjectID       string
	RunID           string
	IncludeDetached bool
	ProjectLimit    int
	ProjectOffset   int
	RunLimit        int
	RunOffset       int
	TaskLimit       int
	AttemptLimit    int
	StaleAfter      time.Duration
	Now             func() time.Time
}

type Report struct {
	SchemaVersion string           `json:"schema_version"`
	Status        string           `json:"status"`
	DatabasePath  string           `json:"database_path"`
	GeneratedAt   string           `json:"generated_at"`
	ReadOnly      bool             `json:"read_only"`
	Filters       Filters          `json:"filters"`
	Pagination    Pagination       `json:"pagination"`
	Projects      []ProjectSummary `json:"projects"`
	Diagnostics   []Diagnostic     `json:"diagnostics,omitempty"`
}

type Filters struct {
	ProjectID       string `json:"project_id,omitempty"`
	RunID           string `json:"run_id,omitempty"`
	IncludeDetached bool   `json:"include_detached,omitempty"`
}

type Pagination struct {
	ProjectLimit   int  `json:"project_limit"`
	ProjectOffset  int  `json:"project_offset"`
	ProjectHasMore bool `json:"project_has_more"`
	RunLimit       int  `json:"run_limit"`
	RunOffset      int  `json:"run_offset"`
	TaskLimit      int  `json:"task_limit"`
	AttemptLimit   int  `json:"attempt_limit"`
}

type ProjectSummary struct {
	registry.Project
	CheckoutStatus string       `json:"checkout_status"`
	RunCount       int          `json:"run_count"`
	Runs           []RunSummary `json:"runs"`
	RunHasMore     bool         `json:"run_has_more,omitempty"`
	Diagnostics    []Diagnostic `json:"diagnostics,omitempty"`
}

type RunSummary struct {
	DeliveryRunID       string        `json:"delivery_run_id"`
	RunID               string        `json:"run_id"`
	RootRunID           string        `json:"root_run_id,omitempty"`
	ParentRunID         string        `json:"parent_run_id,omitempty"`
	State               string        `json:"state"`
	IntentSummary       string        `json:"intent_summary,omitempty"`
	ApprovalStatus      string        `json:"approval_status,omitempty"`
	OverrideStatus      string        `json:"override_status,omitempty"`
	PolicyVersion       string        `json:"policy_version,omitempty"`
	MaxSideEffectClass  string        `json:"max_side_effect_class,omitempty"`
	CreatedAt           string        `json:"created_at,omitempty"`
	UpdatedAt           string        `json:"updated_at,omitempty"`
	StartedAt           string        `json:"started_at,omitempty"`
	EndedAt             string        `json:"ended_at,omitempty"`
	TerminalErrorCode   string        `json:"terminal_error_code,omitempty"`
	ErrorMessage        string        `json:"error_message,omitempty"`
	LegacyIDSource      string        `json:"legacy_id_source,omitempty"`
	TaskCount           int           `json:"task_count"`
	AttemptCount        int           `json:"attempt_count"`
	Tasks               []TaskSummary `json:"tasks"`
	TasksHasMore        bool          `json:"tasks_has_more,omitempty"`
	Blocker             string        `json:"blocker,omitempty"`
	NextAction          string        `json:"next_action,omitempty"`
	LastDurableProgress Progress      `json:"last_durable_progress"`
	Diagnostics         []Diagnostic  `json:"diagnostics,omitempty"`
}

type TaskSummary struct {
	TaskID              string           `json:"task_id"`
	TaskKey             string           `json:"task_key,omitempty"`
	State               string           `json:"state"`
	Title               string           `json:"title,omitempty"`
	Permission          string           `json:"permission,omitempty"`
	SideEffectClass     string           `json:"side_effect_class,omitempty"`
	AttemptCount        int              `json:"attempt_count"`
	ActiveAttemptID     string           `json:"active_attempt_id,omitempty"`
	CreatedAt           string           `json:"created_at,omitempty"`
	UpdatedAt           string           `json:"updated_at,omitempty"`
	StartedAt           string           `json:"started_at,omitempty"`
	EndedAt             string           `json:"ended_at,omitempty"`
	TerminalErrorCode   string           `json:"terminal_error_code,omitempty"`
	ErrorMessage        string           `json:"error_message,omitempty"`
	Blocker             string           `json:"blocker,omitempty"`
	NextAction          string           `json:"next_action,omitempty"`
	LastDurableProgress Progress         `json:"last_durable_progress"`
	Attempts            []AttemptSummary `json:"attempts"`
	AttemptsHasMore     bool             `json:"attempts_has_more,omitempty"`
	Diagnostics         []Diagnostic     `json:"diagnostics,omitempty"`
}

type AttemptSummary struct {
	AttemptID              string       `json:"attempt_id"`
	AttemptOrdinal         int          `json:"attempt_ordinal"`
	State                  string       `json:"state"`
	ExecutorID             string       `json:"executor_id,omitempty"`
	ProviderIdempotencyKey string       `json:"provider_idempotency_key,omitempty"`
	SideEffectClass        string       `json:"side_effect_class,omitempty"`
	StartedAt              string       `json:"started_at,omitempty"`
	EndedAt                string       `json:"ended_at,omitempty"`
	CreatedAt              string       `json:"created_at,omitempty"`
	UpdatedAt              string       `json:"updated_at,omitempty"`
	ReportID               string       `json:"report_id,omitempty"`
	TerminalErrorCode      string       `json:"terminal_error_code,omitempty"`
	ErrorMessage           string       `json:"error_message,omitempty"`
	LastDurableProgress    Progress     `json:"last_durable_progress"`
	Diagnostics            []Diagnostic `json:"diagnostics,omitempty"`
}

type Progress struct {
	At     string `json:"at,omitempty"`
	Source string `json:"source,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type Diagnostic struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

func Load(ctx context.Context, opts Options) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts = normalizeOptions(opts)
	dbPath, err := resolveDatabasePath(opts.DatabasePath)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		SchemaVersion: SchemaVersion,
		Status:        "ok",
		DatabasePath:  dbPath,
		GeneratedAt:   formatTime(now(opts)),
		ReadOnly:      true,
		Filters:       Filters{ProjectID: opts.ProjectID, RunID: opts.RunID, IncludeDetached: opts.IncludeDetached},
		Pagination: Pagination{
			ProjectLimit:  opts.ProjectLimit,
			ProjectOffset: opts.ProjectOffset,
			RunLimit:      opts.RunLimit,
			RunOffset:     opts.RunOffset,
			TaskLimit:     opts.TaskLimit,
			AttemptLimit:  opts.AttemptLimit,
		},
	}
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			report.Status = "empty"
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Severity: "info", Code: "database-missing", Message: "storage database has not been created"})
			return report, nil
		}
		return Report{}, fmt.Errorf("inspect storage database: %w", err)
	}
	health, err := storage.CheckHealth(ctx, dbPath)
	if err != nil {
		return Report{}, err
	}
	if !health.Exists {
		report.Status = "empty"
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Severity: "info", Code: "database-missing", Message: health.Message})
		return report, nil
	}
	if !health.OK {
		report.Status = "partial"
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Severity: "warning", Code: "storage-health", Message: health.Message})
	}
	db, err := openReadOnlyDB(ctx, dbPath)
	if err != nil {
		return Report{}, err
	}
	defer db.Close()
	projects, hasMore, err := loadProjects(ctx, db, opts)
	if err != nil {
		return Report{}, err
	}
	report.Pagination.ProjectHasMore = hasMore
	if len(projects) == 0 {
		report.Status = "empty"
	}
	for i := range projects {
		if err := enrichProject(ctx, db, &projects[i], opts); err != nil {
			return Report{}, err
		}
		if projectHasDiagnostics(projects[i]) {
			report.Status = "partial"
		}
	}
	report.Projects = projects
	if report.Status == "ok" && hasDiagnostics(report.Diagnostics) {
		report.Status = "partial"
	}
	return report, nil
}

func normalizeOptions(opts Options) Options {
	opts.ProjectID = strings.TrimSpace(opts.ProjectID)
	opts.RunID = strings.TrimSpace(opts.RunID)
	opts.ProjectLimit = normalizeLimit(opts.ProjectLimit, DefaultProjectLimit)
	opts.RunLimit = normalizeLimit(opts.RunLimit, DefaultRunLimit)
	opts.TaskLimit = normalizeLimit(opts.TaskLimit, DefaultTaskLimit)
	opts.AttemptLimit = normalizeLimit(opts.AttemptLimit, DefaultAttemptLimit)
	if opts.ProjectOffset < 0 {
		opts.ProjectOffset = 0
	}
	if opts.RunOffset < 0 {
		opts.RunOffset = 0
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = time.Duration(lcdefaults.WorkerStaleAfterSeconds) * time.Second
	}
	return opts
}

func normalizeLimit(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	if value > MaxLimit {
		return MaxLimit
	}
	return value
}

func resolveDatabasePath(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return filepath.Clean(path), nil
	}
	layout, err := home.Resolve(home.DefaultDeps())
	if err != nil {
		return "", err
	}
	return layout.DatabasePath(), nil
}

func openReadOnlyDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open storage read-only: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable storage read-only mode: %w", err)
	}
	return db, nil
}

func loadProjects(ctx context.Context, db *sql.DB, opts Options) ([]ProjectSummary, bool, error) {
	clauses := []string{}
	args := []any{}
	if !opts.IncludeDetached {
		clauses = append(clauses, "detached_at = ''")
	}
	if opts.ProjectID != "" {
		clauses = append(clauses, "id = ?")
		args = append(args, opts.ProjectID)
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	args = append(args, opts.ProjectLimit+1, opts.ProjectOffset)
	rows, err := db.QueryContext(ctx, `SELECT id, display_name, local_path, local_path_canonical, git_root, default_branch, remote_url, remote_url_normalized, github_owner, github_name, identity_source, created_at, updated_at, detached_at FROM projects`+where+` ORDER BY display_name, id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("inspect projects: %w", err)
	}
	defer rows.Close()
	projects := []ProjectSummary{}
	for rows.Next() {
		var source string
		var project ProjectSummary
		if err := rows.Scan(&project.ProjectID, &project.DisplayName, &project.LocalPath, &project.LocalPathCanonical, &project.GitRoot, &project.DefaultBranch, &project.RemoteURL, &project.RemoteURLNormalized, &project.GitHubOwner, &project.GitHubName, &source, &project.CreatedAt, &project.UpdatedAt, &project.DetachedAt); err != nil {
			return nil, false, fmt.Errorf("scan project: %w", err)
		}
		project.IdentitySource = registry.IdentitySource(source)
		project.CheckoutStatus = checkoutStatus(project.LocalPath)
		if project.CheckoutStatus != "present" {
			project.Diagnostics = append(project.Diagnostics, Diagnostic{Severity: "warning", Code: "checkout-" + project.CheckoutStatus, Message: "registered checkout path is " + project.CheckoutStatus + ": " + project.LocalPath})
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(projects) > opts.ProjectLimit
	if hasMore {
		projects = projects[:opts.ProjectLimit]
	}
	return projects, hasMore, nil
}

func enrichProject(ctx context.Context, db *sql.DB, project *ProjectSummary, opts Options) error {
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_runs WHERE project_id = ?`+runIDClause(opts), runCountArgs(project.ProjectID, opts)...).Scan(&project.RunCount); err != nil {
		return fmt.Errorf("count delivery runs for %s: %w", project.ProjectID, err)
	}
	runs, hasMore, err := loadRuns(ctx, db, project.ProjectID, opts)
	if err != nil {
		return err
	}
	project.Runs = runs
	project.RunHasMore = hasMore
	if hasMore {
		project.Diagnostics = append(project.Diagnostics, Diagnostic{Severity: "info", Code: "runs-paginated", Message: "additional delivery runs are available; increase --offset or --limit"})
	}
	return nil
}

func runIDClause(opts Options) string {
	if opts.RunID == "" {
		return ""
	}
	return " AND delivery_run_id = ?"
}

func runCountArgs(projectID string, opts Options) []any {
	args := []any{projectID}
	if opts.RunID != "" {
		args = append(args, opts.RunID)
	}
	return args
}

func loadRuns(ctx context.Context, db *sql.DB, projectID string, opts Options) ([]RunSummary, bool, error) {
	args := []any{projectID}
	where := "WHERE project_id = ?"
	if opts.RunID != "" {
		where += " AND delivery_run_id = ?"
		args = append(args, opts.RunID)
	}
	args = append(args, opts.RunLimit+1, opts.RunOffset)
	rows, err := db.QueryContext(ctx, `SELECT delivery_run_id, run_id, root_run_id, parent_run_id, state, intent_summary, approval_status, override_status, policy_version, max_side_effect_class, created_at, updated_at, started_at, ended_at, terminal_error_code, error_message, legacy_id_source, schema_version, record_version FROM delivery_runs `+where+` ORDER BY updated_at DESC, delivery_run_id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("inspect delivery runs: %w", err)
	}
	defer rows.Close()
	runs := []RunSummary{}
	for rows.Next() {
		var run RunSummary
		var parent, started, ended sql.NullString
		var schema string
		var recordVersion int
		if err := rows.Scan(&run.DeliveryRunID, &run.RunID, &run.RootRunID, &parent, &run.State, &run.IntentSummary, &run.ApprovalStatus, &run.OverrideStatus, &run.PolicyVersion, &run.MaxSideEffectClass, &run.CreatedAt, &run.UpdatedAt, &started, &ended, &run.TerminalErrorCode, &run.ErrorMessage, &run.LegacyIDSource, &schema, &recordVersion); err != nil {
			return nil, false, fmt.Errorf("scan delivery run: %w", err)
		}
		run.ParentRunID = nullString(parent)
		run.StartedAt = nullString(started)
		run.EndedAt = nullString(ended)
		run.Diagnostics = append(run.Diagnostics, validateRun(run, schema, recordVersion)...)
		if err := enrichRun(ctx, db, &run, opts); err != nil {
			return nil, false, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(runs) > opts.RunLimit
	if hasMore {
		runs = runs[:opts.RunLimit]
	}
	return runs, hasMore, nil
}

func enrichRun(ctx context.Context, db *sql.DB, run *RunSummary, opts Options) error {
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_tasks WHERE delivery_run_id = ?`, run.DeliveryRunID).Scan(&run.TaskCount); err != nil {
		return fmt.Errorf("count tasks for %s: %w", run.DeliveryRunID, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_attempts WHERE delivery_run_id = ?`, run.DeliveryRunID).Scan(&run.AttemptCount); err != nil {
		return fmt.Errorf("count attempts for %s: %w", run.DeliveryRunID, err)
	}
	tasks, hasMore, err := loadTasks(ctx, db, run.DeliveryRunID, opts)
	if err != nil {
		return err
	}
	run.Tasks = tasks
	run.TasksHasMore = hasMore
	run.LastDurableProgress = latestRunProgress(*run)
	run.Blocker, run.NextAction = classifyRun(*run, opts)
	if hasMore {
		run.Diagnostics = append(run.Diagnostics, Diagnostic{Severity: "info", Code: "tasks-paginated", Message: "additional tasks are available; increase --task-limit"})
	}
	return nil
}

func loadTasks(ctx context.Context, db *sql.DB, runID string, opts Options) ([]TaskSummary, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_id, task_key, state, title, permission, side_effect_class, attempt_count, active_attempt_id, created_at, updated_at, started_at, ended_at, terminal_error_code, error_message, schema_version, record_version FROM delivery_tasks WHERE delivery_run_id = ? ORDER BY task_key, task_id LIMIT ?`, runID, opts.TaskLimit+1)
	if err != nil {
		return nil, false, fmt.Errorf("inspect tasks for %s: %w", runID, err)
	}
	defer rows.Close()
	tasks := []TaskSummary{}
	for rows.Next() {
		var task TaskSummary
		var active, started, ended sql.NullString
		var schema string
		var recordVersion int
		if err := rows.Scan(&task.TaskID, &task.TaskKey, &task.State, &task.Title, &task.Permission, &task.SideEffectClass, &task.AttemptCount, &active, &task.CreatedAt, &task.UpdatedAt, &started, &ended, &task.TerminalErrorCode, &task.ErrorMessage, &schema, &recordVersion); err != nil {
			return nil, false, fmt.Errorf("scan task: %w", err)
		}
		task.ActiveAttemptID = nullString(active)
		task.StartedAt = nullString(started)
		task.EndedAt = nullString(ended)
		task.Diagnostics = append(task.Diagnostics, validateTask(task, schema, recordVersion)...)
		if err := enrichTask(ctx, db, &task, opts); err != nil {
			return nil, false, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(tasks) > opts.TaskLimit
	if hasMore {
		tasks = tasks[:opts.TaskLimit]
	}
	return tasks, hasMore, nil
}

func enrichTask(ctx context.Context, db *sql.DB, task *TaskSummary, opts Options) error {
	attempts, hasMore, err := loadAttempts(ctx, db, task.TaskID, opts)
	if err != nil {
		return err
	}
	task.Attempts = attempts
	task.AttemptsHasMore = hasMore
	task.LastDurableProgress = latestTaskProgress(*task)
	task.Blocker, task.NextAction = classifyTask(*task, opts)
	if hasMore {
		task.Diagnostics = append(task.Diagnostics, Diagnostic{Severity: "info", Code: "attempts-paginated", Message: "additional attempts are available; increase --attempt-limit"})
	}
	if task.ActiveAttemptID != "" && !attemptExists(attempts, task.ActiveAttemptID) {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_attempts WHERE task_id = ? AND attempt_id = ?`, task.TaskID, task.ActiveAttemptID).Scan(&count); err != nil {
			return fmt.Errorf("inspect active attempt %s: %w", task.ActiveAttemptID, err)
		}
		if count == 0 {
			task.Diagnostics = append(task.Diagnostics, Diagnostic{Severity: "warning", Code: "missing-active-attempt", Message: "task active_attempt_id does not reference a stored attempt"})
		}
	}
	return nil
}

func loadAttempts(ctx context.Context, db *sql.DB, taskID string, opts Options) ([]AttemptSummary, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT attempt_id, attempt_ordinal, state, executor_id, provider_idempotency_key, side_effect_class, started_at, ended_at, created_at, updated_at, report_id, terminal_error_code, error_message, schema_version, record_version FROM delivery_attempts WHERE task_id = ? ORDER BY attempt_ordinal DESC, attempt_id LIMIT ?`, taskID, opts.AttemptLimit+1)
	if err != nil {
		return nil, false, fmt.Errorf("inspect attempts for %s: %w", taskID, err)
	}
	defer rows.Close()
	attempts := []AttemptSummary{}
	for rows.Next() {
		var attempt AttemptSummary
		var started, ended sql.NullString
		var schema string
		var recordVersion int
		if err := rows.Scan(&attempt.AttemptID, &attempt.AttemptOrdinal, &attempt.State, &attempt.ExecutorID, &attempt.ProviderIdempotencyKey, &attempt.SideEffectClass, &started, &ended, &attempt.CreatedAt, &attempt.UpdatedAt, &attempt.ReportID, &attempt.TerminalErrorCode, &attempt.ErrorMessage, &schema, &recordVersion); err != nil {
			return nil, false, fmt.Errorf("scan attempt: %w", err)
		}
		attempt.StartedAt = nullString(started)
		attempt.EndedAt = nullString(ended)
		attempt.LastDurableProgress = progress(attempt.UpdatedAt, "attempt", attempt.AttemptID)
		attempt.Diagnostics = append(attempt.Diagnostics, validateAttempt(attempt, schema, recordVersion)...)
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(attempts) > opts.AttemptLimit
	if hasMore {
		attempts = attempts[:opts.AttemptLimit]
	}
	return attempts, hasMore, nil
}

func checkoutStatus(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "unknown"
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "missing"
		}
		return "unknown"
	}
	if !info.IsDir() {
		return "not-directory"
	}
	return "present"
}

func validateRun(run RunSummary, schema string, recordVersion int) []Diagnostic {
	var out []Diagnostic
	if schema != delivery.SchemaDeliveryRun {
		out = append(out, Diagnostic{Severity: "warning", Code: "unknown-schema-version", Message: "delivery run schema_version is " + schema})
	}
	if recordVersion < 1 {
		out = append(out, Diagnostic{Severity: "warning", Code: "invalid-record-version", Message: "delivery run record_version is invalid"})
	}
	if !knownRunState(run.State) {
		out = append(out, Diagnostic{Severity: "warning", Code: "unknown-run-state", Message: "delivery run state is " + run.State})
	}
	if strings.TrimSpace(run.TerminalErrorCode) != "" {
		out = append(out, Diagnostic{Severity: "warning", Code: "terminal-error", Message: run.TerminalErrorCode})
	}
	return out
}

func validateTask(task TaskSummary, schema string, recordVersion int) []Diagnostic {
	var out []Diagnostic
	if schema != delivery.SchemaTask {
		out = append(out, Diagnostic{Severity: "warning", Code: "unknown-schema-version", Message: "task schema_version is " + schema})
	}
	if recordVersion < 1 {
		out = append(out, Diagnostic{Severity: "warning", Code: "invalid-record-version", Message: "task record_version is invalid"})
	}
	if !knownTaskState(task.State) {
		out = append(out, Diagnostic{Severity: "warning", Code: "unknown-task-state", Message: "task state is " + task.State})
	}
	if strings.TrimSpace(task.TerminalErrorCode) != "" {
		out = append(out, Diagnostic{Severity: "warning", Code: "terminal-error", Message: task.TerminalErrorCode})
	}
	return out
}

func validateAttempt(attempt AttemptSummary, schema string, recordVersion int) []Diagnostic {
	var out []Diagnostic
	if schema != delivery.SchemaAttempt {
		out = append(out, Diagnostic{Severity: "warning", Code: "unknown-schema-version", Message: "attempt schema_version is " + schema})
	}
	if recordVersion < 1 {
		out = append(out, Diagnostic{Severity: "warning", Code: "invalid-record-version", Message: "attempt record_version is invalid"})
	}
	if !knownAttemptState(attempt.State) {
		out = append(out, Diagnostic{Severity: "warning", Code: "unknown-attempt-state", Message: "attempt state is " + attempt.State})
	}
	if strings.TrimSpace(attempt.TerminalErrorCode) != "" {
		out = append(out, Diagnostic{Severity: "warning", Code: "terminal-error", Message: attempt.TerminalErrorCode})
	}
	return out
}

func knownRunState(state string) bool {
	switch state {
	case delivery.RunDraft, delivery.RunPlanning, delivery.RunAwaitingApproval, delivery.RunApproved, delivery.RunQueued, delivery.RunRunning, delivery.RunPaused, delivery.RunCancelling, delivery.RunSucceeded, delivery.RunFailed, delivery.RunCancelled, delivery.RunAbandoned, delivery.RunNeedsHuman:
		return true
	default:
		return false
	}
}

func knownTaskState(state string) bool {
	switch state {
	case delivery.TaskPending, delivery.TaskBlocked, delivery.TaskAwaitingApproval, delivery.TaskReady, delivery.TaskClaimed, delivery.TaskRunning, delivery.TaskPaused, delivery.TaskCancelling, delivery.TaskSucceeded, delivery.TaskFailed, delivery.TaskSkipped, delivery.TaskCancelled, delivery.TaskNeedsHuman:
		return true
	default:
		return false
	}
}

func knownAttemptState(state string) bool {
	switch state {
	case delivery.AttemptPlanned,
		delivery.AttemptClaimed,
		delivery.AttemptLaunching,
		delivery.AttemptRunning,
		delivery.AttemptSucceeded,
		delivery.AttemptFailed,
		delivery.AttemptCancelled,
		delivery.AttemptNeedsHuman,
		delivery.AttemptStale,
		delivery.AttemptSuperseded:
		return true
	default:
		return false
	}
}

func classifyRun(run RunSummary, opts Options) (string, string) {
	switch run.State {
	case delivery.RunDraft:
		return "", "start planning or abandon delivery run"
	case delivery.RunPlanning:
		return "", "wait for plan proposal or inspect planner progress"
	case delivery.RunAwaitingApproval:
		return "approval-required", "review delivery plan and decide approve, reject, or edit"
	case delivery.RunApproved:
		return "", "queue delivery run or start execution"
	case delivery.RunPaused:
		return "paused", "resume or cancel delivery run"
	case delivery.RunCancelling:
		return "cancelling", "wait for cancellation to finish or inspect active task cleanup"
	case delivery.RunNeedsHuman:
		return firstNonEmpty(run.TerminalErrorCode, "needs-human"), "inspect blocker and choose the next delivery action"
	case delivery.RunFailed:
		return firstNonEmpty(run.TerminalErrorCode, "failed"), "inspect failure and decide retry, edit, or abandon"
	case delivery.RunCancelled, delivery.RunAbandoned, delivery.RunSucceeded:
		return "", "none"
	}
	for _, task := range run.Tasks {
		if task.Blocker != "" {
			return task.Blocker, task.NextAction
		}
	}
	if isStale(run.LastDurableProgress.At, opts) && (run.State == delivery.RunRunning || run.State == delivery.RunQueued) {
		return "stale", "inspect active task or attempt for durable progress"
	}
	if run.State == delivery.RunQueued {
		return "", "start eligible tasks"
	}
	if run.State == delivery.RunRunning {
		return "", "wait for active tasks or inspect attempts"
	}
	return "", "inspect delivery run"
}

func classifyTask(task TaskSummary, opts Options) (string, string) {
	switch task.State {
	case delivery.TaskBlocked:
		return "dependencies-blocked", "inspect upstream task blockers"
	case delivery.TaskAwaitingApproval:
		return "approval-required", "review and approve the current plan fingerprint"
	case delivery.TaskFailed:
		return firstNonEmpty(task.TerminalErrorCode, "failed"), "inspect failed attempt and decide retry or edit"
	case delivery.TaskNeedsHuman:
		return firstNonEmpty(task.TerminalErrorCode, "needs-human"), "human decision required before continuing"
	case delivery.TaskPending:
		return "dependencies-pending", "wait for upstream tasks"
	case delivery.TaskPaused:
		return "paused", "resume or cancel task"
	case delivery.TaskCancelling:
		return "cancelling", "wait for cancellation to finish or inspect active attempt cleanup"
	case delivery.TaskSucceeded, delivery.TaskSkipped, delivery.TaskCancelled:
		return "", "none"
	}
	if isStale(task.LastDurableProgress.At, opts) && (task.State == delivery.TaskClaimed || task.State == delivery.TaskRunning) {
		return "stale", "inspect active attempt progress"
	}
	if task.State == delivery.TaskReady {
		return "", "claim task"
	}
	if task.State == delivery.TaskClaimed {
		return "", "start claimed task"
	}
	if task.State == delivery.TaskRunning {
		return "", "wait for attempt progress"
	}
	return "", "wait for attempt progress"
}

func latestRunProgress(run RunSummary) Progress {
	best := progress(run.UpdatedAt, "delivery_run", run.DeliveryRunID)
	for _, task := range run.Tasks {
		best = maxProgress(best, task.LastDurableProgress)
	}
	return best
}

func latestTaskProgress(task TaskSummary) Progress {
	best := progress(task.UpdatedAt, "task", task.TaskID)
	for _, attempt := range task.Attempts {
		best = maxProgress(best, attempt.LastDurableProgress)
	}
	return best
}

func progress(at, source, detail string) Progress {
	if strings.TrimSpace(at) == "" {
		return Progress{}
	}
	return Progress{At: at, Source: source, Detail: detail}
}

func maxProgress(a, b Progress) Progress {
	if strings.TrimSpace(a.At) == "" {
		return b
	}
	if strings.TrimSpace(b.At) == "" {
		return a
	}
	if b.At > a.At {
		return b
	}
	return a
}

func isStale(at string, opts Options) bool {
	if strings.TrimSpace(at) == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return false
	}
	return now(opts).Sub(parsed) > opts.StaleAfter
}

func nullString(value sql.NullString) string {
	if value.Valid {
		return strings.TrimSpace(value.String)
	}
	return ""
}

func attemptExists(attempts []AttemptSummary, id string) bool {
	for _, attempt := range attempts {
		if attempt.AttemptID == id {
			return true
		}
	}
	return false
}

func hasDiagnostics(diagnostics []Diagnostic) bool {
	return len(diagnostics) > 0
}

func projectHasDiagnostics(project ProjectSummary) bool {
	if len(project.Diagnostics) > 0 {
		return true
	}
	for _, run := range project.Runs {
		if len(run.Diagnostics) > 0 {
			return true
		}
		for _, task := range run.Tasks {
			if len(task.Diagnostics) > 0 {
				return true
			}
			for _, attempt := range task.Attempts {
				if len(attempt.Diagnostics) > 0 {
					return true
				}
			}
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func now(opts Options) time.Time {
	if opts.Now == nil {
		return time.Now().UTC()
	}
	return opts.Now().UTC()
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
