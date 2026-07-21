# v0.9.0 Issue Publication Contract

Status: **READY FOR ORDINARY DEVELOPMENT**; final GitHub publication and
assignment remain owner-controlled.

The seven phase files in this directory contain 109 proposed GitHub issue bodies.
They are planning artifacts only. Do not publish, assign, implement, compile,
dispatch, verify, merge, or close an issue until the owner explicitly approves
the phase or named issue.

## Ordinary Development Only

Every published issue must begin with this execution contract:

> This is ordinary development of LoopCoder, not LoopCoder self-development.
> Do not run `loopcoder compile`, `loopcoder dispatch`, `loopcoder tick`, or any
> LoopCoder orchestration command against the LoopCoder repository. Use a normal
> branch or isolated worktree, normal GitHub PR, and ordinary developer/agent
> tools. The owner selects any coding agent's provider, model, effort, permission,
> base branch, and native-subagent policy. Report the actual selection before
> editing and do not substitute it without renewed owner approval.

Issue publication itself does not authorize implementation. The owner or an
ordinary project manager separately assigns one dependency-ready issue.

## Mandatory Body Sections

The issue-specific text in each phase file is combined with this contract when
published. Each final GitHub issue must contain:

1. **Metadata:** milestone, type, size, dependencies, labels, parallelism, primary
   package/ownership boundary, and expected developer time.
2. **Outcome:** one observable result stated without implementation marketing.
3. **Why now:** the user failure or architecture dependency that makes the work
   necessary.
4. **Current evidence:** concrete current packages, tests, commands, defects, or
   accepted contracts that the implementer must revalidate at the actual base SHA.
5. **Scope:** the included behavior and likely destinations.
6. **Implementation constraints:** authority, idempotency, platform, privacy,
   final-mile, and reuse-before-rewrite requirements.
7. **Suggested sequence:** guidance for making the change reviewable; it does not
   excuse skipping current-code discovery.
8. **Acceptance criteria:** no more than five independently verifiable outcomes.
9. **Verification:** focused local evidence, required remote evidence, fault
   injection, and exact-SHA expectations.
10. **Failure and rollback:** partial-failure behavior, retry/adoption boundary,
    retained evidence, and whether revert is data-safe.
11. **Privacy and security:** data classification, redaction, credentials,
    permissions, untrusted input, and path/process safety.
12. **Resource ceiling:** local process, CPU, RSS, output, time, retry, and
    provider-native child limits.
13. **Non-goals:** nearby behavior intentionally excluded.
14. **Definition of done:** accepted PR, exact remote evidence, documentation,
    migration/cleanup state, and downstream unblock condition.

When an issue file combines headings for readability, the publisher must retain
all fourteen concepts in the final body. Do not shorten an issue to a title and a
few acceptance bullets.

## Global Implementation Constraints

These constraints apply to all 109 issues and need not be rediscovered per PR:

- Supported release platform is macOS Apple Silicon (`darwin/arm64`) only.
- Runtime state belongs under `$LOOPCODER_HOME`, never a customer repository,
  checkout, worktree, Git index, or GitHub-tracked sidecar.
- GitHub issue/PR/check/review/commit state is collaboration authority. Local
  SQLite is current-machine execution authority only.
- Cross-Mac continuation happens after terminal issue/PR handoff through GitHub.
  Do not copy, synchronize, branch, or merge live SQLite databases.
- Use pure-Go SQLite through `database/sql` and `modernc.org/sqlite`. Dolt is not
  part of v0.9. Reopen that decision only for concurrent offline DB histories
  that must merge.
- Reuse proven v0.8 mechanics behind a smaller interface before writing a second
  store, supervisor, provider adapter, router, report system, or delivery engine.
- New and legacy stores cannot both write the same operation/project. Legacy
  mutation becomes compatibility-only and then is removed after parity.
- One append-only project event stream owns lifecycle truth. Status, reports,
  UI delivery, attention, and audit are projections or acknowledged consumers.
- Process/OS/Git/GitHub evidence outranks provider prose. Heartbeat/liveness is
  distinct from concrete progress and neither alone proves completion.
- Every supported UI consumes the public `loopcoder.ui.v1` contract. Terminal
  is the reference UI; the local HTTP/SSE bridge is the generic integration;
  Paseo is the first external conformance client and has no privileged core
  path.
