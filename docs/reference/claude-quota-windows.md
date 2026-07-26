# Claude Code quota-window adapter (V090-043)

Package: [`internal/claudequota`](../../internal/claudequota)  
Issue: [#1148](https://github.com/jasonhnd/loopcoder/issues/1148)

## Purpose

Normalize Claude Code usage windows (five-hour, weekly, credit, and related)
with the same honest typed quantities as Codex. Missing, unlimited, unknown,
stale, malformed, and unavailable stay distinct from numeric zero. Never
infers weekly remaining from five-hour data or local token counters.

## Quantity classes

| Class | Meaning |
| --- | --- |
| `finite` | numeric value |
| `zero` | explicit numeric zero |
| `missing` | field absent — **not** zero |
| `unlimited` | provider says unlimited |
| `unknown` | unparsed / unknown |

Missing limits are never fabricated from used+remaining. Partial window sets
remain partial (not auto-healthy / auto-ineligible).

## Verification

```bash
go test ./internal/claudequota/
```
