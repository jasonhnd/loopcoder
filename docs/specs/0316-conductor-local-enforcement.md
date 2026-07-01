---
id: 316
title: Conductor Local Enforcement
status: draft
date: 2026-07-01
issue: 316
pr: null
supersedes: []
superseded_by: []
---

# Conductor Local Enforcement

This is a design-only spec. This PR must add only this document: no Go code, no
hook code, no test, no `.delivery.yml` change, no settings change, and no edit
to any other document. Implementation belongs in separate issues after this
spec is reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

## Goal

The Conductor is the only role in the loopcoder pipeline whose local reporting
obligations are not program-constrained. Worker and Verifier attestation is
binary-validated and fail-closed, but the interactive Conductor can still hide
or summarize local command output that the playbook requires it to relay
verbatim.

This spec designs local code enforcement for those Conductor obligations:

1. a Claude Code `conductor-relay-guard` hook that forces Worker and Verifier
   attestation blocks from `loopcoder dispatch` and `loopcoder loopreview` to
   appear verbatim in local visible command output;
2. activation of the existing `hooks/conductor-attest.js` gate through the
   install contract so Conductor self-attestation is actually enforced in the
   active host settings;
3. a read-only `loopcoder status [--run <id>]` command that renders the
   delivery run status from real local `.loopcoder/` state instead of
   Conductor narration;
4. install and doctor behavior that wires and checks the active Claude Code
   hook settings.

Everything in this spec is local operational and audit surface only. It extends
[`0146-attestation.md`](0146-attestation.md),
[`0218-surface-worker-attestation.md`](0218-surface-worker-attestation.md),
and [`0306-local-only-attestation.md`](0306-local-only-attestation.md) without
changing the attestation schema, renderers, token rules, dispatch stdout
contract, model and effort inheritance, human merge gate, or
reviewer-not-worker guidance.

## Local-Only Invariant

The 0306 local-only invariant applies to every surface introduced or activated
by this spec.

Allowed local surfaces are:

- visible command stdout and stderr in the active Conductor session;
- gitignored `.loopcoder/relay/` files used by `conductor-relay-guard`;
- gitignored `.loopcoder/runs/` records used by `loopcoder status` and
  Conductor recovery;
- gitignored `.loopcoder/hooks/` state used by host hooks.

Forbidden repository-visible or GitHub-hosted surfaces are:

- PR body;
- PR comments;
- issue body;
- issue comments;
- commit message;
- merge commit body;
- merge comments;
- docs, examples, fixtures, snapshots, or any other tracked file.

The relay ledger, status output, and Conductor self-attestation records must
never be copied or persisted into any forbidden surface. If local state is
missing, corrupt, or unavailable, commands must report that absence locally
instead of recovering facts from PRs, issues, comments, commits, merge
artifacts, docs, or tracked files.

## Decision 1: `conductor-relay-guard`

`conductor-relay-guard` is a Claude Code hook that mechanically enforces local
verbatim relay of Worker and Verifier attestation output. It reuses the
operational pattern from [`hooks/conductor-attest.js`](../../hooks/conductor-attest.js):

- process Claude Code `PostToolUse` events for shell tools;
- process `Stop` events and block completion with exit code `2` when a local
  obligation remains unsatisfied;
- fail open on malformed hook input, JSON parse failures, state errors, or
  unrelated turns;
- auto-enforce only in a loopcoder conductor workspace;
- support an environment scope override.

The hook must key on deterministic command output captured in the hook
`tool_response`, not on parsing the Conductor's natural-language prose.
Summaries, paraphrases, or hand-written status lines in chat do not satisfy
the guard.

### Enforced Commands

The first implementation must cover:

- `loopcoder dispatch` when it emits a Worker attestation;
- `loopcoder loopreview` when it emits a Verifier attestation.

If `dispatch-wave` emits per-Worker attestation blocks under the same local
contracts, the implementation should route those blocks through the same guard
instead of adding a parallel mechanism.

