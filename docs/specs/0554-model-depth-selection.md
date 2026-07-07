---
id: 554
title: 0.6.0 Unit A - Model And Depth Selection
status: draft
date: 2026-07-07
issue: 554
pr: null
supersedes: []
superseded_by: []
---

# 0.6.0 Unit A - Model And Depth Selection

This is a design-only spec for loopcoder 0.6.0 Unit A. This PR adds only this
document: no Go code, no `.delivery.yml` change, no command behavior change, no
reference-doc update, and no new dependency. Code and reference-documentation
work must be filed separately after this spec merges, per
[`docs/PROCESS.md`](../PROCESS.md).

Unit A makes Worker and Verifier model selection discoverable, defaulted, and
validated. It also adds the Google Antigravity CLI provider path for Gemini
models. The implementation must remain static in this unit: dynamic provider
discovery, `agy models` reconciliation, and `--refresh` behavior are deferred.

## Goals

- Add a static `internal/models` leaf package that describes provider models,
  per-model depth tokens, and defaults.
- Add `loopcoder models [--provider <provider>]` as a read-only discovery
  command rendered from the static registry.
- Validate Worker and Verifier model/depth selections at parse time against the
  registry, warning by default and rejecting when strict mode is enabled.
- Define provider defaults for `codex`, `claude`, and `antigravity`.
- Preserve role-scoped selection from
  [`0215-per-role-model-override.md`](0215-per-role-model-override.md): Worker
  and Verifier each resolve their own provider, model, and depth.
- Add the Antigravity provider contract for the `agy` CLI, including the
  mandatory `--add-dir <worktree>` workspace pin.

## Terms

**Provider** means the loopcoder provider registry key used by Worker and
Verifier invocations. The 0.6.0 Unit A model registry contains:

- `codex` for OpenAI Codex CLI / GPT models;
- `claude` for Claude Code CLI / Claude models; and
- `antigravity` for Google Antigravity CLI / Gemini-family models.

The executable for the `antigravity` provider is `agy`. The provider key is not
`agy`; `agy` is the CLI binary name.

**Model** means the provider-native model string loopcoder passes to the
selected provider. Model names are exact, case-sensitive strings.

**Depth** means the provider-native token carried through the existing
`reasoning_effort` / `--effort` selection path. Depth is not a cross-provider
scale. Each model has its own list of valid depth tokens, and that list may be
empty.

**Effort** remains pass-through at provider execution time. Today `codex` passes
effort as `-c model_reasoning_effort=<depth>` and `claude` passes effort as
`--effort <depth>`. Unit A adds registry validation around the configured token;
it does not add provider-derived effort validation beyond the curated static
registry.

## Static Registry

Unit A adds a new `internal/models` package. It is a leaf package: it may import
only the standard library and other leaf-safe utility packages if unavoidable.
It must not import `internal/agent`, `internal/config`, `internal/worker`,
`internal/loopreview`, `internal/orchestration`, `internal/cli`, or any package
that calls provider CLIs. Those packages import `internal/models`, not the
other way around.

The registry is immutable static data compiled into loopcoder. It is not loaded
from `.delivery.yml`, provider config files, `agy models`, `codex`, `claude`,
or the network.

The data shape must be equivalent to:

```go
type Registry struct {
    Providers []Provider
}

type Provider struct {
    Name         string // registry key: codex, claude, antigravity
    DisplayName  string // human label
    Vendor       string // OpenAI, Anthropic, Google Antigravity
    CLI          string // executable name: codex, claude, agy
    DefaultModel string
    DefaultDepth string
    Models       []Model
}

type Model struct {
    Name         string
    Depths       []Depth
    DefaultDepth string // empty only when Depths is empty
}

type Depth struct {
    Token string // exact token used in config/CLI
    Label string // human label; may equal Token
}
```

Required invariants:

- Provider names are unique and non-empty.
- Model names are unique within a provider and non-empty.
- Depth tokens are unique within a model.
- If a model has a non-empty `Depths` list, `DefaultDepth` must exactly match
  one token in that list.
