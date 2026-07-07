---
id: 567
title: 0.6.0 Unit B - Reporter
status: draft
date: 2026-07-07
issue: 567
pr: null
supersedes: []
superseded_by: []
---

# 0.6.0 Unit B - Reporter

This is a design-only spec for loopcoder 0.6.0 Unit B. This PR adds only this
document: no Go code, no `.delivery.yml` change, no command behavior change, no
reference-doc update, and no new dependency. Code and reference-documentation
work must be filed separately after this spec merges, per
[`docs/PROCESS.md`](../PROCESS.md).

Unit B performs the BREAKING public rename from `attestation` to `reporter` and
adds light reporter strengthening. The rename must be visible to operators: a
subsystem that still emits `[attestation]` has not been renamed.

## Goals

- Rename the live attestation subsystem to reporter across Go packages,
  public Go identifiers, emitted headers, pretty wording, config/environment
  names, and current reference/manual prose.
- Change the operator-visible one-line header token from `[attestation]` to
  `[reporter]`.
- Update every live emitter, parser, matcher, relay guard, hook, manual, and
  reference-doc consumer in lockstep.
- Keep this release safe across upgrade lag by accepting both `[attestation]`
  and `[reporter]` in relay and conductor hook matchers.
- Freeze historical and invisible-machine contracts that do not buy operator
  clarity when renamed.
- Preserve Unit A's Antigravity reporter validation leniency without relaxing
  `codex` or `claude`.
- Strengthen reports with model+depth display, a work ID, contextual fields,
  observed-ground-truth precedence, a read-only `loopcoder report` command, and
  grouped pretty output.

## Non-Goals

- No Go implementation in this design PR.
- No rewrite of `CHANGELOG.md` history.
- No rewrite of shipped `docs/specs/*` history.
- No rename of the `.attest` relay ledger file extension.
- No rename of existing `Report.CanonicalJSON()` field names.
- No weakening of relay hard-gate behavior.
- No weakening of `codex` or `claude` parsed-model and usage requirements.
- No cost estimation where loopcoder has no exact observed cost.

## Rename Map

The follow-on code issues must apply this map to live, non-frozen surfaces.

| Current | 0.6.0 Unit B target | Notes |
|---|---|---|
| `internal/attestation` | `internal/reporter` | Directory, package name, docs, and imports move together. |
| `attestation` package qualifier | `reporter` | All live Go imports and call sites update. |
| `AttestationRecord` | `Report` | The shared Worker, Verifier, Conductor, and audit review record type. |
| `attestationPresence` | `reportPresence` | Private helper rename. |
| `cloneAttestation`, `parseAttestation`, `buildWorkerAttestation`, etc. | `cloneReport`, `parseReport`, `buildWorkerReport`, etc. | Every live Go identifier containing the noun `Attestation` or `attestation` must be renamed unless it is explicitly frozen below. |
| `ValidationError` messages saying `invalid attestation record` | `invalid report` or `invalid reporter record` | Prefer `invalid report` in user-facing errors. |
| `[attestation]` emitted header token | `[reporter]` | `Report.Header()` emits only `[reporter]` in this release. Matchers accept both during the transition window. |
| Pretty status line `attestation: verified` | `report: verified` | Emoji mode keeps the status icon but changes wording from attestation to report. |
| Pretty status line `attestation: self-reported` | `report: self-reported` | Preserve the trust distinction. |
| Prose `attestation block` | `report block` | Apply to current `README.md`, `SKILL.md`, `AGENTS.md`, `GEMINI.md`, `hooks/*`, and `docs/reference/*`. |
| Prose `attestation record` | `report` | Use `report` for the record and `reporter` for the subsystem. |
| Result envelope key `attestation` on newly emitted command JSON | `report` | This is a BREAKING live command-output rename. Readers in this release must accept old `attestation` as a legacy input alias where they parse persisted or prior-run output. |
| Persisted run-state field `attestation` in newly written attempt/review records | `report` | New writes use `report`; readers accept both `report` and legacy `attestation` so existing `.loopcoder/` state remains readable. |
| `.delivery.yml` or future config keys containing `attestation` | `reporter` | No tracked `.delivery.yml` key with this name exists today; new keys must use `reporter`. |
| Hook/environment keys `LOOPCODER_CONDUCTOR_ATTEST_SCOPE`, `LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR` | `LOOPCODER_CONDUCTOR_REPORTER_SCOPE`, `LOOPCODER_CONDUCTOR_REPORTER_STATE_DIR` | 0.6.0 accepts both names, preferring the new name when both are set. Remove old-name acceptance one version later with the old token acceptance. |
| Hook state label `conductor-attest` where persisted under `.loopcoder/hooks/` | `conductor-reporter` | 0.6.0 reads old state if new state is absent, to avoid a one-turn lock-out after upgrade. |
| Hook command `loopcoder hook conductor-attest` in installed settings | `loopcoder hook conductor-reporter` | `loopcoder skill install` writes the new command. The old command remains an alias for one version so existing host settings do not lock out. |

