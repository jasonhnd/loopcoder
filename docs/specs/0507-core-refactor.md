---
id: 507
title: Core refactor (behavior-preserving)
status: draft
date: 2026-07-05
issue: 507
pr: null
supersedes: []
superseded_by: []
---

# Core refactor (behavior-preserving)

This is a design-only spec for loopcoder **0.5.2**. This PR adds only this
document: no Go code, no `.delivery.yml` change, no command behavior change, and
no new dependency. Code slices are filed only AFTER this spec merges, per
[`docs/PROCESS.md`](../PROCESS.md).

0.5.2 is a core refactor release. It decomposes god-functions and centralizes
scattered defaults after the 0.5.1 hardening line has fixed the security and
robustness issues those functions currently contain. The release is explicitly
behavior-preserving: the point is readability, focused tests, and drift
reduction, not new capability.

## Non-negotiable invariant

The invariant for every 0.5.2 code slice is **ZERO behavior change**.

No slice may change any observable behavior, including:

- CLI command names, flags, defaults, stdout, stderr, JSON output, pretty text,
  exit codes, or failure categories;
- `.delivery.yml` schema, defaulted values, unknown-field tolerance, validation
  accept/reject set, or absent-config behavior;
- provider command argv, provider config files, prompt content, output schema
  content, environment wiring, MCP provider wiring, or `Invocation` semantics;
- attestation canonical JSON, stable header, pretty rendering, relay records, or
  local-only handling;
- branch names, PR titles, PR bodies, commit messages, recovery brief text,
  state events, attempt records, runstatus output, or promotion/tick reports;
- timing constants, retry counts, throttle limits, byte budgets, file modes,
  path confinement, hard caps, stall windows, or default branches.

The absent-config code profile must be byte-for-byte identical in behavior
before and after the refactor. A value may move to a new package, but the value
itself and every rendered effect of that value must remain identical.

If a code slice discovers that preserving behavior conflicts with a desired
cleanup, the cleanup is out of scope for 0.5.2. File a separate doc-first issue
for that behavior change.

## Refactor targets

### B1 - `worker.Dispatch` decomposition

Current anchor: `internal/worker/worker.go`.

`Dispatch` currently spans the full worker lifecycle in one function
(`internal/worker/worker.go:125-508`). It handles input validation, default
provider/base/branch/attempt selection, repo resolution, GitHub repo discovery,
scratch path allocation, attempt tracker setup, deferred cleanup and recovery,
`git fetch`, locked worktree creation, config loading, repo skill injection,
prompt writing, MCP selection, domain worker policy resolution, provider
invocation, hung-worker harvest/report-only handling, attestation validation,
dirty checking, `git add`, commit, push, PR creation, state transitions, and the
final `worker.Result`.

0.5.2 must split that lifecycle into focused helpers while preserving the public
`Dispatch(ctx, opts, deps)` entrypoint and `worker.Options`, `worker.Deps`, and
`worker.Result` contracts. The intended helper boundaries are:

- `prepareDispatch`: validate required inputs, apply default provider/base
  branch/branch/attempt/run ID, resolve the repo path, and resolve the agent
  runner and GitHub client.
- `prepareWorktree`: allocate scratch paths, create the attempt tracker, fetch
  the base branch, and add the worktree under the existing lock.
- `buildInvocation`: load config, collect repo skills, build and write the
  prompt, select MCP servers, resolve domain policy, and construct the
  provider-neutral `agent.Invocation`.
- `runAgent`: execute the selected `agent.Runner`, record phase transitions, and
  return the raw `agent.Result` plus the exact error/hung classification that
  today's `Dispatch` would have produced.
- `handleHungOrPartialWork`: preserve the 0.4.2/0.5.1 hung, report-only, and
  harvest paths, including retry branch naming, needs-human status, conductor
  harvest attestation, and recovery side effects.
- `commitAndOpenPR`: validate worker attestation, read summary, check dirty
  state, add, commit, push, open the PR, and construct the success result.
- `writeRecovery` and `cleanup`: keep the deferred failure/recovery behavior and
  successful cleanup behavior isolated but byte-identical.

The helper names are guidance, not API. They may remain unexported. The code
slice must not create a new worker engine, new state machine, or new durable
schema.

Behavior that must remain exactly unchanged includes:

- default provider `codex`, default base branch `main`, default branch
  `loop/issue-<n>`, default attempt `1`, and generated run ID behavior;