- If a model has an empty `Depths` list, `DefaultDepth` must be empty.
- Each provider's `DefaultModel` must name a model in that provider.
- Each provider's `DefaultDepth` must equal that model's `DefaultDepth`, unless
  the default model has no depths, in which case it must be empty.

### Initial Static Rows

The initial 0.6.0 Unit A registry is loopcoder-curated static data:

| Provider | Vendor | CLI | Model | Depth tokens | Model default | Provider default |
|---|---|---|---|---|---|---|
| `codex` | `OpenAI Codex` | `codex` | `gpt-5.5` | `low`, `medium`, `high`, `xhigh` | `high` | `gpt-5.5` / `high` |
| `claude` | `Anthropic` | `claude` | `claude-opus-4-8[1m]` | `low`, `medium`, `high`, `max` | `max` | `claude-opus-4-8[1m]` / `max` |
| `antigravity` | `Google Antigravity` | `agy` | `Gemini 3.1 Pro` | `Low`, `High` | `High` | `Gemini 3.1 Pro` / `High` |
| `antigravity` | `Google Antigravity` | `agy` | `Opus 4.6` | `Thinking` | `Thinking` | no |
| `antigravity` | `Google Antigravity` | `agy` | `GPT-OSS 120B` | `Medium` | `Medium` | no |

The `antigravity` rows mirror the curated `agy models` surface for this unit,
but they remain static registry data. A later release may specify refresh or
reconciliation behavior; Unit A must not silently derive registry rows from a
live CLI call.

The direct `gemini` CLI provider is not a target for 0.6.0 Unit A discovery or
defaults. Gemini-family target models are reached through the `antigravity`
provider because direct Gemini CLI access is not reliable for personal accounts
(`IneligibleTier`). Existing experimental direct-Gemini code, if still present,
must not appear in `loopcoder models` and must not be recommended by the new
registry.

## Selection Resolution

Worker and Verifier selection remains role-scoped. For each role, loopcoder
resolves provider, model, and depth independently.

Provider precedence:

1. Explicit role command flag, such as `dispatch --provider` or
   `loopreview --provider`.
2. `.delivery.yml` `adapters.worker` or `adapters.verifier`.
3. Existing built-in provider fallback for that role.

Model precedence:

1. Explicit role command flag `--model`.
2. `.delivery.yml` role field `worker.model` or `verifier.model`.
3. Static registry default model for the resolved provider.

Depth precedence:

1. Explicit role command flag `--effort`.
2. `.delivery.yml` role field `worker.reasoning_effort` or
   `verifier.reasoning_effort`.
3. Static registry default depth for the resolved model.

Empty strings and YAML null values are treated as absent, matching spec 0215.

Defaults are runtime resolution values. They must not be written back to
`.delivery.yml` unless the user explicitly asks to persist a preference. This
preserves the distinction between a project preference and loopcoder's
built-in fallback.

If the user configures a model but omits depth, loopcoder uses that model's
registry default depth. It does not use the provider default depth if the model
is not the provider default model.

If the user configures a depth but omits model, loopcoder resolves the provider
default model and validates the configured depth against that default model.

The Conductor is not resolved through this registry. The Conductor is the
active host session chosen by the operator and is not switchable by loopcoder.
Conductor model reporting remains host self-attestation only.

## Validation Semantics

Validation runs after provider/model/depth resolution and before launching a
Worker or Verifier provider process. It also runs during config-oriented checks
such as `loopcoder doctor`.

Validation is exact and case-sensitive:

- Provider names must match a registry provider key.
- Model names must match a model under the selected provider.
- Depth tokens must match a token under the selected provider and model.
- Antigravity depth tokens intentionally use capitalized provider-facing
  strings such as `High`; Codex and Claude depth tokens are lowercase.

Validation produces diagnostics. In default mode, diagnostics are warnings and
execution continues with the selected pass-through values. In strict mode,
diagnostics reject the command before provider launch.

Strict mode is enabled by either:

```yaml
models:
  strict: true
```

in `.delivery.yml`, or the one-run CLI flag:

```text
--strict
```

on commands that resolve Worker or Verifier model/depth selections. The CLI
flag does not mutate `.delivery.yml`.

Required diagnostics:

| Case | Default mode | Strict mode |
|---|---|---|
| Provider missing from model registry | warn; do not apply registry defaults | reject |
| Model not listed under provider | warn; pass configured model through | reject |
| Depth not listed under non-empty model depth list | warn; pass configured depth through | reject |
| Model has empty depth list and a depth is configured | warn; pass configured depth through | reject |
| Depth absent and model has non-empty default depth | use default depth; no warning | use default depth; no warning |
| Depth absent and model has empty depth list | leave depth empty; no warning | leave depth empty; no warning |

The empty-depth-list rule is deliberate. An empty list means loopcoder has no
curated valid depth tokens for that model. A configured depth may still be a
provider-specific pass-through, so default mode warns instead of silently
dropping it. Strict mode rejects because strict means every selected model and
depth must be proven valid by the registry.

Warning text must name the role, provider, model, depth, and registry reason,
for example:

```text
[loopcoder] warning: worker model selection: depth "deeper" is not valid for provider "codex" model "gpt-5.5" (valid depths: low, medium, high, xhigh)
```

Validation must never rewrite a user-provided invalid value in default mode. It
warns and passes the configured value through so current pass-through behavior
is preserved unless the operator opts into strict rejection.

## `loopcoder models`

Unit A adds:

```text
loopcoder models [--provider <provider>]
```

The command is read-only. It reads only the static `internal/models` registry.
It must not inspect `.delivery.yml`, call provider CLIs, call `agy models`, read
provider config, mutate files, or require provider authentication.

With no `--provider`, it prints every registry provider in registry order. With
`--provider`, it prints only the named provider. Unknown provider names exit
non-zero and print the supported provider keys. The command accepts registry
provider keys only; `--provider agy` is unknown and should hint that the
Antigravity provider key is `antigravity` and the CLI executable is `agy`.

The text output format is stable and plain ASCII:

```text
provider: codex
vendor: OpenAI Codex
cli: codex
default: gpt-5.5 / high
models:
  - gpt-5.5
    depths: low, medium, high*, xhigh

provider: claude
vendor: Anthropic
cli: claude
default: claude-opus-4-8[1m] / max
models:
  - claude-opus-4-8[1m]
    depths: low, medium, high, max*

provider: antigravity
vendor: Google Antigravity
cli: agy
default: Gemini 3.1 Pro / High
models:
  - Gemini 3.1 Pro
    depths: Low, High*
  - Opus 4.6
    depths: Thinking*
  - GPT-OSS 120B
    depths: Medium*
```

Rules for rendering:

- `default:` is `<model> / <depth>` when the provider default depth is non-empty.
- `default:` is `<model> / (none)` when the provider default model has no depth.
- `depths:` is `(none)` for an empty depth list.
- A `*` suffix marks the model's default depth token.
- Provider blocks are separated by one blank line.
- No ANSI styling is used.
- JSON output and dynamic refresh are out of scope for Unit A.

## Antigravity Provider

Unit A adds `internal/agent/antigravity.go` and registers an agent provider with
the key:

```text
antigravity
```

The provider uses executable:

```text
agy
```

The vendor string for reports and diagnostics is:

```text
Google Antigravity
```

### Invocation Contract

For an `agent.Invocation`, the Antigravity runner must:

- close stdin / provide no interactive stdin to the child process;
- pass the prompt using `-p <prompt>`;
- include `--add-dir <worktree>` on every invocation;
- set the process working directory to the worktree when practical, but never
  rely on process CWD as the workspace selection mechanism;
- capture stdout/stderr into the normal loopcoder log path;
- treat the plain-text final output as the invocation summary;
- use the selected model string as self-reported model metadata when provider
  output does not provide parseable model metadata; and
- tolerate absent token usage for this provider.

The mandatory workspace pin is:

```text
--add-dir <worktree>
```

This is not optional. It is required even when `cmd.Dir` is already the
worktree. Verified local behavior shows that without `--add-dir <worktree>`,
`agy` may ignore process CWD, write to
`~/.gemini/antigravity-cli/scratch`, and exit 0 after silently writing in the
wrong directory.

The model/depth mapping is:

```text
model + reasoning_effort -> "<model> (<Depth>)"
```

