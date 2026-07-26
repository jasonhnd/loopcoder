# Antigravity invocation consolidation (V090-107)

Package: [`internal/antigravityexec`](../../internal/antigravityexec)  
Issue: [#1153](https://github.com/jasonhnd/loopcoder/issues/1153)

## Purpose

One exact, bounded Antigravity execution adapter. Does not silently use Gemini
CLI surfaces. Discovery stays in `antigravityobs`.

## Verification

```bash
go test ./internal/antigravityexec/
```
