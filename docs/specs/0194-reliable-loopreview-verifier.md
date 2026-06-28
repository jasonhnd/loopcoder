---
id: 194
title: Reliable loopreview Verifier
status: draft
date: 2026-06-28
issue: 194
pr: null
supersedes: []
superseded_by: []
---

# Reliable loopreview Verifier

This is a design-only spec for 0.3.x hardening of the existing `loopreview`
command. This PR must add the design record only: no Go code, no runtime
dependency, no `.delivery.yml` policy change, and no provider default change.
Implementation belongs in separate code issues after this spec is reviewed and
merged per [`docs/PROCESS.md`](../PROCESS.md).

## Goal

`loopcoder loopreview` must become reliable enough for the conductor to use as a
real delivery gate instead of an experimental advisory. A reliable verifier
means:

- bounded review inputs;
- a read-only, headless-safe provider invocation;
- completion before the configured verifier timeout on representative PRs;
- a deterministic-enough `pass`, `fail`, or `needs-human` verdict;
- evidence that cites changed files, issue/spec criteria, and any ambiguity;
- complete Verifier attestation when the provider can run.

This is patch-level 0.3.x hardening. The autonomous conductor tick is still out
of scope; there is no separate held 0.4.0 line for this design.

## Why This Blocks Autonomy

`loopreview` exists and already has a timeout safety net. Attestation also
exists. The remaining blocker is reliability: if an unattended future tick calls
a verifier that usually times out, hangs on an interactive mode, cannot run
headlessly, or emits malformed output, the loop only produces `needs-human`
records. That is safer than a false pass, but it is not autonomous delivery.

This spec hardens the verifier contract before any background or scheduled loop
is considered.

## Diagnosis

The current failure class has four likely causes. The code issue must measure
and mitigate all four; it must not assume a timeout increase is a fix.

1. **Prompt and diff size are unbounded.** The current review prompt includes
   the full PR diff, changed-file list, issue body, and merged spec content.
   Large diffs, generated files, vendored files, or long specs can consume the
   model's useful context and slow the verifier before it reaches judgment.
   The observed `claude` failure mode was a verifier run exceeding a 180s
   timeout and returning `needs-human`; this is the regression target the code
   issue must make reproducible and then avoid on representative PRs.
2. **Read-only tool work is unbudgeted.** `loopreview` runs the provider in a
   read-only worktree, but the verifier prompt does not define a bounded
   inspection budget. A provider can spend the whole timeout reading or grepping
   instead of returning the structured verdict.
3. **Model and effort are inherited.** This is correct and must remain correct:
   loopcoder does not choose model or reasoning effort for the user. It also
   means a verifier can inherit a slower local setting. Reliability must come
   from bounded work and timeout enforcement, not from silently changing model
   or effort.
4. **Plan-mode is unsafe for headless verification.** Provider modes that wait
   for an interactive plan approval can hang in unattended runs. The verifier
   must use a read-only tool allowlist plus a process timeout, not an
   interactive plan or approval mode.

## Reliable Verifier Contract

The implementation issue must make `loopreview` build a bounded review packet
before invoking the provider. The packet is the contract the LLM reviews.

The packet must include:

- PR number, title, head branch, base branch, and linked issue number when
  available;
- changed-file list within its budget, with total file count and an explicit
  marker if the list was truncated;
- issue title and a bounded issue-body excerpt;
- merged spec path from `origin/<base-branch>` and a bounded spec excerpt;
- diff excerpts selected by changed file, with omitted-byte or omitted-line
  counts whenever the full diff cannot fit;
- the exact verdict schema;
- instructions to return `needs-human` when the bounded packet is insufficient
  to decide safely.

The packet must never silently omit evidence. Every truncation must be visible
in the prompt and in the expected evidence string. If omitted material could
hide a relevant acceptance criterion or a risky changed file, the verifier must
return `needs-human`, not `pass`.

## Input Bounds

The first implementation should use fixed byte or rune budgets in Go constants,
covered by tests. Exact values may be tuned in the code issue, but the behavior
must be stable and provider-neutral:

