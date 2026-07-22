package legacystorage

import (
	"fmt"
	"sort"
	"strings"
)

// OpenMode for legacy storage access.
type OpenMode string

const (
	// ModeImmutableRead is the only allowed mode for v0.9 binary reachability.
	ModeImmutableRead OpenMode = "immutable_read"
	// ModeWrite is retired — always denied.
	ModeWrite OpenMode = "write"
	// ModeMigrate is retired — always denied.
	ModeMigrate OpenMode = "migrate"
	// ModeRepair is retired — always denied.
	ModeRepair OpenMode = "repair"
	// ModeTransaction is retired for legacy stores.
	ModeTransaction OpenMode = "transaction"
)

// PathKind classifies a code path that might touch legacy storage.
type PathKind string

const (
	PathOpenForWrite  PathKind = "open_for_write"
	PathSchemaMigrate PathKind = "schema_migrate"
	PathTransaction   PathKind = "transaction"
	PathRepair        PathKind = "repair"
	PathDropTable     PathKind = "drop_table"
	PathImmutableRead PathKind = "immutable_read"
	PathExporterRead  PathKind = "exporter_read"
)

// Disposition of a legacy path in the v0.9 binary.
type Disposition string

const (
	DispDenied       Disposition = "denied"
	DispReadOnlyPort Disposition = "read_only_compat_port"
	DispRemovedCode  Disposition = "removed_from_reachability"
)

// InventoryEntry is one inventoried legacy writer/reader.
type InventoryEntry struct {
	Package     string      `json:"package"`
	Symbol      string      `json:"symbol"`
	Kind        PathKind    `json:"kind"`
	Disposition Disposition `json:"disposition"`
	Notes       string      `json:"notes"`
}

// DefaultInventory lists direct storage/migration/migrate writers and disposition.
func DefaultInventory() []InventoryEntry {
	return []InventoryEntry{
		{Package: "internal/storage", Symbol: "Open", Kind: PathOpenForWrite, Disposition: DispRemovedCode, Notes: "v0.8 write open not reachable from v0.9 commands"},
		{Package: "internal/storage", Symbol: "Migrate", Kind: PathSchemaMigrate, Disposition: DispRemovedCode, Notes: "schema migration write removed from v0.9 command reachability"},
		{Package: "internal/storage", Symbol: "Tx", Kind: PathTransaction, Disposition: DispRemovedCode, Notes: "legacy transaction writers removed from reachability"},
		{Package: "internal/storage", Symbol: "Repair", Kind: PathRepair, Disposition: DispRemovedCode, Notes: "repair path removed"},
		{Package: "internal/migrate", Symbol: "Up", Kind: PathSchemaMigrate, Disposition: DispRemovedCode, Notes: "migrate up not on v0.9 command path"},
		{Package: "internal/migration", Symbol: "Apply", Kind: PathSchemaMigrate, Disposition: DispRemovedCode, Notes: "migration apply not on v0.9 command path"},
		{Package: "internal/v08export", Symbol: "Export", Kind: PathExporterRead, Disposition: DispReadOnlyPort, Notes: "immutable read-only exporter port"},
		{Package: "internal/legacystorage", Symbol: "OpenImmutable", Kind: PathImmutableRead, Disposition: DispReadOnlyPort, Notes: "smallest audited immutable reader"},
	}
}

// OpenRequest is a request to open legacy storage.
type OpenRequest struct {
	Mode OpenMode
	// ForExporter marks V090-069 compatibility port.
	ForExporter bool
	// Command is the CLI/service entry that requested the open.
	Command string
	// Path is the DB path (never written by this package).
	Path string
	// DropTables would delete schema — always denied.
	DropTables bool
}

// Decision for an open/mutate attempt.
type Decision struct {
	Allowed bool
	Reasons []string
	// ImmutableOptions document required open flags when allowed.
	ImmutableOptions []string
}

// RequiredImmutableOptions for the audited reader.
func RequiredImmutableOptions() []string {
	return []string{
		"read_only=true",
		"immutable=1", // SQLite query_only / no create
		"no_migration_pragmas",
		"no_wal_checkpoint_write",
	}
}

// EvaluateOpen denies all write/migrate/repair/transaction opens; allows only
// immutable read via exporter port or explicit immutable reader.
func EvaluateOpen(req OpenRequest) Decision {
	if req.DropTables {
		return Decision{Allowed: false, Reasons: []string{"never mutate/delete user existing DB tables from v0.9"}}
	}
	switch req.Mode {
	case ModeWrite, ModeMigrate, ModeRepair, ModeTransaction:
		return Decision{
			Allowed: false,
			Reasons: []string{fmt.Sprintf("legacy storage mode %s retired from v0.9 command reachability", req.Mode)},
		}
	case ModeImmutableRead:
		if !req.ForExporter && !strings.EqualFold(req.Command, "export-v08") && !strings.EqualFold(req.Command, "legacystorage-read") {
			return Decision{
				Allowed: false,
				Reasons: []string{"immutable legacy read only via exporter/compat port"},
			}
		}
		return Decision{
			Allowed:          true,
			Reasons:          []string{"immutable read-only compatibility port"},
			ImmutableOptions: RequiredImmutableOptions(),
		}
	default:
		return Decision{Allowed: false, Reasons: []string{"unknown open mode"}}
	}
}

// CommandReachable reports whether a command may call a given path kind.
func CommandReachable(command string, kind PathKind) bool {
	cmd := strings.ToLower(strings.TrimSpace(command))
	switch kind {
	case PathImmutableRead, PathExporterRead:
		return cmd == "export-v08" || cmd == "legacystorage-read"
	case PathOpenForWrite, PathSchemaMigrate, PathTransaction, PathRepair, PathDropTable:
		return false
	default:
		return false
	}
}

// SchemaDisposition documents table/code disposition (code only; no user DB delete).
type SchemaDisposition struct {
	Table        string `json:"table"`
	InCode       string `json:"in_code"`        // removed|read_only_export
	UserDBAction string `json:"user_db_action"` // never_auto_mutate
}

// DefaultSchemaDisposition covers representative legacy tables.
func DefaultSchemaDisposition() []SchemaDisposition {
	tables := []string{
		"v08_runs", "v08_progress", "v08_outbox", "v08_leases", "v08_schema_migrations",
	}
	var out []SchemaDisposition
	for _, t := range tables {
		out = append(out, SchemaDisposition{
			Table: t, InCode: "removed", UserDBAction: "never_auto_mutate",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out
}
