# Product Evolution: v0.6 to v0.8

Status: historical engineering record
Audience: maintainers, conductors, worker authors, host integrators, and future
roadmap writers

This document records how LoopCoder's product model evolved from the v0.6
reporter transition through the v0.8 delivery, routing, quota, host, and agent
federation work. It captures durable decisions and rejected alternatives so a
future roadmap does not reopen settled questions without new evidence.

It is not a current-behavior reference. For the live implementation, read
[`reference/architecture.md`](reference/architecture.md),
[`reference/runtime-capabilities.md`](reference/runtime-capabilities.md), and
[`reference/usage.md`](reference/usage.md). Accepted specs remain authoritative
for the contracts they freeze.

## Executive Summary

The central product problem was never a lack of low-level delivery mechanics.
LoopCoder already had issue compilation, worker dispatch, verification, relay,
state, lease, and promotion operations. The problem was that users and host
agents had to think in those internal protocol terms.

The product direction that emerged was:

1. preserve the expert protocol surface;
2. make `DeliveryRun` the user- and host-facing unit of work;
3. move runtime state outside repositories;
4. expose explicit side effects, approvals, and machine-readable capability
   contracts;
5. treat providers, models, quota, and sub-agents as governed execution
   resources rather than implicit CLI behavior; and
6. make evidence, recovery, and uncertainty durable and honest.

This direction produced a stronger system, but v0.8 also demonstrated that
product correctness is not enough. A self-hosting delivery engine needs strict
operational budgets for local resources, retries, verification, and release
scope. See [`v0.8.0-retrospective.md`](v0.8.0-retrospective.md) and
[`self-hosting-playbook.md`](self-hosting-playbook.md).

## Phase 1: Reporter as a Human-Legible Evidence Layer

### Starting point

Before the v0.6 transition, LoopCoder's execution evidence was primarily
described with integrity-oriented terminology. That language was precise for
provenance, but it required users to understand an internal security concept
before they could answer ordinary questions such as:

- Who performed this work?
- Which provider and model were used?
- Did the worker or verifier finish?
- Where is the local evidence?
- Can the evidence be queried after interruption?

### Decision

The shared execution record became a reporter-oriented surface while the
legacy evidence format remained readable during a compatibility window.

The transition established several durable principles:

- Human terminology may improve without discarding provenance.
- New writers can use a clearer schema while readers accept the previous
  schema during a bounded migration window.
- Worker and verifier reports are local operational evidence by default.
- A report must be queryable by work identity, issue, role, provider, and run.
- Evidence should not be copied automatically into PR bodies, issue comments,
  commits, or other public surfaces.

The accepted reporter contract is recorded in
[`specs/0567-reporter.md`](specs/0567-reporter.md).

### What reporter did not solve

Reporter answered what happened. It did not yet answer the more important
product questions:

- What is the current delivery goal?
- Which phase is active?
- What is blocked?
- Which decision belongs to the user?
- What will the next action mutate?
- Is the action local, provider-billed, GitHub-writing, pre-production, or
  production?

This distinction led to the next product layer. Evidence remained necessary,
but evidence alone was not a delivery experience.

## Phase 2: Customer-Ready Bridge Instead of Silent Semantic Change

The v0.6.1 planning work separated customer readiness from the larger product
redesign.

### Customer-ready priorities

- Align release artifacts, changelog, README, usage reference, and stability
  policy.
- Make `doctor` the support entry point for installation, provider, version,
  state, reporter, and legacy compatibility diagnostics.
- Make local-state privacy explicit and protect legacy `.loopcoder/` paths from
  accidental Git tracking.
- Document command side effects.
- Keep compatibility aliases long enough for a real public transition.
- Ship installable artifacts so users do not need to compile from source.

### Compatibility decision

A patch release must not silently reinterpret an existing repository's
promotion policy. Safer defaults belong in new scaffolds; changing the runtime
meaning of an omitted legacy field requires a separately reviewed migration.

This principle remains important beyond promotion gates:

> Improve new-user defaults without silently changing the authority of an
> existing project.

## Phase 3: DeliveryRun as the Primary Product Object

The core product insight was to stop presenting the delivery protocol itself as
the main user mental model.

### Internal protocol versus external product

Expert mechanics include concepts such as ready sets, dispatch waves, relay,
leases, state branches, risk gates, and promotion. They are valid internal and
operator concepts, but a normal developer or host agent should begin with:

- Project
- Workspace
- DeliveryRun
- Goal
- Task
- Worker
- Verifier
- Report
- Blocker
- Decision
- Approval
- Next action

### DeliveryRun contract

`DeliveryRun` became the durable root for one guided delivery. It owns or
references:

- project and workspace identity;
- the requested goal;
- a fingerprinted plan and task dependency graph;
- worker and verifier attempts;
- decisions, approvals, and overrides;
- side-effect and policy authority;
- reports and progress receipts;
- blockers and recovery evidence; and
- terminal outcome.

The v0.8 contract is frozen in
[`specs/0801-delivery-run-contracts.md`](specs/0801-delivery-run-contracts.md).
It intentionally separates planning, approval, and execution. A plan can be
inspected without launching a provider, and approval is bound to the exact
input, plan, and policy fingerprints it authorizes.

### User-facing action model

The product direction grouped actions into three layers:

| Layer | Purpose | Typical behavior |
| --- | --- | --- |
| Guided | Default human and host path | Explain state, risk, next action, and required approval. |
| Headless | CI and automation | JSON-first, non-interactive, fail closed when approval is absent. |
| Expert | Maintainer and debugging surface | Expose low-level compile, dispatch, relay, lease, state, recovery, review, and promotion mechanics. |

The expert surface should remain available. The product improvement is a better
front door, not the removal of operational controls.

### Guided first-run journey

The early product proposal described a guided journey in terms of `setup`,
project inspection, run inspection, planning, approval, continuation, and
capability discovery. The exact command names evolved, but the journey remains
useful:

1. resolve the repository and public remote identity;
2. register the project and current workspace outside the repository;
3. explain tracked configuration versus machine-local runtime state;
4. inspect Git, GitHub, provider executables, credential-blind readiness, and
   model/capability evidence;
5. recommend Worker and Verifier defaults without launching either;
6. create or update shared configuration only with clear side effects;
7. protect any legacy repository-local state from accidental tracking;
8. render a side-effect-free plan and the first safe next action; and
9. require a fingerprint-bound decision before consequential execution.

The current v0.8 surfaces distribute that journey across `init`, `doctor`,
`projects register`, `projects list`, `status`, `report`, `delivery plan`,
`delivery decide`, `delivery continue`, provider inventory, and the runtime
capability contract. Future UX work may compose those mechanics into a smaller
guided surface, but it must preserve their authority and failure semantics.

A first run should not jump directly to an autonomous tick. It should first
show what LoopCoder understood, what it proposes, what evidence is missing, and
which next action needs approval.

### Command impact is part of the product

The guided surface should disclose impact before invocation:

| Question | Required answer |
| --- | --- |
| Will this only read local state? | Side-effect class and affected local roots. |
| Will this contact GitHub or a provider? | Network target and reason. |
| Will this spend paid provider capacity? | Provider, role, reservation, and confidence. |
| Will this write Git or GitHub? | Branch, issue, PR, or artifact target. |
| Will this change pre-production? | Exact branch and required gate. |
| Will this change production? | Exact candidate, protection, and human approval. |

Host agents should consume the machine-readable contract rather than infer
impact from a familiar-looking command name.

## Phase 4: Machine-Local Project and Workspace State

### Problem

Runtime attempts, logs, recovery records, and reports are local development
state. Storing them under a repository creates accidental disclosure risk even
when `.gitignore` exists. Ignore rules are convenience, not a privacy boundary:
files can be force-added, archived, copied, or inspected by unrelated tooling.

### Decision

Registered projects store runtime state beneath `$LOOPCODER_HOME` and use a
stable project identity derived from repository host and ownership, rather than
only a display name. Separate workspaces distinguish clones and worktrees of the
same repository.

Repository-tracked content remains limited to shared project configuration,
roadmap, source, tests, and documentation. Legacy repository-local state is a
read/migration input, not the preferred write target.

The global layout and project identity contract is frozen in
[`specs/0639-global-data-layout-project-identity.md`](specs/0639-global-data-layout-project-identity.md).

### Structured files and SQLite

The early design discussion considered JSON/JSONL as the durable evidence
source with rebuildable SQLite indexes. The implemented v0.8 system uses
normalized SQLite records for identities, lifecycle transitions, authority,
queries, and atomic policy checks while continuing to use bounded JSON payloads
and append-only evidence where appropriate.

The durable lesson is not that one format must own everything. It is:

