# Adaptive observation refresh and cooldown (V090-039)

Package: [`internal/obsrefresh`](../../internal/obsrefresh)  
Issue: [#1142](https://github.com/jasonhnd/loopcoder/issues/1142)

## Purpose

Refresh provider observation facts often enough for safe routing without
busy-polling or quota burn. Health and cooldown are **separate** from auth/model/
quota fact payloads.

## Demand refresh

- Fresh within TTL → reuse (no probe)
- Concurrent in-flight → coalesce (one probe per source)
- Failure → keep last observation digest + `installation_known`; mark
  stale/unknown/unavailable (never “healthy” or zero capacity)
- Failure backoff with bounded jitter and max cap; survives `Checkpoint`/`Restore`

## Cooldown

Rate-limit failures open an **account-protecting** cooldown. Manual refresh is
blocked unless explicit override evidence is supplied.

## Health rendering

| Class | Capacity claim |
| --- | --- |
| healthy | use_last_facts |
| stale / unknown / unavailable / cooldown / degraded | unknown_capacity |

## Non-goals

Route choice, real providers, coding-model polling during wait.

## Verification

```bash
go test ./internal/obsrefresh/
```
