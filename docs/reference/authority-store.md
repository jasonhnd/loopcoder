# v0.9 Authority Store Topology (V090-004)

Package: [`internal/authoritystore`](../../internal/authoritystore)  
Issue: [#1096](https://github.com/jasonhnd/loopcoder/issues/1096)

## Entry points

| API | Role | Format identity | File (target layout) |
| --- | --- | --- | --- |
| `OpenMachine` | `machine` | `loopcoder.machine.v1` | `$LOOPCODER_HOME/data/machine.db` (paths land in V090-005) |
| `OpenProject` | `project` | `loopcoder.project.v1` | `$LOOPCODER_HOME/projects/<id>/project.db` |
| `OpenLegacyReadOnly` | compatibility | v0.8 storage schema | existing legacy DB only |

Callers **must** choose a role. There is no untyped “open any SQLite” API for
new v0.9 writers.

## Rules

1. Machine and project stores reuse the compact foundation (`internal/store`) but
   keep independent format identities, metadata, and migration ledgers.
2. Opening the same path under two roles fails closed (`ErrRoleMismatch` or
   format identity validation).
3. No cross-database transactions (`BeginCrossDBTransaction` always errors).
4. No domain tables yet — registry, events, and provider facts arrive in later
   P1 issues.
5. v0.8 `internal/storage` is reachable only via `OpenLegacyReadOnly`.

## Disposition of legacy opens

| Surface | Disposition |
| --- | --- |
| `internal/store.Open` with default format | Foundation/tests only; not a v0.9 product entry |
| `internal/authoritystore.OpenMachine/OpenProject` | **Only** permitted v0.9 writer entry |
| `internal/storage.Open` | Compatibility / migration exporters; not new product path |
| `internal/storage.OpenReadOnly` | Used by `OpenLegacyReadOnly` |
| Repo-local `.loopcoder` fallbacks | Forbidden on v0.9 path (enforced in V090-005 layout) |

## Resilience (V090-011)

Shared foundation settings (WAL, single-conn pool, busy timeout, unclean-open
markers, quarantine, backup) are documented in
[`store-resilience.md`](store-resilience.md). Machine and project roles use the
same operational limits; they differ only by path and format identity.

## Tests

```bash
go test ./internal/authoritystore ./internal/store -count=1
```
