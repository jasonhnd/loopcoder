# Event, log, runtime-file retention and garbage collection (V090-087)

Package: [`internal/retention`](../../internal/retention)  
Issue: [#1182](https://github.com/jasonhnd/loopcoder/issues/1182)

## Purpose

Bound append-only events, logs, output excerpts, UI delivery evidence, temporary
files, stale worktrees, and backups **without** deleting lifecycle truth or
customer Git data.

## Classes and defaults

| Class | Max age (default) | Archive? | Expendable? | Never delete? |
| --- | --- | --- | --- | --- |
| events | 90d | yes | no | no |
| logs | 30d | yes | yes | no |
| output_excerpt | 30d | yes | yes | no |
| ui_delivery_evidence | 60d | yes | yes | no |
| temp_files | 2d | no | yes | no |
| stale_worktree | 7d | no | yes | no |
| backups | 60d | yes | no | no |
| audit_minimum | 365d | no | no | **yes** |
| customer_repo / credentials / unknown_file | — | — | — | **yes** |

Owner overrides may tighten/loosen caps but cannot lift `NeverDelete`.

## Holds (always, regardless of age/size)

`active`, `nonterminal`, `attention`, `unacknowledged`, `migration`, `ambiguous`,
`audit_minimum`, `never_delete_class`, `path_not_contained`.

## Dry-run plan

`DryRun` returns a deterministic `Plan`:

- each candidate id, class, redacted relative path, bytes, age
- action: `hold` | `archive` | `delete`
- hold reason and human reason
- expected reclaim bytes, store generations, backup rule
- home basename only (no machine-identifying absolute paths by default)

## Apply

`ApplyPlan` refuses dry-run plans, held candidates, and path escapes; action is
idempotent by candidate id. No real filesystem mutation in this package (pure
policy).

## Disk-full policy

- Sets `disk_full_stop_admit`
- May prune only preapproved **expendable** classes (e.g. in-window temp)
- Never silently deletes audit/project truth (events within window stay held)

## Boundaries

- No cloud archive, repo history rewrite, or credential cleanup
- No automatic destructive default (dry-run first)
- Projection rebuild/checkpoint compaction is separate from immutable event
  deletion policy (events archive-eligible only after max age)

## Verification

```bash
go test ./internal/retention/
```
