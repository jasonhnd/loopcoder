---
id: 535
title: loopreview Packet Truncation Reliability
status: draft
date: 2026-07-06
issue: 535
pr: null
supersedes: []
superseded_by: []
---

# loopreview Packet Truncation Reliability

This is a design-only spec for a `loopreview` verifier reliability fix. This PR
adds only this document: no Go code, no `.delivery.yml` change, no command
behavior change, and no new dependency. Code slices are filed only after this
spec merges, per [`docs/PROCESS.md`](../PROCESS.md).

The defect is not that the verifier judged incorrectly. The verifier behaved
correctly under the existing contract: when a bounded packet hides
acceptance-criteria-relevant evidence, the safe verdict is `needs-human`. The
defect is that the packet made a large, valid documentation PR unverifiable even
though the PR head contained the needed evidence.

## Goal

`loopcoder loopreview` must make large-but-valid documentation and added-file
PRs reviewable without weakening the bounded-packet safety model.

The packet builder must:

- keep bounded prompt inputs and explicit truncation markers;
- provide PR-head file content when a truncated diff would otherwise hide
  acceptance-criteria-relevant documentation;
- review against the PR head and true base, not a stale local checkout; and
- preserve `needs-human` as the safe default when required evidence is genuinely
  unavailable.

## Motivating evidence

Run `0533-audit-usability`, PR #534, exposed the gap:

- PR #534 was documentation-only. It changed one file,
  `docs/specs/0533-audit-consumer-repo-usability.md`, at roughly 600 lines, and
  all five required CI checks were green.
- `loopreview` used provider `claude`, model `claude-opus-4-8[1m]`, effort
  `max`, and returned `verdict: needs-human` with `spec_conformance: pass`.
