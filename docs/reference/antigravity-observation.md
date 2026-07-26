# Antigravity discovery and catalog adapter (V090-106)

Package: [`internal/antigravityobs`](../../internal/antigravityobs)  
Issue: [#1152](https://github.com/jasonhnd/loopcoder/issues/1152)

## Purpose

Model Antigravity as its **own** provider installation and account surface. Do
not assume Gemini CLI auth, models, quota, or invocation apply merely because
both relate to Google models.

## Verification

```bash
go test ./internal/antigravityobs/
```
