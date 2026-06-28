# loopcoder Architecture

This document is the living map of loopcoder as it works now. It describes the
current repository and runtime behavior, not the larger north-star system in
[`DESIGN.md`](../../DESIGN.md).

## Overview

loopcoder is a host-agent playbook plus a native Go binary. It is not a daemon
or cloud service: a human-launched conductor session stays open, follows
[`SKILL.md`](../../SKILL.md), and calls the `loopcoder` binary for mechanical
operations such as ready-set classification, worker dispatch, verification,
recovery, state publishing, and leases.

The built loop is:

```text
need
  -> doc-first planning
  -> GitHub issues + dependency DAG
  -> human approval
  -> ready-set / dispatch-wave / dispatch
  -> worker-created pull requests
  -> loopreview + hosted checks + local gates
  -> chat progress and final report
  -> user names PRs to merge
  -> gh pr merge from the conductor session
```

GitHub issues, labels, PRs, branches, and checks remain the delivery source of
truth. Local `.loopcoder/runs/<RunId>/` state and the optional state branch help
with liveness, recovery, and cross-session handoff, but they do not replace
GitHub as the source of truth for what has been delivered.

## Roles

### Conductor

The Conductor is the interactive agent session following
[`SKILL.md`](../../SKILL.md). It intakes the user's need, inspects repository
context, drafts issues and dependency relationships, gets explicit approval
before publishing work, dispatches ready issues through the binary, folds
verification signals into a merge-eligibility report, and merges only PRs the
user names. It does not implement work items directly or recreate worker,
worktree, state, recovery, or PR mechanics.

### Worker

The Worker is a headless provider invocation owned by `loopcoder dispatch`.
The binary creates a fresh git worktree from the base branch, builds a
self-contained issue prompt, invokes the selected provider, verifies that file
changes exist, commits, pushes, opens the PR, writes attempt state, and cleans
up unless `--keep-worktree` is set. The provider edits files only; loopcoder
owns commit, push, PR creation, and cleanup.

The worker provider is selected through the shared provider registry. The
current registry supports `codex`, `claude`, and `gemini`, with `codex` as the
default in the dispatch path. See [`worker.md`](worker.md) for provider details.

### Verifier

The primary adversarial review is delegated to `loopcoder loopreview`. It checks
out the PR head in an isolated read-only worktree, gathers the PR diff, changed
files, linked issue, and referenced merged spec from `origin/<base-branch>`, and
invokes a verifier provider with a structured JSON verdict schema. The verdict
is one of `pass`, `fail`, or `needs-human`; malformed output, timeouts, provider
failure, or an unreadable referenced spec become `needs-human`.

The verifier provider should differ from the worker provider. If the invoked
verifier matches `.delivery.yml`'s worker provider, the CLI emits an advisory
warning and still proceeds because human merge remains the final gate.

## Ports And Adapters

loopcoder is organized around stable responsibilities with native adapters:

| Port | Responsibility | Current adapter |
| --- | --- | --- |
| WorkItemSource | Create, list, and classify work items | GitHub issues and labels via `gh` |
| Workspace | Create isolated implementation and review workspaces | `git worktree` |
| Worker | Implement one approved issue | Provider registry through `loopcoder dispatch` |
| VcsHost | Open PRs, read diffs/checks, and merge named PRs | GitHub via `gh` |
| Verifier | Review PRs against issue, diff, checks, and spec | `loopcoder loopreview` read-only provider invocation |
| LocalGate | Run configured local tests/typecheck/build commands | `loopcoder verify-local` |
| Gate | Decide whether a PR may merge | Human merge gate; user names PRs |
| Reporter | Surface progress, verdicts, failures, and final status | The conductor chat |

`.delivery.yml` selects per-repo defaults such as base branch, worker provider,
verifier provider, required hosted checks, local command gates, and resilience
thresholds. Optional model and effort values are passed only when the user or
configuration explicitly supplies them.

## State Model

The current state model has three layers:

- GitHub is authoritative for issue state, `blocked-by:#N` labels, open PRs,
  PR branches, closing references, and hosted checks.
- Local run state under `.loopcoder/runs/<RunId>/` records worker attempts,
  event transitions, and recovery briefs for liveness and retry decisions.
- `loopcoder state push`, `state pull`, `lease acquire`, and `lease release`
  publish scrubbed run snapshots and a best-effort conductor lease on the
  dedicated state branch when cross-session state is needed.

A fresh conductor session should re-derive delivery state from GitHub first,
then use local or pulled `.loopcoder` state only to classify same-host liveness,
recovery context, and duplicate-dispatch risk.

## Subsystems

### Orchestration

Current orchestration is a conductor-led, binary-assisted loop. `SKILL.md`
defines the doc-first sequence, issue drafting, human approval, dispatch,
verification folding, reporting, and human merge gate. The binary provides the
mechanical pieces: `ready-set` classifies open issues, `dispatch` runs one
worker, `dispatch-wave` preflights and dispatches selected ready issues with a
shared run id and throttle limit, and `resume` reconciles GitHub plus local run
state after interruption. The binary does not run an autonomous conductor or
create issues by itself; those remain conductor actions. Design rationale:
[`../specs/0081-orchestration.md`](../specs/0081-orchestration.md).

