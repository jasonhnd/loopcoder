# ROADMAP

<!--
Format for loopcoder work units:
- Each ## heading is one topic or unit.
- Each "- doc:" or "- code:" list item is one slice and becomes one issue.
- code slices depend on the doc slices in the same unit unless "(needs: ...)" is set.
- Slice refs are <unit-slug>/<kind>-<n>; within the same unit, <kind>-<n> works.
- Use "## [epic] ..." for a slice DAG; add "- doc:" / "- code:" lines for explicit slices.
-->

## 0.9.0 - Owned Go-native development orchestration kernel <!-- lc:u=lc-b0e1d07c3d4c -->
### Status and release intent

Planned major architecture release. This roadmap is design input, not an
authorization to publish issues or begin implementation. Issue publication and
implementation require a separate owner approval after the roadmap and its
architecture documents are reviewed.

The entire R0-R8 program is one final public release: **v0.9.0**. There are no
intermediate public releases, no prerelease version numbers, and no
v0.9.1-v0.9.7 (or similar) ship points. Internal progress uses development
gates only; those gates are not versions and do not produce public packages.

v0.8.1 remains the public production baseline and the self-bootstrap controller
throughout v0.9 development. It receives only approved P0/P1 correctness,
security, data-loss, runaway-process, and release-blocking fixes while v0.9.0
is developed. No v0.9 candidate binary, tag, or install path may replace v0.8.1
as the production or self-bootstrap controller before the R8 release gate
passes on the integrated release SHA.

v0.9.0 is not a fourth feature layer on top of the v0.8 implementation. It is a
controlled replacement and consolidation of the core path, with old commands
kept as compatibility shims only until the new path has proven parity. The only
final release produced by this roadmap is v0.9.0 after all R0-R8 gates pass.

### Product mission

LoopCoder owns the complete local development-delivery control plane:

```text
GitHub issue
  -> durable work graph
  -> deterministic provider/model admission
  -> one bounded provider runtime by default
  -> process and progress supervision
  -> pull request
  -> remote CI
  -> independent verification
  -> explicit human merge or configured release gate
```

The product must be useful first as a quiet, reliable tool for one developer on
macOS Apple Silicon who works across many local repositories, uses public and
private GitHub repositories, has subscriptions from several model providers,
and may move to another computer after the current GitHub issue/PR is finished.
Large autonomous fleets are an optional later mode, not the default product.

LoopCoder is host-neutral. It can be invoked from a terminal, Paseo, Codex,
Claude Code, or another future host, but none of those hosts owns its execution
truth. The authoritative machine interface is structured JSON/JSONL plus exit
status; human-readable rendering and host relay are projections over the same
persisted events.

### Ownership and independence contract

The shipped LoopCoder binary and its official adapters must not require Gas
City, Beads, Plexus, Dolt, tmux, or their databases, daemons, configuration
files, CLIs, or release cadence. LoopCoder may study their public behavior and
architecture, but it owns its domain model, Go interfaces, persistence schema,
routing policy, process lifecycle, reports, tests, and compatibility policy.

"Independent" has a precise boundary:

- LoopCoder does not depend on Gas City, Beads, or Plexus at build time or run
  time.
- LoopCoder still intentionally depends on macOS, Git, GitHub, and the selected
  model provider's supported CLI/API/authentication surface. Those dependencies
  are isolated behind LoopCoder-owned ports.
- A provider-side change may require one official Go adapter to change, but it
  must not require the scheduler, work graph, storage, or delivery core to
  change.
- Official Codex, Claude, Gemini/Antigravity, and Grok adapters ship under the
  LoopCoder release process. A future fifth provider implements the same
  versioned contract.
- No external project may become the authoritative store for LoopCoder work,
  attempts, provider choices, reports, or delivery outcomes.

### External research baseline and license policy

The architecture research for this roadmap is pinned to immutable upstream
revisions so later upstream direction changes cannot silently change the v0.9.0
requirements:

| Project | Research revision | Top-level license | What is studied |
| --- | --- | --- | --- |
| Gas City | [`9ddbea5`](https://github.com/gastownhall/gascity/tree/9ddbea5c0b4b3cebf09fc36c0f88a8c52f9dd991) | [MIT](https://github.com/gastownhall/gascity/blob/9ddbea5c0b4b3cebf09fc36c0f88a8c52f9dd991/LICENSE) | Reconciliation, runtime/session control, formulas, durable work, append-only events |
| Beads | [`f9b2020`](https://github.com/gastownhall/beads/tree/f9b20203e851a48c976cfd0e155b413dd80cb5cf) | [MIT](https://github.com/gastownhall/beads/blob/f9b20203e851a48c976cfd0e155b413dd80cb5cf/LICENSE) | Issue lifecycle, dependency readiness, atomic claim/close, audit history, compaction |
| Plexus | [`95c8519`](https://github.com/mcowger/plexus/tree/95c8519a931ba85a5a91e57312cdd4d6cd382da0) | [MIT](https://github.com/mcowger/plexus/blob/95c8519a931ba85a5a91e57312cdd4d6cd382da0/LICENSE) | Provider inventory, quota windows, health, cooldown, target selection, observations |

Primary architecture references are Gas City's
[`how-gas-city-works.md`](https://github.com/gastownhall/gascity/blob/9ddbea5c0b4b3cebf09fc36c0f88a8c52f9dd991/docs/getting-started/how-gas-city-works.md),
Beads'
[`PROJECT_CHARTER.md`](https://github.com/gastownhall/beads/blob/f9b20203e851a48c976cfd0e155b413dd80cb5cf/engdocs/PROJECT_CHARTER.md),
and Plexus'
[`CONFIGURATION.md`](https://github.com/mcowger/plexus/blob/95c8519a931ba85a5a91e57312cdd4d6cd382da0/docs/CONFIGURATION.md).

All three top-level projects are MIT-licensed at the pinned revisions. MIT
permits use, modification, redistribution, sublicensing, and sale, provided its
copyright and permission notice is retained in copies or substantial portions.
Beads also carries a separate
[`THIRD_PARTY_LICENSES`](https://github.com/gastownhall/beads/blob/f9b20203e851a48c976cfd0e155b413dd80cb5cf/THIRD_PARTY_LICENSES)
inventory; a top-level MIT license must never be assumed to relicense every
dependency, generated asset, or vendored file.

The approved v0.9.0 approach is an independent behavioral rewrite, not a source
port:

- Read public code and documentation to understand constraints, failure modes,
  and observable behavior.
- Write LoopCoder-owned requirements, interfaces, state transitions, and tests
  in LoopCoder terminology.
- Do not copy or line-by-line translate source files, tests, fixtures, schemas,
  prompts, documentation prose, generated clients, or user-facing names.
- Do not use Gas City, Beads, or Plexus names as LoopCoder product concepts or
  imply upstream endorsement.
- Record each adopted idea, rejected assumption, pinned source revision, and
  LoopCoder implementation location in an inspiration ledger.
- Any future exception that copies or ports code requires separate owner and
  license review, isolated provenance, preserved notices, and an update to
  `THIRD_PARTY_NOTICES.md` before merge. No such exception is planned here.
- This is an engineering provenance policy, not a substitute for legal advice.

### Source-of-truth contract

| Concern | Authority | Explicitly not authoritative |
| --- | --- | --- |
| Requested work, collaboration, completion | GitHub issue/PR/check/review state | Local chat transcript, local task cache |
| Cross-computer continuation | Completed GitHub issue/PR/commit history | Copied SQLite files, state branches, cross-host leases |
| Current local execution | Local SQLite job/attempt/event records plus observed process state | Provider self-report alone |
| Code changes | Git commit and PR head | Worker prose saying work is complete |
| Merge and release eligibility | Configured remote CI, independent verifier, explicit gate | Worker exit code alone |
| Provider credentials and account ownership | Provider CLI/keychain/auth system | LoopCoder database |
| Provider quota | Fresh provider-authoritative evidence when available | Guessed values or LoopCoder-observed usage alone |

GitHub is the only synchronization mechanism required between the owner's
computers. The normal handoff occurs after the current issue/PR is terminal. A
new computer rehydrates from GitHub and creates fresh local execution state; it
does not merge in-flight local scheduler databases. Another developer working
on the same repository is ordinary GitHub collaboration, not a distributed
LoopCoder database peer.

### Default operating mode

`loopcoder run <issue>` is the primary v0.9.0 command and the first complete
vertical slice. Its default behavior is deliberately small:

- One GitHub issue, one top-level worker attempt, one worktree, and one PR.
- One selected provider/model is pinned for the whole attempt.
- Provider-native sub-agents may be allowed by policy, but remain inside the
  selected attempt and its supervised process tree.
- Provider-native children cannot independently own GitHub issues, branches,
  PRs, merges, release actions, or LoopCoder task truth.
- The verifier never runs concurrently with the worker on the same local host.
- Waiting for CI, approval, quota reset, or a scheduled retry performs zero
  model calls.
- Full repository test, race, static analysis, security, and release matrices
  run remotely. Local execution is limited to focused checks needed for the
  current change.

An explicit bounded workflow mode may later materialize a small dependency
graph. It is not silently selected. The initial production limit is depth <= 3,
fan-out <= 3, active top-level workers <= 1 by default, and an operator-visible
plan before any child starts. Provider-native parallelism is separately bounded
as part of the parent process tree.

### Go-native target architecture

The new core has seven logical responsibilities. These are boundaries, not a
mandate to create one package per noun or duplicate existing packages.

| Component | Owned responsibility | Primary inspiration |
| --- | --- | --- |
| Work Graph | Work items, edges, ready set, atomic claim/close, bounded plans | Beads |
| Provider Plane | Installation, auth readiness, model catalog, quota/health snapshots | Plexus |
| Router | Eligibility, capability tiers, deterministic selection, fallback decisions | Plexus plus LoopCoder policy |
| Runtime | Launch, signal, inspect, attach, stop, and recover provider CLI attempts | Gas City |
| Supervisor | Process-tree resources, liveness, evidence progress, timeout, cleanup | LoopCoder operational failures plus Gas City lifecycle |
| Delivery | Worktree, Git, PR, CI watcher, verifier, merge/release gates | Existing LoopCoder |
| Store and Events | SQLite transactions, append-only events, compact projections, reports | Beads atomicity plus Gas City events |

The v0.9 core vocabulary is intentionally small: `Project`, `WorkItem`,
`Dependency`, `Job`, `Attempt`, `Provider`, `Model`, `Gate`, and `Event`. Gas
City's City/Rig/Pack/Convoy vocabulary and Beads' universal bead type do not
become LoopCoder concepts.

### Adoption matrix

#### Gas City

Adopt through independent Go implementation:

- Reconcile declared work with observed sessions instead of trusting callbacks.
- Make work durable outside any one model or chat session.
- Separate reusable workflow definitions from materialized workflow runs.
- Represent lifecycle observations as immutable, sequenced events that can be
  replayed from a cursor.
- Expose a provider-neutral runtime interface for launch, liveness, stop,
  adoption, logs, and metadata.
- Recover only when ownership and process state are proven; ambiguous live work
  becomes `needs-human` rather than a duplicate launch.

Do not adopt:

- Mandatory tmux, Beads, Dolt, repo mutation, or always-on city directories.
- Unlimited fleets, role trees, autonomous scheduled orders, or idle patrol
  loops in the default product.
- A liveness signal that can stay green while the provider is blocked on an
  interactive dialog.
- Polling loops that create unbounded subprocesses, CPU use, wakeups, or battery
  drain.

#### Beads

Adopt through independent Go implementation:

- A dependency-aware work graph whose ready set excludes open blockers.
- Atomic claim and guarded close in one database transaction.
- Stable work identity, idempotent replay, audit history, and explicit terminal
  states.
- Metadata for integration-specific data before adding permanent schema fields.
- Bounded compaction of old terminal execution detail into a summary while
  retaining provenance and final outcome.

Do not adopt:

- Dolt as the default store, embedded/server Dolt lifecycle, Dolt remotes, or
  database branching and merge semantics.
- `.beads` state, identity files, hooks, agent instruction mutation, or task
  databases inside a customer's repository.
- A second GitHub issue truth or lossy bidirectional GitHub synchronization.
- Distributed multi-writer machinery for a topology that hands off only after a
  GitHub issue/PR is complete.

#### Plexus

Adopt through independent Go implementation:

- Provider/model inventory with capabilities, freshness, confidence, and
  provenance.
- Separate quota windows, including short rolling windows and weekly windows,
  with reset timestamps and unknown/stale states.
- Persistent provider/model health, classified failures, and bounded cooldown.
- Ordered target groups and deterministic selection policies.
- Request/attempt observations for latency, reliability, usage, and outcome.
- Hard timeouts and stall classification that do not confuse a connected
  process with useful progress.

Do not adopt:

- A general OpenAI/Anthropic/Gemini HTTP translation proxy in v0.9.0.
- LoopCoder custody of provider secrets or a duplicate OAuth login UI.
- Request-by-request invisible vendor switching inside a coding session.
- Random inline exploration or paid background probes that consume the user's
  subscription quota merely to refresh performance rankings.
- Routing unsupported coding CLIs through an API compatibility layer without an
  explicit end-to-end compatibility proof.

### Work Graph contract

The default issue compiles to a one-node work graph. Decomposition is optional,
bounded, persisted, and visible before execution. A work item has stable local
identity plus an external reference such as `github:owner/repo#123`. Blocking
edges are acyclic, and ready-set calculation is deterministic.

Required state transitions:

```text
planned -> ready -> claimed -> running -> waiting -> terminal
```

Terminal states are `succeeded`, `failed`, `cancelled`, and `needs-human`.
Transitions are guarded and append one event in the same transaction. Claiming
and closing are compare-and-set operations. A replay either returns the prior
result or advances one legal transition; it never starts a second provider for
the same active attempt.

### Local data contract

Use the existing pure-Go `modernc.org/sqlite` dependency through
`database/sql`. The v0.9 schema is intentionally compact and should begin with
no more than these durable concepts:

| Table/projection | Purpose |
| --- | --- |
| `projects` | Stable GitHub identity, current local path aliases, public/private classification |
| `work_items` | Local projection of the root issue and optional bounded child work |
| `dependencies` | Validated blocking edges |
| `jobs` | One accepted execution objective and its policy |
| `attempts` | Provider/model-pinned process lifecycle and outcome |
| `events` | Append-only sequenced facts with idempotency keys |
| `provider_snapshots` | Installation, auth, catalog, quota, health, and freshness observations |
| `route_decisions` | Candidate set, exclusions, ranking facts, selected target, fallback boundary |
| `verifications` | CI/verifier/gate evidence and final decision |

Reports are projections over events, not a second competing lifecycle store.
Payload extensions prefer versioned JSON metadata until a field proves broadly
stable. Runtime files, logs, and the database live under
`$LOOPCODER_HOME/projects/<project-id>/`; no registered-project state is written
under the business repository.

The v0.8 database remains readable/exportable during migration but is never
mutated by the v0.9 engine. There is no automatic destructive migration. A
failed import leaves the old database untouched and removes the incomplete new
database.

### Provider adapter contract

Official adapters implement a versioned Go interface owned by LoopCoder:

```text
Discover
ProbeAuth
ListModels
ReadQuota
Capabilities
BuildInvocation
ParseObservation
ClassifyOutcome
```

Adapters must not return credentials. Provider authentication remains in the
provider CLI/keychain. Quota collection follows this order:

1. Supported provider CLI or documented provider API.
2. Provider-owned local status data with a documented schema.
3. An explicitly experimental, isolated adapter for an undocumented endpoint,
   disabled by default and allowed to fail as `unknown`.

No HTML/private-UI scraping is a production authority. Unsupported Grok or
Antigravity quota windows remain `unknown`; they are not fabricated from local
token counts. Adapter probes are bounded, rate-limited, cached, redacted, and
never launch a paid model request merely to test availability.

### Capability tiers and model change

`Luna`, `Tera`, and `Soul` are LoopCoder capability/risk tiers, not permanent
aliases for one vendor's current model names:

- `Luna`: low-risk documentation, formatting, narrow tests, and mechanical
  edits.
- `Tera`: default implementation, debugging, and ordinary repository work.
- `Soul`: architecture, security, release blockers, high-risk migrations, and
  independent verification.

Each provider adapter maps currently discovered models to supported tiers with
freshness and provenance. A new model such as a future GPT, Claude, Gemini, or
Grok release is added to adapter/catalog data and conformance fixtures; it does
not require new scheduler logic. Explicit user pins win when eligible. An
unknown model may pass through only under an explicit non-strict policy and is
never silently assigned high-risk capabilities.

### Deterministic routing and quota policy

A language model does not choose which company receives the task. LoopCoder's
deterministic router makes and explains that decision from stored facts:

1. Hard eligibility: installed, authenticated, compatible host/runtime,
   required permission, context/tool capability, and policy eligibility.
2. Explicit operator pin.
3. Required capability tier and task risk.
4. Fresh authoritative quota windows and reset/expiry pressure.
5. Configured weekly reserve for high-risk work.
6. Cooldown, recent reliability, completion speed, and bounded local load.
7. Stable deterministic tie-break.

The router does not simply choose the largest remaining percentage. It prefers
usable quota that will expire sooner while preserving configured reserves and
ensuring the model can finish the task. Every decision records candidates,
exclusions, source freshness, unknowns, score components, selection, and legal
fallbacks.

Quota exhaustion or a provider failure may start a successor attempt only at an
attempt boundary. The current attempt must first stop and join its process tree,
persist its commit/diff/log/recovery summary, release resources, and classify
whether provider execution definitely stopped. Ambiguous provider execution is
`needs-human`, not automatic failover.

### Runtime and sub-agent contract

The runtime owns the top-level provider process and every descendant it creates.
It must support start, inspect, signal, bounded stop, force stop, join, log tail,
and recovery evidence. It records the actual provider/model/effort returned by
the provider when observable and reports disagreement with the requested value.

Provider-native sub-agents are an implementation detail of one top-level
attempt. They may improve speed but cannot escape:

- the same worktree and write boundary;
- the parent's CPU, RSS, process-count, time, and quota budget;
- one authoritative parent attempt and one PR owner;
- the parent's cancellation and process-group cleanup;
- the rule that only LoopCoder creates GitHub issues/PRs or performs merges.

LoopCoder-managed cross-provider child work is available only in explicit
workflow mode, where each child is a visible Work Graph node with its own claim,
attempt, resource budget, and merge boundary.

### Resource and progress supervisor

The supervisor, not the model, generates the mandatory progress receipt. At
least every five minutes while an attempt is active, it emits a compact report
from observable evidence:

- phase and elapsed time;
- provider/model/effort and attempt identity;
- process and descendant count, CPU, RSS, and resource trend;
- changed files/diff size, latest commit, focused-test changes, and PR/CI state;
- last meaningful provider output time;
- next bounded action, current blocker, and timeout deadline.

Model prose may be attached but cannot satisfy the receipt timer. Two
consecutive intervals with no meaningful evidence progress trigger a bounded
diagnostic and then cancel/detach according to policy, returning control to the
operator. The supervisor may never hide for hours behind `go test`, a pre-push
hook, CI polling, or a provider stream.

Initial self-hosting defaults:

- one active top-level worker;
- verifier never concurrent with worker;
- one local test command at a time;
- focused local command soft deadline 10 minutes, hard deadline 15 minutes;
- maximum eight descendant processes unless explicitly raised;
- process-tree RSS ceiling 2 GiB and CPU ceiling 150% on the owner Mac;
- one automatic retry per issue;
- full race/static/security/release tests remote-only;
- native sub-agents disabled by default for LoopCoder self-development until
  resource accounting is proven.

Stopping an attempt must stop and join all descendants within a bounded period,
remove watchers and timers, close descriptors, release the worktree claim, and
leave no background test, provider, Git, or polling processes. Cleanup failure
is visible and blocks a new attempt on the same work item.

### Delivery and zero-model waiting

Retain and simplify LoopCoder's valuable delivery path:

- GitHub issue intake and repository identity;
- isolated `git worktree` implementation and review workspaces;
- LoopCoder-owned commit, push, and PR creation;
- hosted CI as authoritative heavy verification;
- independent read-only verifier using a different eligible provider;
- explicit human merge/release gate by default;
- redacted reporter and doctor/upgrade surfaces.

CI, approval, quota-reset, and retry timers run as local deterministic watchers
with no model invocation. They wake the control loop only on a state change,
deadline, or mandatory five-minute human receipt. A watcher restart replays its
cursor from SQLite/GitHub and does not duplicate comments, reports, provider
calls, PRs, or merges.

### Host-neutral attach and reporting

`loopcoder run` stays attached by default and streams bounded structured events
and readable receipts. Explicit detached execution starts a LoopCoder-owned
supervisor only for the lifetime of that job; it is not an always-on host daemon.
`loopcoder status` and `loopcoder attach` replay from a durable cursor, so a host
session may close and reopen without losing reports or adopting raw provider
process ownership.

Host integrations consume the same versioned report contract:

- JSON/JSONL is authoritative and stable enough for terminal, Paseo, Codex,
  Claude Code, hooks, and future clients.
- Pretty text is compact and designed for humans, but never parsed back as
  machine state.
- A host relay acknowledges a receipt cursor; failure to relay remains visible
  and does not block local process supervision or fabricate delivery.
- Raw provider logs remain bounded local evidence and are not dumped into chat
  as the progress interface.
- No host adapter may bypass resource admission, mutate route decisions, or
  convert an ambiguous provider state into success.

### Multiple repositories, private repositories, and computers

- Project identity is the normalized GitHub host/owner/repository identity, not
  only a mutable local folder name.
- One project may have multiple local path aliases over time; two different
  GitHub repositories never share execution rows.
- All local repositories use one LoopCoder home with project-scoped state and
  bounded global resource admission.
- Public/private classification controls redaction and diagnostics; private
  issue bodies, diffs, logs, prompts, and credentials are never exposed through
  public reports or another repository's state.
- Quota snapshots contain capacity metadata, not task prompts or repository
  content.
- A completed task can move computers through GitHub alone. In-flight local
  database replication, state branches, and cross-computer leases are out of
  scope for v0.9.0.

### Existing v0.8 implementation disposition

v0.9.0 must reuse proven mechanics instead of rewriting everything, but it must
not preserve accidental complexity merely because tests exist.

Keep after focused audit:

- project-home/path identity and external state layout;
- Git/worktree and GitHub PR mechanics;
- provider CLI invocation code that obeys the new Runtime contract;
- process-group termination/guardian primitives that pass real macOS tests;
- `loopreview`, sanitization, reporter rendering, doctor, upgrade, and focused
  verification behavior that pass the new vertical-slice acceptance suite.

Consolidate behind the new boundaries:

- `providerinventory`, `providerreconcile`, `quotaheadroom`, `availability`,
  `budget`, `usageledger`, `models`, `runtimecap`, and `routing` into Provider
  Plane plus Router responsibilities;
- planner/task requirement/nested graph behavior into Work Graph and explicit
  Workflow responsibilities;
- progress, progress-host, relay, report-query, wait-state, and orchestration
  cost behavior into one event/report/watcher model;
- duplicated lifecycle and ownership records into the compact v0.9 store.

Deprecate on the new path, then remove after compatibility evidence:

- state-branch push/pull and cross-computer conductor leases;
- default autonomous `dispatch-wave`, `tick`, and `trigger` behavior;
- the full nested agent federation/ownership graph as the ordinary way to run
  one issue;
- layered delivery-run approval/override/fingerprint machinery that duplicates
  GitHub and the new Job/Attempt/Gate model;
- distributed outbox claim/lease/ack/cursor semantics where a single local
  sequenced event cursor is sufficient.

Old commands remain clearly marked compatibility surfaces for one release and
call the old engine only when explicitly requested. v0.9.0 never mixes old and
new writers in one database or one active work item. Removal targets and usage
telemetry are documented before v1.0.

### Development governance and issue sizing

LoopCoder self-development must use the same bounded product path it is trying
to ship:

- Work in progress is one implementation issue/PR at a time until the direct
  vertical slice is production-proven.
- A worker issue owns one state invariant, one adapter method family, one
  migration step, or one operator-visible behavior. It must not span multiple
  architecture components.
- Target 30-90 minutes of worker execution. If the plan cannot explain why the
  issue fits, split it before dispatch rather than after a timeout.
- Every issue states files/ownership boundary, acceptance tests, non-goals,
  resource ceiling, timeout, and rollback behavior.
- No worker may create follow-up issues, widen scope, merge, promote, amend the
  roadmap, or start the next issue.
- Documentation and contract merge before implementation; implementation does
  not redesign a merged contract inside a code PR.
- Five-minute supervisor receipts are mandatory during all self-hosting work.
- Heavy full-repository gates run once on remote CI, not repeatedly on the
  owner's computer or in pre-push hooks.
- A red required check gets one diagnosis and one scoped repair. Repeated broad
  repair loops require owner review.

#### Architecture-release exception (v0.9.0 only)

The self-hosting playbook normally budgets **8-12 implementation issues per
minor public release**. That budget remains the default for ordinary minor
work.

v0.9.0 is an owner-approved, one-time **architecture-release exception**: the
full R0-R8 program (14 documentation slices + 56 implementation slices = 70
planned slices) ships as a single final public version, **v0.9.0**. The
exception does **not** relax issue sizing, single-PR-in-flight, self-bootstrap
controller rules, resource ceilings, or phase exit criteria. It only permits
the cumulative implementation count across the whole architecture program to
exceed 8-12 issues because those issues are serialized through internal
development gates rather than intermediate public releases.

This exception applies only to the v0.9.0 architecture program defined in this
roadmap. Later minor releases return to the normal 8-12 implementation-issue
budget unless the owner records a new, explicit exception.

#### Internal development gates (not versions)

Progress is tracked by **internal development gates**. Gates are not version
numbers, not prereleases, and not public ship points. They exist only to bound
activation, issue publication, and in-flight work while the single final
release remains v0.9.0.

| Gate | Scope | Code slices | Notes |
| --- | --- | --- | --- |
| Gate A | R0 + R1 | 6 | R0.5, R1.3-R1.7 (plus R0/R1 docs before code) |
| Gate B | R2 | 4 | R2.2-R2.5 |
| Gate C | R3.1-R3.10 | 9 | R3.1 is documentation; R3.2-R3.10 are code (provider core + Codex + Claude) |
| Gate D | R3.11-R3.17 + R4 | 11 | Antigravity/Grok/future kit + Router |
| Gate E | R5 | 8 | Runtime and Supervisor |
| Gate F | R6 | 7 | Direct `loopcoder run` vertical slice |
| Gate G | R7 | 5 | Explicit bounded workflow mode |
| Gate H | R8 | 6 | Migration, deletion, release qualification |

Gate activation rules:

- Owner approval activates a **gate boundary**, not every slice inside that
  gate. Opening Gate C means R3.1-R3.10 are the eligible set; it does not
  convert or publish them in bulk.
- Only **one** gate may be open at a time. The next gate must not open until
  the prior gate's phase exit criteria pass.
- Only **one** `planned-doc` or `planned-code` slice may be converted to
  `doc`/`code` at a time. Never convert or compile an entire gate in one
  activation PR.
- At most **one** v0.9 documentation or implementation issue/PR may be in
  flight at a time.
- Each documentation slice must be accepted (merged and closed with exit
  evidence) before its dependent code slice is activated or published.
- The next slice starts only after the current slice is merged, closed, and
  its cleanup/exit evidence is complete.
- Completing a gate does **not** create a public release, tag, or controller
  replacement. Only after Gate H (R8) and the final acceptance gates pass may
  **v0.9.0** ship as the sole public release from this roadmap.
- v0.8.1 remains the public production and self-bootstrap controller until the
  R8 release gate passes; no intermediate v0.9 candidate may replace it.

### Planned slices

The stable IDs below must survive later issue publication. Each bullet is one
issue and one PR. Issues are not to be published until the owner approves the
complete roadmap and then opens the next internal development gate.

Unapproved slices intentionally use `planned-doc` and `planned-code`, which the
current `loopcoder compile` parser ignores. Activation is strictly serialized:
owner approval opens one gate boundary; then exactly one planned slice is
converted to `doc` or `code` in a small activation change; that single slice
is published, delivered, merged, closed, and cleaned up with exit evidence
before the next planned slice is converted. Dependent code slices wait until
their documentation slices are accepted. Never convert, compile, or publish an
entire gate—or the complete v0.9.0 backlog—in one step. Gates A-H bound which
slices may become eligible; they do not split the program into public v0.9.x
intermediate releases. The only final public release remains **v0.9.0**.

#### Active-slice lifecycle (compiler-visible count = 1)

Compiler-visible active slices are only lines starting with `- doc:` or
`- code:`. History uses `shipped-doc` / `shipped-code`. Future work stays
`planned-doc` / `planned-code` (ignored by `loopcoder compile`).

When the current active slice is completed and merged:

1. Convert that slice from `doc`/`code` to `shipped-doc`/`shipped-code` in the
   same activation change that advances the program.
2. In that same activation PR, convert exactly one next
   `planned-doc`/`planned-code` line to `doc`/`code`.
3. Do not leave zero or more than one compiler-visible `doc`/`code` slice.

This keeps the active compiler-visible count exact: **1**. The v0.8.1
`[epic]` dark-migration expander must not be used for the v0.9.0 program;
top-level units are ordinary `##` headings with explicit planned/doc/code
lines only.


## Phase R0 - Charter, provenance, and measurable baseline <!-- lc:u=lc-2a56dc031a9b -->

- shipped-doc: **R0.1 Product charter** - define user, default one-worker workflow, <!-- lc:u=lc-7304f225c290 -->
  ownership boundaries, GitHub/local/provider authorities, default/advanced
  modes, and explicit non-goals.
- doc: **R0.2 External inspiration ledger** - pin upstream revisions, licenses, <!-- lc:u=lc-c785349ec276 -->
  adopted concepts, rejected assumptions, rewrite-only policy, and provenance
  review checklist.
- planned-doc: **R0.3 v0.8 disposition map** - map every current top-level subsystem to
  keep, consolidate, compatibility-only, or remove; name the owning v0.9
  component and deletion gate.
- planned-doc: **R0.4 Operational SLOs** - five-minute reports, stop/join deadline,
  resource ceilings, zero-model waits, issue duration, retry budget, and no
  duplicate PR/provider execution.
- planned-code: **R0.5 Baseline measurement command** - report current binary startup,
  one dry-run path, process count, CPU/RSS, database/table count, package count,
  local test duration, and report latency without changing repository state.

Exit: the owner can identify exactly what v0.9 will build, replace, retain, and
refuse before the first new runtime code merges.

## Phase R1 - Compact domain and event store <!-- lc:u=lc-997e40a9ed29 -->

- planned-doc: **R1.1 Domain/state contract** - define Project, WorkItem, Dependency,
  Job, Attempt, Provider, Model, Gate, Event, legal transitions, idempotency, and
  authority.
- planned-doc: **R1.2 Storage/migration contract** - define the compact schema,
  transaction boundaries, event cursor, v0.8 read-only import, backup, abort,
  rollback, and file permissions.
- planned-code: **R1.3 Store open/schema foundation** - create/open, schema version,
  owner-only permissions, integrity check, and close behavior without domain
  writes.
- planned-code: **R1.4 Append-only event and cursor API** - idempotency key,
  monotonic sequence, ordered replay, and projection checkpoint primitives.
- planned-code: **R1.5 SQLite contention/restart policy** - WAL, busy classification,
  bounded retry, transaction rollback, abrupt-restart, and two-connection tests.
- planned-code: **R1.6 Project identity projection** - GitHub identity, path aliases,
  repository privacy, same-name isolation, and multi-repository tests.
- planned-code: **R1.7 Job/attempt state machine** - guarded transitions and atomic event
  append without provider execution.

Exit: deterministic storage tests pass under crash/replay/busy conditions with
no repo-local state and no old-engine writes.

## Phase R2 - Work Graph <!-- lc:u=lc-415c61df69c9 -->

- planned-doc: **R2.1 Work Graph contract** - one-node default, edge semantics, graph
  bounds, ready set, claim/close guards, GitHub projection, and compaction.
- planned-code: **R2.2 Graph validation and ready set** - cycle/root/depth/fan-out checks
  and deterministic ordering as pure Go logic.
- planned-code: **R2.3 Atomic claim and guarded close** - compare-and-set transitions and
  event append in one SQLite transaction with two-connection contention tests.
- planned-code: **R2.4 GitHub issue projection** - import/reconcile the root issue and
  terminal PR/check outcome without bidirectional task-database sync.
- planned-code: **R2.5 Terminal compaction** - bounded summary retaining final state,
  source links, decisions, verification, and event hash/cursor.

Exit: replay and two-process tests prove one claim, one terminal transition, and
one GitHub external identity.

## Phase R3 - Provider Plane <!-- lc:u=lc-e00d552a3b1a -->

- planned-doc: **R3.1 Provider adapter protocol** - versioned interface, timeout,
  redaction, auth ownership, quota source authority, model capability, and
  conformance fixtures.
- planned-code: **R3.2 Adapter conformance harness** - common fake CLI/API fixtures,
  deadlines, redaction assertions, unknown/stale semantics, and contract golden
  files without a real provider call.
- planned-code: **R3.3 Snapshot/cache core** - typed installation, auth, model, quota,
  and health observations with freshness/confidence and no credentials.
- planned-code: **R3.4 Health and cooldown core** - classified failures, success reset,
  bounded exponential cooldown, persistence, and per-provider/model isolation.
- planned-code: **R3.5 Codex discovery/auth/catalog adapter** - executable discovery,
  non-secret auth readiness, models, capabilities, and fixtures.
- planned-code: **R3.6 Codex quota adapter** - authoritative short/weekly windows when
  available, reset timestamps, caching, redaction, and honest unknown fallback.
- planned-code: **R3.7 Codex invocation/outcome adapter** - argv/environment, bounded
  output observations, actual model/effort evidence, and typed outcome mapping.
- planned-code: **R3.8 Claude discovery/auth/catalog adapter** - executable discovery,
  non-secret auth readiness, models, capabilities, and fixtures.
- planned-code: **R3.9 Claude quota adapter** - authoritative five-hour/seven-day
  windows when available, reset timestamps, redaction, and unknown fallback.
- planned-code: **R3.10 Claude invocation/outcome adapter** - argv/environment,
  bounded observations, actual model/effort evidence, and typed outcomes.
- planned-code: **R3.11 Antigravity/Gemini discovery/auth/catalog adapter** - `agy`
  discovery, non-secret readiness, dynamic models/capabilities, and fixtures.
- planned-code: **R3.12 Antigravity/Gemini quota adapter** - bounded supported sources;
  unavailable or unauthoritative windows remain `unknown`.
- planned-code: **R3.13 Antigravity/Gemini invocation/outcome adapter** - workspace
  pinning, argv/environment, bounded observations, and typed outcomes.
- planned-code: **R3.14 Grok discovery/auth/catalog adapter** - executable discovery,
  non-secret readiness, dynamic models/capabilities, and no fabricated default.
- planned-code: **R3.15 Grok quota adapter** - bounded supported sources; unavailable or
  unauthoritative windows remain `unknown`.
- planned-code: **R3.16 Grok invocation/outcome adapter** - argv/environment, bounded
  observations, actual model/effort evidence, and typed outcomes.
- planned-code: **R3.17 Future-provider registration kit** - fixtures and a small adapter
  registration path that adds a fifth provider without scheduler changes.

Exit: each installed provider can be discovered and explained independently;
one failing adapter cannot block or corrupt the others.

## Phase R4 - Deterministic Router <!-- lc:u=lc-dc8d98eaaedf -->

- planned-doc: **R4.1 Routing policy** - hard eligibility, Luna/Tera/Soul mapping,
  explicit pins, quota expiry pressure, weekly reserve, cooldown, reliability,
  stable tie-break, fallback boundary, and full explanation schema.
- planned-code: **R4.2 Candidate eligibility** - pure deterministic exclusions with
  golden fixtures for missing auth, unsupported permissions, stale quota,
  unknown quota, cooldown, and model capability.
- planned-code: **R4.3 Quota-aware ranking** - reset-aware ranking and reserve policy;
  prove it does not merely choose the largest remaining percentage.
- planned-code: **R4.4 Persisted route decision** - one decision per attempt boundary,
  replay stability, operator pin behavior, and redacted explanation output.
- planned-code: **R4.5 Successor attempt policy** - stop/checkpoint/join before legal
  failover; ambiguous provider execution becomes `needs-human`.

Exit: a table-driven simulation covers all providers, short/weekly windows,
unknown/stale evidence, exhaustion, and deterministic replay without a model
call.

## Phase R5 - Runtime and Supervisor <!-- lc:u=lc-f6a32c5179e8 -->

- planned-doc: **R5.1 Runtime/process contract** - process tree ownership, start,
  observe, signal, stop, join, recovery evidence, native sub-agent boundary, and
  interactive-dialog/stall classification.
- planned-code: **R5.2 macOS process-tree runtime** - launch and observe one provider
  process group with bounded logs and no task semantics.
- planned-code: **R5.3 Stop/join cleanup** - graceful deadline, force termination,
  descendant proof, descriptor/watcher cleanup, and zero-leftover integration
  tests.
- planned-code: **R5.4 Resource policy and admission** - global/per-attempt worker,
  verifier, local-test, descendant, CPU, RSS, and time budgets with pure policy
  decisions and visible denial reasons.
- planned-code: **R5.5 macOS resource sampling/enforcement** - process-tree CPU/RSS/
  descendant measurement, threshold actions, and real child-process fixtures.
- planned-code: **R5.6 Progress evidence collectors** - bounded process, filesystem,
  Git, focused-test, PR, and CI observations without rendering or model prose.
- planned-code: **R5.7 Receipt timer/store/rendering** - mandatory five-minute timer,
  event persistence, JSON contract, compact text, deduplication, and restart.
- planned-code: **R5.8 No-progress and stall policy** - two-interval policy, bounded
  diagnostic, cancellation/detach, interactive prompt detection, and return of
  user control.
- planned-code: **R5.9 Provider-native sub-agent containment** - aggregate descendant
  accounting, cancellation inheritance, and proof that children cannot own
  GitHub delivery actions.

Exit: real macOS Apple Silicon tests prove bounded load, mandatory reports, and
zero descendant processes after success, cancellation, timeout, and crash.

## Phase R6 - Direct delivery vertical slice <!-- lc:u=lc-ddb33246f698 -->

- planned-doc: **R6.1 `loopcoder run` operator contract** - command inputs, phases,
  prompts, outputs, side effects, stop/resume behavior, and human gates.
- planned-code: **R6.2 Intake and preflight** - resolve project/issue, focused policy,
  Work Graph root, provider snapshot, route decision, and worktree claim without
  launching a provider on failure.
- planned-code: **R6.3 One worker execution** - run one pinned attempt through Runtime
  and Supervisor, capture changes, and persist the outcome.
- planned-code: **R6.4 LoopCoder-owned Git/PR delivery** - validate diff, commit, push,
  create one PR, and make replay return the existing PR.
- planned-code: **R6.5 Zero-model CI/approval watcher** - event/cursor-based waiting,
  five-minute receipts, restart recovery, and no busy/model polling.
- planned-code: **R6.6 Host-neutral attach/report stream** - attached and explicit
  detached operation, JSONL cursors, compact pretty projection, relay
  acknowledgement, and terminal/Paseo/Codex/Claude Code conformance fixtures.
- planned-code: **R6.7 Independent verifier and gate** - different eligible provider,
  read-only evidence, remote checks, structured verdict, and explicit merge
  authority.
- planned-code: **R6.8 Direct-path end-to-end canary** - one docs fixture and one code
  fixture through issue -> PR -> CI -> verifier, including quota exhaustion and
  cancellation variants.

Exit: a normal user needs one command and sees bounded progress through a
complete real delivery path without understanding internal orchestration.

## Phase R7 - Explicit bounded workflow mode <!-- lc:u=lc-5b222f9715ef -->

- planned-doc: **R7.1 Workflow definition/materialization contract** - reusable steps,
  immutable materialized graph, owner approval, bounds, roles, dependencies,
  and final aggregation.
- planned-code: **R7.2 Workflow compiler** - validate and materialize a small explicit
  plan into Work Graph rows with stable child identity.
- planned-code: **R7.3 Ready-node scheduler and admission** - deterministic dependency
  gating, one active top-level worker by default, and shared resource admission.
- planned-code: **R7.4 Workflow cancellation and restart** - parent/child cancellation,
  terminal replay, orphan classification, and no duplicate child launch.
- planned-code: **R7.5 Child delivery isolation** - one writer/worktree/branch ownership
  policy and an explicit aggregation strategy that cannot create duplicate or
  conflicting PRs.
- planned-code: **R7.6 Workflow observability** - parent/child progress and terminal
  roll-up from the same event stream without a second reporting system.

Exit: a three-node serial/parallel fixture survives restart and failure with no
duplicate execution, no hidden child, and bounded local resources.

## Phase R8 - Migration, deletion, and production qualification <!-- lc:u=lc-2eebb15e7398 -->

- planned-doc: **R8.1 Compatibility/deprecation guide** - old command mapping, old/new
  state isolation, one-release warning period, v1.0 removal list, and operator
  recovery steps.
- planned-code: **R8.2 v0.8 read-only importer/exporter** - import supported project and
  report history into new projections without mutating or deleting the source.
- planned-code: **R8.3 Compatibility command shims** - explicit old-engine selection,
  warning output, and prevention of mixed writers for one work item.
- planned-code: **R8.4 Provider/routing consolidation deletion** - remove superseded
  inventory, quota, availability, budget, usage, model, and route paths proven
  unused by the new direct path.
- planned-code: **R8.5 Progress/wait/report consolidation deletion** - remove superseded
  progress-host, relay, outbox, wait-state, and duplicate lifecycle paths.
- planned-code: **R8.6 State/nested/autonomous retirement** - retire state-branch leases
  and default nested/dispatch-wave/tick/trigger surfaces after compatibility and
  usage evidence.
- planned-doc: **R8.7 Production operations guide** - install, first run, multi-repo,
  private repo, provider setup, quota interpretation, resource limits, reports,
  stop/recovery, upgrade, and uninstall.
- planned-code: **R8.8 Release qualification** - macOS arm64 package/install/upgrade,
  zero-repo-pollution, private/public repo, second-computer rehydrate, four-
  provider adapter, process cleanup, and rollback canaries.

Exit: v0.9.0 ships only when the direct path is simpler to operate than v0.8.1,
all release gates pass on the integrated release SHA, and the owner has accepted
the deletion/deprecation report.

## Release acceptance gates <!-- lc:u=lc-68fd0fffcc19 -->
v0.9.0 is not releasable unless all of the following are demonstrated:

- One-command direct path from a real GitHub issue to one PR and a structured
  verification result.
- Progress appears at least every five minutes without asking the model to
  narrate status.
- Waiting 30 minutes for CI or quota reset makes zero provider/model calls.
- Default local execution never exceeds configured worker/test/process/CPU/RSS
  limits and leaves zero descendants after stop.
- Replay, crash, and two-process contention create no duplicate provider
  execution, commit, PR, verifier run, or terminal event.
- Four official provider adapters fail independently and report unknown/stale
  evidence honestly.
- Route explanations reproduce exactly from persisted inputs and respect
  explicit user pins, capability tiers, quota resets, and reserves.
- Registered projects write no LoopCoder runtime state into the customer repo.
- Same-name local folders, multiple repositories, and public/private repos stay
  isolated.
- A second Mac can continue a completed issue/PR from GitHub without copying a
  LoopCoder database.
- v0.8.1 data remains intact and exportable after failed and successful import.
- Documentation, examples, `doctor`, `--help`, README, changelog, release notes,
  installer, and packaged binary describe the same behavior.
- The integrated release commit, not only individual PR heads, passes remote
  verification, test, race, security, install, upgrade, and real-product
  canaries.

## Explicit non-goals for v0.9.0 <!-- lc:u=lc-4e2d92138b20 -->
- Reimplementing or embedding Dolt.
- Shipping a clone of Gas City, Beads, or Plexus under different names.
- General-purpose issue tracking, team chat, provider API gateway, credential
  vault, dashboard, Kubernetes fleet, Windows/Linux support, or mobile control.
- Multiple computers concurrently orchestrating one in-flight issue.
- Unlimited autonomous agents, silent model decomposition, or random provider
  exploration.
- Automatic production merge/release without the configured human gate.
- Maintaining every v0.8 internal API or database table indefinitely.
- Claiming authoritative quota where the provider exposes none.
- Optimizing breadth before the one-worker direct path is proven reliable.

## 0.6.1 — Customer-ready bridge release — shipped 2026-07-09 <!-- lc:u=lc-d90d80350dbf -->

Shipped. v0.6.1 was the customer-ready bridge for the public 0.6 line.
It packages the 0.6 capabilities for external consumers while keeping larger
SQLite/global-project-registry and native sub-agent orchestration work out of
scope for a later 0.7.0 line.

- docs: release truth across README, usage, stability policy, changelog,
  roadmap, and v0.6.1 release note.
- docs: customer quickstart in install/version/init/skill-install/doctor/report
  order, including the command side-effect table and `.loopcoder/` local-state
  boundary.
- shipped-code: local `.loopcoder/` protection through `.git/info/exclude` in
  `init --repo` and `skill install --repo`.
- shipped-code: first-run `init --repo/--gate`, defaulting new scaffolds to
  `adapters.gate: human-merge`.
- shipped-code: `doctor --format text|json`, local-state checks, reportquery
  readability, installed-skill and hook checks.
- shipped-code: `report --format json` compatibility array plus richer `records`
  entries with source, run id, and local path.

## 0.6.0 — Model & depth selection: discovery, validation, defaults (+ agy provider) <!-- lc:u=lc-b9f16bd616c4 -->

Implemented for the 0.6 line and released to customers through v0.6.1. Make models and their depth tiers discoverable, validated, and defaulted so operators
choose without guessing. Target models: **claude, gpt (codex), gemini** — gemini reached **via
the Antigravity (`agy`) provider**, since the direct gemini CLI is dead for personal accounts
(Google `IneligibleTier`). Depth is a **per-model list of valid tokens that may be empty** (not a
cross-provider scale), wired per provider: claude `--effort`, codex `-c model_reasoning_effort=`
(both **pass-through today — loopcoder does no effort validation yet**), agy = whatever
`agy models` lists per model (`Gemini 3.1 Pro`→[Low,High], `Opus 4.6`→[Thinking],
`GPT-OSS 120B`→[Medium]). The registry's per-model tiers are **loopcoder-curated** (not
provider-derived); the parse-time membership check is **net-new** (today effort is unvalidated
pass-through). All three target models have depth — gemini's comes from agy's tiers. Effort
against an empty list is a warning.

Selection is **per-role**: worker and verifier each choose their own model+depth (config;
per-role override already shipped in spec 0215) and both are validated against the registry. The
conductor is the host session driving loopcoder, not a dispatched subprocess — its model is the
operator's host choice, so loopcoder does not switch it.

Dispatch is **natural-language**: the operator tells the conductor "use gemini, deeper" and the
conductor translates it to `--provider/--model/--effort`. The registry is the conductor's
ground-truth for valid model+depth (translation targets real options) and validation rejects
invalid names; the reporter's recorded model+depth per work-ID is the operator's confirmation
that the translation matched intent — validation catches *invalid* picks, the reporter catches
*wrong-but-valid* ones.

- shipped-doc: spec — static model registry (per provider: models × depth tiers + defaults);
  `loopcoder models [--provider]`; parse-time validation (warn default, `--strict` rejects);
  provider defaults; agy provider + its OAuth-login prerequisite.
- shipped-code: `internal/models` **leaf package** (static registry + pure validation + defaults;
  imports no orchestration/config/agent — they import it) + provider default model/depth.
- shipped-code: `loopcoder models [--provider]` command — print models × depth × default per provider
  from the static registry. (dynamic `--refresh` / `agy models` reconcile deferred to a later
  version; the static registry alone delivers discovery/validation/defaults.)
- shipped-code: parse-time validation of worker/verifier `model`+`reasoning_effort` vs registry (warn
  by default; `.delivery.yml`/CLI `--strict` escalates to reject).
- shipped-code: `internal/agent/antigravity.go` agy runner (close stdin, `-p`, plain-text summary,
  self-reported model, vendor "Google Antigravity"); register `antigravity`; depth via
  `model`+`reasoning_effort` → `"<model> (<Depth>)"`; **MUST pin the worktree as agy's workspace
  via `--add-dir <worktree>` (verified fix) — agy otherwise ignores process CWD and writes to its
  own `~/.gemini/antigravity-cli/scratch` (silent wrong-directory writes, exit 0)**; `loopcoder
  doctor` checks agy OAuth login so a missing login fails clearly.
- shipped-code: docs/reference — `loopcoder models` usage, model/depth config, agy setup + login.

## 0.6.0 — reporter (attestation → reporter rename + light strengthening) <!-- lc:u=lc-b8ffb03c1172 -->

Implemented for the 0.6 line and released to customers through v0.6.1. Rename `attestation`→`reporter` **including the operator-visible
`[attestation]`→`[reporter]` token** (a rename nobody can see is pointless) — Go package, type,
emitted header, and all human prose. The hard part is the relay hard-gate: the token is emitted,
matched (`relay_guard.go`), and instructed for verbatim relay (SKILL.md, host-hook templates,
GEMINI.md/AGENTS.md); a naive swap risks relay lock-out / fail-open on upgrade-lag between
binary, skill manual, and host hooks. So it ships with a transition window, not a raw swap.
Sequence: land Unit A (incl. `antigravity.go`) before this rename sweep, so the sweep renames
the new agy file in one pass instead of colliding with it.

- shipped-doc: spec — rename map + **full consumer inventory** (grep: ~1068 refs / 60 files across
  emit + match + manual: cli.go, worker.go, audit/*, agent/* providers, claudehooks,
  cli/hook.go, cli/pretty.go, doctor, guardrails, loopreview, conductor hooks, `relay_guard.go`,
  SKILL.md, GEMINI.md, AGENTS.md, hooks/*); **freeze CHANGELOG + shipped `docs/specs/*` history,
  CanonicalJSON field names, and the `.attest` ledger extension** (invisible machinery — same
  rationale as freezing schema fields: changing them adds transition risk for zero operator-
  visible gain); invariant: `Validate()` keeps accepting agy self-reported model + absent tokens.
- shipped-code: emit `[reporter]`; rename `internal/attestation`→`internal/reporter`,
  `AttestationRecord`→`Report`, all Go identifiers + pretty wording + current `docs/reference/*`;
  sweep every emit + match + manual site in lockstep; update golden/inventory tests.
- shipped-code: **transition safety** — `relay_guard.go` (`ledgerHeaderRe`, `rolePattern`) **and
  `conductorhooks/attest.go` (`conductorHeaderRe`)** accept BOTH `[attestation]` and `[reporter]`
  for this release so upgrade-lag (binary vs propagated skill manual vs host-side hooks) can
  neither lock out nor fail open; drop `[attestation]` acceptance one version later. (relay
  hard-gate spec 0447 + the "blocking gate must not lock out" rule)
- shipped-code: strengthen — model+depth display (`Gemini 3.1 Pro (High)`); **work-ID = the worker's
  internal `RunID` (spec 0390; unique per dispatch, set as `LOOPCODER_RUN_ID`), surfaced in every
  report — note dispatch's `--run-id` flag is currently ignored (0.5.4 learning), so the ID is
  loopcoder-generated, not user-set**; add `issue`/`branch`/
  `worktree`/`round` context fields (dispatch/tick-filled, optional); **prefer loopcoder-observed
  ground-truth** (dispatched model+depth, process timing, parsed tokens) over agent self-report,
  marking `self-reported` only where unobservable (e.g. agy); pretty grouping
  (who·what·result·cost); must not re-tighten validation against agy. (needs: 0.6.0 models
  registry for depth display)
- shipped-code: `loopcoder report` — on-demand, read-only command to list/query recent per-work reports
  (work-ID, role, model+depth, start/end, tokens) from the persisted log; this is how the
  reporter subsystem "reports at any time".
- shipped-code: docs/reference — reporter concept + attestation→reporter prose in usage.md/worker.md
  (CHANGELOG + shipped specs frozen).

## 0.6.0 — Upgrade, migration & operational health (doctor) <!-- lc:u=lc-9fc4c1e04cdd -->

Implemented for the 0.6 line and released to customers through v0.6.1. 0.6.0 is the first BREAKING release (reporter rename), so it must ship a clean upgrade
from 0.5.x and a defined self-check. Everything we froze (`.attest` ledgers, CanonicalJSON field
names) migrates as a no-op; only renamed config keys and stale logs need real handling.

- shipped-doc: spec — upgrade/migration plan (old-version detection, config-key migration map, log
  retention policy) + `loopcoder doctor` definition (who runs it, what it checks, which roles).
- shipped-code: `loopcoder upgrade` 0.5.x→0.6.0 — detect old version; migrate any renamed config keys
  (from the reporter rename); keep frozen machinery intact; idempotent, no data loss; the relay
  dual-token window (Unit B) must stay valid across the version boundary.
- shipped-code: old-file handling — config-key migration; **bounded cleanup of stale logs** (run logs,
  worktree-liveness, relay state) under a retention policy; leave `.attest` ledgers + schema
  untouched.
- shipped-code: `loopcoder doctor` — **operator-run; default diagnoses only (read-only, safe anytime)**:
  git + provider CLIs present; **per-provider auth/reachability** (codex, claude, agy OAuth
  login); config validity (worker/verifier model+depth vs registry); reporter/relay wiring sane;
  version + upgrade status — reports each problem + its fix command, changing nothing. **`--fix`
  (explicit opt-in) performs the mutations** (upgrade, stale-log cleanup, config-key migration);
  doctor is the operational-health entry point but never changes state by default. (Doctor is
  not a role — it verifies readiness FOR worker/verifier dispatch and conductor/relay wiring.)
- code+docs: **0.6.0 CHANGELOG + Release Note + README** — detailed feature + how-to-use docs;
  and establish a standing per-release rule in `docs/reference/releasing.md`: every version bump
  rewrites all three (changelog / release note / README), as complete and detailed as possible.

## 0.5.3 — loopcoder audit (built-in security audit) — ✅ shipped v0.5.3 (2026-07-06) <!-- lc:u=lc-4de0421522dd -->

Shipped. `loopcoder audit` is a read-only, built-in security audit that institutionalizes
catching the class of issue the external audit surfaced — on demand and in CI. Two layers: a
deterministic SAST floor (govulncheck/staticcheck/gosec + native secret & file-permission
scans, CI-gateable) and an adversarial LLM security-review lens (read-only verifier path,
attested, needs-human on failure). Configurable via `.delivery.yml audit`; wired as a required
CI `audit` check that loopcoder runs against itself; reported by `loopcoder doctor`.

- Design/spec: [`docs/specs/0518-loopcoder-audit.md`](docs/specs/0518-loopcoder-audit.md)
- Release notes: [`CHANGELOG.md`](CHANGELOG.md) — `## [0.5.3]`
- Guide + example rubric: [`docs/reference/audit.md`](docs/reference/audit.md),
  [`docs/security/`](docs/security/)

Built by loopcoder itself under the self-hosting guard (spec + C1 command/SAST floor →
C2 LLM lens → C3 CI/doctor/docs, serial). Wiring the self-audit surfaced and fixed real
findings: the worker-layer prompt/recovery `0o600` gap (a 0.5.1 A1-scope miss) and a
`golang.org/x/sys` dependency vulnerability. The E1 ReadOnly boundary, H5 exit-code split,
self-hosting guard, 0.5.1 hardening, and 0.5.2 behavior-preservation are all preserved.

## 0.5.2 — Core refactor (behavior-preserving) — ✅ shipped v0.5.2 (2026-07-05) <!-- lc:u=lc-e8090e82a836 -->

Shipped. Behavior-preserving internal restructuring for readability, testability, and reduced
drift, with **zero observable behavior change** (proven by golden/inventory tests and
independent verifier path-tracing, gated by the full CI suite incl. `-race`/staticcheck/
govulncheck).

- Design/spec: [`docs/specs/0507-core-refactor.md`](docs/specs/0507-core-refactor.md)
- Release notes: [`CHANGELOG.md`](CHANGELOG.md) — `## [0.5.2]`

Delivered (B1–B4): `worker.Dispatch` decomposed into focused helpers behind the unchanged
entrypoint; orchestration state/render split (byte-identical tick/promote/dispatch-wave
output); MCP validation consolidated into one shared parse-time validator (unchanged
accept/reject set, byte-identical provider argv); defaults/limits centralized into a new
`internal/defaults` leaf package with no value tuning.

## 0.5.1 — Security & robustness hardening — ✅ shipped v0.5.1 (2026-07-05) <!-- lc:u=lc-839f686c0285 -->

Shipped. Fixes every verified finding from the external security audit. loopcoder is a local
single-operator dev CLI, so most were Low–Medium hardening rather than active-exploit fixes;
all are closed. No behavior change to the code profile.

- Design/spec: [`docs/specs/0484-security-robustness-hardening.md`](docs/specs/0484-security-robustness-hardening.md)
- Release notes: [`CHANGELOG.md`](CHANGELOG.md) — `## [0.5.1]`

Delivered (A1–A9): cosign-signed `SHA256SUMS` verified before checksum in install/upgrade;
`govulncheck` + `staticcheck` required CI checks + all Actions SHA-pinned + Dependabot; `0o600`
file modes and Gemini prompt via stdin (shared-host disclosure); statebranch path confinement;
additive no-shell `argv` command form for the evidence producer + custom liveness; honest
failure reporting (runJSON / CreateIssue-UpdateIssue / codex log); and bounded hook / runstatus
/ worktree-liveness I/O.

## 0.5.0 — Generalize loopcoder beyond code (domain profiles) — ✅ shipped v0.5.0 (2026-07-04) <!-- lc:u=lc-57919ccf2f3b -->

Shipped. loopcoder is now a general autonomous-delivery engine for any verifiable,
repo-based, AI-doable work (documents, content, data…), not only code — via purely-additive
**domain profiles**, with the core engine (tick / dispatch / loopreview / risk-gate /
promote / guardrails / watchdog / relay) unchanged. Code is the first of several domains.

Built by loopcoder itself under the self-hosting guard (human merge gate): the spec merged
first, then nine code slices in dependency order, each worker → PR → read-only verifier →
CI → human-merge.

- Design/spec: [`docs/specs/0459-domain-profiles.md`](docs/specs/0459-domain-profiles.md)
- Release notes: [`CHANGELOG.md`](CHANGELOG.md) — `## [0.5.0]`
- Domain-profile guide + worked example: [`docs/domains.md`](docs/domains.md),
  [`examples/docs-domain/`](examples/docs-domain/)

Plug points delivered: configurable skill sources; injectable verification rubric +
review-packet ordering; rendered-artifact evidence producer; append-only domain red-lines;
MCP servers (local stdio + external HTTP) on `agent.Invocation`; and domain-configurable
partial-work / liveness (the 0.4.2 H1/H2 fold-ins). An absent `domain` section behaves
exactly like the 0.4.x code profile; the ReadOnly boundary (spec 0161 E1) and the 0.4.2 H5
exit-code contract are preserved. Validation target proven via a self-contained `docs`
domain profile fixture (mirroring the corporate IR document-production shape); the private
SB_Glome repo is the real-world reference.
