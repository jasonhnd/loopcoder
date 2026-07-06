---
id: 539
title: loopreview Cited Spec Is Not A Conformance Target
status: draft
date: 2026-07-06
issue: 539
pr: null
supersedes: []
superseded_by: []
---

# loopreview Cited Spec Is Not A Conformance Target

This is a design-only spec for a `loopreview` verifier reliability fix. This PR
adds only this document: no Go code, no `.delivery.yml` change, no command
behavior change, and no new dependency. Code slices are filed only after this
spec merges, per [`docs/PROCESS.md`](../PROCESS.md).

The defect is a target-classification bug. `loopreview` must not treat every
`docs/specs/*.md` path cited in a PR, issue, or spec body as the merged design
document that code must conform to. A body-cited spec can be historical
evidence. A conformance target is the specific merged design document a code
issue says it implements.

## Goal

`loopcoder loopreview` must distinguish a referenced documentation path from a
spec-conformance target.

The verifier must:

- derive a conformance target only from the code issue's explicit
  `implement per docs/...md` design-doc reference;
- treat documentation-only spec PRs as having no code conformance target;
- treat absent body-cited sibling specs as expected and non-blocking when they
  are not the conformance target; and
- preserve `needs-human` for a code issue whose actual conformance target is
  absent or unreadable.

## Motivating evidence

PR #536, the doc-first spec PR for
[`0535-loopreview-packet-truncation-reliability.md`](0535-loopreview-packet-truncation-reliability.md),
exposed the defect:

- PR #536 was documentation-only and changed one new spec file. The full diff
  was present from `@@ -0,0 +1,287 @@`, and there was no `TRUNCATED` marker.
- All five required CI checks were green.
- `loopreview` returned `verdict: needs-human` with
  `spec_conformance: not-applicable`.
- The verifier's own evidence said the packet was sufficient and that it was
  deciding pass. It also verified all six issue acceptance criteria in the
  in-diff spec content.
- The sole `needs-human` trigger was a failed base-branch lookup:

```text
git -C ... show origin/main:docs/specs/0533-audit-consumer-repo-usability.md
  -> exit status 128
```

Spec 0533 was cited in spec 0535's motivating evidence because PR #534 exposed
the truncation failure class. It was an unmerged sibling spec, absent from
`origin/main`, and not the conformance target for PR #536. The verifier itself
identified this absence as expected and non-blocking under
[`0220-loopreview-new-spec-not-a-blocker.md`](0220-loopreview-new-spec-not-a-blocker.md),
but the harness still forced `needs-human`.

This is distinct from
[`0535-loopreview-packet-truncation-reliability.md`](0535-loopreview-packet-truncation-reliability.md).
For PR #536, the review packet was complete. The failure was not hidden
acceptance-criteria evidence; it was a body-cited sibling spec being
misclassified as a merged conformance target.

## Problem

`loopreview` currently conflates two different path roles:

1. **Conformance target:** the merged design/spec a code issue must implement.
   This document is read from `origin/<base>` so the verifier checks code
   against the accepted contract, not against a PR-modified copy.
2. **Referenced doc:** a documentation path cited in issue text, PR text, or
   spec prose as historical context, evidence, related work, or motivation.

The fail-closed rule for unreadable conformance targets is correct for code
work. It is incorrect for arbitrary referenced docs. A doc-first spec may cite
unmerged sibling specs precisely because related spec PRs are moving through
the process in parallel. That citation must not silently become a hard
base-branch lookup that can override the verifier's substantive pass.

## Decisions

### 1. Conformance-target identification

`loopreview` MUST derive the spec-conformance target only from a code issue's
explicit implementation reference, such as:

```text
implement per docs/specs/0194-reliable-loopreview-verifier.md
```

The implementation reference MUST come from the linked issue body being
reviewed, or from a machine-parsed field populated only from that issue body.
The reference MUST identify the merged design document the code issue is
required to implement.

`loopreview` MUST NOT derive a conformance target from arbitrary
`docs/*.md` or `docs/specs/*.md` paths mentioned in:

- PR body prose;
- the added or edited spec body;
- motivating evidence;
- relationship sections;
- changelog-style references; or
- issue text that cites docs without saying the work implements them.

