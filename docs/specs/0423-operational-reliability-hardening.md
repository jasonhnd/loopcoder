---
id: 423
title: Operational reliability hardening (harvest-before-discard + honest signals)
status: draft
date: 2026-07-03
issue: 423
pr: null
supersedes: []
superseded_by: []
---

# Operational reliability hardening (harvest-before-discard + honest signals)

This is a design-only spec for loopcoder **0.4.2**. This PR adds only this document:
no Go code, no `.delivery.yml` change, no command behavior change. Code slices are
filed only AFTER this spec merges, per [`docs/PROCESS.md`](../PROCESS.md).

0.4.2 is a **reliability-hardening** release that closes the remaining bugs from the
real-session bug report #407. It is orthogonal to the E2 auto-promote work (0.4.1)
and does NOT change the delivery model or any failsafe. Built spec-first and shipped
**human-gated** (like 0.4.1). Bug 3 of #407 (the claude verifier `--output-format
json` stall false-kill) was already fixed in 0.4.1 by spec
[`0408`](0408-verifier-stream-json.md); this spec covers #407 Bugs 1, 2, 4, 5, 6.

## Principle

Every bug in #407 is one failure: **a crude proxy signal produced a wrong or
destructive outcome.** 0.4.2 establishes one operating principle and applies it to
five seams:

> loopcoder must fail **loud** and **non-destructive**, and its control signals must
> reflect **real progress / intent**, not a crude proxy.

The load-bearing insight, confirmed against the #407 evidence: the reaped worker had
already **finished** its implementation (worktree complete; last source write minutes
before the kill) and was running a legitimately-silent smoke test when it was killed.
So the *critical* fix is not smarter hang-detection — it is making a kill
**non-destructive** (harvest). Better liveness is a quality improvement layered on top;
harvest is the safety net for anything liveness still mis-classifies.

## H1 — Harvest before discard (Bug 1; the load-bearing fix)

Today, when a worker is reaped as hung/stall, its scratch worktree is preserved on
disk and a recovery brief is written, but dispatch exits 1 with **no commit, no push,
no PR** — a finished deliverable is discarded (`worker.go:297-316` hung return;
`worker.go:196-248` defer preserves scratch; the commit/push/PR at `worker.go:343-373`
is never reached).

Normative:

- On a hung/stall kill, BEFORE returning, loopcoder MUST check the preserved worktree
  for committable changes (`git.StatusPorcelain(worktreePath)`). If there are none,
  behave as today (report hung; nothing to harvest).
- If there ARE committable changes, loopcoder MUST **harvest**: reuse the existing
  success sequence (`AddAll` → `Commit` → `PushUpstream` → `github.CreatePR`,
  `worker.go:343-373`) to commit the work and open a PR.
- The harvested PR MUST be flagged **needs-human** and clearly labelled as harvested
  from a hung/killed worker and **possibly incomplete**. Its body carries the recovery
  brief (changed files + log tail) and the hung worker's partial attestation. It MUST
  reference the issue with **`Refs #<n>` / `Part of #<n>`, never `Closes #<n>`** (a
  harvested, unverified deliverable must not auto-close the issue).
- The harvest commit is a **conductor** action (the worker was hung and never
  completed): its `AttestationRecord.Role` MUST be `conductor`, not `worker`. The hung
  worker's attestation (hung exit, no usage) is surfaced in the PR body, not asserted
  as a completed worker attestation.
- **Retry suppression + idempotency (reuse existing gates, no new mechanism):** the
  harvest branch MUST be named on the existing retry pattern
  (`loop/issue-<n>-retry-<attempt>`) so the existing idempotency gates —
  `recovery.findOpenIssuePR` (`recovery.go:298-333`) and the ready-set
  `candidateBranches` (`ready_set.go:524-542`) — detect the open PR and **adopt**
  rather than re-dispatch. loopcoder MUST also write a `state.AttemptRecord` with the
  harvest branch and `Status: "needs-human"` so `localAttemptDisposition`
  (`ready_set.go:622-700`) classifies the issue as recovery-needed and blocks
  re-dispatch. A repeat harvest for an issue that already has an open harvested PR MUST
  be a no-op.
- Harvest is a `needs-human` PR: it is NEVER auto-merged. The risk gate / human decides
  — consistent with the delivery gate. This preserves the safety model.

## H2 — Honest worker liveness (Bug 2; quality, cross-platform)

