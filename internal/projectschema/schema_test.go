package projectschema_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/projectschema"
)

func fixedNow() time.Time {
	return time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
}

func TestProjectSchemaIsolationAndEventImmutability(t *testing.T) {
	ctx := context.Background()
	layout, err := home.EnsureMinimumLayout(filepath.Join(t.TempDir(), "home"), "")
	if err != nil {
		t.Fatal(err)
	}

	psA, err := layout.OpenProject(ctx, "proj_a", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	defer psA.Close()
	psB, err := layout.OpenProject(ctx, "proj_b", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	defer psB.Close()

	for _, ps := range []*authoritystore.ProjectStore{psA, psB} {
		if err := projectschema.Ensure(ctx, ps); err != nil {
			t.Fatal(err)
		}
	}
	if err := projectschema.SeedProject(ctx, psA, "proj_a", "owner-a/app", fixedNow()); err != nil {
		t.Fatal(err)
	}
	if err := projectschema.SeedProject(ctx, psB, "proj_b", "owner-b/app", fixedNow()); err != nil {
		t.Fatal(err)
	}

	ts := fixedNow().UTC().Format(time.RFC3339Nano)
	// Overlapping issue number 42 in both projects — isolated stores.
	evA := projectschema.EventRow{
		EventID: "ev-a-1", ProjectID: "proj_a", AggregateKind: "work_item", AggregateID: "wi-42",
		Kind: "work_item.observed", EnvelopeVersion: projectschema.EventEnvelopeVersion,
		Sequence: 1, RecordedAt: ts, IdempotencyKey: "scope:wi-42:observe",
		PayloadVersion: 1, PayloadJSON: `{"issue":42,"note":"synthetic-a"}`,
	}
	if err := projectschema.InsertEventForTest(ctx, psA, evA); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	evB := evA
	evB.EventID = "ev-b-1"
	evB.ProjectID = "proj_b"
	evB.PayloadJSON = `{"issue":42,"note":"synthetic-b"}`
	if err := projectschema.InsertEventForTest(ctx, psB, evB); err != nil {
		t.Fatalf("insert B: %v", err)
	}

	if err := projectschema.TryUpdateEvent(ctx, psA, "ev-a-1", `{"hacked":true}`); err == nil {
		t.Fatal("expected update to fail")
	} else if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("update error = %v, want immutable", err)
	}
	if err := projectschema.TryDeleteEvent(ctx, psA, "ev-a-1"); err == nil {
		t.Fatal("expected delete to fail")
	} else if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("delete error = %v, want immutable", err)
	}

	// Correction appends a new event
	evA2 := evA
	evA2.EventID = "ev-a-2"
	evA2.Sequence = 2
	evA2.Kind = "work_item.corrected"
	evA2.IdempotencyKey = "scope:wi-42:correct-1"
	evA2.CausalEventID = "ev-a-1"
	evA2.PayloadJSON = `{"issue":42,"note":"correction-synthetic"}`
	if err := projectschema.InsertEventForTest(ctx, psA, evA2); err != nil {
		t.Fatalf("correction: %v", err)
	}

	if err := projectschema.AssertNoMachineDomainTables(ctx, psA); err != nil {
		t.Fatal(err)
	}
	if err := projectschema.ValidatePayloadBound(strings.Repeat("x", projectschema.MaxPayloadBytes+1)); err == nil {
		t.Fatal("expected oversized payload rejection")
	}
	if err := projectschema.ValidatePayloadBound(`{"token":"ghp_not_allowed"}`); err == nil {
		t.Fatal("expected secret-shaped rejection")
	}

	// Reopen A and confirm events remain
	_ = psA.Close()
	psA2, err := layout.OpenProject(ctx, "proj_a", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	defer psA2.Close()
	var count int
	if err := psA2.Foundation().WithDB(func(db *sql.DB) error {
		return db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE project_id='proj_a'`).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("event count = %d, want 2", count)
	}
}

func TestForeignKeysAndPayloadCheck(t *testing.T) {
	ctx := context.Background()
	layout, err := home.EnsureMinimumLayout(filepath.Join(t.TempDir(), "home"), "proj_fk")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := layout.OpenProject(ctx, "proj_fk", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	if err := projectschema.Ensure(ctx, ps); err != nil {
		t.Fatal(err)
	}
	if err := projectschema.SeedProject(ctx, ps, "proj_fk", "x/y", fixedNow()); err != nil {
		t.Fatal(err)
	}
	ts := fixedNow().UTC().Format(time.RFC3339Nano)
	// job without work_item fails FK
	err = ps.Foundation().WithDB(func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `INSERT INTO jobs(
			job_id, project_id, work_item_id, state, created_at, updated_at
		) VALUES ('job-1','proj_fk','missing','queued',?,?)`, ts, ts)
		return err
	})
	if err == nil {
		t.Fatal("expected foreign key failure for missing work_item")
	}
	// oversized payload rejected by CHECK
	big := `{"x":"` + strings.Repeat("y", projectschema.MaxPayloadBytes) + `"}`
	err = ps.Foundation().WithDB(func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `INSERT INTO work_items(
			work_item_id, project_id, state, payload_json, created_at, updated_at
		) VALUES ('wi-1','proj_fk','observed',?,?,?)`, big, ts, ts)
		return err
	})
	if err == nil {
		t.Fatal("expected payload CHECK failure")
	}
}

func TestIdempotencyKeyUnique(t *testing.T) {
	ctx := context.Background()
	layout, err := home.EnsureMinimumLayout(filepath.Join(t.TempDir(), "home"), "proj_idemp")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := layout.OpenProject(ctx, "proj_idemp", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	if err := projectschema.Ensure(ctx, ps); err != nil {
		t.Fatal(err)
	}
	if err := projectschema.SeedProject(ctx, ps, "proj_idemp", "x/y", fixedNow()); err != nil {
		t.Fatal(err)
	}
	ts := fixedNow().UTC().Format(time.RFC3339Nano)
	row := projectschema.EventRow{
		EventID: "e1", ProjectID: "proj_idemp", AggregateKind: "job", AggregateID: "j1",
		Kind: "job.queued", EnvelopeVersion: 1, Sequence: 1, RecordedAt: ts,
		IdempotencyKey: "k1", PayloadJSON: `{}`,
	}
	if err := projectschema.InsertEventForTest(ctx, ps, row); err != nil {
		t.Fatal(err)
	}
	row.EventID = "e2"
	row.Sequence = 2
	if err := projectschema.InsertEventForTest(ctx, ps, row); err == nil {
		t.Fatal("expected unique idempotency key failure")
	}
}

func TestTableInventory(t *testing.T) {
	names := projectschema.TableNames()
	if len(names) < 8 {
		t.Fatalf("expected full table inventory, got %v", names)
	}
}
