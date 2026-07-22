# Multi-project global admission and isolation (V090-066)

Package: [`internal/multiproject`](../../internal/multiproject)  
Issue: [#1178](https://github.com/jasonhnd/loopcoder/issues/1178)

## Purpose

One machine, many projects: shared resource budgets, isolated stores/paths/events,
redacted machine summary, explicit archive.

## Verification

```bash
go test ./internal/multiproject/
```
