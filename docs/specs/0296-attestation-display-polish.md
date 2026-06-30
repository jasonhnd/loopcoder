---
id: 296
title: Human-Readable Attestation Display Polish
status: draft
date: 2026-06-30
issue: 296
pr: null
supersedes: []
superseded_by: []
---

# Human-Readable Attestation Display Polish

This is a design-only spec for loopcoder 0.3.5. This PR must add only this
document: no Go code, no `.delivery.yml` change, no command behavior change,
and no new runtime dependency. Implementation belongs in a separate code issue
after this spec is reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

This spec builds on
[`0146-attestation.md`](0146-attestation.md) and
[`0214-human-readable-attestation.md`](0214-human-readable-attestation.md). It
does not amend, supersede, or edit either accepted spec. It refines only the
human pretty rendering produced from the existing validated attestation record.

## Goal

Attestation pretty output should be easier for humans to scan while preserving
the exact machine contracts already used by loopcoder.

The follow-on implementation must polish only the display layer in
`internal/attestation`. Canonical JSON from 0146, the stable
`[attestation] ...` header, validation, fail-closed behavior, and stored
attestation field values are unchanged. There is no 0146 schema change.

## Decisions

1. **Pretty `provider` shows the vendor.** The pretty `provider` line maps the
   canonical adapter name to its vendor:
   - `codex` renders as `OpenAI`;
   - `claude` renders as `Anthropic`;
   - `gemini` renders as `Google`;
   - any unknown adapter renders as its own name unchanged.

   The canonical JSON `provider` field remains the adapter value, such as
   `codex`, `claude`, or `gemini`. Machine consumers and durable artifacts must
   continue to use that unchanged adapter value.
2. **Pretty output adds a `tool` line.** The new `tool` display line renders
   the adapter or CLI name from the canonical `provider` value. This preserves
   direct provenance of which CLI ran after the human-facing `provider` line is
   changed to the vendor name.
3. **Model source wording becomes human terminology.** Pretty `model` output
   renders `model_source=parsed` as `(detected)` and
   `model_source=self-reported` as `(self-reported)`. Canonical
   `model_source` values remain `parsed` and `self-reported`.
4. **Pretty timestamps use host local time.** The pretty `started` and `ended`
   lines render in the host local timezone as `YYYY-MM-DD HH:MM:SS TZ`, for
   example `2026-06-30 14:25:21 JST`. The pretty renderer drops sub-second
   precision. Canonical JSON keeps the existing UTC RFC3339Nano timestamp
   values from the record.
5. **Pretty duration shows seconds, not milliseconds.** The pretty `duration`
   line renders as `<human> (<seconds> s)` with one decimal place, for example
   `7m53.9s (473.9 s)`. The compact human value also keeps a one-decimal
   seconds component, such as `2m7.0s`. It no longer renders the 0214 `(N ms)`
   suffix.
6. **Pretty tokens use grouped decimal output.** Token counts in the pretty
   `tokens` line use thousands separators, for example `165,268`.
   - When input and output are present and total is absent, pretty output
     displays a derived `total=<input+output>`.
   - When only total is present, such as current `codex` output verified by
     probe, pretty output displays `total=<value>` only.
   - When input, output, and total are all present, pretty output displays all
     three known values.

   The derived total is a presentation value only. It must not be stored,
   serialized into canonical JSON, written into attempt state, or backfilled
   into the stable header.
7. **All pretty fields always render.** The pretty block remains complete and
   always renders these labels in this order: `role`, `provider`, `tool`,
   `model`, `effort`, `permission`, `action`, `exit`, `started`, `ended`,
   `duration`, `tokens`, and `verified`. The Conductor relays the block
   verbatim with no truncation, summary, field removal, or alternate
   hand-formatting.
8. **Verifier effort is explicitly configured later.** The loopcoder repo
   `.delivery.yml` should set `verifier.reasoning_effort: max` so Verifier
   pretty output reports a depth instead of `unset`. The `claude --effort`
   option accepts `low`, `medium`, `high`, `xhigh`, and `max`, and `max` is the
   explicit user-requested value. This `.delivery.yml` change ships only in the
   follow-on code PR, not in this design PR.
