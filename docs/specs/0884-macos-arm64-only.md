---
id: 884
title: v0.8.0 macOS Apple Silicon Support Contract
status: accepted
date: 2026-07-14
issue: 884
pr: null
supersedes: []
superseded_by: []
---

# v0.8.0 macOS Apple Silicon Support Contract

This documentation-only spec freezes the v0.8.0 platform contract before any
implementation changes: LoopCoder v0.8.0 supports native macOS on Apple Silicon
only, expressed as Go tuple `darwin/arm64`.

This is a deliberate breaking support-policy change for roadmap
[#714](https://github.com/jasonhnd/loopcoder/issues/714) and release-readiness
coordination [#744](https://github.com/jasonhnd/loopcoder/issues/744). The
implementation work is tracked by
[#885](https://github.com/jasonhnd/loopcoder/issues/885). Per
[`../PROCESS.md`](../PROCESS.md), this spec must merge before any code,
workflow, installer, release, or current-documentation implementation issue is
opened.

## Goals

- Define `darwin/arm64` as the only v0.8.0 runtime, CI, smoke, release, and
  product-support tuple.
- Explicitly classify Windows, Linux, Ubuntu, WSL, containers used as the
  LoopCoder runtime, Intel macOS, and all other tuples as unsupported for
  v0.8.0.
- Define one fail-fast runtime, installer, and upgrade error contract for
  unsupported hosts, including machine-readable diagnostics.
- Define the safe migration from the existing eight required CI contexts to
  the four durable contexts `verify`, `test`, `race`, and `security` without
  temporarily making every pull request unmergeable.
- Define the v0.8.0 release artifact set, install behavior, upgrade behavior,
  release smoke behavior, current-documentation obligations, and v0.7 historical
  compatibility policy.
- Inventory existing platform-specific source and documentation surfaces and
  assign bounded keep/delete dispositions without turning this spec PR into a
  source deletion sweep.
- Decompose follow-up implementation under #885 into independently testable
  issues.

## Non-Goals

- No Go code, workflow, installer, release automation, issue body, README,
  changelog, or living-reference documentation changes in this issue.
- No deletion of historical OS-specific source in this PR.
- No provider/model routing, quota, progress, or agent-federation behavior
  change.
- No rewrite of frozen historical specs, v0.7.0 release notes, v0.7.0
  go/no-go evidence, or already-published v0.7.0 artifacts.
- No publication of v0.8.0.

## Normative Support Table

For v0.8.0, "macOS" means native Apple Silicon only. Intel macOS and macOS
under an `amd64` or Rosetta build are not supported.

| Tuple or host class | v0.8.0 support status | Required treatment |
| --- | --- | --- |
| `darwin/arm64` on Apple Silicon macOS | Supported | The only runtime, CI, release, smoke, install, upgrade, documentation, compatibility, and platform-specific bug-fix target. |
| `darwin/amd64` on Intel macOS | Unsupported | No artifact, CI job, smoke, compatibility criterion, or platform-specific bug-fix commitment. Must fail before side effects. |
| `darwin/amd64` under Rosetta on Apple Silicon | Unsupported | Not a supported macOS runtime. Must fail before side effects even if the physical host is Apple Silicon. |
| `windows/amd64`, `windows/arm64`, and all Windows variants | Unsupported | No v0.8.0 artifact, installer, smoke, CI, compatibility acceptance, or platform-specific maintenance. |
| `linux/amd64`, `linux/arm64`, Ubuntu, other Linux distributions | Unsupported | No v0.8.0 artifact, smoke, CI, compatibility acceptance, or platform-specific maintenance. |
| WSL | Unsupported | Treated as Linux, not as a supported Windows or macOS path. |
| Containers used as the LoopCoder runtime | Unsupported | Containers may exist as external tooling fixtures only; a containerized LoopCoder runtime is not a supported v0.8.0 host. |
| Any other `GOOS/GOARCH` tuple | Unsupported | No product, CI, release, smoke, compatibility, or maintenance target. |

Successful source compilation on an unsupported tuple is not supported behavior.
Current documentation must not describe such compilation as a v0.8.0 support
promise.

## Historical Compatibility

v0.7.0 remains the final legacy multi-platform release. Its published Windows,
macOS, and Linux artifacts, release notes, changelog entries, go/no-go evidence,
and frozen specs remain truthful historical records and must stay available.

v0.7.x receives no routine feature, compatibility, CI, or platform maintenance.
A critical v0.7.x maintenance release is allowed only for a maintainer-approved
security or data-loss issue that explicitly targets the v0.7 line, documents
the narrowed evidence required for that release, and does not change the
v0.8.0 `darwin/arm64` contract. Absence of such an approved issue means users
who need Windows, Linux, Ubuntu, WSL, containers, or Intel macOS should remain
on v0.7.0 with no implied future patch stream, or contribute to a later
explicitly approved platform roadmap.

Frozen historical specs such as
[`0089-go-migration.md`](0089-go-migration.md),
[`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md),
[`0390-process-watchdog.md`](0390-process-watchdog.md), and the v0.7.0
go/no-go reference must not be terminology-swept merely because v0.8.0 changes
the support policy. Current v0.8 docs may point to this spec as the superseding
support contract for v0.8.0 without rewriting those historical records.

## Fail-Fast Error Contract

All v0.8.0 entry points that can observe the host platform must use the same
stable error identity on unsupported tuples:

| Field | Required value |
| --- | --- |
| Stable error code | `ErrUnsupportedPlatform` |
| Human first line | `LoopCoder v0.8.0 supports macOS Apple Silicon only (darwin/arm64).` |
| CLI/process exit code | `78` |
| Supported tuple | `darwin/arm64` |
| Actual tuple | The observed or injected `GOOS/GOARCH` tuple. |
| Side-effect flag | `side_effects_performed: false` |

Machine-readable diagnostics must use this minimum JSON shape wherever the
entry point already has a JSON or diagnostic mode:

```json
{
  "schema_version": "loopcoder.diagnostic.v1",
  "error_code": "ErrUnsupportedPlatform",
  "message": "LoopCoder v0.8.0 supports macOS Apple Silicon only (darwin/arm64).",
  "supported": [{"goos": "darwin", "goarch": "arm64"}],
  "actual": {"goos": "linux", "goarch": "amd64"},
  "phase": "startup",
  "side_effects_performed": false
}
```

The `phase` value may be `startup`, `install`, `upgrade`, `smoke`, `doctor`, or
another stable command phase, but the error code and side-effect semantics are
shared. Human output may add remediation text after the first line, but must
not imply v0.8.0 support for unsupported tuples.

Unsupported-host failure must occur before:

- network access or release lookup;
- checksum, signature, or provenance download;
- credential, provider profile, or provider CLI access;
- provider launch or model invocation;
- state open, schema migration, backup, cleanup, or doctor repair;
- repository, worktree, hook, settings, or git mutation;
- binary replacement, installer extraction, PATH mutation, or filesystem
  replacement.

## Runtime And Command Behavior

The main `loopcoder` binary must perform the platform gate before command
dispatch for every command that can produce side effects. Read-only help and
version output may remain available on unsupported hosts only if they do not
open runtime state, contact the network, inspect credentials, or mutate the
repository. Any command that cannot prove side-effect-free behavior must return
`ErrUnsupportedPlatform`.

`doctor --format json` and comparable diagnostics on unsupported hosts must
emit the machine-readable diagnostic above and exit `78`; they must not probe
storage permissions, provider compatibility, releases, or credentials first.

Self-bootstrap and migration paths must gate before opening v0.7 state,
creating backups, migrating schemas, installing skills, writing hooks, or
registering projects. v0.7-to-v0.8 upgrade proof exists only on `darwin/arm64`.

Release smoke must verify that the artifact itself reports `darwin/arm64` and
must include an injected unsupported-tuple test proving `ErrUnsupportedPlatform`
without requiring Windows or Linux runners.

## CI And Branch Protection

Every required GitHub Actions job for v0.8.0 must run on the fixed
GitHub-hosted Apple Silicon macOS image label `macos-15`, never the floating
`macos-latest` label. The authoritative runner references for this requirement
are the GitHub-hosted runner reference
<https://docs.github.com/en/actions/reference/runners/github-hosted-runners>
and the runner-images repository <https://github.com/actions/runner-images>.
A runner label alone is insufficient evidence: each required job must assert
the actual Go runtime tuple with `go env GOOS GOARCH` and fail unless it
resolves to `darwin arm64` before substantive work begins.

Ubuntu and Windows jobs must be removed from v0.8.0 CI and release workflows.
They must not remain as optional, advisory, or non-blocking compatibility jobs.
Unsupported-platform behavior is covered by injected tuple tests on the
supported Apple Silicon macOS runner, not by native unsupported-host CI.

The target durable required check layout is exactly four contexts:

| Required context | Runner requirement | Tuple proof |
| --- | --- | --- |
| `verify` | `runs-on: macos-15` | Assert `go env GOOS GOARCH` resolves to `darwin arm64` before YAML, config, release-policy, or repository-policy checks. |
| `test` | `runs-on: macos-15` | Assert `go env GOOS GOARCH` resolves to `darwin arm64` before build, vet, Markdown/link validation, or ordinary Go tests. |
| `race` | `runs-on: macos-15` | Assert `go env GOOS GOARCH` resolves to `darwin arm64` before race-enabled Go tests. This job runs in parallel with `test`. |
| `security` | `runs-on: macos-15` | Assert `go env GOOS GOARCH` resolves to `darwin arm64` before staticcheck, govulncheck, gosec, loopcoder audit, or other security checks. |

The current branch-protection state on `main` requires eight contexts:
`verify`, `go`, `staticcheck`, `govulncheck`, `audit`,
`native (ubuntu-latest)`, `native (macos-latest)`, and
`native (windows-latest)`. After the migration is complete, the implemented
`.delivery.yml` allow-list must become exactly `[verify, test, race, security]`.
This consolidation reduces context count, not coverage: tests, race detection,
static analysis, vulnerability scanning, gosec, audit, policy, and release
checks remain required, while compatible work is grouped and race detection is
parallelized.

The safe no-missing-context rollout order for protected `main` is:

1. Add and dual-emit the four new contexts `verify`, `test`, `race`, and
   `security` while the old eight contexts still exist.
2. Validate the new four contexts on a fresh pull request.
3. Update the `main` branch-protection required-context list through the GitHub
   API to require only `verify`, `test`, `race`, and `security`.
4. Read back and verify the effective branch-protection configuration through
   the GitHub API.
5. Only then remove the old eight job contexts, native OS matrix, and duplicate
   standalone jobs from workflows.
6. Validate another fresh pull request and prove that no required context is
   permanently pending or absent.

Implementation must include a deterministic repository-policy test that rejects
future required workflow jobs on Ubuntu, Windows, Linux, Intel macOS, or generic
unpinned macOS runners, including `macos-latest`.

## Release And Installation

v0.8.0 publishes exactly one binary archive:

```text
loopcoder_<version>_darwin_arm64.tar.gz
```

The release may also publish the existing checksum, signature, and provenance
material required to verify that archive, including `SHA256SUMS` and the
configured signature/provenance files. No Windows, Linux, Ubuntu, or
`darwin/amd64` binary archive may be built, staged, smoked, advertised, or
published for v0.8.0.

The shell installer must reject non-Darwin or non-arm64 hosts before download,
checksum retrieval, signature retrieval, extraction, PATH mutation, or binary
replacement. The Windows PowerShell installer must be removed from the
advertised/current v0.8 installation surface. Historical v0.7.0 installer and
artifact references may remain only as historical release records.

`loopcoder upgrade` must select only
`loopcoder_<version>_darwin_arm64.tar.gz` and reject unsupported hosts before
GitHub release lookup, network access, checksum/signature/provenance download,
filesystem replacement, skill refresh, state open, migration, or cleanup.
Windows deferred replacement is not part of v0.8.0 behavior.

Release staging and smoke must build once, sign/checksum once, smoke the exact
Darwin arm64 artifact that will be published, preserve failed drafts for
diagnosis, and keep the protected publication environment approval. The final
publication job must not rebuild or upload a different archive.

## Documentation And Release Communication

The v0.8.0 implementation must update current release-facing and support-facing
documentation to call this a deliberate breaking support change:

| Surface | Required v0.8.0 statement |
| --- | --- |
| `README.md` | Installation, upgrade, platform support, and feature summaries identify `darwin/arm64` as the only supported v0.8.0 target. |
| `docs/reference/stability-policy.md` | Support policy names unsupported targets and the v0.7.0 legacy path. |
| `docs/reference/architecture.md` | Architecture description no longer claims a cross-platform v0.8 runtime. |
| `docs/reference/usage.md` | Current install/upgrade instructions use Darwin arm64 only and remove Windows/Linux success claims. |
| `docs/reference/releasing.md` | Release flow, branch protection, and smoke instructions use the single Darwin arm64 artifact and Apple Silicon CI. |
| `CHANGELOG.md` | v0.8.0 entry lists this as a breaking support change. |
| `.github/release-notes/v0.8.0.md` or release body source | Release notes explain the practical path for unsupported-platform users. |
| v0.8.0 go/no-go evidence | Evidence records Apple Silicon macOS install, upgrade, migration, self-bootstrap, and release smoke only. |

The practical path for users who need Windows, Linux, Ubuntu, WSL, containers,
or Intel macOS is to remain on v0.7.0 or contribute to a later explicitly
approved platform roadmap. Current docs must not imply v0.8.0 support or
platform-specific bug-fix commitments for those users.

## Existing Platform-Specific Source Inventory

This spec classifies current platform-specific surfaces by cohesive ownership.
The implementation issues must use this table to avoid broad historical-source
deletion bundled with CI or release policy changes.

| Surface | Current evidence | Disposition |
| --- | --- | --- |
| CI native matrix | [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml) runs `native (ubuntu-latest)`, `native (macos-latest)`, and `native (windows-latest)`. | Delete/rewrite in Slice D after branch-protection migration is safe. |
| Release build and smoke matrix | [`.github/workflows/release.yml`](../../.github/workflows/release.yml) builds six archives and smokes Ubuntu, macOS, and Windows. | Delete/rewrite in Slice E. |
| Shell installer | [`scripts/install.sh`](../../scripts/install.sh) detects Linux/Darwin and amd64/arm64. | Retain as the single v0.8 installer, narrowed to Darwin arm64 in Slice B. |
| Windows installer | [`scripts/install.ps1`](../../scripts/install.ps1) installs Windows archives. | Remove from advertised/current v0.8 surface in Slice B; deletion from source is allowed if no v0.7 migration test depends on it. |
| PowerShell release and self-bootstrap smoke | [`scripts/release-smoke.ps1`](../../scripts/release-smoke.ps1) and [`scripts/self-bootstrap-smoke.ps1`](../../scripts/self-bootstrap-smoke.ps1). | Replace or narrow to Apple Silicon macOS smoke in Slice E; retain temporarily only when needed to verify historical v0.7 upgrade evidence. |
| Release staging helper test | [`scripts/stage-draft-release_test.sh`](../../scripts/stage-draft-release_test.sh) contains historical Linux archive fixture names. | Update in Slice E only as needed for the single artifact contract. Historical fixture names are not support promises. |
| Upgrade asset selection and replacement | [`internal/upgrade/upgrade.go`](../../internal/upgrade/upgrade.go), [`internal/upgrade/replace_windows.go`](../../internal/upgrade/replace_windows.go), and [`internal/upgrade/replace_other.go`](../../internal/upgrade/replace_other.go). | Restrict asset selection and unsupported-host gating in Slice C. Remove Windows deferred replacement only if Slice C proves no v0.7 migration compatibility need; otherwise defer to Slice G. |
| Process supervision platform files | [`internal/supervisedexec/killgroup_windows.go`](../../internal/supervisedexec/killgroup_windows.go), [`killgroup_unix.go`](../../internal/supervisedexec/killgroup_unix.go), [`pdeathsig_linux.go`](../../internal/supervisedexec/pdeathsig_linux.go), [`pdeathsig_other.go`](../../internal/supervisedexec/pdeathsig_other.go), [`process_activity_linux.go`](../../internal/supervisedexec/process_activity_linux.go), and [`process_activity_ps.go`](../../internal/supervisedexec/process_activity_ps.go). | Retain temporarily, untested and unsupported outside Darwin arm64, unless a bounded Slice G issue proves deletion is safe. Darwin-compatible Unix files may remain. |
| Process liveness platform files | [`internal/process/process_windows.go`](../../internal/process/process_windows.go), [`process_unix.go`](../../internal/process/process_unix.go), [`process_other.go`](../../internal/process/process_other.go), and related tests. | Retain Darwin-compatible code; defer Windows/other cleanup to Slice G because it is historical process behavior, not required for CI/release policy. |
| Storage permission platform files | [`internal/storage/permissions_windows.go`](../../internal/storage/permissions_windows.go), [`permissions_unix.go`](../../internal/storage/permissions_unix.go), and related tests. | Retain Unix/Darwin path; Windows code is unsupported and may be deleted only in a bounded Slice G issue after migration risk is reviewed. |
| Lockfile and path platform files | [`internal/lockfile/path_windows.go`](../../internal/lockfile/path_windows.go), [`path_other.go`](../../internal/lockfile/path_other.go), [`internal/pathid/pathid_windows_test.go`](../../internal/pathid/pathid_windows_test.go). | Retain Darwin-compatible path behavior; defer Windows cleanup to Slice G unless Slice A needs injected tests. |
| Audit native permission platform tests | [`internal/audit/native_permission_windows_test.go`](../../internal/audit/native_permission_windows_test.go) and [`native_permission_unix_test.go`](../../internal/audit/native_permission_unix_test.go). | Keep or convert to injected unsupported-platform fixtures in Slice A/D. Native Windows CI proof is forbidden. |
| Runtime `runtime.GOOS` branches in tests and docs | Numerous tests and docs reference Windows/Linux paths or path redaction. | Retain as credential-free data fixtures when they test serialization or redaction, not native support. Update misleading current-doc claims in Slice F. |
| Current user docs and release history | [`README.md`](../../README.md), [`docs/reference/usage.md`](../reference/usage.md), [`docs/reference/releasing.md`](../reference/releasing.md), [`CHANGELOG.md`](../../CHANGELOG.md), and `.github/release-notes/`. | Update current v0.8-facing surfaces in Slice F. Preserve v0.7.0 historical records. |

Retained unsupported code is explicitly untested and unsupported for v0.8.0.
Its presence in source is not a compatibility promise. Broad deletion of
historical platform code must not be combined with CI/release policy
implementation unless this table proves that deletion is necessary for the
specific slice.

## Roadmap And Issue Policy

All not-yet-started v0.8.0 implementation issues inherit the `darwin/arm64`
constraint. Existing acceptance criteria that require Windows, Linux,
Ubuntu, WSL, Intel macOS, cross-platform proof, or three-platform release proof
must be replaced after this spec is accepted.

Known issue updates required after acceptance:

| Issue | Required update |
| --- | --- |
| [#714](https://github.com/jasonhnd/loopcoder/issues/714) | Keep the platform support gate and ensure release-level acceptance names Apple Silicon macOS only. |
| [#744](https://github.com/jasonhnd/loopcoder/issues/744) | Keep children #867-#869 aligned to Darwin arm64 migration, docs, smoke, and publication evidence. |
| [#867](https://github.com/jasonhnd/loopcoder/issues/867) | Require Apple Silicon macOS migration, backup, permission, interruption, and rollback evidence only. |
| [#868](https://github.com/jasonhnd/loopcoder/issues/868) | Require Darwin arm64 fresh-install, v0.7 upgrade, self-bootstrap, and docs proof; preserve v0.7 history. |
| [#869](https://github.com/jasonhnd/loopcoder/issues/869) | Require exactly the Darwin arm64 artifact and no Windows/Linux/Intel macOS smoke or artifacts. |
| [#862](https://github.com/jasonhnd/loopcoder/issues/862) | Replace Windows junction acceptance with credential-free injected/path-fixture coverage; native proof is Darwin arm64 only. |
| [#823](https://github.com/jasonhnd/loopcoder/issues/823) | Keep host/provider matrix fixtures provider-neutral, but native platform proof is Darwin arm64 only. |
| [#825](https://github.com/jasonhnd/loopcoder/issues/825) and child [#838](https://github.com/jasonhnd/loopcoder/issues/838) | Replace Windows/macOS/Linux/WSL Grok conformance wording with Darwin arm64 native proof plus unsupported-host fixture behavior. |

Future issue templates, release documentation, and acceptance criteria must not
reintroduce cross-platform native proof by default. If a later release wants to
restore another platform, it needs a new doc-first support-policy spec and a
separate implementation plan.

## #885 Implementation Slices

The follow-up implementation under #885 must be decomposed into small,
independently testable issues. The parent #885 is not dispatchable.

| Slice | Boundary | Dependencies | Required acceptance evidence |
| --- | --- | --- | --- |
| A | Runtime platform contract and shared `ErrUnsupportedPlatform` diagnostic. | This accepted spec. | Injected tuple tests for `darwin/arm64`, `darwin/amd64`, Linux, and Windows; side-effect-before-gate tests; JSON diagnostic fixture. |
| B | Installer surface. | Slice A contract available or duplicated as a shell-level equivalent. | Shell installer rejects unsupported hosts before download; Darwin arm64 path selects the single archive; Windows installer removed from current advertised surface. |
| C | `upgrade` asset selection and migration/self-bootstrap gate. | Slice A. | Unsupported tuple rejects before release lookup or state open; Darwin arm64 selects only the Darwin arm64 archive; v0.7-to-v0.8 proof remains Apple Silicon-only. |
| D | CI and repository policy. | Slice A for injected tests. | Exactly `verify`, `test`, `race`, and `security` run on `macos-15` and assert `go env GOOS GOARCH` resolves to `darwin arm64`; Ubuntu/Windows jobs removed; repository-policy test rejects unsupported runners; branch-protection migration evidence recorded. |
| E | Release build, staging, smoke, and publication gates. | Slices B, C, and D. | Exactly one archive plus integrity/provenance files; staged smoke tests the exact artifact; no unsupported-platform artifact or smoke job remains. |
| F | Living docs and v0.8 issue acceptance. | This accepted spec; can run partly in parallel with A-C if it does not claim implemented behavior before it exists. | README, support/stability, architecture, usage, releasing, changelog, release notes, go/no-go template/evidence, and platform-sensitive issue criteria align with this spec. |
| G | Bounded legacy-source cleanup, optional. | Inventory evidence from this spec and completed A-F slice needs. | One cohesive unsupported subsystem per issue; Darwin arm64 build/test proof; no cleanup justified only by reducing grep matches. |

## Acceptance Mapping

This spec satisfies #884 when:

- frontmatter has `id: 884`, `issue: 884`, `status: draft` before review, and
  the conductor changes only `status: accepted` at merge;
- the normative support table names `darwin/arm64` as the sole supported tuple
  and classifies every other tuple as unsupported;
- runtime, installer, upgrade, CI, branch-protection migration, release
  artifact, smoke, documentation, and historical v0.7 behavior are all
  specified;
- unsupported-host failures are defined before network, credentials, provider
  launch, state migration, repository mutation, installer download, or
  filesystem replacement;
- platform-specific source is inventoried and assigned keep/delete/defer
  dispositions without changing source in this PR;
- follow-up implementation is decomposed into independently testable issues
  that normally fit one worker run;
- no code, workflow, release, installer, or living-doc implementation changes
  are included in this spec PR.