### Verification

Current verification combines three signals. Hosted CI checks listed in
`.delivery.yml ci.checks` are read through GitHub by the conductor. The
`loopcoder verify-local` command creates an isolated worktree for a PR or branch
and runs configured `ci.tests`, `ci.typecheck`, and `ci.build` command groups,
returning `pass`, `fail`, or `needs-human`. `loopcoder loopreview` runs the
independent read-only verifier described above and returns a structured verdict
plus `spec_conformance`. The conductor folds these signals and never treats
ambiguous, unavailable, malformed, pending, or missing required evidence as
merge-eligible.
Design rationale: [`../specs/0039-verification.md`](../specs/0039-verification.md).

### Resilience

Current resilience is attempt-state and recovery tooling, not a background
supervisor. `loopcoder dispatch` writes compact attempt sidecars under
`.loopcoder/runs/<RunId>/workers/*.attempt.json` and appends phase events to
`events.jsonl`; `heartbeat_at`, `last_progress_at`, phase changes, log size,
exit code, and status let `ready-set` and `resume` classify attempts as running,
stale, hung, orphaned, failed, completed-without-PR, or needing recovery. On
failure, dispatch writes a scrubbed Markdown recovery brief with issue, branch,
worktree, log, changed files, existing PR lookup, and log tail. `recover` first
adopts an existing PR when one is found, otherwise retries on a retry branch
with bounded backoff until the configured max attempts, then blocks for a human
decision. `statebranch` can publish scrubbed run snapshots, log tails, and a
best-effort lease to `loopcoder/state`. Design rationale:
[`../specs/0041-resilience.md`](../specs/0041-resilience.md).

### Delivery Guardrails

Current built guardrails are limited to readiness checks, open-PR duplicate
prevention, liveness classification, and `resilience.worker.max_attempts` retry
bounds. The 0.3.2 delivery guardrails design defines follow-up budget caps and
loop circuit-breakers for `dispatch-wave` and `recover`, but those caps are not
enforced until separate code issues implement the spec. Design rationale:
[`../specs/0192-delivery-guardrails.md`](../specs/0192-delivery-guardrails.md).

### Self-Improvement

Current self-improvement is human-gated and advisory. `docs/learnings.md` is an
append-only operational notebook; `SKILL.md` tells the conductor to read
relevant entries for loopcoder self-work, pass only relevant excerpts to
workers, and propose new learning entries at close-out when a run exposes a
reusable pattern. The optional improvement review is a bounded reflection pass
that can draft candidate improvements with evidence, risk, and verification
plans, but it must stop for human approval before creating issues or changing
`SKILL.md`, scripts, docs, `.delivery.yml`, or code. There is no autonomous
self-modifying loop and no automatic merge path. Design rationale:
[`../specs/0040-self-improvement.md`](../specs/0040-self-improvement.md).

### Scheduling

Current scheduling is partially built. The binary implements ready-set
classification from GitHub issues, `blocked-by:#N` labels, closing PR
references, open PRs, check summaries, and local attempt state. An issue is
ready when it is open, its blocked-by dependencies are completed (closed as
completed or carrying a closing PR reference), no matching open PR exists, and
no live or recovery-needed local attempt blocks duplicate dispatch.
`dispatch-wave` can dispatch a selected ready wave concurrently up to its
throttle limit, while preserving one shared run id. The remaining scheduling
design is designed-not-yet-built or playbook-only today: layered DAG planning,
observe-at-merge file-overlap ordering, conflict eviction, rebase decisions,
and merge execution are conductor and human-gate responsibilities rather than
an autonomous binary scheduler. Design rationale:
[`../specs/0028-scheduling.md`](../specs/0028-scheduling.md).

## Doc-First Process

loopcoder follows the mandatory doc-first workflow in
[`PROCESS.md`](../PROCESS.md):

1. Write and merge the design or spec under `docs/specs/`.
2. Open separate code issues only after the relevant document is merged.
3. Verify the implementation against the merged document and working behavior.

Documentation and code are intentionally not bundled in the same issue or PR.

## Design References

- [`DESIGN.md`](../../DESIGN.md) is the north-star autonomous delivery engine
  design. It includes larger ideas that are not built in the current system.
- [`../specs/0000-loopcoder-v1.md`](../specs/0000-loopcoder-v1.md) is the
  original v1 design record.
- [`../specs/0146-attestation.md`](../specs/0146-attestation.md) defines per-invocation Worker, Verifier, and Conductor attestation.
- [`../specs/0192-delivery-guardrails.md`](../specs/0192-delivery-guardrails.md) defines planned budget caps and loop circuit-breakers.
- [`../specs/`](../specs/) contains frozen design records. This architecture
  document is the living current-behavior reference.
