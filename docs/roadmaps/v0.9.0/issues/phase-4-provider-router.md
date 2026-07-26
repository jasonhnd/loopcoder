# v0.9.0 Issue Drafts: P4 Provider Plane And Deterministic Router

Status: development-ready issue drafts; owner publication/assignment required

Publish only after the hardened visibility canary V090-024 passes and owner
approval. P4 extends the minimal execution contract from V090-095 and consolidates existing
provider inventory, agent invocation, quota, health, cooldown, routing, and
CodexBar bridge behavior. It must not add parallel stores or silently override an
explicit provider/model/effort pin. Credential ownership always remains with the
provider CLI/keychain/auth system.

## V090-037: Provider descriptor registry and conformance harness

**Metadata:** code, size M; depends on V090-024 and V090-095; labels `v0.9.0`,
`provider`, `spi`, `conformance`; exclusive in provider core interfaces.

### Outcome and rationale

Define one versioned provider descriptor/adapter contract for discovery, auth
status, model catalog, quota observations, invocation, capabilities, diagnostics,
and redaction. Existing provider logic is broad but spread across inventory,
agent, routing, and configuration packages.

### Scope and constraints

- Define stable adapter ID, installation/account identity, source capabilities,
  supported operations, probe plans, model entries, invocation request/result,
  and typed diagnostics.
- Keep discovery/observation separate from credentials and route policy.
- Add a descriptor registry with duplicate/version/capability validation.
- Build a fake provider conformance kit covering success, missing install, auth
  unknown, malformed output, timeout, rate limit, and unsupported operations.
- Inventory existing provider implementations and assign migration disposition.

### Acceptance criteria

1. A provider registers one versioned descriptor and only the capabilities it can
   prove; unsupported operations are explicit.
2. Discovery, catalog, quota, and invocation results share normalized provenance,
   confidence, freshness, and typed diagnostic envelopes.
3. Adapter code cannot read/write route decisions, project lifecycle, GitHub
   delivery, or raw credential values.
4. Duplicate IDs, incompatible descriptor versions, and capability/result mismatch
   fail before the adapter becomes eligible.
5. The fake adapter passes a reusable conformance suite with bounded time/output
   and environment redaction.

### Verification and boundaries

Conformance/golden tests and remote race, no real provider/network. Registration
failure leaves no machine observation. No provider-specific implementation,
automatic routing, quota policy, or plugin download. Done when official/future
providers use one contract and no new scheduler edits are needed for registration.

---

## V090-038: Ordered observation-source plans and provenance snapshots

**Metadata:** code, size M; depends on V090-037; labels `v0.9.0`, `provider`,
`inventory`, `provenance`; exclusive in observation planning.

### Outcome and rationale

Model provider discovery as an ordered, bounded source plan rather than one
opaque probe. Inspired by CodexBar's descriptor/source approach, each observation
must explain where it came from, what failed, and why a fallback source was used.

### Scope and constraints

- Define source kinds such as supported API/CLI, local status command, structured
  auth metadata, optional bridge, and unavailable; no scraping without review.
- Order sources by authority and safety, with per-step timeout/output/redirect/
  network policy and stop/fallback conditions.
- Persist immutable observations and one aggregate snapshot/digest in `machine.db`.
- Preserve diagnostics from skipped/failed sources without treating them as facts.
- Never parse credentials or copy auth tokens into snapshots.

### Acceptance criteria

1. Every provider capability has a deterministic ordered source plan and records
   selected source, attempted/skipped sources, diagnostic codes, and capture time.
2. Identical observations deduplicate; changed facts create a new immutable
   snapshot whose digest can be copied into a project route event.
3. Timeout, malformed, unauthenticated, stale, and unsupported are distinct and
   cannot be normalized to zero quota or no installation.
4. All commands/network calls have bounded time/output/redirects and scrubbed
   environment.
5. Fixture replay of the same source outputs yields byte-stable normalized facts
   and explanations.

### Verification and boundaries

Table-driven source-plan fixtures, malformed/timeout/fallback tests, remote race.
No real credentials/provider calls. No refresh scheduler, route scoring, or
CodexBar dependency. Done when provider-specific issues only supply descriptors
and parsers, not their own observation orchestration.

---

## V090-039: Adaptive refresh, health, and cooldown state

**Metadata:** code, size M; depends on V090-038; labels `v0.9.0`, `provider`,
`health`, `quota`; exclusive in provider observation scheduling.

### Outcome and rationale

Refresh provider facts often enough to route safely without burning quota or
busy-polling. Track health/cooldown separately from auth/model/quota facts and
make stale/unknown evidence visible.

### Scope and constraints

- Define per-source TTL, next refresh, success backoff, failure backoff, jitter
  bounds, manual refresh, in-flight deduplication, and restart checkpoint.
- Record health transitions and cooldown reason/scope/expiry from typed failures.
- Demand-refresh only when required evidence is stale for a pending decision.
- Coalesce many project requests into one machine-scoped refresh.
- Waiting/refresh uses zero coding-model calls and respects provider rate limits.

### Acceptance criteria

1. Fresh evidence is reused; concurrent stale requests trigger at most one probe
   per source scope.
2. Backoff/cooldown survives restart, is bounded, and avoids synchronized or busy
   polling under repeated failure.
3. Unknown/stale/unavailable remain distinct and are never rendered as healthy or
   zero remaining capacity.
4. Manual refresh reports source-by-source results but cannot bypass cooldown that
   protects account/provider safety without explicit override evidence.
