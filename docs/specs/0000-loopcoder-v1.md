---
id: 0000
title: loopcoder v1 - Design Spec
status: accepted
date: 2026-06-26
issue: null
pr: null
supersedes: []
superseded_by: []
---

# loopcoder v1 — Design Spec

Status: DRAFT (approved; building M1). Date: 2026-06-26.
Relationship to [`DESIGN.md`](../../DESIGN.md): `DESIGN.md` is the north-star vision (autonomous, cloud-capable delivery engine). **This spec scopes that down to a buildable first version** and locks the architecture so generality can be added later without a rewrite.

## 1. Problem
Today the delivery loop is run by a human relaying between tools:
1. chat with Opus → Opus drafts issues → **human posts them to GitHub**
2. **human triggers** a coding agent (Codex) to pick up issues
3. Codex branches → implements → opens a PR
4. **human copies the PR/result back to Opus** for review
5. human sometimes opens the site to verify

The pain is **context-switching and copy-paste relay** across Opus / GitHub / Codex / browser.

## 2. Goal (v1)
One conversational interface (the Opus session) drives the whole loop and reports back:

> You state a need → Opus drafts issues + a dependency plan → you approve → loopcoder publishes the issues, dispatches a **background** worker per ready issue (parallel where independent, serial where dependent), each worker branches → implements → opens a PR; Opus reviews each PR and streams **progress + a final summary** into the same chat. **You can step away and come back — you don't have to watch every second.** When you say which PRs to merge, Opus merges them for you via `gh` — you never leave the chat.

## 3. Shape
loopcoder is **not** a daemon. v1 is three things:
1. **Conductor** — a Claude Code **skill** (a playbook) the Opus session follows. The session **is** the runtime and the state machine. It **must stay open while the loop runs**, but workers run in the **background** and Opus reports progress + a final summary — you needn't stare at it.
2. **Helper scripts** — thin PowerShell/git glue for steps the session can't do as plain reasoning (worktree, the worker CLI, `gh`).
3. **Spec format** — `ROADMAP.md` + `.delivery.yml`, declaring what to build and which adapters/policies to use.

Invocation: the user types `/loopcoder <need>`, **or** just states a delivery-type need and the skill auto-activates (a repo `CLAUDE.md` line can make it the default so the command is never required). The smart parts (drafting issues, review, reporting) run in Opus; the mechanical parts (worktree, worker CLI, PR, merge) run in helper scripts / `gh` that Opus calls.

Architecture = **ports & adapters**. The loop core is generic; every varying concern is a **port** (stable interface) with **one adapter** in v1. The **Worker port is the deliberate exception**: it is designed for multiple provider adapters, because swapping the implementing LLM is a primary requirement.

## 4. The loop (core — adapter-agnostic)
```
intake(need)
  → plan: draft WorkItems + dependency DAG   → human approves
  → publish WorkItems
  → loop until DAG drained or blocked:
       ready = items whose deps are all done
       dispatch each ready item as a BACKGROUND worker (parallel if independent):
          ws      = Workspace.create(item)
          result  = Worker.implement(item, ws)        // Worker.provider is selectable
          change  = VcsHost.openChange(ws, item)
       on worker finish:
          verdict = Verifier.verify(change, item)      // different model than Worker
          Gate.decide(change, verdict)                 // v1: report to chat
          mark item done/blocked; unblock dependents
          Reporter.report(item, result, verdict)       // progress into the chat
  → Reporter.report(final summary)
  → on user instruction: VcsHost.merge(named PRs)       // gh pr merge, from the chat
```
The core knows nothing about GitHub, Codex, or git — only the port interfaces below.

## 5. Ports (stable interfaces) and v1 adapters
| Port | Contract | v1 adapter |
|---|---|---|
| **WorkItemSource** | `list()` / `create()` / `setStatus()` over WorkItem | GitHub issues via `gh` |
| **Workspace** | `create(item, baseBranch) -> {path,branch}`; `cleanup()` | `git worktree` (branch off `main`) |
| **Worker** | `implement(item, ws, provider) -> {ok, summary, changedFiles[]}` | **`codex exec`** via PowerShell, run in the **background** (v1). Provider-pluggable: M3 adds `gemini`/`claude`/`openai` as **direct-CLI adapters**. Not through a runtime-bound worker agent. |
| **VcsHost** | `openChange(ws,item) -> {pr,url}`; `checks(pr) -> status`; `merge(pr)` | `gh pr create` / `gh pr checks` / `gh pr merge` |
| **Verifier** | `verify(change,item) -> {pass, notes, needsHuman}` | Opus reads diff + checks — **different model than Worker** |
| **Gate** | `decide(change,verdict) -> merge \| report \| escalate` | **report to chat; the human names PRs to merge and Opus runs `gh pr merge` for them — no leaving the chat, never auto-merges without the user's word** |
| **Reporter** | `report(event)` | this Opus chat (progress + final summary) |

