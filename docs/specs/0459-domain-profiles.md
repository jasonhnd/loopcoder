---
id: 459
title: Domain profiles
status: draft
date: 2026-07-04
issue: 459
pr: null
supersedes: []
superseded_by: []
---

# Domain profiles

This is a design-only spec for loopcoder 0.5.0. This PR adds only this document:
no Go code, no `.delivery.yml` change, no command behavior change, and no new
runtime dependency. Code slices are filed only AFTER this spec merges, per
[`docs/PROCESS.md`](../PROCESS.md).

## Goal

Generalize loopcoder from a code-delivery loop into a general autonomous-delivery
engine for any verifiable, repo-based, AI-doable work: documents, content, data,
governance packets, reports, and code. Code becomes the first domain profile, not
the definition of the engine.

The core engine remains unchanged. `tick`, `compile`, `dispatch`,
`dispatch-wave`, `loopreview`, `risk-gate`, `promote`, guardrails, watchdog,
relay, recovery, and reporting keep their current ordering and authority. 0.5.0
adds configurable plug points that those existing stages consume.

## Foundation and preserved invariants

This spec builds directly on the "Seams reserved for 0.5.0" chapter in
[`0161-autonomous-delivery-loop.md`](0161-autonomous-delivery-loop.md). The 0.5.0
domain profile must honor all of these constraints:

- M1: add plug points only at existing consumed edges. Do not pre-build unused
  abstractions.
- M2: the self-hosting guard remains first. Any change to loopcoder core,
  including domain-support machinery, routes `needs-human` and takes effect only
  after a human rebuild and tick restart.
- M3: every new machine-facing schema is additive, optional, snake_case,
  `omitempty`-safe, `Default()`-safe, and unknown-field-tolerant on read.
- M4 and F1-F5: failsafes may only be added to, never degraded. 0.4.1's accepted
  amendment to F3 is in force: promotion remains a distinct step and tick still
  has no production merge capability; the gate may be human or automatic per the
  accepted E2 spec.
- E1: `agent.Runner` remains the only provider door, `agent.Invocation` remains
  the provider-neutral request contract, and `Invocation.ReadOnly` remains the
  one permission boundary.

The 0.4.2 H5 loopreview verdict/exit-code contract is preserved. The verifier
still returns the closed verdict enum `pass`, `fail`, or `needs-human`; clean
verdict exit codes remain `0`, `1`, and `2`, distinct from command/runtime
failures.

## Domain profile bundle

A project declares its domain through a new optional top-level `.delivery.yml`
section:

```yaml
domain:
  name: docs
  description: corporate IR document production
  skills:
    paths:
      - .claude/skills/*/SKILL.md
      - governance/**/skill.md
    machine_library:
      paths:
        - .loopcoder/skill-library/**/*.md
    select:
      - governance
      - disclosure
    prompt_budget_bytes: 4096
  verification:
    rubric:
      paths:
        - governance/qa-checklist.md
      checklist:
        - "Rendered PDF matches the approved governance spec."
    review_packet_order:
      - rendered_artifact
      - rubric
      - changed_files
      - diff
      - issue
      - spec
  evidence:
    producer:
      command: make render-ir-pdf
      outputs:
        - out/report.pdf
      timeout_seconds: 300
      include_in_loopreview: true
  red_lines:
    - category: disclosure-compliance
      detail: unresolved disclosure or legal approval requirement
      path_globs:
        - disclosure/**
  partial_work:
    mode: harvest-needs-human
  liveness:
    mode: worktree-mtime
```

All fields are optional. The absent profile is the current code profile. An empty
`domain` section must behave exactly like today's defaults.

The profile is a bundle of plug points, not a new scheduler or worker type. It
feeds the same existing stages:

- worker prompt construction consumes `domain.skills`;
- loopreview packet construction consumes `domain.verification`,
  `domain.evidence`, and rendered artifacts;
- risk-gate consumes `domain.red_lines`;
- worker watchdog/recovery consumes `domain.partial_work` and
  `domain.liveness`;
- report rendering surfaces configured and produced evidence.

## Plug point 1: configurable skill sources

Current anchor: repo skill discovery is hardcoded in
`internal/skills/skills.go` to immediate children matching
`.claude/skills/*/SKILL.md`; `worker.DefaultDeps` injects that metadata through
`skills.BuildPromptSection` before the worker prompt is built.

0.5.0 adds `domain.skills` as an additive extension:

