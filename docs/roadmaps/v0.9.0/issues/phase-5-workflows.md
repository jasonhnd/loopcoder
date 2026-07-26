# v0.9.0 Issue Drafts: P5 Bounded Workflows

Status: development-ready issue drafts; owner publication/assignment required

Publish only after V090-055 passes and owner approval. The one-node direct run
remains the default. Workflow mode is explicit and bounded: initial depth at most
3, fan-out at most 3, one active top-level worker by default, one writer per
worktree, and an operator-visible plan before children start. Provider-native
sub-agents are process descendants of one Attempt, not independent LoopCoder work.

## V090-056: Work Graph public contract and materialization boundary

**Metadata:** code/docs, size S; depends on V090-055; labels `v0.9.0`, `workflow`,
`architecture`; exclusive in workflow public contract.

### Outcome and rationale

Define a small LoopCoder-owned Work Graph contract and when a direct run may be
explicitly materialized into multiple WorkItems. Avoid importing v0.8 nested,
federation, compile, or autonomous-loop concepts wholesale.

### Scope and constraints

- Define WorkItem identity, intent, required/optional status, dependency kinds,
  owner, route requirement, output contract, integration order, and terminal state.
- Specify one-node graph equivalence with direct run and explicit workflow opt-in.
- Define limits, authorization, plan digest, mutation/replan boundary, and no
  hidden model-generated graph.
- Separate LoopCoder WorkItems from provider-native child sessions/processes.
- Record behavioral inspiration independently; no source/schema/prose port.

### Acceptance criteria

1. One-node materialization is behaviorally equivalent to the accepted direct
   path and introduces no extra provider call.
2. Multi-node workflow requires explicit definition/approval, stable graph digest,
   limits, and visible integration order before any child starts.
3. WorkItem, dependency, Attempt, provider-native child, and GitHub issue/PR
   ownership are unambiguous.
4. Graph mutation after execution starts requires a versioned replan and cannot
   rewrite completed history.
5. Contract rejects automatic ROADMAP compilation, synthetic epic expansion, and
   implicit self-bootstrap as workflow sources.

### Verification and boundaries

Contract examples/golden validation only. No schema/scheduler/decomposition or
provider call. Done when implementation issues can target stable terms and the
old nested/federation APIs are explicitly non-authoritative.

---

## V090-057: Work item and dependency schema

**Metadata:** code, size M; depends on V090-011 and V090-056; labels `v0.9.0`,
`workflow`, `storage`; exclusive in Work Graph persistence.

### Outcome and rationale

Add compact project-store tables for immutable graph versions, WorkItems, typed
dependencies, output/integration contracts, and lifecycle references without
recreating the v0.8 nested/federation schema family.

### Scope and constraints

- Persist graph/version/digest/limits/approval, WorkItem stable key/intent/state,
  dependencies, route requirement, output contract, and attempt references.
- Use normalized keys/checked states/foreign keys; lifecycle changes append events
  and update only guarded current records.
- Keep GitHub issue/PR identity as external evidence, not graph primary key.
- No local database synchronization or cross-project dependencies.
- Do not implement ready set, claims, scheduler, or decomposition.

### Acceptance criteria

1. Schema stores multiple immutable graph versions and links each WorkItem to one
   version/project with stable key and bounded payload.
2. Dependency kinds and required/optional semantics match V090-056 and reject
   missing/cross-project endpoints.
3. Completed/obsolete graph versions remain queryable and cannot be overwritten by
   replan.
4. Migration is transactional, idempotent, and keeps existing direct-run project
   data readable.
5. Schema inventory proves no duplicated provider credentials, process truth,
   report table, or v0.8 federation ownership.

### Verification and boundaries

Schema golden/migration/reopen/isolation tests and remote race. Rollback retains
old schema on failure. No workflow execution. Done when V090-058 can read a graph
through typed repositories only.

---

## V090-058: Graph validation and deterministic ready set

**Metadata:** code, size M; depends on V090-057; labels `v0.9.0`, `workflow`,
`dependencies`; exclusive in graph invariants/readiness.

### Outcome and rationale

Validate graph limits and calculate ready WorkItems deterministically. Borrow the
behavioral lesson from Beads: dependency readiness must be explicit, cycle-safe,
and reproducible before any process starts.

### Scope and constraints

- Validate stable keys, endpoint existence, self/multi-level cycles, depth,
  fan-out, node count, duplicate edges, required/optional semantics, and output
  integration order.
- Compute blocked/ready/terminal/ignored states from one immutable graph version
  plus accepted WorkItem terminal evidence.
- Return ordered reasons and stable ready order independent of map/SQL order.
- Invalid materialization writes no partial executable graph.
- No claim, launch, route choice, or model-based decomposition.