Examples:

| Model | Depth | Antigravity selected model string |
|---|---|---|
| `Gemini 3.1 Pro` | `High` | `Gemini 3.1 Pro (High)` |
| `Gemini 3.1 Pro` | `Low` | `Gemini 3.1 Pro (Low)` |
| `Opus 4.6` | `Thinking` | `Opus 4.6 (Thinking)` |
| `GPT-OSS 120B` | `Medium` | `GPT-OSS 120B (Medium)` |

The config/CLI model value remains the base model string from the registry.
Operators must not configure `Gemini 3.1 Pro (High)` as `model`; that combined
string is an Antigravity execution/reporting string derived from `model` plus
`reasoning_effort`.

If depth is empty for a future Antigravity model with an empty depth list, the
selected model string is just `<model>` with no parentheses.

### Summary And Structured Output

Antigravity is a plain-text-summary provider in Unit A. It does not have a
specified structured-output flag in this spec. When `agent.Invocation` carries
an `OutputSchema`, the caller remains responsible for asking for JSON or
structured text in the prompt. The Antigravity runner captures the returned
plain text as `Result.Summary`; higher-level parsers decide whether that text
satisfies the Worker summary or Verifier verdict contract.

### Read-Only Invocations

Existing loopcoder Verifier and audit-review paths rely on
`agent.Invocation.ReadOnly`. Unit A does not specify a verified Antigravity
read-only flag. Therefore the Antigravity runner must fail closed for
`ReadOnly=true` in Unit A.

Fail-closed means:

- a Worker invocation with `ReadOnly=false` may run;
- a Verifier or audit-review invocation with `ReadOnly=true` must not run in a
  mutating mode;
- `loopreview` must surface the provider failure as `needs-human`, not pass;
  and
- the failure must explain that Antigravity read-only mode is not available or
  not verified.

A later spec may define full Antigravity read-only support if a documented,
non-mutating Antigravity mode is verified. Unit A must not weaken the existing
Verifier read-only boundary.

### Attestation / Reporter Leniency

Existing attestation contracts require parsed Worker/Verifier model and token
usage when providers expose them. Antigravity is the exception introduced by
this spec because Unit A only has self-reported model selection and no stable
token usage parse.

For provider `antigravity`, attestation/reporter validation must accept:

- `model_source: self-reported` for Worker and Verifier records;
- a model string equal to the selected Antigravity model string, such as
  `Gemini 3.1 Pro (High)`;
- empty or absent token usage; and
- ordinary observed process timing, exit code, role, permission, action, and
  provider fields.

This leniency is provider-scoped. It must not relax parsed-model or token
requirements for `codex` or `claude`. The later attestation-to-reporter rename
must preserve this Antigravity exception.

## Doctor

`loopcoder doctor` must include model/depth readiness after Unit A.

Doctor must:

- validate `.delivery.yml` Worker and Verifier provider/model/depth selections
  using the same registry semantics as command parse-time validation;
- report warnings in default mode and failures when `models.strict: true` is
  configured;
- check that configured provider CLIs resolve on `PATH`;
- for provider `antigravity`, check executable `agy`, not `antigravity`; and
- for configured `antigravity`, run a cheap read-only OAuth probe so a missing
  Antigravity login fails clearly before dispatch.

The Antigravity OAuth probe is:

```text
agy models
```

Doctor must run it with a bounded timeout and bounded output capture. A
successful `agy models` command proves enough local OAuth readiness for Unit A.
A missing binary, non-zero exit, timeout, or login/OAuth error output must be
reported as a doctor failure with an actionable fix such as:

```text
run: agy login
```

Doctor must not use `agy models` to refresh or mutate the static registry.

## Natural-Language Dispatch

Natural-language model requests remain a conductor responsibility. For example,
an operator may say "use gemini, deeper"; the conductor translates that into a
provider/model/depth selection such as:

```text
--provider antigravity --model "Gemini 3.1 Pro" --effort High
```

The static registry is the ground truth for valid translation targets. Registry
validation catches invalid provider/model/depth names. It does not catch a
wrong-but-valid translation; the resulting report/attestation remains the
operator-visible confirmation of what was actually requested for that work ID.

