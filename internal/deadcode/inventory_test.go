package deadcode_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/deadcode"
)

func TestInventoryBeforeAfter(t *testing.T) {
	inv := deadcode.BuildInventory()
	if len(inv.Before) <= len(inv.After) {
		t.Fatalf("before=%d after=%d", len(inv.Before), len(inv.After))
	}
	if err := deadcode.AssertAllRemovedHavePR(inv); err != nil {
		t.Fatal(err)
	}
	if !deadcode.LicensePreserved(inv) {
		t.Fatal("license")
	}
	if !deadcode.ForbiddenUserDBMigration() {
		t.Fatal()
	}
	if deadcode.NoNewBehavior() != "inventory_only" {
		t.Fatal()
	}
	s := deadcode.Summarize(inv)
	if s["removed"] < 5 {
		t.Fatalf("%v", s)
	}
}

func TestResidualUnreachable(t *testing.T) {
	if !deadcode.ResidualUnreachable("compile") {
		t.Fatal()
	}
	if deadcode.ResidualUnreachable("internal/privacy") {
		t.Fatal()
	}
}

func TestMatchPrefix(t *testing.T) {
	m := deadcode.MatchPrefix("internal/")
	if len(m) == 0 {
		t.Fatal()
	}
}
