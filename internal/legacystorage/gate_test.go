package legacystorage_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/legacystorage"
)

func TestDenyWriteMigrateRepair(t *testing.T) {
	for _, mode := range []legacystorage.OpenMode{
		legacystorage.ModeWrite, legacystorage.ModeMigrate,
		legacystorage.ModeRepair, legacystorage.ModeTransaction,
	} {
		d := legacystorage.EvaluateOpen(legacystorage.OpenRequest{Mode: mode, Path: "/x.db"})
		if d.Allowed {
			t.Fatalf("%s allowed", mode)
		}
	}
}

func TestAllowImmutableExporter(t *testing.T) {
	d := legacystorage.EvaluateOpen(legacystorage.OpenRequest{
		Mode: legacystorage.ModeImmutableRead, ForExporter: true, Command: "export-v08", Path: "/x.db",
	})
	if !d.Allowed {
		t.Fatal(d)
	}
	if len(d.ImmutableOptions) == 0 {
		t.Fatal("missing immutable options")
	}
}

func TestDenyImmutableWithoutPort(t *testing.T) {
	d := legacystorage.EvaluateOpen(legacystorage.OpenRequest{
		Mode: legacystorage.ModeImmutableRead, Command: "status", Path: "/x.db",
	})
	if d.Allowed {
		t.Fatal("status must not open legacy storage")
	}
}

func TestDropTablesDenied(t *testing.T) {
	d := legacystorage.EvaluateOpen(legacystorage.OpenRequest{
		Mode: legacystorage.ModeImmutableRead, ForExporter: true, Command: "export-v08", DropTables: true,
	})
	if d.Allowed {
		t.Fatal("drop denied")
	}
}

func TestCommandReachability(t *testing.T) {
	if legacystorage.CommandReachable("dispatch", legacystorage.PathOpenForWrite) {
		t.Fatal("write not reachable")
	}
	if !legacystorage.CommandReachable("export-v08", legacystorage.PathExporterRead) {
		t.Fatal("exporter should reach read")
	}
	if legacystorage.CommandReachable("doctor", legacystorage.PathSchemaMigrate) {
		t.Fatal("migrate not reachable")
	}
}

func TestInventoryHasReadOnlyPort(t *testing.T) {
	found := false
	for _, e := range legacystorage.DefaultInventory() {
		if e.Disposition == legacystorage.DispReadOnlyPort {
			found = true
		}
		if e.Kind == legacystorage.PathOpenForWrite && e.Disposition != legacystorage.DispRemovedCode {
			t.Fatalf("write path not removed: %#v", e)
		}
	}
	if !found {
		t.Fatal("missing read-only port entry")
	}
}

func TestSchemaNeverAutoMutate(t *testing.T) {
	for _, s := range legacystorage.DefaultSchemaDisposition() {
		if s.UserDBAction != "never_auto_mutate" {
			t.Fatalf("%#v", s)
		}
	}
}