- `paths` is an ordered list of repo-relative file globs. The current
  `.claude/skills/*/SKILL.md` rule is the default first entry when absent.
- `machine_library.paths` is an optional machine-readable skill library. It may
  contain generated or centrally managed skill metadata, but it is still read
  from repo-controlled or explicitly configured local paths. It does not create a
  network fetcher.
- `select` optionally filters discovered skills by normalized name, path stem, or
  declared tag. Empty `select` means "include all discovered skills within the
  prompt budget."
- `prompt_budget_bytes` defaults to the current
  `DefaultPromptBudgetBytes`.

Discovery must remain metadata-first and bounded. Workers may be told where
skills live, but the prompt must not blindly inline entire skill bodies beyond the
budget. This fixes the current inability to discover a domain skill such as
`governance/skill.md` without changing worker orchestration.

## Plug point 2: injectable verification rubric

Current anchor: `internal/loopreview/loopreview.go` hardcodes a code-oriented
review contract in `formatReviewPrompt`, uses the closed
`VerdictJSONSchema`, and maps verdicts through `ExitCodeForVerdict`.

0.5.0 adds `domain.verification.rubric`:

- `paths` names repo files whose contents are copied into a bounded "Rubric"
  packet section.
- `checklist` supplies small inline checklist items for repos that do not need a
  separate file.
- Missing configured rubric files are missing evidence. The verifier must return
  or be forced to `needs-human`, not pass from an incomplete packet.

The rubric changes what the verifier judges, not how verdicts work. The JSON
schema keeps the existing verdict enum and `spec_conformance` field. The prompt
must instruct the verifier to evaluate the PR against the issue, merged spec, and
domain rubric. It must not add a fourth verdict, overload `pass`, or weaken H5's
exit-code split.

## Plug point 3: evidence producer fed to the verifier

Current anchors: `.delivery.yml evidence` maps to `config.Evidence` and
`EvidenceArtifact`; tick passes `cfg.Evidence.Artifacts()` into
`ConfiguredEvidence`, which is surfaced in reports. The verifier packet currently
does not consume those configured artifacts. `verification.browser` is a
website-shaped special case in `config.Verification.Browser`.

0.5.0 adds `domain.evidence.producer`:

- `command` runs in the PR worktree after worker output is available and before
  `loopreview` builds its packet.
- `outputs` is an allow-list of repo-relative produced files or directories.
- `timeout_seconds` is optional and bounded by existing watchdog behavior.
- `include_in_loopreview` controls whether produced artifacts are attached to the
  verifier packet. The safe default is true when a producer is configured.

The produced artifact is a generic rendered artifact. For text, Markdown, JSON,
CSV, and HTML, loopreview may include bounded content directly. For binary
formats such as PDF, loopreview includes a manifest plus a deterministic text
extraction or render summary when configured by the slice. If the producer fails,
times out, or declares an output that is absent, the item routes `needs-human`.

`verification.browser` becomes a compatibility profile for one rendered-artifact
producer class. Browser preview remains valid for website repos, but the engine
speaks in generic rendered artifacts so document, data, and content domains can
feed their actual product to the verifier.

## Plug point 4: append-only domain red lines

Current anchor: `internal/orchestration/risk_gate.go` hardcodes destructive,
build-not-green, and loopcoder-core red lines. It already accepts
`RiskGateOptions.AdditionalRedLines`, normalizes them, and can only raise risk.
`TickOptions.AdditionalRiskRedLines` is passed into that field.

0.5.0 wires `.delivery.yml domain.red_lines[]` into that existing path:

- Domain red lines append to the deterministic floor. They may only add vetoes.
- A domain red line can carry `category`, `detail`, and optional matcher fields
  such as `path_globs`.
- Empty or malformed entries are ignored only when they do not express a real
  rule. A syntactically invalid matcher is a configuration error, not a silent
  pass.
- Domain red lines cannot remove, rename, or suppress the built-in destructive,
  build-not-green, or loopcoder-core red lines.
- The loopcoder-core red line is untouchable. A domain profile cannot classify
  loopcoder core as safe or bypass the self-hosting guard.

The ordering stays the 0161 floor: red lines are evaluated before and
independently of any promotion gate. A gate policy may veto more; it may never
authorize what the red-line floor blocked.

## Plug point 5: MCP servers

Current anchors: all worker and verifier provider calls flow through
`agent.Runner.Run(ctx, agent.Invocation)`. `agent.Invocation` already carries
`WorktreePath`, `Prompt`, `Model`, `Effort`, `ReadOnly`, `OutputSchema`,
`LogPath`, `Stderr`, watchdog caps, `RunID`, and `Role`. Provider-specific
tool flags stay private in `BuildCodexArgs`, `BuildClaudeArgs`, and
`BuildGeminiArgs`.