5. Injected-clock tests prove TTL, reset wake, deduplication, backoff, and recovery
   with no provider runner dependency.

### Verification and boundaries

Fake source/injected-clock tests and remote race. A refresh failure preserves the
last observation with stale status; it never deletes known installations. No
route choice or real provider. Done when all official adapters share one refresh
and cooldown mechanism.

---

## V090-040: Codex discovery and model-catalog consolidation

**Metadata:** code, size S; depends on V090-037, V090-038, V090-039; labels
`v0.9.0`, `provider:codex`; exclusive in Codex observations/catalog.

### Outcome and rationale

Consolidate Codex executable discovery, installation/account identity, auth
status, model catalog, and capability aliases behind the provider observation
contract. Invocation remains the separate V090-103 responsibility.

### Scope and constraints

- Use supported Codex CLI surfaces and bounded local probes; record executable
  identity/version and account profile without reading credentials.
- Normalize canonical model IDs, aliases, context/capability/effort support, and
  source provenance.
- Preserve unknown/unsupported/stale observations rather than selecting defaults.
- Keep observation code unable to launch Codex or choose a route.
- Keep old Codex paths compatibility-only until P6 deletion.

### Acceptance criteria

1. Installed/not-installed, version, auth known/unknown, account profile, and model
   catalog are discovered through documented bounded sources.
2. Alias normalization is reversible/explainable and cannot turn an unknown model
   into an eligible default silently.
3. Catalog entries name source, capture time, canonical ID, aliases, context,
   effort/permission capabilities, confidence, and freshness.
4. Probe timeout, malformed output, auth unknown, unsupported version, and stale
   source map to typed observations and never erase the last known snapshot.
5. Observation conformance passes without launching Codex, reading credentials,
   or writing repo-local runtime state.

### Verification and boundaries

Recorded/synthetic CLI fixtures by default; one opt-in bounded discovery smoke
may be release evidence. No invocation (V090-103), quota (V090-041), automatic
route, or Codex-specific scheduler behavior.

---

## V090-103: Codex invocation consolidation

**Metadata:** code, size S; depends on V090-040 and V090-095; labels `v0.9.0`,
`provider:codex`, `invocation`; exclusive in Codex execution translation.

### Outcome and rationale

Move existing Codex command construction, exact model/effort/permission mapping,
process launch, parsing, and normalized outcome behind the minimal execution
contract without duplicating discovery, quota, routing, or supervision.

### Scope and constraints

- Translate one immutable request into one bounded Codex invocation using the
  accepted observed capabilities.
- Pass supported idempotency metadata and record actual route/receipt/usage only
  when Codex proves it.
- Enforce requested/actual mismatch and unsupported combinations before success.
- Reuse the one runtime/process tree and scrub inherited configuration that can
  silently replace model, permission, tools, or delegation policy.
- Keep credentials in Codex auth and keep lifecycle/Git/GitHub writes outside.

### Acceptance criteria

1. Exact model, effort, permission, cwd, input, timeout, and delegation policy
   reach the command and normalized launch evidence.
2. Unsupported or mismatched actual route fails closed and never becomes a
   successful attempt.
3. Auth, model, rate-limit, timeout, cancellation, malformed output, output
   flood, nonzero exit, and descendant escape map to typed joined outcomes.
4. Retry/recovery cannot reinterpret the immutable request through a newer
   catalog or current defaults.
5. Execution conformance passes with no credential persistence, route choice,
   project-lifecycle write, or second process supervisor.

### Verification and boundaries

Synthetic/recorded invocation fixtures plus bounded opt-in real smoke at release.
No discovery, quota, router, fallback, or Codex-specific scheduler.

---

## V090-041: Codex quota-window adapter

**Metadata:** code, size S; depends on V090-040; labels `v0.9.0`,
`provider:codex`, `quota`; exclusive in Codex quota normalization.

### Outcome and rationale

Collect Codex five-hour, weekly, credit, or other supported windows honestly so
the router can use available subscription capacity without guessing.

### Scope and constraints

- Parse only approved structured/local surfaces with ordered provenance.
- Normalize window kind, scope/account/model, used/remaining/limit units, reset
  time, capture time, confidence, freshness, and source diagnostic.
- Preserve partial windows independently; no arithmetic fabrication of a missing
  limit or reset.
- Store observations in `machine.db`; no credentials/raw responses beyond bounded
  diagnostic fixtures.
- Do not decide which model to run.

### Acceptance criteria

1. Supported Codex quota windows normalize with source, account scope, units,
   capture/reset times, confidence, and freshness.
2. Missing, unlimited, unknown, stale, malformed, and unavailable remain distinct
   from numeric zero.
3. Reset/time parsing is timezone-safe and tested at boundary/clock-skew cases.
4. Parser/probe output is bounded/redacted and never exposes auth material.
5. Recorded fixtures cover multiple/partial windows and source changes without
   requiring a live account in PR CI.

### Verification and boundaries

Golden parser/time tests and provider conformance; optional real smoke only with
owner consent and redacted evidence. No route score or quota reservation. Done
when V090-052 can consume normalized Codex windows without provider logic.

---

## V090-042: Claude Code discovery and model-catalog consolidation

**Metadata:** code, size S; depends on V090-037, V090-038, V090-039; labels
`v0.9.0`, `provider:claude`; exclusive in Claude observations/catalog.

### Outcome and rationale

Consolidate Claude Code discovery, installation/account/auth state, model
catalog, aliases, and capability observations. Invocation is isolated in
V090-104 so observation refresh never launches a coding model.

