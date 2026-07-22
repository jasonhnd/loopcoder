# Exact-artifact install, migration, and cleanup smoke (V090-082)

Package: [`internal/installsmoke`](../../internal/installsmoke)  
Issue: [#1197](https://github.com/jasonhnd/loopcoder/issues/1197)

## Purpose

Exercise the **exact** V090-081 draft archive digest in clean and v0.8-upgrade
consumer fixtures. Never rebuild during smoke.

## Steps

checksum/signature → install → version/help → doctor → global layout → direct
fixture → status/events/cancel/resume → uninstall guidance → v0.8 export/import
idempotent (source hash stable) → no repo-local runtime → private redaction →
process cleanup → DB integrity after interrupt.

## Verification

```bash
go test ./internal/installsmoke/
```
