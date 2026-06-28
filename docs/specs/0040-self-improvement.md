---
id: 40
title: loopcoder Self-Improvement Loop
status: accepted
date: 2026-06-26
issue: 40
pr: null
supersedes: []
superseded_by: []
---

# loopcoder Self-Improvement Loop

Status: TARGET DESIGN (not yet built). Date: 2026-06-26.

This document designs bounded recursive self-improvement for loopcoder. It is
written doc-first per [PROCESS.md](../PROCESS.md): the design must merge before
any implementation issue changes `SKILL.md`, scripts, prompts, or run-state
handling.

## 1. Problem

loopcoder is already self-hosting in a narrow but important sense: it can use
its own doc-first process, issue planning, worker dispatch, review loop, and
human merge gate to improve the loopcoder repository itself. It has already
written or revised its own conductor playbook, scheduler documentation, and
worker mechanics through the same workflow it gives to other repositories.

Self-hosting is not the same as self-improvement.

The current system does not yet have a durable feedback channel from past runs
back into future harness behavior. The conductor can notice a worker failure in
the current chat, and a human can manually edit `SKILL.md`, docs, or scripts
after seeing a pattern. But once the session ends, most operational knowledge is
lost unless somebody turns it into a committed document by hand. That creates
four recurring problems:

- The same gotcha can be rediscovered in several runs.
- Worker prompts can omit conventions that were learned in earlier runs.
- Mechanical failures in scripts can be treated as one-off noise rather than
  evidence for a harness improvement.
- Changes to the harness can be made opportunistically, without a clear record
  of the run evidence that justified them.

The goal is to close that loop without letting the agent rewrite itself
unboundedly.

## 2. Goal

loopcoder should observe its own runs, preserve useful run learnings, and
periodically propose improvements to its own harness under human approval.

The harness includes:

- The conductor playbook in [`../SKILL.md`](../../SKILL.md).
- Worker and dispatch code such as `loopcoder dispatch` and `internal/worker`.
- Process, architecture, scheduler, worker, and usage docs under `docs/`.
- Future prompt templates, issue templates, trace parsers, and verification
  helpers that shape how loopcoder behaves.

The target loop is:

```text
run loopcoder
  -> capture run trace and append learnings
  -> identify recurring failures, conventions, and effective tactics
  -> draft improvement candidates
  -> human approves which candidates become GitHub issues
  -> improvements follow the normal doc-first flow
  -> implementation PRs are reviewed, verified, and merged only by human choice
  -> future runs read the merged learnings and improved harness
```

The design deliberately makes "reflection" a distinct stage. loopcoder may
analyze its own behavior and propose changes, but proposal is not mutation. Any
change that can affect future behavior must pass through the same issue,
documentation, review, verification, and merge gates as human-requested work.

## 3. Non-Goals

This design does not add any of the following:

- Automatic modification of `SKILL.md`, scripts, or docs without human
  approval.
- Automatic merge of self-improvement PRs.
- Model-weight updates, fine-tuning, or hidden policy changes.
- A large autonomous daemon that rewrites the whole repository.
- A cross-repository telemetry service.
- A replacement for the mandatory doc-first process in [PROCESS.md](../PROCESS.md).
- A special privileged path for loopcoder's own repository.

Self-improvement is a proposal and evidence system wrapped around the existing
delivery loop, not a bypass around that loop.

## 4. References And Lineage

This design adapts three external patterns, with stricter gates for loopcoder's
own harness.

### snarktank/ralph