### Scope and constraints

- Use documented/supported CLI behavior with bounded probes and scrubbed env.
- Normalize executable/version, auth state, account profile, canonical models,
  aliases, context/effort/permission and native-subagent capability.
- Record native-subagent capability as observed metadata only; it grants no
  execution permission or GitHub/work ownership.
- Keep observation unable to launch Claude or select a route.
- Keep old Claude paths compatibility-only until deletion evidence.

### Acceptance criteria

1. Discovery/catalog satisfy observation conformance and expose only proven
   installation, auth, account, model, effort, permission, and child capabilities.
2. Unknown/unsupported aliases or capabilities never become eligible defaults.
3. Timeout, malformed output, auth unknown, unsupported version, and stale source
   map to typed observations without erasing last known evidence.
4. Every snapshot records provenance, capture time, confidence, freshness, and
   diagnostics and launches no model.
5. No credential or raw private conversation is persisted in observations.

### Verification and boundaries

Recorded/synthetic fixtures; optional bounded discovery smoke. No invocation
(V090-104), quota (V090-043), auto-routing, or external decomposition.

---

## V090-104: Claude Code invocation consolidation

**Metadata:** code, size S; depends on V090-042 and V090-095; labels `v0.9.0`,
`provider:claude`, `invocation`; exclusive in Claude execution translation.

### Outcome and rationale

Place existing Claude Code command construction, exact route/policy mapping,
process launch, structured parsing, and outcome normalization behind the minimal
provider execution contract while preserving native-child containment.

### Scope and constraints

- Translate one immutable request against the accepted catalog snapshot.
- Enforce exact model, effort, permission, tools/context, and native-delegation
  policy and record actual supported evidence.
- Reuse one runtime process tree; all native descendants remain inside the parent
  Attempt resource/output/cancellation/terminal envelope.
- Scrub inherited user configuration that could silently broaden permission,
  change model, or enable delegation.
- Keep credentials, route choice, project writes, and Git/GitHub outside.

### Acceptance criteria

1. Requested route and policy reach invocation exactly and unsupported
   combinations fail before launch.
2. Actual route mismatch, auth/model/rate-limit, timeout, cancellation, malformed
   output, and nonzero exit map to typed outcomes.
3. Native descendants are observed, counted, stopped, and joined under the parent
   and never become hidden LoopCoder WorkItems.
4. Retry/recovery uses the immutable request/catalog digest and cannot adopt
   current defaults silently.
5. Execution conformance passes without credential/private-conversation
   persistence, route choice, or second supervision.

### Verification and boundaries

Recorded invocation/child fixtures and optional bounded real smoke. No discovery,
quota, automatic routing, fallback, or cross-provider decomposition.

---

## V090-043: Claude Code quota-window adapter

**Metadata:** code, size S; depends on V090-042; labels `v0.9.0`,
`provider:claude`, `quota`; exclusive in Claude quota normalization.

### Outcome and rationale

Represent all supported Claude Code usage windows and reset times with the same
honest semantics as Codex, including unknown or unavailable evidence.

### Scope and constraints

- Implement ordered approved sources and normalized quota observations.
- Support multiple account/model/window scopes without collapsing them.
- Preserve source provenance, confidence, freshness, capture/reset, units, and
  diagnostics.
- Never infer weekly remaining from five-hour remaining or local token counters.
- No credential/auth mutation and no route policy.

### Acceptance criteria

1. Every supported window has explicit scope, units, source, times, confidence,
   and freshness.
2. Unknown/stale/unavailable/unlimited/malformed remain distinct from zero.
3. Partial observations are retained without making the whole provider healthy or
   ineligible automatically.
4. Boundary/reset/clock-skew and changed-source fixtures normalize deterministically.
5. Logs/events contain redacted summaries, never auth or raw account payloads.

### Verification and boundaries

Golden fixtures and conformance, optional owner-approved live smoke only. No
router/reservation. Done when V090-052 consumes provider-neutral windows.

---

## V090-044: Gemini CLI discovery and model-catalog consolidation

**Metadata:** code, size S; depends on V090-037, V090-038, V090-039; labels
`v0.9.0`, `provider:gemini`; exclusive in Gemini CLI observations/catalog.

### Outcome and rationale

Make Gemini CLI an explicit provider surface with honest installation, account,
auth, model, effort, context, and permission observations. Antigravity remains a
separate installation/adapter in V090-106 rather than being conflated by brand.

### Scope and constraints

- Document accepted Gemini CLI surfaces, auth ownership, and unsupported modes.
- Discover installation/version/account/auth/model catalog through bounded
  ordered sources.
- Normalize Flash/deeper model capabilities and exact canonical IDs from evidence;
  do not invent version names.
- Treat surface ambiguity or unsupported auth as ineligible/needs setup.
- Keep observation unable to invoke Gemini or reuse Antigravity credentials.

### Acceptance criteria

1. Gemini CLI installation/account/auth evidence is uniquely identifiable and
   never merged with Antigravity or another Google surface.
2. Catalog includes only observed supported models/efforts/permissions; an
   unknown advertised name does not become eligible.
3. Every catalog fact records source, capture time, confidence, freshness, and
   exact surface/account scope.
4. Auth unavailable, unsupported surface/version, timeout, malformed output, and
   stale source are typed and actionable without launching a model.
5. Observation conformance passes with no credential copying, invocation, or
   repo-local state.

### Verification and boundaries