- use normalized tables for identity, uniqueness, joins, transitions, claims,
  budgets, and atomic checks;
- use explicit, versioned JSON contracts for host-facing and fixture-facing
  data;
- use append-only event/evidence records where history must not be rewritten;
- never make an opaque cache the only copy of irreplaceable evidence; and
- keep every format versioned, migratable, and honest about unknown data.

## Phase 5: Host-Neutral Invocation and Side-Effect Contracts

LoopCoder can be invoked from different interactive hosts. A host may preserve
stdout and stderr differently, offer different cancellation semantics, or have
no documented callback and wake-up API.

### Host contract

A supported host integration must declare or preserve:

- subprocess invocation and working-directory semantics;
- stdout JSON integrity;
- stderr visibility for provider and human reports;
- cancellation and timeout behavior;
- progress transport and acknowledgment evidence;
- local-only evidence boundaries; and
- side-effect and approval classes.

Core behavior must not depend on private host APIs. Host capabilities are
negotiated explicitly and unknown evidence does not become claimed support.

The living contract is
[`reference/runtime-capabilities.md`](reference/runtime-capabilities.md).

### Side-effect classes

Hosts must not infer impact from command names. A machine-readable surface
should distinguish at least:

- local read;
- remote read;
- local write;
- provider spend;
- Git remote write;
- GitHub write;
- pre-production write; and
- production write.

Approval must be evaluated against the real side effect, not against vague chat
language or the apparent simplicity of a command.

### Host and provider are separate dimensions

The host running LoopCoder is not the provider performing Worker or Verifier
work. A Paseo-style host can invoke a Codex worker and a Claude verifier; a
Codex host can invoke another supported provider. Host progress delivery and
provider/model routing therefore remain separate contracts.

## Phase 6: Provider, Account, and Model Inventory

The next problem was dynamic local capability discovery. A user may have
multiple provider CLIs, profiles, model catalogs, and versions. Hard-coding a
small set of provider names into scheduler policy would not scale.

### Inventory decisions

The accepted provider inventory contract
([`specs/0802-provider-inventory.md`](specs/0802-provider-inventory.md))
requires:

- bounded executable probes;
- credential-blind auth-readiness observations;
- dynamic model catalog snapshots with provenance and freshness;
- explicit capability facts rather than model-name folklore;
- support for multiple installations and account profiles;
- a future-provider adapter contract that avoids scheduler-core edits; and
- typed `exact`, `estimated`, `unknown`, `unavailable`, and `stale` confidence.

An installed CLI does not prove login readiness. Login readiness does not prove
model authorization. A model name does not prove read-only mode, nested-agent
support, cancellation, MCP support, or token reporting. Each fact requires its
own evidence.

### Privacy boundary

Inventory may store redacted references and hashes of non-secret identifiers.
It must not read, copy, hash, parse, serialize, or publish credential material.
Raw absolute executable and credential paths remain machine-local diagnostics.

## Phase 7: Quota, Usage, Budgets, and Availability

The product goal was not merely to detect providers, but to use available paid
capacity intelligently without fabricating certainty.

### Evidence hierarchy

Quota and usage may come from different sources with different authority:

- official machine-readable provider output;
- supported local application RPC or adapter output;
- LoopCoder-observed report usage;
- conservative local estimates; or
- unknown/unavailable evidence.

The scheduler must preserve provenance, freshness, reset windows, and
confidence. It must not scrape private UI state or infer exact account limits
from incomplete evidence.

### Budget model

The accepted contract in
[`specs/0803-quota-usage-budget.md`](specs/0803-quota-usage-budget.md)
separates:

- external provider quota windows;
- LoopCoder-observed local usage;
- hierarchical run/task/agent budgets;
- atomic reserve, commit, and release accounting;
- availability and circuit-breaker evidence; and
- routing eligibility.

Using the provider with the most remaining capacity is only one policy input.
Risk, task requirements, model capability, reset timing, uncertainty, user
pins, and fallback policy can override raw headroom.

## Phase 8: Explainable Decomposition and Routing

Dynamic routing must answer two questions before choosing a provider or model:

1. What does this task require?
2. Which currently eligible candidate satisfies those requirements under
   policy and budget?

The phase-4 contract
([`specs/0804-decomposition-routing.md`](specs/0804-decomposition-routing.md))
introduced:

- deterministic task requirement classification;
- bounded dependency-aware graph proposals;
- hard eligibility filtering before ranking;
- capability-, availability-, quota-, and cost-aware scoring;
- stable, explainable routing decisions;
- policy profiles, pins, and overrides;
- bounded fallback and replanning; and
- independent verification and council decisions for high-risk work.

### Role envelopes

`Soul`, `Tera`, and `Luna` are provider-neutral role envelopes rather than
vendor model aliases. They describe the required depth, autonomy, verification,
and risk posture for work. A new model can participate when fresh capability
evidence shows it satisfies an envelope; core routing should not require a code
change for every new model name.

### Decision ownership

LoopCoder owns deterministic task decomposition, policy, budgets, approvals,
and final routing. A provider may propose decomposition or use internal
execution helpers, but it does not become the authority for global scope,
budget, permissions, merge, or production promotion.

## Phase 9: Bounded Agent Federation

Provider-native sub-agents can improve parallelism, but uncontrolled nesting
creates a second invisible scheduler inside the first.

### Red line

The human-to-host-to-LoopCoder-to-provider tree must remain one governed
DeliveryRun. A child agent never bypasses:

- task and plan fingerprints;
- scope and permission inheritance;
- one-writer ownership;
- worktree isolation;
- provider execution claims and fencing;
- hierarchical budgets;
- approvals and side-effect policy;
- cancellation and recovery; or
- final acceptance.

The accepted federation contract is
[`specs/0805-agent-federation.md`](specs/0805-agent-federation.md).

### Safe progression

The original product direction recommended staged capability:

1. read-only research children;
2. patch-proposal children whose output is applied by one owning worker;
3. isolated implementation children with one-writer worktrees; and
4. provider-native federation only when registration, scope, budget, claim,
   cancellation, and evidence contracts are proven.

Depth and fan-out must be bounded. Child agents cannot create GitHub issues,
merge PRs, or promote production on their own authority.

### Required observability

A host should see an agent tree, not unrelated terminal streams. Every child
must have a durable identity, parent, role, provider/model evidence, permission,
task, timestamps, usage when available, status, and artifact/result summary.

## Decisions Intentionally Preserved

The following constraints remain deliberate:

- Do not weaken safety gates to improve apparent smoothness.
- Do not remove expert commands when adding guided workflows.
- Do not publish machine-local report payloads automatically.
- Do not make provider authentication or paid quota a release-smoke
  dependency.
- Do not use private host APIs as a core requirement.
- Do not treat SQLite, JSON, events, or GitHub as interchangeable sources of
  truth; assign each a specific responsibility.
- Do not let unknown capability or quota evidence satisfy a hard requirement.
- Do not allow recursive sub-agent authority to grow monotonically.
- Do not pursue unattended autonomy before the user can inspect, pause,
  approve, cancel, and recover a run.

## Decision Index

| Topic | Primary record |
| --- | --- |
| Reporter and local evidence | [`specs/0567-reporter.md`](specs/0567-reporter.md) |
| Global project state | [`specs/0639-global-data-layout-project-identity.md`](specs/0639-global-data-layout-project-identity.md) |
| Nested execution claims | [`specs/0646-nested-sub-agent-plan.md`](specs/0646-nested-sub-agent-plan.md) |
| DeliveryRun contracts | [`specs/0801-delivery-run-contracts.md`](specs/0801-delivery-run-contracts.md) |
| Provider inventory | [`specs/0802-provider-inventory.md`](specs/0802-provider-inventory.md) |
| Quota, usage, and budgets | [`specs/0803-quota-usage-budget.md`](specs/0803-quota-usage-budget.md) |
| Decomposition and routing | [`specs/0804-decomposition-routing.md`](specs/0804-decomposition-routing.md) |
| Agent federation | [`specs/0805-agent-federation.md`](specs/0805-agent-federation.md) |
| Host capability contract | [`reference/runtime-capabilities.md`](reference/runtime-capabilities.md) |
| Current system map | [`reference/architecture.md`](reference/architecture.md) |

## How To Use This Record

Before proposing a new roadmap item:

1. Identify whether the proposal changes a settled decision above.
2. Read the linked accepted spec and current living reference.
3. State the new evidence that justifies a different decision.
4. Prefer a superseding spec over silently changing old rationale.
5. Keep roadmap issues small enough to preserve the operational limits in
   [`self-hosting-playbook.md`](self-hosting-playbook.md).

Product history should reduce repeated debate. It must not freeze the product
against new evidence.
