---
name: loopcoder
description: "Use this skill when the user invokes /loopcoder with a delivery need or describes a delivery/build request that should be planned, split into GitHub issues, dispatched to workers, reviewed, and reported in chat, such as add feature X, build Y, fix Z, implement a roadmap item, or ship a batch of repo changes."
---

# loopcoder Conductor Playbook

Use this as the canonical conductor session procedure. Claude Code loads this
file as the skill entrypoint; Codex CLI and Gemini CLI should use
[`AGENTS.md`](AGENTS.md) and [`GEMINI.md`](GEMINI.md), which point back here
instead of forking the procedure. The full design lives in
[`docs/specs/0000-loopcoder-v1.md`](docs/specs/0000-loopcoder-v1.md);
keep this playbook practical and procedural instead of duplicating the spec.

## Process discipline (doc-first)

Follow the mandatory workflow in [`docs/PROCESS.md`](docs/PROCESS.md) for every
unit of work. The order is non-skippable: first a documentation issue writes and
merges the design/spec under `docs/`; only then a separate code issue implements
per that merged doc; verification comes last and checks both conformance to the
doc and working behavior.

Never open a code issue before its design doc is merged. Never bundle
documentation and code in the same issue or PR. Never hand-write code outside
this loop.

## Operating Contract

- Invocation is `/loopcoder <need>`, or automatic activation when the user states a delivery/build request.
- The conductor session is the runtime. It must be a sufficiently capable agent session, and it must stay open while the loop runs; workers may run in the background.
- Read `.delivery.yml` as the per-repo config when present; see spec section 6. It declares adapters and defaults such as worker provider, base branch, checks, gate, and report channel. If it is absent, use the v1 defaults from the spec.
- Do not implement work items in the conductor. Dispatch implementation through
  the `loopcoder` binary (`loopcoder dispatch`); the binary owns worktree ->
  worker agent -> commit -> push -> PR.
- Keep a compact in-chat state table: issue, dependencies, status, worker job, PR, check status, verifier notes.
- Never auto-merge. Merge only PRs the user names, via `gh pr merge`.

## Backend selection

The conductor's sole mechanical backend is the native `loopcoder` binary. The
conductor still owns planning, review, reporting, doc-first ordering,
observe-at-merge ordering, and the human merge gate; only the binary executes
mechanical work.

Resolve the binary before calling it:

1. If `LOOPCODER_BIN` is set, call that path.
2. Else call `loopcoder` from `PATH`.
3. Else report that the `loopcoder` binary is required on all platforms.

Use the resolved binary for every mechanical operation. Never recreate
worktree, worker-agent invocation, commit, push, PR, liveness, recovery, resume, local
verification, state, or lease mechanics in the conductor.

Keep model and effort omitted unless the user explicitly requested them. When a
one-off override is explicit, pass it through to the selected backend for that
run only.

## Learnings (self-improvement)

Use [`docs/learnings.md`](docs/learnings.md) as advisory operational memory per
[`docs/specs/0040-self-improvement.md`](docs/specs/0040-self-improvement.md).

- Read path: during intake for loopcoder self-work, when the target repository
  is the loopcoder repository, or when the user asks for self-improvement
  analysis, read the `docs/learnings.md` header and most recent entries. Search
  it for entries matching changed paths, commands, labels, failing checks, or
  component names.
- Worker context: pass only relevant excerpts to workers. Mark them advisory
  and tell workers to reconcile them with the issue and current docs. Do not
  paste the whole learnings file into every worker prompt.
- Authority: learnings never override system or tool safety constraints, the
  user's current request, [`docs/PROCESS.md`](docs/PROCESS.md), this playbook,
  the target repo `.delivery.yml`, or issue acceptance criteria. On conflict,
  prefer the higher-authority source and report the conflict.
- Write path: at final report time, draft proposed learning entries from the
  run when it exposed reusable conventions, repeated worker mistakes,
  command/script/check failures, missing prompt instructions, or gaps human
  review caught. Present the drafts for human approval; after approval, append
  them via a separate documentation PR. Never auto-apply a learning, bundle a
  learnings change into a feature PR, or silently edit `docs/learnings.md`.
- Concurrency: parallel workers may return learning candidates in their output.
  The conductor deduplicates and classifies them; a separate learning-update PR
  appends approved entries.

## Improvement review

