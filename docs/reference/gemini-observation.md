# Gemini CLI discovery and catalog observation (V090-044)

Package: [`internal/geminiobs`](../../internal/geminiobs)  
Issue: [#1149](https://github.com/jasonhnd/loopcoder/issues/1149)

## Purpose

Make Gemini CLI an explicit provider surface with honest installation, account,
auth, model, effort, context, and permission observations. Antigravity is a
separate adapter (V090-106) — not conflated by brand.

Bounded local probes only; no Gemini invocation, no Antigravity credentials,
no silent default models.

## Verification

```bash
go test ./internal/geminiobs/
```
