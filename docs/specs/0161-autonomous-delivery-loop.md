---
id: 161
title: Autonomous Delivery Loop (Automation + Perception)
status: draft
date: 2026-06-30
issue: 161
pr: null
supersedes: []
superseded_by: []
---

# Autonomous Delivery Loop (Automation + Perception)

This is a design-only spec for loopcoder 0.4.0. This PR must add only this
document: no Go code, no `.delivery.yml` change, no command behavior change, and
no new runtime dependency. Code slices are filed only AFTER this spec merges, per
[`docs/PROCESS.md`](../PROCESS.md).

This spec realizes issue #161 and updates its scope after v0.3.3. The original
#161 hard prerequisites are now satisfied: a reliable `loopreview` verifier
(spec [`0194`](0194-reliable-loopreview-verifier.md), proven for `codex`/`claude`)
and the 0.3.2 delivery guardrails (spec [`0192`](0192-delivery-guardrails.md))
have shipped. The staged HOLD gate (#160) is closed. 0.4.0 is therefore unblocked.

## Goal

Make the delivery loop run unattended. The human touches exactly two ends:

1. **Writes `ROADMAP.md`** (intent), and
2. **Reviews the pre-production environment and promotes it to production**
   (the single irreversible gate).

Everything in between — turning the roadmap into issues, writing the docs and
code, opening PRs, running CI and the verifier, and landing reviewed changes into
a pre-production environment — runs automatically on a trigger, with no human
prompting each step.

## Paradigm

This spec adopts Loop Engineering: the discipline of structuring an agent's work
into iterative cycles with feedback, termination conditions, and human gates. Its
central shift is "you do not prompt the agent; you design the system that prompts
the agent." The human is removed from doing the work and kept where judgment is
scarce: setting intent up front, and guarding the irreversible gate.

loopcoder already implements four of the five Loop Engineering moves — Discovery
(`ready-set`), Handoff (`dispatch` into isolated worktrees), Verification
(`loopreview`), and Persistence (`state`). The missing move is **Scheduling**
(automatic triggering). 0.4.0 adds Scheduling and the orchestration that strings
the moves into one unattended pass.

## Architecture: deterministic orchestration + three LLM nodes

0.4.0 does NOT introduce an unattended "conductor LLM session" that sits in the
loop making decisions. The orchestration is deterministic; LLMs appear only at
three well-defined nodes:

| Node | Role | Status |
|---|---|---|
| **Planner** (`compile`) | Turn high-level ROADMAP intent + context into detailed doc/code issues | new in 0.4.0 |
| **Generator** (`worker`) | Write docs/code, open a PR | existing (`dispatch`) |
| **Evaluator** (`loopreview`) | Independently verify each PR | existing |

A deterministic `tick` strings these nodes together; an automation triggers the
tick. The Generator and Evaluator already run headless and unattended today; this
spec adds the Planner node, the deterministic `tick`, and the trigger.

## The `tick` command

`loopcoder tick --repo <path>` runs one unattended pass:

1. **compile** — `ROADMAP.md` → GitHub issues with dependencies (Planner node;
   doc-first ordering; idempotent).
2. **ready-set** — classify ready vs blocked/held work (existing).
3. **guard** — enforce 0.3.2 budget/circuit-breaker guardrails; stop and report
   if a cap is hit (existing).
4. **dispatch-wave** — dispatch ready issues to workers in isolated worktrees;
   each opens a PR (existing).
5. **loopreview** — run the independent verifier on each new PR
   (`pass`/`fail`/`needs-human`) (existing).
6. **risk gate + auto-merge** — for PRs that clear the quality gate and are not
   red-line, auto-merge into the pre-production environment (see below).
7. **state push + report** — persist run state and render the report (existing +
   extended).

`tick` MUST NOT have the capability to merge into the production branch
(`main`). That capability does not exist in the command at all. Promotion to
production is exclusively a human action (failsafe against accidental
auto-promotion).

## ROADMAP.md and the Planner (`compile`)

- Each repository has one `ROADMAP.md` at its root (scaffolded by `init`). The
  human's brainstorm output lands here.
- A roadmap entry MAY be coarse (high-level intent) or fine (a near-complete
  issue description). When coarse, the Planner (an LLM) refines intent plus
  repository context into detailed **documentation** issues first, then
  **code** issues, mirroring loopcoder's doc-first rule (a merged design precedes
  its implementation). When entries are already fine-grained, `compile` MAY be
  purely deterministic.
- `compile` is **idempotent**: re-running it on an unchanged roadmap creates no
  duplicate issues. It correlates roadmap entries to issues via a hidden, stable
  unit-id marker. Editing an entry updates (or supersedes) its issue rather than
  forking a second one.
