# Atomic work claim and guarded close (V090-059)

Package: [`internal/workclaim`](../../internal/workclaim)  
Issue: [#1170](https://github.com/jasonhnd/loopcoder/issues/1170)

## Purpose

One eligible WorkItem is owned by **one attempt generation** at a time. Claims
verify graph readiness and create ownership in one critical section. Stale
generations cannot renew or close. Ambiguous expired live execution is
**needs-human** (not auto-reclaimed).

## Claim results

| Code | Meaning |
| --- | --- |
| `claimed` | new owner generation |
| `already_running` | live owner holds item |
| `terminal_reused` | already closed/terminal |
| `blocked` / `not_ready` | readiness failed |
| `needs_human` | ambiguous expiry |
| `stale_generation` | fence mismatch |

## Close

- Fenced by claim_id + generation + executor + attempt  
- Success requires `output_evidence`  
- Idempotent for same terminal  
- Accepted terminals feed `workgraph.EvaluateReady` for dependents  

## Verification

```bash
go test ./internal/workclaim/
```

## Non-goals

Provider launch, scheduler tick, multi-machine leases (local atomic model only).
