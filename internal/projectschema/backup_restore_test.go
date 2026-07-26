package projectschema_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/projectschema"
)

func TestBackupRestoreReproducesEventsAndProjection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	layout, err := home.EnsureMinimumLayout(filepath.Join(root, "home"), "proj_bak")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := layout.OpenProject(ctx, "proj_bak", tNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := projectschema.Ensure(ctx, ps); err != nil {
		t.Fatal(err)
	}
	if err := projectschema.SeedProject(ctx, ps, "proj_bak", "o/r", tNow()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, err := projectschema.Append(ctx, ps, projectschema.AppendRequest{
			ProjectID: "proj_bak", AggregateKind: "job", AggregateID: "j1",
			Kind: "job.tick", IdempotencyKey: fmt.Sprintf("tick-%d", i),
			PayloadJSON: fmt.Sprintf(`{"i":%d}`, i), RecordedAt: tNow(),
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	cp, err := projectschema.RebuildProjection(ctx, ps, "proj_bak", "status", "1", func(events []projectschema.EventRow) (string, error) {
		return fmt.Sprintf(`{"n":%d}`, len(events)), nil
	}, tNow())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	man, err := ps.Foundation().Backup(ctx, filepath.Join(root, "backup", "project.db"))
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	meta, err := ps.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.Close(); err != nil {
		t.Fatal(err)
	}

	rps, err := authoritystore.OpenProject(ctx, authoritystore.OpenOptions{
		Path: man.BackupPath,
		Now:  func() time.Time { return tNow() },
	})
	if err != nil {
		t.Fatalf("OpenProject backup: %v", err)
	}
	defer rps.Close()

	rm, err := rps.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rm.StoreID != meta.StoreID || rm.SchemaVersion != meta.SchemaVersion {
		t.Fatalf("metadata mismatch restored=%#v source=%#v", rm, meta)
	}

	events, _, err := projectschema.ReplayAll(ctx, rps, "proj_bak", projectschema.ZeroCursor("proj_bak"), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d", len(events))
	}
	got, ok, err := projectschema.GetCurrentCheckpoint(ctx, rps, "proj_bak", "status")
	if err != nil || !ok {
		t.Fatalf("checkpoint: ok=%v err=%v", ok, err)
	}
	if got.OutputDigest != cp.OutputDigest || got.InputSequence != cp.InputSequence {
		t.Fatalf("projection digest/seq mismatch: got=%#v want=%#v", got, cp)
	}
}