### Acceptance criteria

1. Self-loop, multi-node cycle, limit breach, missing endpoint, duplicate/conflicting
   edge, and invalid required/optional combinations fail before execution.
2. Ready set contains exactly nonterminal WorkItems whose required dependencies
   satisfy the contract, with deterministic stable order and reasons.
3. Optional failure/ignore semantics and required failure blocking match the
   accepted contract for every fixture.
4. Replaying the same graph/events after restart yields identical ready set/digest.
5. Invalid plan produces zero worker process, claim, partial active graph, branch,
   or PR side effect.

### Verification and boundaries

Table/property tests for DAGs/cycles/limits/order and remote race. No provider or
GitHub. Done when readiness is a pure auditable function available to claims.

---

## V090-059: Atomic work claim and guarded close

**Metadata:** code, size M; depends on V090-057 and V090-058; labels `v0.9.0`,
`workflow`, `claim`, `concurrency`; exclusive in WorkItem ownership transitions.

### Outcome and rationale

Guarantee that one eligible WorkItem execution is owned by one attempt generation
and that stale/losing owners cannot close or overwrite it. This applies Beads'
atomic claim principle in LoopCoder's own Go/SQLite model.

### Scope and constraints

- Atomically verify graph/version/readiness and create claim/attempt link/event in
  one immediate transaction.
- Return typed claimed, already-running, terminal-reused, blocked, conflict, and
  needs-human results.
- Fence renew/close by WorkItem, attempt, executor, and generation.
- Guard terminal close on persisted output/integration evidence and legal state.
- Ambiguous expired live execution cannot be automatically reclaimed.

### Acceptance criteria

1. Concurrent processes claiming the same WorkItem produce exactly one claimed
   owner and one execution authorization.
2. Claim, lifecycle transition, and event commit atomically or not at all.
3. Stale/losing generation cannot renew, close, emit terminal success, or advance
   dependent readiness.
4. Reclaim is allowed only when non-launch/nonexecution is proven; ambiguity is
   needs-human.
5. Close retry is idempotent and dependency readiness observes only accepted
   terminal state.

### Verification and boundaries

Two-store/two-process barrier tests, stale generation, crash windows, busy/retry,
and remote race. No scheduler or provider launch. Done when one WorkItem has one
durable writer at a time.

---

## V090-060: Explicit workflow definition and materialization

**Metadata:** code, size M; depends on V090-056 and V090-058; labels `v0.9.0`,
`workflow`, `cli`, `materialization`; exclusive in workflow input.

### Outcome and rationale

Accept a small explicit workflow definition, validate it, show the normalized
plan/digest, require approval, and atomically materialize it. No hidden prompt or
roadmap marker may generate work.

### Scope and constraints

- Define versioned YAML/JSON input or CLI request with WorkItems, dependencies,
  route requirements, output/integration contracts, and limits.
- Normalize/default deterministically and expose dry-run plan/digest.
- Require explicit approval actor/time/digest before materialization.
- Keep definitions user-authored or externally generated then reviewed; LoopCoder
  does not call a planner model in v0.9.
- Materialization creates one immutable graph version transactionally.

### Acceptance criteria

1. Valid definition normalizes to a stable digest and human/JSON plan; repeated
   dry-run is byte-stable and nonmutating.
2. Invalid graph/route/output/limit input fails with exact path/reason and writes
   no graph or process side effect.
3. Materialization requires approval tied to the exact digest and creates one
   graph version idempotently.
4. Changed definition after approval requires a new digest/approval/version.
5. No `ROADMAP.md` marker, GitHub epic label, model output, or hidden auto-split
   can create WorkItems implicitly.

### Verification and boundaries

Golden parser/normalization/approval/duplicate fixtures. No provider, scheduler,
or GitHub issue creation. Done when scheduler consumes only approved immutable
graphs.

---

## V090-061: Deterministic bounded-wave scheduling

**Metadata:** code, size M; depends on V090-059 and V090-060; labels `v0.9.0`,
`workflow`, `scheduler`; exclusive in bounded workflow scheduling.

### Outcome and rationale

Plan ready work serially and execute only within admitted bounds. Integration is
a separate V090-100 stage so scheduler completion cannot silently mutate the
parent branch or close WorkItems.

### Scope and constraints

- Produce wave plan from ready set, route/resource availability, WIP limit, and
  stable WorkItem order; persist plan before claims.
- Default active top-level workers to one; configured parallelism still requires
  disjoint worktrees/outputs and machine admission.
- Emit immutable completion candidates for later integration; do not integrate,
  merge, cherry-pick, or close WorkItems here.
- Replan only on versioned evidence change and preserve prior wave history.
- Waiting performs zero model calls and no busy poll.

