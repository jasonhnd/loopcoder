# Codex discovery and catalog observation (V090-040)

Package: [`internal/codexobs`](../../internal/codexobs)  
Issue: [#1143](https://github.com/jasonhnd/loopcoder/issues/1143)

## Purpose

Consolidate Codex **observation** (install, version, auth status, model catalog,
aliases) behind `providerdesc` + `obsplan`. Invocation is **not** here (V090-103);
quota is V090-041.

## Guarantees

- Bounded fixture/local probes only; `LaunchAttempted` and `RouteChosen` always false  
- Unknown models/`default`/`auto` never become silent eligible defaults  
- Alias normalization is reversible (`alias_of:<canonical>`)  
- Timeout/malformed/stale preserve last known snapshot fields  
- No credentials in account profile or facts  

## Descriptor

Adapter ID `codex`; claims discover, auth_status, catalog, diagnose; marks invoke
and quota unsupported in this package.

## Verification

```bash
go test ./internal/codexobs/
```