The hook's command detection should mirror `conductor-attest.js` shell parsing:
recognize `loopcoder` or `loopcoder.exe` tokens, tolerate an explicit
`LOOPCODER_BIN` path, and stop parsing the command at shell separators. It must
not rely on brittle substring matches that would mark unrelated commands as
enforced.

### Local Relay Ledger

`loopcoder dispatch` and `loopcoder loopreview` must write an authoritative
local relay ledger before returning whenever they have a complete Worker or
Verifier attestation to surface.

The ledger path is under:

```text
.loopcoder/relay/<run>/<inv>.attest
```

The file is gitignored local state. For each attestation record produced by the
invocation, it contains:

- the exact stable header line produced by `AttestationRecord.Header()`;
- the exact pretty block produced for the local human surface;
- enough local metadata for the hook to associate the block with the command
  invocation, such as role, run id, issue or PR number when known, command
  kind, and invocation timestamp.

The header and pretty block content in the ledger must be the same bytes that
the command is supposed to expose locally. The ledger is an authoritative
fallback source for the hook; it is not a new public or tracked artifact.

For commands that can produce multiple records, such as a future guarded
`dispatch-wave`, the implementation may either append multiple ledger entries
to one invocation file or create one invocation file per record. In either
case, each record must remain individually printable by the Stop backstop.

### PostToolUse Behavior

On each successful or failed shell-tool completion for an enforced command, the
hook inspects the captured `tool_response` text.

An attestation is considered surfaced when the command output contains either:

- the canonical stable header prefix for the expected role, using
  `[attestation] role=worker` or `[attestation] role=verifier`; or
- canonical attestation JSON for the expected role with `verified: true`.

The hook then compares the visible command output with new local relay ledger
records for that invocation or session:

- If every ledger record for the invocation is visible in `tool_response`, mark
  those records as surfaced in the hook state.
- If a ledger record exists but its exact header or canonical JSON is absent
  from `tool_response`, record it as pending and unsurfaced.
- If no ledger record exists because the command failed before producing a
  complete attestation, the hook must not invent a pending block.
- If ledger discovery or state update fails, the hook fails open and allows the
  turn to continue.

The guard specifically catches cases where the Conductor redirects, suppresses,
or swallows stderr, or otherwise prevents the command-produced attestation from
appearing in local visible output. It does not judge whether later prose was
accurate, because prose is not a reliable enforcement target.

### Stop Backstop

On `Stop`, if the session has pending unsurfaced Worker or Verifier ledger
records, the hook must block completion by exiting with code `2`.

The hook stderr must print the missing verbatim block or blocks from the local
relay ledger. That stderr output is itself a local visible surface. After the
hook prints a pending block locally, it may mark that record as surfaced by the
hook so the next `Stop` event can allow the turn to complete.

The Stop message must be concise and actionable:

- identify that local verbatim attestation relay was missing;
- print each missing ledger block without reformatting;
- remind the operator that the output is local-only and must not be copied into
  PRs, issues, comments, commits, merge artifacts, docs, or tracked files.

Malformed hook input, unreadable state, unreadable ledger files, or unexpected
filesystem errors must fail open. The hook must never break unrelated Claude
Code turns or non-loopcoder workspaces.

### Scope Control

Auto-enforcement uses the same conductor workspace detection as
`conductor-attest.js`: a workspace that contains the loopcoder Conductor
playbook, the Codex loopcoder entrypoint, or equivalent loopcoder conductor
configuration.

The hook must support:

```text
LOOPCODER_RELAY_GUARD_SCOPE=always
LOOPCODER_RELAY_GUARD_SCOPE=off
```

`always` enforces outside auto-detected conductor workspaces. `off` disables
the hook. Unset or any default value uses auto-detection.

## Decision 2: Conductor Self-Attestation Activation

This spec reaffirms the existing 0146 Conductor gate. Before completing a
delivery or merge turn, the Conductor must run:

```text
loopcoder attest --role conductor --provider <provider> --model <model> --permission orchestrate --action "<delivery action>" --duration-ms <ms> --total-tokens <tokens>
```

The current enforcement hook is `hooks/conductor-attest.js`. This spec does
not redesign the `attest` command, the `AttestationRecord` schema, the
`verified: false` trust marker, or the hook's fail-open behavior.

