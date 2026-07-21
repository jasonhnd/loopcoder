# v0.9.0 Issue Drafts: P3 Direct Run

Status: development-ready issue drafts; owner publication/assignment required

Publish each issue only when its named dependencies pass and the owner approves
that bounded unit. This lane deliberately reaches the first visible source-build
canary before the later twelve-minute/detach hardening gate. It builds one
explicit route only: one issue, one worker attempt, one provider/model/effort pin,
one worktree, one branch, one PR, one verifier, and a human merge gate. It does
not add automatic routing, decomposition, waves, or self-development.

## V090-095: Minimal provider execution contract and reference adapter

**Metadata:** code, size M; depends on V090-003 and V090-012; labels `v0.9.0`,
`provider`, `runtime`, `direct-run`; exclusive in the pre-router execution port.

### Outcome and rationale

Resolve the dependency inversion in which the direct path needs a real provider
before the full provider inventory/router phase. Define the smallest immutable
execution contract and adapt one existing `internal/agent` runner without
building discovery, quota, scoring, fallback, or a second launcher.

### Scope and constraints

- Define adapter identity/version, normalized explicit model/effort/permission,
  immutable invocation request, process-start evidence, bounded outcome, usage
  evidence, capability declaration, and typed refusal/failure.
- Reuse the runtime facade and existing agent/supervised execution mechanics.
- Provide a fake adapter plus one reference wrapper selected from existing
  supported adapters based on current code conformance, not owner development
  model preference.
- Require exact requested-to-actual route digest and reject unsupported aliases,
  effort, permission, or native delegation before launch.
- Keep credentials in provider-owned auth and keep installation/catalog/quota/
  routing policy outside this interface.

### Acceptance criteria

1. Fake and reference adapters accept the same immutable execution request and
   return the same normalized process/outcome evidence envelope.
2. Actual provider/model/effort/permission/delegation values exactly match the
   accepted explicit request; mismatch launches nothing or becomes a blocking
   integrity failure with process evidence.
3. Timeout, auth refusal, unsupported capability, rate limit, malformed output,
   cancellation, and process failure are distinct typed results.
4. Adapter execution uses the one runtime/process supervisor and cannot write
   project lifecycle, choose a route, create Git/GitHub state, or read credentials
   into LoopCoder persistence.
5. The PR inventories each existing agent runner as reference-ready, later P4
   migration, or compatibility-only without duplicating invocation code.

### Verification and boundaries

Fake executable conformance, reference-adapter command construction/parsing,
route-mismatch, redaction, cancellation, output bound, and remote race tests. No
real quota, automatic provider choice, fallback, provider installation mutation,
or self-bootstrap. Done when direct run no longer depends on an undefined future
Provider SPI or a hidden v0.8 call path.

---

## V090-025: `loopcoder run` command contract and CLI shell

**Metadata:** code/docs, size S; depends on V090-010, V090-012, V090-022,
V090-023, V090-088, V090-089, and V090-095; labels `v0.9.0`, `cli`, `direct-run`;
exclusive in the primary command surface.

### Outcome and rationale

Create the small primary command users should actually run. It validates input,
opens the project authority, records a requested run, and delegates to explicit
ports without importing the v0.8 autonomous command inventory.

### Scope and constraints

- Define flags for repo, issue, provider, model, effort, permission, base branch,
  required/optional UI clients, explicit detach policy, dry-run, and human/JSON/
  JSONL behavior.
- Return stable exit categories and one run/job identity before long work begins.
- Make omitted automatic-route inputs a clear unsupported error until P4.
- Keep command wiring thin; business transitions belong to services/events.
- Root help leads with `run`, `status`, `events`, `cancel`, `doctor`, and
  `providers`; legacy commands are explicitly compatibility-only.

### Acceptance criteria

1. A valid explicit request creates exactly one requested run record and prints
   its stable identity before execution.
2. Invalid/missing route, repo, issue, base, permission, or UI options fail
   before provider, worktree, or GitHub side effects.
3. `--dry-run` reports normalized inputs and pending preflight without mutation.
4. Human, JSON, and JSONL modes are projections of the same result/event schema.
5. CLI help documents the direct path and does not advertise self-bootstrap or
   automatic routing as available.