0.5.0 adds two pure additions:

```yaml
mcp:
  servers:
    - name: governance-index
      transport: stdio
      command: ./tools/governance-mcp
      args: ["--root", "."]
      roles: [worker, verifier]
      read_only: true
    - name: disclosure-system
      transport: http
      url: https://mcp.example.com/disclosure
      auth:
        header: Authorization
        env: DISCLOSURE_MCP_TOKEN
      roles: [worker]
      read_only: false
```

and an optional append-only `MCPServers`-style field on `agent.Invocation`.
Absent means no MCP, preserving every current invocation literal.

Normative rules:

- Support local stdio servers and external HTTP servers.
- Remote HTTP auth must come from environment variables or configured secret
  references. Tokens, bearer strings, cookies, and API keys must never be
  hardcoded in `.delivery.yml`, prompts, PR bodies, state records, or reports.
- `roles` gates which invocations receive the server. Unknown roles are config
  errors.
- `read_only` is loopcoder's local classification, not a server self-report.
  Verifier invocations, where `Invocation.ReadOnly` is true, receive only servers
  locally classified `read_only: true`.
- Never trust a server's advertised "read only" capability. A remote server can
  still cause external side effects outside the local sandbox. If the operator has
  not locally classified it read-only, it is not available to the verifier.
- `Invocation.ReadOnly` remains the one permission boundary. Providers must map
  MCP setup into their most restrictive native mode when `ReadOnly` is true and
  must fail closed if a configured MCP server cannot be represented safely.
- Provider-specific MCP flags, config files, and environment variables stay
  inside the provider runners. Worker, verifier, tick, compile, and recover pass
  provider-neutral invocation data only.

Adding MCP plumbing changes loopcoder core. Under the self-hosting guard, those
code slices route `needs-human` and take effect only after rebuild and tick
restart when loopcoder is developing itself.

## Fold-ins from 0.4.2 pluggability

0.4.2 fixed code-shaped reliability bugs. 0.5.0 makes the parts that are
domain-shaped configurable without weakening the fixes.

### Partial-work acceptance

Current anchor: H1 harvest opens a `needs-human` PR with salvaged work when a
hung or killed worker has committable changes.

`domain.partial_work.mode` configures the domain policy. The initial values are:

- `harvest-needs-human` (default): today's behavior. Salvaged work opens a PR
  that references but does not close the issue and is never auto-merged.
- `report-only`: preserve artifacts and report them, but do not open a PR. This
  is for domains where partial artifacts are misleading or legally unsafe.

No mode may auto-merge harvested work or mark the issue complete.

### Liveness mode

Current anchor: H2 worker liveness uses log growth plus worktree file mtime. That
is correct for code and document production, but weak for remote-effect or API
domains whose meaningful progress may not write the worktree.

`domain.liveness.mode` configures the signal:

- `worktree-mtime` (default): current log plus worktree activity.
- `log-only`: use provider log growth only.
- `custom`: run a configured read-only liveness command whose output is logged
  and bounded by the watchdog.

Liveness configuration changes only hang classification. It does not disable the
hard cap, guardrails, relay, or partial-work safety net.

### Review-packet section ordering

Current anchor: H3 made `ReviewPacketLimits.GeneratedPatterns` and
`sourceFirstDiffPatches` code-friendly by putting source/config before generated
artifacts.

`domain.verification.review_packet_order` configures the top-level packet section
order. The code profile default remains source-first. A docs profile can put
`rendered_artifact` and `rubric` before diff excerpts; a data profile can put
evaluation output before source changes. Each section remains bounded, and
truncation markers remain mandatory evidence.

## Validation target

The proving project for 0.5.0 is a corporate IR document-production repo using a
`docs` domain profile:

- Governance spec: the merged design/spec is the authoritative governance
  contract.
- QA rubric: a corporate QA checklist is injected as the verification rubric.
- Deterministic checks: CI validates linting, links, schema, and any required
  document checks.
- Rendered evidence: a producer renders the final PDF and feeds the rendered PDF
  summary into loopreview.
- Disclosure/compliance red lines: domain red lines route unresolved disclosure,
  compliance, or legal-approval changes to `needs-human`.
- Human approval: promotion remains the approval point for the IR artifact under
  the configured gate.