The change specified here is activation: loopcoder's install path must wire
the existing Conductor attestation hook into the active Claude Code
`.claude/settings.json` alongside the new relay guard. A hook file present in
the repository is not enough; the hook only gates the Conductor session after
it is registered in the active settings.

Conductor self-attestation remains local-only. The emitted record is visible in
local command output and persisted in gitignored `.loopcoder/` run records for
same-host recovery. It must never be copied into PR bodies, comments, issue
bodies, issue comments, commit messages, merge artifacts, docs, or tracked
files.

## Decision 3: `loopcoder status`

`loopcoder status [--run <id>]` is a new read-only command that renders the
delivery run status table from real local state. It replaces hand-typed
Conductor status tables for delivery reporting.

The command reads local state under `.loopcoder/runs/<id>/`, including:

- Worker attempt records;
- the run event log;
- verifier records or event-log entries where present;
- the full Worker, Verifier, and Conductor `AttestationRecord` values persisted
  per 0306.

The command must not write to repository-visible or GitHub-hosted surfaces. It
prints to stdout and stderr only and may read gitignored `.loopcoder/` files.
It must not update PR bodies, issues, comments, commits, merge artifacts, docs,
tracked files, or `.delivery.yml`.

### Run Selection

When `--run <id>` is provided, the command reads exactly that run directory.

When `--run` is omitted, the command may select the latest local run by
deterministic local metadata, such as the newest `.loopcoder/runs/<id>/`
event-log timestamp. If no run can be selected, it must print a local
`not reported` or `no local run found` result rather than guessing from
GitHub.

### Rendered Fields

For each issue or Worker attempt in the run, `loopcoder status` must show:

- issue number when known;
- Worker job or attempt id when known;
- Worker provider;
- Worker model and model source;
- Worker effort;
- Worker permission;
- Worker duration;
- Worker token usage, preserving input, output, and total exactly as stored;
- Worker `verified`;
- PR number or URL when known;
- current phase and status;
- Verifier provider, model, effort, permission, duration, token usage, and
  `verified` when a verifier record exists;
- Verifier verdict when available.

Unavailable fields must render explicitly as `not reported`, `not available`,
or another consistent non-fabricated value. The command must follow the 0218
token rules: never split a total-only usage value, never infer totals except
where a display-only pretty renderer spec explicitly allows it, and never
derive missing fields from duration, log size, summary text, or provider
heuristics.

### Output Contract

The exact table layout is implementation-defined, but it must be stable enough
for a human to scan and for tests to assert field presence and missing-field
behavior. The output is a local surface, not a machine API and not a durable
GitHub artifact.

`loopcoder status` should also support a structured local mode in a later
implementation issue if automation needs it, but this spec requires only the
human local status surface.

### Forced Local Appearance

The Conductor reports delivery status by running `loopcoder status` and
relaying the command output verbatim locally, not by hand-typing a parallel
table from memory.

The status output is local-only like attestation. The Conductor must not paste
it into PR bodies, PR or issue comments, issue bodies, commit messages, merge
comments, merge commit bodies, docs, or tracked files.

If feasible in the follow-on implementation, `conductor-relay-guard` should
cover `loopcoder status` output using the same command-output principle:
visible command output satisfies the obligation, while hidden redirected output
causes a Stop backstop to print the local status rendering from gitignored
state. If status coverage proves unreliable under the available Claude Code
hook input, the attestation relay guard still remains mandatory and the
implementation must document why status hook coverage was deferred.

## Decision 4: Install And Activation Contract

loopcoder must provide an install path that wires both Conductor hooks into the
active Claude Code project settings:

- existing `hooks/conductor-attest.js`;
- new `hooks/conductor-relay-guard.js`.

The install surface may be `loopcoder skill install`, a dedicated
`loopcoder hooks install`, or both. The command must default to project-scoped
installation by writing or merging the repository's `.claude/settings.json`.
It must never edit a user's global Claude Code settings unless the user
explicitly opts into a global install target.

### Settings Merge

