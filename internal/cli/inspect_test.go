package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	deliverypkg "github.com/jasonhnd/loopcoder/internal/delivery"
	inspectpkg "github.com/jasonhnd/loopcoder/internal/inspect"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestProjectsInspectMissingDatabaseIsStableEmptyJSON(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	var stdout, stderr bytes.Buffer
	code := RunWithDeps([]string{"projects", "inspect", "--db", dbPath, "--format", "json"}, &stdout, &stderr, Deps{Now: fixedCLINow})
	if code != 0 {
		t.Fatalf("projects inspect exit = %d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var report inspectpkg.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode inspect JSON: %v\n%s", err, stdout.String())
	}
	if report.SchemaVersion != inspectpkg.SchemaVersion || report.Status != "empty" || !report.ReadOnly || len(report.Projects) != 0 {
		t.Fatalf("report = %#v, want stable empty read-only report", report)
	}
}

func TestProjectsInspectRendersBoundedDeliveryRunState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "loopcoder.db")
	missingCheckout := filepath.Join(t.TempDir(), "moved-away")
	presentCheckout := t.TempDir()
	seedProjectInspectFixture(t, dbPath, missingCheckout, presentCheckout)

	var stdout, stderr bytes.Buffer
	code := RunWithDeps([]string{"projects", "inspect", "--db", dbPath, "--project-id", "proj_inspect_a", "--limit", "1", "--format", "json"}, &stdout, &stderr, Deps{Now: fixedCLINow})
	if code != 0 {
		t.Fatalf("projects inspect json exit = %d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var report inspectpkg.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode inspect JSON: %v\n%s", err, stdout.String())
	}
	if report.Status != "partial" || !report.ReadOnly || len(report.Projects) != 1 {
		t.Fatalf("report status/projects = %s/%d, want partial one project", report.Status, len(report.Projects))
	}
	project := report.Projects[0]
	if project.ProjectID != "proj_inspect_a" || project.CheckoutStatus != "missing" || project.RunCount != 2 || !project.RunHasMore {
		t.Fatalf("project = %#v, want missing checkout and paginated two-run history", project)
	}
	if len(project.Runs) != 1 || project.Runs[0].DeliveryRunID != "run_inspect_new" {
		t.Fatalf("runs = %#v, want newest bounded run only", project.Runs)
	}
	run := project.Runs[0]
	if run.State != deliverypkg.RunRunning || run.Blocker != "stale" || run.LastDurableProgress.At == "" || len(run.Tasks) != 1 {
		t.Fatalf("run = %#v, want running stale run with progress and one task", run)
	}
	task := run.Tasks[0]
	if task.State != deliverypkg.TaskRunning || task.Blocker != "stale" || len(task.Attempts) != 1 || task.Attempts[0].State != "running" {
		t.Fatalf("task = %#v, want stale running task with running attempt", task)
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithDeps([]string{"projects", "inspect", "--db", dbPath, "--project-id", "proj_inspect_a", "--limit", "1"}, &stdout, &stderr, Deps{Now: fixedCLINow})
	if code != 0 {
		t.Fatalf("projects inspect text exit = %d stderr=%q", code, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{"PROJECT INSPECTION", "read_only: true", "checkout=missing", "run run_inspect_new", "more available", "blocker=stale"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text output missing %q:\n%s", want, text)
		}
	}
}

func seedProjectInspectFixture(t *testing.T, dbPath, missingCheckout, presentCheckout string) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: fixedCLINow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	nowText := deliverypkg.CanonicalTimestamp(fixedCLINow())
	oldText := deliverypkg.CanonicalTimestamp(fixedCLINow().Add(-10 * time.Minute))
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		for _, row := range []struct {
			id          string
			displayName string
			path        string
		}{
			{id: "proj_inspect_a", displayName: "alpha", path: missingCheckout},
			{id: "proj_inspect_b", displayName: "beta", path: presentCheckout},
		} {
			if _, err := tx.Exec(ctx, `INSERT INTO projects(id, display_name, local_path, local_path_canonical, identity_source, created_at, updated_at) VALUES (?, ?, ?, ?, 'local-path', ?, ?)`, row.id, row.displayName, row.path, row.path, oldText, nowText); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	actor := deliverypkg.Actor{ActorKind: "user", ActorID: "local-user", DecisionAuthority: "user", Source: "test"}
	host := deliverypkg.Host{HostKind: "cli", HostID: "host-test", SessionID: "session-test", ProcessID: 1, LoopcoderVersion: "0.8.0", Platform: "test"}
	oldRun := deliverypkg.DeliveryRun{
		DeliveryRunID:      "run_inspect_old",
		ProjectID:          "proj_inspect_a",
		State:              deliverypkg.RunSucceeded,
		IntentSummary:      "old run",
		MaxSideEffectClass: "local-read",
		ApprovalStatus:     "not-required",
		OverrideStatus:     "none",
		PolicyVersion:      "test-policy-v1",
		CreatedBy:          actor,
		UpdatedBy:          actor,
		Host:               host,
	}
	if _, err := deliverypkg.PersistDeliveryRun(ctx, store, oldRun, deliverypkg.PersistOptions{IdempotencyKey: "inspect-old-run", Now: fixedCLINow().Add(-10 * time.Minute)}); err != nil {
		t.Fatalf("PersistDeliveryRun old: %v", err)
	}
	newRun := oldRun
	newRun.DeliveryRunID = "run_inspect_new"
	newRun.State = deliverypkg.RunRunning
	newRun.IntentSummary = "new run"
	if _, err := deliverypkg.PersistDeliveryRun(ctx, store, newRun, deliverypkg.PersistOptions{IdempotencyKey: "inspect-new-run", Now: fixedCLINow().Add(-5 * time.Minute)}); err != nil {
		t.Fatalf("PersistDeliveryRun new: %v", err)
	}
	task := deliverypkg.Task{
		ProjectID:        "proj_inspect_a",
		DeliveryRunID:    "run_inspect_new",
		TaskKey:          "build",
		State:            deliverypkg.TaskReady,
		Title:            "Build feature",
		RequirementsJSON: `{"do":"work"}`,
		ScopeJSON:        `{"paths":["README.md"]}`,
		Permission:       "write",
		SideEffectClass:  "repo-write",
		PolicyVersion:    "test-policy-v1",
		PlanFingerprint:  "sha256:" + strings.Repeat("3", 64),
		CreatedBy:        actor,
		UpdatedBy:        actor,
		Host:             host,
	}
	createdTask, err := deliverypkg.PersistTask(ctx, store, task, deliverypkg.PersistOptions{IdempotencyKey: "inspect-task", Now: fixedCLINow().Add(-5 * time.Minute)})
	if err != nil {
		t.Fatalf("PersistTask: %v", err)
	}
	attempt, err := deliverypkg.ClaimTask(ctx, store, "proj_inspect_a", "run_inspect_new", createdTask.TaskID, "executor-test", actor, host, deliverypkg.PersistOptions{IdempotencyKey: "inspect-claim", Now: fixedCLINow().Add(-5 * time.Minute)})
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE delivery_tasks SET state = 'running', updated_at = ?, started_at = ? WHERE task_id = ?`, oldText, oldText, createdTask.TaskID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE delivery_attempts SET state = 'running', updated_at = ?, started_at = ? WHERE attempt_id = ?`, oldText, oldText, attempt.AttemptID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("mark attempt running: %v", err)
	}
}
