# Claude Code discovery and catalog observation (V090-042)

Package: [`internal/claudeobs`](../../internal/claudeobs)  
Issue: [#1146](https://github.com/jasonhnd/loopcoder/issues/1146)

## Purpose

Consolidate Claude Code observation (install, version, auth, model catalog,
aliases) behind `providerdesc` + `obsplan`. Same guarantees as Codex observation:
no launch, no credentials, no silent default models.

## Verification

```bash
go test ./internal/claudeobs/
```
