---
id: 300
title: Honest Model Attribution For Pinned Attestation
status: draft
date: 2026-06-30
issue: 300
pr: null
supersedes: []
superseded_by: []
---

# Honest Model Attribution For Pinned Attestation

This is a design-only spec. This PR must add only this document: no Go code, no
`.delivery.yml` change, no command behavior change, no schema change, and no
runtime dependency. Implementation belongs in a separate code issue after this
spec is reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

This spec builds on [`0146-attestation.md`](0146-attestation.md). It does not
amend, supersede, or edit that accepted spec. It refines only how an adapter
chooses the existing attestation `model` value when an invocation explicitly
pins a model and the provider reports usage for multiple models.

## Goal

Attestation must honestly name the configured model that received the work and
ran the main reasoning for a pinned Worker or Verifier invocation, while still
refusing to fabricate a model the provider did not report.

For providers that report multiple models in one invocation, the pinned model is
the intended primary model when it is also observed in provider usage. Auxiliary
models may consume more tokens in a run, but that alone must not relabel the
attested model away from the configured model that was asked to perform the
work.

## Problem

The current Claude adapter derives the attestation `model` field from
`claudePrimaryModel(modelUsage)`, which chooses the model with the most
`input+output` tokens in Claude's reported `modelUsage`.

Claude Code can run a small auxiliary model alongside the requested main model.
For example, a verifier-mode probe pinned `--model claude-opus-4-8[1m]` in
safe mode with `allowedTools` and `--output-format json`. The reported
`modelUsage` included:

| Reported model | Input tokens | Output tokens | Role in the run |
|---|---:|---:|---|
| `claude-opus-4-8[1m]` | 2446 | 4 | Pinned main model, context window 1,000,000, received the review context |
| `claude-haiku-4-5-20251001` | 441 | 13 | Auxiliary model |

That probe still selected the pinned main model because it had more total
tokens. In a real PR #295 review, the auxiliary model dominated token count, so
the attestation block showed `claude-haiku-4-5-20251001` even though
`claude-opus-4-8[1m]` was the configured verifier and ran the verification.

This is a labeling bug, not evidence that the wrong model actually ran. The
binary parsed real provider usage, but the "largest token count" heuristic
mistook an auxiliary model for the primary model when a pinned primary model was
also present.

## Decisions

1. **Pinned model wins when observed.** If an agent invocation explicitly pins a
   model and that exact configured model string appears in the provider's
   reported model usage, the attestation `model` must be the pinned model.

   The pinned model is the configured model that received the work and ran the
   main reasoning. Because the provider reported that exact model in usage,
   `model_source` remains `parsed`.
2. **Pinned-but-absent falls back to observed behavior.** If an invocation pins a
   model but that exact string does not appear in the provider's reported usage,
   the adapter must fall back to the no-pin primary-selection rule and report
   what the provider evidence shows.

   The adapter must not fabricate the pinned model when the provider ignored,
   rejected, renamed, or remapped it.
3. **No-pin behavior is unchanged.** When no model is pinned, this fix retains
   the existing provider-chosen primary selection. A deeper heuristic for
   no-pin multi-model runs is out of scope and may be specified later.
4. **Machine contracts are unchanged.** This spec changes only the selection of
   the existing `model` value for pinned multi-model invocations. It does not
   change canonical JSON schema, validation, `model_source` semantics, token
   usage semantics, `verified`, the stable `[attestation]` one-line header, or
   fail-closed behavior from 0146.
5. **Scope is multi-model adapters.** The rule applies to agent adapters that
   can report multiple models for one invocation, currently the Claude adapter.
   Codex and Gemini single-total parsing are unaffected.

## Selection Rule

For an adapter invocation with configured model string `inv.Model` and parsed
provider model usage `modelUsage`, the attested model is selected as follows:

1. If `inv.Model` is non-empty and `modelUsage` contains an exact key equal to
   `inv.Model`, return `inv.Model`.
2. Otherwise, return the provider-chosen primary model from the existing no-pin
   rule.

The first branch still uses parsed provider evidence. It does not treat the
configured model as self-reported because the provider usage confirms the exact
model string. The second branch preserves 0146 truthfulness by reporting a
model observed in provider output when the configured string is absent.

Exact string matching is intentional. If a provider aliases, canonicalizes, or
remaps a requested model into a different reported model name, the attestation
must report the observed model until a later spec defines provider-specific
alias reconciliation.

## Follow-On Code Acceptance Criteria

- The Claude adapter in `internal/agent/claude.go` reports `inv.Model` when
  `inv.Model` is set and that exact model appears in parsed `modelUsage`.
- When `inv.Model` is set but absent from parsed `modelUsage`, the Claude
  adapter reports the provider-chosen primary model from the existing no-pin
  rule.
- When `inv.Model` is unset, the Claude adapter reports the provider-chosen
  primary model from the existing no-pin rule.
- Tests cover pinned-and-present reports pinned, pinned-but-absent reports the
  provider primary, and unset reports the provider primary.
- Existing attestation schema, validation, stable header, `model_source`,
  token usage, and fail-closed tests still pass.

## Acceptance Criteria For This PR

- This PR adds only `docs/specs/0300-model-attribution.md`.
- The spec has `status: draft`, `date: 2026-06-30`, `id: 300`, `issue: 300`,
  and empty `supersedes` / `superseded_by` frontmatter.
- The spec references and builds on `docs/specs/0146-attestation.md` without
  editing, amending, or superseding it.
- The spec records the pinned-present, pinned-absent, and no-pin decisions from
  issue #300.
- The spec states that canonical JSON schema, validation, `model_source`
  semantics, the stable `[attestation]` one-line header, and command behavior
  are unchanged.
- No Go code, `.delivery.yml`, accepted spec, command behavior, canonical
  schema, stable header, validation rule, or runtime dependency change is
  included.

## Non-Goals

- No Go implementation in this design PR.
- No `.delivery.yml` change in this design PR.
- No command behavior change in this design PR.
- No schema, validation, stable header, `model_source`, token usage, or
  `verified` change.
- No edit to 0146 or any other accepted spec.
- No broader no-pin multi-model heuristic refinement.
- No provider-specific alias or remapping table.

## Relationship To 0146

[`0146-attestation.md`](0146-attestation.md) defines `AttestationRecord`, the
canonical JSON and header renderings, `model`, `model_source`, usage,
`verified`, validation, and fail-closed behavior. This spec preserves those
contracts.

0146 requires the `model` field to name the actual model used for the
invocation. For pinned multi-model Claude runs, provider usage can show both the
configured main model and an auxiliary model. When the configured main model is
observed in that usage, reporting the configured main model is the honest 0146
value; using token dominance alone can misattribute the invocation to an
auxiliary model.
