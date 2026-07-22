# Retire duplicate provider inventory/router writers (V090-075)

Package: [`internal/noproviderdup`](../../internal/noproviderdup)  
Issue: [#1189](https://github.com/jasonhnd/loopcoder/issues/1189)

## Purpose

Remove old new-path-callable provider inventory, agent adapters, quota snapshots,
route writers, and fallback decision paths after official adapter/router
conformance (V090-037–V090-055).

## Disposition

| Surface | Disposition |
| --- | --- |
| providerinventory, agent_legacy, routing_write, quota_snapshot_write, reconciliation_write, fallback_decision, raw_sql_repository | **removed** |
| official_adapter, process_invocation_behind_adapter | **facade_only** |
| explicit_pin_reader, historical_route_import | **preserved_reader** |

## Verification

```bash
go test ./internal/noproviderdup/
```