### Acceptance criteria

1. Same graph/state/policy snapshot yields identical wave members, order, reasons,
   and digest.
2. Scheduler never exceeds graph, machine, provider, or worktree WIP limits and
   never gives two writers one worktree.
3. Out-of-order completion produces ordered immutable integration candidates but
   performs no parent-branch or terminal close mutation.
4. Restart resumes the persisted wave and claims without duplicate launch or
   silent membership change.
5. Blocked/no-ready state emits explanation and waits with zero provider calls.

### Verification and boundaries

Injected-clock scheduler fixtures with out-of-order finish, failure, restart, and
resource denial; remote race. No integration, provider-native child handling, or
automatic merge. Done when V090-100 receives stable candidates.

---

## V090-100: Ordered integration receipts and conflict boundary

**Metadata:** code, size M; depends on V090-032, V090-061, and V090-098; labels
`v0.9.0`, `workflow`, `integration`, `git`; exclusive in workflow integration.

### Outcome and rationale

Integrate accepted child outputs into one designated integration worktree in the
approved stable order, with read-back receipts and explicit conflict handling.
Execution success does not equal integration success, and a model is never kept
alive merely to wait for or conceal a conflict.

### Scope and constraints

- Freeze integration worktree/branch, ordered candidates, source commits/tree
  digests, expected parent, method, and idempotency key before mutation.
- Support only documented deterministic methods selected by policy.
- Apply one candidate at a time, read back result, and append a receipt before
  advancing or closing the WorkItem.
- On conflict, stop, preserve safe evidence/state, raise attention, and require
  an explicit resolution attempt or owner action.
- Never allow two integrators in one worktree, force/discard changes, or keep the
  original worker alive while integration waits.

### Acceptance criteria

1. Same candidates/policy/parent produce the same integration order and intent
   digest independent of worker finish order.
2. Identical retry adopts each exact applied result and never duplicates merge,
   cherry-pick, commit, or WorkItem close.
3. Conflict, changed parent/source, missing commit, dirty/unowned state, hook
   failure, or timeout stops at one known candidate with evidence.
4. Conflict creates durable attention and cannot be auto-resolved by changing
   model, strategy, or source without a new approved attempt.
5. WorkItem closes only after integration read-back, required verification, and
   receipt persistence; failed integration retains execution evidence.

### Verification and boundaries

Disposable Git DAGs for ordered/out-of-order results, conflicts, already-applied,
changed parent, hooks, timeout, crash/reopen, and remote race. No automatic model
conflict resolution, force operation, protected-branch mutation, or PR merge.

---

## V090-062: Provider-native sub-agent containment and resource aggregation

**Metadata:** code, size M; depends on V090-016, V090-017, V090-019, V090-060;
labels `v0.9.0`, `subagents`, `runtime`, `resources`; exclusive in native-child
containment.

### Outcome and rationale

Allow an owner-approved provider model to use its native sub-agents while keeping
all children inside one LoopCoder Attempt, route pin, process tree, resource
reservation, output policy, and terminal decision.

### Scope and constraints

- Persist requested native-child policy and observed child sessions/processes as
  evidence under the parent Attempt, not WorkItems.
- Aggregate child CPU/RSS/process/output/token evidence and enforce parent/global
  limits.
- Native children cannot own GitHub issues, branches, worktrees, PRs, verification,
  merge, route changes, or LoopCoder terminal truth.
- Parent cancellation/stop joins all observed descendants.
- Unsupported/unobservable native child behavior is disabled or attention-required.

### Acceptance criteria

1. Native children remain descendants of one immutable provider/model parent
   attempt and create no independent WorkItem/claim/GitHub ownership.
2. Their process/resource/output usage aggregates into parent and machine budgets;
   limits cannot be multiplied per child.
3. Parent status distinguishes native child activity from top-level progress and
   does not infer completion from child prose.
4. Parent cancel/failure/terminal cleanup joins all observable children; escape is
   attention-required and blocks clean terminal.
5. Disallowed native-subagent policy reaches the provider invocation exactly and
   any observed violation blocks the run.

### Verification and boundaries

Synthetic provider child-tree fixtures, no real model required; optional bounded
real smoke per provider later. No child routing or cross-provider WorkItems. Done
when native speedups cannot escape LoopCoder ownership/resource rules.

---

## V090-063: Cross-provider child-attempt isolation

**Metadata:** code, size M; depends on V090-037, V090-053, V090-059, V090-062;
labels `v0.9.0`, `workflow`, `routing`, `isolation`; exclusive in routed child
attempts.

### Outcome and rationale

Execute an explicitly materialized WorkItem on a different provider as its own
Attempt with its own route, worktree/output contract, claim, budget, and evidence,
while preserving graph and parent-workflow authority.

