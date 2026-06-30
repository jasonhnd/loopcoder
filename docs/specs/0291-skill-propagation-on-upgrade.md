---
id: 291
title: Skill Propagation On Binary Upgrade
status: draft
date: 2026-06-30
issue: 291
pr: null
supersedes: []
superseded_by: []
---

# Skill Propagation On Binary Upgrade

This is a design-only spec for loopcoder 0.3.5. This PR must add only this
document: no Go code, no `.delivery.yml` change, no command behavior change,
and no edits to existing specs. Implementation belongs in separate code issues
after this spec is reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

## Goal

The conductor playbook installed on disk must follow the loopcoder binary that
owns it. When a user upgrades loopcoder, the embedded `SKILL.md` and
`AGENTS.md` content carried by that binary must reach the installed Claude
skill directory instead of leaving an older playbook in place.

This closes a real release skew where a 0.3.4 binary emitted the human-readable
Worker attestation block, but an already-installed
`~/.claude/skills/loopcoder/SKILL.md` still carried pre-0.3.4 guidance that
only told the Conductor to summarize an attestation line. The binary behavior
was current, but the playbook that operators actually followed was stale.

## Problem

[`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md)
requires binary and playbook to move together: a loopcoder version is one
artifact, and `loopcoder upgrade` refreshes the bundled playbook copy for that
version. In practice, two gaps let them skew:

1. `loopcoder skill install` without `--force` silently skips existing files.
   Re-running the command after upgrading returns an `exists` result and leaves
   stale files untouched.
2. `loopcoder upgrade` does not refresh the installed skill at all after a
   successful binary upgrade.

The result is that consumers can upgrade the binary and still run an older
conductor playbook. That undermines playbook changes such as
[`0214-human-readable-attestation.md`](0214-human-readable-attestation.md) and
[`0218-surface-worker-attestation.md`](0218-surface-worker-attestation.md),
where the binary can surface richer attestation output only if the installed
playbook tells the Conductor to relay it.

## Decisions

1. **`skill install` is stale-aware by default.** For loopcoder-managed skill
   files, `loopcoder skill install` compares the installed file content to the
   binary's embedded content. If the installed file is absent, it writes the
   embedded content and reports `created`. If the installed file is present and
   identical, it reports `unchanged`. If the installed file is present and
   differs, it overwrites the installed file with the embedded content and
   reports `updated`.
2. **`--force` still forces a rewrite.** `loopcoder skill install --force`
   rewrites loopcoder-managed files even when the installed content is already
   identical to the embedded content. The command must still report that the
   file was force-written distinctly enough for operators and tests to tell it
   apart from an ordinary `unchanged` result.
3. **Silent stale skips are removed for managed files.** The current `exists`
   skip behavior is no longer valid when an installed loopcoder-managed file
   differs from the embedded version. A stale managed file must be overwritten
   unless a future spec defines an explicit user-owned override mechanism.
4. **Upgrade refreshes the bundled skill.** After `loopcoder upgrade`
   successfully installs and selects the new binary, it refreshes the bundled
   skill with the same stale-aware semantics as `loopcoder skill install`.
   Upgrade must report which managed files were created, updated, unchanged, or
   force-written as part of the refresh.
5. **Doctor warns on stale installed skills.** `loopcoder doctor` compares
   installed loopcoder-managed skill files against the selected binary's
   embedded content. If an installed managed file differs, `doctor` warns that
   the installed skill is stale and tells the user to run
   `loopcoder skill install`.
6. **Overwrite scope is limited.** The only files covered by this spec are the
   loopcoder-managed files `SKILL.md` and `AGENTS.md` in the installed
   loopcoder skill directory. No other file in that directory may be created,
   deleted, rewritten, normalized, formatted, or otherwise touched by this
   stale-aware update path unless a later spec explicitly adds it to the
   managed set.
7. **One-time transition is explicit.** Users who already have a stale
   installed skill must run `loopcoder skill install` once after upgrading to
   the fixed loopcoder version. That fixed `skill install` command is
   stale-aware and updates the old files. From the fixed version onward,
   `loopcoder upgrade` performs this refresh automatically after each
   successful binary upgrade.
8. **Attestation contracts do not change.** This spec does not change the
   machine attestation contracts from [`0146-attestation.md`](0146-attestation.md),
   the human-readable rendering from
   [`0214-human-readable-attestation.md`](0214-human-readable-attestation.md),
   or the 0.3.4 default-on pretty behavior carried by
   [`0218-surface-worker-attestation.md`](0218-surface-worker-attestation.md)
   and related implementation. It only ensures the embedded playbook content
   reaches disk when the binary changes.

## Managed File Semantics

The embedded binary content is the source of truth for managed skill files.
Comparison is by exact file content after reading the installed file and the
embedded file as bytes. The implementation must not rely on timestamps, binary
version strings, file size alone, or a cached manifest to decide whether an
installed managed file is current.

The command result for each managed file must be observable and testable. The
minimum statuses are:

- `created`: the installed file did not exist and was written from embedded
  content.
- `updated`: the installed file existed, differed from embedded content, and
  was overwritten.
- `unchanged`: the installed file existed and already matched embedded content.
- `force-written`: `--force` rewrote the file even though ordinary stale-aware
  comparison would not have required a write.

The exact CLI formatting is implementation-defined, but it must be clear enough
for a user to see what happened and for tests to assert each status.

## Upgrade Behavior

`loopcoder upgrade` keeps the release flow from
[`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md):
download or select the requested version, verify integrity, stage the binary,
and switch the active binary only after the upgrade succeeds.

