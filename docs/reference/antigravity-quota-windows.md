# Antigravity quota-window adapter (V090-108)

Package: [`internal/antigravityquota`](../../internal/antigravityquota)  
Issue: [#1154](https://github.com/jasonhnd/loopcoder/issues/1154)

## Purpose

Normalize Antigravity usage windows independently of Gemini CLI. Typed quantities
with missing/unlimited/unknown ≠ zero.

## Verification

```bash
go test ./internal/antigravityquota/
```