- Dependencies are emitted as `blocked-by` labels: code issues are blocked-by
  their documentation issue; cross-entry dependencies declared in the roadmap
  become `blocked-by` edges in the issue DAG.

## Environment model: pre-production gate, human promotion to production

There are two environments. "preview" and "staging" are the same thing here: a
single pre-production environment.

```
worker PR ──auto-merge (aggressive)──▶ pre-production (= preview = staging; human reviews the combined effect)
                                            │  auto-revert keeps it green/deployable
                       human promote (whole batch + can kick individual items back) ──▶ main / production
```

- PR → pre-production is **reversible**, so it is automated.
- pre-production → production is **irreversible**, so it is the human gate.
- The human reviews the **combined effect** of an accumulated batch in the
  pre-production environment, not individual PR diffs, then promotes.

### Promotion

Promotion is **whole-batch by default, with per-item kick-back**: the human
reviews the pre-production environment as a whole and promotes the batch to
production; if a specific change is not acceptable, the human can kick that single
item back (it returns to the loop to be redone) while the rest is promoted.

## Risk gate and auto-merge (E2, in 0.4.0)

Auto-merge is **aggressive but only into pre-production**. Because pre-production
is reversible, the risk gate is small:

- A PR auto-merges into pre-production when it clears the quality gate
  (required CI checks green AND `loopreview = pass`) AND touches no red line.
- **Red lines** (never auto-merge; route to `needs-human`): deleting data /
  destructive operations, breaking the build, and **changing loopcoder's own
  core** (see Self-hosting guard). This list is intentionally minimal — only what
  could damage pre-production itself.
- An LLM risk assessment MAY raise a PR's risk (route to `needs-human`) but MUST
  NOT lower it below the deterministic red-line rules.
- **Pre-production stays green:** if an auto-merge turns pre-production red
  (build or deploy breaks), the loop automatically reverts the offending merge so
  the environment remains deployable and reviewable.

The "auto-promote to production" half of E2 is NOT in 0.4.0: production promotion
stays a human gate.

## Evidence / preview per PR

The human judges on running effect, not code. Each PR produces a per-project
**evidence artifact** so the human (and, later, the machine evaluator) can judge
behavior rather than diffs. The form is configurable per project in
`.delivery.yml`, because "effect" differs by software type:

| Project type | Evidence artifact |
|---|---|
| Website / frontend | a preview/pre-production deployment URL |
| Backend API / service | an ephemeral environment plus recorded API behavior |
| CLI tool / software | recorded command output, or a downloadable preview build |
| Library / SDK | test results plus example-run output, API/changelog diff |
| Desktop / mobile | a downloadable preview build; screenshots |
| Data / ML | an evaluation report; before/after sample output |

The `tick` report surfaces each PR's evidence artifact. A website preview URL is
simply one configured form. Where effect cannot be produced automatically, the
loop provides a one-command "checkout + build preview" so the human can run it
locally.

## Failure loop (recover)

A failed PR (worker crash, red CI, `loopreview = fail`) never enters
pre-production, so it does not pollute what the human reviews. The loop therefore
retries automatically, bounded:

- Attempt 1: retry with the same configuration (absorbs transient/flaky failures).
- Attempt 2: retry **upgraded** (stronger model / higher effort) (absorbs
  capability-limited failures).
- After 2 attempts: stop and route to `needs-human` in the report.

All retries are bounded by the 0.3.2 guardrails (budget/circuit-breaker); the loop
never retries indefinitely.

## Self-hosting guard

Changes to loopcoder's own core (`tick`, `worker`, `loopreview`, `compile`, and
the safety machinery such as auto-revert and the risk gate) are a **red line**:
they never auto-merge; they route to `needs-human`. A core change takes effect
only after a rebuild and a tick restart — until then the running loop keeps using
the prior, known-good core.

Consequence (an accepted asymmetry): loopcoder is **more autonomous for consumer
projects than for itself**. Consumer projects (e.g. a website) do not touch
loopcoder core and flow fully automatically; loopcoder developing itself touches
core constantly and therefore keeps the human in the loop for those changes. This
is correct — changing the loop itself is the most dangerous operation, and a bad
core change could damage the very safety mechanisms the loop relies on.

loopcoder's own "production" is its published release: non-core PRs auto-merge to
a pre-production branch, core PRs go to `needs-human`, the human promotes, and a
release is cut.

## Multi-project

0.4.0 implements a solid **single-project** `tick` (one repo, one trigger). `tick`
is designed to be **stateless and batch-callable** so a future multi-project
poller is a thin outer loop, not a rewrite. A multi-project scheduler (polling
many repos, cross-project budget/priority, aggregated report) is deferred to
0.4.x — it is purely a batch over the single-project tick and has no core
difficulty of its own. The human's own loopcoder and a consumer project are
enough to prove the single-project loop first.

