# Work Graph public contract and materialization boundary (V090-056)

Package: [`internal/workgraph`](../../internal/workgraph)  
Issue: [#1167](https://github.com/jasonhnd/loopcoder/issues/1167)

## Purpose

Define a small **LoopCoder-owned** Work Graph contract. Avoid wholesale import of
v0.8 nested/federation/compile/autonomous-loop concepts.

## Core terms

| Term | Meaning |
| --- | --- |
| WorkItem | LoopCoder graph node (`ownership=loopcoder_workitem`) |
| Attempt | Route attempt — **not** a WorkItem |
| Provider-native child | Provider session/process — **not** a WorkItem |
| GitHub issue/PR | External reference — **not** a WorkItem |

## One-node ≡ direct run

`MaterializeDirectRun` produces a single required WorkItem with
`direct_run_equivalent=true` and **no dependencies**. Materialization itself
introduces **no extra provider call**.

## Multi-node

Requires:

- `explicit_opt_in=true`
- `approved_by` non-empty before any child starts
- stable `plan_digest`
- visible `integration_order` on every item
- limits (`max_items`, `max_depth`, `max_parallel`)

## Mutation / replan

After `execution_started`, changes go through `ApplyReplan` (version++). Completed
`terminal` states on prior items **cannot be rewritten**.

## Forbidden sources

- `roadmap_compile`
- `synthetic_epic`
- `self_bootstrap`

## Verification

```bash
go test ./internal/workgraph/
```

Old nested/federation APIs are **non-authoritative** relative to this contract.
