# Remove nested/federation/state-branch/cross-machine leases (V090-077)

Package: [`internal/nofed`](../../internal/nofed)  
Issue: [#1191](https://github.com/jasonhnd/loopcoder/issues/1191)

## Purpose

v0.9 does **not** support distributed local DB peers or concurrent multi-Mac
ownership. Nested/federation/state-branch/conductor-lease machinery is removed
after Work Graph + terminal GitHub handoff.

## Removed

nested plan/scope, agent federation/locks, state branches/publication, conductor
and cross-machine leases, state-DB push/pull/merge.

## Preserved

Work Graph, native child containment, terminal GitHub rehydration, v0.8 export
reader (read-only).

## Verification

```bash
go test ./internal/nofed/
```
