---
id: 214
title: Human-Readable Attestation Rendering
status: accepted
date: 2026-06-28
issue: 214
pr: null
supersedes: []
superseded_by:
  - docs/specs/0306-local-only-attestation.md
---

# Human-Readable Attestation Rendering

This is a design-only spec for loopcoder 0.3.x. This PR must add only this
document: no Go code, no `.delivery.yml` change, no command behavior change,
and no new runtime dependency. Implementation belongs in separate code issues
after this spec is reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

This spec amends [`0146-attestation.md`](0146-attestation.md). It does not
replace the `AttestationRecord` schema, canonical JSON, validation rules, trust
semantics, or fail-closed behavior defined there.

## Goal

Attestation must remain stable for machines while becoming easier for humans to
read in terminal and chat output.

The current attestation system has two renderings:

- canonical JSON, which is the machine contract;
- a one-line greppable header, such as
  `[attestation] role=... provider=... model=...(...) effort=... perm=... action="..." exit=... dur=... tokens=... verified=...`.

Operators also need a human-friendly rendering with emoji and multiple aligned
lines. That rendering must not weaken durable artifact greppability, change the
canonical record, or create a new parser target.

## Decisions

1. **Canonical JSON is unchanged.** The `AttestationRecord` JSON defined by
   [`0146-attestation.md`](0146-attestation.md) remains the authoritative
   machine contract.
2. **The stable line remains available.** The existing one-line `Header()`
   rendering remains the recommended durable stamping line for PR bodies, merge
   commits, verifier records, and other artifacts that must stay greppable.
3. **Pretty rendering is separate.** A new human rendering may show emoji,
   line breaks, and aligned labels, but it is a separate rendering of the same
   validated record rather than a replacement for JSON or `Header()`.
4. **Durable artifacts keep JSON and the stable line.** PR bodies, merge
   commits, merge comments, verifier outputs consumed by tools, and any
   redirected output used for stamping must include canonical JSON plus the
   stable one-line header. Machine state that stores structured attestation
   records uses canonical JSON. No durable human-readable artifact may lose a
   greppable `[attestation] ...` line because a pretty form exists.
5. **Pretty output is never a parse target.** Machine consumers must parse
   canonical JSON, or the stable one-line header only where that compatibility
   contract already exists. They must not parse emoji, alignment, terminal
   color, glyphs, or pretty labels.
6. **Output is cross-platform and non-TTY safe.** Emoji and ANSI color are
   presentation only. The implementation must degrade to plain ASCII without
   changing attestation meaning, and it must not emit color or emoji into
   redirected/non-TTY output used for durable stamping unless a caller has
   explicitly requested human output.

## Rendering Contracts

### Canonical JSON

Canonical JSON remains exactly the schema from
[`0146-attestation.md`](0146-attestation.md):

- role;
- provider;
- model;
- model_source;
- effort;
- permission;
- action;
- exit_code;
- started_at;
- ended_at;
- duration_ms;
- usage;
- verified.

The JSON renderer is the only authoritative machine contract. Any new pretty
renderer must be generated from the same validated `AttestationRecord` and must
not add fields that become required machine state.

### Stable Header

The one-line header remains the durable human and grep contract:

```text
[attestation] role=worker provider=codex model=gpt-5.5(parsed) effort=xhigh perm=write action="implement issue #172" exit=0 dur=42s tokens=120/34|154 verified=true
```

The implementation should keep the existing header shape for 0.3.x. If a later
spec ever changes the header, it must preserve an equally stable, greppable
machine line and must define migration behavior for existing durable artifacts.

### Pretty Rendering

The pretty rendering is for humans reading an interactive terminal or chat. It
must contain the same canonical record fields, using aligned labels and an
unambiguous status line.

Preferred emoji form:

```text
✅ attestation verified
   role        worker
   provider    codex
   model       gpt-5.5 (source=parsed)
   effort      xhigh
   permission  write
   action      "implement issue #172"
   exit        0
   duration    42s (42000 ms)
   started     2026-06-28T00:00:00Z
   ended       2026-06-28T00:00:42Z
   tokens      input=120 output=34 total=154
   verified    true
```

If `exit_code` is non-zero, the leading glyph and label should show the failed
process result even when the record itself was successfully parsed:

```text
❌ attestation failed
   role        verifier
   provider    claude
   model       claude-haiku-4-5-20251001 (source=parsed)
   effort      unset
   permission  read-only
   action      "review PR #214"
   exit        1
   duration    3.2s (3200 ms)
   started     2026-06-28T00:00:00Z
   ended       2026-06-28T00:00:03.2Z
   tokens      input=2447 output=4947 total=unset
   verified    true
```

If `verified` is `false` and `exit_code` is zero, the leading glyph should show
that the record is self-reported rather than binary-verified:

```text
⚠️ attestation self-reported
   role        conductor
   provider    codex-cli
   model       gpt-5 (source=self-reported)
   effort      xhigh
   permission  orchestrate
   action      "merge PR #214"
   exit        0
   duration    1m12s (72000 ms)
   started     2026-06-28T00:00:00Z
   ended       2026-06-28T00:01:12Z
   tokens      total=18266
   verified    false
```

Status priority is:

1. `exit_code != 0`: failed process result.
2. `verified == true`: binary-verified record.
3. `verified == false`: self-reported record.

Color may be added only as redundant decoration. Color must not be the only way
to distinguish verified, failed, or self-reported states.

## Plain Fallback

When emoji is disabled, unsupported, or inappropriate for the output target,
the same record must render with plain ASCII and no ANSI color:

