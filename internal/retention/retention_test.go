package retention_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/retention"
)

func day(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func TestDryRunListsHoldsAndCandidates(t *testing.T) {
	in := retention.InventoryInput{
		HomeBasename: "home-b",
		HomeRoot:     "/tmp/home-b",
		Resources: []retention.Resource{
			{ID: "e1", Class: retention.ClassEvents, RelPath: "projects/a/events/1", Bytes: 100, Age: day(100), ProjectID: "p1", StoreGeneration: 1},
			{ID: "a1", Class: retention.ClassLogs, RelPath: "projects/a/logs/1", Bytes: 50, Age: day(1), Active: true, ProjectID: "p1", StoreGeneration: 1},
			{ID: "t1", Class: retention.ClassTemp, RelPath: "tmp/x", Bytes: 10, Age: day(5), ProjectID: "p1", StoreGeneration: 1},
			{ID: "repo", Class: retention.ClassCustomerRepo, RelPath: "repos/app", Bytes: 999, Age: day(999)},
		},
	}
	p := retention.DryRun(in)
	if !p.DryRun {
		t.Fatal("default dry-run")
	}
	if p.HeldCount < 2 {
		t.Fatalf("held=%d plan=%#v", p.HeldCount, p)
	}
	byID := map[string]retention.Candidate{}
	for _, c := range p.Candidates {
		byID[c.ID] = c
	}
	if byID["a1"].Hold != retention.HoldActive {
		t.Fatalf("active hold: %#v", byID["a1"])
	}
	if byID["repo"].Hold != retention.HoldNeverDelete {
		t.Fatalf("repo: %#v", byID["repo"])
	}
	if byID["e1"].Action != "archive" {
		t.Fatalf("old events should archive: %#v", byID["e1"])
	}
	if byID["t1"].Action != "delete" {
		t.Fatalf("old temp delete: %#v", byID["t1"])
	}
	if p.ExpectedReclaim < 110 {
		t.Fatalf("reclaim=%d", p.ExpectedReclaim)
	}
	// No absolute paths in report paths.
	for _, c := range p.Candidates {
		if strings.HasPrefix(c.RelPath, "/") {
			t.Fatalf("absolute path leaked: %s", c.RelPath)
		}
	}
}

func TestHoldsIgnoreAgePressure(t *testing.T) {
	flags := []struct {
		name string
		mut  func(*retention.Resource)
		hold retention.HoldReason
	}{
		{"attention", func(r *retention.Resource) { r.Attention = true }, retention.HoldAttention},
		{"nonterminal", func(r *retention.Resource) { r.Nonterminal = true }, retention.HoldNonterminal},
		{"unack", func(r *retention.Resource) { r.Unacknowledged = true }, retention.HoldUnacknowledged},
		{"migration", func(r *retention.Resource) { r.Migration = true }, retention.HoldMigration},
		{"ambiguous", func(r *retention.Resource) { r.Ambiguous = true }, retention.HoldAmbiguous},
		{"audit", func(r *retention.Resource) { r.AuditMinimum = true }, retention.HoldAuditMinimum},
	}
	for _, tc := range flags {
		r := retention.Resource{ID: "x", Class: retention.ClassLogs, RelPath: "logs/x", Bytes: 1, Age: day(999)}
		tc.mut(&r)
		p := retention.DryRun(retention.InventoryInput{HomeBasename: "h", Resources: []retention.Resource{r}})
		if len(p.Candidates) != 1 || p.Candidates[0].Action != "hold" || p.Candidates[0].Hold != tc.hold {
			t.Fatalf("%s: %#v", tc.name, p.Candidates)
		}
	}
}

func TestPathEscapeHeld(t *testing.T) {
	p := retention.DryRun(retention.InventoryInput{
		HomeBasename: "h",
		HomeRoot:     "/tmp/h",
		Resources: []retention.Resource{
			{ID: "evil", Class: retention.ClassTemp, RelPath: "../etc/passwd", Bytes: 1, Age: day(99)},
		},
	})
	if p.Candidates[0].Hold != retention.HoldPathEscape {
		t.Fatalf("%#v", p.Candidates[0])
	}
}

func TestDiskFullStopsAdmitPrunesExpendableTemp(t *testing.T) {
	p := retention.DryRun(retention.InventoryInput{
		HomeBasename: "h",
		DiskFull:     true,
		Resources: []retention.Resource{
			{ID: "temp-new", Class: retention.ClassTemp, RelPath: "tmp/n", Bytes: 5, Age: time.Hour}, // within window
			{ID: "events-new", Class: retention.ClassEvents, RelPath: "e/1", Bytes: 5, Age: time.Hour},
			{ID: "audit", Class: retention.ClassAuditMin, RelPath: "audit/1", Bytes: 5, Age: day(999)},
		},
	})
	if !p.DiskFullStopAdmit {
		t.Fatal("expected stop admit")
	}
	byID := map[string]retention.Candidate{}
	for _, c := range p.Candidates {
		byID[c.ID] = c
	}
	if byID["temp-new"].Action != "delete" {
		t.Fatalf("temp under disk-full: %#v", byID["temp-new"])
	}
	if byID["events-new"].Action != "hold" {
		t.Fatalf("events must not silently delete: %#v", byID["events-new"])
	}
	if byID["audit"].Hold != retention.HoldAuditMinimum && byID["audit"].Hold != retention.HoldNeverDelete {
		t.Fatalf("audit: %#v", byID["audit"])
	}
}

func TestApplyIdempotentAndRefusesDryRun(t *testing.T) {
	p := retention.DryRun(retention.InventoryInput{
		HomeBasename: "h",
		Resources: []retention.Resource{
			{ID: "t1", Class: retention.ClassTemp, RelPath: "tmp/x", Bytes: 1, Age: day(9)},
		},
	})
	if _, err := retention.ApplyPlan(p); err == nil {
		t.Fatal("dry-run apply must fail")
	}
	p.DryRun = false
	a1, err := retention.ApplyPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	// duplicate candidate ids idempotent
	p.Candidates = append(p.Candidates, p.Candidates...)
	a2, err := retention.ApplyPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(a1) != len(a2) {
		t.Fatalf("idempotent acted %v vs %v", a1, a2)
	}
}

func TestApplyRefusesHeld(t *testing.T) {
	p := retention.Plan{
		DryRun: false,
		Candidates: []retention.Candidate{
			{ID: "a", Action: "delete", Hold: retention.HoldActive, RelPath: "x"},
		},
	}
	if _, err := retention.ApplyPlan(p); err == nil {
		t.Fatal("must refuse held")
	}
}

func TestOverridesCannotLiftNeverDelete(t *testing.T) {
	base := retention.DefaultPolicies()
	o := retention.Overrides{MaxAge: map[retention.Class]time.Duration{retention.ClassAuditMin: time.Hour}}
	got := retention.ApplyOverrides(base, o)
	if !got[retention.ClassAuditMin].NeverDelete {
		t.Fatal("never delete must remain")
	}
	// Age override not applied to never-delete.
	if got[retention.ClassAuditMin].MaxAge != base[retention.ClassAuditMin].MaxAge {
		t.Fatal("override should not change never-delete class")
	}
}

func TestDeterministicOrdering(t *testing.T) {
	res := []retention.Resource{
		{ID: "b", Class: retention.ClassLogs, RelPath: "l/b", Bytes: 1, Age: day(1)},
		{ID: "a", Class: retention.ClassLogs, RelPath: "l/a", Bytes: 1, Age: day(1)},
		{ID: "c", Class: retention.ClassEvents, RelPath: "e/c", Bytes: 1, Age: day(1)},
	}
	p1 := retention.DryRun(retention.InventoryInput{HomeBasename: "h", Resources: res})
	// reverse input
	res2 := []retention.Resource{res[2], res[0], res[1]}
	p2 := retention.DryRun(retention.InventoryInput{HomeBasename: "h", Resources: res2})
	if len(p1.Candidates) != len(p2.Candidates) {
		t.Fatal("len")
	}
	for i := range p1.Candidates {
		if p1.Candidates[i].ID != p2.Candidates[i].ID {
			t.Fatalf("order drift %s vs %s", p1.Candidates[i].ID, p2.Candidates[i].ID)
		}
	}
}
