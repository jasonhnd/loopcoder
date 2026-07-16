# Self-Hosting Development Playbook

Status: mandatory repository process
Applies to: every run in which LoopCoder plans, implements, verifies, recovers,
or releases changes to the LoopCoder repository itself

This playbook supplements [`PROCESS.md`](PROCESS.md). The doc-first sequence
remains mandatory. This document adds the stricter scope, resource, progress,
retry, verification, and release controls required when the delivery engine is
developing itself.

When the binary does not yet enforce a rule in this document, the Conductor
must enforce it procedurally and fail closed. Documentation must not claim a
runtime guard exists until implementation and tests prove it.

## Why Self-Hosting Needs a Separate Policy

In a normal consumer repository, a LoopCoder defect can fail one delivery. In
the LoopCoder repository, a defect in dispatch, supervision, progress,
recovery, worktree ownership, verification, or delivery can corrupt the very
mechanism being used to fix that defect.

Self-hosting therefore has recursive risk:

```text
LoopCoder controls a provider
  -> the provider changes LoopCoder
    -> the changed LoopCoder controls later providers
      -> recovery depends on the changed LoopCoder
```

The safe response is not to prohibit dogfooding. It is to keep authority,
resource use, checkpoints, and recovery bounded at every layer.

## Authority Order

For self-hosting work, apply authority in this order:

1. system and tool safety constraints;
2. the user's current explicit instruction;
3. [`PROCESS.md`](PROCESS.md);
4. this playbook;
5. [`SKILL.md`](../SKILL.md);
6. `.delivery.yml`;
7. the accepted spec and issue acceptance criteria;
8. append-only learnings.

Report a conflict rather than silently selecting the less restrictive rule.

## 1. Intake and Scope Budget

### Version budget

A minor release should contain no more than 8 to 12 implementation issues.
That number excludes a small number of documentation and release-record issues,
but it includes hardening needed to make the planned behavior usable.

If the roadmap needs more work, split it across milestones before dispatch.
Do not use a larger model or more sub-agents to conceal an oversized release.

### Issue budget

One implementation issue must satisfy all of the following by default:

- one primary product behavior;
- no more than five independently testable acceptance criteria;
- one ownership boundary;
- one migration concern at most;
- one expected Worker attempt of no more than 30 minutes;
- one focused test set; and
- one PR.

Split the issue when it combines multiple lifecycle systems, multiple storage
migrations, independent host integrations, or unrelated failure modes.

### Dependency budget

- Maximum roadmap dependency depth: 3.
- Maximum active issue fan-out: 3.
- Maximum implementation work in flight: 1 during self-hosting release
  stabilization.
- Never stack a dependent PR before its prerequisite contract and code are
  merged unless the user explicitly approves a disposable experiment.

### Acceptance test

Before publishing an issue, answer:

1. What single user-visible or operator-visible outcome changes?
2. What does not change?
3. What public evidence proves completion?
4. What is the largest plausible failure blast radius?
5. Can delivery resume without re-running the provider?

If any answer is unclear, the issue is not ready.

## 2. Release-Candidate Freeze

### Entering RC

The Conductor may declare release-candidate freeze only when:

- every planned implementation issue is merged;
- migration and rollback contracts are accepted;
- release documentation is current;
- no known P0 or P1 implementation defect is open; and
- one candidate SHA can be named.

Record the freeze in the release-readiness issue.

### Allowed changes after freeze

Only these classes may enter the candidate:

| Class | Definition |
| --- | --- |
| P0 | Data loss/corruption, credential exposure, security boundary failure, or inability to install/start. |
| P1 | Reproducible failure of a core acceptance path, duplicate external execution, false success, or unrecoverable state. |
| Release contract | Incorrect version, archive, checksum, signature, install, upgrade, migration, rollback, or publication behavior. |

P2 defects, formatting, optional provider gaps, low-risk compatibility issues,
and enhancements default to the next patch milestone.

### Candidate limit

At most two release candidates may fail automatically. After the second failed
candidate, stop and require a human GO/NO-GO decision that chooses one of:

- fix one narrowly bounded blocker;
- defer the finding;
- reduce advertised scope;
- roll back the release; or
- abandon the candidate and re-plan.

Do not continue an unbounded audit-and-repair loop.

## 3. Local versus Remote Execution

### Remote by default

The following work belongs on GitHub-hosted runners:

| Work | Required location |
| --- | --- |
| `go test ./...` | remote |
| full `go test -race ./...` | remote |
| staticcheck, vet, and security scans | remote |
| macOS arm64 release build | remote macOS runner |
| checksums and signing | remote release workflow |
| archive install and upgrade smoke | remote macOS runner |
| predecessor migration and rollback smoke | remote macOS runner |
| protected-main integration checks | remote |

The Conductor must not repeat these checks locally simply because a remote
check is pending.

### Allowed local work

Local work is limited to:

- reading and editing source or documentation;
- formatting changed files;
- compiling the directly changed package when necessary;
- focused tests for directly changed packages;
- credential-blind provider installation and readiness inspection;
- a bounded real-host smoke that cannot run on a generic remote runner; and
- short Git/GitHub delivery commands.