- The verifier's own evidence identified the root cause: the packet diff
  carried a `TRUNCATED ... omitted 5194 bytes, 100 lines` marker covering lines
  501-600. Issue #533 AC#1 required two sections, `Relationship to existing
  specs` and `Non-goals`, that fell inside that omitted range.
- The verifier could not self-resolve by reading the file directly. Its
  checkout was stale: listing `docs/specs` ended at 0484, so neither 0518 nor
  0533 was present. Shell access was also disallowed.
- The conductor resolved the gap out of band by reading the full PR-head file
  with `git show origin/loop/issue-533:docs/specs/0533-audit-consumer-repo-usability.md`.
  Both required sections were present, so the substantive review result was
  pass.

This is a packet reliability defect. A valid documentation PR should not become
`needs-human` solely because the packet chose a generic per-file diff excerpt
instead of bounded, PR-head documentation content.

## Problem

The current packet contract is safe but too coarse for long documentation
deliverables.

1. A generic per-file diff cap can truncate the tail of a large documentation
   file even when the whole file is the deliverable and is small enough to be
   reviewed within the overall prompt budget.
2. A `TRUNCATED` marker does not tell the verifier whether omitted material is
   also available elsewhere in the packet. If the hidden region might contain a
   required section, the verifier must return `needs-human`.
3. Direct file inspection is not a reliable fallback when the verifier's
   checkout is stale or when provider permissions do not include shell access.
4. Raising limits without a guard could mask real omissions. The fix must make
   more relevant evidence available, not make truncation easier to ignore.

## Decisions

### 1. Packet sizing for documentation and added files

`loopreview` MUST keep a total prompt budget. It MUST NOT send an unbounded
diff, unbounded file body, or unbounded repository snapshot to the provider.

The implementation MUST add a documentation/added-file body path that is
separate from the generic per-file diff cap. For documentation-only PRs, and
for added textual files that are themselves the deliverable, the packet builder
MUST prefer bounded PR-head file bodies over generic diff snippets when the file
body fits within the documentation-file and total prompt budgets.

The initial implementation SHOULD add explicit `ReviewPacketLimits` fields for
this path, such as:

- a per documentation-file body budget;
- an aggregate documentation-body budget; and
- a maximum count of full bodies to include before falling back to excerpts.

Those limits MUST have conservative defaults in the centralized defaults layer
and MUST be injectable in tests. The first code slice SHOULD NOT add a new
operator-facing `.delivery.yml` knob unless a code issue shows that local
configuration is necessary. If a later slice exposes configuration, it MUST
validate minimums and maximums and MUST still obey the total prompt budget.

Generic source diffs, generated files, large binary artifacts, and mixed
code/documentation PRs remain governed by the existing bounded diff behavior
unless a later accepted spec defines a broader file-body policy.

### 2. File-read fallback from the PR head

When a changed documentation or added textual file has a `TRUNCATED diff for
<path>` marker, the packet MUST either include a bounded PR-head file body for
the same path or explicitly state why PR-head content is unavailable.

For documentation-only PRs that add or replace `docs/specs/*.md`, the packet
SHOULD include the full PR-head file body when it fits the documentation-file
body budget. A 600-line design spec like PR #534 should fit by default unless
the total prompt budget is already consumed by other required evidence.

If the full body does not fit, the packet builder SHOULD include section-aware
PR-head excerpts around headings, paths, or phrases named by the issue
acceptance criteria. For issue #533's failure class, that means headings such as
`Relationship to existing specs` and `Non-goals` must be selected when present
and when they fit the excerpt budget.

The packet MUST label the fallback content distinctly, for example:

```text
# PR-head file content
## docs/specs/0533-audit-consumer-repo-usability.md
Source: origin/loop/issue-533:<path>
Completeness: complete | excerpted | unavailable
```

The prompt MUST tell the verifier to treat PR-head file content as the
authoritative fallback for that changed file. A `TRUNCATED` diff marker is not a
blocking omission when the packet also provides complete PR-head content for the
same path and the verifier can confirm the required evidence there.

### 3. Checkout and reference freshness

`loopreview` MUST build its review packet from the PR head and the PR's true
base branch as reported by GitHub for the PR being reviewed.

Before invoking the provider, `loopreview` MUST know and record, at minimum:

- PR number;
- base branch and base SHA when available;
- head branch and head SHA when available; and
- the source ref used for any PR-head file body.

Any direct file content fallback MUST read from the PR head ref or a verified
PR-head checkout. It MUST NOT read from the conductor's current worktree, a
stale base checkout, or an inferred branch that has not been fetched or
verified against the PR metadata.

If a verifier worktree is made available to the provider, that checkout MUST
reflect the PR head and true base. If `loopreview` cannot fetch or verify the
PR head/base state needed for direct file inspection, the packet MUST say so and
the verifier MUST treat the missing evidence as `needs-human` when it matters to
the acceptance criteria.

### 4. Anti-masking guard

The implementation MUST NOT let a truncated or oversized diff silently pass
review.

A `TRUNCATED` marker remains a fail-closed signal unless one of these is true:

1. the omitted material is irrelevant to the issue, spec, changed files, and
   risk under review; or
2. the packet includes complete or sufficient PR-head fallback content for the
   same path, and the verifier cites that fallback as evidence.

When acceptance-criteria-relevant content is unavailable because all applicable
budgets are exhausted, PR-head content cannot be read, the checkout/ref is
stale, or the overlap cannot be determined safely, the verdict MUST remain
`needs-human`. The implementation MUST NEVER convert that condition into
`pass` by relying on a documentation-only label, a green CI check, or a clean
`spec_conformance` field.

The packet should make the guard auditable by recording:

- every truncated path and omitted byte/line count;
- whether that truncation is covered by complete PR-head content, covered by a
  targeted excerpt, or uncovered;
- the ref used for fallback content; and
- any reason fallback content could not be included.

## Expected behavior

For a PR like #534, the packet should contain the one changed documentation
file's PR-head body, or at least complete section-aware excerpts containing the
acceptance-criteria-named sections. The verifier should not need shell access or
a lucky fresh local checkout to confirm those sections.

If the packet includes complete PR-head content for
`docs/specs/0533-audit-consumer-repo-usability.md`, the verifier may return
`pass` when the issue criteria are satisfied and no blocking findings remain,
even if the diff excerpt still carries a covered `TRUNCATED` marker.

If the packet cannot include or read the portions containing `Relationship to
existing specs` and `Non-goals`, the verifier must still return `needs-human`.
That outcome is correct and must remain non-merge-eligible.

## Follow-up code slices

The implementation should be filed as separate code issues in this order:

1. **Fresh PR refs and packet metadata.** Fetch or otherwise verify the PR head
   and true base for `loopreview`, record head/base metadata in the packet, and
   add a read-only helper for PR-head file content. Tests must prove fallback
   reads do not use a stale base or the conductor's current worktree.
2. **Documentation and added-file body packet.** Add bounded
   documentation/added-file body limits to `ReviewPacketLimits` and centralized
   defaults. Include complete PR-head bodies for doc-only added or replaced
   textual files that fit the budgets, with aggregate-count and aggregate-byte
   guards. Add regression coverage for a roughly 600-line `docs/specs/*.md`
   file whose required sections are beyond the generic per-file diff cap.
3. **Truncation coverage classification.** Track whether each `TRUNCATED` diff
   marker is uncovered, covered by complete PR-head content, or covered by a
   targeted excerpt. Render that classification in the packet and update the
   prompt so covered truncation can pass while uncovered relevant truncation
   remains `needs-human`.
4. **Section-aware fallback excerpts.** When a full PR-head file body exceeds
   the documentation-body budget, select bounded excerpts around headings and
   phrases named by issue acceptance criteria, merged specs, or the changed
   file path. Tests must cover named headings near the tail of a large spec.
5. **Configuration and docs polish if needed.** If the earlier slices show the
   defaults are insufficient for real repos, add validated configuration for
   documentation-body packet limits. Otherwise keep the surface internal and
   update command/help docs only to describe the new packet behavior.

Each slice must preserve the read-only verifier boundary, closed verdict set,
local-only attestation behavior, relay behavior, and human merge gate.

## Acceptance criteria for implementation

- A documentation-only PR that adds or replaces one normal-size `docs/specs/*.md`
  file can be reviewed from the packet even when the generic diff excerpt would
  truncate the tail.
- The packet includes PR-head content for covered documentation truncation and
  identifies the ref used to read it.
- Stale local checkouts cannot be used as fallback evidence.
- Uncovered acceptance-criteria-relevant truncation still returns
  `needs-human`.
- Tests cover complete fallback, section-aware excerpt fallback, unreadable
  PR-head fallback, stale-checkout protection, and total-prompt-budget
  exhaustion.
- No code path allows `pass` solely because a PR is documentation-only or CI is
  green.

## Relationship to existing specs

- [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md)
  remains the parent reliable-verifier contract. This spec refines the bounded
  packet so high-signal documentation evidence is included before a safe
  `needs-human` fallback is needed.
- [`0408-verifier-stream-json.md`](0408-verifier-stream-json.md) fixed a
  provider streaming/stall false-kill. This spec fixes a separate content
  completeness false escalation: the provider ran and answered correctly, but
  the packet lacked necessary evidence.
- [`0039-verification.md`](0039-verification.md) defines the verification gate,
  `pass` / `fail` / `needs-human` routing, and the rule that documentation-only
  PRs can pass without code gates when they satisfy the issue scope. This spec
  ensures the verifier has the documentation evidence needed to apply that rule.
- [`0220-loopreview-new-spec-not-a-blocker.md`](0220-loopreview-new-spec-not-a-blocker.md)
  says a net-new doc-first spec's absence from `origin/<base>` is expected and
  non-blocking. This spec complements it by requiring the PR-head spec content
  to be available in the packet when diff truncation would otherwise hide it.
- [`0423-operational-reliability-hardening.md`](0423-operational-reliability-hardening.md)
  made packet ordering source-first so generated files do not consume the
  global diff budget. This spec addresses the remaining per-file documentation
  truncation and stale-fallback problem.

## Non-goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` change in this design-doc PR.
- No command behavior change in this design-doc PR.
- No new runtime dependency.
- No weakening of bounded packet limits, visible truncation markers,
  read-only verifier execution, timeout handling, structured verdict parsing,
  local-only attestation, relay obligations, or human merge authority.
- No change to worker or verifier attestation contracts.
- No permission to ignore genuinely missing evidence.
