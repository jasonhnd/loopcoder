# Graph validation and deterministic ready set (V090-058)

Package: [`internal/workgraph`](../../internal/workgraph) (`ready.go`)  
Issue: [#1169](https://github.com/jasonhnd/loopcoder/issues/1169)

## Purpose

Validate Work Graph executable invariants and compute a **deterministic ready
set** from one immutable graph version plus accepted terminal evidence. Pure
function — no claim, launch, route choice, or model decomposition.

## Validation (`ValidateExecutable`)

Extends `ValidateGraph` with:

- self-loop / multi-node hard-dep cycles  
- missing endpoints, duplicate edges  
- node count / depth / fan-out limits  
- required item hard-depends on optional predecessor (rejected)  
- `output` deps require producer `integration_order` < consumer  

Invalid materialization returns an error; callers must not write partial graphs,
claims, workers, branches, or PRs (`MaterializeIfValid`).

## Ready set (`EvaluateReady`)

| Lifecycle | Meaning |
| --- | --- |
| `ready` | nonterminal; all hard deps succeeded |
| `blocked` | nonterminal; waiting or hard dep failed |
| `terminal` | already finished |
| `ignored` | optional path short-circuit (when applicable) |

Hard deps: `finish_to_start`, `output`. Soft: `soft_order` (never gates).

Ready IDs ordered by `integration_order`, then stable key. Same graph + evidence
→ identical `digest`.

## Verification

```bash
go test ./internal/workgraph/
```

## Non-goals

Claims (V090-059), scheduler launch, provider calls, GitHub side effects.
