# Smart-routing end-to-end acceptance canary (V090-055)

Package: [`internal/routecanary`](../../internal/routecanary)  
Issue: [#1166](https://github.com/jasonhnd/loopcoder/issues/1166)

## Purpose

P4 phase acceptance: prove explicit and automatic routes across Codex, Claude,
Gemini, Antigravity, and Grok using **deterministic fixtures only**. Optional real
observation smoke is owner-opt-in and **never required for PR CI**.

## Matrix (fixtures)

| Scenario | Proves |
| --- | --- |
| pin_unchanged_all_providers | pin wins; no silent substitution |
| pin_fail_closed_no_fallback | ineligible pin does not fall back |
| automatic_prefers_near_reset | burn urgency soft preference |
| no_route_missing_install | hard ineligible → no_route |
| unknown_quota_not_zero | unknown ≠ exhausted zero |
| rate_limit_excluded | rate limit soft exclude |
| successor_pre_launch_authorized | causal successor + evidence preserved |
| ambiguous_no_auto_fallback | needs-human |
| future_provider_kit_no_core_edit | kit checklist without core edits |
| explain_matches_decision_digest | replay + explain parity |
| policy_modes_replayable | balanced/burn/preserve digests |
| soft_reservation_conflict | concurrent capacity conflict |

## Manifest

`Run(now, preProdSHA)` emits `loopcoder.route.canary.manifest.v1` with scenario
results, resource notes (`no_live_provider_calls`, `no_child_processes`, …), and a
stable digest for exact-SHA archival.

## Verification

```bash
go test ./internal/routecanary/
```

## Non-goals

Live quotas, self-bootstrap, work graph, release packaging, or flaky network.
