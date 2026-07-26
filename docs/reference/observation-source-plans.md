# Ordered observation-source plans (V090-038)

Package: [`internal/obsplan`](../../internal/obsplan)  
Issue: [#1141](https://github.com/jasonhnd/loopcoder/issues/1141)

## Purpose

Provider discovery is an **ordered, bounded source plan**, not one opaque probe.
Each run explains selected source, attempted/skipped sources, diagnostics, and
capture time. Failed sources contribute diagnostics only — never facts.

## Source kinds

| Kind | Role |
| --- | --- |
| `api` | Optional network API (explicit allow) |
| `cli` | Local CLI |
| `local_status` | Local status command |
| `auth_metadata` | Structured auth metadata (no tokens) |
| `bridge_optional` | Optional bridge |
| `unavailable` | Explicit unavailable marker |

Ordering: authority desc, then safety desc. Default plans stop on first OK.

## Snapshot

Schema `loopcoder.obs.snapshot.v1`:

- `selected_source`, `attempted`, `skipped`, `diagnostics`
- `facts` — only `OutcomeOK` steps
- `digest` — byte-stable over facts/explanation/selection (route-event ready)
- Immutable store deduplicates identical digests; fact changes create a new snapshot

## Distinct failures

`timeout`, `malformed`, `unauthenticated`, `stale`, `unsupported` never normalize
to zero quota or “not installed” facts.

## Bounds and scrubbing

Per-step timeout, max output, network allow flag. Redirects denied by policy.
`ScrubEnv` strips tokens/secrets and git redirect variables.

## Verification

```bash
go test ./internal/obsplan/
```
