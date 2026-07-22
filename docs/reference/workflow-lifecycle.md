# Workflow cancellation, restart, and terminal compaction (V090-064)

Package: [`internal/workflowrecover`](../../internal/workflowrecover)  
Issue: [#1176](https://github.com/jasonhnd/loopcoder/issues/1176)

## Purpose

Deterministic cancel/restart and compact terminal projections without deleting
audit events. Parent success is never optimistic.

## Rules

- Cancel: join running, release unstarted claims, record ambiguous  
- Restart: adopt live children; resume wave/integration seq; no duplicate launch  
- Parent terminal only after durable required child terminals  
- Persist errors suppress parent success  
- Projection carries audit range digest; source events intact  

## Verification

```bash
go test ./internal/workflowrecover/
```