Use this optional, bounded reflection pass per
[`docs/specs/0040-self-improvement.md`](docs/specs/0040-self-improvement.md), especially sections 6,
9, and 12. It is proposal-only and human-gated: the conductor may draft
improvement candidates, but it must not create issues, dispatch workers, mutate
the harness, change `SKILL.md`, scripts, docs, or `.delivery.yml`, or merge
anything without explicit human approval through the normal gates.

- Triggers: run only on a human command such as "run a loopcoder improvement
  review", at the end of a run that produced failures or repeated human
  corrections, or when a failure class recurs across runs. Prefer manual and
  end-of-run triggers; do not add an automatic periodic trigger in this slice.
- Observe: read a limited recent window of run traces, issues, PRs, checks,
  verifier notes, and [`docs/learnings.md`](docs/learnings.md).
- Classify: group observations by failure class, missing convention, script
  weakness, verification gap, or effective reusable tactic.
- Reflect: decide whether each pattern justifies a harness improvement, a
  learning entry, both, or neither.
- Propose: draft at most a small set of candidates, default three, and reject
  vague "make the agent better" candidates.
- Approve: present the candidate report and ask which candidates should become
  issues. Stop after the report unless the human approves; never publish issues
  or start workers without approval.
- Doc-first: approved behavior or implementation changes follow
  [`docs/PROCESS.md`](docs/PROCESS.md): documentation issue and merged design
  first, then a separate code issue and implementation.
- Verify: review the resulting PRs against the approved doc, changed harness
  surface, checks, and evidence chain.
- Measure: in later runs, compare the same failure class against the changed
  harness version and record whether the change helped.

Improvement candidates must be presented before any issue creation in this
format:

- Evidence: concrete run, issue, PR, check, verifier note, or learning entry.
- Failure class: the grouped behavior being addressed.
- Target surface: `SKILL.md`, docs, worker prompt, dispatch script, gates,
  `.delivery.yml`, or another explicit harness surface.
- Proposed change: the smallest specific change being recommended.
- Expected effect: how the next run should behave differently.
- Risk: how the change could overfit, weaken review, or affect unrelated work.
- Verification plan: how the doc, diff, command, or prompt behavior will be
  checked.
- Recommendation: approve, defer, reject, or collect more evidence.

Bounds and scrutiny:

- Analyze only a limited window of recent runs or PRs.
- Propose no more than three candidates by default.
- Do not recursively trigger another improvement review from an unmerged
  self-improvement PR.
- If a candidate was rejected before, escalate the prior rejection and new
  evidence to the human; do not silently re-propose it.
- Treat changes to `SKILL.md`, dispatch scripts, gates, and `.delivery.yml`
  defaults as high-scrutiny. Prefer documentation or prompt clarification before
  script logic, never weaken gates, and never change model or effort defaults as
  ordinary self-improvement.

## Model and speed (never chosen for the user)

- DEFAULT: pass no model or effort flags; inherit the selected worker provider's own local/global config. Never auto-pick a model or effort per issue.
- TRANSPARENCY: in the plan, show the worker line from `.delivery.yml` `adapters.worker`, for example `worker: <adapters.worker> (configured worker provider; default codex if absent)`, so the user sees what will run without being asked.
- RECOMMENDATION ONLY: you may add one short passive hint, such as `tip: say use high for faster runs`. It is never auto-applied, and if the user says to stop suggesting, stop including the hint.
- OVERRIDE ONLY ON EXPLICIT REQUEST: for a one-off request such as `run these
  faster` or `#B use max`, pass `--effort` and/or `--model` to `loopcoder
  dispatch` / `loopcoder dispatch-wave` for that run only. For a permanent
  request such as `from now on default to high`, only then write
  `worker.reasoning_effort` and/or `worker.model` into `.delivery.yml`.
- Never write to `.delivery.yml` or change model/effort without an explicit user statement. The config must only ever reflect what the user has said.
- Natural-language effort mapping: `fast`/`quick` -> `low`; `balanced` -> `medium`; `thorough`/`max`/`highest` -> `xhigh`; `high` -> `high`.

## Procedure

1. Intake the user's need.
   - Restate the delivery goal, constraints, and any acceptance criteria.
   - Inspect the repo context needed to plan safely, especially `.delivery.yml`, `ROADMAP.md`, existing issues, and relevant docs.
   - Keep v1 to a small batch. If the need is too broad for one session, propose a smaller first batch.