Recorded/synthetic fixtures and optional owner-approved discovery smoke. No
invocation (V090-105), quota (V090-045), auto-route, or credential repair.

---

## V090-105: Gemini CLI invocation consolidation

**Metadata:** code, size S; depends on V090-044 and V090-095; labels `v0.9.0`,
`provider:gemini`, `invocation`; exclusive in Gemini CLI execution translation.

### Outcome and rationale

Adapt existing Gemini execution to the minimal provider contract with exact
surface/model/effort/permission evidence, bounded process ownership, and no
dependence on future automatic routing.

### Scope and constraints

- Translate one immutable request using the accepted Gemini catalog snapshot.
- Pass exact supported model/effort/permission/context/delegation controls and
  normalize actual route, receipt, usage, output, and exit evidence.
- Reuse one runtime process tree and scrub inherited provider configuration.
- Reject requested/actual mismatch and unsupported surface before success.
- Keep credentials, discovery, quota, route choice, and Git/GitHub outside.

### Acceptance criteria

1. Exact requested route and policy reach the Gemini command and launch evidence.
2. Auth/model/rate-limit, timeout, cancellation, malformed output, nonzero exit,
   output flood, and child escape map to typed joined outcomes.
3. Actual surface/model/effort mismatch blocks success and cannot fall back to
   Antigravity or another provider.
4. Retry/recovery uses immutable request/catalog digests and never current aliases
   or defaults silently.
5. Execution conformance passes with no credential persistence, route decision,
   project lifecycle write, or second supervisor.

### Verification and boundaries

Synthetic/recorded invocation fixtures plus optional bounded real smoke. No
discovery, quota, automatic route, Antigravity fallback, or credential repair.

---

## V090-045: Gemini CLI quota-window adapter

**Metadata:** code, size S; depends on V090-044; labels `v0.9.0`,
`provider:gemini`, `quota`; exclusive in Gemini CLI quota normalization.

### Outcome and rationale

Collect supported Gemini CLI windows, credits, and reset evidence without
pretending local use estimates are provider-authoritative.

### Scope and constraints

- Define source plans per Gemini CLI installation/account.
- Normalize observed limits/remaining/used/reset/units/confidence/freshness.
- Mark estimated local usage as estimated and never use it as exact provider
  remaining capacity.
- Preserve unsupported/unknown per window and per installation.
- No route choice or auth mutation.

### Acceptance criteria

1. Quota observations retain exact Gemini CLI installation and account scope.
2. Exact provider evidence and estimated local evidence are never conflated.
3. Missing reset/limit/remaining fields remain partial, not fabricated.
4. Multi-window/reset/clock-skew/malformed fixtures normalize deterministically.
5. Probe/parser is bounded, redacted, and passes the provider conformance rules.

### Verification and boundaries

Fixture golden/time tests, optional real smoke. No score/reservation. Done when
V090-052 can compare honest normalized windows or explain why it cannot.

---

## V090-106: Antigravity discovery and model-catalog adapter

**Metadata:** code, size S; depends on V090-037, V090-038, V090-039; labels
`v0.9.0`, `provider:antigravity`; exclusive in Antigravity observations/catalog.

### Outcome and rationale

Model Antigravity as its own provider installation and account surface. Do not
assume Gemini CLI auth, models, quota, invocation flags, or receipts apply merely
because both surfaces relate to Google models.

### Scope and constraints

- Revalidate the supported local Antigravity executable/API surface and auth
  ownership at implementation time.
- Discover installation/version/account/auth and model/capability catalog through
  bounded reviewed sources.
- Normalize exact canonical models, efforts, permissions, context, native-child
  capability, provenance, confidence, and freshness.
- Keep unsupported or unobservable capability explicit.
- Do not invoke Antigravity, read credentials, or borrow Gemini CLI observations.

### Acceptance criteria

1. Antigravity identity/account/catalog remains distinct from Gemini CLI and
   other provider installations on the same machine.
2. Catalog exposes only observed supported models/capabilities with source and
   freshness; unknown product claims never become eligible.
3. Missing install/auth, unsupported version/surface, timeout, malformed output,
   and stale data produce typed observations.
4. Discovery is bounded/redacted, launches no coding model, and stores no
   credential or private project content.
5. Observation conformance passes with deterministic fixtures and an explicit
   unsupported result when no reviewed source exists.

### Verification and boundaries

Synthetic/recorded fixtures; optional owner-approved discovery smoke. No
invocation, quota, route, credential repair, or Gemini fallback.

---

## V090-107: Antigravity invocation consolidation

**Metadata:** code, size S; depends on V090-095 and V090-106; labels `v0.9.0`,
`provider:antigravity`, `invocation`; exclusive in Antigravity execution.

### Outcome and rationale

Provide one exact, bounded Antigravity execution adapter when current public/local
surfaces support it. If invocation cannot be proven, the adapter remains
explicitly unsupported rather than silently using Gemini CLI or another model.

### Scope and constraints

- Translate one immutable request against an accepted Antigravity catalog.
- Enforce exact model/effort/permission/context/delegation and record actual
  surface/route evidence.
- Reuse one runtime process tree and provider-owned auth.
- Normalize unsupported, auth, model, rate-limit, timeout, cancellation,
  malformed output, nonzero exit, and child escape.
- Keep route choice, project lifecycle, Git/GitHub, and Gemini fallback outside.

### Acceptance criteria

1. Supported invocation proves exact requested/actual surface and route; an
   unsupported surface launches nothing and is actionable.