Correcting a misconception in #407: the stall watchdog DOES sample mid-run — it polls
`os.Stat(LogPath)` (size + mtime) on a ticker every `StallTimeout/4`
(`supervisedexec.go:110-137`). The `log_bytes: 0` observation in the report is a
SEPARATE system — the events ledger snapshots `log_bytes` only at phase transitions
(`worker.go:615-656`), not the kill decision. The kill decision's only signal is
**log-file growth**. A legitimately-silent phase (headless browser test, long link)
produces no log growth → false stall.

Normative:

- The worker stall watchdog MUST treat **worktree file activity** as a liveness signal
  in addition to log growth: within the stall window, if the worktree
  (`Invocation.WorktreePath`, already available at `worker.go:170`) has any file whose
  mtime advanced, the worker is making progress and MUST NOT be reaped. This requires
  adding `WorktreePath` to `supervisedexec.Options` and stat-ing it alongside `LogPath`
  (cross-platform, `os.Stat`).
- The default hung/stall windows MUST be raised to be commensurate with a real
  build + test cycle (the current worker `stall_timeout` 120s is far shorter than an
  `astro build` + browser smoke test). Exact values are a slice-time decision; the
  windows stay **modest** because worktree-mtime + harvest cover the tail — not a huge
  window that makes a genuine hang waste minutes.
- **Out of scope by decision:** process-tree CPU sampling. A purely CPU-bound,
  log-silent, worktree-silent phase (e.g. a headless browser test that writes nothing
  to the worktree) may still be reaped; **H1 (harvest) makes that non-destructive** —
  the deliverable is salvaged, at the cost of one wasted attempt. CPU liveness (feasible
  on Windows via the already-owned Job Object accounting API) is left as a future
  enhancement, not required here.
- The same `supervisedexec` path serves worker and verifier; this liveness addition
  benefits both. (0.4.1's `stream-json` fix already keeps the verifier's log growing;
  H2 is the general, provider-agnostic signal.)

## H3 — Source-first review packet (Bug 4)

The bounded review packet is built by consuming `gh pr diff` output in its native
(path-alphabetical) order (`buildDiffSection`, `loopreview.go:736`; `splitDiffPatches`,
`:779`). A large generated file early in the alphabet (e.g. `tests/baseline/*.jsonl`,
~1.6 MB) exhausts the 80 KB global diff budget (`loopreview.go:38-44`) before any
`internal/`/`cmd/` source patch is admitted → the actual code under review is dropped
into `omittedFiles` → a false `needs-human`.

Normative:

- The packet builder MUST include **non-generated source/config diffs first**, at the
  full per-file budget, before any generated file consumes the global budget. Generated
  files are admitted last at a **minimal** cap and MUST be **noted** (name + omitted
  size via the existing `[TRUNCATED diff for X: ...]` marker), never silently evicted.
- "Generated" is classified by: configurable path globs with sensible defaults
  (`tests/baseline/**`, `*.lock`, `dist/**`, `*.min.*`, `vendor/**`), the repo's
  `.gitattributes` `linguist-generated` / `linguist-diff` markers, and a size threshold.
  Classification MUST be conservative (only clearly-generated) so a large legitimate
  source file is never mis-classified.
- Seam: add `GeneratedPatterns []string` (+ a minimal generated per-file cap) to
  `ReviewPacketLimits` (`loopreview.go:106`, already injected via `Deps`) and reorder in
  `buildDiffSection` after `splitDiffPatches`. Parse `.gitattributes` once in
  `gatherInputs` (`:870`), not in the hot packet-build path. `splitDiffPatches` is
  unchanged — only its output is reordered.

## H4 — Loud config resolution (Bug 5)