Those paths are referenced docs. They MAY be included as bounded contextual
evidence when available, but they MUST NOT be promoted to conformance targets.

### 2. Documentation-only spec PRs

For a documentation-only PR whose deliverable is a new or edited spec, the PR
IS the design document under review. It is not code implementing a previously
merged design.

For that case, `spec_conformance` MUST be `not-applicable`. The verdict MUST be
based on the issue acceptance criteria, changed files, bounded packet evidence,
frontmatter validity, relationship/non-goal coverage, and ordinary doc-review
risks.

The verdict MUST NOT be forced to `needs-human` merely because a sibling spec
cited in the PR body or spec prose is absent from `origin/<base>`.

### 3. Referenced-doc absence handling

When a referenced doc is absent from `origin/<base>` because it is a net-new
spec, an unmerged sibling spec, or otherwise not yet present on the base
branch, `loopreview` MUST treat that absence as expected and non-blocking when
the doc is not the conformance target.

The packet or verdict evidence SHOULD label this case plainly, for example:

```text
expected: cited doc is not the conformance target and is absent from origin/<base>
```

The absence MAY reduce optional context. It MUST NOT override a verifier
decision that the bounded packet is otherwise sufficient. This rule follows the
classification in
[`0220-loopreview-new-spec-not-a-blocker.md`](0220-loopreview-new-spec-not-a-blocker.md)
and extends it from "the PR introduces the spec under review" to "the PR cites
an unmerged sibling spec that is not the conformance target."

### 4. Anti-masking guard

This spec MUST NOT weaken the genuine fail-closed case.

For a code issue whose linked issue says `implement per docs/...md`, that doc
is the conformance target. If that actual conformance target is missing,
unreadable, ambiguous enough to prevent safe criteria extraction, or cannot be
loaded from `origin/<base>` because of repository state or permissions,
`needs-human` remains correct.

`loopreview` MUST NEVER convert a missing actual conformance target into
`pass` by calling it a referenced doc. The distinction MUST be made before the
base-branch lookup result is folded into the verdict.

Mixed code/documentation PRs do not receive the documentation-only exemption
unless their linked issue is a valid code issue with a readable merged
conformance target and the code can be checked against it. A PR cannot edit its
own contract and then claim conformance to that edited contract in the same
review.

### 5. Packet and prompt shape

The bounded review packet SHOULD make path roles explicit before provider
review:

- `conformance_target`: the single merged design doc path for a code issue, or
  `none` for documentation-only spec work;
- `referenced_docs`: bounded contextual doc paths cited by the issue, PR, or
  spec prose;
- `base_lookup`: for each path that was looked up, whether the lookup was for a
  conformance target or optional referenced context; and
- `absence_classification`: `blocking` only for an actual conformance target,
  `expected-non-blocking` for cited sibling/net-new docs that are not the
  target.

The verifier prompt MUST tell providers that only the conformance target
participates in spec-conformance. It MUST tell providers that absent
referenced docs may be mentioned as optional context loss, but they are not a
standalone `needs-human` reason when the issue criteria and changed diff are
otherwise reviewable.

### 6. Verdict folding

Harness-level verdict folding MUST preserve the provider's substantive verdict
unless a blocking infrastructure or conformance-target condition exists.

If the provider returns `pass` with `spec_conformance: not-applicable` for a
documentation-only spec PR, a failed lookup for an absent body-cited sibling
spec MUST NOT rewrite the verdict to `needs-human`.

If the provider returns `needs-human` because the packet is truncated, the
issue is ambiguous, a required check is unavailable, or the actual conformance
target is unreadable, the result remains `needs-human`. This spec only removes
the false escalation caused by misclassified referenced docs.

## Expected behavior

For a PR like #536, `loopreview` should be able to return:

```json
{
  "verdict": "pass",
  "findings": [],
  "evidence": "The PR is documentation-only, spec_conformance is not-applicable, and the packet contains the full added spec. The cited 0533 sibling spec is absent from origin/main, but it is not the conformance target and its absence is expected/non-blocking.",
  "spec_conformance": "not-applicable"
}
```

For a code issue whose body says:

```text
implement per docs/specs/0194-reliable-loopreview-verifier.md
```

