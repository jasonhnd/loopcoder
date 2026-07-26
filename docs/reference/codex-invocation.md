# Codex invocation consolidation (V090-103)

Package: [`internal/codexexec`](../../internal/codexexec)  
Issue: [#1144](https://github.com/jasonhnd/loopcoder/issues/1144)

## Purpose

Translate one immutable `providerexec.Request` into one bounded Codex command
plan and normalized outcome. Discovery/catalog stay in `codexobs`; quota and
routing stay outside.

## Rules

- Frozen observed capabilities only (no live catalog re-read on retry)
- Alias → canonical model resolution; unknown model fails closed
- Requested/actual mismatch → `route_mismatch`
- Typed outcomes: auth, rate_limit, timeout, cancel, malformed, flood, nonzero, escape
- `ScrubEnv` strips secrets and CODEX_MODEL/PERMISSION/EFFORT overrides
- No credential persistence, route choice, or second process supervisor

## Verification

```bash
go test ./internal/codexexec/
```