## Automation / triggers (Scheduling)

`tick` is triggered, not a resident session. Supported triggers:

- **cron / OS scheduler** — run `tick` on a schedule.
- **goal loop** — iterate until `ROADMAP.md` is exhausted, with a `max_iterations`
  ceiling (cost-runaway guard) on top of the guardrails.
- **hook** — an event such as a CI failure triggers a tick (feeds Perception D1).

The trigger drives the deterministic tick; it does not host an LLM.

## Perception

- **D1 discover** — automatically file issues from CI failures / labels, feeding
  the next tick's Discovery (closes the self-repair feedback loop). Held/parked
  work (e.g. a known-unfixable issue) MUST be excluded from auto-dispatch.
- **D2 skills** — inject project skill/convention files into the worker prompt so
  workers follow the project's standards.

## Reporting / the human interface

Each tick produces one durable report — the human's morning surface:

- the pre-production batch currently awaiting promotion, with each PR's evidence
  artifact (e.g. preview URL);
- the `needs-human` list (verifier needs-human, red-line PRs, guardrail-frozen);
- the failure list (items that exhausted recover).

The human reads one report plus the pre-production environment, then promotes.

## Epic support: large tasks and migrations

An *epic* is one ROADMAP entry too big for a single PR — a large migration,
rewrite, or refactor (e.g. rewrite the repo Go→Rust), or a large multi-slice
feature. Epic support is a generalization: any epic decomposes into a dependency
tree of slices, each a normal slice PR through the loop above; migration/rewrite
epics additionally require behavioral equivalence. This section builds on
`compile`, the risk gate, evidence, and promotion; it adds decomposition, an
equivalence gate, and an incremental-migration discipline. Design reference:
`0161-epic-research-reference.md` (industry practice, verified sources).

### Decomposition (compile as Planner over an epic)

- An epic compiles into a **slice dependency DAG** (nodes = slices, edges =
  resource/temporal deps), not a flat list, emitted as an in-repo mutable JSON
  artifact (JSON so the model does not overwrite it).
- **Hybrid upfront + rolling**: `compile` emits the DAG once; each tick
  re-derives/patches it (add/remove/reorder) as slice PRs land and reveal reality.
  The tick is the natural replan boundary.
- **Plan-approval gate, once**: the first DAG is a plan-review `needs-human` — the
  human approves the top-level decomposition one time. Thereafter the tick patches
  the DAG freely and only re-escalates when a patch would churn an already-merged
  slice.
- Acceptance invariant: **every slice must be implementable AND testable in
  isolation** — the single property that makes one-PR-per-slice, independent
  `loopreview`, and per-slice equivalence checks tractable. Slice along
  ownership/module boundaries; over-decompose then prune.

### Dependency graph and ordering

- The dependency graph is **tool-extracted as the authoritative backbone**
  (`go list`/goda for the source-side Go graph, acyclic by construction); the
  Planner supplies only what tools cannot (cross-language mapping, logical deps);
  the human may declare or override special seams in the epic spec.
- The ready-set is Kahn's in-degree-zero frontier over the slice DAG, recomputed on
  each merge. **Migrate leaf-first**; dispatch the ready layer to parallel workers;
  tie-break by unblock-count + on-critical-path + explicit `dependsOn`; surface the
  **critical path as the honest ETA**.
- **Cycles are SCC-condensed** at seed time (tool-computed on the real graph) into
  single atomic slices that must ship in one PR; the "these N modules are a tangled
  cluster" verdict is surfaced to the human at the plan-approval gate.

### Equivalence gate (migration-class epics)

- Beyond `loopreview` (which *reads* the diff), migration epics get an
  **equivalence gate: a distinct verifier stage that executes old vs new**.
- **Layered**: per-slice PRs gate on golden-master/characterization + differential
  replay (with a noise baseline: run the original twice); the whole pre-production
  environment gates on parallel-run reconciliation whose matched/unmatched report is
  the human's promotion evidence.
- The migration epic spec **must declare an equivalence contract** — tolerance rules
  (null-mapping, float precision, ordering-insensitivity) plus a read vs side-effect
  partition (only read/pure slices get live dual-execution). Hard schema requirement;
  the gate never assumes byte-identity.
- A golden mismatch **within contract tolerance passes automatically**; **outside
  tolerance it routes to `needs-human`** (a mismatch is a change-detector — never an
  auto-pass, never an auto-fail). Workers **may never silently re-baseline** a
  golden/snapshot; re-baselining is a promotion-class human decision backed by an
  approved intentional-divergence allowlist.

### Incremental execution and promotion

