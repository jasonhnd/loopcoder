---
id: 218
title: Surface Worker Attestation in Dispatch Output and Report
status: accepted
date: 2026-06-29
issue: 218
pr: null
supersedes: []
superseded_by:
  - docs/specs/0306-local-only-attestation.md
---

# Surface Worker Attestation in Dispatch Output and Report

This is a design-only spec for loopcoder 0.3.x. This PR must add only this
document: no Go code, no `.delivery.yml` change, no command behavior change,
and no new runtime dependency. Implementation belongs in separate issues after
this spec is reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

## Goal

Every dispatched Worker run must expose the same attestation facts to the
Conductor that `loopcoder loopreview` exposes for the Verifier: provider, real
model, reasoning depth or effort, permission, duration, exit code, token usage,
and the `verified` trust marker.

The Conductor must be able to report the Worker model, effort, duration, and
input/output/total token usage from `loopcoder dispatch` output alone. It must
not need to open the worker-created PR body only to recover the Worker
attestation.

## Problem

[`0146-attestation.md`](0146-attestation.md) defines one
`AttestationRecord` schema for Worker, Verifier, and Conductor invocations. The
current Worker path already builds a complete Worker attestation record and
stamps it into the PR body through the stable `[attestation] ...` header plus
canonical JSON.

The gap is the `dispatch` command's own result. A successful
`loopcoder dispatch` currently prints a compact JSON summary shaped like:

```json
{"ok":true,"issue":218,"branch":"loop/issue-218","run_id":"run-218","pr":"https://github.com/owner/repo/pull/999","summary":"Worker summary","attempt_path":".loopcoder/runs/run-218/workers/job-218-1.attempt.json","status":"succeeded","exit_code":0,"log_bytes":12345}
```

That summary has no Worker attestation field. As a result, the Conductor can
directly see the Verifier attestation from `loopreview` output, but cannot
directly see the Worker attestation without opening the PR body. This makes the
final report asymmetric and hides the Worker model, effort, and token facts
that operators need.

## Decisions

1. `loopcoder dispatch` must surface the Worker attestation in its own output,
   symmetrically with the Verifier attestation surfaced by `loopreview`.
2. The dispatch result JSON must include the same `AttestationRecord` under an
   `attestation` field. It must not add parallel top-level fields such as
   `worker_model`, `worker_effort`, or `worker_tokens`; those would fork the
   schema.
3. Dispatch stdout must include the stable one-line `Header()` rendering and
   the `CanonicalJSON()` rendering of the Worker attestation, in addition to
   the dispatch result JSON.
4. Worker token usage must capture `input_tokens` and `output_tokens` when the
   provider exposes both. If the provider exposes only a total, record
   `total_tokens` only. Never infer or fabricate an input/output split.
5. The Conductor playbook in `SKILL.md` and the Codex entrypoint in `AGENTS.md`
   must require reporting the Worker attestation per dispatched worker:
   provider, model, effort, permission, duration, input tokens, output tokens,
   total tokens, and `verified`.
6. All rendering must reuse the existing 0146 `AttestationRecord`,
   `Header()`, and `CanonicalJSON()` contract. When issue 0214's
   human-readable attestation rendering lands, that pretty form may be used for
   interactive display, but durable artifacts and machine output must still
   keep the stable header and canonical JSON.
7. Fail-closed behavior is unchanged. An incomplete or unparseable Worker
   attestation blocks the Worker PR per 0146. Surfacing the attestation must not
   weaken validation or allow delivery with missing required fields.
8. Inherit-by-default model and effort behavior, the human merge gate, and the
   reviewer-not-worker rule from [`0131-multi-provider-roles.md`](0131-multi-provider-roles.md)
   are unchanged.

## Dispatch Output Contract

For every successful `loopcoder dispatch` result that creates a PR, stdout must
contain exactly these newline-terminated records in this order:

1. The Worker attestation header from `record.Header()`.
2. The Worker attestation canonical JSON from `record.CanonicalJSON()`.
3. The dispatch result JSON.

Example:

