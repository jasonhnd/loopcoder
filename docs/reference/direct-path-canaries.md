# Direct-path canaries (V090-036)

Package: [`internal/directcanary`](../../internal/directcanary)  
Issue: [#1138](https://github.com/jasonhnd/loopcoder/issues/1138)

## Purpose

First source-build usability checkpoint for the explicit-route product path.
Prove end-to-end delivery in two disposable **consumer** repositories:

| Fixture | Change surface |
| --- | --- |
| `docs-only` | documentation-only commit |
| `small-go` | small Go code commit |

This is ordinary-development acceptance evidence — not a release artifact and not
LoopCoder self-bootstrap.

## Pipeline under test

Deterministic fakes only (no network, no real model calls):

1. `preflight` — launch readiness gate  
2. `intake` — immutable issue snapshot  
3. `routepin` — explicit provider/model/effort pin  
4. `wtclaim` — isolated worktree/branch claim (runtime root outside customer repo)  
5. `directattempt` — single worker launch after `start:rendered` (terminal + UI bridge)  
6. `localverify` — focused verification plan  
7. `commitstage` — idempotent local commit  
8. `hookpolicy` — respect hooks; no silent bypass  
9. `pushstage` — idempotent push with timeout reconciliation  
10. `prstage` — single PR create/adopt  
11. `ciwatch` — zero-model required-check wait  
12. `mergegate` — read-only verifier only after CI ready; explicit human gate  
13. `deliveryresume` — delivery-only resume without worker replay  

## Fault scenarios

| Fault | Expectation |
| --- | --- |
| (none) golden | one worker launch, route match, PR + human approve_merge |
| `push_timeout` | adopt remote side-effect; resume plan never proposes new worker |
| `delivery_resume` | full path + terminal resume plan is `done`; still one launch |
| `worker_fail` | single failed launch; no delivery side-effects beyond cleanup |
| `cancel` | cleanup terminal, reservation released, zero residue |
| `ui_reconnect` | start report re-rendered after reconnect; one launch |
| `changed_head` | CI head change invalidates readiness; verifier stale on shift |

## Evidence

`loopcoder.directcanary.manifest.v1` JSON under the canary evidence directory:

- `tested_sha` — commit under test  
- `requested_route` / `actual_route` / `route_match`  
- `worker_launch_count` (must be 1 on success paths)  
- `provider_calls_during_ci` (must be 0)  
- redacted events/side-effects; fail closed on `/Users/...`, `HOME=`, token shapes  

## Residue

`ScanResidue` walks the **customer checkout only**. Runtime state must live under
the canary runtime root outside that tree. Any `.loopcoder` (or related markers)
in the consumer repo fails the canary.

## Non-goals

- Auto-routing, decomposition, sub-agent ownership  
- Merge automation / auto-merge  
- Real provider integration as a PR gate (opt-in owner path only)  
- LoopCoder developing LoopCoder via compile/dispatch/tick  

## Verification

```bash
go test ./internal/directcanary/
```