### Verification, rollback, and boundaries

CLI parser/golden tests and fixture store tests; no real provider/network. Failed
validation appends no run. Reverting removes only the new shell. No intake,
worktree, worker, or delivery implementation. Done when later issues attach to a
stable command contract without enlarging `internal/cli` ownership.

---

## V090-026: First-run doctor and project preflight

**Metadata:** code, size S; depends on V090-006, V090-007, V090-010, V090-085,
and V090-095; labels `v0.9.0`, `doctor`, `preflight`; exclusive in direct-run
readiness.

### Outcome and rationale

Give a new user a deterministic readiness decision before any model is invoked:
supported platform, global home, project identity/store, Git/GitHub, explicit
provider executable/auth capability, UI capability/handshake, and resource budget.

### Scope and constraints

- Implement read-only probes with stable `pass`, `warn`, `fail`, `unknown` and
  remediation codes.
- Separate product prerequisites from optional capabilities.
- Auto-create only validated global/project layout and registration when the
  user invokes first run; never mutate provider credentials or repo files.
- Bound every external probe by timeout/output and scrub inherited environment.
- Doctor reports evidence provenance and time; no guessed quota or model success.

### Acceptance criteria

1. Unsupported platform, unsafe home, invalid repo, unavailable Git, missing
   explicit provider, and insufficient resource budget fail before launch.
2. First valid run can create/register global project state outside the repo
   idempotently and report exactly what changed.
3. Optional UI rendering/quota/catalog gaps warn or degrade without becoming
   false provider-auth failures.
4. Every probe is bounded and redacted; credentials and raw auth files are never
   stored or printed.
5. Repeating doctor with unchanged inputs yields the same normalized decision and
   performs no write beyond refreshed observation metadata.

### Verification, rollback, and boundaries

Fixture matrix for missing/unsafe/partial/healthy prerequisites; network and
provider commands are fakes. Partial auto-setup removes only empty newly created
paths. No route choice, issue intake, or migration. Done when `run` cannot cross
the preflight gate without an accepted evidence snapshot.

---

## V090-027: GitHub issue intake and immutable policy snapshot

**Metadata:** code, size M; depends on V090-025 and V090-026; labels `v0.9.0`,
`github`, `intake`, `policy`; exclusive in issue-to-work request intake.

### Outcome and rationale

Fetch one GitHub issue, validate repository/base/authorization, normalize the
requested work, and persist the exact source and policy snapshot used by the run.
Later edits remain visible but cannot silently change active work.

### Scope and constraints

- Resolve repository identity and fetch issue number, state, title, body digest,
  labels, assignees, updated time, URL, and authorization evidence.
- Freeze sanitized issue content and effective local/repository policy digest.
- Reject closed, transferred, wrong-repo, unauthorized, or ambiguous issue input.
- Record later source drift as an event requiring explicit continue/restart choice.
- Bound API pagination, redirects, retries, output, and rate-limit behavior.

### Acceptance criteria

1. Intake persists one immutable work-request snapshot tied to normalized project,
   issue node identity, source revision, and policy digest.
2. Retry of the same source is idempotent; changed issue/policy content never
   overwrites the active snapshot silently.
3. Wrong repository, closed/invalid issue, insufficient authorization, and rate
   limit return typed pre-launch results with no provider/worktree side effects.
4. Secrets and private issue content remain project-scoped and are redacted from
   machine/global logs and default status output.
5. GitHub fixture tests cover edit, transfer, reopen, pagination, timeout, and
   duplicate intake deterministically.

### Verification, rollback, and boundaries

Fake GitHub contract tests and remote race. Network failure leaves a requested
run retryable at intake. No comments, labels, branch, worker, or PR creation. Done
when downstream behavior uses only the frozen request plus explicit drift action.

---

## V090-028: Immutable explicit provider, model, and effort pin

**Metadata:** code, size S; depends on V090-027 and V090-095; labels `v0.9.0`,
`routing`, `user-control`; exclusive in explicit route authority.

### Outcome and rationale