- lock timeout, scratch path shape, prompt/summary/log filenames, attempt path
  shape, job ID shape, phase names, statuses, and recovery brief path;
- prompt text from `BuildPrompt`, repo skill section ordering, recovery context
  insertion, and provider invocation fields;
- worker hard cap and stall timeout fallback values, including config override
  precedence;
- `domain.partial_work` behavior, hung harvest behavior, retry branch naming,
  report-only behavior, and needs-human harvest result shape;
- attestation validation, canonical JSON rendering check, usage recording, and
  pretty/local-only relay obligations;
- dirty-worktree error behavior, commit message, PR body, PR title, push target,
  and cleanup warnings.

### B2 - orchestration decomposition

Current anchors:

- `internal/orchestration/tick.go`
- `internal/orchestration/promote.go`
- `internal/orchestration/dispatch_wave.go`
- CLI render call sites in `internal/cli/cli.go`.

The orchestration package already returns structured reports, but state
progression and presentation still live together in large files. `Tick`
(`tick.go:268-524`) compiles work, computes ready sets, dispatches a wave,
recovers failed dispatches and reviews, reviews PRs, evaluates risk gates,
handles pre-prod merge/health/revert flows, pushes state, and finalizes the
report. The same file also contains default wiring (`withTickDefaults` at
`tick.go:525-607`), many recovery/pre-prod helpers, and `RenderTickText`
(`tick.go:1342-1623`).

`DispatchWave` (`dispatch_wave.go:86-275`) handles ready-set preflight,
guardrail budget/circuit decisions, dispatch concurrency, per-issue result
collection, and report construction. The same file also renders dispatch-wave
text (`dispatch_wave.go:434-482`) and worker completion text.

`Promote` (`promote.go:247-610`) validates promotion options, evaluates the
gate, gathers reconciliation/toggle evidence, kicks back items, promotes
pre-prod to main, performs health/rollback/sync work, records the ledger event,
and finalizes the report. The same file renders text output
(`promote.go:871-1044`) and normalizes report defaults.

0.5.2 must separate state progression from presentation without changing report
types or output bytes:

- State progression functions keep returning structured reports only. They must
  not concatenate user-facing text as part of control flow.
- Text renderers move to dedicated files such as `tick_render.go`,
  `dispatch_wave_render.go`, and `promote_render.go`, or equivalently named
  files under `internal/orchestration/`.
- JSON marshal helpers, text render helpers, normalization helpers, and exit-code
  helpers may be grouped by report type, but their exported names should remain
  stable unless the old names are left as byte-identical wrappers.
- CLI wrappers such as `runTick` and `runDispatchWave` may keep calling the same
  orchestration render functions; they must not gain new orchestration logic.

Behavior that must remain exactly unchanged includes:

- `TickReport`, `DispatchWaveReport`, `PromoteReport`, and all nested JSON field
  names, omitted fields, defaulted slices, ordering, timestamps, and normalization
  behavior;
- `RenderTickText`, `RenderDispatchWaveText`,
  `RenderDispatchWaveIssueCompletion`, and `RenderPromoteText` output bytes for
  every existing fixture and unit test;
- default base branch `main`, pre-prod branch `pre-prod`, dispatch-wave throttle
  default `4`, promotion gate defaults, and statebranch defaults;
- ready-set selection, guardrail budget/circuit evaluation ordering, concurrency
  semantics, `OnIssueComplete` behavior, recovery ordering, risk-gate ordering,
  pre-prod merge/health/revert ordering, state push behavior, and exit codes.

### B3 - MCP validation consolidation

Current anchors:

- `internal/config/config.go:213-233` defines `.delivery.yml` MCP structs.
- `internal/config/config.go:429-440` validates only `mcp.servers[].roles` at
  parse time.
- `internal/mcp/mcp.go` selects config MCP servers for worker/verifier
  invocations and copies them to `agent.MCPServer`.
- `internal/agent/agent.go:14-34` carries provider-neutral `Invocation`
  MCP data.
- `internal/agent/codex.go:205-410` performs role, name, transport, URL, and
  auth validation used by Codex, Claude, and Gemini helpers.
- `internal/agent/claude.go` and `internal/agent/gemini.go` render
  provider-specific MCP config after that validation path.

The current validation split makes `config.Parse` reject only unknown MCP roles,
while full server validation happens later in the agent layer. 0.5.2 must
consolidate that validation into a shared parse-time validator so MCP config
errors surface early and consistently.

The consolidation is still behavior-preserving. It must not change the set of
accepted and rejected configs. In particular:

- The validation rules for safe provider MCP server names, inferred or explicit
  `stdio`/`http` transports, required command or URL fields, stdio-vs-HTTP field
  exclusions, HTTP/HTTPS URL checks, HTTP auth header/env pairing, valid header
  names, valid environment names, role filtering, and read-only verifier
  classification must match today's agent-layer behavior.
- Error paths must keep pointing at `mcp.servers[<index>]` and must remain
  diagnosable. If exact message bytes are not preserved, tests must prove the
  command exit category, config path, and rejection reason remain equivalent.
- Provider argv and provider config rendering must remain byte-identical for
  Codex, Claude, and Gemini.
- Read-only verifier protection must remain fail-closed. A server not locally
  classified `read_only: true` must never reach a read-only invocation.
- The implementation must avoid import cycles. The shared validator can live in
  an existing MCP/config boundary or a small leaf package, but it must not become
  a new provider abstraction or change the `agent.Runner` door.

Because this is a refactor release, B3 must first capture current MCP validation
cases in tests, then move the logic. Any previously accepted malformed profile
that would become rejected is a behavior change and must be left accepted in
0.5.2 or moved to a later doc-first behavior-change issue.

### B4 - defaults and limits centralization

Current anchors include scattered defaults and limits such as:

- worker watchdog fallback constants in `internal/worker/worker.go:27-30`;
- default base branch `"main"` in worker, recovery, ready-set, loopreview,
  verify, doctor, guardrails, tick, dispatch-wave, promote report
  normalization, and CLI wrappers;
- default pre-prod branch `"pre-prod"` in config defaults, tick, promote paths,
  CLI wrappers, and tests;
- dispatch-wave throttle default `4` in `internal/orchestration/tick.go` and
  `internal/orchestration/dispatch_wave.go`;
- resilience defaults in `internal/config/config.go:295-323`;
- retry defaults in `internal/recovery/recovery.go:255-264`;
- command hard caps in git, doctor, scaffold, upgrade, verify, compile ordering,
  loopreview, supervisedexec, and GitHub adapters;
- loopreview packet limits and rendered-artifact limits in
  `internal/loopreview/loopreview.go`;
- worktree liveness file cap in `internal/supervisedexec/supervisedexec.go`;
- GitHub list limit in `internal/vcs/github/github.go`;
- hook, runstatus, relay, and statebranch bounds in their respective packages.

0.5.2 must centralize these values into a new leaf package, preferably
`internal/defaults` unless implementation discovers a clearer local name such as
`internal/limits`. The package should expose a single documented values surface
and small helpers for copy-sensitive data. For example:

```go
type Values struct {
    BaseBranch string
    PreProdBranch string
    DispatchWaveThrottleLimit int
    WorkerHardCap time.Duration
    WorkerStallTimeout time.Duration
    WorkerMaxAttempts int
    WorkerRetryBackoffSeconds []int
}

func Current() Values
```

The exact shape is a code-slice decision, but these rules are normative:

- `internal/defaults` must be a low-level package that avoids import cycles.
  It may import standard-library packages such as `time`, but it must not import
  high-level loopcoder packages like `config`, `worker`, `orchestration`, or
  `loopreview`.
- Mutable values such as slices must be returned as copies or exposed through
  helper functions that cannot be mutated globally by a caller.
- Existing package-level exported constants may remain as wrappers when they are
  part of current tests or package contracts, but their source of truth becomes
  the defaults package.
- Values are not tuned in 0.5.2. Moving `4`, `main`, `pre-prod`, `45m`,
  `5m`, `3`, `[10,30,120]`, byte budgets, list limits, file caps, and hard caps
  must preserve the exact value at every call site.

B4 runs after B1-B3 because it is cross-cutting. It must be the only active code
slice in its wave.

## Behavior-preservation proof

Every code slice must prove behavior preservation with tests and verifier
evidence. A style-only review is insufficient.

Minimum proof for every slice:

- Existing package tests continue to pass. Tests may move mechanically with the
  code, but assertions must not be weakened.
- New focused unit tests cover extracted helpers at their new boundaries.
- A contract test or golden test covers the public behavior that the slice might
  accidentally change.
- The independent verifier must trace at least one representative path through
  the old public entrypoint and the new helper chain, and state whether behavior
  is preserved.
- CI remains green: `go build`, `go vet`, `go test ./...`,
  `go test -race ./...`, `staticcheck ./...`, and `govulncheck ./...` after the
  0.5.1 gates are present.