The install command must structurally merge hook entries instead of replacing
the whole settings file.

For both hook scripts, project settings must include:

- a `PostToolUse` hook entry with `matcher: "Bash"`;
- a `Stop` hook entry;
- command type `command`;
- command value that runs the hook from the repo root with Node;
- a bounded timeout consistent with the existing conductor hook guidance.

The merge must be idempotent. Re-running the install command should not
duplicate hook entries. Existing unrelated user hooks and settings must be
preserved.

If `.claude/settings.json` is absent, the command may create it. If it is
malformed JSON, the command must fail with a clear local error and avoid
rewriting the file.

### Doctor Warning

`loopcoder doctor` must warn when the active Claude Code settings do not
contain the loopcoder conductor hooks. The warning must identify which hook is
missing and provide the local remediation command, such as:

```text
loopcoder hooks install --project
```

The warning is analogous to the stale installed skill warning from
[`0291-skill-propagation-on-upgrade.md`](0291-skill-propagation-on-upgrade.md):
it is a conductor-enforcement readiness warning, not a provider authentication
failure and not a substitute for other doctor checks.

Doctor must not mutate settings. It only reports the active hook state and the
remediation path.

## Hook Semantics

This spec names the Claude Code mechanisms used by the follow-on hook
implementation:

- `PostToolUse` with `matcher: "Bash"` observes completed shell command output;
- `Stop` can block completion by exiting with code `2`;
- hook stderr is visible local output;
- malformed hook input should fail open;
- state is stored under gitignored `.loopcoder/hooks/`.

Codex and Gemini host notes may keep best-effort equivalents in their entrypoint
docs, but the enforcement design here is for Claude Code's documented hook
semantics. Follow-on issues may add host-specific adaptations without changing
the local-only invariant.

## Unchanged Contracts

This spec does not change:

- the 0146 `AttestationRecord` schema;
- `Header()`;
- `CanonicalJSON()`;
- pretty rendering rules;
- token usage parsing or missing-token rules;
- `verified` or `model_source` semantics;
- Worker and Verifier fail-closed strictness;
- the 0218 dispatch stdout three-record contract;
- dispatch result JSON shape except for local records already required by
  accepted specs;
- model and effort inheritance;
- reviewer-not-worker guidance;
- the human merge gate;
- the 0306 prohibition on attestation in repository-visible or GitHub-hosted
  surfaces.

## Follow-Up Issues

After this spec merges, implementation should be split into separate code
issues:

1. **Relay ledger and hook:** add local relay ledger writes for `dispatch` and
   `loopreview`, implement `hooks/conductor-relay-guard.js`, and test
   PostToolUse surfacing, Stop blocking, fail-open behavior, scope overrides,
   and gitignored state.
2. **Conductor hook installer:** add the project-scoped settings merge for both
   conductor hooks, make it idempotent, preserve unrelated settings, and avoid
   global settings unless explicitly requested.
3. **Doctor hook warning:** teach `loopcoder doctor` to warn when either
   conductor hook is missing from active Claude Code settings.
4. **Status command:** implement read-only `loopcoder status [--run <id>]`
   from `.loopcoder/runs/<id>/` state with explicit missing-field rendering.
5. **Status relay coverage:** if feasible, extend `conductor-relay-guard` to
   force local appearance of `loopcoder status` output through the same local
   command-output and Stop-backstop pattern.

## Acceptance Criteria For Follow-On Code

- `conductor-relay-guard` detects `loopcoder dispatch` and
  `loopcoder loopreview` shell-tool completions in Claude Code `PostToolUse`
  events.
- The guard marks attestations surfaced only when the captured command output
  contains the expected Worker or Verifier stable header or canonical JSON.
- The guard records pending unsurfaced attestations from gitignored
  `.loopcoder/relay/<run>/<inv>.attest` ledger files.
- On `Stop`, pending unsurfaced records block completion with exit code `2` and
  print the missing verbatim block or blocks from the local ledger.
- The guard fails open on malformed hook input, state errors, ledger read
  errors, and unrelated turns.
