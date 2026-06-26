---
name: loopcoder
description: "Use this skill when the user invokes /loopcoder with a delivery need or describes a delivery/build request that should be planned, split into GitHub issues, dispatched to workers, reviewed, and reported in chat, such as add feature X, build Y, fix Z, implement a roadmap item, or ship a batch of repo changes."
---

# loopcoder Conductor Playbook

Use this as the Opus session's conductor procedure. The full design lives in
[`docs/specs/2026-06-26-loopcoder-v1-design.md`](docs/specs/2026-06-26-loopcoder-v1-design.md);
keep this playbook practical and procedural instead of duplicating the spec.

## Operating Contract

- Invocation is `/loopcoder <need>`, or automatic activation when the user states a delivery/build request.
- The Opus chat session is the conductor/runtime. Keep it open while the loop runs; workers may run in the background.
- Read `.delivery.yml` as the per-repo config when present; see spec section 6. It declares adapters and defaults such as worker provider, base branch, checks, gate, and report channel. If it is absent, use the v1 defaults from the spec.
- Do not implement work items in the conductor. Dispatch implementation through `scripts/dispatch-worker.ps1`; that script owns worktree -> codex -> commit -> push -> PR.
- Keep a compact in-chat state table: issue, dependencies, status, worker job, PR, check status, verifier notes.
- Never auto-merge. Merge only PRs the user names, via `gh pr merge`.

## Model and speed (never chosen for the user)

- DEFAULT: pass no model or effort flags; inherit the user's codex config (`~/.codex/config.toml`). Never auto-pick a model or effort per issue.
- TRANSPARENCY: in the plan, show the worker line as `worker: codex (your global codex setting)` so the user sees what will run without being asked.
- RECOMMENDATION ONLY: you may add one short passive hint, such as `tip: say use high for faster runs`. It is never auto-applied, and if the user says to stop suggesting, stop including the hint.
- OVERRIDE ONLY ON EXPLICIT REQUEST: for a one-off request such as `run these faster` or `#B use max`, pass `-Effort` and/or `-Model` to `scripts/dispatch-worker.ps1` for that run only. For a permanent request such as `from now on default to high`, only then write `worker.reasoning_effort` and/or `worker.model` into `.delivery.yml`.
- Never write to `.delivery.yml` or change model/effort without an explicit user statement. The config must only ever reflect what the user has said.
- Natural-language effort mapping: `fast`/`quick` -> `low`; `balanced` -> `medium`; `thorough`/`max`/`highest` -> `xhigh`; `high` -> `high`.

## Procedure

1. Intake the user's need.
   - Restate the delivery goal, constraints, and any acceptance criteria.
   - Inspect the repo context needed to plan safely, especially `.delivery.yml`, `ROADMAP.md`, existing issues, and relevant docs.
   - Keep v1 to a small batch. If the need is too broad for one session, propose a smaller first batch.

2. Draft GitHub issues and a dependency DAG.
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
   - A ready issue is one whose dependencies are all done. Dispatch independent ready issues in parallel; dispatch dependent issues only after their blockers are verified done.
   - Call the existing worker adapter once per ready issue. Do not recreate worktree, Codex, commit, push, or PR logic in the conductor.
   - Capture each worker's output, job handle, PR URL, and failure details.

   Example shape:

   ```powershell
   pwsh scripts/dispatch-worker.ps1 `
     -Repo . `
     -IssueNumber <number> `
     -IssueTitle "<title>" `
     -IssueBody "<body>" `
     -BaseBranch <base-branch> `
     -Provider codex
   ```

5. Verify each resulting PR.
   - For each worker-created PR, review as the Verifier. The worker model and verifier model differ on purpose.
   - Run `gh pr diff <pr>` and inspect whether the diff satisfies the issue and avoids unrelated changes.
   - Run `gh pr checks <pr>` and record whether required checks pass.
   - Report failures, risky changes, or missing acceptance coverage in chat. Do not silently merge or hide verifier concerns.

6. Report progress and final status.
   - Report meaningful state changes in chat: issues published, workers dispatched, PRs opened, checks passed/failed, verifier verdicts, blocked items, and unblocked dependents.
   - Continue until the DAG is drained or blocked.
   - End with a final summary listing issues, PRs, verifier status, check status, and any human decisions still needed.
   - When the user names PRs to merge, run `gh pr merge` for those PRs only, following `.delivery.yml` merge settings when present.

## Recovery Notes

- If a worker fails, mark that item blocked in the chat state, include the error or log path, and do not dispatch dependents.
- If the session is interrupted, re-derive issue and PR state from GitHub before continuing. The v1 conductor cannot reliably adopt orphaned background workers; see the spec's single-session limits.