`WorkItem = { id, title, body, deps[], status, labels[] }`. Conductor/runtime = the Opus session.

## 6. Spec format (`.delivery.yml` + `ROADMAP.md`)
```yaml
version: 1
adapters:
  work_items: github
  workspace:  git-worktree
  worker:     codex      # provider-pluggable; v1 only 'codex'; M3: gemini|claude|openai (direct-CLI)
  vcs:        github
  verifier:   opus       # review model — kept distinct from worker → model-diverse review
  gate:       human-merge # never auto-merge; merge-on-instruction via gh, from the chat
worker:
  base_branch: main
  command_hint: "implement the issue, run <ci>, commit"
ci:
  checks: []
report:
  channel: chat
```
`ROADMAP.md` = human-written work units Opus compiles into WorkItems + a DAG.

## 7. Dependency model (parallel / serial)
- Opus proposes the DAG at **plan time** (which run parallel, which are `blocked-by` which); human approves before publishing.
- Stored as `deps[]`; v1 GitHub adapter = `blocked-by:#N` label + in-memory DAG.
- An item dispatches only when **all** its deps are `done`.

## 8. State, and the single-session ceiling (honest limits)
- Source of truth = GitHub (issues, PRs, checks) + the session's in-memory DAG.
- Because the conductor **is one chat session**, v1 is sized for **small batches** — a handful of issues, short-to-medium tasks. This is fine for the MVP and everyday "a few issues" use.
- **It does not scale to large unattended roadmaps:** one session has a finite context budget (review + progress eat it) and is not a robust long-runner. If the session ends mid-run, in-flight background workers are **orphaned**; on restart the conductor re-derives the ready set from GitHub labels but does not adopt the orphaned workers (may need a manual cleanup).
- The fix for scale is to bring back a **background / stateless conductor** (cron or event-driven, fresh context per tick) — exactly `DESIGN.md` §5b/§12. That is **v2**, not v1. v1 deliberately trades scale for "no new runtime."

## 9. Build milestones
- **M1 — thin full loop (build & try FIRST):** a small but **complete** loop on **2–3 issues with one serial dependency** (e.g., a docs issue in parallel with a code issue, plus a third blocked-by the code one) → approve → publish → background `codex` workers (parallel where independent) → PRs → Opus review → progress + final report → merge-on-instruction. **This is the smallest thing that demonstrates the actual desired workflow** (parallel + serial + all-done report), not a bare single issue.
- **M2:** scale & robustness — larger batches, better in-flight/orphan handling, firmer state re-derivation.
- **M3:** multi-provider Worker (`gemini`/`claude`/`openai` direct-CLI adapters by config) + Gate policies (the "open the site to verify" visual/human gate; richer Verifier).

## 10. Non-goals (v1 — YAGNI)
- No cron/daemon, no runtime-specific worker-agent dependency, no auto-merge, no phone approval, no risk-policy engine.
- **No per-issue model routing**, **no ensemble**.
- One adapter per port — **except the Worker port** (multi-provider, but only `codex` in v1).
- No support for large unattended roadmaps (see §8 — that is v2).

## 11. Locked decisions
1. Runtime = the Opus session. It stays open while the loop runs, but **workers run in the background** and you get progress + a final report — you can step away (lightweight async, not "watch every second").
2. Worker v1 = `codex` CLI (`codex exec` via PowerShell; `codex` is on the Windows PATH, not Git-Bash PATH).
3. **No auto-merge** — Opus opens + reviews PRs and **merges only the ones you name, via `gh pr merge`, from the chat** (you never open GitHub to merge).
4. The dependency DAG is proposed by Opus at approval time, not inferred silently.
5. Ports defined now (choice **B**); one adapter per port, **except Worker**.
6. **Multi-LLM = Worker-port concern:** provider config-selectable; v1 ships `codex` only; `gemini`/`claude`/`openai` are added as **direct-CLI** adapters in M3 — not through a runtime-bound worker agent.
7. **Model-diverse review** via the Worker ≠ Verifier split; both roles' providers are selectable.
8. Invocation = `/loopcoder` **or** auto-activation; a repo `CLAUDE.md` line can make it the default.
9. v1 is **small-batch / single-session**; large unattended roadmaps wait for a v2 background conductor (§8).

## 12. Open questions (resolve while building M1)
- **(first)** Exact `codex exec` flags for non-interactive, auto-approved runs scoped to a worktree directory that can edit files, run commands, and commit — verify with `codex exec --help` as the very first build spike. The whole loop hinges on this.
- How the session runs + watches background workers (`run_in_background` + collect on finish) and how parallel workers stream progress without flooding the chat.
- Which provider CLIs are installed for M3 (verify `gemini` / `openai` / `claude` headless invocation).
- Minimal `ROADMAP.md` / `.delivery.yml` field sets.
