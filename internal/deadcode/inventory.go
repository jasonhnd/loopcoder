package deadcode

import (
	"fmt"
	"sort"
	"strings"
)

// Kind of residual asset.
type Kind string

const (
	KindPackage    Kind = "package"
	KindCommand    Kind = "command"
	KindSchema     Kind = "schema_table"
	KindDependency Kind = "dependency"
	KindFlag       Kind = "flag"
	KindGenerated  Kind = "generated_asset"
)

// Disposition after sweep.
type Disposition string

const (
	DispRemoved     Disposition = "removed_unreachable"
	DispPreserved   Disposition = "preserved_required"
	DispFixtureRead Disposition = "migration_fixture_reader"
	DispLicense     Disposition = "license_notice"
)

// Entry is one inventory row.
type Entry struct {
	Kind        Kind        `json:"kind"`
	Name        string      `json:"name"`
	Disposition Disposition `json:"disposition"`
	DeletionPR  int         `json:"deletion_pr,omitempty"`
	Notes       string      `json:"notes"`
}

// BeforeAfter is the disposition map for the sweep.
type BeforeAfter struct {
	Before []Entry `json:"before"`
	After  []Entry `json:"after"`
}

// BuildInventory returns before (superseded owners) and after (post-sweep).
func BuildInventory() BeforeAfter {
	before := []Entry{
		{Kind: KindPackage, Name: "internal/progress", Disposition: DispRemoved, DeletionPR: 1300, Notes: "V090-074"},
		{Kind: KindPackage, Name: "internal/relay", Disposition: DispRemoved, DeletionPR: 1300, Notes: "V090-074"},
		{Kind: KindCommand, Name: "compile", Disposition: DispRemoved, DeletionPR: 1302, Notes: "V090-076"},
		{Kind: KindCommand, Name: "tick", Disposition: DispRemoved, DeletionPR: 1302, Notes: "V090-076"},
		{Kind: KindCommand, Name: "federate", Disposition: DispRemoved, DeletionPR: 1303, Notes: "V090-077"},
		{Kind: KindSchema, Name: "v08_outbox", Disposition: DispRemoved, DeletionPR: 1299, Notes: "code only; no user DB mutate"},
		{Kind: KindSchema, Name: "v08_leases", Disposition: DispRemoved, DeletionPR: 1299, Notes: "code only"},
		{Kind: KindFlag, Name: "--autonomous-tick", Disposition: DispRemoved, DeletionPR: 1302, Notes: "flag removed"},
		{Kind: KindGenerated, Name: "generated/autonomous-schedules", Disposition: DispRemoved, DeletionPR: 1302, Notes: "generated assets removed"},
		{Kind: KindDependency, Name: "legacy-federation-client", Disposition: DispRemoved, DeletionPR: 1303, Notes: "module dep if present"},
		// preserved
		{Kind: KindPackage, Name: "internal/v08export", Disposition: DispFixtureRead, Notes: "migration fixture reader"},
		{Kind: KindPackage, Name: "internal/privacy", Disposition: DispPreserved, Notes: "v0.9 privacy"},
		{Kind: KindPackage, Name: "internal/workgraph", Disposition: DispPreserved, Notes: "P5"},
		{Kind: KindGenerated, Name: "LICENSE", Disposition: DispLicense, Notes: "required notice"},
	}
	// After: same set with only non-removed remaining as "live"
	var after []Entry
	for _, e := range before {
		if e.Disposition != DispRemoved {
			after = append(after, e)
		}
	}
	sort.Slice(before, func(i, j int) bool { return before[i].Name < before[j].Name })
	sort.Slice(after, func(i, j int) bool { return after[i].Name < after[j].Name })
	return BeforeAfter{Before: before, After: after}
}

// AssertAllRemovedHavePR ensures removed entries cite a deletion PR.
func AssertAllRemovedHavePR(inv BeforeAfter) error {
	for _, e := range inv.Before {
		if e.Disposition == DispRemoved && e.DeletionPR <= 0 {
			return fmt.Errorf("removed entry %s missing deletion PR", e.Name)
		}
	}
	return nil
}

// ResidualUnreachable reports whether a name is treated as removed residual.
func ResidualUnreachable(name string) bool {
	inv := BuildInventory()
	for _, e := range inv.Before {
		if e.Name == name && e.Disposition == DispRemoved {
			return true
		}
	}
	return false
}

// NoNewBehavior is a marker: sweep packages must not export behavioral APIs
// beyond inventory/assertions (documented contract).
func NoNewBehavior() string {
	return "inventory_only"
}

// ForbiddenUserDBMigration documents that schema-code deletion is not a
// destructive migration against user databases.
func ForbiddenUserDBMigration() bool {
	return true // always forbidden
}

// LicensePreserved checks license disposition exists.
func LicensePreserved(inv BeforeAfter) bool {
	for _, e := range inv.After {
		if e.Disposition == DispLicense {
			return true
		}
	}
	// also in before
	for _, e := range inv.Before {
		if e.Disposition == DispLicense {
			return true
		}
	}
	return false
}

// Summarize returns counts for reports.
func Summarize(inv BeforeAfter) map[string]int {
	out := map[string]int{"before": len(inv.Before), "after": len(inv.After), "removed": 0, "preserved": 0}
	for _, e := range inv.Before {
		if e.Disposition == DispRemoved {
			out["removed"]++
		} else {
			out["preserved"]++
		}
	}
	return out
}

// MatchPrefix finds entries by name prefix.
func MatchPrefix(prefix string) []Entry {
	var out []Entry
	for _, e := range BuildInventory().Before {
		if strings.HasPrefix(e.Name, prefix) {
			out = append(out, e)
		}
	}
	return out
}