[snarktank/ralph](https://github.com/snarktank/ralph) is an autonomous coding
loop that runs fresh agent iterations against a PRD until the PRD items pass.
Its key persistence pattern is that memory crosses iterations through files,
especially `progress.txt`, `prd.json`, git history, and agent instruction files
such as `AGENTS.md` or `CLAUDE.md`.

The Ralph pattern matters here because it separates ephemeral agent context from
durable operational memory. A fresh agent instance does not remember the prior
chat, so the loop writes down what happened and reads it on the next iteration.
Ralph's `progress.txt` captures chronological progress and learnings; its
agent instruction files capture reusable patterns, gotchas, and conventions
that future agents should follow.

loopcoder should adopt the same memory shape but with stronger governance:
learnings become evidence, and evidence can drive proposed harness changes, but
the harness does not change itself automatically.

### Addy Osmani On Self-Improving Coding Agents

Addy Osmani's
[Self-Improving Coding Agents](https://addyosmani.com/blog/self-improving-agents/)
describes continuous coding loops that pick small tasks, validate them, log
learnings, reset context, and continue. His
[Agent Harness Engineering](https://addyosmani.com/blog/agent-harness-engineering/)
framing is directly relevant to loopcoder: the harness is the prompts, tools,
context policies, feedback loops, and recovery paths around the model. When an
agent repeatedly fails, the durable fix is often a harness improvement rather
than a larger prompt in the next one-off chat.

loopcoder's self-improvement loop treats that harness as a real engineering
artifact. The improvement path should ask: what behavior failed, what evidence
shows recurrence, which harness component should change, and how will the next
run verify that the change helped?

### 2026 Self-Modification Trend

By 2026, self-improving agents are increasingly discussed as systems with an
explicit reflection or improvement stage rather than a single execution loop.
The research direction includes agents that reflect on traces, generate
improvements, and in some cases edit their own implementation. The
[Self-Improving Coding Agent](https://arxiv.org/abs/2504.15228) work is a
clear example of an agent framework where the agent can edit its own code and
evaluate whether the change improves benchmark performance. The 2026 trend
that matters for loopcoder is this separation of stages: execution produces
traces, reflection studies those traces, and implementation changes are
proposed as a distinct artifact with their own verification needs.

loopcoder borrows the structure, not the autonomy level. It treats reflection
as a first-class stage, but it keeps code-level self-modification behind human
approval, doc-first review, and normal merge gates.

## 5. Current Baseline

The built v1 architecture is described in [architecture.md](../reference/architecture.md).
The important constraints are:

- The Opus chat session is the conductor and runtime.
- The conductor keeps a compact in-chat state table.
- Workers run in separate git worktrees through `loopcoder dispatch`.
- GitHub issues, PRs, checks, and labels are the durable project state.
- The verifier role is the conductor reviewing worker PRs and checks.
- The user names PRs to merge; loopcoder never auto-merges.
- Documentation and code work are separated by [PROCESS.md](../PROCESS.md).

Self-improvement must fit that baseline. In v1 it should not require a daemon,
cloud service, database, or new orchestrator. The first implementation can be
small: a learnings file, prompt changes that make conductor and workers read
it, and a manual or triggered reflection pass that drafts improvement issues.

## 6. Design Principles

### Evidence Before Rules

Do not add harness rules because they sound plausible. Add them because a run
trace, PR review, check failure, or repeated human correction shows that the
harness needs the rule.

### Proposal Before Mutation

The agent may draft a change proposal. It may not silently apply that proposal
to the harness. A proposed self-change becomes real only after the normal issue,
doc, review, verification, and merge flow accepts it.

### Append-Only Memory

Operational memory should be append-only by default. Append-only history makes
it possible to see what the agent learned, when it learned it, and which
evidence supported it. Corrections should be new entries that supersede older
entries rather than rewrites that erase history.

### Small, Bounded Loops

Reflection is useful only if bounded. Each reflection pass should analyze a
limited window of runs, propose a small number of candidates, and stop. It must
not recursively spawn more self-improvement work without human approval.

### Higher Scrutiny For Harness Surface

Changes to `SKILL.md`, dispatch code, process docs, and gate behavior are
high-scrutiny because they affect every future run. The default should be
documentation or prompt clarification before implementation logic, and code
before any change that weakens gates.

### Human-Owned Governance

The human remains the approver of record. This is not just a safety rule; it is
an engineering quality rule. Human review decides whether a pattern is real,
whether the proposed abstraction is worth carrying, and whether the harness is
becoming too complex.

## 7. The Learnings File

### Purpose

The learnings file is loopcoder's durable operational notebook. It captures the
parts of a run that should influence future runs:

- Gotchas discovered while planning, dispatching, reviewing, or merging.
- Repository conventions that workers should follow.
- Recurring worker failures and their likely causes.
- Commands that worked or failed in the actual environment.
- Verification gaps found by the conductor or human.
- Prompt instructions that were missing or ambiguous.
- Worker or helper behavior that caused confusion or operational risk.
- Improvement candidates that should be considered later.

It is inspired by Ralph's `progress.txt` and `AGENTS.md` pattern, but it is
adapted for loopcoder's doc-first process.

### Target Path

The target canonical file is:

```text
docs/learnings.md
```

Rationale:

- It lives under `docs/`, matching loopcoder's doc-first convention.
- It is versioned and reviewable through the normal PR flow.
- It can be linked from `SKILL.md` without introducing a new root-level agent
  instruction surface.
- It keeps the first implementation minimal.

A future root `AGENTS.md` or `CLAUDE.md` pointer may be added only through a
separate approved issue. Root instruction files are higher authority than a
normal reference doc in many tools, so adding one is a harness change, not just
a documentation convenience.

### Authority Level

The learnings file has advisory authority. It informs conductor and worker
prompts, but it does not override:

1. System and tool safety constraints.
2. The user's current request.
3. [PROCESS.md](../PROCESS.md).
4. [`../SKILL.md`](../../SKILL.md).
5. `.delivery.yml` in the target repository.
6. Issue acceptance criteria.

If a learning conflicts with a higher-authority source, the conductor should
report the conflict and prefer the higher-authority source.

### Append-Only Structure

`docs/learnings.md` should be append-only after its header. A future
implementation should use a structure like this:

```markdown
# loopcoder Learnings

Status: APPEND ONLY. Entries may be superseded by later entries, but existing
entries are not rewritten except to fix formatting or remove sensitive data.

## Entry Template

### 2026-06-26 - run <run-id> - <short title>

- Scope: <issue, PR, or command>
- Role: conductor | worker | verifier | human
- Observed: <what happened>
- Evidence: <links to issue, PR, check, log, or command output>
- Learning: <reusable fact or pattern>
- Applies to: <SKILL.md | worker code | helper scripts | docs | scheduling | worker prompts | repo-specific>
- Candidate improvement: <none | suggested issue title>
- Confidence: low | medium | high
- Supersedes: <optional earlier entry id>
```

This shape keeps the file useful to both humans and agents. It distinguishes
evidence from interpretation, records confidence, and names the harness surface
that might change.

### Read Policy

The conductor should read `docs/learnings.md` during loopcoder intake when the
current repository is the loopcoder repository or when the user explicitly asks
for self-improvement analysis. It should not blindly paste the entire file into
every worker prompt once the file becomes large.

The target read policy is:

- Always read the header and most recent entries.
- Search for entries matching changed paths, commands, issue labels, failing
  checks, or target component names.
- Include only relevant excerpts in worker prompts.
- Tell the worker that learnings are advisory and must be reconciled with the
  issue and current docs.

Workers should receive selected learnings in their issue prompt, not be asked
to infer the whole history from a large file. The conductor remains responsible
for deciding which memory is relevant.

### Write Policy

Every loopcoder run should end with a learning review. The review asks:

- Did we discover a reusable convention?
- Did a worker repeat a known mistake?
- Did a command, script, or check fail in a way future runs should know?
- Did a prompt instruction prevent an error?
- Did human review catch something the harness should catch earlier?

If the answer is yes, the conductor drafts one or more learning entries.

Because `docs/learnings.md` influences future behavior, committed updates to it
must go through review like other documentation. The lowest-friction path is:

1. The conductor includes a proposed learning entry in the final run report.
2. The human approves, edits, or rejects the entry.
3. A small documentation PR appends the approved entry.
4. Future runs read the merged entry.

For long-running local experiments, an implementation may also keep uncommitted
run notes outside the repository or under an ignored trace directory. Those
notes are raw observations, not authoritative learnings, until promoted through
review.

### Concurrency

Parallel workers should not all edit `docs/learnings.md` directly. That would
create noisy conflicts and bundle unrelated learning changes into feature PRs.

Instead:

- Workers may return learning candidates in their final output.
- The conductor collects those candidates.
- The conductor deduplicates and classifies them.
- A separate learning-update PR appends the approved entries.

This keeps feature PRs focused and preserves the one-concern-per-PR rule.

## 8. Run Trace Model

The learnings file should not be the only evidence source. It is the distilled
memory. The reflection stage also needs run traces.

A run trace is a structured record of what happened during a loopcoder run. In
the minimal version, it can be reconstructed from chat state, worker output,
GitHub issues, PRs, and checks. A later implementation may write explicit trace
files.

Target fields:

| Field | Purpose |
| --- | --- |
| `run_id` | Stable identifier for the conductor run or batch. |
| `started_at` / `ended_at` | Bounds the observation window. |
| `repo` / `branch` | Names the repository and base branch. |
| `issues` | Issues planned, published, dispatched, blocked, or closed. |
| `workers` | Worker command, worktree path, branch, exit status, and log path. |
| `prs` | PR numbers, URLs, changed files, check status, and review verdict. |
| `failures` | Command failures, check failures, merge conflicts, review blockers. |
| `human_decisions` | Approvals, rejections, merge instructions, and escalations. |
| `learning_candidates` | Candidate entries proposed by workers or conductor. |
| `harness_version` | Commit SHA or file checksums for `SKILL.md`, worker/helper code, and docs used in the run. |

The `harness_version` field is important. Without it, a future reflection pass
cannot tell whether a failure happened before or after a relevant harness
change.

## 9. Improvement Loop

The self-improvement loop is a separate reflection pass around normal
loopcoder delivery.

### Triggers

The loop may be triggered by:

- A human command such as "run a loopcoder improvement review".
- The end of a loopcoder run that produced failures or human corrections.
- A recurrence threshold, such as the same failure class appearing in two runs.
- A scheduled maintenance issue, if a future implementation adds scheduling.
- A high-signal event, such as a worker touching forbidden files, dispatch
  failing before Codex starts, or verification catching an issue the worker
  should have caught.

The first implementation should prefer manual and end-of-run triggers. Periodic
triggers can come later after trace quality is good.

### Stages

```text
1. Observe
   Read recent run traces, issue/PR state, checks, verifier notes, and
   docs/learnings.md.

2. Classify
   Group events by failure class, missing convention, worker/helper weakness,
   verification gap, or successful reusable tactic.

3. Reflect
   Decide whether each pattern justifies a harness improvement, a learning
   entry, both, or neither.

4. Propose
   Draft a small set of improvement candidates with evidence, target surface,
   expected impact, risk, and verification plan.

5. Approve
   Ask the human which candidates should become GitHub issues. Do not publish
   issues without approval.

6. Document
   For behavior or implementation changes, open a documentation issue first.
   The doc describes the target change and must merge before code work starts.

7. Implement
   Dispatch code or helper changes only after the relevant design/spec doc has
   merged, following PROCESS.md.

8. Verify
   Review diffs, run checks, inspect changed harness surfaces, and confirm that
   the new behavior is consistent with the approved doc.

9. Measure
   In later runs, compare the same failure class against the new harness
   version and append a learning entry if the change helped or failed.
```

### Improvement Candidate Format

Improvement candidates should be presented before issue creation in a compact,
reviewable form:

```markdown
### Candidate: Add worker prompt instruction for stale generated artifacts

- Evidence: Issue #N worker left generated artifacts after local build; verifier removed them.
- Failure class: cleanup / repo hygiene
- Target: SKILL.md worker briefing section or docs/reference/worker.md
- Proposed change: Tell workers to inspect `git status --short` after verification and remove generated build artifacts not required by the issue.
- Expected effect: Fewer noisy PRs and fewer verifier cleanup passes.
- Risk: Medium if phrased too broadly; could cause workers to delete legitimate generated files.
- Verification: Run a doc-only worker task and confirm final prompt/report includes git status hygiene without deleting expected files.
- Recommendation: Approve documentation issue, defer script enforcement.
```

This format prevents vague "make the agent better" issues. Each candidate must
name evidence, target, risk, and verification.

## 10. What May Self-Improve

Self-improvement targets are divided by scrutiny level.

| Target | Allowed? | Scrutiny | Notes |
| --- | --- | --- | --- |
| `docs/learnings.md` append entries | Yes | Medium | Advisory memory. Entries require review before commit. |
| Process and architecture docs | Yes | Medium | Must not contradict `PROCESS.md`; process changes need explicit human approval. |
| Usage docs and troubleshooting docs | Yes | Medium | Good first target for repeated operational gotchas. |
| Worker briefing templates | Yes | High | Affects generated code quality and scope control. |
| [`../SKILL.md`](../../SKILL.md) playbook text | Yes | High | Changes conductor behavior across all loopcoder runs. Keep diffs small and evidence-backed. |
| `internal/worker` and `loopcoder dispatch` logging and ergonomics | Yes | High | Requires command-level verification of the binary worker path. |
| `internal/worker` and `loopcoder dispatch` execution semantics | Conditional | Very high | Requires design doc, focused tests or dry run, and explicit human review. |
| `.delivery.yml` defaults in loopcoder repo | Conditional | High | Do not change model or effort defaults unless the human explicitly requested that policy. |
| Issue templates or PR templates | Yes | Medium | Useful for standardizing improvement evidence and acceptance criteria. |
| Trace parser or reflection helper scripts | Yes | High | Must be deterministic and inspectable; no hidden network behavior. |
| CI checks for harness changes | Yes | High | Additive checks are allowed; weakening checks is off-limits without governance approval. |

## 11. Off-Limits Targets

The self-improvement loop must not propose or perform these changes as ordinary
self-improvement work:

- Disabling or weakening the human approval gate.
- Enabling auto-merge for loopcoder's own harness changes.
- Editing secrets, tokens, credentials, or local authentication files.
- Changing GitHub branch protection or repository permissions.
- Hiding prompts, traces, or tool outputs from human review.
- Removing required verification steps to make the loop faster.
- Broad rewrites of `SKILL.md` or scripts without a decomposed design.
- Model routing, model choice, or reasoning-effort defaults unless explicitly
  requested by the human.
- Installing new background services, scheduled tasks, or startup hooks without
  a separate operations design.
- Training or fine-tuning models as part of the loop.
- Cross-repository data collection beyond artifacts the user explicitly points
  loopcoder at.

Changes to the guardrails themselves are governance changes. They require a
separate design issue that names the guardrail being changed, the reason, the
risk, and the replacement control.

## 12. Guardrails

### Human Approval Is Mandatory

Every self-change follows the normal flow:

```text
candidate -> human approves issue -> design doc PR -> human merge
          -> code issue -> implementation PR -> review/checks -> human merge
```

The conductor may prepare drafts. It may not publish issues, dispatch workers,
or merge PRs for self-improvement without the user's explicit approval at the
same gates used for other loopcoder work.

### No Automatic Harness Mutation

loopcoder must never auto-edit its own harness as a side effect of reflection.
That includes:

- No silent `SKILL.md` edits.
- No silent script edits.
- No silent `.delivery.yml` policy edits.
- No silent root instruction-file creation.
- No hidden prompt-template rewrites.

If an implementation needs scratch output, it should write to a clearly
non-authoritative trace artifact or present a patch for review.

### Bounded Reflection

A reflection pass should have explicit limits:

- Analyze at most a configured number of recent runs or PRs.
- Propose at most a small number of candidates, default three.
- Stop after the candidate report unless the human approves issue creation.
- Do not recursively trigger another reflection pass from an unmerged
  self-improvement PR.
- Escalate if the same candidate has been rejected before.

### High-Scrutiny Path Rules

For `SKILL.md` and worker/helper changes, the improvement issue must include:

- The exact failure or missed behavior being addressed.
- The current text or implementation behavior that caused the gap.
- The proposed new behavior.
- Examples of prompts, commands, or runs that should behave differently.
- A verification plan.
- Rollback guidance.

For worker/helper changes, verification should include at least one dry run or
command-level check of the native binary. For playbook changes, verification should
include a prompt-level review: the new instruction must be specific enough to
help but not so broad that it distorts unrelated tasks.

### Separation Of Concerns

Learning updates, design docs, and code changes should stay in separate PRs
unless the approved issue explicitly says a small documentation update is part
of verification. The default remains one concern per PR.

## 13. Review And Verification

Self-improvement PRs need review that is stricter than ordinary docs cleanup.

### Documentation PRs

The verifier should check:

- The doc states whether it is current behavior or target design.
- The proposed behavior is consistent with [PROCESS.md](../PROCESS.md).
- The doc does not imply auto-merge or automatic harness mutation.
- Every improvement claim points to run evidence or a clearly stated hypothesis.
- Open questions are real unknowns, not placeholders for core requirements.

### Playbook PRs

The verifier should check:

- The change is small and localized.
- It does not weaken approval, review, or merge gates.
- It does not make the conductor implement code directly.
- It does not create ambiguous instructions that could override issue scope.
- It references the relevant design doc.

### Worker And Helper Code PRs

The verifier should check:

- The worker or helper change matches the approved design.
- It preserves worktree isolation.
- It preserves explicit provider/model behavior from `SKILL.md`.
- It does not introduce destructive filesystem operations without path checks.
- It reports enough information for future run traces.
- It was verified with the relevant native command-level checks.

### Learning PRs

The verifier should check:

- Entries are factual and evidence-backed.
- Entries do not include secrets, private tokens, or unnecessary logs.
- Entries distinguish observation from recommendation.
- Entries are broadly useful enough to carry forward.
- Entries do not contradict higher-authority docs.

## 14. Metrics

Self-improvement needs evidence that changes help. The first implementation can
track lightweight metrics manually in run summaries:

- Repeated failure count by class.
- Worker failure rate before PR creation.
- PRs requiring verifier cleanup.
- Check failures by command.
- Re-dispatch count.
- Merge conflict evictions.
- Human corrections repeated across runs.
- Time from approved issue to PR opened.
- Percentage of improvement candidates accepted, rejected, or deferred.

The goal is not to optimize every number. The goal is to know whether a harness
change reduced the failure it was designed to address.

## 15. Failure Modes

### Overfitting To One Bad Run

One failure does not always justify a harness change. The reflection stage
should prefer a learning entry for a one-off failure and reserve harness changes
for repeated or high-severity patterns.

### Memory Bloat

An append-only file can become too large to inject usefully. The conductor
should search and excerpt relevant entries rather than paste the entire file.
If memory bloat becomes a problem, a later design can add a curated summary doc
or topic index through the normal doc-first process.

### Conflicting Learnings

Some learnings will become stale. New entries should supersede older entries
explicitly. The conductor should prefer the newest relevant entry unless the
older entry cites a still-valid higher-authority source.

### Harness Drift

Small prompt changes can accumulate into a playbook that is hard to reason
about. High-scrutiny review should reject broad rules, duplicate rules, and
rules whose evidence no longer applies.

### Self-Protection Bias

A self-improving agent may propose changes that make its own work easier while
weakening review quality. The verifier must treat "faster" or "less blocked" as
insufficient unless the change preserves correctness, reviewability, and human
control.

### Hidden Authority Escalation

Creating root instruction files, adding hooks, or changing tool defaults can
increase the harness authority surface. Those changes require explicit design
and human approval; they are not routine learning updates.

## 16. Minimal Implementation Milestones

### M1 - Learnings File Design And Read Path

- Add `docs/learnings.md` with append-only structure.
- Update `SKILL.md` to read relevant learnings for loopcoder self-work.
- Update worker briefing guidance so selected learnings can be passed to
  workers.
- Verify with a documentation-only issue.

### M2 - Run Close-Out Learning Review

- Add a conductor checklist item at final report time.
- Have workers return learning candidates in their output.
- Have the conductor present proposed learning entries for human approval.
- Keep learning updates in separate PRs.

### M3 - Manual Reflection Pass

- Add a documented command or playbook section for "improvement review".
- Analyze recent issues, PRs, checks, worker outputs, and learnings.
- Produce candidate improvement issues with evidence and risk classification.
- Require human approval before issue creation.

### M4 - First Self-Improvement Through The Full Flow

- Pick one low-risk recurring failure.
- Write and merge the design/spec doc.
- Implement the smallest corresponding playbook, doc, or code change.
- Verify the next run against the original failure class.

### M5 - Trace Artifacts

- Add deterministic trace capture if manual reconstruction becomes too costly.
- Keep trace artifacts non-authoritative until promoted to learnings.
- Include harness version identifiers.

### M6 - Periodic Review

- Add periodic reflection only after manual reflection produces useful
  candidates.
- Keep the trigger bounded and report-only by default.
- Continue requiring approval before issue creation, dispatch, or merge.

## 17. Relationship To Existing Docs

- [PROCESS.md](../PROCESS.md) remains the mandatory workflow. Self-improvement
  does not bypass doc-first development.
- [architecture.md](../reference/architecture.md) describes the built v1 system. This design
  fits inside that system rather than replacing it with a daemon.
- [scheduling.md](0028-scheduling.md) governs issue dispatch and merge ordering.
  Self-improvement issues are ordinary issues in that DAG, with high-scrutiny
  labels or review notes when they touch harness surfaces.
- [`../SKILL.md`](../../SKILL.md) is the conductor playbook. Future changes to it
  should reference this design and the specific learning entries or traces that
  justify the change.
- [`../DESIGN.md`](../../DESIGN.md) is the north-star autonomous delivery design.
  This document adds a bounded improvement layer to the current local,
  human-gated v1 path.

## 18. Acceptance Criteria For Future Implementation

A future code or playbook implementation of this design is acceptable only if:

- The conductor and workers can read relevant approved learnings.
- Each run can produce proposed learning entries.
- Learning entries are append-only and evidence-backed.
- Reflection can draft improvement candidates with target, risk, and
  verification plan.
- Improvement issue creation requires human approval.
- Harness changes follow [PROCESS.md](../PROCESS.md).
- `SKILL.md` and worker/helper changes are treated as high-scrutiny.
- No self-improvement path can auto-merge or silently mutate the harness.
- The loop is bounded by explicit limits.
- The verifier can inspect the evidence chain from failure to learning to
  proposed harness change.

## 19. Summary

loopcoder should learn from its own runs, but it should not become an
uncontrolled self-rewriter. The safe version of recursive self-improvement is a
bounded evidence loop: observe real runs, append durable learnings, propose
small improvements, route those improvements through the same doc-first process
as any other work, and let the human remain the approval and merge authority.

That gives loopcoder the compounding benefit of Ralph-style persistent memory
and modern reflection-stage agent design while preserving the engineering
controls that make its output reviewable.
