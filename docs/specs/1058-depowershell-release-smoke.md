---
id: 1058
title: Replace PowerShell Release Smoke with Go releasesmoke
status: accepted
date: 2026-07-20
issue: 1058
pr: null
supersedes: []
superseded_by: []
---

# Replace PowerShell Release Smoke with Go releasesmoke

One-page implementation plan for [#1058](https://github.com/jasonhnd/loopcoder/issues/1058).
Completes the unfinished PowerShell disposition in
[0884](0884-macos-arm64-only.md) Slice E for the **current** release surface.
Does not reopen platform support for Windows/Linux.

## Problem

- Product contract is **darwin/arm64 only**; mechanical backend is the Go binary.
- Release gate still runs `shell: pwsh` → `scripts/release-smoke.ps1` (and nested
  `self-bootstrap-smoke.ps1`). `scripts/install.ps1` remains in tree.
- PowerShell smokes hardcode **schema 30** while
  `storage.CurrentSchemaVersion` is **31**, so publish can fail on a false
  negative (second source of truth).
- Go unit tests already use `CurrentSchemaVersion`; scripts do not.

## Goal

| Must | Must not |
| --- | --- |
| Zero `scripts/*.ps1` on the current surface | Broad delete of historical `*_windows.go` (0884 Slice G) |
| Release/self-bootstrap smoke without `pwsh` | Rewrite install product path away from `install.sh` |
| Schema assertions bound to binary/`CurrentSchemaVersion` | “Just change 30 → 31” in PowerShell |
| CI/docs for **current** operators use bash + Go only | Claim multi-platform smoke |

## Target design

```text
internal/releasesmoke/     # Go acceptance: host, install, migrate, self-bootstrap, upgrade, evidence
scripts/release-smoke.sh   # thin: env + go test (or loopcoder smoke release)
scripts/self-bootstrap-smoke.sh
# DELETE: release-smoke.ps1, self-bootstrap-smoke.ps1, install.ps1
```

- **Host gate:** fail before side effects unless `darwin/arm64`.
- **Schema:** never hardcode current generation.
  - Fresh DB: `source == target`, `status == current`,
    `target == storage.CurrentSchemaVersion`.
  - v0.7 upgrade: `source == 9`, `target == CurrentSchemaVersion`,
    apply ends `health.ok` at that version. **Legacy 9 may stay literal.**
- **Install path:** exercise real `scripts/install.sh` (and upgrade) against the
  staged darwin/arm64 archive; mock GitHub release API in Go
  (`net/http`), not in PowerShell.
- **Evidence:** keep machine-readable evidence contracts
  (`loopcoder.release_smoke_evidence.v1`,
  `loopcoder.self_bootstrap_evidence.v1`) with schema fields filled from plan/health.
- **Policy tests:** stop string-matching `.ps1`; assert no `scripts/*.ps1`, no
  `shell: pwsh` / `pwsh scripts/` in workflows and current docs.

## Slices (doc-first code issues after this merges)

### Slice A — Go smoke parity (code)

1. Port acceptance from the two `.ps1` files into `internal/releasesmoke`
   (behavior checklist, not a line-for-line port).
2. Bind all “current schema” checks to `storage.CurrentSchemaVersion`.
3. Local proof: `go test ./internal/releasesmoke -count=1` on Apple Silicon.
4. Optional thin `scripts/*-smoke.sh` calling that package.

**Exit:** Go smoke green; schema drift bug class gone.

### Slice B — Cut over release + delete PowerShell (code)

1. `.github/workflows/release.yml` smoke job: bash driver only (no `pwsh`).
2. Delete `scripts/release-smoke.ps1`, `self-bootstrap-smoke.ps1`, `install.ps1`.
3. Rewrite `repository_policy_test.go` (and any CI checks) for the new layout.
4. Guard: test or script fails if any `scripts/*.ps1` or release `pwsh` returns.

**Exit:** tag Release uses Go/bash smoke; tree has **zero** `.ps1`.

### Slice C — Current docs + learning (docs)

1. Update **current** surfaces: `docs/reference/releasing.md`,
   `self-bootstrap.md`, README install/smoke pointers, CHANGELOG.
2. Historical v0.7/v0.8.0 go-no-go text may keep past `pwsh` evidence as history.
3. `docs/learnings.md`: root cause = second truth in PowerShell; fix = smoke in
   Go next to `CurrentSchemaVersion`.
4. Conductor hooks: product docs no longer require PowerShell; host matcher
   `PowerShell|pwsh` is optional cleanup, not a release blocker.

**Exit:** operators never instructed to install or run `pwsh` for loopcoder.

## Non-goals

- Deleting all Windows-oriented Go build tags / `replace_windows.go` (Slice G).
- Changing `CurrentSchemaVersion` itself or inventing schema 32.
- Reintroducing multi-OS release matrices.

## Acceptance (done when)

- [ ] `find scripts -name '*.ps1'` is empty.
- [ ] Release workflow has no `pwsh` / `shell: pwsh`.
- [ ] Fresh + 9→current migration assertions use `CurrentSchemaVersion` (or
      binary plan/health equal to it), not a magic current integer.
- [ ] Self-bootstrap + release smoke pass on darwin/arm64 against the staged
      single artifact `loopcoder_<ver>_darwin_arm64.tar.gz`.
- [ ] Policy/guard tests prevent `.ps1` and release-pwsh regression.
- [ ] Current releasing/self-bootstrap docs describe bash/Go only.

## Order and risk

| Order | Risk | Mitigation |
| --- | --- | --- |
| This doc → merge | Low | Contract only |
| Slice A then B | Medium: missed acceptance line | Checklist from existing `.ps1` before delete; keep draft release on failure |
| Slice C | Low | Same PR series as B or immediately after |

**Do not** ship a PowerShell-only schema hotfix as the long-term fix. If a
tag must publish before Slice B lands, prefer landing Slice A+B first; a
one-line `30→31` in `.ps1` is explicitly rejected by this plan.

## Implementer notes

- Prefer `go test ./internal/releasesmoke` over a large new CLI surface unless
  a `loopcoder smoke …` subcommand is needed for operators; thin bash is enough
  for CI.
- `source_schema_version == 9` is the only intentional numeric schema anchor
  (v0.7 fixture). All “current” endpoints read the constant or the binary.
- One concern per PR per [`PROCESS.md`](../PROCESS.md): this document first;
  then separate code issues referencing this file.
