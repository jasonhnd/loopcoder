# Compact Store Resilience (V090-011)

Package: [`internal/store`](../../internal/store)
Issue: [#1103](https://github.com/jasonhnd/loopcoder/issues/1103)

Applies to both machine (`loopcoder.machine.v1`) and project
(`loopcoder.project.v1`) authority stores. The foundation enforces one-writer
local SQLite policy; there is no distributed lock or cross-Mac multi-writer.

## Operating pragmas and pool limits

| Setting | Value | Notes |
| --- | --- | --- |
| `journal_mode` | `WAL` | Required; set at open |
| `synchronous` | `NORMAL` | Paired with WAL |
| `busy_timeout` | 5s default (`DefaultBusyTimeout`) | Overridable via `Options.BusyTimeout` |
| `wal_autocheckpoint` | 1000 pages | Default SQLite-scale |
| `MaxOpenConns` | 1 | Hard pool bound |
| `MaxIdleConns` | 1 | Matches open bound |
| Conn max lifetime | none | Single long-lived conn |

Clean close runs `PRAGMA wal_checkpoint(TRUNCATE)` under a bounded independent
cleanup context (`CloseCleanupTimeout`), then removes the unclean-open marker.

## Unclean-open marker

Sidecar: `<dbpath>.open` (owner-only, no paths inside domain payloads).

| Event | Behavior |
| --- | --- |
| Successful `Open` | Write marker with pid + timestamp |
| Clean `Close` | Remove marker |
| Abrupt process death | Marker remains |
| Next `Open` | `OpenReport.Recovered=true`; committed rows preserved |

Projections remain disposable; events (when present) are the rebuild source.

## Failure classes (redacted)

| Class | Typed / signals | Operator action |
| --- | --- | --- |
| `busy_exhausted` | `ErrBusy`, SQLITE_BUSY/LOCKED | Retry with backoff; one writer |
| `disk_full` | `ErrDiskFull`, SQLITE_FULL | Free space; do not delete live DB |
| `permission` | `ErrPermission` | Restore 0600/0700 owner-only |
| `corruption` | `ErrCorrupt`, integrity_check | Quarantine + restore backup to **new** path |
| `incompatible_version` | `ErrIncompatibleVersion` | Use compatible binary; never auto-recreate over data |
| `cancelled` | context cancel/deadline | Retry; no repair |

Use `store.Classify(err)` and `store.RedactPath` for logs. Diagnostics never
include event payloads.

## Quarantine

On open-time **corruption** or **unsupported schema version**, the database and
sidecars (`-wal`, `-shm`, `.open`) are moved to
`<parent>/quarantine/` (or `Options.QuarantineDir`). The same `Open` call does
**not** create a replacement DB. Wrong format identity (role mismatch) fails
closed without moving the file.

## Backup / restore

| API | Role |
| --- | --- |
| `Store.Backup(ctx, destPath)` | Online WAL truncate + owner-only file copy; 0600 dest |
| `VerifyBackupOpen` | Pre-open SHA-256 + metadata/store_id check |

Restore is “open the backup path as a separate store”; never overwrite a live
path in place. Owner-only permissions apply to backup files.

## Write transactions

`WithWriteTx` rolls back with an independent `context.Background()` timeout
(`RollbackCleanupTimeout`) so caller cancellation cannot return a poisoned
connection to the single-conn pool.

## Tests

```bash
go test ./internal/store ./internal/authoritystore -count=1
```
