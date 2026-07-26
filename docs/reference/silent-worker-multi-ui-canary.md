# Silent-worker multi-UI visibility canary (V090-024)

Package: [`internal/silentcanary`](../../internal/silentcanary)  
Issue: [#1139](https://github.com/jasonhnd/loopcoder/issues/1139)

## Purpose

Hardened visibility checkpoint before automatic routing and workflows. Prove a
**12-minute silent worker** path with multi-UI report parity and clean process
ownership — using an **injected clock** (no wall-clock correctness) and **zero
provider/model polling** for report scheduling.

## Clients under test

| Client ID | Role |
| --- | --- |
| `term` | Terminal reference UI (required) |
| `uibridge` | Generic UI bridge client |
| `conform` | Independent black-box conformance client |

All three must render the same report **content digests** on the complete path.

## Report schedule (injected time)

1. **start** — attempt begins, worker silent-launched  
2. **+5m** — periodic / five-minute status receipt  
3. **+10m** — periodic or no-progress attention  
4. **+12m** — terminal (complete/cancel) or blocker (resource breach)  

Status during the silent interval exposes: process liveness, last concrete
progress, resources, next report time, and final-mile stage.

## Variants

| Variant | Expectation |
| --- | --- |
| `complete` | start→5m→10m→terminal; digest parity; one provider launch |
| `cancel` | terminal cancelled; zero survivors; reservation released |
| `ui_reconnect` | required client disconnect/reconnect replays; no worker restart |
| `core_restart` | scheduler snapshot/restore; no duplicate start; no worker restart |
| `required_client_outage` | mid-interval outage then restore/replay |
| `resource_breach` | blocker report; cleanup; reservation released |
| `ambiguous_child` | spawn-child fixture; attention if escape; zero survivors after cleanup |

## Evidence

`loopcoder.silentcanary.manifest.v1`:

- `tested_sha`, `simulated_elapsed`, `provider_calls` (must be 1)  
- `report_kinds`, `client_digests`, `digest_parity`  
- `worker_restarts` (0), `surviving_children` (0), `reservation_held` (false)  
- Fail closed on `/Users/...`, `HOME=`, tokens, and machine-identifying keys  

## Non-goals

- Real Paseo smoke (see V090-093)  
- Auto-routing, workflows, model-generated progress  
- LoopCoder self-bootstrap  

## Verification

```bash
go test ./internal/silentcanary/
```
