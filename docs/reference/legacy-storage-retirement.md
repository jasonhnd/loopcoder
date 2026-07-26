# Retire legacy v0.8 storage mutation paths (V090-073)

Package: [`internal/legacystorage`](../../internal/legacystorage)  
Issue: [#1187](https://github.com/jasonhnd/loopcoder/issues/1187)

## Purpose

Remove v0.8 schema migration/write entry points from v0.9 command reachability
after the read-only exporter and v0.9 importer pass. Keep only the smallest
audited **immutable** reader for one-release migration support.

## Allowed open

| Mode | Result |
| --- | --- |
| write / migrate / repair / transaction | **denied** |
| immutable_read via export-v08 / exporter port | allowed with immutable options |
| drop tables | **denied** (never mutate user DB) |

Immutable options: `read_only=true`, `immutable=1`, `no_migration_pragmas`,
`no_wal_checkpoint_write`.

## Inventory

`DefaultInventory` maps `internal/storage`, `migrate`, `migration` writers to
`removed_from_reachability` and exporter/immutable reader to
`read_only_compat_port`.

## User DB

Code may drop old table **symbols**; user databases are never auto-mutated or
deleted (`UserDBAction: never_auto_mutate`).

## Verification

```bash
go test ./internal/legacystorage/
```