2. No Antigravity failure or missing capability substitutes Gemini, Codex,
   Claude, Grok, or another model automatically.
3. Every process/descendant is supervised, bounded, stopped, and joined under one
   Attempt.
4. Retry/recovery uses immutable request/catalog evidence and cannot adopt new
   defaults silently.
5. Execution conformance passes without credentials, route decisions, or a
   second process supervisor.

### Verification and boundaries

Synthetic success/unsupported/failure/silent/child fixtures and optional bounded
real smoke only where a reviewed public/local surface exists. No quota or route.

---

## V090-108: Antigravity quota-window adapter

**Metadata:** code, size S; depends on V090-106; labels `v0.9.0`,
`provider:antigravity`, `quota`; exclusive in Antigravity quota evidence.

### Outcome and rationale

Represent only quota/credit/reset windows that Antigravity can actually expose.
Unknown or unsupported telemetry remains honest and does not borrow Gemini CLI
windows or infer provider-authoritative capacity from local token counters.

### Scope and constraints

- Define reviewed source plans per installation/account/model scope.
- Normalize values, units, reset/capture, confidence, freshness, and diagnostics.
- Separate exact provider evidence, estimated local usage, and rate-limit retry.
- Store bounded redacted observations in machine authority.
- Do not route, reserve, invoke, or access credentials.

### Acceptance criteria

1. Every observation names Antigravity installation/account/model/window scope
   and cannot be mistaken for Gemini CLI evidence.
2. Exact, estimated, unknown, stale, unsupported, unlimited, and zero remain
   distinct.
3. Partial windows retain missing fields without fabrication or false health.
4. Reset/clock-skew/malformed/source-change fixtures normalize deterministically.
5. Probe/parser is bounded, redacted, and passes provider quota conformance.

### Verification and boundaries

Golden/time fixtures and optional owner-approved telemetry smoke. No scraping of
unsupported endpoints, score, reservation, invocation, or credential access.

---

## V090-046: Grok discovery and model-catalog consolidation

**Metadata:** code, size S; depends on V090-037, V090-038, V090-039; labels
`v0.9.0`, `provider:grok`; exclusive in Grok observations/catalog.

### Outcome and rationale

Make Grok a first-class observed provider with bounded discovery, account/auth
state, model catalog, and truthful diagnostics. Invocation is isolated in
V090-109 so refresh cannot consume coding capacity or launch a worker.

### Scope and constraints

- Document the supported Grok local/CLI surface and auth ownership.
- Discover executable/version/account/auth/model catalog and supported effort,
  permission, context, and native-child behavior.
- Record observed silent/liveness/native-child capabilities without launching a
  model or converting them into permission.
- Keep observation unable to invoke Grok or select a route.
- Preserve old Grok adapter only as compatibility until parity/deletion.

### Acceptance criteria

1. Discovery/catalog pass observation conformance and identify unsupported setup
   with actionable diagnostics.
2. Catalog includes only observed model/effort/permission/context/native-child
   capabilities with source, freshness, and confidence.
3. Missing install/auth, unsupported version, timeout, malformed output, and
   stale source remain typed and do not erase last known evidence.
4. Observation launches no coding model and cannot silently substitute another
   provider or default model.
5. No credential or private prompt/output enters machine observations.

### Verification and boundaries

Synthetic/recorded discovery/catalog fixtures; optional bounded discovery smoke.
No invocation (V090-109), quota (V090-047), route, or credential repair.

---

## V090-109: Grok invocation consolidation

**Metadata:** code, size S; depends on V090-046 and V090-095; labels `v0.9.0`,
`provider:grok`, `invocation`; exclusive in Grok execution translation.

### Outcome and rationale

Adapt Grok execution to the minimal provider contract with exact owner pins,
truthful long/silent process evidence, bounded resources, and no silent fallback
to another company or model.

### Scope and constraints

- Translate one immutable request using the accepted Grok catalog snapshot.
- Enforce exact model/effort/permission/context/delegation and record actual route,
  receipt, usage, output, and terminal evidence only when proven.
- Keep terminal inactivity separate from OS liveness and semantic progress.
- Reuse one runtime process tree and scrub inherited provider configuration.
- Keep credentials, discovery, quota, route choice, and Git/GitHub outside.

### Acceptance criteria

1. Exact requested Grok route/policy reaches invocation and no failure can
   substitute another provider/model automatically.
2. Long silent execution remains visible through core timed reports, process
   evidence, and resources rather than provider narration.
3. Actual mismatch, auth/model/rate-limit, timeout, cancellation, malformed
   output, nonzero exit, output flood, and child escape map to typed outcomes.
4. Every descendant is bounded, signalled, and joined under one Attempt; liveness
   alone never proves progress or success.
5. Execution conformance passes without credentials, route decision, project
   lifecycle write, or second supervisor.

### Verification and boundaries

Synthetic/recorded silent/hang/child/output/failure fixtures and optional bounded
real smoke. No discovery, quota, router, fallback, or model-generated reports.

---

## V090-047: Grok quota-window adapter

**Metadata:** code, size S; depends on V090-046; labels `v0.9.0`,
`provider:grok`, `quota`; exclusive in Grok quota normalization.

### Outcome and rationale

Represent any supported Grok quota/credit/rate-limit windows with provenance and
honest unknowns so unused capacity can later participate in routing.

### Scope and constraints

- Use only reviewed structured/local sources and per-account/model scope.
- Normalize exact/estimated values, units, capture/reset, confidence, freshness,
  and diagnostics.