2. Draft GitHub issues and a dependency DAG.
   - Classify each WorkItem as documentation or code.
   - For new behavior or implementation work, draft the documentation issue first. It writes the design/spec under `docs/` and must merge before any code issue is opened.
   - Draft code issues only for already-merged docs. Each code issue must be separate, reference the merged doc path, and state `implement per docs/<doc>.md`.
   - Do not publish speculative code issues for docs that are not merged yet.
   - Never combine documentation and code in one issue or PR.
   - Convert the need into WorkItems with clear titles, bodies, acceptance criteria, and temporary IDs.
   - Identify which items can run in parallel and which are blocked by other items.
   - Show the proposed issue list and DAG to the user, including `blocked-by` relationships and the worker line from "Model and speed".
   - Get explicit user approval before publishing anything.

3. Publish approved issues.
   - Create each issue with `gh issue create`, preserving the approved title/body.
   - Record the mapping from temporary WorkItem IDs to GitHub issue numbers.
   - For dependencies, add a `blocked-by:#N` label to each blocked issue after the dependency issue number is known.
   - Keep the in-chat DAG updated with the real issue numbers.

   Example shape:

   ```powershell
   gh issue create --title "<title>" --body "<body>"
   gh issue edit <blocked-issue-number> --add-label "blocked-by:#<dependency-issue-number>"
   ```

4. Dispatch ready issues.
   - Follow the layered ready-set scheduler in [`docs/specs/0028-scheduling.md`](docs/specs/0028-scheduling.md).
   - Compute the ready set with `loopcoder ready-set --repo . --base-branch <base-branch> --run-id <run-id> --format text`.
   - A ready issue is an unstarted issue whose `depends_on` entries / `blocked-by:#N` labels are all merged to `main`, not merely open as PRs.
   - Keep the two ordering axes from [`docs/specs/0028-scheduling.md`](docs/specs/0028-scheduling.md) separate: a real code dependency forces serial order, so B waits until A is merged and then branches from `main`; file overlap does not block dispatch.
   - Dispatch one ready wave with `loopcoder dispatch-wave --repo . --base-branch <base-branch> --run-id <run-id> ...`, or dispatch one issue with `loopcoder dispatch ...`. Then recompute as PRs merge. Repeat until the DAG is drained or blocked.
   - `loopcoder dispatch` and `loopcoder dispatch-wave` preserve git worktree creation serialization, so independent ready issues can be dispatched concurrently safely.
   - Call the selected backend once per ready issue or ready wave. Do not recreate worktree, worker-agent invocation, commit, push, or PR logic in the conductor.
   - Capture each worker's output, job handle, PR URL, and failure details.

   Example shape:

   ```text
   loopcoder ready-set --repo . --base-branch <base-branch> --run-id <run-id> --format text

   loopcoder dispatch \
     --repo . \
     --issue-number <number> \
     --issue-title "<title>" \
     --issue-body "<body>" \
     --base-branch <base-branch> \
     --provider <worker-provider>

   loopcoder dispatch-wave --repo . --base-branch <base-branch> --run-id <run-id> --issue-numbers <n1>,<n2>
   ```

5. Verify each resulting PR.
   - For each worker-created PR, delegate the primary adversarial review to the independent Verifier command. The verifier provider SHOULD differ from the configured worker provider (`adapters.worker`); if they match, report the reviewer-not-worker advisory warning and continue because the human merge gate remains the backstop.
   - Resolve the verifier provider from `.delivery.yml` `adapters.verifier` when present, otherwise use the v1 default from the spec, and run:

     ```text
     loopcoder loopreview --repo . --pr-number <pr> --provider <verifier-provider>
     ```

     Add `--base-branch <base-branch>` when the run is not targeting the default base branch.
   - Read the structured `loopreview` verdict (`pass`, `fail`, or `needs-human`) plus its findings, evidence, and spec-conformance field. A malformed verdict, unreadable referenced design doc, or verifier infrastructure / permission failure is `needs-human`, never a silent pass.
   - Follow the "Verification gate" subsection below, which folds the independent Verifier verdict together with hosted checks from `gh pr checks <pr>` and deterministic local gates from `loopcoder verify-local --repo . --pr-number <pr>`.
   - Use `gh pr diff <pr> --name-only` and, when needed, `gh pr diff <pr>` as supporting evidence for changed files and unrelated-change checks; the primary adversarial findings come from `loopreview`.
   - End every PR review with exactly one explicit conductor verdict in chat: `pass`, `fail`, or `needs-human`, with evidence for the `loopreview` verdict, required check status, local gate status, spec criteria, and changed files.

