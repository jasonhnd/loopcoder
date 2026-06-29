---
id: 215
title: Per-Role Model and Effort Override
status: accepted
date: 2026-06-28
issue: 215
pr: null
supersedes: []
superseded_by: []
---

# Per-Role Model and Effort Override

This is a design-only spec for 0.3.x role-scoped model and reasoning-effort
configuration. This PR must add only this design record: no Go code, no
`.delivery.yml` change, no command behavior change, and no runtime dependency.
Implementation belongs in separate code issues after this spec is reviewed and
merged per [`docs/PROCESS.md`](../PROCESS.md).

## Goal

loopcoder must preserve inherit-by-default model behavior while giving users two
explicit control points:

1. A project first run or setup flow can record the user's worker and verifier
   model or effort preference once, then all later runs inherit that persisted
   project preference.
2. A conductor can override one role's model or effort for one run when output
   quality is insufficient, without changing the project default.

The default remains unchanged: when no user-stated role preference and no
per-run override exist, loopcoder passes no model or reasoning-effort flag and
inherits the selected provider CLI's own global configuration.

## Background

[`0131-multi-provider-roles.md`](0131-multi-provider-roles.md) introduced
provider roles for Worker and Verifier and moved model and effort handling into
provider adapters. It also records that `codex` and `claude` can honor an
effort knob, while `gemini` has no separate effort knob and must ignore effort
with an advisory rather than a silent behavior change.

