# Storage Migration

LoopCoder v0.8 upgrades the machine-local SQLite database from the published
v0.7 schema version 9 to schema version 30. The database remains outside every
repository at `$LOOPCODER_HOME/data/loopcoder.db`, or
`~/.loopcoder/data/loopcoder.db` when `LOOPCODER_HOME` is unset.

## Plan Before Applying

Planning is the default and is side-effect-free:

```text
loopcoder migrate storage
loopcoder migrate storage --format json
loopcoder migrate storage --database /absolute/path/to/loopcoder.db --format json
```

The command opens an existing database read-only, verifies SQLite integrity and
contiguous migration history, and reports the ordered migration steps. It does
not create the database, a parent directory, or a backup directory. The JSON
contract is `loopcoder.storage_migration.v1` and includes:

- `plan.source_schema_version` and `plan.target_schema_version`;
- `plan.status`: `fresh`, `current`, or `upgrade-required`;
- `plan.steps[]` and `plan.plan_fingerprint`;
- `plan.backup_required` and `plan.backup_directory`;
- `rollback.supported`, `rollback.strategy`, and stable limitation codes.

An existing corrupt database, incomplete migration history, symlink, non-file
path, or schema newer than the running binary fails without mutation.

## Apply

After reviewing the plan, apply it explicitly:

```text
loopcoder migrate storage --apply
loopcoder migrate storage --apply --format json
```

The apply path delegates to the same storage engine used by normal LoopCoder
commands. For a v0.7 database it performs these operations in order:

1. Capture a consistent schema-9 SQLite image with `VACUUM INTO` before opening
   the migration write transaction.
2. Store the image under `$LOOPCODER_HOME/data/backups/` with owner-only file
   permissions.
3. Compute and verify its SHA-256 digest.
4. Apply schema versions 10 through 30 in one transaction. A failed statement
   or interrupted transaction cannot leave the source marked as schema 30.
5. Reopen the result read-only, verify required tables, integrity, and durable
   run-graph health.
6. Reopen the backup read-only and verify its schema, migration history,
   integrity, path confinement, permissions, and recorded SHA-256 digest.

The result status is `created`, `migrated`, or `no-op`. Re-running `--apply` on
an already-current database is an idempotent `no-op`; it does not create a
second v0.7 backup. Project identities, run trees, reports, and legacy import
history remain in the same machine-local database.

## Recovery Point

A successful v0.7 upgrade returns a `backup` object with `verified: true`, plus
the same path and digest in `rollback.backup_path` and
`rollback.backup_sha256`. Do not use a backup unless all three values agree.
The backup is local runtime state and must never be committed, attached to an
issue, or copied into a repository.

The stable rollback limitation codes mean:

| Code | Meaning |
| --- | --- |
| `requires-all-loopcoder-processes-stopped` | Stop conductors, workers, and direct LoopCoder commands before replacing SQLite files. |
| `restore-copy-never-mutate-backup` | Restore from a copy. Keep the verified backup unchanged. |
| `discards-v0.8-only-state` | Restoring schema 9 removes state written only after the v0.8 migration. |
| `v0.7-cannot-open-v0.8-schema` | A v0.7 binary must open the restored schema-9 copy, never the schema-30 database. |

## Offline Rollback

Rollback is deliberately not an automatic LoopCoder command. Replacing an
active SQLite database could mix a restored file with live WAL/SHM state. Use
the following macOS procedure only when the migration result reported a
verified backup and rollback is necessary:

1. Stop every LoopCoder process.
2. Preserve the complete current `data` directory, including any `-wal` and
   `-shm` files, outside the live path.
3. Copy the verified schema-9 backup to a temporary file beside
   `loopcoder.db`; do not move or edit the backup itself.
4. Set the temporary file to mode `0600`.
5. Remove the stopped database's `loopcoder.db-wal` and `loopcoder.db-shm`, then
   atomically rename the temporary copy to `loopcoder.db`.
6. Run the v0.7 binary's read-only `doctor` or `projects show` command before
   resuming work.

If the upgrade failed before returning `backup.verified: true`, leave the
source database and any recovery files untouched. The migration transaction
does not claim schema 30 on failure; diagnose the original error, including
`ENOSPC`, permissions, corruption, or cancellation, before retrying.

## Release Evidence

The v0.8 release smoke uses the published v0.7.0 macOS arm64 binary to create a
real schema-9 project database. The staged v0.8 binary must then pass read-only
planning, apply, repeated no-op, backup verification, and a restored-backup
open with the v0.7 binary. Unit fixtures additionally cover corrupt input,
interruption at every backup boundary, and disk-full cleanup.