- Preserve rate-limit retry times separately from subscription quota windows.
- Do not derive provider quota from LoopCoder token usage alone.
- No route decision or credential access.

### Acceptance criteria

1. Supported windows normalize to the common quota contract with exact source and
   scope.
2. Rate limit, subscription remaining, credits, unknown, stale, unlimited, and
   zero are distinct.
3. Partial/malformed/reset-boundary fixtures produce deterministic results.
4. No unsupported endpoint scraping or credential extraction is introduced.
5. Adapter passes bounded-output, timeout, redaction, and conformance tests.

### Verification and boundaries

Golden/time fixtures; optional real smoke after owner approval. No score or
reservation. Done when V090-052 can include or explicitly exclude Grok evidence.

---

## V090-048: Optional CodexBar observation bridge

**Metadata:** code, size S; depends on V090-038, V090-041, V090-043, V090-045,
V090-047, and V090-108; labels `v0.9.0`, `integration:codexbar`, `quota`; disjoint optional
adapter after official source plans stabilize.

### Outcome and rationale

Allow an installed CodexBar-compatible local observation surface to supplement
official provider observations while keeping it optional, bounded, provenance-
tagged, and unable to own credentials or override higher-authority evidence.

### Scope and constraints

- Add one optional source descriptor and parser for reviewed public structured
  output only.
- Record CodexBar as source/strategy with version, capture time, diagnostics, and
  confidence; retain provider/account/window scope.
- Prefer official higher-authority fresh sources; define deterministic conflict
  and fallback rules.
- Bound executable discovery, timeout, output, redirects, and environment.
- LoopCoder must work fully when CodexBar is absent, broken, or removed.

### Acceptance criteria

1. Bridge absence has no error or eligibility effect beyond source-unavailable.
2. Bridge observations never silently override fresher higher-authority provider
   facts and conflicts remain visible.
3. Malformed, old-version, timeout, partial, and duplicate output are bounded and
   typed.
4. No credential/auth file is read or persisted by the bridge.
5. Removing the bridge leaves official provider adapters and stored provenance
   valid without migration.

### Verification and boundaries

Recorded output fixtures only in PR CI; no required CodexBar install. No provider
invocation or router policy. Done when it is a replaceable observation source,
not a runtime dependency.

---

## V090-049: Future-provider registration kit

**Metadata:** code/docs, size S; depends on V090-037; labels `v0.9.0`, `provider`,
`extension`; exclusive in provider developer tooling.

### Outcome and rationale

Prove a fourth/fifth future company can be added through the provider contract
without editing scheduler, store schema, direct-run lifecycle, or route engine.

### Scope and constraints

- Provide an internal adapter scaffold, descriptor checklist, conformance command,
  fixture format, provenance/redaction checklist, and capability version policy.
- Add a synthetic `example` provider used only in tests.
- Document official built-in versus future external adapter support and security
  review; do not implement runtime-downloaded packs in v0.9.
- Require explicit registration and allowlist; no arbitrary executable discovery.

### Acceptance criteria

1. The synthetic provider registers and passes discovery/catalog/quota/invocation
   conformance without changes outside provider registration/test fixtures.
2. Checklist covers auth ownership, source authority, bounds, redaction, model
   identity, quota semantics, actual-route proof, cancellation, and child cleanup.
3. Incompatible contract/capability versions fail with actionable diagnostics.
4. Unknown/untrusted adapter code cannot auto-load from a customer repo.
5. Documentation states that user-installable provider packs are deferred and
   future work needs a signing/trust/update design.

### Verification and boundaries

Synthetic adapter tests, no provider/network. No plugin marketplace, dynamic Go
loading, credentials, or new official provider. Done when extension pressure no
longer requires core edits but remains a controlled build-time boundary.

---

## V090-050: Task risk classes and Luna, Tera, and Soul capability mapping

**Metadata:** code/docs, size S; depends on V090-036; labels `v0.9.0`, `routing`,
`policy`; exclusive in task classification/capability policy.

### Outcome and rationale

Define what LoopCoder means by Luna, Tera, and Soul as provider-neutral capability
classes, and map deterministic task risk/complexity evidence to required class.
Marketing model names change; route policy must not hard-code one company model.

### Scope and constraints

- Define risk inputs: change type, affected ownership, migration, security,
  concurrency, external side effects, test breadth, reversibility, and ambiguity.
- Define Luna for narrow routine work, Tera for standard bounded implementation,
  and Soul for high-risk architecture/security/migration/complex reasoning.
- Map provider models to classes using observed capabilities and reviewed policy,
  not name ordering or quota size.
- Permit owner override upward/downward with recorded reason; explicit model pin
  remains authoritative if eligible.
- Classification is deterministic and explainable, not model-generated.

### Acceptance criteria

1. Versioned rules classify a fixture task set consistently and list every risk
   input/reason.
2. Capability classes are provider-neutral and separate from canonical model IDs.
3. Unknown evidence chooses the conservative documented class or needs-human;
   it never silently chooses a cheaper/weaker model.
4. Owner override is persisted with actor/reason and cannot mutate an active
   attempt route.
5. Adding a newly observed model changes only capability policy data/tests, not
   scheduler code.

### Verification and boundaries

Golden classification/mapping tests and policy review. No model calls, quota,
route winner, or automatic decomposition. Done when V090-051 can evaluate required
capability without brand-specific heuristics.

---

## V090-051: Hard eligibility and immutable-pin precedence

