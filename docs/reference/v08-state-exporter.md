# Read-only v0.8 state exporter (V090-069)

Package: [`internal/v08export`](../../internal/v08export)  
Issue: [#1183](https://github.com/jasonhnd/loopcoder/issues/1183)

## Purpose

Extract supported facts from v0.8 global/repo-local schema into a **versioned
neutral export** without letting v0.9 write, repair, upgrade, or execute through
the old store.

## Supported schemas

`0.8.0`, `0.8.1`, `0.8.2`. Newer/unknown/corrupt records become `unsupported`
with warnings — no auto-migration, repair, deletion, or recreation.

## Export contents

- Normalized project identities and aliases
- Selected **terminal** work/run/delivery/report evidence (payload digest only)
- Source schema versions, digests, counts, warnings
- Manifest with bundle digest, source digests/modes, idempotent key

## Safety

| Rule | Behavior |
| --- | --- |
| Read-only | Source bytes and modes snapshotted; `AssertImmutable` |
| Credentials / tokens / secrets | Stripped; never enter bundle |
| Live lease / PID authority | Stripped |
| Nonterminal evidence | Skipped with warning |
| Alias conflicts | Surfaced; projects not silently merged |
| Export location | Must be **outside** customer repo path |

## Downstream

Manifest + digests are sufficient inputs for V090-070 (v0.9 importer).

## Verification

```bash
go test ./internal/v08export/
```