9. **Machine contracts do not change.** Canonical JSON, the 0146 schema and
   validation rules, the stable one-line header, stored attestation values, and
   fail-closed behavior are unchanged.

## Target Pretty Output

Worker total-only usage should render like:

```text
✅ attestation verified
   role        worker
   provider    OpenAI
   tool        codex
   model       gpt-5.5 (detected)
   effort      xhigh
   permission  write
   action      "implement issue #293"
   exit        0
   started     2026-06-30 14:25:21 JST
   ended       2026-06-30 14:33:15 JST
   duration    7m53.9s (473.9 s)
   tokens      total=165,268
   verified    true
```

Verifier split usage should render like:

```text
✅ attestation verified
   role        verifier
   provider    Anthropic
   tool        claude
   model       claude-haiku-4-5-20251001 (detected)
   effort      max
   permission  read-only
   action      "review PR #295"
   exit        0
   started     2026-06-30 14:33:51 JST
   ended       2026-06-30 14:35:58 JST
   duration    2m7.0s (127.0 s)
   tokens      input=2,447  output=9,844  total=12,291
   verified    true
```

The ASCII fallback from 0214 must preserve the same labels, order, values, and
display rules. Only the leading status decoration and spacing prefix may differ
according to the existing emoji/plain mode contract.

## Rendering Rules

The follow-on implementation must update only the pretty renderer.

### Provider And Tool

The canonical `AttestationRecord.Provider` value remains the adapter key. Pretty
rendering derives two display fields from it:

| Canonical `provider` | Pretty `provider` | Pretty `tool` |
|---|---|---|
| `codex` | `OpenAI` | `codex` |
| `claude` | `Anthropic` | `claude` |
| `gemini` | `Google` | `gemini` |
| `custom-cli` | `custom-cli` | `custom-cli` |

The mapping is display-only. It must not change adapter lookup, provider
configuration, stored state, canonical JSON, the stable header, or any existing
machine parser.

### Model Source

Pretty rendering maps source values as follows:

| Canonical `model_source` | Pretty suffix |
|---|---|
| `parsed` | `(detected)` |
| `self-reported` | `(self-reported)` |

The pretty renderer must not introduce a new canonical source value or rewrite
the stored record.

### Time And Duration

Pretty `started` and `ended` render from the canonical timestamps after
converting to the host local timezone. The format is:

```text
YYYY-MM-DD HH:MM:SS TZ
```

The renderer truncates or rounds away sub-second display precision by emitting
whole seconds only. The canonical JSON continues to carry the original UTC
RFC3339Nano values.

Pretty `duration` renders the existing canonical duration in milliseconds in
two human forms:

```text
<compact human duration with one-decimal seconds> (<seconds with one decimal> s)
```

Examples:

```text
42.0s (42.0 s)
2m7.0s (127.0 s)
7m53.9s (473.9 s)
```

This is display-only. The canonical `duration_ms` field and stable header
duration remain unchanged.

### Tokens

Pretty token counts render with thousands separators. Values may be present in
the canonical usage object as input, output, total, or input plus output without
total.

Pretty rendering must follow these rules:

| Canonical usage fields | Pretty tokens |
|---|---|
| total only | `total=<total>` |
| input and output only | `input=<input>  output=<output>  total=<input+output>` |
| input, output, and total | `input=<input>  output=<output>  total=<total>` |

The pretty renderer may derive a display-only total when input and output are
both present. It must not infer input or output from a total-only record, and it
must not persist a derived total anywhere.

## Surface Contracts

This spec changes only the pretty block produced by the renderer and relayed by
the Conductor. It does not change stdout contracts, durable artifacts, command
success criteria, or machine-readable state.

The following surfaces remain unchanged:

- canonical attestation JSON;
- the stable `[attestation] ...` header;
- PR bodies and merge artifacts that stamp canonical JSON plus the stable
  header;