- **Strangler Fig + Branch by Abstraction**: the conductor is the facade;
  pre-production is the living host tree that stays green after every merged slice,
  never gated on full parity. Migration/refactor epics decompose as seam slice →
  implementation slices → flip+delete slices, and the tree **must include cleanup
  slices** (toggle/abstraction removal). Feature-addition epics need no seam, only
  toggles.
- Each implementation slice carries a **build-tag toggle** as the revert net — a bad
  slice in pre-production is toggled off, not manually rolled back (the Turborepo
  Go→Rust pattern). Unfinished-epic slices merge into pre-production but stay
  **toggled-off (dark)**, so unrelated ROADMAP work proceeds and promotes normally;
  batched promotion flips only completed work and leaves unfinished epic slices dark
  — **an in-flight epic never blocks unrelated ROADMAP**.
- Batched human promotion consumes a **combined go/no-go panel**: the parallel-run
  reconciliation report + the toggle inventory (which dark slices remain) + the
  `needs-human`/failed list.
- The tick resume contract records **failed approaches and why** (in the
  progress/CHANGELOG) so re-dispatched workers do not retry dead ends. Never emit a
  megadiff. Because loopcoder is a single Go binary, cross-compiling across the six
  platforms is a first-class toolchain slice, not an afterthought.

## Safety

- Guardrails (budget / circuit-breaker) bound every tick (0.3.2).
- Termination: `max_iterations` and "ROADMAP exhausted" stop the goal loop.
- Attestation: worker, verifier, and the tick's own conductor invocation are
  attested (spec [`0146`](0146-attestation.md)).
- Rollback: pre-production auto-reverts on red; production promotion is a careful
  human action.
- Failsafe: `tick` has no capability to merge into `main`; production promotion is
  human-only.
- The human promotion gate is never bypassed in 0.4.0.

## Out of 0.4.0 scope (0.5.0+)

- **E1 MCP connectors** — 0.5.0.
- **Auto-promote to production** (the production half of E2) — production
  promotion stays a human gate in 0.4.0.
- **E3 conductor running entirely as a cloud GitHub Action** — dropped /
  indefinitely deferred.
- Multi-project scheduler — deferred to 0.4.x.

## Seams reserved for 0.5.0

This chapter is normative for 0.4.0. It defines the **hard invariants** — edge and
contract level only — that the twelve 0.4.0 code slices MUST obey so that the three
deferred 0.5.0 features (E1 MCP connectors, E2 auto-promote to production,
multi-project scheduler) land as **pure additions**, never as a redesign and never
as a weakening of a failsafe.

Form is A1: each seam is an invariant of the 0.4.0 spec, not a 0.5.0 spec. This
chapter locks **boundaries and contracts only**. It MUST NOT predefine any 0.5.0
struct, field, command, or interface method, and MUST NOT introduce an abstraction
whose only consumer is a deferred feature (YAGNI). Where a seam names an anchor,
that anchor MUST already exist and already be consumed in 0.4.0 (for example
`agent.Runner`, `agent.Invocation`, `adapters.gate`, the two-method `github.Writer`).
Referencing an existing anchor as the seam is permitted; inventing scaffolding for
the future is not.

### Cross-cutting principles

These four meta-invariants bind every slice.

- **M1 — Lock edges, do not pre-build abstractions.** 0.4.0 MUST NOT add any config
  field, `Invocation` field, CLI command, interface method, or abstraction whose
  only consumer is a deferred 0.5.0 feature. Every reserved extension point MUST be
  an existing seam already consumed in 0.4.0 — the `github.Writer` interface
  (`internal/vcs/github/github.go:23`), `adapters.gate` (`config.go:30`), the
  `agent.Runner` registry (`agent.go:37`), the parsed `.delivery.yml` `Config`
  (`config.go:11`). A 0.5.0 addition MUST therefore be a pure add — a new `Writer`
  method, a new `Gate` value, a new `Runner` self-registered via `init()`, a new
  optional `Config` section — not the activation of dormant present-day scaffolding.

- **M2 — Self-hosting guard first; no seam is a backdoor.** Any 0.4.0 path that
  reaches loopcoder core (tick, worker, loopreview, compile, auto-revert, risk gate)
  MUST exit via the `"needs-human"` string outcome — declared independently in
  `guardrails/budget.go:21`, `orchestration/dispatch_wave.go:25`, `loopreview.go:27`,
  and `verify/verify.go:26` — and take effect only after a human rebuild and tick
  restart. No reserved extension point — `adapters.gate` or any 0.4.0 config field —
  may carry a value that routes a core-change path to any status other than
  `"needs-human"`. The gate slot is reserved for merge-policy extension (E2), never
  for bypassing the core-change red line.