`.delivery.yml` is read only from the `--repo` working tree; a checkout of a branch
lacking it silently falls back to `config.Default()` (`loadDeliveryConfig`,
`cli.go:2547-2557`; `config.Load`, `config.go:215`). Consequence: dispatch/tick/recover
silently run on defaults (`base_branch=main`, `ci.checks=[]`, `resilience=120/300`) —
the very short windows that caused Bug 1. **Depth catch:** there are TWO independent
silent loaders — `loadDeliveryConfig` AND `ResilienceForRepo` (`config.go:240`), and the
latter is the one `loopreview.Run` uses for the verifier stall timeout (the direct cause
of #407). Both MUST be fixed; fixing one leaves the bug.

Normative:

- When the `--repo` working tree LACKS `.delivery.yml` but the `--base-branch` HAS one
  (checkable via `git show <base>:.delivery.yml`), loopcoder MUST **fail loud**: refuse
  to run silently on defaults (dispatch/tick/recover/loopreview error out with a clear
  message naming the mismatch — "probably the wrong branch; checkout the base or pass
  `--config-from-base`"), rather than silently defaulting. `loopcoder doctor` MUST report
  the mismatch explicitly ("absent from working tree but present on `<base>`"),
  superseding the generic "absent; documented defaults apply" message
  (`doctor.go:326-345`).
- An opt-in flag (`--config-from-base`) MUST let the operator read `.delivery.yml` from
  the base branch (`git show <base>:.delivery.yml`) when the working tree lacks it.
- BOTH loaders (`loadDeliveryConfig` and `ResilienceForRepo`) and `doctor`'s check MUST
  honour this behavior; the base branch is already known in the dispatch/loopreview/tick
  flows.
- Genuine "no `.delivery.yml` anywhere" is not an error — it warns and uses documented
  defaults, as today. Only the CWD-vs-base **mismatch** is a hard stop.

## H5 — Distinguishable exit codes (Bug 6)

`loopreview` maps verdicts to exit codes `pass=0 / fail=1 / needs-human=2`
(`ExitCodeForVerdict`, `loopreview.go:1209`), but the CLI also returns `1` on a
command error (`cli.go:3121`, colliding with `fail`) and `2` on a flag/repo error
(colliding with `needs-human`). A CI runner cannot tell a verdict from a crash.

Normative:

- Exit codes `0/1/2` MUST be reserved for clean VERDICTS only (pass/fail/needs-human).
- "The command itself failed" (provider error, git error, panic-recovered error, bad
  flags, bad `--repo`) MUST use a distinct reserved code (e.g. `3` for a runtime command
  failure; a usage/flag error MAY use its own code). The exit-code map MUST be documented
  (README / command help), so a runner distinguishes "verifier said needs-human" from
  "loopreview crashed."

## Invariants preserved

- **Self-hosting:** every change here is loopcoder core (`internal/supervisedexec`,
  `internal/worker`, `internal/loopreview`, `internal/config`, `internal/cli`); each
  takes effect only after a human rebuild + tick restart, and the red-line/self-hosting
  guard is unchanged.
- **Delivery model / E2 unaffected:** no change to tick / promote / the gate / auto-
  rollback. Harvest opens a **needs-human** PR — never auto-merged — so it is consistent
  with the risk gate and the human/auto promotion model (0.4.1). F1–F5 unchanged.
- **Additive & byte-stable:** new `state.AttemptRecord` usage, new
  `supervisedexec.Options.WorktreePath`, and new `ReviewPacketLimits.GeneratedPatterns`
  are additive; no durable schema is broken.

## Follow-up code slices (filed after this spec merges, in dependency order)

1. **H1 — harvest-before-discard** (worker): the load-bearing fix. Reuse the success
   commit/push/PR seam; needs-human PR (`Refs #n`); conductor attestation; retry
   suppression via retry-branch naming + a needs-human attempt record; idempotent.
2. **H2 — honest liveness** (supervisedexec): add `WorktreePath` + worktree-mtime as a
   liveness signal; raise the default hung/stall windows (modest). No CPU.
3. **H3 — source-first packet** (loopreview): `ReviewPacketLimits.GeneratedPatterns` +
   reorder source-first / generated-last-and-noted; `.gitattributes` parsed once.
4. **H4 — loud config** (config + cli + doctor): fix BOTH loaders; fail loud on the
   CWD-vs-base mismatch; `--config-from-base` opt-in; doctor message.
5. **H5 — distinguishable exit codes** (loopreview + cli): reserve a command-failure
   code; document the map.
6. **docs + CHANGELOG** for 0.4.2.

Dependency note: H1 (harvest) is independent and highest-value — it can land first and
alone removes the data-loss risk. H2 reduces the false-kills that make H1 necessary. H3,
H4, H5 are independent.

## Relationship to existing specs

- Extends [`0390-process-watchdog.md`](0390-process-watchdog.md) — the watchdog whose
  stall/kill behavior H1 (harvest) and H2 (liveness) refine (additively; the kill-group /
  `LOOPCODER_MANAGED` machinery is unchanged).
- Extends [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md)
  — the reliable verifier; H3 (packet) and H5 (exit codes) harden it.
- Follows [`0408-verifier-stream-json.md`](0408-verifier-stream-json.md) — 0.4.1's
  verifier-stall fix; H2 is the general, provider-agnostic version of the same signal
  problem.
- Source: real-session bug report #407 (Bugs 1, 2, 4, 5, 6; Bug 3 fixed by 0408).