Persist the user's exact provider, model, effort, permission, and native-subagent
choice before launch, and prove the actual invocation equals that pin. Prior
development silently substituted GPT for an owner-requested Grok worker; v0.9
must make that structurally impossible.

### Scope and constraints

- Define requested, normalized, resolved, and actual route fields with provenance.
- Validate against the explicitly named provider adapter without auto-fallback.
- Include permission and whether provider-native sub-agents are allowed.
- Any route change requires a new successor attempt and fresh owner approval.
- Store no credential and do not implement automatic choice.

### Acceptance criteria

1. A direct run cannot launch until provider, canonical model, effort, permission,
   and sub-agent policy are persisted and acknowledged.
2. The runtime launch request and terminal record include the same route digest;
   mismatch fails before execution or becomes a blocking integrity defect.
3. Missing/unavailable explicit route fails closed and never selects another
   provider, model, or effort.
4. Route change after intake creates a new approved attempt; it cannot mutate the
   active attempt.
5. Status and PR evidence show requested and actual route without exposing auth.

### Verification, rollback, and boundaries

Adapter fixtures for exact, alias, unavailable, mismatched, and changed pins. No
real provider calls. No quota scoring, capability tiering, or fallback. Done when
V090-030 can accept only a verified immutable route snapshot.

---

## V090-029: Idempotent worktree and branch claim

**Metadata:** code, size M; depends on V090-006 and V090-027; labels `v0.9.0`,
`git`, `worktree`, `idempotency`; exclusive in Git workspace ownership.

### Outcome and rationale

Create or recover one isolated worktree and one branch for an accepted run,
without clobbering user changes, reusing another attempt's path, or confusing a
timed-out Git operation with failure.

### Scope and constraints

- Define deterministic branch intent, unique worktree allocation under project
  runtime storage, claim generation, base SHA, and ownership evidence.
- Use scrubbed Git environment and bounded commands.
- Detect existing branch/worktree/PR relationships and return reuse/conflict.
- Never clean, reset, delete, force-push, or checkout the user's primary worktree.
- Persist side-effect receipt after verifying actual Git state.

### Acceptance criteria

1. First claim creates one worktree/branch at the frozen base SHA outside the
   customer checkout; identical retry reuses it without a second branch.
2. Conflicting owner, dirty state, moved/deleted worktree, changed base, or
   existing unrelated branch fails typed and preserves all files.
3. Command timeout triggers state inspection before retry; an already-completed
   side effect is adopted rather than repeated.
4. Inherited `GIT_DIR`, `GIT_WORK_TREE`, index/object/common-dir, hooks, and pager
   variables cannot redirect the operation.
5. Cleanup removes only an exactly owned clean worktree after terminal policy and
   never deletes a branch/commit needed for recovery.

### Verification, rollback, and boundaries

Disposable bare/working repos, dirty/conflict/timeout/moved fixtures, and remote
race. No GitHub push/PR or provider. A failed claim remains inspectable and does
not clean unknown paths. Done when one durable claim owns each worker workspace.

---

## V090-030: Worker attempt lifecycle on the direct path

**Metadata:** code, size M; depends on V090-012, V090-017, V090-019, V090-028,
V090-029, and V090-091; labels `v0.9.0`, `worker`, `runtime`, `direct-run`; exclusive in worker
attempt state.

### Outcome and rationale

Launch the explicitly pinned provider once in the claimed worktree, supervise it,
persist truthful events, and terminalize only after process join. This is the
smallest real worker path and must not import v0.8 autonomous orchestration.

### Scope and constraints

- Define direct attempt states from requested/admitted/launching/running/stopping
  to process-terminal and cleanup-terminal.
- Persist and render the required start report through the frozen UI policy
  before invoking the provider adapter.
- Build a bounded provider request from frozen issue/policy/route/worktree data.
- Claim machine resources before launch and release through V090-017 only.
- Include an idempotency key in supported provider requests; treat provider
  receipt as evidence, never fabricate one.
- Do not commit, push, create PR, verify, merge, retry, or choose another route.

### Acceptance criteria

1. Exactly one provider launch occurs for one accepted attempt generation, even
   under concurrent/restarted start requests, and only after a required client
   proves the matching start report `rendered`.