- `persisted`, `streamed`, `accepted`, `rendered`, and `seen` are different
  final-mile stages. Claim only the highest stage with real evidence. A report
  required by policy is not received until a required client proves `rendered`.
- The required attempt-start report must be rendered before provider launch.
  One missed mandatory report creates durable `delivery_degraded` attention;
  two consecutive missed intervals stop or explicitly detach the run according
  to the immutable policy snapshot. The product must never continue silently.
- An explicit provider/model/effort/permission pin wins or fails closed. Automatic
  routing cannot replace it. Any route change creates a visible successor attempt.
- Waiting for CI, approval, quota reset, cooldown, retry, or host reconnect makes
  zero coding-model calls and uses bounded local watchers.
- Provider-native sub-agents remain inside one top-level Attempt and its process,
  route, resource, output, cancellation, and terminal envelope.
- One writer owns one worktree. A verifier reviews a stable commit and never runs
  concurrently with a worker that can still modify it.
- No code, test, fixture, schema, prompt, or prose is copied/translated from the
  researched projects. Paseo's AGPL implementation receives strict separation.

## Default Development Resource Ceiling

Unless the issue explicitly narrows it further:

| Resource | Limit |
| --- | ---: |
| Active coding agent | 1 |
| Active verifier while coding agent runs | 0 |
| Provider-native sub-agents | 0 unless explicitly approved |
| Local test process | 1 |
| Aggregate child processes | 8 |
| Aggregate RSS | 2 GiB |
| Sustained CPU | 150 percent |
| Local command soft/hard deadline | 10 / 15 minutes |
| Automatic retry per failed stage | 1 |
| Agent status interval | at most 5 minutes |

Full repository test, full race, security, release build, signing, and exact-
artifact smoke belong to remote evidence tiers. Pre-push must remain under 60
seconds.

## Five-Minute Development Report

While an ordinary coding agent is active, the current host must post at least:

```text
issue / stage / elapsed
last concrete evidence
actual provider / model / effort / permission
active process count and resource state
local and remote gate
blocker and next timeout
next action and next report deadline
```

This is a development-control rule until the product's P2 visibility path is
proven. Two consecutive intervals without concrete evidence require stopping or
detaching and returning control. A generic "still working" line is not concrete
evidence.

## PR And Merge Rules

- One issue normally produces one PR and one primary behavior.
- Split before implementation if the work exceeds two days, five acceptance
  criteria, one state machine, one migration concern, or one public command.
- Local validation is focused. Required hosted checks are derived from current
  repository policy. Greptile Review is optional unless policy explicitly makes
  that exact check required.
- A worker exit or prose report is not merge evidence. The PR head, remote checks,
  independent verifier where required, and human gate are authoritative.
- A timeout after an external side effect requires read-back/reconciliation before
  retry. Never rerun provider work merely because push/PR/report delivery timed out.
- Do not force-push, reset/delete unknown worktrees, auto-merge, or alter protected
  branch/release settings unless the named issue explicitly requires and the owner
  approves that side effect.

## Publication Order

1. Review the roadmap, disposition map, UI protocol, issue index, and this
   contract.
2. Approve one dependency-ready issue or a bounded capability-gate backlog.
3. Create the `v0.9.0` milestone and labels without assigning implementation.
4. Publish dependency-ready issue bodies manually; preserve stable `V090-NNN`
   IDs. Do not publish all 109 issues as an active implementation queue.
5. Assign one issue through ordinary development with an owner-selected developer
   or agent route.
6. Merge only accepted evidence, update the issue index through a roadmap PR,
   and then unblock the next dependency. Phase numbering organizes ownership;
   capability gates, not phase order alone, control product advancement.

## Draft Files

- [`phase-0-foundation.md`](phase-0-foundation.md)
- [`phase-1-local-authority.md`](phase-1-local-authority.md)
- [`phase-2-visible-runtime.md`](phase-2-visible-runtime.md)
- [`phase-3-direct-run.md`](phase-3-direct-run.md)
- [`phase-4-provider-router.md`](phase-4-provider-router.md)
- [`phase-5-workflows.md`](phase-5-workflows.md)
- [`phase-6-operations-release.md`](phase-6-operations-release.md)
