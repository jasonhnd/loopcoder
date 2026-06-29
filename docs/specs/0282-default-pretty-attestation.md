---
id: 282
title: Default Pretty Attestation On Both Surfaces And Conductor Relay
status: draft
date: 2026-06-30
issue: 282
pr: null
supersedes: []
superseded_by: []
---

# Default Pretty Attestation On Both Surfaces And Conductor Relay

This is a design-only spec for loopcoder 0.3.4. This PR must add only this
document: no Go code, no `.delivery.yml` change, no command behavior change,
and no new runtime dependency. Implementation belongs in separate code issues
after this spec is reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

This spec builds on
[`0214-human-readable-attestation.md`](0214-human-readable-attestation.md) and
is a peer of
[`0218-surface-worker-attestation.md`](0218-surface-worker-attestation.md). It
does not amend, supersede, or edit either accepted spec.

## Goal

Human-readable attestation must be reliably visible where operators actually
watch Worker and Verifier commands: the headless `dispatch`, `loopreview`, and
`dispatch-wave` stderr streams.

The implementation must use the single `AttestationRecord.Pretty()` renderer
from 0214 and must not change any machine contract, durable stamping contract,
or validation rule.

## Background

Spec 0214 added the `AttestationRecord.Pretty()` renderer and defined emoji and
plain ASCII forms for the same validated attestation record. That renderer is
already the human source of truth, but its display is gated behind `--pretty`,
`LOOPCODER_PRETTY`, or an interactive TTY.

loopcoder runs Workers and Verifiers headless. In the exact non-TTY contexts
where Conductors and operators need the readable block, the current display
logic suppresses it. They see canonical JSON and the dense stable
`[attestation] ...` header, but not the multi-line human form.

Spec 0218 separately requires `dispatch` to surface the Worker attestation in
stdout as exactly three records: the stable header, canonical JSON, and final
dispatch result JSON. This spec keeps that stdout contract intact and adds only
a default human diagnostic stderr display.

## Decisions

1. **Pretty emits by default on diagnostic stderr.** `dispatch`, `loopreview`,
   and `dispatch-wave` emit the 0214 `Pretty()` block to stderr by default. No
   flag, environment variable, or TTY is required. This refines 0214 Decision
   6: emitting to stderr is a human and diagnostic surface, not a durable
   stamping or machine-contract channel, so it is not gated on an explicit
   human request.
2. **Pretty mode follows the target unless forced.** The default stderr block
   uses emoji on an interactive TTY and plain ASCII on a non-TTY. This keeps
   headless logs cross-platform and readable while preserving the richer
   terminal form for interactive sessions.
3. **Stdout and durable artifacts are unchanged.** `dispatch` keeps the 0218
   three-record stdout contract: stable header, canonical JSON, then result
   JSON. `loopreview` keeps its verdict JSON on stdout and its stable
   `[attestation]` header on stderr. `dispatch-wave` keeps its text report on
   stdout, including the existing compact per-issue attestation line. PR
   bodies, merge commits, verifier JSON, attempt state, and other durable or
   machine artifacts are untouched.
4. **Pretty remains a non-parse-target.** Machine consumers must continue to
   parse canonical JSON or an existing stable header contract. They must not
   parse emoji, labels, alignment, whitespace, color, or the presence of a
   pretty block.
5. **`dispatch-wave` emits one block per issue.** For each dispatched issue,
   `dispatch-wave` relays or renders exactly one Worker pretty block on stderr.
   Its stdout text report remains unchanged and must not replace the compact
   per-issue attestation line with the pretty form.
6. **Suppression and force precedence is explicit.** Display decisions use this
   highest-to-lowest precedence:
   - suppression: `--no-pretty` or `LOOPCODER_NO_PRETTY` disables pretty
     output;
   - force: `--pretty` or `LOOPCODER_PRETTY` enables pretty output and requests
     emoji even on non-TTY output;
   - default: emit pretty output, using emoji on TTY and plain ASCII otherwise.

   When pretty output is shown, `NO_COLOR`, `LOOPCODER_PLAIN`, or
   `LOOPCODER_NO_EMOJI` still force plain ASCII mode. Suppression always beats
   force.
7. **The Conductor relays the pretty block verbatim.** `SKILL.md` and
   `AGENTS.md` should stop requiring hand-formatted attestation report lines
   once this behavior is implemented. The Conductor relays the pretty block
   from the command's stderr, one block for each Worker and each Verifier. The
   relayed block already carries provider, model with source, effort,
   permission, duration, token usage, and `verified`.
8. **Worker and Verifier records stay separate.** The Conductor must not merge
   Worker and Verifier pretty blocks, summarize them into one synthetic record,
   or sum their tokens. Existing fail-closed behavior remains unchanged.