- cap total diff text;
- cap each individual file patch;
- cap issue-body text;
- cap merged-spec text;
- cap total prompt size;
- always reserve space for the verdict schema and review instructions.

Generated or low-signal files should be deprioritized in the diff excerpt when
they can be recognized from paths or patch headers. Source files, tests,
configuration, docs/spec references, and files mentioned by the issue or spec
should be preferred. If the selected excerpts cannot cover the risky files, the
packet should say so and the safe verdict is `needs-human`.

## Read-Only Headless Invocation

`loopreview` must remain read-only.

Provider invocation requirements:

- `codex`: use `codex exec` in read-only sandbox mode.
- `claude`: use `claude --print` with a read-only tool allowlist such as
  `Read Grep Glob`; do not use plan mode or any approval mode that expects an
  interactive user.
- `gemini`: use the existing headless read-only settings path only after the
  Gemini auth gap is resolved; until then it remains unverified.

The verifier may inspect files only through provider read-only capability. It
must not commit, push, write comments, mutate the PR branch, or edit files in
the review worktree. If a provider invocation exits non-zero, times out,
returns malformed JSON, lacks complete attestation, or appears to require
interactive input, `loopreview` returns `needs-human`.

## Tool Budget

The LLM should be able to decide from the review packet alone for normal PRs.
Read-only tools are a backup for targeted confirmation, not an open-ended
research pass.

The provider prompt must state a small inspection budget in provider-neutral
terms:

- prefer the review packet over exploratory reads;
- inspect only files that are changed, cited by the issue/spec, or necessary to
  confirm a finding;
- stop and return `needs-human` when deciding would require broad repository
  exploration;
- return the structured verdict before the timeout instead of continuing to
  search for more confidence.

Where provider logs expose tool-call counts, tests should assert the budget.
Where they do not, the bounded packet plus hard timeout is the enforcement
mechanism for 0.3.x.

## Verdict Semantics

The existing JSON schema remains the wire contract:

```json
{
  "verdict": "pass | fail | needs-human",
  "findings": [
    { "severity": "...", "file": "...", "note": "..." }
  ],
  "evidence": "...",
  "spec_conformance": "pass | fail | not-applicable"
}
```

Verdict rules:

- `pass` means the bounded evidence shows the PR satisfies the linked issue and
  merged spec, no blocking findings remain, and any truncation is irrelevant to
  the decision.
- `fail` means there is a concrete defect, missing acceptance criterion,
  regression, unrelated change, or test/spec gap that a worker can fix.
- `needs-human` means the verifier cannot decide safely because evidence is
  incomplete, the spec is ambiguous, provider infrastructure failed, output or
  attestation is incomplete, or the review would require work beyond the bounded
  input/tool budget.

`spec_conformance` must be `pass` or `fail` for code issues with an available
merged spec. It may be `not-applicable` only for documentation-only work with no
code conformance target, missing/unreadable spec state, or provider failure that
already forced `needs-human`.

## Provider Strategy

Provider verification is an earned status, not a registry side effect. A
provider is a verified verifier only when a real headless `loopreview` run
proves all of these:

- the provider completes under the configured timeout on a representative
  loopcoder PR;
- the invocation is read-only and headless-safe;
- output parses against the shared verdict schema;
- attestation includes provider, parsed model, effort when available, timing,
  permission `read-only`, and token usage;
- the verdict cites issue/spec evidence and changed files.

The primary verified verifier target for the default loopcoder configuration is
`claude`, because the default worker provider is `codex` and reviewer should
normally differ from worker. The current `claude` timeout behavior is the thing
this spec is meant to fix: bounded inputs, no plan mode, a read-only allowlist,
and the hard timeout are required before `claude` may be called verified for
`loopreview`.

`codex` remains verifier-capable and should pass the same contract. It is the
preferred verifier when the worker provider is `claude`. When `codex` reviews a
`codex` worker PR, loopcoder may proceed with the existing reviewer-not-worker
advisory, but that is not the preferred default for author-bias reduction.