```text
[attestation] role=worker provider=codex model=gpt-5.5(parsed) effort=xhigh perm=write action="implement issue #218" exit=0 dur=42s tokens=2447/4461|6908 verified=true
{"role":"worker","provider":"codex","model":"gpt-5.5","model_source":"parsed","effort":"xhigh","permission":"write","action":"implement issue #218","exit_code":0,"started_at":"2026-06-29T00:00:00Z","ended_at":"2026-06-29T00:00:42Z","duration_ms":42000,"usage":{"input_tokens":2447,"output_tokens":4461,"total_tokens":6908},"verified":true}
{"ok":true,"issue":218,"branch":"loop/issue-218","run_id":"run-218","pr":"https://github.com/owner/repo/pull/999","summary":"Worker summary","attempt_path":".loopcoder/runs/run-218/workers/job-218-1.attempt.json","status":"succeeded","exit_code":0,"log_bytes":12345,"attestation":{"role":"worker","provider":"codex","model":"gpt-5.5","model_source":"parsed","effort":"xhigh","permission":"write","action":"implement issue #218","exit_code":0,"started_at":"2026-06-29T00:00:00Z","ended_at":"2026-06-29T00:00:42Z","duration_ms":42000,"usage":{"input_tokens":2447,"output_tokens":4461,"total_tokens":6908},"verified":true}}
```

The final non-empty stdout line remains the dispatch result JSON. Conductors and
machine consumers that need the dispatch summary should parse the last line.
Conductors that only need the Worker attestation may read either the stable
header line or the `attestation` object in the final JSON result.

The attestation canonical JSON line is the exact machine rendering of the same
record that appears in the final result's `attestation` field and in the PR
body. It is not pretty-printed and is not wrapped in a Markdown fence on stdout.

Warnings, progress messages, and non-contract diagnostics belong on stderr, not
between the three stdout records. Future human-readable rendering from issue
0214 may add an interactive display only if it does not replace or obscure this
three-record contract.

If a dispatch attempt runs a provider and can produce a complete Worker
attestation but then fails later in delivery, such as during commit, push, or PR
creation, the implementation should still surface the complete Worker
attestation before returning the existing non-zero dispatch failure. If the
Worker attestation itself is incomplete or unparseable, dispatch must fail
closed before PR creation and report the validation failure; it must not emit a
record that looks verified.

## Dispatch Result JSON

The existing success fields remain:

- `ok`
- `issue`
- `branch`
- `run_id`
- `pr`
- `summary`
- `attempt_path`
- `status`
- `exit_code`
- `log_bytes`

The new field is:

- `attestation`: the Worker `AttestationRecord` object.

No additional schema is introduced for Worker attestation. The `attestation`
object must validate under the same 0146 rules as the record stamped into the
PR body.

`dispatch-wave`, recovery paths, and any foreground report that aggregate
multiple dispatch results must preserve the per-worker `attestation` object for
each dispatched issue. They must not collapse multiple Worker attestations into
a single combined model, effort, or token value.

## Token Usage Rule

Worker adapters must parse token usage from provider output without inventing
missing facts:

- If the provider exposes both input and output token counts, set
  `usage.input_tokens` and `usage.output_tokens`.
- If the provider also exposes a total token count, set `usage.total_tokens`.
- If the provider exposes only a total token count, set `usage.total_tokens`
  and leave `usage.input_tokens` and `usage.output_tokens` absent.
- If the provider exposes input/output counts but no total, keep the split and
  leave `usage.total_tokens` absent.
- Never split a total-only value into guessed input and output counts.
- Never infer input or output counts from duration, log size, summary text, or
  provider-independent heuristics.

The stable header's existing token formatting remains:

- `tokens=<input>/<output>|<total>` when split and total are present.
- `tokens=<input>/<output>` when only split usage is present.
- `tokens=<total>` when only total usage is present.

The Conductor report must render unavailable fields explicitly, for example
`input: not reported`, `output: not reported`, or `total: not reported`. It
must not hide a total-only Worker behind a fabricated split.

Provider-specific examples:

- A provider that reports `2447` input tokens and `4461` output tokens must
  surface those values as the split.
- A provider such as current `codex` output that reports `tokens=102585` total
  only must surface `total_tokens: 102585` and leave input/output absent.
- A provider such as `gemini` may surface split and/or total only when the real
  output exposes those fields in a parseable form.

## Conductor Reporting