Slice-specific proof:

- B1 must keep end-to-end `Dispatch` tests green and add focused tests for
  validation/defaulting, worktree preparation, invocation construction,
  hung/harvest/report-only handling, recovery writing, cleanup, and PR creation.
  Tests must assert phase/status names, generated branch names, PR body text,
  attestation preservation, and recovery brief content for the paths they touch.
- B2 must keep renderer golden output byte-identical. At minimum, existing
  `RenderTickText`, `RenderDispatchWaveText`, and `RenderPromoteText` assertions
  remain intact, and any moved renderer gets a fixture that fails on byte drift.
  State progression tests must assert that moved render code is not consulted for
  control-flow decisions.
- B3 must include a MCP validation matrix shared across config parsing and agent
  invocation: valid stdio, valid HTTP, inferred transport, invalid role,
  unsafe name, missing command, stdio URL/auth misuse, missing HTTP URL, HTTP
  command/args misuse, invalid URL, auth header/env pairing, invalid header,
  invalid env, role filtering, and read-only verifier rejection. Provider argv
  tests must prove no Codex/Claude/Gemini output changes.
- B4 must add a defaults inventory test. The test should assert that the new
  defaults package values equal the old documented values and that legacy wrapper
  constants, config defaults, CLI defaulting, orchestration defaulting, recovery
  defaults, and loopreview/supervisedexec limits read the same values.

Any `staticcheck` finding introduced by a refactor must be fixed in the same
slice. Suppression is not an acceptable proof of behavior preservation.

## Preserved invariants

The following invariants are explicitly preserved:

- [`0161-autonomous-delivery-loop.md`](0161-autonomous-delivery-loop.md) F1-F5:
  tick still does not merge production, verifier remains read-only, promotion is
  distinct, guardrails still gate dispatch waves, and all roles still attest.
- 0161 M1-M4: only existing consumed edges are refactored, the self-hosting guard
  remains first, machine-facing schema remains additive/compatible, and failsafe
  floors may only be added to, never degraded.
- 0161 E1: `agent.Runner` remains the provider door, `agent.Invocation` remains
  the provider-neutral request, and `Invocation.ReadOnly` remains the permission
  boundary.
- 0.4.2 H5: loopreview clean verdict exit codes remain `pass=0`, `fail=1`, and
  `needs-human=2`, distinct from command/runtime failures.
- The self-hosting guard: every B-slice changes loopcoder core, routes
  `needs-human` when loopcoder works on itself, and takes effect only after
  human merge, rebuild, and tick restart.
- 0.5.1 hardening must not regress. The refactor must preserve restrictive
  `0600` file modes where 0.5.1 introduced them, release signing and
  verification, statebranch path confinement, no-shell argv command forms,
  honest failure reporting for `runJSON`, issue mutation readbacks, Codex log
  parsing, hook/runstatus bounds, and bounded worktree liveness.
- Local-only attestation remains local-only. No refactor may copy worker,
  verifier, or conductor attestation data into repository-visible artifacts.

## Follow-up code slices

The wave plan is:

1. Doc first: merge this spec.
2. Wave 1 parallel: B1, B2, and B3 may run in parallel because their primary
   file ownership is disjoint.
3. Wave 2 solo: B4 runs alone after B1-B3 merge because defaults/limits
   centralization touches many call sites.
4. Checkpoint: B1-B4 merged, tag `v0.5.2`, verify the real artifact, and rebuild
   the dev binary.

### B1 - worker.Dispatch decomposition

Owned files: `internal/worker/*`.

Acceptance:

- `Dispatch` is decomposed into focused unexported helpers covering preparation,
  worktree setup, invocation construction, agent execution, hung/harvest
  handling, commit/PR creation, recovery writing, and cleanup.
- `Dispatch(ctx, opts, deps)` remains the public entrypoint and returns the same
  `worker.Result` values and errors for the same dependencies.
- Existing worker tests remain green; new tests cover extracted helper seams.
- End-to-end behavior for 0.5.1 partial-work, liveness, argv, harvest,
  attestation, recovery, and cleanup paths is unchanged.
- No `.delivery.yml` schema, provider argv, PR body, commit message, branch name,
  state record, recovery brief, file mode, timeout, or exit behavior changes.

No-behavior-change proof: compare existing worker test fixtures before and after
the split, add helper-level tests for each extracted stage, and have the verifier
trace a successful dispatch path plus a hung harvest path through the new helper
chain.

### B2 - orchestration decomposition