2. Requested and actual route/worktree/base digests match persisted snapshots
   before launch.
3. Start, runtime evidence, five-minute, stop, exit, flush, join, and terminal
   transitions use the one event path and status projection.
4. Provider exit alone is not completion; terminal cleanup requires output flush,
   child join, and reservation release.
5. Cancellation, launch failure, nonzero exit, UI disconnect, and process escape
   produce typed recoverable/failed/attention states without duplicate launch.

### Verification, rollback, and boundaries

Fake provider modes and concurrent/restart barriers; no real provider. Failed
attempt retains logs/events/worktree for policy-based recovery. No Git delivery,
verification, automatic retry, or sub-agent materialization. Done when a fixture
worker safely reaches cleanup-terminal once.

---

## V090-031: Focused local verification plan

**Metadata:** code, size S; depends on V090-030; labels `v0.9.0`, `verification`,
`resources`; exclusive in local check selection/execution.

### Outcome and rationale

Select and run a small deterministic local verification set appropriate to the
changed files, preserving host responsiveness and leaving broad gates to remote
CI. Avoid the v0.8 pattern of running full test/race repeatedly on the Mac.

### Scope and constraints

- Define policy inputs, changed-file classification, allowed commands, time/RSS/
  process/output budgets, and result evidence.
- Support formatting, diff checks, generated consistency, package build, and
  focused tests; deny full repo/race/security/release commands locally by default.
- Execute through the same runtime/resource admission and event path.
- Persist command digest, scope, exit, duration, resource use, and bounded output.
- No model may choose or reinterpret verification results.

### Acceptance criteria

1. A deterministic policy maps fixture changes to a bounded explicit command
   plan and explains included/excluded checks.
2. Default plans cannot invoke `go test ./...`, full race, provider probes,
   packaging, signing, or release smoke locally.
3. Each command has soft/hard deadline, output/process/resource limit, and is
   stopped/joined through the runtime lifecycle.
4. Failure or timeout blocks delivery but preserves worker output and resumable
   verification evidence; completed worker work is not rerun.
5. Documentation and small Go fixtures finish within the ordinary local budget.

### Verification, rollback, and boundaries

Policy golden tests plus pass/fail/hang/output-flood commands. No network/provider.
No remote CI watcher or verifier. Done when delivery consumes a stable verification
receipt rather than raw shell success.

---

## V090-032: Idempotent local commit stage

**Metadata:** code, size S; depends on V090-030 and V090-031; labels `v0.9.0`,
`delivery`, `git`; exclusive in local commit creation.

### Outcome and rationale

Turn one stable verified worktree into one inspectable commit with an immutable
intent and read-back receipt. Commit creation is separate from customer hooks,
remote push, and PR creation so failure at one stage never repeats the worker.

### Scope and constraints

- Freeze selected paths, parent/base, tree digest, message digest, author policy,
  verification receipt, route digest, and idempotency key.
- Require stable worker terminal and accepted local verification before commit.
- Stage only the owned mutation scope and reject unrelated index/worktree drift.
- Sanitize commit content and preserve explicit issue linkage by policy.
- Bound Git commands and inspect HEAD/index/tree after timeout or cancellation.

### Acceptance criteria

1. Identical retry produces or adopts exactly one matching commit and never
   creates an empty or duplicate commit after an ambiguous timeout.
2. Worktree/base/route/verification or owned-scope drift blocks commit before a
   new side effect and preserves all user/unowned state.
3. The committed tree contains only accepted owned-scope changes and does not use
   broad staging as a substitute for ownership.
4. Receipt records parent, commit, tree, message/policy, route, and verification
   digests without private source or credentials.
5. Success is persisted only after Git read-back proves HEAD/tree/parent match
   the immutable commit intent.

### Verification, rollback, and boundaries

Disposable Git scenarios for owned/unowned changes, dirty index, timeout before/
after effect, duplicate retry, cancellation, and read-back. No push, GitHub,
hooks, PR, merge, provider replay, or branch deletion. Done when V090-097 can
consume one stable commit receipt.

---

## V090-096: Customer Git-hook policy and bounded hook reconciliation

