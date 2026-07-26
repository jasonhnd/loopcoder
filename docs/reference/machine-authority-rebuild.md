# Machine-authority rebuild and reservation reconciliation (V090-086)

Package: [`internal/machinerebuild`](../../internal/machinerebuild)  
Issue: [#1181](https://github.com/jasonhnd/loopcoder/issues/1181)

## Purpose

Recover safely when `machine.db` is missing or corrupt **without** merging or
copying a live database and without losing independently healthy project
history. Machine inventory and reservations are **current-Mac authority**.

## Project self-identity

Each project store carries `ProjectSelfID`:

- `project_id`, repo owner/name/visibility
- `registration_gen` independent of mutable local path
- schema version

Rebuild treats local path as advisory only.

## Scan rules

Only validated `$LOOPCODER_HOME/projects` children are considered. Rejected with
precise diagnostics (never poison authority):

| Anomaly | Diagnostic |
| --- | --- |
| symlink | `symlink_rejected` |
| file / non-dir | `not_directory` |
| wrong owner | `wrong_owner` |
| missing self-id | `partial_or_missing_self_id` |
| incomplete repo identity | `partial_repo_identity` |
| duplicate project_id | `duplicate_project_id:…` |

## Damaged store

- Missing → rebuild empty registry from valid projects; no backup path.
- Corrupt/present → record **backup path beside** damaged file under
  `backups/` plus content digest. Never silently overwrite or salvage uncertain
  rows into authority.

## Provider facts

Only probe-provenance observations are admitted. `stale_ignored` / empty
provenance snapshots are dropped. No credential fields exist on `ProviderFact`.

## Reservation reconciliation

| Outcome | When |
| --- | --- |
| `live_owned` | live process evidence matches project |
| `released` | no live process for that reservation |
| `attention_required` | unknown ownership, orphan project, or ambiguous live PID |

Unknown live processes never auto-adopt capacity (no double-admit).

## Idempotency

`RebuildIdempotent` compares an evidence fingerprint (accepted ids, rejection
diagnostics, live processes, damaged digest, reservation statuses). Unchanged
evidence marks `idempotent_replay` on a single redacted manifest that still
records backup path/digest.

## Boundaries

- No database merge or cross-Mac state copy
- No credential recovery
- No automatic deletion of damaged data
- No automatic release of uncertain capacity

## Verification

```bash
go test ./internal/machinerebuild/
```
