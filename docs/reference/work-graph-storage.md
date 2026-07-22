# Work item and dependency schema (V090-057)

Package: [`internal/workgraphstore`](../../internal/workgraphstore)  
Schema: [`internal/storage/workgraph_schema.go`](../../internal/storage/workgraph_schema.go) (storage schema v32)  
Issue: [#1168](https://github.com/jasonhnd/loopcoder/issues/1168)

## Tables

| Table | Role |
| --- | --- |
| `workgraph_versions` | Immutable graph versions (digest, limits, approval, payload) |
| `workgraph_items` | WorkItems with stable_key per version |
| `workgraph_dependencies` | Typed deps (`finish_to_start`, `output`, `soft_order`) |
| `workgraph_events` | Append-only lifecycle events |

## Rules

- Graph primary key is `(project_id, graph_id, version)` — **not** GitHub issue/PR.  
- Replan inserts a **new version**; prior versions remain queryable (`obsolete` flag only).  
- No provider credentials, process truth, report table, or v0.8 federation ownership columns.  
- Cross-project dependency endpoints rejected by repository validation.  

## Migration

Transactional via storage migrations list as version **32**. Idempotent `CREATE TABLE IF NOT EXISTS`. Existing direct-run project rows are unaffected.

## Verification

```bash
go test ./internal/workgraphstore/ ./internal/storage/ -count=1
```
