# Task risk classes and Luna / Tera / Soul capability mapping (V090-050)

Package: [`internal/capclass`](../../internal/capclass)  
Issue: [#1160](https://github.com/jasonhnd/loopcoder/issues/1160)

## Purpose

Define **provider-neutral** capability classes and a **deterministic** task-risk
classifier so route policy does not hard-code company marketing model names.
Marketing names change; Luna / Tera / Soul stay stable.

| Class | Meaning |
| --- | --- |
| `luna` | Narrow routine work (docs polish, tiny pure fixes) |
| `tera` | Standard bounded implementation |
| `soul` | High-risk architecture, security, migration, complex reasoning |
| `needs_human` | Automatic routing must stop; owner decision required |

Classes are **not** model IDs. Model→class mapping is a separate, data-only table
(`DefaultModelMap` / `ModelMap`). Adding a newly observed model updates only that
table and its tests — never the scheduler or route engine.

## Risk inputs

Every classification lists all of these inputs (known / unknown / absent):

| Input | Role |
| --- | --- |
| `change_type` | docs, code, config, migration, architecture, release |
| `ownership_affected` | multi-owner or exclusive boundary impact |
| `migration` | storage / schema migration |
| `security` | security-sensitive change |
| `concurrency` | concurrency-sensitive change |
| `external_side_effects` | network, GitHub, publish, external writes |
| `test_breadth` | none, unit, integration, system |
| `reversibility` | easy, hard, irreversible |
| `ambiguity` | intent not clear enough for automatic routing |

## Policy rules (version `capability-class-v1`)

- Classification is **pure** and **explainable**: every reason has a stable code.
- Floors are combined with a total order:  
  `luna < tera < soul < needs_human`.
- **Unknown evidence never silently chooses a weaker/cheaper class.** High-impact
  unknown fields raise at least `soul` or `needs_human` (ambiguity).
- Owner overrides require **actor + reason**, are **append-only**, and **cannot
  mutate an active attempt route** (`OverrideStore.MarkActive`).

## Explicit model pin

An explicit eligible model pin remains authoritative downstream (V090-051+). This
package only answers: *what class does the task require?* and *does a model class
meet that requirement?* (`ModelMeets`).

## Verification

```bash
go test ./internal/capclass/
```

Golden fixtures cover docs→luna, code→tera, security/migration→soul,
ambiguity→needs_human, unknown security→soul, override raise/lower, and
immutable active-attempt override rejection.

## Non-goals

- Route winner selection, quota scoring, provider probes, or model calls
- Brand-specific heuristics inside the classifier
- Automatic task decomposition
