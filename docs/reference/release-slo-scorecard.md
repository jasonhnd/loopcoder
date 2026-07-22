# Release SLO scorecard and GO/NO-GO evidence compiler (V090-102)

Package: [`internal/releaseslo`](../../internal/releaseslo)  
Issue: [#1198](https://github.com/jasonhnd/loopcoder/issues/1198)

## Purpose

Compile accepted canary/artifact evidence into one deterministic scorecard so
release is decided by product behavior, visibility, safety, and cleanup — not
issue count or unrelated green checks.

## Verdicts

`pass` | `fail` | `not_run` | `stale` | `unsupported` | `waiver_requested` |
`waiver_approved`

**Missing metrics never default to pass.**

## Metrics

run-id/start-report latency, report interval, rendered-ack latency, status
freshness, stop/join, process leaks, repo-local state, route substitution,
delivery replay, resources, redaction, migration, artifact.

## Waivers

Require owner, rationale, scope, expiry (future), risk, and `Approved=true`.

## Verification

```bash
go test ./internal/releaseslo/
```