The skill refresh happens after the new binary is the selected binary and only
uses the embedded content from that selected binary. A failed binary upgrade
must not rewrite the installed skill from a partially downloaded or unselected
artifact. If the binary upgrade succeeds but the skill refresh fails, the
command must report the refresh failure clearly and recommend
`loopcoder skill install` so the user has an explicit recovery step.

Upgrade output must include a concise summary of the managed files refreshed,
including whether `SKILL.md` and `AGENTS.md` were created, updated, unchanged,
or force-written.

## Doctor Behavior

`loopcoder doctor` must report stale installed managed files as a warning, not
as an unrelated provider, GitHub, or runtime failure. The warning must identify
that the installed loopcoder skill differs from the selected binary's embedded
skill content and must give the direct remediation:

```text
run: loopcoder skill install
```

Doctor should continue reporting other release, provider, repository, and
configuration checks independently. A stale skill warning must not hide or
replace existing compatibility checks from
[`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md).

## Acceptance Criteria For Code Issues

- `loopcoder skill install` writes absent managed files as `created`, overwrites
  differing managed files as `updated`, reports identical managed files as
  `unchanged`, and no longer silently skips stale `SKILL.md` or `AGENTS.md`.
- `loopcoder skill install --force` rewrites managed files even when identical
  and reports the forced rewrite separately from `unchanged`.
- `loopcoder upgrade` refreshes the bundled skill from the successfully selected
  new binary using the same stale-aware managed-file semantics and reports what
  was refreshed.
- `loopcoder doctor` warns when installed `SKILL.md` or `AGENTS.md` differs from
  the selected binary's embedded content and tells the user to run
  `loopcoder skill install`.
- The stale-aware overwrite scope is limited to `SKILL.md` and `AGENTS.md` in
  the installed loopcoder skill directory. Other files in that directory are not
  touched.
- Tests cover absent, identical, stale, and forced managed-file cases for
  `skill install`; successful upgrade refresh; upgrade refresh failure
  reporting; doctor stale-skill warning; and preservation of unrelated files in
  the skill directory.
- No attestation schema, canonical JSON, stable header, pretty rendering, or
  default-on human-readable behavior changes as part of this implementation.

## Follow-Up Issues

After this spec merges, implementation should be split only as needed, but each
code issue must reference this merged spec and preserve its limited scope. The
expected implementation surface is:

1. Add stale-aware managed-file comparison and reporting to
   `loopcoder skill install`.
2. Refresh the installed bundled skill after a successful `loopcoder upgrade`.
3. Add the stale installed skill warning to `loopcoder doctor`.
4. Add tests for managed-file status reporting, upgrade refresh behavior,
   doctor warnings, and untouched unrelated files.

## Non-Goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` change in this design-doc PR.
- No command behavior change in this design-doc PR.
- No edits to existing specs.
- No change to machine attestation contracts from
  [`0146-attestation.md`](0146-attestation.md).
- No change to the 0.3.4 default-on pretty attestation behavior.
- No broad sync of every file in the installed skill directory.
- No deletion or rewriting of user-created files under the installed skill
  directory.

## Relationship To Existing Specs

- [`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md)
  defines the release, upgrade, embedded playbook, and binary/playbook skew
  prevention model. This spec narrows the concrete 0.3.5 behavior for stale
  installed skill files.
- [`0214-human-readable-attestation.md`](0214-human-readable-attestation.md)
  defines the human-readable attestation rendering carried by the playbook and
  binary. This spec ensures installed playbooks can receive that guidance when
  users upgrade.
- [`0218-surface-worker-attestation.md`](0218-surface-worker-attestation.md)
  defines surfacing Worker attestation and updating conductor reporting
  guidance in `SKILL.md` and `AGENTS.md`. This spec ensures those managed files
  can be refreshed on disk.
- [`0146-attestation.md`](0146-attestation.md) remains the machine attestation
  contract and is not amended by this spec.
