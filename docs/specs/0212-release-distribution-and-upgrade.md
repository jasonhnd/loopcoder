---
id: 212
title: Release, Distribution, Isolation, and Upgrade
status: draft
date: 2026-06-28
issue: 212
pr: null
supersedes: []
superseded_by: []
---

# Release, Distribution, Isolation, and Upgrade

This is a design-only spec for the loopcoder 0.3.x release, distribution,
installation, upgrade, and multi-project isolation model. This PR must add only
this design record: no Go code, no `.delivery.yml` change, no command behavior
change, and no runtime dependency. Implementation belongs in separate code
issues after this spec is reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

## Goal

loopcoder must be installable, upgradable, and usable by many users on many
machines through standard tooling. No consumer should hand-copy a binary,
playbook, skill directory, or machine-local checkout to another machine.

The release model must also support self-hosting: the maintainer develops
loopcoder using loopcoder while keeping development builds isolated from the
world-facing stable release line that all consumer projects use.

## Decisions

1. **GitHub Releases are the source of truth.** A version is released by pushing
   a tag. CI builds cross-platform release assets for Windows, macOS, and Linux
   on `amd64` and `arm64`, publishes `SHA256SUMS`, and publishes a signing
   artifact or signature path such as `cosign` or `minisign`. Consumers install
   from GitHub Releases only; machine-to-machine copying is not a release or
   upgrade mechanism.
2. **There are two tracks.** The world-facing stable release line is the only
   line consumed by users and by consumer projects. The maintainer's local
   source build is the only special case and is used only for loopcoder
   development. The maintainer's own non-loopcoder consumer projects install
   the published stable release like everyone else.
3. **Install must not require Go.** The primary install path is a script:
   `curl | sh` on Unix-like systems and `irm | iex` on Windows. The script
   selects the correct GitHub Release asset, verifies it, installs it under
   `~/.loopcoder/bin`, and ensures that binary is on `PATH`. The installed
   binary supports `--version`. `go install ...@vX` remains a supported
   developer-friendly path, and package managers may be added later.
4. **Self-upgrade is the mass-upgrade path.** `loopcoder upgrade [--version]`
   queries GitHub Releases, selects either the requested version or the latest
   stable release, downloads the matching asset, verifies checksums and
   signatures when available, and atomically swaps the active binary. The
   implementation must handle the Windows running-executable case by staging
   the replacement and completing the swap safely. Upgrade also refreshes the
   bundled playbook copy for that version.
5. **The playbook is bundled inside the binary.** `SKILL.md` and the related
   skill files are embedded into the release binary with Go `embed`.
   `loopcoder skill install` and `loopcoder init` write the version-matched
   playbook to the Claude skill directory and the Codex `AGENTS.md` entrypoint.
   A loopcoder version is one artifact: binary and brain must never skew.
6. **Multi-project isolation is explicit.** `~/.loopcoder/` is the user-level
   home for versioned binaries and the stable playbook copy. Projects select the
   binary through the existing `LOOPCODER_BIN` resolution path. Delivery state
   remains per repository through `.delivery.yml`, `.loopcoder/`, worktrees,
   state branches, leases, and worktree locks. The worktree lock is keyed by the
   canonical repository path. The development repository pins its own `go build`
   binary and its own in-repository playbook through a separate `loopcoder-dev`
   skill; it must never use a junction or symlink that mutates the global stable
   skill.
7. **Version compatibility is visible.** `.delivery.yml` already has
   `version`. A new optional `min_loopcoder_version` declares the minimum
   loopcoder binary version required by the project. `loopcoder doctor` reports
   the project's track, selected binary path, selected binary version, playbook
   version, `.delivery.yml` schema version, and compatibility result. Within
   0.x, loopcoder follows a semver-based stability policy for `.delivery.yml`
   schema, CLI flags, and label names, with migration guidance whenever a
   project must change configuration or automation.
8. **Prerequisites and integrity are checked, not shipped.** A release does not
   bundle the conductor runtime, provider CLIs, GitHub CLI, Git, or provider
   authentication. `loopcoder doctor` checks for the configured conductor
   runtime, provider CLI availability, provider CLI authentication where it can
   be detected, `git`, and `gh`. Every release publishes checksums and a signing
   path so install and upgrade downloads can be verified.
9. **Multi-machine use is identical everywhere.** Each machine independently
   installs, pins, and upgrades from GitHub Releases. No maintainer laptop,
   consumer workstation, CI runner, remote development box, or project checkout
   is special except for the loopcoder development repository's isolated source
   build.

## Release Artifacts

Each tag produces a single immutable version set:

- `loopcoder_<version>_windows_amd64.zip`
- `loopcoder_<version>_windows_arm64.zip`
- `loopcoder_<version>_darwin_amd64.tar.gz`
- `loopcoder_<version>_darwin_arm64.tar.gz`
- `loopcoder_<version>_linux_amd64.tar.gz`
- `loopcoder_<version>_linux_arm64.tar.gz`
- `SHA256SUMS`
- `SHA256SUMS` signature or an equivalent signing artifact