6. Merge ordering.
   - Follow the observe-at-merge ordering and conflict eviction rules in [`docs/specs/0028-scheduling.md`](docs/specs/0028-scheduling.md).
   - Never auto-merge. A `pass` verdict means merge-eligible only; it never calls `gh pr merge`. When the user names PRs to merge, read each named ready PR's real changed files with `gh pr diff <pr> --name-only`, group PRs by file-set overlap, and run `gh pr merge` only for those named PRs that remain merge-eligible.
   - Non-overlapping PRs may merge in any order. Overlapping PRs merge serially: merge the first, rebase the next onto updated `main`, verify it remains acceptable, then merge it.
   - If an overlapping PR cannot rebase cleanly, evict it from the merge group, capture the changed files, conflicting paths, rebase output, and PRs that landed, then narrow the scope and re-dispatch a worker with that context instead of blindly retrying.

7. Report progress and final status.
   - Report meaningful state changes in chat: issues published, workers dispatched, PRs opened, checks passed/failed, verifier verdicts, blocked items, and unblocked dependents.
   - Continue until the DAG is drained or blocked.
   - Run the learning review from "Learnings (self-improvement)": collect
     worker-returned candidates, draft any proposed learning entries for human
     approval, and keep approved append changes for a separate documentation
     PR.
   - If triggered by a human request, run failures, repeated human corrections,
     or a recurring failure class, offer the optional deeper pass from
     "Improvement review" and stop after its candidate report unless the human
     approves issue creation.
   - End with a final summary listing issues, PRs, verifier status, check status, and any human decisions still needed.
   - Merge only through the "Merge ordering" step, following `.delivery.yml` merge settings when present.

## Verification gate

Follow [`docs/specs/0039-verification.md`](docs/specs/0039-verification.md) for the gate model, with
the primary adversarial review delegated to `loopcoder loopreview`.

- Independent Verifier: run
  `loopcoder loopreview --repo <repo> --pr-number <pr> --provider <verifier-provider>`
  for each worker-created PR, adding `--base-branch <base-branch>` when needed.
  The verifier provider SHOULD differ from `adapters.worker`; if it does not,
  report the reviewer-not-worker advisory warning and continue. Read the
  structured verdict (`pass` / `fail` / `needs-human`), findings, evidence, and
  spec-conformance field. A parse failure, unreadable referenced spec, or
  verifier infrastructure / permission failure is `needs-human`.
- Required checks: every check named in `.delivery.yml` `ci.checks` must be
  present and green in `gh pr checks <pr>` before a PR is called
  merge-eligible. A missing, failed, cancelled, timed-out, skipped, or
  still-pending required check means the PR is not merge-eligible.
- Local command gates: for a code PR, in addition to hosted checks and the
  independent Verifier verdict, the conductor may run
  `loopcoder verify-local --repo <repo> --pr-number <pr>` to execute the
  configured local command gates from `.delivery.yml` `ci.tests`,
  `ci.typecheck`, and `ci.build`, as described in
  [`docs/specs/0039-verification.md`](docs/specs/0039-verification.md). Fold its explicit
  `pass` / `fail` / `needs-human` result into the PR verdict. If no local
  commands are configured, report that portion as `not-configured`; this is not
  a failure.
- Spec conformance: for a code PR, read the merged design doc referenced by the
  code issue from the base branch, for example
  `git show origin/main:docs/<doc>.md`. Extract acceptance criteria, Goals, and
  normative `must` / `only` / `never` items; then check that the diff implements
  them and avoids the doc's non-goals and unrelated changes.
- Empty automated gate: if `ci.checks` is empty for a code PR, report
  `needs-human` for the automated-gate portion instead of treating the gate as
  passed, while still completing the diff and spec-conformance review.
- Merge eligibility: fold the `loopreview` verdict, required hosted checks, and
  `verify-local` result into one conductor verdict. `pass` requires all
  configured required checks green, local command gates passing or
  `not-configured`, and `loopreview` returning `pass`. Any failing signal makes
  the conductor verdict `fail`; any ambiguous, unavailable, malformed, or
  permission-blocked signal makes it `needs-human`.
