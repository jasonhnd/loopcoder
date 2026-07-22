# Quota policy modes, soft reservations, and usage attribution (V090-099)

Package: [`internal/quotamode`](../../internal/quotamode)  
Issue: [#1163](https://github.com/jasonhnd/loopcoder/issues/1163)

## Purpose

Machine-local **soft spending authority** on top of hard eligibility (V090-051)
and soft ranking (V090-052):

1. Owner-selectable modes: `balanced`, `burn_before_reset`, `preserve_premium`
2. Short-lived **soft reservations** so two projects cannot both treat the same
   unreserved remaining fraction as fully available
3. **Usage attribution** that records local drift without rewriting provider
   evidence or fabricating authoritative remaining values

Explicit pins still win or fail closed; modes never substitute a pin.

## Modes

| Mode | Behavior |
| --- | --- |
| `balanced` | Default quotapolicy weights |
| `burn_before_reset` | Boost burn-urgency weight; lower completion headroom |
| `preserve_premium` | Raise soul reserve; deprioritize burning premium for low-class tasks |

## Soft reservations

- Keyed by provider / account / model / window kind  
- Hold a **fraction** of the window (not cross-provider absolute tokens)  
- Headroom rejection distinguishes: exact exhaustion, unknown quota, estimated
  demand exceed, overcommit with active reservations, owner risk acceptance  
- Terminal paths: release, cancel, expire (TTL), reconcile — all idempotent  

## Attribution

Post-attempt `Reconcile` stores `observed_fraction`, `source`, `confidence`, and
`drift = observed - reserved` with an explicit note that local estimates are not
provider-authoritative remaining.

## Verification

```bash
go test ./internal/quotamode/
```

## Non-goals

Live provider/billing, hard monetary budgets, credentials, cross-machine
reservation, or pin mutation.
