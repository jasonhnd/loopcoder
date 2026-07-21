# Gemini CLI quota-window adapter (V090-045)

Package: [`internal/geminiquota`](../../internal/geminiquota)  
Issue: [#1151](https://github.com/jasonhnd/loopcoder/issues/1151)

## Purpose

Normalize Gemini CLI usage windows with typed quantities (parity with
codexquota/claudequota). Missing/unlimited/unknown ≠ zero.

## Verification

```bash
go test ./internal/geminiquota/
```
