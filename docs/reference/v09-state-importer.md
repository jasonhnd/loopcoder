# v0.9 project-state importer and migration report (V090-070)

Package: [`internal/v09import`](../../internal/v09import)  
Issue: [#1184](https://github.com/jasonhnd/loopcoder/issues/1184)

## Purpose

Import the neutral v0.8 export (`internal/v08export`) into machine/project v0.9
stores **transactionally**, preserving terminal history and provenance while
refusing unsafe live-state claims. Emit a human/JSON migration report with an
honest rollback limitation.

## Dry-run

`DryRun: true` reports projects, history, conflicts, omissions, required space,
and target paths **without mutation**.

## Import rules

| Rule | Behavior |
| --- | --- |
| Bundle schema / source digests | required; optional expected bundle digest |
| Idempotency | project/history import keys; reimport skips duplicates |
| Newer v0.9 records | never overwritten (`MarkNewer` / conflict) |
| Per-project atomicity | failed project does not corrupt others |
| Execution authority | imported history is historical; `AuthorizesExecution=false` |
| Nonterminal/live | demoted to attention/historical |
| Credentials / leases / PIDs | not in export; never imported |
| Old state | never deleted/moved automatically |

## Migration report

Includes source/target versions, digests, counts, warnings, conflicts, omissions,
actions, succeeded/failed projects, and:

> post-write rollback requires restoring backup or new stores; importer never
> provides binary rollback or automatic old-state deletion

## Verification

```bash
go test ./internal/v09import/
```