### Explicit heavy-local override

Until a typed override exists, heavy local work requires explicit user
approval in the active conversation. A repository config value or old approval
does not authorize a new heavy local run.

## 4. Local Resource Budget

These are default aggregate limits for the entire LoopCoder-owned local process
tree, not limits per child.

| Resource | Default limit |
| --- | --- |
| Active Worker providers | 1 |
| Active Verifier providers | 1, never concurrent with a Worker |
| Provider-native sub-agents | 0 during self-hosting and release work |
| Local test commands | 1 |
| Soft task duration | 10 minutes |
| Hard task duration | 15 minutes |
| Child processes | 8 |
| Resident memory | 2 GiB |
| Sustained CPU | 150% (approximately 1.5 cores) |
| Automatic retries per stage | 1 |

Before launching local work, sample current machine pressure. Refuse a new
task when the host is already under high CPU, memory pressure, swap growth, or
thermal pressure.

When a limit is exceeded:

1. stop launching new work;
2. request cooperative cancellation;
3. terminate the verified process group after the grace period;
4. join cleanup and confirm no child remains;
5. persist the last known stage and evidence;
6. return `needs-human`; and
7. do not switch providers automatically.

The current v0.8 token-cost contract does not enforce all host-resource limits.
Until a resource governor is implemented, the Conductor owns this check.

## 5. Provider and Sub-Agent Policy

### One useful call at a time

Self-hosting runs advance one provider call at a time. Reconcile its report and
durable reservation before starting another call.

Do not run Worker and Verifier concurrently. A Verifier evaluates stable work;
it should not review files that a Worker can still change.

### No nested execution by default

Provider-native sub-agents are disabled for self-hosting and release work by
default, even when a provider supports them. They may be enabled only by a
separate, fingerprint-bound approval that specifies:

- maximum depth;
- maximum fan-out;
- read/write permission;
- path and worktree scope;
- budget;
- cancellation behavior; and
- expected report tree.

Multiple agents must never write the same worktree concurrently.

### Model selection

Choose a model because the task requires its capability, not because quota is
available. Quota, reset timing, and price are ranking inputs only after hard
eligibility, permission, risk, and quality requirements pass.

Waiting, heartbeat, progress, CI polling, approval polling, quota polling,
report rendering, and delivery retry must use zero model calls.

## 6. Five-Minute Progress Contract

Any active task longer than five minutes must emit a user-visible status packet
at least every five minutes.

The packet must contain:

```text
stage: <planning|coding|testing|waiting-ci|reviewing|delivering|cleanup>
elapsed: <duration>
last_progress_at: <timestamp>
last_progress: <specific evidence>
provider_active: <yes|no>
local_processes: <count>
remote_gate: <name and state or none>
next_timeout_at: <timestamp or none>
next_action: <one concrete action>
```

### Evidence requirement

Examples of real progress:

- a file diff changed;
- a focused test completed;
- a commit was created;
- a branch was pushed;
- a PR was opened;
- a hosted check changed state;
- a report was persisted;
- a blocker was classified; or
- cleanup confirmed a child exited.

CPU activity, log growth, a live PID, generic model thinking, or an unchanged
pending check is not proof of useful progress.

### No-progress stop

- First five-minute interval without evidence: report the wait and its bound.
- Second consecutive interval without evidence: cancel or detach the task and
  return control to the user.
- Never keep a local provider alive solely to wait for remote state.

Receipt generation, transport write, host acceptance, user visibility, and
acknowledgment remain separate facts. Do not claim the user saw a receipt
without matching host evidence.

## 7. Failure Classification Before Retry

Every failure must receive exactly one primary class:

| Class | Meaning | Resume point |
| --- | --- | --- |
| `implementation-failure` | Provider did not produce a valid implementation. | Worker coding stage |
| `test-failure` | A specific required check found a defect. | Fix stage for that defect |
| `provider-failure` | Provider failed before producing usable work. | Provider stage, once |
| `delivery-failure` | Commit/push/PR/report delivery failed after usable work existed. | Exact failed delivery step |
| `infrastructure-failure` | GitHub, runner, network, disk, or host failed independently of implementation. | Preserve work; retry infrastructure step |
| `waiting-timeout` | External state did not change before its bound. | Waiting state or human decision |
| `human-decision-required` | Evidence is ambiguous or policy requires approval. | Stop |

### Retry rules

- Classify before retrying.
- Retry automatically at most once per stage.
- Never restart coding for a push, PR creation, report, or CI-wait failure.
- Never call a provider again when a valid implementation commit already
  exists.
- Never infer that a timed-out delivery command means the remote write failed;
  reconcile remote state first.
- Preserve the worktree and commit until delivery is resolved.
- Ambiguous external side effects become `human-decision-required`.

### Delivery-only recovery sequence

When implementation is already committed:

1. verify the commit identity and parent;
2. inspect the remote branch;
3. push only when the remote lacks the commit;
4. inspect existing PRs before creating one;
5. create or update the PR idempotently;
6. persist and surface the report; and
7. clean the worktree only after durable delivery evidence exists.