- Verdicts: every PR review reports exactly one conductor verdict of `pass`,
  `fail`, or `needs-human` in chat, with evidence for `loopreview`, check
  status, local gate status, spec criteria, and changed files.
- Routing: `pass` means merge-eligible and waits for the user to name it for
  merge. `fail` means a red check, missing criterion, spec violation, or
  unrelated change; re-dispatch a bounded fix pass with the failure evidence
  attached, respecting `verification.max_fix_passes`. `needs-human` means an
  ambiguous spec, missing or unreadable referenced doc, or infrastructure /
  permission failure; ask one concise question and do not mark the PR
  merge-eligible.
- Human merge remains the gate: a `pass` verdict never calls `gh pr merge`.

## Worker liveness & recovery

Follow the resilience contract in [`docs/specs/0041-resilience.md`](docs/specs/0041-resilience.md).
The conductor passes a stable `--run-id` to `loopcoder dispatch` or `loopcoder
dispatch-wave` for all dispatches in one batch so those attempts share
`.loopcoder/runs/<RunId>/` under the main repo.

Durable run state currently consists of
`.loopcoder/runs/<RunId>/workers/*.attempt.json` plus
`.loopcoder/runs/<RunId>/events.jsonl`. These files are local, gitignored, and
advisory for liveness and recovery only. GitHub issues, labels, PRs, and checks
remain the source of truth for delivery state.

- Liveness: `heartbeat_at` means the adapter advanced through a write point;
  `last_progress_at` means observable progress happened, either a phase advance
  or log growth. Treat an attempt as stale when there is no progress beyond
  `resilience.worker.stale_after_seconds`.
- Hung: treat an attempt as hung when it remains stale beyond
  `resilience.worker.hung_after_seconds`, or when the sidecar heartbeat is stale
  and no matching live process can be found.
- Idle is not done: a worker that exits with no branch, PR, or concrete
  deliverable is a retryable failure, not success. Verify a deliverable through
  the branch, PR, and required checks before counting an issue done.
- Recovery briefs: on adapter failure, `loopcoder dispatch` writes a
  context brief under `.loopcoder/runs/<RunId>/recovery/<job_id>-context.md`
  with issue, branch, worktree, log, summary, attempt, phase, status, error,
  changed files, log tail, and PR lookup details. Use it as the concrete
  recovery context described in [`docs/specs/0041-resilience.md`](docs/specs/0041-resilience.md).
- Bounded retry: recover failed, hung, or idle attempts through
  `loopcoder recover --repo <repo> --issue-number <n> --issue-title "<title>"
  --issue-body "<body>" --run-id <run-id>`. It adopts an existing PR first;
  otherwise it re-dispatches with the latest recovery brief up to
  `resilience.worker.max_attempts`, using
  `resilience.worker.retry_backoff_seconds`, and blocks after the limit with
  the brief, attempt history, and the concrete human decision needed.
- Resume: a fresh conductor session re-derives state from GitHub first: issues,
  labels, PRs, branches, and checks. Local sidecars are advisory for local
  liveness only and must not cause duplicate dispatch when GitHub already has a
  deliverable.
- Resume command: before dispatching anything after an interruption, run
  `loopcoder resume --repo <repo> --run-id <run>` (omit `--run-id` to select the
  latest local run). The report follows [`docs/specs/0041-resilience.md`](docs/specs/0041-resilience.md):
  GitHub is the source of truth, `.loopcoder` state is advisory, and resume
  prints next ready actions but never auto-dispatches, pushes, merges, or adopts
  a local attempt without conductor/human review.
- Cross-session state and conductor lease operations use the native backend:
  `loopcoder state push`, `loopcoder state pull`, `loopcoder lease acquire`,
  and `loopcoder lease release`. Treat lease operations as mutating operations:
  use the resolved binary path consistently for the operation.

## Recovery Notes

- If a worker fails, goes hung, or exits idle with no deliverable, capture the
  sidecar path, error, log path, changed files if available, and retry/block
  reason before deciding whether to re-dispatch.
- Do not dispatch dependents while an item is failed, stale, hung, idle, or past
  its retry limit. Mark it blocked in the chat state when human input is needed.
- If the session is interrupted, re-derive issue and PR state from GitHub before
  continuing, then use local sidecars only to classify same-host liveness.