Owned files: `internal/orchestration/{tick,promote,dispatch_wave}.go` and any new
render/helper files under `internal/orchestration/`.

Acceptance:

- Tick, promote, and dispatch-wave state progression remains in structured
  report-returning functions.
- Text/JSON presentation logic is separated into dedicated render files or
  clearly isolated render helpers.
- Existing exported renderer names remain available, or old names remain as
  wrappers that call the new implementation.
- Rendered output bytes and JSON output are identical for tick, promote,
  dispatch-wave, and dispatch-wave per-worker completion.
- State transitions, guardrail ordering, dispatch concurrency, recovery
  ordering, promotion ordering, state push behavior, and exit codes are
  unchanged.

No-behavior-change proof: renderer golden tests compare exact strings, JSON
tests compare exact normalized reports, and orchestration tests prove no state
branch depends on presentation helpers.

### B3 - MCP validation consolidation

Owned files: `internal/config/config.go` and `internal/agent/*` MCP validation
sites. The existing `internal/mcp` bridge may be adjusted only if needed to keep
the validator shared without an import cycle.

Acceptance:

- MCP role, server-name, transport, command, args, URL, auth, role-filter, and
  read-only rules are validated by one shared validator.
- Config parse-time validation catches the same invalid MCP declarations that
  provider invocation would reject today.
- The accepted/rejected config set is unchanged. Any edge case that would become
  newly rejected is preserved or deferred to a separate behavior-changing spec.
- Provider-specific Codex, Claude, and Gemini argv/config rendering remains
  byte-identical for valid inputs.
- Verifier read-only MCP filtering remains fail-closed.

No-behavior-change proof: add a shared MCP validation matrix, keep provider argv
golden tests green, and have the verifier compare the current and refactored
accept/reject behavior for representative valid and invalid MCP configs.

### B4 - defaults/limits centralization

Owned files: new `internal/defaults` or `internal/limits`, plus call sites that
currently own duplicated defaults and magic values across `internal/`, `cmd/`,
scripts only if they already consume Go defaults, and tests.

Needs: B1, B2, B3.

Acceptance:

- A single low-level package owns documented defaults and limits for branch
  names, pre-prod branch, dispatch-wave throttle, worker/verifier hard caps and
  stall windows, retry counts/backoff, command hard caps, packet budgets, file
  caps, list limits, and similar repeated values.
- Existing package-level constants remain as compatibility wrappers when they
  are part of current package contracts.
- All values remain identical to today's values. No tuning happens in B4.
- Mutable default slices are copied before exposure.
- The package does not create import cycles and does not import high-level
  loopcoder packages.

No-behavior-change proof: add a defaults inventory test that pins current values,
keep existing tests green, and have the verifier inspect a representative sample
of call sites to confirm they now read from the centralized source without value
drift.

## Relationship to existing specs

- [`0161-autonomous-delivery-loop.md`](0161-autonomous-delivery-loop.md) remains
  the parent for F1-F5, M1-M4, and E1. 0.5.2 refactors core internals while
  preserving those constraints.
- [`0459-domain-profiles.md`](0459-domain-profiles.md) introduced domain
  profiles and MCP. 0.5.2 does not add domain capability; it consolidates MCP
  validation without changing domain or provider behavior.
- [`0484-security-robustness-hardening.md`](0484-security-robustness-hardening.md)
  is the 0.5.1 hardening foundation. 0.5.2 must preserve every hardening
  behavior after that line lands.
- [`0423-operational-reliability-hardening.md`](0423-operational-reliability-hardening.md)
  supplies the 0.4.2 H1-H5 reliability fixes. 0.5.2 preserves harvest,
  liveness, review-packet safety, loud config behavior, and H5 exit-code
  separation.
- [`0146-attestation.md`](0146-attestation.md),
  [`0306-local-only-attestation.md`](0306-local-only-attestation.md), and
  [`0447-relay-enforcement-hardgate.md`](0447-relay-enforcement-hardgate.md)
  remain unchanged.

## Non-goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml`, workflow, installer, script, README, CHANGELOG, or
  dependency change in this design-doc PR.
- No new feature, new command, new provider, new scheduler, new durable schema,
  or new public API.
- No `loopcoder audit`; that is the separate 0.5.3 roadmap unit.
- No tuning of centralized default values or limits.
- No weakening of the self-hosting guard, verifier read-only boundary,
  guardrails, relay obligations, local-only attestation handling, H5 exit-code
  contract, 0.5.1 hardening, or F1-F5.
