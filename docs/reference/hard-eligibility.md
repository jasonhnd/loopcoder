# Hard eligibility and immutable-pin precedence (V090-051)

Package: [`internal/eligibility`](../../internal/eligibility)  
Issue: [#1161](https://github.com/jasonhnd/loopcoder/issues/1161)

## Purpose

Before any soft quota scoring (V090-052), LoopCoder freezes a **hard eligibility**
decision from a captured snapshot:

1. **Immutable explicit pin** — if present, that route wins unchanged when
   eligible; if ineligible, **fail closed** with reasons and **no automatic
   fallback**.
2. Policy allow/deny  
3. Installation / auth  
4. Model present / capability class / effort / permission  
5. Task required class (`capclass` Luna/Tera/Soul/needs_human)  
6. Health / cooldown  
7. Resource fit and machine capacity  

**Quota remaining never makes an incompatible route eligible.**

## Snapshot purity

`Evaluate(Snapshot)` is pure: same snapshot + policy + task class → identical
eligible set, exclusion reasons, and `Digest`. Evidence cells carry
`evidence_id` and `freshness` (`fresh|stale|expired|unknown|missing`). Unknown
or stale hard prerequisites are ineligible — never assumed true.

## Modes

| Mode | Trigger | Outcome |
| --- | --- | --- |
| `explicit_pin` | `Snapshot.ExplicitPin` set | Pin selected or `fail_closed` |
| `automatic` | no pin | All hard-eligible candidates (stable order) |

## Verification

```bash
go test ./internal/eligibility/
```

Fixture matrix covers official providers (codex/claude/gemini/antigravity/grok),
unknown/stale evidence, policy deny, cooldown, machine capacity, pin
eligible/ineligible, and quota non-compensation.

## Non-goals

Soft score, burn urgency, reserve policy, launch, or network probes.
