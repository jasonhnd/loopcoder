# Persisted route decision and `route explain` (V090-053)

Package: [`internal/routedecision`](../../internal/routedecision)  
Issue: [#1164](https://github.com/jasonhnd/loopcoder/issues/1164)

## Purpose

Compose **hard eligibility** (V090-051), **soft quota ranking** (V090-052), and
**policy modes** (V090-099) into one immutable decision record with stable digests
for replay and a redacted explain surface.

Existing durable CLI wiring lives in `internal/routing` + `loopcoder route
explain|decide`. This package is the v0.9.0 pure decision envelope those layers
(and tests) can adopt without live provider calls.

## Pipeline

1. `eligibility.Evaluate` — pin fail-closed or hard-eligible set  
2. `quotamode.Rank` (wraps `quotapolicy.Rank`) — soft scores + mode  
3. Winner = first hard-eligible, not soft-excluded candidate  
4. Persist by `decision_key` (idempotent on same digest; conflict otherwise)  

## Digests retained

| Field | Source |
| --- | --- |
| `evidence_digest` | caller-provided observation snapshot |
| `eligibility_digest` | hard gate decision |
| `soft_ranking_digest` | base quotapolicy ranking |
| `mode_ranking_digest` | mode-adjusted ranking |
| `digest` | full decision content (excludes store-local id) |

## Explain

`Explain` / `ExplainJSON` emit human text and JSON covering:

- explicit pin precedence or pin fail-closed  
- hard exclusions with reason codes  
- ordered candidates and soft scores  
- tie-break when applicable  
- redaction note (no credentials / raw quota payloads)  

## Verification

```bash
go test ./internal/routedecision/
```

## Non-goals

Provider launch, live observation refresh, multi-project event stream writers
(those attach digests from this package in later wiring).