`gemini` remains experimental/unverified for `loopreview` until issue #188 or a
successor resolves headless authentication in the target environment. A Gemini
auth or headless-start failure must be reported as `needs-human` with evidence;
it must not be hidden behind another provider fallback.

## Completion And Timeout

The hard timeout remains a safety net, not the success mechanism. A reliable
verifier should finish ordinary loopcoder PR reviews well before the configured
timeout because the review packet and allowed inspection work are bounded.

The code issue must add at least one deterministic timeout test using a fake
provider that blocks until context cancellation, and at least one real or
documented smoke procedure for the verified provider. The smoke evidence should
record elapsed time, provider, parsed model/effort, token usage, verdict, and
whether any inputs were truncated.

Because the known failure exceeded 180s, the smoke procedure should include a
representative run with `--timeout 180s` even if the CLI default timeout is
longer. Passing only by raising the timeout does not satisfy this spec.

If a provider hits the timeout, the result remains `needs-human`. The conductor
must not treat timeout-to-`needs-human` as a merge-eligible state.

## Configuration

No new `.delivery.yml` keys are required for this spec. Existing role slots and
timeout flags remain sufficient for 0.3.x:

```text
loopcoder loopreview --repo . --pr-number <pr> --provider <verifier> --timeout <duration>
```

The inherit-by-default rule remains unchanged. loopcoder must not pick a model
or effort level for reliability. Operators may still pass explicit provider
overrides through existing command/config surfaces.

## Code-Slice Plan After Merge

After this spec merges, separate code issues should be filed in this order:

1. **Bounded review packet:** add the packet builder, truncation markers,
   prompt rewrite, and unit tests for diff/spec/issue budgets.
2. **Headless verifier hardening:** assert provider argv for read-only
   verifier mode, forbid plan/approval modes for verification, keep timeout
   cancellation and malformed output as `needs-human`, and test fake-provider
   timeout behavior.
3. **Provider verification proof:** run and document real `loopreview` smoke
   evidence for `claude` and `codex`; keep `gemini` unverified until the
   headless auth issue is fixed; update living reference docs only when the
   proof exists.

These are code or reference-documentation issues, not part of this design PR.

## Acceptance Criteria For Code Issues

- `loopreview` constructs a bounded review packet and never passes an
  unbounded full diff, issue body, or spec body to the provider.
- Truncated inputs are explicitly marked with omitted counts.
- The verifier prompt tells providers to return `needs-human` when omitted
  evidence prevents a safe decision.
- Provider read-only invocations are covered by argv/unit tests.
- Claude verifier invocation uses a read-only tool allowlist and does not use
  plan mode.
- Timeout, provider error, non-zero exit, malformed verdict JSON, unreadable
  spec, and incomplete attestation all produce `needs-human`.
- `pass` requires evidence and spec conformance; it is never emitted for a
  materially truncated or ambiguous review packet.
- A real or documented smoke run identifies the verified verifier provider for
  the current release line.
- Gemini remains unverified and fails closed until headless auth is proven.
- No new runtime dependencies are introduced.
- The implementation remains cross-platform Go.

## Non-Goals

- No autonomous tick, daemon, cron, or cloud scheduler.
- No automatic merge.
- No weakening of doc-first, human merge, or reviewer-not-worker guidance.
- No model or effort auto-selection.
- No provider secret or auth management inside loopcoder.
- No new runtime dependency.
- No Go implementation in this design-doc PR.

## Relationship To Existing Specs

- [`0039-verification.md`](0039-verification.md) defines the verification gate
  and verdict model. This spec hardens the LLM `loopreview` portion of that
  gate.
- [`0131-multi-provider-roles.md`](0131-multi-provider-roles.md) defines the
  provider registry, Worker/Verifier roles, and read-only verifier command.
- [`0146-attestation.md`](0146-attestation.md) defines the Verifier attestation
  required for a normal pass/fail verdict.
- [`0192-delivery-guardrails.md`](0192-delivery-guardrails.md) deliberately
  avoids depending on reliable LLM verifier output; this spec closes that gap
  for later autonomy work.