Success means the unchanged loop can take a repo issue for an IR document change,
dispatch a worker, render the PDF, review the produced artifact against the
governance spec and QA rubric, apply disclosure red lines, and surface the result
for promotion.

## Follow-up code issues

File these after this spec merges, in dependency order:

1. **Config schema for domain profiles and MCP.** Add optional `domain` and `mcp`
   structs to `internal/config`, defaults, parse tests, unknown-field tolerance,
   scaffold comments, and docs. No behavior wiring yet. Binds 0161 M3.
2. **Configurable skill discovery.** Extend `internal/skills` and
   `worker.DefaultDeps` to consume `domain.skills.paths`,
   `machine_library.paths`, `select`, and budget. Preserve the current
   `.claude/skills/*/SKILL.md` default. Binds plug point 1.
3. **Domain rubric and review-packet ordering.** Add bounded rubric loading and
   packet section ordering to `internal/loopreview` while preserving
   `VerdictJSONSchema`, verdict strings, and H5 exit codes. Binds plug point 2
   and the H3 fold-in.
4. **Rendered-artifact evidence producer.** Run the configured producer in the PR
   worktree, collect declared outputs, surface artifacts in reports, and feed a
   bounded rendered-artifact section into loopreview. Generalize
   `verification.browser` as a compatibility rendered-artifact producer. Binds
   plug point 3.
5. **Domain red-line wiring.** Convert `domain.red_lines[]` into
   `RiskGateOptions.AdditionalRedLines` using strict matcher validation. Prove the
   built-in destructive/build/core red lines cannot be lowered or bypassed. Binds
   plug point 4 and 0161 M2/M4.
6. **MCP invocation contract.** Add optional MCP server structs to config loading
   and a pure-append MCP field to `agent.Invocation`; plumb worker and verifier
   invocations without provider flags yet. Absent MCP must be byte-compatible with
   current callers. Binds 0161 E1 and M3.
7. **Provider MCP plumbing.** Teach codex, claude, and gemini runners to translate
   invocation MCP servers into their native MCP configuration for stdio and HTTP.
   Enforce env/secret auth, role filters, and local read-only classification for
   verifier invocations. Fail closed when `ReadOnly` cannot be represented safely.
   Binds plug point 5 and 0161 E1/F2.
8. **Domain partial-work and liveness policies.** Wire
   `domain.partial_work.mode` and `domain.liveness.mode` into worker recovery and
   supervised execution. Preserve hard caps, guardrails, and the rule that
   salvaged work is never auto-merged. Binds the H1/H2 fold-ins.
9. **Docs-domain validation slice.** Add examples/docs and prove the validation
   target on the corporate IR document-production repo: governance spec, QA
   rubric, deterministic CI, rendered PDF evidence, disclosure/compliance red
   lines, and promotion approval.

Every code slice above changes loopcoder core. When loopcoder applies these
slices to itself, the self-hosting guard must classify them as core changes,
route them `needs-human`, and require rebuild plus tick restart before they can
affect a running loop.

## Relationship to existing specs

- [`0161-autonomous-delivery-loop.md`](0161-autonomous-delivery-loop.md) is the
  parent. This spec realizes E1 MCP and the 0.5.0 domain-generalization use case
  while preserving M1-M4 and the failsafe floors.
- [`0403-e2-auto-promote-production.md`](0403-e2-auto-promote-production.md) has
  already shipped E2. This spec does not change the promotion gate or teach tick
  to merge production.
- [`0423-operational-reliability-hardening.md`](0423-operational-reliability-hardening.md)
  supplies H1-H5. This spec keeps H5 fixed and makes H1-H3 domain-configurable.
- [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md)
  remains the verifier foundation. Domain rubrics change verifier inputs, not the
  verdict contract.
- [`0146-attestation.md`](0146-attestation.md),
  [`0306-local-only-attestation.md`](0306-local-only-attestation.md), and
  [`0447-relay-enforcement-hardgate.md`](0447-relay-enforcement-hardgate.md)
  remain unchanged. Domain profiles do not copy local-only attestations into repo
  artifacts and do not bypass relay.

## Non-goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` change in this design-doc PR.
- No behavior change in this design-doc PR.
- No redesign of tick, dispatch, loopreview, risk-gate, promote, guardrails,
  watchdog, relay, or recovery.
- No weakening of the self-hosting guard, ReadOnly verifier boundary, red-line
  floor, relay obligations, or H5 exit-code contract.
- No multi-project scheduler.
- No new production-promotion policy.