**Metadata:** code, size M; depends on V090-028, V090-037, V090-039, V090-050;
labels `v0.9.0`, `routing`, `eligibility`, `safety`; exclusive in route admission.

### Outcome and rationale

Build the hard eligibility filter and precedence ladder before scoring quota.
Installed/authenticated/model-compatible/permission-capable/healthy/resource-fit
routes may compete; all others are excluded with reasons. Explicit owner pin wins
or fails closed.

### Scope and constraints

- Precedence: immutable explicit pin, then policy allow/deny, installation/auth,
  model/capability/effort/permission, task class, health/cooldown, concurrency and
  machine admission.
- Produce candidate and exclusion records with evidence IDs/freshness.
- Unknown hard prerequisites are ineligible or needs-human, never assumed true.
- Do not use quota remaining to make an incompatible route eligible.
- Evaluation is pure/deterministic from a captured snapshot.

### Acceptance criteria

1. An eligible explicit pin is selected unchanged; an ineligible explicit pin
   fails with reasons and never falls back automatically.
2. Every candidate has a deterministic eligible/excluded result and ordered reason
   codes tied to captured evidence.
3. Missing auth/model/permission/task capability, active cooldown, or unavailable
   machine resources cannot be compensated by high quota.
4. Replaying the same snapshot/policy/task produces an identical candidate set
   and digest.
5. Fixture matrix covers all official providers, unknown/stale evidence, aliases,
   owner override, and concurrent capacity.

### Verification and boundaries

Pure policy/golden/property tests, no provider/network. No soft score, burn policy,
fallback, or launch. Done when only hard-eligible candidates reach V090-052.

---

## V090-052: Quota normalization, burn urgency, reserve, and reliability policy

**Metadata:** code, size M; depends on V090-041, V090-043, V090-045, V090-047,
V090-051, and V090-108; labels `v0.9.0`, `routing`, `quota`, `policy`; exclusive
in soft route policy.

### Outcome and rationale

Use paid capacity intelligently without wasting near-reset quota or exhausting a
provider needed for later high-risk work. Compare heterogeneous windows only
through normalized urgency/reliability features, never fake token equivalence.

### Scope and constraints

- Compute per-window remaining fraction when exact, time-to-reset, burn urgency,
  reservation floor, freshness/confidence, recent reliability, rate-limit state,
  and current concurrency.
- Prefer usable capacity likely to expire, subject to task capability and
  configurable reserves for Tera/Soul work.
- Treat five-hour, weekly, credits, and rate-limit windows as separate constraints;
  a route is bounded by its most relevant scarce window.
- Unknown/stale evidence receives explicit uncertainty policy, not numeric zero.
- Version weights/policy and expose sensitivity inputs.

### Acceptance criteria

1. Near-reset abundant eligible capacity is preferred over equivalent capacity
   that resets later, unless reserve/reliability policy explains otherwise.
2. A route with exhausted required window, active rate limit, or reserve breach is
   excluded/deprioritized with exact reason; unrelated window surplus cannot mask it.
3. Unknown/stale/estimated/exact evidence follows documented distinct policy and
   never fabricates comparable absolute tokens across providers.
4. Recent failure/cooldown and concurrency influence soft ranking without changing
   immutable pin or hard eligibility semantics.
5. Golden scenarios cover abundant-to-expire, weekly scarcity, reserved Soul
   capacity, partial windows, no telemetry, ties, and reset boundaries.

### Verification and boundaries

Injected-clock pure policy tests; no live quotas/provider. No route persistence,
fallback, or quota spending reservation beyond existing resource facts. Done when
V090-053 receives an ordered score breakdown rather than opaque ranking.

---

## V090-099: Quota policy modes, soft reservations, and usage attribution

**Metadata:** code, size M; depends on V090-016 and V090-052; labels `v0.9.0`,
`quota`, `routing`, `budget`; exclusive in machine-wide quota spending authority.

### Outcome and rationale

Prevent several projects or candidate runs from all spending the same observed
remaining window, and let the owner choose whether to burn expiring capacity,
balance providers, or preserve premium capacity. Post-attempt evidence reconciles
the estimate instead of pretending quota telemetry is exact accounting.

### Scope and constraints

- Define owner-selectable `balanced`, `burn-before-reset`, and
  `preserve-premium` policy modes with explicit reserves and completion headroom.
- Create short-lived soft reservations against one normalized provider/account/
  model/window snapshot before automatic launch.
- Include active/pending project reservations in candidate ranking and expose
  overcommit/unknown risk.
- Attribute observed post-attempt usage/receipts to the route and reconcile or
  expire reservations without rewriting provider evidence.
- Explicit pins still win or fail closed; policy modes never substitute a pinned
  provider/model/effort.

### Acceptance criteria

1. Two concurrent candidates cannot both treat the same unreserved remaining
   capacity as fully available; reservation conflict is deterministic.
2. Each policy mode produces documented, replayable rankings and reserve behavior
   from the same evidence snapshot.
3. Completion-headroom rejection distinguishes estimated demand, unknown quota,
   exact exhaustion, and owner-approved risk acceptance.
4. Terminal/cancel/timeout/start-refusal releases or reconciles reservations
   idempotently; stale reservations expire with visible evidence.
5. Usage attribution records source/confidence and drift without fabricating
   provider-authoritative remaining values from local token counts.

### Verification and boundaries

Injected-clock multi-project reservation races, policy goldens, crash/reopen,
stale expiry, cancel, and usage reconciliation. No live provider, billing claim,
credential access, hard monetary budget, or cross-machine reservation.

