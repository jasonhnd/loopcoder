# Release-candidate consumer canary and GO/NO-GO record (V090-083)

Package: [`internal/rcgonogo`](../../internal/rcgonogo)  
Issue: [#1199](https://github.com/jasonhnd/loopcoder/issues/1199)

## Purpose

Final consumer-oriented evidence set and **explicit owner GO/NO-GO** for
publishing the single v0.9.0 darwin/arm64 release. Product usability and exact
artifact truth — not issue count — decide.

## GO requires

- Exact SHA dual-green: integration-verify + integration-canary
- Release SLO scorecard GO (V090-102)
- Install smoke pass on **same** archive digest (V090-082); no rebuild
- All 9 consumer canaries pass (public/private, multi-project, routes, workflow,
  host visibility, cross-Mac handoff, cancel/recovery, cleanup)
- No open P0/P1
- Security + SBOM present; docs/capability honest; migration OK
- Operator approval
- **Publish** additionally requires protected environment approval

## Record fields

SHA, archive digest, scorecard/smoke flags, canary counts, evidence links,
known limitations, deferred issues, rollback limitations, publication steps.

## Verification

```bash
go test ./internal/rcgonogo/
```