- dispatch result JSON and verifier verdict JSON;
- attempt state, run state, or any stored attestation object;
- validation and fail-closed behavior from 0146.

The Conductor continues to relay each Worker and Verifier pretty block
verbatim. It must not summarize the block, hide fields, merge Worker and
Verifier records, or add up token usage across roles.

## Follow-On Code Acceptance Criteria

- Pretty renderer maps `codex` to `OpenAI`, `claude` to `Anthropic`, `gemini`
  to `Google`, and unknown adapters to themselves.
- Pretty renderer includes the `tool` line using the canonical adapter name.
- Pretty renderer displays `(detected)` for `model_source=parsed` and
  `(self-reported)` for `model_source=self-reported`.
- Pretty `started` and `ended` use host-local `YYYY-MM-DD HH:MM:SS TZ`
  formatting and drop sub-second precision.
- Pretty `duration` uses `<human> (<seconds> s)` with one decimal place and no
  `(N ms)` suffix.
- Pretty `tokens` uses thousands separators.
- Pretty `tokens` derives display-only total when input and output are present
  without total.
- Pretty `tokens` renders `total=<value>` only for total-only usage.
- Pretty output always includes `role`, `provider`, `tool`, `model`, `effort`,
  `permission`, `action`, `exit`, `started`, `ended`, `duration`, `tokens`, and
  `verified`.
- Canonical JSON and the stable `[attestation]` header are byte-for-byte
  unchanged.
- `.delivery.yml` sets `verifier.reasoning_effort: max`.
- Tests cover vendor mapping for `codex`, `claude`, `gemini`, and an unknown
  adapter; detected versus self-reported model source wording; local-time
  timestamp formatting; duration seconds formatting; token separators; derived
  total for input/output-only usage; total-only usage; and presence of all
  pretty fields.
- No 0146 canonical change and no new runtime dependency are introduced.

## Acceptance Criteria For This PR

- This PR adds only `docs/specs/0296-attestation-display-polish.md`.
- The spec has `status: draft`, `date: 2026-06-30`, `id: 296`, `issue: 296`,
  and empty `supersedes` / `superseded_by` frontmatter.
- The spec references and builds on 0146 and 0214 without editing,
  superseding, or amending either document.
- The spec records every display-layer decision in issue #296: vendor provider,
  `tool` line, human model-source wording, host-local second-precision
  timestamps, seconds duration suffix, token separators and display-only
  derived total, complete field rendering, follow-on Verifier effort
  configuration, and unchanged machine contracts.
- No Go code, `.delivery.yml`, accepted spec, command behavior, canonical JSON,
  stable header, validation, stored field value, or runtime dependency changes
  are included.

## Non-Goals

- No Go implementation in this design PR.
- No `.delivery.yml` change in this design PR.
- No command behavior change in this design PR.
- No new runtime dependency.
- No edit to 0146, 0214, or any other accepted spec.
- No 0146 schema change.
- No change to canonical JSON.
- No change to the stable one-line `[attestation]` header.
- No change to attestation validation, stored usage values, stored provider
  values, `verified`, `model_source`, or fail-closed behavior.
- No parser contract for pretty output.
- No aggregate token accounting across Worker, Verifier, or Conductor records.

## Relationship To Existing Specs

- [`0146-attestation.md`](0146-attestation.md) defines the shared
  `AttestationRecord`, canonical JSON, stable header, `provider`,
  `model_source`, token usage, `verified`, validation, and fail-closed
  behavior. This spec preserves those contracts.
- [`0214-human-readable-attestation.md`](0214-human-readable-attestation.md)
  defines the separate pretty renderer, emoji/plain modes, field completeness,
  and non-parse-target rule. This spec polishes that renderer's displayed field
  values and adds the `tool` line.
- [`0218-surface-worker-attestation.md`](0218-surface-worker-attestation.md)
  and [`0282-default-pretty-attestation.md`](0282-default-pretty-attestation.md)
  define where Worker and Verifier attestation is surfaced and relayed. This
  spec does not change those surface contracts; it changes only the pretty block
  they display.
