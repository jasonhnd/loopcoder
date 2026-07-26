# Quota normalization, burn urgency, reserve, and reliability policy (V090-052)

Package: [`internal/quotapolicy`](../../internal/quotapolicy)  
Issue: [#1162](https://github.com/jasonhnd/loopcoder/issues/1162)

## Purpose

Soft-rank **hard-eligible** routes using normalized window features — never fake
cross-provider absolute tokens. Prefer capacity that will expire soon, hold
reserves for higher capability classes, and apply reliability/concurrency as soft
signals only.

This package does **not** change immutable pin or hard eligibility semantics
(V090-051).

## Window kinds

`five_hour` | `weekly` | `credit` | `rate_limit` | `other`

A route is bounded by its **most scarce known** window. Unrelated surplus (e.g.
credits) cannot mask an exhausted five-hour or weekly window.

## Evidence classes

| Class | Soft treatment |
| --- | --- |
| `exact` | Full remaining fraction / burn urgency |
| `estimated` | Slight discount on remaining |
| `unknown` / `missing` | Explicit uncertainty penalty; **not** numeric zero |
| `stale` | Stronger penalty; **not** numeric zero |

## Burn urgency

`burn ≈ remaining × (0.35 + 0.65 × near_reset)` where `near_reset` is 1 when
reset is imminent and 0 beyond `NearResetHorizon` (default 2h).

## Reserves

Configurable `SoulReserveFraction` / `TeraReserveFraction`. Lower-class tasks
cannot soft-spend capacity at or below the higher-class reserve floor
(`reserve.breach`).

## Score breakdown

Each candidate emits ordered components: `burn_urgency`, `remaining`,
`reliability`, `concurrency` with weights (default policy version
`quota-policy-v1`). Soft exclusions: exhausted window, rate limit, cooldown,
reserve breach.

## Verification

```bash
go test ./internal/quotapolicy/
```

## Non-goals

Live quota probes, route persistence, soft reservations (V090-099), launch, or
pin mutation.