[`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md)
hardened `loopreview` reliability and intentionally refused silent verifier
model changes. Reliability still comes from a bounded review packet, read-only
headless invocation, timeout enforcement, structured verdicts, and attestation.
This spec keeps that stance and adds only user-stated persistent preferences
and explicit one-run overrides.

[`../BACKLOG.md`](../BACKLOG.md#b1--worker-model--speed-selection) B1 records
the original worker-only rule: absent model and effort means provider-global
inheritance, while `dispatch --model`, `dispatch --effort`, and optional
`.delivery.yml` worker keys exist only when the user explicitly asks for them.
This spec extends the same rule to the Verifier role.

## Decisions

### 1. Inherit By Default

With no explicit per-run flag and no persisted role value, both Worker and
Verifier inherit the selected provider CLI's global model and effort settings.
loopcoder must never auto-select a model or reasoning effort on its own.

Inheritance applies independently per role. For example, a project may persist
`worker.model` while leaving `verifier.model` absent; the worker then uses the
project model while the verifier still inherits the verifier provider's global
configuration.

### 2. First-Run Specification, Then Inherit

The first setup path for a project, such as `loopcoder init` or the first
invocation that creates `.delivery.yml`, must let the user specify all four
optional role values:

- worker model;
- worker reasoning effort;
- verifier model;
- verifier reasoning effort.

The setup flow must also offer the explicit "inherit provider global config"
choice for each value. Choosing inherit leaves the key absent.

When the user states a first-run preference, loopcoder persists it into
`.delivery.yml`:

```yaml
worker:
  model: gpt-5
  reasoning_effort: high
verifier:
  model: claude-sonnet-4-5
  reasoning_effort: max
```

The exact model and effort strings are provider-specific pass-through values.
loopcoder stores what the user stated; it does not translate provider model
names, infer an equivalent model, or pick a value on the user's behalf.

Every later run inherits the persisted role value. The user does not re-specify
routine model or speed settings on every run. If no first-run preference is
specified, the keys remain absent and behavior falls back to provider-global
inheritance as today.

### 3. Per-Run Overrides For Both Roles

Per-run overrides are explicit, provider-agnostic knobs for a single role
invocation. They do not update `.delivery.yml`.

Worker keeps the existing flags:

```text
loopcoder dispatch --model <model> --effort <effort> ...
```

Those flags are no longer Codex-specific in documentation or help text. They
mean "pass this model or effort request to the selected worker provider for this
run only." `codex` and `claude` honor both where their CLIs support them;
`gemini` honors model and reports that effort is ignored.

Verifier gains matching flags:

```text
loopcoder loopreview --model <model> --effort <effort> ...
```

`loopreview --model` is a role-scoped verifier model override. `loopreview
--effort` is honored when the selected verifier provider has a reasoning-effort
or equivalent depth knob. Providers without an effort knob must emit an advisory
and proceed without silently pretending the effort was applied.

### 4. Config Surfaces

`.delivery.yml` keeps the existing optional worker fields and adds a parallel
optional verifier section:

```yaml
worker:
  # Optional. Absent = inherit the worker provider's global config.
  model:
  # Optional. Absent = inherit the worker provider's global config.
  reasoning_effort:

verifier:
  # Optional. Absent = inherit the verifier provider's global config.
  model:
  # Optional. Absent = inherit the verifier provider's global config.
  reasoning_effort:
```

Absent means inherit. Empty strings should be treated like absent values by
config loading and validation. A project may configure only one role, only one
field within a role, both roles, or none.

The `adapters` section continues to select providers, not models:

```yaml
adapters:
  worker: codex
  verifier: claude
```

Provider names are registry keys such as `codex`, `claude`, and `gemini`. Model
names belong under `worker.model` or `verifier.model`.

### 5. Precedence

For each role and field, precedence is:

1. Explicit per-run CLI flag.
2. Persisted `.delivery.yml` role value.
3. Provider-global inherit.

The precedence is evaluated separately for Worker model, Worker effort,
Verifier model, and Verifier effort. A per-run model flag does not imply an
effort override, and a persisted effort value does not imply a model override.

loopcoder writes model or effort values into `.delivery.yml` only when the user
states a preference at first run or explicitly later. It must not fill in a
model discovered from provider output, a recommended default, or an inferred
"best" verifier setting.

### 6. Conductor Quality Escalation

When the conductor judges a role's output quality insufficient, it may re-run
that role with an explicit higher-capability model or effort override for that
run only. Examples include re-dispatching a worker after a low-quality patch or
re-running `loopreview` after an over-strict or under-evidenced verdict.

This is a one-off override path. It must not mutate `.delivery.yml`, and it
must be visible in the command invocation, logs, and attestation for that role
run. The human merge gate remains unchanged.

### 7. Per-Provider Honoring

Provider adapters must handle unsupported model or effort requests explicitly:

- If a provider supports the requested flag, pass it through using that
  provider's native CLI syntax.
- If a provider lacks an effort knob, emit an advisory and omit the effort flag.
  This is the expected `gemini` behavior from 0131.
- If a provider lacks a model knob, emit an advisory and omit the model flag.
- If a provider CLI rejects a requested model or effort, report the provider
  error. Worker dispatch must fail rather than silently falling back. Verifier
  review must return `needs-human` rather than silently falling back.

Attestation remains the evidence layer. It records the actual parsed provider
model and effort when available, so a requested override that was ignored,
rejected, or mapped by the provider is visible to the conductor and human.

### 8. Correct The Stale Verifier Provider Config

The current repository `.delivery.yml` uses `adapters.verifier: opus`. Under the
0131 provider registry, `opus` is not a provider; it is a stale model-family or
host shorthand. The design correction is:

```yaml
adapters:
  verifier: claude
verifier:
  model: <optional Claude model, only if the user states one>
```

The provider slot must name a real provider such as `claude`. Any desire to use
a particular Claude model, including an Opus-class model, belongs in
`verifier.model`. This PR does not change `.delivery.yml`; the config correction
is a follow-on implementation/config issue after this spec merges.

### 9. Reconcile With 0194

0194 says verifier reliability must not come from changing model or effort. This
spec preserves that rule.

The reliable verifier contract still depends on bounded inputs, read-only
headless provider execution, timeout handling, structured verdict parsing, and
complete attestation. A verifier must not become merge-eligible merely because a
stronger model was selected. Conversely, an inherited default is still valid
when the bounded verifier contract is satisfied.

This spec only adds two explicit user-driven paths that 0194 did not define:
first-run persisted verifier preferences and one-run verifier overrides.
Neither path permits loopcoder to silently choose a model or effort for
reliability.

## Lifecycle

The complete lifecycle is:

1. Project setup or first invocation offers optional Worker and Verifier model
   and effort fields.
2. Values stated by the user are written to `.delivery.yml`; inherit choices are
   represented by absent keys.
3. Normal runs read `.delivery.yml` and pass the persisted role values to the
   selected provider adapters.
4. If a per-run flag is supplied, it overrides the persisted value for that role
   invocation only.
5. Provider adapters either honor the requested value, advise that they cannot
   honor it, or fail closed when the provider rejects it.
6. Attestation records the actual model and effort observed for the invocation.

## Follow-On Issues

After this spec merges, separate issues should be filed in this dependency
order:

1. **Config schema and stale-provider correction.** Add optional
   `verifier.model` and `verifier.reasoning_effort` config support, keep empty
   values equivalent to absent, preserve existing worker keys, and change the
   loopcoder repository's `adapters.verifier` from `opus` to a registered
   provider such as `claude`.
2. **First-run preference capture.** Update `loopcoder init` or the first-run
   `.delivery.yml` creation path so users can specify Worker and Verifier model
   and effort once, or explicitly choose provider-global inherit, without being
   asked every run.
3. **Worker provider-agnostic flag semantics.** Update `dispatch --model` and
   `dispatch --effort` help text, config precedence, adapter tests, and
   provider advisory behavior so the flags apply to `codex`, `claude`, and
   experimental `gemini` consistently.
4. **Verifier model and effort overrides.** Add `loopreview --model` and
   `loopreview --effort`, wire them through config precedence and provider
   adapters, and ensure unsupported or rejected values produce advisory or
   fail-closed behavior as specified above.
5. **Reference documentation and migration notes.** Update usage, worker,
   verifier, and configuration docs to separate provider slots from model
   overrides, preserve inherit-by-default language, and explain the
   quality-escalation path.

Each issue must be cross-platform Go where code changes are required, introduce
no runtime dependency, and preserve the human merge gate and reviewer-not-worker
guidance.

## Non-Goals

- No code, `.delivery.yml`, or command behavior change in this design PR.
- No automatic model, effort, or per-issue routing policy.
- No provider beyond the registry work already scoped by 0131.
- No weakening of `loopreview` bounded-packet reliability from 0194.
- No automatic merge and no change to the human merge gate.
- No provider secret or auth management inside loopcoder.
- No new runtime dependency.

## Acceptance Criteria For Implementation

- With no CLI flag and no role config value, Worker and Verifier pass no model
  or effort flag and inherit provider-global configuration.
- A first-run user preference is persisted under the correct role section and
  inherited on later runs.
- Per-run CLI flags override persisted role config for that invocation only.
- `dispatch --model` and `dispatch --effort` are provider-agnostic.
- `loopreview --model` and `loopreview --effort` are available and follow the
  same precedence rules.
- Unsupported provider knobs are advisory, not silent; provider rejection fails
  closed.
- `.delivery.yml` separates provider selection from model override, including a
  real verifier provider instead of `opus`.
- 0194's reliable verifier contract remains the reliability source; model and
  effort are never silently changed for reliability.