The existing `attest` command verb is a compatibility surface. 0.6.0 must keep
`loopcoder attest` working as an alias for the Conductor self-report emitter so
old manuals and installed hooks remain recoverable. Current docs must describe
the output as a Conductor report, not an attestation. A later spec may remove or
rename the alias after the reporter transition window.

## Frozen Surfaces

These surfaces must not be renamed by Unit B:

- `CHANGELOG.md`: it is release history and must keep historical wording.
- Shipped `docs/specs/*`: accepted historical specs are immutable except for
  the lifecycle frontmatter fields allowed by `docs/PROCESS.md`.
- Existing `Report.CanonicalJSON()` field names: `role`, `provider`, `model`,
  `model_source`, `effort`, `permission`, `action`, `exit_code`, `started_at`,
  `ended_at`, `duration_ms`, `usage`, and `verified` keep their names and
  meanings. Additive optional fields are allowed only as specified below; do
  not rename existing fields.
- `Usage` JSON field names: `input_tokens`, `output_tokens`, and
  `total_tokens` keep their names.
- The `.attest` relay ledger extension under `.loopcoder/relay/`. This is
  invisible machinery, and renaming it adds upgrade risk for no operator gain.
- Historical `.loopcoder/` files already written with `attestation` fields or
  `[attestation]` headers. Readers must continue to handle them during the
  transition and migration window; writers use new names except for the frozen
  `.attest` extension.

Future specs may refer to historical attestation specs by filename or title
when discussing provenance. They must not rewrite the historical files.

## Current Consumer Inventory

The inventory below was generated from the current main tree with:

```text
git grep -n -i "attestation" -- .
git grep -n -i -E "attestation|attest" -- .
```

The current tree contains 1346 `attestation` matches across 87 tracked files,
and 1585 `attestation|attest` matches across 94 tracked files. The issue body
estimated roughly 1068 refs / 60 files; implementation must use the live grep
inventory at the time of the code sweep, not the estimate.

### Lockstep Token Sites

The header token is emitted in exactly one shared place:

- `internal/attestation/attestation.go`: `AttestationRecord.Header()` becomes
  `internal/reporter.Report.Header()` and emits `[reporter]`.

The token is matched in lockstep in these live safety gates:

- `internal/conductorhooks/relay_guard.go`: `ledgerHeaderRe` must accept both
  `[attestation]` and `[reporter]` for Worker and Verifier records in 0.6.0.
- `internal/conductorhooks/relay_guard.go`: `containsSurfacedAttestation`
  becomes `containsSurfacedReport`; its dynamic `rolePattern` must accept both
  tokens for the discovered role in 0.6.0.
- `internal/conductorhooks/relay_guard.go`: helper names and user-facing block
  text change from attestation to report, but ledger discovery still walks
  `*.attest`.
- `internal/conductorhooks/attest.go`: `conductorHeaderRe` must accept both
  `[attestation]` and `[reporter]` for Conductor records in 0.6.0.
- `internal/conductorhooks/attest.go`: Conductor hook wording changes to
  report/reporter while the `loopcoder attest` compatibility alias still
  satisfies the gate.

The token and relay obligation are instructed in lockstep in:

- `SKILL.md`;
- `AGENTS.md`;
- `GEMINI.md`;
- `hooks/README.md`;
- `hooks/claude-settings.snippet.json`;
- `.claude/settings.json` templates or fixtures maintained by this repo; and
- `internal/claudehooks/*` installer tests and renderers that write hook
  settings.

### Live Go And Test Consumers To Rename

These files contain live code or tests and must be swept unless a specific
field/extension is frozen above.

