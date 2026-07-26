# Explicit workflow definition and materialization (V090-060)

Package: [`internal/workflowdef`](../../internal/workflowdef)  
Issue: [#1171](https://github.com/jasonhnd/loopcoder/issues/1171)

## Purpose

Accept user-authored YAML/JSON workflow definitions, normalize to a stable plan
digest, require approval tied to that digest, and materialize one immutable Work
Graph version. No planner model, ROADMAP marker, GitHub epic, or auto-split can
create WorkItems.

## Flow

1. `ParseYAML` / `ParseJSON`  
2. `Normalize` / `DryRunJSON` (byte-stable, nonmutating)  
3. `Approve(digest, actor, reason)`  
4. `Registry.Materialize` (idempotent on same digest)  

## Forbidden sources

`roadmap_compile`, `synthetic_epic`, `self_bootstrap`, `ROADMAP.md`,
`github_epic`, `auto_split`, `model_generated_graph`.

## Verification

```bash
go test ./internal/workflowdef/
```
