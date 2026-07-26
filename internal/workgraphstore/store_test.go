package workgraphstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func openStore(t *testing.T) storage.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	st, err := storage.Open(context.Background(), storage.Options{
		Path: path,
		Now:  func() time.Time { return time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func ensureProject(t *testing.T, st storage.Store, id string) {
	t.Helper()
	ctx := context.Background()
	err := st.WithWriteTx(ctx, func(tx storage.Tx) error {
		now := st.Now().UTC().Format(time.RFC3339Nano)
		_, err := tx.Exec(ctx, `INSERT OR IGNORE INTO projects(id, local_path, created_at, updated_at) VALUES(?,?,?,?)`,
			id, "/tmp/"+id, now, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func sampleGraph(version int) workgraph.Graph {
	g := workgraph.Graph{
		Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
		GraphID: "g_main", Version: version,
		Source: workgraph.SourceOwnerApproved, ExplicitOptIn: true, ApprovedBy: "owner",
		Items: []workgraph.WorkItem{
			{Schema: workgraph.SchemaItem, ID: "wi_a", Intent: "A", Status: workgraph.ItemRequired,
				Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 1},
			{Schema: workgraph.SchemaItem, ID: "wi_b", Intent: "B", Status: workgraph.ItemRequired,
				Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 2,
				OutputContract: "patch"},
		},
		Dependencies: []workgraph.Dependency{
			{Schema: workgraph.SchemaDep, From: "wi_a", To: "wi_b", Kind: workgraph.DepFinishToStart},
		},
		Limits: workgraph.DefaultLimits(),
	}
	g.PlanDigest = workgraph.DigestGraph(g)
	return g
}

func TestPutAndGetImmutableVersions(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "proj1")
	repo := New(st)
	ctx := context.Background()

	g1 := sampleGraph(1)
	if err := repo.PutVersion(ctx, "proj1", g1); err != nil {
		t.Fatal(err)
	}
	// cannot overwrite
	if err := repo.PutVersion(ctx, "proj1", g1); err == nil {
		t.Fatal("expected immutable")
	}
	// replan v2
	g2 := sampleGraph(2)
	g2.Items = append(g2.Items, workgraph.WorkItem{
		Schema: workgraph.SchemaItem, ID: "wi_c", Intent: "C", Status: workgraph.ItemOptional,
		Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 3,
	})
	g2.PlanDigest = workgraph.DigestGraph(g2)
	if err := repo.PutVersion(ctx, "proj1", g2); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkObsolete(ctx, "proj1", "g_main", 1); err != nil {
		t.Fatal(err)
	}
	// v1 still queryable
	got1, err := repo.GetVersion(ctx, "proj1", "g_main", 1)
	if err != nil || got1.Version != 1 || len(got1.Items) != 2 {
		t.Fatalf("%+v %v", got1, err)
	}
	got2, err := repo.GetVersion(ctx, "proj1", "g_main", 2)
	if err != nil || len(got2.Items) != 3 {
		t.Fatalf("%+v %v", got2, err)
	}
	vers, err := repo.ListVersions(ctx, "proj1", "g_main")
	if err != nil || len(vers) != 2 || vers[0] != 1 || vers[1] != 2 {
		t.Fatalf("%v %v", vers, err)
	}
}

func TestDirectRunOneNode(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "proj2")
	repo := New(st)
	ctx := context.Background()
	g, err := workgraph.MaterializeDirectRun("g_direct", "docs fix", "worker", st.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.PutVersion(ctx, "proj2", g); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetVersion(ctx, "proj2", g.GraphID, 1)
	if err != nil || !got.DirectRunEquivalent || len(got.Items) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestSchemaInventory(t *testing.T) {
	inv := Inventory()
	for _, k := range []string{"no_provider_credentials", "no_v08_federation_ownership", "workgraph_versions"} {
		if inv[k] == "" {
			t.Fatalf("missing %s", k)
		}
	}
}

func TestMigrationReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reopen.db")
	ctx := context.Background()
	st, err := storage.Open(ctx, storage.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	h, err := st.Health(ctx)
	if err != nil || !h.OK || h.SchemaVersion != storage.CurrentSchemaVersion {
		t.Fatalf("health %+v err %v", h, err)
	}
	_ = st.Close()
	st2, err := storage.Open(ctx, storage.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	h2, err := st2.Health(ctx)
	if err != nil || h2.SchemaVersion != storage.CurrentSchemaVersion {
		t.Fatalf("%+v", h2)
	}
}