| Matches | File |
|---:|---|
| 2 | `examples/docs_domain_validation_test.go` |
| 2 | `internal/agent/agent.go` |
| 1 | `internal/agent/codex_test.go` |
| 6 | `internal/agent/gemini.go` |
| 2 | `internal/agent/parse.go` |
| 21 | `internal/attestation/attestation.go` |
| 24 | `internal/attestation/attestation_test.go` |
| 2 | `internal/attestation/doc.go` |
| 12 | `internal/attestation/pretty.go` |
| 6 | `internal/attestation/pretty_test.go` |
| 21 | `internal/audit/review.go` |
| 10 | `internal/audit/review_test.go` |
| 2 | `internal/audit/types.go` |
| 2 | `internal/claudehooks/settings.go` |
| 3 | `internal/claudehooks/settings_test.go` |
| 13 | `internal/cli/audit.go` |
| 17 | `internal/cli/audit_test.go` |
| 94 | `internal/cli/cli.go` |
| 107 | `internal/cli/cli_test.go` |
| 3 | `internal/cli/conductor_hook_e2e_test.go` |
| 8 | `internal/cli/pretty.go` |
| 11 | `internal/conductorhooks/attest.go` |
| 3 | `internal/conductorhooks/attest_test.go` |
| 3 | `internal/conductorhooks/relay_escape_test.go` |
| 18 | `internal/conductorhooks/relay_guard.go` |
| 14 | `internal/conductorhooks/relay_guard_test.go` |
| 2 | `internal/conductorhooks/shared.go` |
| 2 | `internal/guardrails/budget.go` |
| 6 | `internal/guardrails/budget_test.go` |
| 15 | `internal/loopreview/loopreview.go` |
| 27 | `internal/loopreview/loopreview_test.go` |
| 4 | `internal/orchestration/dispatch_wave.go` |
| 4 | `internal/orchestration/dispatch_wave_render.go` |
| 42 | `internal/orchestration/dispatch_wave_test.go` |
| 9 | `internal/orchestration/orchestration_render_test.go` |
| 1 | `internal/orchestration/risk_gate.go` |
| 2 | `internal/orchestration/risk_gate_test.go` |
| 6 | `internal/orchestration/tick.go` |
| 2 | `internal/recovery/recovery.go` |
| 25 | `internal/recovery/recovery_test.go` |
| 7 | `internal/relay/relay.go` |
| 1 | `internal/relaygate/relaygate.go` |
| 39 | `internal/runstatus/runstatus.go` |
| 28 | `internal/runstatus/runstatus_test.go` |
| 10 | `internal/state/state.go` |
| 22 | `internal/state/state_test.go` |
| 42 | `internal/worker/worker.go` |
| 63 | `internal/worker/worker_test.go` |

Implementation must also sweep files containing only `attest` verb references
where they are live compatibility surfaces:

- `.claude/settings.json`;
- `internal/cli/hook.go`;
- `internal/cli/skill_test.go`;
- `internal/doctor/doctor_test.go`; and
- any newly added Unit A Antigravity files containing reporter/attestation
  references.

### Current Reference And Manual Consumers To Update

These files are current operator/reference material and must switch to reporter
language unless a line explicitly discusses historical releases:

| Matches | File |
|---:|---|
| 3 | `AGENTS.md` |
| 3 | `GEMINI.md` |
| 18 | `SKILL.md` |
| 10 | `README.md` |
| 3 | `docs/reference/architecture.md` |
| 3 | `docs/reference/audit.md` |
| 33 | `docs/reference/usage.md` |
| 22 | `docs/reference/worker.md` |
| 1 | `docs/security/audit-rubric.md` |
| 6 | `hooks/README.md` |

`ROADMAP.md` is planning source and may keep historical planning text until a
separate roadmap-maintenance issue decides otherwise. It is not part of the
Unit B code sweep.

### Frozen Historical Consumers

These files are intentionally not swept by Unit B even though they contain
`attestation` text:

| Matches | File |
|---:|---|
| 24 | `CHANGELOG.md` |
| 22 | `docs/specs/0146-attestation.md` |
| 15 | `docs/specs/0161-autonomous-delivery-loop.md` |
| 8 | `docs/specs/0192-delivery-guardrails.md` |
| 7 | `docs/specs/0194-reliable-loopreview-verifier.md` |
| 1 | `docs/specs/0212-release-distribution-and-upgrade.md` |
| 31 | `docs/specs/0214-human-readable-attestation.md` |
| 5 | `docs/specs/0215-per-role-model-override.md` |
| 50 | `docs/specs/0218-surface-worker-attestation.md` |
| 4 | `docs/specs/0220-loopreview-new-spec-not-a-blocker.md` |
| 28 | `docs/specs/0282-default-pretty-attestation.md` |
| 18 | `docs/specs/0291-skill-propagation-on-upgrade.md` |
| 26 | `docs/specs/0296-attestation-display-polish.md` |
| 14 | `docs/specs/0300-model-attribution.md` |
| 86 | `docs/specs/0306-local-only-attestation.md` |
| 48 | `docs/specs/0316-conductor-local-enforcement.md` |
| 1 | `docs/specs/0390-process-watchdog.md` |
| 4 | `docs/specs/0403-e2-auto-promote-production.md` |
| 1 | `docs/specs/0408-verifier-stream-json.md` |
| 5 | `docs/specs/0423-operational-reliability-hardening.md` |
| 7 | `docs/specs/0447-relay-enforcement-hardgate.md` |
| 3 | `docs/specs/0459-domain-profiles.md` |
| 6 | `docs/specs/0484-security-robustness-hardening.md` |
| 12 | `docs/specs/0507-core-refactor.md` |
| 18 | `docs/specs/0518-loopcoder-audit.md` |
| 10 | `docs/specs/0533-audit-consumer-repo-usability.md` |
| 3 | `docs/specs/0535-loopreview-packet-truncation-reliability.md` |
| 4 | `docs/specs/0539-loopreview-cited-spec-not-conformance-target.md` |
| 10 | `docs/specs/0554-model-depth-selection.md` |

`docs/learnings.md` is advisory operational memory. Do not rewrite older
learning entries as part of Unit B. Future entries should use reporter
terminology.

## Transition Safety

Spec 0447 says relay enforcement must fail loud but never lock out a valid run.
Unit B therefore uses a one-release dual-token acceptance window.

### Emission

0.6.0 emits the new token only:

```text
[reporter] role=worker provider=codex model=gpt-5.5(parsed) effort=high perm=write action="implement issue #567" exit=0 dur=42s tokens=120/34|154 verified=true
```

`Report.Header()` must not emit `[attestation]` in new output. Tests that assert
the old emitted token must be updated to expect `[reporter]`.

### Matching

0.6.0 matchers accept both old and new tokens:

```text
[(attestation|reporter)] role=worker
[(attestation|reporter)] role=verifier
[(attestation|reporter)] role=conductor
```

The exact implementation may use non-capturing groups, but the role capture
must continue to capture the role value, not the token. Required sites:

- `relay_guard.go` `ledgerHeaderRe`;
- `relay_guard.go` dynamic `rolePattern` in the surfaced-output check; and
- `conductorhooks/attest.go` `conductorHeaderRe`.

The relay guard must still accept verified role JSON as a fallback exactly as
today; the token transition must not remove the JSON fallback.

### Upgrade-Lag Cases

The dual-token window must handle these cases:

- New binary, stale manuals: the binary emits `[reporter]`; old manuals saying
  "attestation" do not matter because operators relay the actual visible block.
- Old binary, new manuals: old binary emits `[attestation]`; new matchers accept
  it and `relay flush` can still recover.
- New binary, old installed hook command: `loopcoder hook conductor-attest`
  remains an alias for one version and runs the new reporter-aware hook logic.
- New installed hook command, old state: `conductor-reporter` state reads old
  `conductor-attest` state if new state is absent.
- Old `.loopcoder/relay/*.attest` ledgers: new relay discovery keeps the
  `.attest` extension and accepts old headers.

### Removal

The release immediately after 0.6.0 removes `[attestation]` token acceptance,
old hook command aliases, and old environment key aliases. That follow-up must
be a separate issue and must first confirm that release notes and upgrade docs
tell operators to reinstall hooks with `loopcoder skill install --repo <repo>`.

## Report Schema And Validation

`internal/reporter.Report` is the single validated record type for Worker,
Verifier, Conductor, and audit-review reports.

The existing core fields remain:

- `role`;
- `provider`;
- `model`;
- `model_source`;
- `effort`;
- `permission`;
- `action`;
- `exit_code`;
- `started_at`;
- `ended_at`;
- `duration_ms`;
- `usage`; and
- `verified`.

Unit B adds optional report context fields:

- `work_id`: the loopcoder work identifier. For Worker reports this is the
  Worker internal `RunID` from spec 0390, the same value passed to spawned
  provider processes as `LOOPCODER_RUN_ID`. It is not the operator-facing batch
  `--run-id` flag; that flag is currently not a reliable source of per-worker
  identity and must not be presented as such. Verifier and audit reports should
  use the target Worker `work_id` when known; otherwise they use their own
  loopcoder invocation `RunID` and make that provenance explicit in the
  persisted report.