**Metadata:** code/docs, size S; depends on V090-029 and V090-031; labels
`v0.9.0`, `git`, `hooks`, `policy`; exclusive in customer-hook behavior.

### Outcome and rationale

Define how LoopCoder treats customer commit and pre-push hooks. Hooks are
respected by default but never silently bypassed or allowed to escape resource,
time, environment, ownership, and side-effect reconciliation boundaries.

### Scope and constraints

- Freeze `respect`, `approved-bypass`, or `unsupported` hook policy in the run.
- Discover repository/global hook configuration without executing at preflight.
- Run applicable hooks with scrubbed environment and runtime supervision.
- Treat hook content/output as untrusted and detect mutation outside owned scope.
- Permit `--no-verify` only through explicit immutable authorization; recovery
  can never introduce it automatically.

### Acceptance criteria

1. Default commit/push respects configured hooks and records bounded pass/fail/
   timeout/mutation evidence without exposing hook-output secrets.
2. Hook timeout or ambiguous external side effect triggers Git/GitHub read-back
   before retry and never causes provider/worker replay.
3. Explicit bypass is authorized, visible in reports/PR evidence, and cannot be
   inferred from recovery, adapter defaults, or agent prose.
4. Out-of-scope mutation, Git config/ref mutation, background children, and
   environment escape block delivery and remain inspectable.
5. No hook process, descendant, temporary configuration, or descriptor survives
   terminal cleanup.

### Verification and boundaries

Disposable pass/fail/hang/output/mutation/background-child hooks plus config and
environment redirection tests. No real customer hooks, remote push, provider, or
blanket hook disabling. Done when commit and push share one policy result.

---

## V090-097: Idempotent remote branch push stage

**Metadata:** code, size S; depends on V090-032 and V090-096; labels `v0.9.0`,
`delivery`, `git`, `remote`; exclusive in remote branch publication.

### Outcome and rationale

Publish the accepted commit to one intended remote branch with read-back and a
stage receipt. A push timeout is remote-state reconciliation, not permission to
rerun the worker, create a commit, or force a branch.

### Scope and constraints

- Freeze remote identity, branch, expected old/new OID, commit/hook receipts, and
  idempotency key.
- Use scrubbed environment and bounded Git transport.
- Read remote refs after success, timeout, disconnect, or ambiguous exit.
- Adopt exact intended state and reject conflicting or moved refs.
- Never force-push, rewrite history, alter config/credentials, or delete refs.

### Acceptance criteria

1. First push publishes exactly the accepted commit and identical retry adopts
   the matching remote ref without another side effect.
2. Timeout before/after remote update reconciles to not-applied, applied,
   conflict, or unknown using read-back evidence.
3. Wrong remote, conflicting branch, non-fast-forward, auth/rate-limit, and hook
   failure preserve state and never force or rerun worker.
4. Success persists only after remote OID matches intent and records no credential
   material.
5. Cancellation joins Git/hook descendants and leaves a resumable stage with no
   unknown local process.

### Verification and boundaries

Disposable bare remotes, timeout barriers, conflicting refs, auth/rate-limit
fakes, hook results, cancellation, and remote race. No PR, merge, branch deletion,
provider, or unreconciled automatic retry.

---

## V090-098: Idempotent pull-request creation and reconciliation

**Metadata:** code, size S; depends on V090-027 and V090-097; labels `v0.9.0`,
`delivery`, `github`, `pull-request`; exclusive in PR creation/update.

### Outcome and rationale

Turn one verified remote branch into one pull request and immutable receipt. PR
creation is isolated from push so a network timeout cannot create duplicates or
cause local/provider work to repeat.

### Scope and constraints

- Freeze repository identity, base/head refs and OIDs, source issue, sanitized
  title/body digests, route/verification/hook summary, and idempotency key.
- Read matching PR state before and after ambiguous create/update.
- Create once or adopt only an exact compatible existing PR.
- Reject wrong repo/base/head, changed head, conflicting PR, permission, rate
  limit, and body mismatch.
- Never merge, auto-merge, change protection, close unrelated PRs, or publish raw
  logs/prompts/private source.

### Acceptance criteria