```text
attestation: verified
  role        worker
  provider    codex
  model       gpt-5.5 (source=parsed)
  effort      xhigh
  permission  write
  action      "implement issue #172"
  exit        0
  duration    42s (42000 ms)
  started     2026-06-28T00:00:00Z
  ended       2026-06-28T00:00:42Z
  tokens      input=120 output=34 total=154
  verified    true
```

The fallback must preserve every field shown by the emoji form. It must not
fall back to the one-line `[attestation] ...` header, because that header has a
separate durable stamping role and should remain visually distinct from the
pretty renderer.

## Field Formatting Rules

- `model` must include the source as `model-name (source=<model_source>)`.
- `action` must be quoted or escaped so embedded newlines, tabs, quotes, or
  other control characters cannot inject additional pretty-rendering lines.
- `duration` must show both a human duration and the canonical millisecond
  value, for example `42s (42000 ms)`.
- `started` and `ended` must use the canonical timestamp strings from the
  record.
- `tokens` must show only known usage values. It may show `total=unset` when
  split input and output counts exist without a total; it must not infer totals
  that the canonical record did not carry.
- Empty optional presentation values should render as `unset` rather than being
  omitted. Required canonical fields still follow the validation rules in
  [`0146-attestation.md`](0146-attestation.md).

## Usage Split

Use the renderings as follows:

| Surface | Required rendering |
|---|---|
| PR body created by `dispatch` | Stable one-line header plus fenced canonical JSON. |
| Merge commit body or merge comment | Stable one-line header plus canonical JSON. |
| Attempt state, verifier JSON, state branch, or other machine data | Canonical JSON. |
| Terminal output intended for a human at an interactive TTY | Pretty rendering may be shown. |
| Chat progress intended for a human, not used as durable machine state | Pretty rendering may be shown. |
| Redirected stdout or files used for stamping durable artifacts | Stable one-line header plus canonical JSON, not pretty by default. |

`loopcoder attest` is the most sensitive command because conductors copy its
output into durable artifacts. Its default or stamping-oriented output must
remain safe for redirection and durable use. A future implementation may add an
explicit human-output flag or TTY-aware companion display, but it must not make
redirected stamping output depend on emoji, color, terminal width, or alignment.

## Cross-Platform Requirements

The eventual implementation must use cross-platform Go and add no runtime
dependency.

Pretty rendering must:

- work on Windows, macOS, and Linux;
- avoid ANSI color when the target is not an interactive terminal;
- provide an ASCII fallback for environments where emoji width or font support
  is unreliable;
- avoid terminal-width-dependent wrapping for the field labels and values in
  the required examples;
- keep user-provided strings escaped so logs, PR text, redirected files, and
  terminals are not corrupted by control characters.

Terminal color support may be detected with standard-library facilities and
simple environment checks only. Color is optional for 0.3.x; the plain fallback
is mandatory.

## Process And Follow-Up Issues

This design PR is the only deliverable for issue #214.

After this spec merges, separate code issues should be filed in this order:

1. **Pretty renderer API:** add a human renderer for `AttestationRecord` with
   emoji and ASCII modes, unit tests for all status states, usage variants,
   timestamp fields, and escaped actions.
2. **Output surface integration:** wire the pretty renderer into interactive
   terminal and chat surfaces while preserving canonical JSON plus `Header()` for
   durable artifacts, redirected output, PR bodies, merge commits, verifier
   records, and stamping workflows.
3. **Reference documentation update:** update user-facing reference docs after
   the behavior exists, including examples that show the usage split and the
   plain fallback.

Each code issue must reference this merged spec and
[`0146-attestation.md`](0146-attestation.md). Code issues must preserve 0146's
`verified`, `model_source`, token usage, validation, and fail-closed semantics.

## Acceptance Criteria For Code Issues

- Canonical JSON remains byte-for-byte compatible with the existing
  `AttestationRecord` field names and semantics.
- The existing one-line header remains available for durable greppable
  artifacts.
- Pretty rendering includes role, provider, model with source, effort,
  permission, action, exit code, duration in human and millisecond forms,
  started timestamp, ended timestamp, token usage, and verified status.
- Pretty rendering has emoji and ASCII modes, with no ANSI color in non-TTY
  output by default.
- Machine consumers are not changed to parse the pretty rendering.
- Durable human-readable output paths keep using canonical JSON plus the stable
  one-line header.
- Tests cover verified, failed, and self-reported status lines; total-only and
  split-token usage; action escaping; and no-emoji fallback.
- No new runtime dependency is introduced.

## Non-Goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` change in this design-doc PR.
- No command behavior change in this design-doc PR.
- No new runtime dependency.
- No replacement of canonical JSON as the machine contract.
- No replacement of the stable one-line header for durable greppable stamping.
- No requirement for machine consumers to parse emoji, color, or aligned text.
- No weakening of attestation validation, token usage requirements,
  `verified` / `model_source` semantics, or fail-closed behavior from
  [`0146-attestation.md`](0146-attestation.md).

## Relationship To Existing Specs

- [`0146-attestation.md`](0146-attestation.md) defines the
  `AttestationRecord`, canonical JSON, one-line header, trust marker, token
  usage semantics, and fail-closed behavior. This spec amends 0146 by adding a
  separate human-only rendering.
- [`0192-delivery-guardrails.md`](0192-delivery-guardrails.md) consumes
  attestation usage for budget evidence; it must continue to consume canonical
  records, not pretty output.
- [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md)
  requires complete Verifier attestation; verifier automation must continue to
  use canonical records and stable machine output.
- [`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md)
  is a peer 0.3.x spec. Future release documentation should describe pretty
  rendering only after the follow-on implementation issues merge.