- **M3 — Extension is additive, versioned, byte-stable.** Every durable
  machine-facing schema 0.4.0 emits MUST be extensible add-only: new fields
  snake_case, optional, `omitempty`-safe, and unknown-field-tolerant on read
  (`Parse`/`Unmarshal` MUST NOT error on unrecognised keys, as `config.Parse`
  already does not). `Config` (`config.go:12`), `state.AttemptRecord`, the
  statebranch `snapshot` (`statebranch.go:119`) and `Lease` (`statebranch.go:94`)
  already carry an integer `version`. `AttestationRecord` (`attestation.go:36`)
  deliberately does NOT gain a `version` field in 0.4.0: adding one now would be a
  field whose only consumer is a deferred feature (M1), and would re-touch the
  0.3.6-frozen attestation contract. Instead, `AttestationRecord.CanonicalJSON`
  output MUST stay byte-stable and its reader MUST tolerate unknown fields, and the
  FIRST additive change to `AttestationRecord` (whenever 0.5.0 or later needs one)
  MUST introduce a `version` field in the same change. A 0.5.0 reader of a 0.4.0
  artifact and a 0.4.0 reader of a 0.5.0 artifact MUST both parse without error.

- **M4 — Failsafes may only be added to, never degraded.** 0.4.0 MUST establish
  these five floors as non-degradable; 0.5.0 may only add automation atop them:
  - **F1** — tick has NO capability to merge into main/production. `github.Writer`'s
    only mutating method is `CreatePR`; no merge-to-main method may be added to the
    `Writer` interface or its concrete implementation in 0.4.0 (`github.go:23`).
  - **F2** — the verifier `Invocation` is hardcoded `ReadOnly: true`
    (`loopreview.go:214`) and its attestation `Permission` is hardcoded
    `PermissionReadOnly` (`loopreview.go:261`); no 0.4.0 path may pass
    `ReadOnly: false` to a verifier runner or set its attestation permission to
    `write` or `orchestrate`.
  - **F3** — promotion to production is a distinct step callable only by a human;
    tick MUST NOT invoke it.
  - **F4** — the guardrail budget/circuit-breaker ledger gates every dispatch wave
    and every recover attempt; no path may bypass it.
  - **F5** — every worker, verifier, and conductor invocation emits an
    `AttestationRecord` that passes `Validate`, whose permission enum is the closed
    three-value set `{read-only, write, orchestrate}` (`validPermission`,
    `attestation.go:273`). The enum MUST NOT be extended for an existing role, and no
    existing role's hardcoded permission may be widened (`worker.go:391` = `write`,
    `loopreview.go:261` = `read-only`).

  The code enforcing these floors — the attestation validator, the verifier
  `ReadOnly` assignment, and the risk-gate logic — is itself loopcoder core; any
  change to it is a red-line item requiring rebuild and tick restart under the
  self-hosting guard (slice 8).

### E1 — MCP connectors

Worker and verifier reach external tools and data via MCP in 0.5.0. The seam is that
all agent invocation already flows through one provider-neutral contract.

- **Runner is the only door.** Worker and verifier MUST invoke every LLM provider
  ONLY through the `agent.Runner` interface obtained from the registry
  (`agent.go:33`, `agent.go:37`). They MUST NOT exec a provider binary directly,
  branch on a hardcoded provider name, or reach outside the
  `Run(ctx, Invocation) (Result, error)` contract. An MCP-capable provider in 0.5.0
  registers behind the same `Runner` via `init()` and requires zero change to worker
  (`worker.go:261`) or verifier (`loopreview.go:209`) call sites.

- **Invocation is the only request contract, and it is additive-only.**
  `agent.Invocation` (`agent.go:12`) is the single provider-neutral request shape.
  Every caller added by the slices — worker, loopreview, recover, D1-discover,
  D2-skills-injection — MUST populate only the existing fields (`WorktreePath`,
  `Prompt`, `Model`, `Effort`, `ReadOnly`, `OutputSchema`, `LogPath`) with their
  0.4.0-release semantics; no existing field's meaning may be overloaded to carry
  provider- or tool-specific data. 0.4.0 MUST NOT alter the `Run` signature. A future
  MCP field (for example an endpoint list) is permitted only as a pure append that is
  optional, `Default()`-safe, and byte-compatible with every 0.4.0 caller's
  `Invocation` literal.

- **ReadOnly is the one permission boundary, and MCP tools inherit it.**
  `Invocation.ReadOnly` (`agent.go:17`) is the single authoritative permission
  boundary across all providers. When true, each provider's `Run` MUST engage its
  most-restricted, side-effect-free mode — codex `-s read-only` (`codex.go:23`),
  claude `--safe-mode --allowedTools Read Grep Glob` (`claude.go:30`), gemini
  `--skip-trust --extensions none` (`gemini.go:31`). No provider may treat `ReadOnly`
  as advisory, ignore it, or expose a write-capable path reachable when it is true.
  Any tool reachable when `ReadOnly` is true — including any 0.5.0 MCP tool —
  inherits this guarantee. 0.5.0 may add sub-modes strictly more restrictive than the
  read-only sandbox; it may NOT weaken or bypass the boundary.