1. First create yields one PR and identical retry adopts it after exact repository,
   base, head, source, and digest read-back.
2. Timeout before/after creation converges without duplicate PR or provider,
   commit, or push replay.
3. PR evidence names source issue, tested base/head, requested/actual route,
   verification, hook policy, and redacted run identity.
4. Changed head, conflict, wrong repository, permission/rate-limit, or source
   drift returns typed resumable/attention state without unsafe mutation.
5. Success persists only after GitHub read-back matches the intended receipt.

### Verification and boundaries

Fake GitHub create/read/update timeout/conflict/rate-limit/permission scenarios
and remote race. No CI wait, verifier, merge, auto-merge, provider replay, or
branch cleanup. Done when V090-033 watches one stable PR receipt.

---

## V090-033: Zero-model CI and approval watcher

**Metadata:** code, size M; depends on V090-021 and V090-098; labels `v0.9.0`,
`github`, `ci`, `wait`; exclusive in remote wait behavior.

### Outcome and rationale

Watch required PR checks and approvals locally without keeping a coding model
alive or repeatedly asking it for status. Waiting is a deterministic state
machine driven by GitHub evidence, timers, and webhooks/polls.

### Scope and constraints

- Discover required checks from repository policy and snapshot the requirement.
- Poll or accept notifications with bounded adaptive backoff, rate-limit/reset
  awareness, restart checkpoint, and event deduplication.
- Classify pending, success, failure, cancelled, skipped, missing-required,
  approval-needed, changed-head, rate-limited, and unavailable.
- Feed timed reports/status from local events; zero provider runner dependency.
- Optional checks, including Greptile, remain evidence but not hard requirements.

### Acceptance criteria

1. A 30-minute pending fixture makes zero provider/model calls, emits due reports,
   and resumes after restart without losing state.
2. Required-check success/failure/missing and approval changes wake exactly once
   per semantic transition; duplicate remote responses do not flood events.
3. PR head or requirement-policy change invalidates prior readiness and requires
   fresh evidence for the new snapshot.
4. Rate limit/unavailability uses persisted bounded backoff and never busy-polls.
5. Optional review-bot absence cannot block readiness unless repository policy
   explicitly marks that exact check required.

### Verification, rollback, and boundaries

Injected clock and fake GitHub transitions; structural no-provider dependency test
and remote race. Watch failure leaves PR pending/attention-required, never ready.
No verifier, merge, CI rerun, or model summary. Done when remote waits are cheap,
visible, restartable, and deterministic.

---

## V090-034: Independent verifier and human merge gate

**Metadata:** code, size M; depends on V090-033 and V090-098; labels `v0.9.0`,
`verification`, `gate`; exclusive in verifier/gate state.

### Outcome and rationale

Start one separately selected read-only verifier only after worker delivery and
required checks are stable, persist its structured verdict, then stop at a human
merge gate by default.

### Scope and constraints

- Define verifier request with explicit provider/model/effort selected by owner,
  stable PR head/base, issue snapshot, checks, and permission `read-only`.
- Enforce no concurrent local worker/verifier and machine resource admission.
- Normalize pass, fail, needs-human, unavailable, invalid-output, and cancelled;
  preserve bounded findings and evidence.
- Re-run only on explicit approval when the PR head changes.
- Human gate records decision/actor/time/head; no automatic merge in v0.9 default.

### Acceptance criteria

1. Verifier cannot launch until worker is cleanup-terminal, PR head is stable, and
   required remote checks satisfy the configured pre-verifier policy.
2. Requested verifier route is immutable and actual route mismatch blocks the
   verdict exactly as for the worker.
3. Verifier has read-only repository/GitHub permission and cannot mutate worker
   branch, PR, issue, checks, or merge state.
4. Verdict is tied to exact PR head; any head change makes it stale.
5. Pass still stops at an explicit human merge gate unless a future separately
   approved release policy says otherwise.

### Verification, rollback, and boundaries

Fake verifier for all verdicts/head changes/concurrency denial. No real provider
in PR tests. Invalid verifier output is needs-human, never pass. No merge execution,
auto-fix, council, or fallback verifier. Done when the gate has one stable verdict
contract and owner control.