---

## V090-053: Persisted route decision and `route explain`

**Metadata:** code, size M; depends on V090-038, V090-051, V090-052, and
V090-099; labels `v0.9.0`, `routing`, `explainability`, `cli`; exclusive in
route decision record.

### Outcome and rationale

Select one route deterministically, copy the exact evidence/policy digests into
the project event stream, and let users inspect why each provider/model was
selected, skipped, or unavailable.

### Scope and constraints

- Define decision request/snapshot/candidates/winner/tie-break/policy version and
  evidence references.
- Apply stable tie-break after hard eligibility and soft policy.
- Persist decision before launch; actual launch proves the same digest.
- Add `loopcoder route explain` for pending or historical decisions with human and
  JSON output.
- Redact other projects/accounts and never expose credentials/raw quota payloads.

### Acceptance criteria

1. Same task, policy, and evidence snapshot always produce the same ordered
   candidates, winner, reasons, and digest.
2. Explanation lists explicit pin precedence or all hard exclusions, soft score
   components, quota windows/reset/freshness, reserves, reliability, and tie-break.
3. Project events retain immutable snapshot IDs/digests sufficient to replay a
   historical decision after machine observations change.
4. No eligible candidate yields a typed no-route/needs-human result and launches
   nothing.
5. Requested decision digest and actual attempt route digest must match.

### Verification and boundaries

Golden replay/explain/redaction tests and remote race. No provider launch or
fallback. Done when route choice is an auditable project fact, not hidden router
state.

---

## V090-054: Successor attempt and fallback boundary

**Metadata:** code, size M; depends on V090-032 and V090-053; labels `v0.9.0`,
`routing`, `recovery`, `fallback`; exclusive in post-failure reroute lifecycle.

### Outcome and rationale

Define when failure may create a new attempt using another eligible route. Never
switch provider/model inside an active attempt or replay a provider after an
ambiguous launch just because another company has quota.

### Scope and constraints

- Classify pre-launch definitive failure, provider-declined/not-started, launched
  definitive terminal failure, ambiguous execution, quota/rate-limit, and policy
  change.
- Create a successor attempt with causal link, new route decision, idempotency key,
  and owner/policy authorization.
- Explicit pin has no automatic cross-route fallback unless the owner pre-authorized
  a named ordered policy.
- Preserve prior attempt worktree/log/events; define reuse only when proven safe.
- Bound automatic successors and stop after one configured retry by default.

### Acceptance criteria

1. No route changes inside an attempt; every change creates a separately visible
   successor with new decision/approval.
2. Ambiguous launch/execution never auto-falls back and becomes needs-human.
3. Proven pre-launch failure may select an authorized successor without counting
   provider execution; terminal failure follows explicit retry policy.
4. Prior attempt evidence and side effects remain linked and cannot be overwritten
   by the successor.
5. Retry/fallback budget, candidate exclusions, and final stop reason are visible
   in status/explain output.

### Verification and boundaries

Fixture failures at each launch/delivery boundary, concurrent successor requests,
and route replay. No real provider. No workflow children or model-internal switch.
Done when fallback is explicit finite attempt history, not invisible mutation.

---

## V090-055: Smart-routing end-to-end acceptance canary

**Metadata:** test/docs, size M; depends on V090-040, V090-041, V090-042,
V090-043, V090-044, V090-045, V090-046, V090-047, V090-049, V090-050,
V090-051, V090-052, V090-053, V090-054, V090-099, V090-103, V090-104,
V090-105, V090-106, V090-107, V090-108, and V090-109; labels `v0.9.0`,
`acceptance`, `routing`,
`provider`; exclusive P4 checkpoint.

### Outcome and rationale

Prove explicit and automatic routes across Codex, Claude, Gemini/Antigravity, and
Grok using deterministic installation/model/quota/health fixtures, plus bounded
opt-in real observations. Verify that abundant expiring capacity is useful without
violating pins, capability, reserves, or reliability.

### Scope and constraints

- Exercise installed/missing/auth-unknown, aliases, task classes, multi-window
  quota, reset, cooldown, reliability, concurrency, no telemetry, and ties.
- Prove exact pin, automatic winner, no route, and authorized successor behavior.
- Run direct path through every fake official observation/invocation/quota adapter
  and archive decision manifests.
- Exercise policy modes, concurrent soft reservations, completion headroom,
  terminal release, and usage attribution.
- Demonstrate future synthetic provider registration without core routing edits.
- Optional real observation smoke is redacted, short, and never required for PR CI.

### Acceptance criteria

1. Explicit pins across all supported adapters remain unchanged or fail closed;
   no fixture shows silent company/model/effort substitution.
2. Automatic routing uses only hard-eligible candidates and prefers expiring
   usable capacity according to documented reserve/reliability policy.
3. Every winner/exclusion is replayable from persisted evidence/policy digests and
   `route explain` matches the decision.
4. Provider/source failure, unknown quota, rate limit, and ambiguous launch lead
   to documented finite behavior with no busy polling or duplicate execution.
5. Canary stays within machine resource budget, leaves no child/repo-local state,
   and emits a redacted exact-SHA evidence manifest.

### Verification, failure, and non-goals

Hosted Darwin deterministic matrix and optional owner-approved observations.
Flakes or unexplained decisions block P5. No work graph, provider-native child
ownership, self-bootstrap, or release. Done when owner accepts the manifest and
the capability matrix names real versus fixture-only provider evidence.