The exact filename convention may be refined by the implementation issue, but
release assets must remain predictable enough for install, upgrade, and package
manager automation to select the right artifact without scraping arbitrary
release text.

## Install Model

The install script is a thin bootstrapper. It must:

1. Detect OS and architecture.
2. Resolve the requested version or latest stable release from GitHub Releases.
3. Download the matching archive and checksum file.
4. Verify the checksum and signature path when configured for that release.
5. Install the binary under `~/.loopcoder/bin`.
6. Ensure `~/.loopcoder/bin` is on `PATH` or print exact shell-specific
   instructions when it cannot edit the user's shell profile safely.
7. Run or recommend `loopcoder --version` and `loopcoder doctor`.

The script must not require Go. `go install github.com/jasonhnd/loopcoder/cmd/loopcoder@vX`
remains supported for users who already have Go, but it is not the baseline
consumer install path.

## Upgrade Model

`loopcoder upgrade [--version]` is the standard way to move fleets and local
machines forward. It must:

1. Report the currently selected binary path and version.
2. Query GitHub Releases.
3. Select the requested version, or latest stable when `--version` is omitted.
4. Download the matching asset, checksums, and signing material.
5. Verify integrity before touching the active binary.
6. Stage the new binary under `~/.loopcoder/versions/<version>/`.
7. Atomically update the stable binary selection.
8. Handle Windows running-exe replacement by staging and completing the swap
   through a safe rename or deferred replacement strategy.
9. Refresh the stable bundled playbook copy from the same artifact.
10. Print the before and after versions and recommend `loopcoder doctor` when
    compatibility may need attention.

Upgrade must be deterministic and local to the machine. It must not reach into
other machines or copy artifacts from another checkout.

## Bundled Playbook And Skills

The release binary owns the stable playbook for its version. The embedded files
include `SKILL.md` and any skill files needed by the conductor entrypoints.

`loopcoder skill install` writes the version-matched Claude skill files from the
binary into the configured Claude skill directory. `loopcoder init` writes or
refreshes the repository entrypoint for Codex, including `AGENTS.md`, so Codex
hosts can find the same playbook contract.

The installed stable skill is not a live pointer to the source checkout. The
loopcoder development repository may have a separate `loopcoder-dev` skill that
points at the in-repo playbook for self-hosted development, but that development
skill must not mutate the global stable skill used by consumer repositories.

## Multi-Project Isolation

The user-level home is:

```text
~/.loopcoder/
  bin/
  versions/
  skills/
```

The exact internal layout is implementation-defined, but it must support
versioned binaries, a stable selected binary, and a stable playbook copy.

Per-project isolation uses existing mechanisms:

- `LOOPCODER_BIN` selects the binary when set.
- Otherwise `loopcoder` is resolved from `PATH`.
- `.delivery.yml` contains project delivery configuration.
- `.loopcoder/` stores per-repo run state.
- Git worktrees isolate worker checkouts.
- State branches isolate published loopcoder state.
- Leases isolate conductor ownership.
- Worktree locks are keyed by canonical repository path.

These mechanisms mean co-located repositories can use different loopcoder
versions, different `.delivery.yml` settings, different worktrees, and different
run state without clobbering each other.

## Version And Compatibility Policy

`.delivery.yml` remains the project configuration contract. The existing
`version` field identifies the config schema. The optional
`min_loopcoder_version` field identifies the minimum loopcoder binary version
that can safely operate on the project:

```yaml
version: 1
min_loopcoder_version: 0.3.0
```

`loopcoder doctor` must check:

- selected binary path and version;
- selected track, such as stable release or development source build;
- embedded playbook version and installed playbook version when applicable;
- `.delivery.yml` schema version;
- `min_loopcoder_version` compatibility;
- conductor runtime availability;
- provider CLI availability and detectable authentication;
- `git` and `gh` availability;
- whether the current repository appears to be the loopcoder development repo.

Within 0.x, loopcoder should treat `.delivery.yml` schema fields, documented CLI
flags, and documented GitHub label names as stable across patch releases.
Breaking changes require a minor release, migration guidance, and `doctor`
output that explains what changed. A removed or renamed field, flag, or label
must not fail silently.

## Prerequisites And Integrity

loopcoder releases ship loopcoder. They do not ship the host agent or provider
stack.

Required external tools remain separate:

- conductor host runtime, such as Claude Code or Codex;
- provider CLIs, such as `codex`, `claude`, or other configured providers;
- provider authentication;
- `git`;
- `gh` for GitHub issue, PR, and check integration.