### Scope and constraints

- Create child attempt only from an atomic WorkItem claim and persisted route
  decision.
- Enforce separate worktree or read-only/no-code output; never share a writable
  checkout with another child.
- Isolate prompt/context, credentials, logs, and project-scoped outputs according
  to the WorkItem contract.
- Parent workflow aggregates status but cannot rewrite child terminal evidence.
- Cross-provider failure/fallback follows successor-attempt rules, not provider
  switching inside child.

### Acceptance criteria

1. Each child has independent claim generation, route digest, runtime reservation,
   process tree, output contract, and terminal evidence.
2. Two provider children never share writable worktree/index/branch or credentials.
3. Child receives only declared bounded inputs; private sibling prompt/output is
   not exposed by default.
4. Parent status deterministically derives aggregate progress and required failure
   semantics without declaring success before accepted child close.
5. Child route failure creates a visible finite successor/needs-human result and
   cannot mutate parent or sibling routes.

### Verification and boundaries

Two fake providers, isolation/redaction/concurrent claim/restart fixtures, remote
race. No autonomous decomposition or native-child conversion to WorkItem. Done
when multi-company work remains explicit and auditable.

---

## V090-064: Workflow cancellation, restart, and terminal compaction

**Metadata:** code, size M; depends on V090-010, V090-017, V090-059, V090-061,
V090-062, V090-063, and V090-100; labels `v0.9.0`, `workflow`, `recovery`,
`cancellation`; exclusive in workflow recovery/terminalization.

### Outcome and rationale

Make workflow stop/restart deterministic and keep long-lived status compact
without deleting audit truth. Parent terminal state must follow accepted child
state, not optimistic in-memory aggregation.

### Scope and constraints

- Define cancellation propagation order, stop/join, unstarted claim release,
  running/unknown child handling, and integration cancellation.
- Reconcile persisted wave/claims/process evidence on restart before new work.
- Derive parent required/optional terminal state only after all relevant child
  terminal writes succeed.
- Build compact terminal projection/snapshot with event range/digest; retain events
  under separate retention policy.
- Never emit parent success after a child close/persistence error.

### Acceptance criteria

1. Workflow cancellation stops and joins every owned running/native child,
   releases only proven unstarted claims, and records ambiguous ownership.
2. Restart adopts exact live children and resumes waves/integration without
   duplicate claim, launch, close, or route decision.
3. Parent terminal event occurs only after every required child terminal state and
   output/integration result is durably accepted.
4. Failed child terminal persistence suppresses parent terminal/status success.
5. Compact projection reproduces parent/child outcome and audit range digest after
   reopen without mutating/deleting source events.

### Verification and boundaries

Crash/cancel/failure injection at all child/parent windows, remote race. Unknown
ownership fails closed. No event deletion/retention enforcement or cross-Mac live
resume. Done when workflow lifecycle has no optimistic terminal gap.

---

## V090-065: Bounded-workflow end-to-end acceptance canary

**Metadata:** test/docs, size M; depends on V090-061, V090-062, V090-063,
V090-064, and V090-100; labels `v0.9.0`, `acceptance`, `workflow`; exclusive P5
checkpoint.

### Outcome and rationale

Prove small explicit graphs, deterministic waves, native-child containment,
cross-provider isolation, cancellation, restart, and ordered integration without
turning LoopCoder into an unbounded autonomous fleet.

### Scope and constraints

- Test one-node equivalence, a three-node dependency chain, fan-out/fan-in,
  optional failure, required failure, out-of-order completion, native children,
  cross-provider children, cancel, and restart.
- Enforce depth/fan-out/WIP/resource/worktree limits and archive exact-SHA manifest.
- Use fake providers/GitHub by default; optional real smoke is bounded and
  owner-selected.
- Scan for duplicate execution, shared writer, leaked process, repo-local state,
  and private sibling data.

### Acceptance criteria

1. One-node graph produces the same direct-path result/evidence without extra
   provider call or ownership layer.
2. Multi-node fixtures claim each WorkItem once, execute within limits, and
   integrate in deterministic declared order.
3. Native and cross-provider children remain within their distinct containment,
   credential, context, process, and resource boundaries.
4. Cancellation/restart leaves zero owned child, no duplicate launch/close, and
   correct parent terminal state only after durable children.
5. Invalid/oversized/cyclic graph creates zero process/branch/PR/partial executable
   graph and returns clear reasons.

### Verification, failure, and non-goals

Hosted Darwin deterministic scenarios and remote race. Any unexplained duplicate,
leak, flake, or order drift blocks P6. No self-bootstrap, roadmap compilation,
unbounded depth/fan-out, distributed DB, or public release. Done when owner accepts
the redacted manifest and records P5 completion.