## Follow-Up Code Slices

After this spec merges, implementation should be split into separate code
issues:

1. **Registry package.** Add `internal/models` with the static data, defaults,
   lookup helpers, and pure validation diagnostics. Prove it imports no
   orchestration/config/agent packages.
2. **Models command.** Add `loopcoder models [--provider]` rendering exactly
   from the static registry.
3. **Parse-time validation.** Wire Worker and Verifier config/CLI resolution
   through `internal/models`, add `models.strict` config and `--strict` command
   handling, and preserve warn-by-default pass-through behavior.
4. **Antigravity runner and doctor.** Add `internal/agent/antigravity.go`,
   register provider `antigravity`, implement the `agy` invocation contract,
   preserve Antigravity attestation/reporter leniency, and add doctor OAuth
   probing.
5. **Reference docs.** Update usage/config/provider docs for `loopcoder
   models`, model/depth config, strict validation, and Antigravity setup/login.

Each code issue must implement only its slice and reference this accepted spec.

## Acceptance Criteria For Implementation

- `internal/models` exists as a leaf package and contains the initial static
  registry rows from this spec.
- Provider defaults resolve to `codex` `gpt-5.5` / `high`, `claude`
  `claude-opus-4-8[1m]` / `max`, and `antigravity` `Gemini 3.1 Pro` / `High`.
- `loopcoder models` prints the stable text format from this spec and never
  calls provider CLIs.
- `loopcoder models --provider antigravity` prints the Antigravity registry
  block; `--provider agy` is rejected with a hint.
- Worker and Verifier each resolve provider, model, and depth independently.
- The Conductor is not switchable through this registry.
- Parse-time model/depth validation warns by default and rejects under
  `.delivery.yml` `models.strict: true` or CLI `--strict`.
- Empty model depth lists are supported; configured depth against an empty list
  warns by default and rejects in strict mode.
- Existing pass-through behavior is preserved in default mode: invalid
  configured values are warned about, not rewritten.
- `internal/agent/antigravity.go` invokes `agy` with closed stdin, `-p`, and
  mandatory `--add-dir <worktree>`.
- The Antigravity selected model string is derived as
  `"<model> (<Depth>)"` when depth is non-empty.
- Antigravity summaries are captured as plain text.
- Attestation/reporter validation accepts Antigravity self-reported model
  strings and absent token usage without relaxing `codex` or `claude`.
- `loopcoder doctor` validates configured model/depth selections and checks
  Antigravity OAuth readiness with `agy models`.

## Non-Goals

- No Go implementation in this design PR.
- No `.delivery.yml` change in this design PR.
- No reference-doc update in this design PR.
- No dynamic registry refresh, `--refresh`, or live provider reconciliation.
- No attempt to keep direct Gemini CLI as the target Gemini path for 0.6.0.
- No cross-provider depth scale or fuzzy depth mapping.
- No automatic natural-language interpretation inside the binary.
- No change to the human merge gate.
- No weakening of Verifier read-only requirements.
- No broad attestation/reporter validation relaxation beyond the
  provider-scoped Antigravity exception.

## Relationship To Existing Specs

- [`0215-per-role-model-override.md`](0215-per-role-model-override.md) remains
  the role-scoped config and CLI override foundation. This spec adds registry
  defaults and validation for those fields.
- [`0131-multi-provider-roles.md`](0131-multi-provider-roles.md) remains the
  provider-role foundation. This spec narrows the 0.6.0 Gemini target to the
  Antigravity provider instead of direct Gemini CLI.
- [`0146-attestation.md`](0146-attestation.md) remains the base attestation
  contract. This spec adds a provider-scoped Antigravity leniency for
  self-reported model and absent token usage.
- [`0300-model-attribution.md`](0300-model-attribution.md) remains the rule for
  honest pinned-model attribution when providers report parseable model usage.
  Antigravity has no parseable usage in Unit A, so its selected model string is
  self-reported.
- [`0447-relay-enforcement-hardgate.md`](0447-relay-enforcement-hardgate.md)
  remains unchanged. Antigravity reports are still local-only and subject to the
  same relay obligations as other Worker or Verifier reports.