- **The read-only tool allow-list stays a private provider detail.** Each provider's
  allow-list MUST be encapsulated inside its own argument builder (`BuildClaudeArgs`,
  `BuildGeminiArgs`, `BuildCodexArgs`). It MUST NOT be lifted into `agent.Runner`,
  `agent.Invocation`, or any config field, and MUST NOT be referenced as a fixed
  constant by worker, verifier, tick, compile, or recover. This keeps the hardcoded
  tool set from calcifying into a cross-cutting dependency that would block 0.5.0 MCP
  tool-set injection.

- **The provider-invocation boundary is core.** The `agent.Runner` boundary —
  through which worker (`worker.go:261`) and verifier (`loopreview.go:209`) invoke
  providers — is declared core for the self-hosting guard. Any change that widens
  provider tool reach through that boundary (including MCP connectivity) MUST be
  classified as a core change: it MUST route `needs-human`, MUST take effect only
  after rebuild + tick restart, and the guard MUST enforce this at slice-8 time. Tick
  MUST NOT expose a runtime pathway — via `Invocation` fields, config values, or
  dynamic dispatch — that widens provider tool reach without triggering the core red
  line.

### E2 — Auto-promote to production

Promotion becomes automated (still gated) in 0.5.0. The seam is that promotion is
already policy-driven, red-line-floored, single-path, and idempotent.

- **The gate is replaceable policy, not wired-in control flow.** The promotion
  allow-decision — whether a verified pre-production batch or item may enter
  production — MUST be evaluated by consulting `adapters.gate` (`config.go:30`) and
  MUST NOT be hardcoded into the promote command's merge, ledger, or rollback logic.
  In 0.4.0 the only valid `Gate` value is `human-merge`; the promote command MUST
  enforce it and MUST NOT proceed without explicit human confirmation. Introducing
  any non-`human-merge` policy is a 0.5.0 concern and MUST NOT appear in any 0.4.0
  slice. Slices 2, 3, and 5 MUST be implemented so a future gate policy changes only
  the `Gate` string value and its evaluation function — never the ledger, rollback,
  or merge paths.

- **Red lines are a floor beneath any gate.** The deterministic risk-gate red lines
  (destructive/data-deleting ops, build-breaking, changing loopcoder core) MUST be
  evaluated as a hard floor BEFORE and INDEPENDENT OF any gate policy. A gate policy
  expressed via `adapters.gate` may only ADD a veto on items the red lines permit; it
  MUST NOT authorize a promotion the red lines have blocked. An LLM or policy may
  RAISE risk, never lower the deterministic floor. This ordering binds both the 0.4.0
  `human-merge` gate and any 0.5.0 automated gate, and MUST route red-line-blocked
  items to the existing `needs-human` sink (`scaffold.go:90`) before the gate is
  consulted.

- **Promote is the only path into production.** The promote command (slice 5) MUST be
  the ONLY code path that advances changes into production (main). Tick and worker
  MUST NOT gain any capability to merge or push into main during 0.4.0; the GitHub
  write surface reachable from tick and worker MUST stay limited to the two
  `github.Writer` methods `CreatePR` and `ListHeadPRs` (`github.go:23`), with no
  merge-to-main method ever added or called. Worker opens PRs only against
  `opts.BaseBranch` (`worker.go:326`), and the statebranch layer already refuses to
  name main as a state branch (`statebranch.go:509`). E2 MUST upgrade the promote
  gate, never teach tick to merge.

- **Promote is idempotent and ledgered.** The promote operation MUST be idempotent: a
  repeat call for the same item MUST detect the already-merged state via the PR
  `Merged`/`MergedAt` fields (`PullRequestReference`, `github.go:41`) and return
  success without re-merging. Every attempt — proceed, skip-as-done, or fail — MUST
  be recorded as an append-only event to the statebranch ledger before the call
  returns; no promotion outcome may live only in memory or in GitHub state alone.
  This makes slice 5 safe for future automated re-entry (E2) with no change to the
  promote interface.

