# Successor attempt and fallback boundary (V090-054)

Package: [`internal/successor`](../../internal/successor)  
Issue: [#1165](https://github.com/jasonhnd/loopcoder/issues/1165)

## Purpose

Define when failure may create a **new attempt** with another eligible route.
Never switch provider/model **inside** an active attempt. Never auto-fallback
after **ambiguous** launch. Explicit pins have no cross-route fallback unless
the owner pre-authorized a named ordered list.

## Failure classes

| Class | Auto successor (default policy) |
| --- | --- |
| `pre_launch_definitive` | yes (budget 1) |
| `provider_declined_not_started` | yes (budget 1) |
| `launched_terminal_failure` | no (needs human) |
| `ambiguous_execution` | **never** — needs human |
| `quota_or_rate_limit` | no (needs human unless policy enables) |
| `policy_change` | no — owner re-decide |

## Rules

1. Route identity is frozen per attempt; failures cannot rewrite provider/model.  
2. Successors carry a **new** `routedecision` digest and causal predecessor link.  
3. Prior worktree/log/event refs remain on the prior attempt (not overwritten).  
4. Default max automatic successors = 1.  
5. Failed route excluded from successor winner when `ExcludeFailedRoute` is set.  
6. Pin ordered fallback only via `PinFallbackOrdered`.  

## Verification

```bash
go test ./internal/successor/
```

## Non-goals

Provider launch, live quota, or mutating an in-flight attempt's route pin.
