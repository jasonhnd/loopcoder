# Codex quota-window adapter (V090-041)

Package: [`internal/codexquota`](../../internal/codexquota)  
Issue: [#1145](https://github.com/jasonhnd/loopcoder/issues/1145)

## Purpose

Normalize Codex five-hour, weekly, credit, and related quota windows with honest
typed quantities so routers never treat missing/unknown/unlimited as zero.

## Quantity classes

| Class | Meaning |
| --- | --- |
| `finite` | numeric value |
| `zero` | explicit numeric zero |
| `missing` | field absent — **not** zero |
| `unlimited` | provider says unlimited |
| `unknown` | unparsed / unknown |

Missing limits are **never** fabricated from used+remaining.

## Reset times

RFC3339 / RFC3339Nano only (timezone-safe). Ambiguous small integers rejected.

## Verification

```bash
go test ./internal/codexquota/
```
