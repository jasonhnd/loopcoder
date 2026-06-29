---
id: 220
title: loopreview Must Not Flag a Brand-New Doc-First Spec as needs-human
status: accepted
date: 2026-06-29
issue: 220
pr: null
supersedes: []
superseded_by: []
---

# loopreview Must Not Flag a Brand-New Doc-First Spec as needs-human

This is a design-only amendment to
[`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md).
This PR must add only this document: no Go code, no `.delivery.yml` change, no
command behavior change, and no new runtime dependency. Implementation belongs
in separate code issues after this spec is reviewed and merged per
[`docs/PROCESS.md`](../PROCESS.md).

## Goal

`loopcoder loopreview` must distinguish a brand-new doc-first spec from a
missing merged design spec that a code PR needs for conformance review.

When a PR's deliverable is a new spec under the documentation layout defined by
[`0165-documentation-layout.md`](0165-documentation-layout.md), that spec is
expected to be absent from `origin/<base>` until the PR merges. That expected
absence must not by itself force a `needs-human` verdict.

## Problem

`loopreview` currently uses a merged-spec lookup so code PRs can be checked
against their already accepted design record:

```text
git show origin/main:docs/specs/<NNNN>-<slug>.md
  -> exit 128: path does not exist in 'origin/main'
```

That lookup is correct for code PRs, but it is over-applied to doc-first PRs
that introduce the spec being reviewed. Four consecutive doc-first spec PRs
reported that the new spec satisfied the issue acceptance criteria and also
reported that missing-on-base was expected because the PR added the file. The
final verdict still became `needs-human`, driven solely by the warning that the
merged design/spec was unavailable.

This reproduced across both Claude Haiku and Claude Opus 4.8 1M at xhigh. The
failure is therefore a `loopreview` classification defect, not a model-quality
or nondeterminism problem.

## Decisions

### 1. Detect Doc-First Spec Introduction

`loopreview` must detect the case where the PR introduces the spec under
review. The case is present when both are true:

- the PR is documentation-only, for example changes are confined to
  `docs/specs/*.md` or otherwise within `docs/**`;
- the merged-spec lookup on `origin/<base>` fails because the referenced spec
  path is net-new in the PR.

The implementation may use the changed-file list, base-branch lookup result,
and referenced spec path in the bounded packet to make this classification.
This classification is narrow: it applies to a doc-first PR whose deliverable is
the new spec, not to code PRs or mixed code/documentation PRs.

### 2. Reclassify Expected Absence As Non-Blocking

For the doc-first-introduces-the-spec case, `loopreview` must not surface
`merged spec unavailable` as a missing-evidence gap that forces `needs-human`.
The packet and prompt should instead identify the condition as expected:

```text
expected: this PR introduces the spec, so it is absent from origin/<base>
```

`spec_conformance` remains `not-applicable`. That value is already correct
because documentation-only work has no code implementation to compare against a
previously merged spec.

### 3. Decide The Verdict From The Issue And Packet

After expected absence is reclassified, the verifier must decide the PR like
any other review:

- return `pass` when the bounded packet shows the new spec satisfies the issue
  acceptance criteria and no blocking findings remain;
- return `fail` when there is a concrete defect, missing required decision,
  unrelated change, invalid frontmatter, or other worker-fixable problem;
- return `needs-human` only for genuine uncertainty, such as truncation hiding
  relevant evidence, ambiguous issue requirements, provider failure, malformed
  output, incomplete attestation, or another 0194 fail-closed condition.

The new spec's absence on the base branch is not one of those genuine
uncertainties when the PR itself introduces that spec.

### 4. Preserve Code-PR Safety

This amendment does not weaken 0194's safety case for code PRs.

For a code PR that should implement an already merged design spec, an unreadable
or absent merged spec remains a `needs-human` condition. In that case,
`loopreview` cannot safely check `spec_conformance`, and the conductor must not
treat the PR as merge-eligible.

Only the doc-first-introduces-the-spec case is reclassified as expected and
non-blocking. Mixed PRs that include code changes do not get this exemption
unless a later accepted spec explicitly defines a safe mixed-PR classification.

### 5. Make The Packet And Prompt Explicit

The bounded review packet and verifier prompt must distinguish expected
doc-first absence from missing evidence. The packet should carry enough
structured context for the provider to tell which case applies:

- changed files and whether they are documentation-only;
- the referenced spec path;
- whether that path exists on the PR head;
- whether the `origin/<base>` lookup failed because the file is net-new;
- the expected/non-blocking label for this specific condition.

The prompt must not ask the provider to treat every failed merged-spec lookup as
the same evidence gap. It should reserve the missing-evidence wording for code
PRs or other cases where a merged spec should already exist.

Tests that assert verdict behavior must include a net-new doc-first spec PR
whose bounded evidence is clean and whose verifier result is `pass` with
`spec_conformance: not-applicable`.

### 6. Preserve 0194 Reliability Guarantees

All 0194 reliability guarantees remain intact:

- bounded review packets;
- visible truncation and omitted-evidence markers;
- read-only provider invocation;
- timeout enforcement;
- structured `pass`, `fail`, or `needs-human` verdicts;
- complete Verifier attestation for normal pass/fail results.

This amendment changes only the classification of an expected base-branch
absence for a net-new doc-first spec. It does not permit silent evidence
omission, broad unbounded review work, provider mutation, malformed verdicts, or
incomplete attestation.

## Expected Behavior

For a clean doc-first PR that adds
`docs/specs/0220-loopreview-new-spec-not-a-blocker.md`, changes no code, and
satisfies issue #220, `loopreview` should be able to return:

```json
{
  "verdict": "pass",
  "findings": [],
  "evidence": "The PR is documentation-only and introduces the referenced spec; the spec is expected to be absent from origin/<base>. The bounded packet shows the draft spec satisfies the issue acceptance criteria.",
  "spec_conformance": "not-applicable"
}
```

For a code PR whose issue says `implement per
docs/specs/0194-reliable-loopreview-verifier.md`, failure to read that merged
spec from `origin/<base>` remains `needs-human` because the verifier cannot
safely evaluate conformance.

## Follow-On Code Issues

After this spec merges, separate implementation issues should be filed:

1. **Classify net-new doc-first specs in the review packet.** Teach the packet
   builder to identify documentation-only PRs, recognize when the referenced
   spec exists on the PR head but not on `origin/<base>`, and label that state
   as expected rather than missing evidence.
2. **Update verifier prompt and verdict handling.** Rewrite the relevant prompt
   wording so expected doc-first spec absence does not force `needs-human`,
   while code PRs with absent or unreadable merged specs still fail closed as
   `needs-human`.
3. **Add regression tests.** Cover a clean net-new doc-first spec PR returning
   `pass` with `spec_conformance: not-applicable`, and a code PR with a missing
   merged spec returning `needs-human`.

Each follow-on issue must be cross-platform Go where code changes are required,
introduce no runtime dependency, and preserve the human merge gate and
reviewer-not-worker guidance.

## Non-Goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` change.
- No command behavior change in this design-doc PR.
- No new runtime dependency.
- No weakening of 0194 bounded-packet, read-only, timeout, structured-verdict,
  or attestation guarantees.
- No change to the human merge gate.
- No change to reviewer-not-worker guidance.

## Acceptance Criteria For Implementation

- A documentation-only PR that introduces a new referenced spec can receive
  `pass` when the bounded evidence shows the issue acceptance criteria are
  satisfied and no blocking findings remain.
- In that case, the missing base-branch spec is reported as expected and
  non-blocking, not as a missing-evidence gap.
- `spec_conformance` remains `not-applicable` for documentation-only spec PRs.
- A code PR that should have an already merged spec still returns `needs-human`
  when that merged spec is absent or unreadable.
- Packet and prompt wording distinguish `expected: this PR introduces the spec`
  from genuine missing evidence.
- Regression tests cover both the clean net-new doc-first spec pass case and
  the code-PR missing-merged-spec `needs-human` case.