- **Any pre-production auto-merge targets base by parameter only.** Any merge method
  added to `github.Writer` (or any new write interface) in slices 3 and 4 MUST take
  its target branch exclusively as a parameter derived from `Worker.BaseBranch`
  (`config.go:43`, snake_case, optional, safe default); no automated write path may
  embed a literal branch name or resolve a production branch independently.
  `github.Writer` MUST NOT gain a merge-to-production overload. 0.5.0 auto-promote
  MUST therefore be a NEW, separately-gated method or command — not a relaxation of
  the target parameter of any existing automated merge path. This keeps aggressive
  auto-merge confined to reversible pre-production and production reachable only via
  the human promote step.

### Multi-project scheduler

A poller over many repos with cross-project budget and priority arrives in 0.5.0. The
seam is that a single `tick` is already stateless, repo-parameterized,
aggregation-friendly, and merge-authority-bounded.

- **All durable state is repo-rooted.** Tick MUST derive every durable fact it reads
  or writes (runs, attempts, events, guardrail ledgers, conductor lease, config)
  solely from paths rooted at the caller-supplied repo path or that repo's own state
  branch. It MUST NOT read or write any cross-tick, cross-repo, or package-global
  mutable store. With no in-process cross-tick memory, a 0.5.0 poller can invoke tick
  N times in one process with zero shared-state coupling.

- **The repo is an explicit parameter to every step.** Every pipeline step — compile,
  ready-set, guard, dispatch-wave, loopreview, risk-gate, state, report — MUST receive
  its target repository as an explicit input (a `RepoPath` field on its Options struct
  or equivalent signature). No step may resolve the repo from process cwd, an
  environment default, or a hardcoded path. Existing anchors: `worker.Options.RepoPath`
  (`worker.go:24`), `DispatchWaveOptions.RepoPath` (`dispatch_wave.go:34`),
  `statebranch.PushOptions.RepoPath`, `guardrails.BudgetOptions.RepoPath`. Steps added
  by the slices (compile, risk-gate, promote, recover, triggers) MUST follow the same
  contract, requiring no new command or abstraction beyond the per-step Options struct.
  Multi-project dispatch in 0.5.x is then a plain loop over the same parameter.

- **Budget is aggregation, not cross-repo recompute.** Guardrail budget accounting for
  one tick MUST load ledgers and compute totals exclusively from filesystem state
  rooted at that invocation's `RepoPath`, and the resulting `Observed` value MUST be
  scoped to that single repo. No 0.4.0 path may merge, share, or cross-subsidise
  ledger data across distinct `RepoPath`s. A future scheduler can then aggregate by
  summing per-repo `Observed` totals (`runs`, `total_attempts`, `total_tokens`,
  `total_cost_usd`) with no change to accounting logic.

- **The report struct stays mergeable and render-pure.** `DispatchWaveReport` MUST
  remain a JSON-serialisable struct carrying `Repo` and `RunID` as top-level fields
  (`dispatch_wave.go:55`) populated from durable inputs (`opts.RepoPath`/`opts.RunID`),
  never from ephemeral in-process variables absent from the struct.
  `RenderDispatchWaveText` MUST remain a pure function of a `DispatchWaveReport` value,
  emitting no data not derivable from the struct alone. Slices 6 and 10 MUST NOT drop
  `Repo` or `RunID` and MUST NOT move data into a render-only side-channel. 0.5.0
  digest-merging is then a fold over existing per-tick outputs, requiring no new digest
  type, poller field, or JSON command.

- **No scheduling layer gains cross-repo merge authority.** A trigger, batch
  invocation, or poller MUST NOT gain any merge, promote, or write authority beyond
  what a single tick already possesses; tick's only VCS-boundary authority is opening
  a PR against `opts.BaseBranch` (`worker.go:326`), gated by the per-repo
  `adapters.gate` (`config.go:30`). Any 0.4.0 scheduling layer (slice 9) MUST invoke
  tick as an independent unit and MUST NOT add a merge, cross-PR, or promote code
  path. Doing so is a CRITICAL violation of tick-never-merges-main and the human-only
  promote gate.

### Invariant-to-slice binding

| Invariant | Binds slices |
|---|---|
| M1 — edges only, no dormant scaffolding | 1–12 (all) |
| M2 — core guard, no seam backdoor | 2, 3, 5, 8, 9, 12 |
| M3 — additive, versioned, byte-stable schemas | 1, 2, 4, 6, 7, 10, 11, 12 |
| M4 — failsafes only added to (F1–F5) | 2, 3, 4, 5, 6, 7, 8, 10 |
| E1 — Runner is the only door | 2, 4, 5, 7, 11, 12 |
| E1 — Invocation additive-only contract | 2, 5, 7, 11, 12 |
| E1 — ReadOnly is the one boundary | 2, 4, 5, 7, 12 |
| E1 — tool allow-list stays private | 2, 5, 7, 12 |
| E1 — provider-invocation boundary is core | 2, 3, 8 |
| E2 — gate is replaceable policy | 2, 3, 5 |
| E2 — red lines are a floor beneath the gate | 3, 5, 8 |
| E2 — promote is the only path (tick never merges main) | 1, 2, 3, 5 |
| E2 — promote is idempotent and ledgered | 5, 6, 10 |
| E2 — pre-production auto-merge targets base by parameter | 3, 4, 5 |
| Scheduler — all durable state is repo-rooted | 2, 3, 4, 6, 7, 10 |
| Scheduler — repo is an explicit per-step parameter | 1, 2, 3, 4, 9 |
| Scheduler — budget is aggregation, not recompute | 3, 7, 10 |
| Scheduler — report struct is mergeable and render-pure | 6, 10 |
| Scheduler — no cross-repo merge authority | 2, 3, 5, 8, 9 |