## 8. Verification Evidence Plan

Verification is selected by evidence need, not repeated by every actor.

| Boundary | Required checks |
| --- | --- |
| Worker | format, compile, and focused tests for directly changed packages |
| Implementation PR | remote `verify`, `test`, `race`, and `security` as configured |
| Promotion PR | protected remote checks; no duplicate local full suite |
| Integrated `main` | one remote integration run on the merge SHA |
| Release tag | one full race gate, build once, sign once, exact-artifact smoke |

### Pre-push budget

Pre-push checks must finish within 60 seconds on the supported development
host. They may validate formatting, generated files, and a small deterministic
sentinel. They must not run the full repository suite or full race detector.

### Timing tests

Tests must use visible state transitions, barriers, or injected clocks instead
of assuming a fast or slow wall clock. Stress iteration counts must be
appropriate for the test tier; full stress belongs in a dedicated remote gate.

### Failed checks

Read the exact failed job and test before changing code. A package timeout, OS
runner delay, or fixture collision is not automatically evidence of a product
defect in the feature under review.

## 9. Deterministic Waiting

CI, approval, quota reset, delivery outbox, and worker-terminalization waits
must:

- invoke no provider;
- hold no Worker or Verifier process;
- use no busy loop;
- persist restartable state;
- emit the five-minute status packet;
- use bounded backoff or event-driven wake-up;
- deduplicate equivalent wake events; and
- terminate at a documented deadline.

Do not run `gh ... --watch` or an unbounded shell polling loop from an
interactive conductor. Perform one short query, persist the state, and return
control. A later invocation can reconcile the remote state.

## 10. Worktree, Branch, and Process Hygiene

### Worktree ownership

- One writer owns one worktree.
- A review worktree is read-only.
- A worktree path is never reused for a different issue until cleanup is
  confirmed.
- A branch tip must be reconciled against the intended commit before any
  forceful update.
- Empty commits must not be used as a generic CI retrigger when they can replace
  or obscure an implementation tip.

### End-of-attempt cleanup

Before declaring an attempt terminal, prove:

- provider process exited;
- test process exited;
- status watcher exited;
- process group has no verified child;
- commit and delivery state are durable;
- retained worktree has an explicit reason and owner;
- stale worktree registrations are identified;
- temporary evidence contains no credentials or raw host session data; and
- the final report is persisted and visible.

Cleanup failure is a visible `delivery-failure` or
`human-decision-required` state. It is not background housekeeping.

## 11. Privacy and Public Evidence

Never place the following in public issues, PRs, commits, fixtures, or docs:

- local usernames, home directories, absolute machine paths, hostnames, device
  identifiers, process IDs, or temporary directory names;
- credentials, cookies, private account identifiers, private quota balances,
  billing details, or subscription data;
- raw provider transcripts, prompts, local report payloads, or application
  session identifiers; or
- screenshots containing private workspace or account data.

Use public-safe evidence:

- issue and PR links;
- workflow URLs;
- commit and artifact hashes;
- stable error codes;
- redacted command shapes;
- deterministic fixture results; and
- generic paths such as `<repo>`, `$LOOPCODER_HOME`, and `<candidate-sha>`.

## 12. Release Completion

Use
[`reference/development-release-checklist.md`](reference/development-release-checklist.md)
for the executable checklist.

A release is complete only when all of the following are true:

- protected `main` contains the final candidate SHA;
- integrated CI passes on that exact SHA;
- the tag resolves to that SHA;
- the release contains exactly the advertised platform assets;
- checksums and signature verify;
- the extracted binary reports the intended version, commit, and build date;
- install and upgrade smoke pass on the staged artifact;
- predecessor migration, backup, idempotent replay, and rollback pass;
- the protected publication environment approves the already-smoked draft;
- the release is public, not draft;
- post-publication download verification passes;
- release blockers are closed; and
- the GO/NO-GO record links the evidence.

## 13. Stop Conditions

The Conductor must stop automatic work and return `needs-human` when any of the
following occurs:

- local resource budget exceeded;
- two consecutive five-minute intervals without evidence;
- a stage fails after one automatic retry;
- one issue is reopened twice;
- two release candidates fail;
- provider launch or external side effect is ambiguous;
- Worker and Verifier evidence disagree on a P0/P1 issue;
- current work would violate RC freeze;
- remote branch identity cannot be proven; or
- privacy-safe reporting cannot be produced.

Stopping is a successful safety outcome. It must not be treated as a reason to
start a different provider automatically.

## 14. Conductor Closeout

At the end of every self-hosting unit, report:

```text
goal:
scope delivered:
scope deferred:
implementation PR:
candidate ancestor:
checks:
provider calls:
local peak process count:
local heavy tests: none | explicitly approved
retries by class:
retained worktrees:
open blockers:
next human decision:
```

Then propose any reusable learning with public evidence. Do not silently edit
the playbook in the same run that exposed the problem unless the user approved
that process-document change.
