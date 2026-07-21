# Compact Store Platform Conformance (V090-001)

This inventory records the v0.9.0 `internal/store` platform boundary after
Darwin-only cleanup. It is living reference evidence for issue `#1091`
(`V090-001`), not a schema design.

## Product platform

| Item | Value |
| --- | --- |
| Supported GOOS/GOARCH | `darwin/arm64` only |
| SQLite driver | `modernc.org/sqlite` (pure Go, no CGO) |
| Fail-closed error | `store.ErrUnsupportedPlatform` |
| Permission model | Owner-only Unix modes (`0700` dirs, `0600` files), no symlink database path or ancestor |

## Kept (v0.9 store path)

| Path | Role |
| --- | --- |
| `internal/store/store.go` | Open/bootstrap/integrity/close foundation |
| `internal/store/schema.go` | Foundation DDL (`store_metadata`, `migration_ledger`) |
| `internal/store/permissions.go` | Shared permission report types |
| `internal/store/permissions_darwin.go` | Darwin owner-only permission enforcement |
| `internal/store/platform.go` | `darwin/arm64` gate and stable unsupported error |
| `internal/store/*_test.go` | Darwin-focused conformance and fail-closed tests |

## Removed from the v0.9 store path

| Path | Reason |
| --- | --- |
| `internal/store/permissions_windows.go` | Windows ACL implementation; not a v0.9 release target |
| `internal/store/permissions_windows_test.go` | Windows-only tests for the removed ACL path |
| `internal/store/permissions_crosscompile_test.go` | Forced Windows cross-compile of the removed ACL path |
| `internal/store/permissions_unix.go` | Renamed/narrowed to Darwin-only `permissions_darwin.go` |

## Intentionally left as v0.8 compatibility (out of scope)

These packages remain for the v0.8 line and migration fixtures. They are **not**
the v0.9 compact store and were not deleted by V090-001:

| Path / area | Notes |
| --- | --- |
| `internal/storage/**` | Legacy schema v31 and helpers; compatibility-only for migration |
| `internal/storage/permissions_*.go` | v0.8 permission files; separate from `internal/store` |
| `internal/pathid/physical_windows.go` | Legacy path identity helper, not store open |
| `internal/supervisedexec/killgroup_windows.go` | Legacy process helper, not store open |
| Release metadata claiming multi-platform v0.8/v0.7 history | Historical truth retained; current product remains `darwin/arm64` |

## Unsupported-platform behavior

1. `Open` calls `requireSupportedPlatform()` before creating files or opening SQLite.
2. Non-`darwin` builds use `permissions_unsupported.go` stubs that only return
   `ErrUnsupportedPlatform` (no weaker permission path).
3. `darwin` with non-`arm64` architecture also fails closed at `Open` /
   `CheckPermissions` with the same error.
4. Diagnostics include only `GOOS/GOARCH` and the stable error text — never
   usernames, home directories, or database contents.

## Conformance checks

Focused local verification (evidence tier `local-focused`):

```bash
go test ./internal/store -count=1
```

Remote CI remains authoritative for repository-wide suites.