- The guard auto-enforces only in loopcoder conductor workspaces unless
  `LOOPCODER_RELAY_GUARD_SCOPE=always`; it disables when
  `LOOPCODER_RELAY_GUARD_SCOPE=off`.
- `loopcoder dispatch` and `loopcoder loopreview` write the exact local header
  and pretty block to gitignored relay ledger files before returning whenever a
  complete Worker or Verifier attestation exists.
- `hooks/conductor-attest.js` remains the Conductor self-attestation gate and
  is installed into active project settings with the relay guard.
- `loopcoder status [--run <id>]` reads `.loopcoder/runs/<id>/` local state and
  renders issue, provider, model, effort, permission, duration, token usage,
  `verified`, PR, phase, status, and verifier verdict without inventing missing
  facts.
- The hook installer writes or merges project `.claude/settings.json`
  idempotently for both hooks, preserves unrelated settings, and does not edit
  global settings without explicit opt-in.
- `loopcoder doctor` warns when either loopcoder conductor hook is absent from
  active settings and recommends the install command.
- Relay ledger files, status output, and Conductor records remain local-only
  and never appear in PR bodies, issue bodies, comments, commits, merge
  artifacts, docs, or tracked files.

## Acceptance Criteria For This PR

- This PR adds only `docs/specs/0316-conductor-local-enforcement.md`.
- The spec has `status: draft`, `date: 2026-07-01`, `id: 316`, `issue: 316`,
  `pr: null`, and valid frontmatter.
- The spec specifies `conductor-relay-guard` enforcement keyed on command
  output, with a local ledger, Stop-block backstop, fail-open behavior,
  workspace auto-enforcement, and an environment scope override.
- The spec reaffirms Conductor self-attestation activation through the install
  contract while keeping Conductor records local-only and `.loopcoder/`
  persisted.
- The spec defines `loopcoder status [--run <id>]` as a read-only local command
  rendered from `.loopcoder/` run state, with explicit missing-field behavior.
- The spec defines the install and activation contract for wiring both
  conductor hooks into active Claude Code project settings and for doctor
  warnings when hooks are absent.
- The spec states that the 0306 local-only invariant applies to the relay
  ledger, status output, and Conductor records.
- No Go code, hook code, test, `.delivery.yml`, settings, accepted spec body,
  command behavior, schema, renderers, token rules, or runtime dependency
  changes are included.

## Non-Goals

- No Go implementation in this design PR.
- No hook implementation in this design PR.
- No test change in this design PR.
- No `.delivery.yml` change in this design PR.
- No `.claude/settings.json` change in this design PR.
- No edit to other specs or operational docs in this design PR.
- No change to `AttestationRecord`.
- No change to `Header()`.
- No change to `CanonicalJSON()`.
- No change to pretty renderer behavior.
- No change to token usage rules.
- No weakening of Worker or Verifier fail-closed strictness.
- No change to the dispatch stdout three-record contract.
- No change to model or effort inheritance.
- No change to the human merge gate.
- No change to reviewer-not-worker guidance.
- No new repository-visible or GitHub-hosted attestation or status surface.

## Amendment 2026-07-01: Binary-Invoked Hooks

This amendment records a delivery-mechanism correction made after the follow-on
implementation shipped. It does not change any decision or contract in the spec
body above; it only replaces how the hook logic is delivered and invoked.

### Problem

Decision 4's Settings Merge required a command value that "runs the hook from
the repo root with Node" (`node hooks/conductor-attest.js` and
`node hooks/conductor-relay-guard.js`). That command form implicitly assumed the
hook scripts sit at the repo root, which is true only in loopcoder's own
repository. `loopcoder skill install` merged the settings command into consumer
repositories but never installed the `.js` scripts there, so in every consumer
repo the hooks failed to resolve (`Cannot find module`) on every event. Because
doctor's readiness check matched only the command string, it reported the hooks
healthy even though they never ran.

### Amendment

1. **Binary-invoked hooks.** The hook logic is embedded in the loopcoder binary
   and invoked as `loopcoder hook conductor-attest` and
   `loopcoder hook conductor-relay-guard`. There is no Node dependency and no
   `.js` hook file. The commands resolve regardless of the current working
   directory as long as `loopcoder` is on `PATH`, so they work in any consumer
   repository, not just loopcoder's own.