failure to read that exact merged spec from `origin/<base>` still returns
`needs-human`, because the verifier cannot safely evaluate code conformance.

## Follow-up code slices

The implementation should be filed as separate code issues in this order:

1. **Path-role extraction.** Teach the review packet builder to identify one
   conformance target only from issue-level `implement per docs/...md`
   language for code issues. Classify all other cited doc paths as referenced
   docs. Add tests proving PR/spec prose citations do not become conformance
   targets.
2. **Absence classification.** Record base-branch lookup failures with their
   path role. Fold missing or unreadable conformance targets to
   `needs-human`, but label missing referenced docs as expected/non-blocking
   when they are net-new or unmerged sibling specs. Add regression coverage for
   `origin/main:docs/specs/0533-audit-consumer-repo-usability.md` returning
   exit 128 while reviewing a clean doc-first 0535-style PR.
3. **Prompt and packet rendering.** Render `conformance_target`,
   `referenced_docs`, lookup role, and absence classification in the bounded
   packet. Update the verifier prompt so providers use only the conformance
   target for `spec_conformance` and treat absent referenced docs as optional
   context loss.
4. **Verdict-folding regression tests.** Cover a documentation-only spec PR
   returning `pass` with `spec_conformance: not-applicable` despite an absent
   body-cited sibling spec, and a code PR returning `needs-human` when its
   actual `implement per` conformance target is absent or unreadable.
5. **Reference docs and help polish if needed.** After behavior is implemented,
   update command/reference documentation to describe conformance targets
   versus referenced docs. Do not change relay obligations, attestation
   wording, provider defaults, or merge authority.

Each slice must preserve the read-only verifier boundary, closed verdict set,
bounded packet behavior, local-only attestation behavior, relay behavior, and
human merge gate.

## Acceptance criteria for implementation

- A documentation-only spec PR with complete bounded evidence can receive
  `pass` when it satisfies the issue, even if it cites an unmerged sibling spec
  absent from `origin/<base>`.
- `spec_conformance` remains `not-applicable` for documentation-only spec PRs.
- Only a code issue's `implement per docs/...md` reference creates a
  conformance target.
- Arbitrary spec paths in PR bodies or spec prose are referenced docs, not
  conformance targets.
- Missing referenced docs are expected/non-blocking when they are net-new or
  sibling unmerged specs and are not needed to decide the issue criteria.
- Missing or unreadable actual conformance targets for code issues still return
  `needs-human`.
- Tests cover the PR #536 / 0535 failure class and the preserved code-issue
  fail-closed case.
- No code path permits `pass` when the bounded packet hides required evidence
  or when the actual code conformance target cannot be read.

## Relationship to existing specs

- [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md)
  remains the parent reliable-verifier contract. This spec refines how the
  bounded packet identifies the merged spec used for conformance, while keeping
  the `pass` / `fail` / `needs-human` schema and fail-closed provider behavior.
- [`0220-loopreview-new-spec-not-a-blocker.md`](0220-loopreview-new-spec-not-a-blocker.md)
  says a net-new doc-first spec's absence from `origin/<base>` is expected and
  non-blocking. This spec applies the same principle to body-cited sibling
  specs that are not the conformance target.
- [`0039-verification.md`](0039-verification.md) defines the verification gate,
  doc-first contract, and spec-driven verification for code PRs. This spec
  narrows 0039's document-discovery rule so conformance is driven by
  issue-level `implement per docs/...md` code references, not arbitrary prose
  citations.
- [`0535-loopreview-packet-truncation-reliability.md`](0535-loopreview-packet-truncation-reliability.md)
  addresses packet completeness when diff truncation hides required evidence.
  This spec addresses a separate false escalation where the packet was complete
  and the verifier decided pass, but a body-cited sibling spec lookup was
  incorrectly treated as blocking.

## Non-goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` change in this design-doc PR.
- No command behavior change in this design-doc PR.
- No new runtime dependency.
- No weakening of bounded packet limits, visible truncation markers,
  read-only verifier execution, timeout handling, structured verdict parsing,
  local-only attestation, relay obligations, or human merge authority.
- No change to worker or verifier attestation contracts.
- No permission to ignore genuinely missing acceptance-criteria evidence.
- No permission to pass a code PR when its actual conformance target is missing
  or unreadable.