- `issue`: the GitHub issue number when known.
- `branch`: the worker or target branch when known.
- `worktree`: the absolute or repo-relative worktree path when known.
- `round`: the dispatch/tick/recovery round when known.

These fields are dispatch/tick filled and optional for backwards compatibility.
They must be populated from loopcoder state when available, not from agent
summary prose.

### Antigravity Invariant

`Report.Validate()` must preserve Unit A's provider-scoped Antigravity leniency:

- provider `antigravity` may use `model_source: self-reported` for Worker and
  Verifier reports;
- the model may be the selected Antigravity display string such as
  `Gemini 3.1 Pro (High)`;
- token usage may be empty or absent; and
- normal observed process fields still validate.

This exception is scoped to provider `antigravity` and roles Worker/Verifier.
`codex` and `claude` must still require parsed model attribution and token usage
where the current attestation contract requires them. Unit B must not turn
Antigravity's exception into a general self-reporting escape hatch.

## Observed Ground Truth

Report construction must prefer loopcoder-observed facts over agent
self-report:

1. Use resolved provider, model, and depth from loopcoder config/CLI resolution
   and model registry, not from agent prose.
2. Use process start/end time, duration, exit code, worktree, branch, issue,
   round, and work ID from loopcoder orchestration state.
3. Use parsed provider token usage and parsed provider model metadata when
   available.
4. Use agent self-report only for fields that loopcoder cannot observe, such as
   Antigravity model selection in Unit A or Conductor host-session metadata.
5. Mark self-reported fields with `model_source: self-reported` and keep
   `verified: false` for Conductor self-reports.

If observed loopcoder facts conflict with agent summary prose, the report uses
the observed facts and may include a warning outside the canonical report.

## Model And Depth Display

Pretty output and `loopcoder report` text output must display model plus depth
as one operator-readable value when depth is known:

```text
Gemini 3.1 Pro (High)
gpt-5.5 (high)
claude-opus-4-8[1m] (max)
```

Rendering rules:

- If `model` already includes the selected depth suffix, do not duplicate it.
- If `effort` is empty, display only `model`.
- Preserve exact provider depth casing (`High` for Antigravity, `high` for
  Codex).
- The canonical fields `model` and `effort` remain separately available for
  machine readers.

## Pretty Grouping

The reporter pretty renderer replaces the current flat attestation wording with
four groups: `who`, `what`, `result`, and `cost`.

Plain output shape:

```text
report: verified
who
  role        worker
  provider    OpenAI Codex / codex
  model       gpt-5.5 (high) (parsed)
what
  work_id     run-20260707-issue-567
  issue       #567
  branch      loop/issue-567
  worktree    C:\repo\.worktrees\issue-567
  action      implement issue #567
result
  exit        0
  duration    42s
  started     2026-07-07T00:00:00Z
  ended       2026-07-07T00:00:42Z
cost
  tokens      input=120 output=34 total=154
```

Emoji output may keep the existing success/warning icon convention but must use
the same group names and reporter wording. `cost` means measured usage facts.
Only display currency cost when loopcoder has an exact observed value; do not
estimate or infer prices in this unit.

Pretty output is still local diagnostic output. It remains forbidden in PR
bodies, issue comments, merge artifacts, commits, fixtures, and tracked docs
except for deliberate documentation examples.

## `loopcoder report`

Unit B adds an on-demand, read-only command:

```text
loopcoder report --repo <path> [--work-id <id>] [--issue <n>] [--role <role>] [--limit <n>] [--format text|json]
```

Behavior:

- Read only local `.loopcoder/` report/run/relay state.
- Do not call provider CLIs.
- Do not flush relay records.
- Do not mutate hook state, run state, worktrees, git, GitHub, or config.
- Default `--repo` to the current directory.
- Default `--limit` to the newest 20 reports.
- Default `--format` to `text`.
- Sort newest first by `ended_at`, then `started_at`, then file mtime when
  timestamps are absent.
- Include Worker, Verifier, Conductor, and audit-review reports when present.
- Accept legacy persisted `attestation` fields as inputs during the transition
  window, but render them as reports.

Text output must include at least:

- `work_id`;
- `role`;
- `provider`;
- model+depth display;
- `issue` when known;
- `branch` when known;
- `round` when known;
- result/exit status;
- duration;
- started/ended time; and
- token usage when known.

JSON output returns an object with a `reports` array. New JSON uses `report`
terminology. During the transition window, readers may accept old input fields,
but command output must not emit a top-level `attestation` key.

## Implementation Slices

After this spec merges, implementation should be split into separate code
issues:

1. **Core reporter rename.** Move `internal/attestation` to
   `internal/reporter`, rename `AttestationRecord` to `Report`, update imports,
   helper identifiers, result/state envelope names, and golden tests while
   preserving frozen canonical fields.
2. **Header and transition safety.** Emit `[reporter]`; update relay guard and
   conductor hook matchers to accept both tokens; keep `.attest` ledgers; add
   tests for old and new tokens, old hook aliases, old env aliases, and old
   persisted state.
3. **Manual and reference docs.** Update current `README.md`, `SKILL.md`,
   `AGENTS.md`, `GEMINI.md`, `hooks/*`, and `docs/reference/*` to reporter
   wording. Do not rewrite `CHANGELOG.md` or shipped specs.
4. **Strengthening.** Add context fields, work ID, observed-ground-truth
   precedence, model+depth display, grouped pretty output, and the read-only
   `loopcoder report` command.
5. **Inventory/golden tests.** Add or update tests that prove no live
   non-frozen `attestation` wording remains, old token parsing works for the
   transition, and frozen historical files are excluded from the sweep.

Each code issue must implement only its slice and reference this accepted spec.

## Acceptance Criteria For Implementation

- `internal/reporter.Report` replaces live uses of
  `internal/attestation.AttestationRecord`.
- `Report.Header()` emits `[reporter]` and no new live output emits
  `[attestation]`.
- `relay_guard.go` `ledgerHeaderRe` accepts both `[attestation]` and
  `[reporter]` for Worker/Verifier during 0.6.0.
- `relay_guard.go` surfaced-output `rolePattern` accepts both tokens during
  0.6.0.
- `conductorhooks/attest.go` `conductorHeaderRe` accepts both tokens during
  0.6.0.
- Old `.loopcoder/relay/*.attest` ledgers with `[attestation]` headers remain
  readable during 0.6.0.
- New ledgers keep the `.attest` extension.
- `CHANGELOG.md` and shipped `docs/specs/*` are not rewritten for terminology.
- `Report.CanonicalJSON()` preserves existing field names and meanings.
- New live command/result/state output uses `report` terminology and accepts
  legacy `attestation` input where needed for transition.
- `Report.Validate()` accepts Antigravity self-reported Worker/Verifier models
  and absent token usage without relaxing `codex` or `claude`.
- Pretty output shows model+depth display and is grouped as who/what/result/cost.
- Every newly written report carries `work_id` when loopcoder knows the
  invocation/run ID, and Worker reports use the Worker internal `RunID`.
- `issue`, `branch`, `worktree`, and `round` are populated from dispatch/tick
  state when available and omitted when unknown.
- Report construction prefers loopcoder-observed facts over agent self-report.
- `loopcoder report` lists/queries local reports read-only and never mutates
  relay, hook, git, GitHub, provider, or config state.
- `loopcoder skill install --repo <repo>` writes reporter hook names while old
  hook commands and old environment keys remain accepted for one version.

## Relationship To Existing Specs

- [`0554-model-depth-selection.md`](0554-model-depth-selection.md) is the Unit A
  prerequisite. Unit B preserves its Antigravity reporter leniency and uses its
  model/depth selection for display.
- [`0146-attestation.md`](0146-attestation.md) is historical foundation. Unit B
  renames the live subsystem but does not rewrite this shipped spec.
- [`0306-local-only-attestation.md`](0306-local-only-attestation.md) remains the
  local-only invariant. Reporter output is still local-only.
- [`0316-conductor-local-enforcement.md`](0316-conductor-local-enforcement.md)
  remains the hook and relay foundation. Unit B renames live surfaces while
  preserving fail-open hook safety.
- [`0390-process-watchdog.md`](0390-process-watchdog.md) defines the
  `LOOPCODER_RUN_ID` process marker used as the Worker work ID.
- [`0447-relay-enforcement-hardgate.md`](0447-relay-enforcement-hardgate.md)
  remains the relay hard-gate contract. Unit B's dual-token window exists
  because a blocking relay gate must not lock out valid runs during upgrade lag.
