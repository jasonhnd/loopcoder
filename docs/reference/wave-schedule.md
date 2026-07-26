# Deterministic bounded-wave scheduling (V090-061)

Package: [`internal/waveschedule`](../../internal/waveschedule)  
Issue: [#1172](https://github.com/jasonhnd/loopcoder/issues/1172)

## Purpose

Plan ready work into bounded waves from readiness + resource limits. Persist
plans before claims. Emit **immutable completion candidates** for V090-100
integration. Never merge, integrate, or close WorkItems.

## Defaults

- Max active top-level workers = 1 (serial)
- Parallelism requires capacity and disjoint worktrees
- Waiting: zero provider/model calls

## Verification

```bash
go test ./internal/waveschedule/
```