Install, upgrade, and `doctor` must distinguish "loopcoder is missing or old"
from "the host runtime or provider CLI is missing or unauthenticated." Integrity
verification is mandatory for downloaded release assets through checksums and a
signing path.

## Maintainer Release Runbook

The permanent release process is:

1. Confirm the tree is ready for release and the changelog or release notes
   source is prepared.
2. Create and push the release tag.
3. CI builds all release assets for Windows, macOS, and Linux on `amd64` and
   `arm64`.
4. CI generates `SHA256SUMS`.
5. CI signs the checksum file or publishes the configured signing artifact.
6. CI publishes the GitHub Release with binaries, checksums, signing material,
   and release notes.
7. The maintainer verifies that all expected assets are present and that a
   clean install can verify checksums.
8. The maintainer smoke-checks `loopcoder --version`, `loopcoder doctor`,
   `loopcoder skill install`, and a minimal hosted conductor entrypoint from the
   published artifact.
9. Consumers upgrade with `loopcoder upgrade`, or install the tagged version
   with the install script or `go install ...@vX`.
10. Any failed asset, missing signature, wrong version, or bundled-playbook skew
    blocks promotion and requires a corrected release artifact path.

This runbook is intentionally release-asset driven. Copying a locally built
binary to another machine is never a release step.

## Self-Hosting Development Model

All future loopcoder development follows the doc-first workflow:

1. A design/spec issue writes and merges the spec under `docs/specs/`.
2. Separate code issues are opened only after the relevant spec merges.
3. The conductor dispatches one worker per issue.
4. An independent verifier reviews the resulting PR; the reviewer should not be
   the same provider instance that authored the work.
5. A human remains the merge gate.

The stable and development tracks stay isolated during this process. The
loopcoder development repository may use its own local `go build` binary and an
in-repo `loopcoder-dev` playbook. Consumer repositories, including the
maintainer's own non-loopcoder projects, use the published stable release.

For load-bearing self-modification, the maintainer must smoke-check merged
binary or playbook changes before relying on those changes in the same run. If
a run changes dispatch, verification, upgrade, skill installation, or the
conductor playbook itself, the conductor should continue using the previously
trusted stable path until the merged build and bundled playbook have passed a
minimal smoke check.

Model and effort inheritance remain unchanged. loopcoder does not choose a
model or reasoning effort for the user. Human merge remains mandatory.

## Follow-Up Code Issues

After this spec merges, implementation issues should be filed in this dependency
order:

1. **Release workflow:** add tag-triggered CI that builds cross-platform assets,
   generates checksums, signs or publishes signing material, and publishes
   GitHub Releases.
2. **Install script:** add Unix and Windows install scripts that install without
   Go, select the correct release asset, verify integrity, install under
   `~/.loopcoder/bin`, and expose `loopcoder --version`.
3. **Embedded playbook and skill install/init:** embed `SKILL.md` and related
   skill files with Go `embed`; make `loopcoder skill install` and
   `loopcoder init` write version-matched Claude and Codex entrypoints.
4. **`~/.loopcoder` home and binary pinning:** implement the user-level home,
   versioned binaries, stable selected binary, and project-level selection
   through `LOOPCODER_BIN` without disturbing existing per-repo state
   isolation.
5. **Self-upgrade:** implement `loopcoder upgrade [--version]` on top of the
   release workflow, installer integrity checks, embedded playbook, and
   versioned home layout, including Windows running-exe handling.
6. **Doctor compatibility reporting:** add `min_loopcoder_version`, compatibility
   checks, track/version reporting, bundled-playbook reporting, runtime and
   provider prerequisite checks, and clear failure messages.
7. **0.x stability policy and migrations:** document and enforce the semver
   stability policy for `.delivery.yml` schema, CLI flags, and label names, and
   add migration guidance for any future breaking change.

The issue order is intentional: upgrade depends on published release assets,
integrity verification, embedded playbook material, and a versioned local home.

## Non-Goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` change in this design-doc PR.
- No command behavior change in this design-doc PR.
- No new runtime dependency.
- No package manager integration in the first implementation slice.
- No bundled conductor runtime or provider CLI.
- No machine-to-machine copying path.
- No third release track for consumer projects.
- No weakening of the doc-first workflow, human merge gate,
  inherit-by-default model/effort behavior, or reviewer-not-worker guidance.

## Relationship To Existing Specs

- [`0000-loopcoder-v1.md`](0000-loopcoder-v1.md) defines the original
  conductor, worker, GitHub, and human merge model.
- [`0039-verification.md`](0039-verification.md) defines the verification gate.
- [`0146-attestation.md`](0146-attestation.md) defines attestation.
- [`0165-documentation-layout.md`](0165-documentation-layout.md) defines the
  `docs/specs/<NNNN>-<slug>.md` convention used by this file.
- [`0192-delivery-guardrails.md`](0192-delivery-guardrails.md) and
  [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md)
  are peer 0.3.x hardening specs.
