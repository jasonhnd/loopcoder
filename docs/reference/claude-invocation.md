# Claude Code invocation consolidation (V090-104)

Package: [`internal/claudeexec`](../../internal/claudeexec)  
Issue: [#1147](https://github.com/jasonhnd/loopcoder/issues/1147)

## Purpose

Translate one immutable `providerexec.Request` into one bounded Claude Code
command plan and normalized outcome. Discovery stays in `claudeobs`.

## Verification

```bash
go test ./internal/claudeexec/
```