9. **Token absence terminology follows the renderer.** 0218 and the current
   playbook describe absent token fields as `not reported`. The pretty
   renderer's actual output uses `unset` for empty optional presentation values
   and may omit unavailable token fields according to 0214's token formatting
   rules. This spec records that the relayed `Pretty()` block is the single
   source of truth for the human form. Editing `SKILL.md` to align terminology
   is a later documentation issue, not part of this PR.

## Surface Contracts

### Dispatch

`loopcoder dispatch` must emit the Worker pretty block to stderr by default.
The stdout contract from 0218 is unchanged and remains exactly:

1. `record.Header()` for the Worker attestation;
2. `record.CanonicalJSON()` for the same Worker attestation;
3. the final dispatch result JSON with the `attestation` object.

The final non-empty stdout line remains the dispatch result JSON. Warnings,
progress messages, and the pretty block belong on stderr.

### Loopreview

`loopcoder loopreview` must emit the Verifier pretty block to stderr by
default. The command continues to print its verdict JSON on stdout and the
stable `[attestation] ...` header on stderr. The pretty block is an additional
human diagnostic rendering of the same record, not a replacement for either
existing output.

### Dispatch-Wave

`loopcoder dispatch-wave` must emit one Worker pretty block on stderr for each
issue it dispatches. The stdout text report is unchanged, including any compact
per-issue attestation line that already exists. Aggregation must preserve each
issue's individual Worker attestation and must not collapse model, effort,
permission, duration, token usage, or `verified` across issues.

### Conductor Relay

The Conductor should relay each pretty block exactly as received from command
stderr. It should keep Worker and Verifier blocks separate and should preserve
ordering clear enough for a human to associate a block with its issue or PR
from the surrounding dispatch or review report.

The Conductor must not reformat the fields into a parallel attestation syntax.
The pretty renderer is the single human rendering source, while canonical JSON
and the stable header remain the machine and durable stamping contracts.

## Constraints

- No new runtime dependency.
- Canonical JSON is unchanged.
- The stable one-line `Header()` rendering is unchanged.
- Attestation validation is unchanged.
- `verified`, `model_source`, and token-usage rules from
  [`0146-attestation.md`](0146-attestation.md) are unchanged.
- Fail-closed behavior from 0146 is unchanged.
- Cross-platform behavior is required.
- Non-TTY default output is plain ASCII.
- Emoji appears only on an interactive TTY or when explicitly forced.
- Pretty output is never a parse target.
- Stdout and durable artifacts remain compatible with 0214 and 0218.

## Acceptance Criteria For Follow-On Code

- `dispatch` emits a Worker pretty block on stderr by default.
- `dispatch` keeps the 0218 stdout contract unchanged: stable header,
  canonical JSON, and final result JSON in that order.
- `loopreview` emits a Verifier pretty block on stderr by default.
- `loopreview` keeps verdict JSON on stdout and the stable `[attestation]`
  header on stderr.
- `dispatch-wave` emits one Worker pretty block per dispatched issue on stderr.
- `dispatch-wave` keeps its stdout text report unchanged, including the compact
  per-issue attestation line.
- `--no-pretty` and `LOOPCODER_NO_PRETTY` suppress pretty output and beat every
  force or default setting.
- `--pretty` and `LOOPCODER_PRETTY` force pretty output and request emoji even
  on non-TTY output unless plain mode is separately forced.
- The default emits pretty output without a flag or TTY, using emoji on TTY and
  plain ASCII otherwise.
- `NO_COLOR`, `LOOPCODER_PLAIN`, and `LOOPCODER_NO_EMOJI` force plain ASCII
  whenever pretty output is shown.
- The Conductor relays command stderr pretty blocks verbatim, one for each
  Worker and each Verifier, instead of hand-formatting attestation lines.
- Worker and Verifier token usage remains separate and is never summed.
- PR bodies, merge commits, verifier JSON, attempt state, and other durable or
  machine artifacts are not changed by the pretty default.
- No machine consumer parses the pretty block.
- No new runtime dependency is introduced.

## Acceptance Criteria For This PR

- This PR adds only `docs/specs/0282-default-pretty-attestation.md`.
- The spec has `status: draft` frontmatter.
- The spec records the default stderr pretty behavior, unchanged stdout and
  durable contracts, `dispatch-wave` per-issue behavior, flag and environment
  precedence, Conductor relay, and renderer terminology decision.
- The spec references 0214 and 0218.
- No accepted spec is edited.
- No Go code, `.delivery.yml`, or command behavior is changed.

## Non-Goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` change in this design-doc PR.
- No command behavior change in this design-doc PR.
- No new runtime dependency.
- No change to canonical JSON.
- No change to the stable one-line `Header()` output.
- No change to validation, `verified`, `model_source`, token-usage rules, or
  fail-closed behavior from 0146.
- No change to PR bodies, merge commits, verifier JSON, attempt state, or other
  durable machine artifacts.
- No parser contract for pretty output.
- No aggregate token accounting across Worker, Verifier, or Conductor records.