`SKILL.md` and `AGENTS.md` reporting guidance must be updated after this spec
merges so that every worker-dispatch progress report and final summary includes
the Worker attestation facts for each dispatched issue.

The report should include:

- issue number and PR;
- Worker provider;
- Worker model and `model_source`;
- Worker reasoning depth or effort;
- Worker permission;
- Worker duration;
- token usage as input, output, and total, with unavailable fields shown as
  `not reported`;
- `verified`.

A compact report line can follow this shape:

```text
Worker #218 -> PR #999: provider=codex model=gpt-5.5(parsed) effort=xhigh permission=write duration=42s tokens input=2447 output=4461 total=6908 verified=true
```

For a total-only provider:

```text
Worker #218 -> PR #999: provider=codex model=gpt-5.5(parsed) effort=xhigh permission=write duration=42s tokens input=not reported output=not reported total=102585 verified=true
```

The Verifier report remains symmetric: the Conductor should continue to report
Verifier provider, model, effort, permission, duration, token usage, and
`verified` from `loopreview` output. Worker and Verifier records remain
separate. The Conductor must not add their token counts together unless a
future spec defines an explicit aggregate run-cost report.

## Validation And Failure Behavior

The Worker attestation must be validated before the implementation prints the
successful dispatch result JSON and before it opens or reports a deliverable PR.
Validation is the same 0146 validation used for the PR body.

Missing model, missing permission, missing usage, malformed timestamps,
negative duration, negative token counts, absent `verified`, or invalid enum
values remain delivery blockers. A PR whose Worker attestation cannot validate
must not be opened.

Surfacing the attestation in stdout is an additional reporting requirement, not
a fallback path. It must not allow a Worker PR to be delivered when the
attestation would have failed 0146.

## Relationship To Existing Specs

- [`0131-multi-provider-roles.md`](0131-multi-provider-roles.md) defines the
  Conductor, Worker, and Verifier role split, inherit-by-default model and
  effort behavior, and reviewer-not-worker guidance. This spec does not change
  those rules.
- [`0146-attestation.md`](0146-attestation.md) defines the shared
  `AttestationRecord`, `Header()`, `CanonicalJSON()`, `verified`,
  `model_source`, token usage validation, and fail-closed behavior. This spec
  extends the Worker surface area that carries the same record.
- Issue 0214 defines human-readable attestation rendering. When it lands, that
  rendering may improve terminal readability, but it must not replace the
  stable header or canonical JSON required here.
- `SKILL.md` and `AGENTS.md` must be updated by a follow-up issue so conductor
  reports include Worker attestation facts per dispatch.
- [`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md)
  is a 0.3.x peer. This spec does not change release, installation, upgrade, or
  runtime dependency policy.

## Follow-Up Issues

After this spec merges, implementation should be split in this dependency
order:

1. **Dispatch result attestation surface:** add the Worker `AttestationRecord`
   to `worker.Result`, `recovery.DispatchResult`, dispatch stdout rendering,
   and dispatch tests. Print `Header()`, `CanonicalJSON()`, then the final
   dispatch result JSON with an `attestation` field.
2. **Worker token split parsing:** update provider parsers and tests so Worker
   usage captures input/output tokens where provider output exposes them,
   preserves total-only usage where that is all the provider exposes, and never
   fabricates a split.
3. **Aggregated dispatch propagation:** update `dispatch-wave`, recovery, and
   report aggregation paths to preserve and surface each worker's individual
   `attestation` object.
4. **Conductor reporting docs:** update `SKILL.md` and `AGENTS.md` reporting
   guidance so progress reports and final summaries include Worker provider,
   model, effort, permission, duration, input/output/total token usage, and
   `verified` for every dispatched worker.
5. **0214 rendering integration:** after issue 0214 lands, optionally use the
   human-readable attestation rendering for interactive display while keeping
   the stable header and canonical JSON in stdout, PR bodies, and durable
   artifacts.

## Non-Goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` change in this design-doc PR.
- No command behavior change in this design-doc PR.
- No new runtime dependency.
- No fork of the 0146 `AttestationRecord` schema.
- No change to model or effort inheritance.
- No change to the human merge gate.
- No change to reviewer-not-worker guidance.
- No aggregate token accounting across Worker, Verifier, and Conductor roles.