---

## V090-035: Delivery-only resume without worker replay

**Metadata:** code, size M; depends on V090-018, V090-033, V090-034, V090-097,
and V090-098; labels `v0.9.0`, `recovery`, `delivery`, `idempotency`; exclusive
in stage resume.

### Outcome and rationale

Resume from the first incomplete delivery/watch/verifier/gate stage after crash,
timeout, UI disconnect, or restart without launching the completed worker again.
This addresses the repeated v0.8 pattern of treating push timeout as worker failure.

### Scope and constraints

- Derive next action from immutable stage inputs, persisted events/receipts, and
  independently observed Git/GitHub/process state.
- Distinguish definitive failure, retryable failure, ambiguous completion,
  completed/adoptable, and needs-human.
- Reconcile worktree/commit/branch/PR/check/verifier/gate in order.
- Require new worker attempt only when worker output itself is absent/invalid and
  the owner explicitly approves it.
- Expose `resume` dry-run plan before mutation.

### Acceptance criteria

1. Crash or timeout after worker completion but before/during commit, push, PR,
   CI wait, verifier, or gate resumes that stage with worker launch count zero.
2. Ambiguous Git/GitHub operation is read back before retry; completed side effects
   are adopted idempotently.
3. Changed worktree, base, PR head, route, policy, or verification evidence blocks
   automatic resume and explains required owner action.
4. Repeating resume converges to the same next/terminal state without duplicate
   commit, push, PR, verifier, or terminal event.
5. Dry-run lists evidence, proposed actions, side effects, and reasons without
   mutation.

### Verification, rollback, and boundaries

Crash at every stage boundary using fixture barriers, then reopen and resume.
No real provider/GitHub. Recovery never deletes uncertain data. No automatic
reroute, worker replay, merge, or workflow recovery. Done when delivery recovery
is a first-class path rather than manual shell repair.

---

## V090-036: Documentation and Go-code visible direct-path canaries

**Metadata:** test/docs, size M; depends on V090-025, V090-026, V090-027,
V090-028, V090-029, V090-030, V090-031, V090-032, V090-033, V090-034,
V090-035, V090-088, V090-089, V090-090, V090-091, V090-092, V090-095,
V090-096, V090-097, and V090-098; labels `v0.9.0`, `acceptance`,
`direct-run`, `ui`; exclusive first source-build usability checkpoint.

### Outcome and rationale

Prove the complete explicit-route product path in two disposable consumer
repositories: one documentation-only change and one small Go-code change. This
is the first source-build usability checkpoint, not a release or self-bootstrap.

### Scope and constraints

- Run preflight, intake, pin, claim, worker, mandatory reports, focused
  verification, commit, hook policy, push, PR, zero-model CI wait, verifier, and
  human-gate fixture paths.
- Exercise success, worker failure, push timeout, host reconnect, cancellation,
  changed PR head, and delivery resume.
- Use deterministic fake provider/GitHub by default; a bounded opt-in integration
  canary may use an owner-selected real provider but is not a normal PR gate.
- Scan source repos/worktrees for runtime files and process table for survivors.
- Archive redacted evidence manifests tied to exact merge SHA.

### Acceptance criteria

1. Both consumer fixtures reach one stable PR and human gate with requested route
   equal to actual route, one worker launch, and mandatory reports rendered by
   terminal and the generic UI bridge.
2. CI/approval waiting uses zero model calls and verifier starts only after stable
   worker delivery/check evidence.
3. Push timeout and UI/core restart resume delivery without worker replay or duplicate
   commit/branch/PR/verifier.
4. Cancellation and every terminal variant leave zero owned child processes,
   release reservations, and retain replayable evidence.
5. Customer repositories contain no LoopCoder runtime state, and manifests/logs
   are bounded, redacted, and linked to the tested SHA.

### Verification, failure, and non-goals

Hosted Darwin acceptance scenarios with deterministic barriers. Any failure or
flake blocks P4; do not hide with retries. No auto-routing, decomposition,
sub-agent ownership, merge automation, release artifact, or LoopCoder self-use.
Done when owner reviews the manifests and records P3 acceptance.