2. **Idempotent upgrade of stale entries.** The settings merge upgrades any
   pre-existing `node hooks/*.js` conductor entries to the new command form. The
   merge stays idempotent: re-running install neither duplicates entries nor
   leaves stale Node commands behind.
3. **Doctor verifies command and PATH.** `loopcoder doctor` verifies the new
   command form is present in the active Claude Code settings AND that
   `loopcoder` resolves on `PATH`, instead of matching the command string only.
   This closes the false-healthy gap where a registered command pointed at a
   binary or script that could not run.
4. **Marker-based auto-enforcement.** Auto-enforcement detection adds a
   gitignored `.loopcoder/conductor-workspace` marker file that
   `loopcoder skill install` writes into installed repositories. Detection
   recognizes this marker in addition to the existing conductor-workspace
   signals, so enforcement actually fires in installed consumer repos rather
   than only in workspaces that already carry the loopcoder Conductor playbook
   or entrypoint configuration.

### Invariant Preserved

The local-only invariant is unchanged. The `.loopcoder/conductor-workspace`
marker and all hook, relay, and status state remain under gitignored
`.loopcoder/` and never appear in PR bodies, issue bodies, comments, commits,
merge artifacts, docs, or tracked files.

## Amendment 2026-07-02: Delivery-Scoped, One-Shot Conductor-Attest Gate

Activating `conductor-attest` in the binary-invoked form (previous amendment)
exposed a design flaw in the original gate: it required a Conductor
self-attestation before *every* `Stop` in a conductor workspace, and it never
honored Claude Code's `stop_hook_active` escape valve. In a real conductor
session that blocked ordinary planning and chat turns, and any turn where the
attestation was not recorded could hard-lock the conversation with no escape.

The gate is refined as follows, without changing the local-only invariant or the
attestation schema:

1. **Delivery-scoped.** The hook watches completed shell commands for a delivery
   or merge action (`loopcoder dispatch` / `dispatch-wave` / `loopreview`, or
   `gh pr merge`) and records `delivery_seen` in its per-session state. The Stop
   gate applies only when a delivery or merge actually occurred; planning and
   chat turns are never gated.
2. **One-shot.** On a delivery turn without a Conductor self-attestation, the
   hook blocks at most once to surface the reminder, then marks the session
   `reminded` and self-clears, so it cannot loop even if the Conductor never
   attests.
3. **Escape valve.** Both `conductor-attest` and `conductor-relay-guard` honor
   `stop_hook_active`: if Claude Code signals the session is already inside a
   Stop-hook block loop, the hooks allow completion.

This keeps the Conductor self-attestation reminder on genuine delivery and merge
turns while making the gate structurally incapable of blocking a non-delivery
turn or looping.

## Relationship To Existing Specs

- [`0146-attestation.md`](0146-attestation.md) defines the shared
  attestation schema, renderers, trust marker, and Conductor self-attestation
  concept. This spec activates local hook enforcement but does not change those
  contracts.
- [`0218-surface-worker-attestation.md`](0218-surface-worker-attestation.md)
  defines Worker attestation in dispatch output and missing-token reporting.
  This spec mechanically enforces local relay of those command outputs without
  changing the stdout contract.
- [`0282-default-pretty-attestation.md`](0282-default-pretty-attestation.md)
  requires the Conductor to relay Worker and Verifier pretty blocks verbatim.
  This spec adds a local hook backstop so hidden command output is printed
  locally before the turn can complete.
- [`0291-skill-propagation-on-upgrade.md`](0291-skill-propagation-on-upgrade.md)
  defines a doctor warning pattern for stale installed skill files. This spec
  reuses that warning style for missing active conductor hooks.
- [`0306-local-only-attestation.md`](0306-local-only-attestation.md) defines
  the local-only invariant and gitignored `.loopcoder/` recovery surface. This
  spec applies that invariant to relay ledgers, status output, and Conductor
  hook state.