## Follow-up code issues (filed after this spec merges, in dependency order)

1. `ROADMAP.md` format + `compile` (Planner: idempotent unit-id marker, doc-first
   ordering, blocked-by DAG; LLM refinement of coarse entries).
2. `tick` orchestration (string moves 2–7; no merge-to-main capability).
3. Risk gate + auto-merge into the pre-production environment (red lines,
   quality-gate precondition, LLM-raise-only).
4. Pre-production environment model + auto-revert-keeps-green.
5. `promote` (whole-batch + per-item kick-back; human-only; production gate).
6. Evidence/preview configuration in `.delivery.yml` + report surfacing.
7. Failure-loop recover (2 attempts: same-config then upgraded).
8. Self-hosting guard (core red line; rebuild/restart to take effect).
9. Automation triggers (cron / goal loop with max_iterations / hook).
10. Report (pre-production pending-promotion + needs-human + failures + evidence).
11. D1 discover (CI failure → issue; exclude held work).
12. D2 skills injection into worker prompt.

Epic support adds (blocked-by the core slices above):

13. Epic decomposition — `compile` emits a slice DAG (hybrid upfront + rolling),
    plan-approval gate once, patch-on-tick, "implementable AND testable in
    isolation" acceptance invariant.
14. Dependency graph + ordering — tool-extracted backbone (`go list`/goda), Kahn's
    leaf-first layered dispatch, critical-path ETA, SCC condensation into atomic
    single-PR slices.
15. Equivalence gate (migration epics) — equivalence-contract schema, per-slice
    golden-master + differential replay, pre-production parallel-run reconciliation,
    tolerance-in auto / tolerance-out `needs-human`, re-baseline as promotion-class.
16. Incremental migration discipline — Branch-by-Abstraction slices (seam → impl →
    flip+delete + cleanup), per-slice build-tag toggle revert, dark-slice isolation.
17. Batched promotion panel for epics — reconciliation report + toggle inventory +
    `needs-human`/failed.

Dependency rule: every slice is blocked-by this spec; `tick` (2) is blocked-by
`compile` (1), the 0.3.2 guardrails, and the reliable `loopreview` (all
satisfied). The IRON RULE stands: an unattended tick must never run without the
reliable verifier and the guardrails.

## Relationship to existing specs

- [`0131-multi-provider-roles.md`](0131-multi-provider-roles.md) — conductor /
  worker / verifier split, inherit-by-default, reviewer-not-worker. Unchanged;
  the Planner/Generator/Evaluator nodes map onto these roles.
- [`0146-attestation.md`](0146-attestation.md) — attestation for all roles,
  including the tick's conductor invocation.
- [`0192-delivery-guardrails.md`](0192-delivery-guardrails.md) — budget /
  circuit-breaker; the mandatory bound on every unattended tick.
- [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md) —
  the reliable verifier; the hard prerequisite, now satisfied.
- [`0212-release-distribution-and-upgrade.md`](0212-release-distribution-and-upgrade.md) —
  distribution; loopcoder's own production is its published release.
- [`0218-surface-worker-attestation.md`](0218-surface-worker-attestation.md) /
  [`0214-human-readable-attestation.md`](0214-human-readable-attestation.md) —
  attestation surfacing used in tick reports.
- [`0220-loopreview-new-spec-not-a-blocker.md`](0220-loopreview-new-spec-not-a-blocker.md) —
  doc-first verifier behavior the Planner relies on.

## Non-Goals

- No Go implementation in this design-doc PR.
- No `.delivery.yml` change in this design-doc PR.
- No new runtime dependency.
- No auto-promotion to production; the human promotion gate stays.
- No central unattended "conductor LLM session"; orchestration is deterministic.
- No hardcoded migration pipeline; a migration is a general epic plus a declared
  equivalence gate.
- No multi-project scheduler in 0.4.0.
- No weakening of guardrails, attestation, reviewer-not-worker, or the
  human-merge/promote gate.
- English-only repo artifacts.
