---
id: 447
title: Relay-enforcement hard gate (verbatim worker/verifier relay made non-bypassable)
status: accepted
date: 2026-07-04
issue: 447
pr: 448
supersedes: []
superseded_by: []
---

# Relay-enforcement hard gate

This is a **design-only** spec for loopcoder **0.4.2**. This PR adds only this
document: no Go code, no `.delivery.yml` change, no command behavior change. Code
slices are filed only AFTER this spec merges, per [`docs/PROCESS.md`](../PROCESS.md).

It extends [`0316`](0316-conductor-local-enforcement.md) (conductor local
enforcement) and [`0146`](0146-attestation.md) (attestation), and belongs to the
0.4.2 operational-reliability train ([`0423`](0423-operational-reliability-hardening.md)):
it applies the same **fail-loud, honest-signal** principle to the relay guard itself.

## Problem

The Worker/Verifier local-only attestation relay (spec 0316) is only **softly**
enforced. SKILL.md instructs the conductor to relay each pretty attestation block
verbatim, and a `conductor-relay-guard` hook is supposed to backstop it. A real
delivery session bypassed **both** with no signal:

1. **The guard is blind to the PowerShell tool.** `conductorhooks.isShellTool`
   (`internal/conductorhooks/shared.go:345`) only matches `Bash`,
   `run_shell_command`, `shell_command`, and the installed PostToolUse matcher in
   `.claude/settings.json` is `"Bash"`. On Windows the conductor drives loopcoder
   through the **PowerShell** tool, so `handleToolComplete`
   (`internal/conductorhooks/relay_guard.go:108`) returns `allow()` immediately, no
   ledger record is ever written, `handleStop` finds nothing pending, and the Stop
   gate never fires.
2. **Backgrounded runs hide the block.** Running `loopcoder dispatch` / `loopreview`
   with `run_in_background` puts the pretty block in a captured output file, not in
   the tool response, so even a matching tool would not surface it to
   `containsSurfacedAttestation` (`relay_guard.go:470`).

Net: the conductor hand-summarized (or omitted) every Worker/Verifier block and
nothing caught it. **A guard that silently no-ops instead of failing loud** — the
exact bug class 0.4.2 hardens elsewhere.

## Principle

> Verbatim relay must be **hard, not advisory**, and enforcement must **fail loud**
> when it cannot verify surfacing — but it must **never lock out** a valid run.

The conductor is an LLM in some host (Claude Code, Codex, Gemini). A Go binary can
enforce **its own** behavior and **gate all mechanical progress**; it cannot reach
into the host and force the LLM to type into the user-visible channel — only a
host-side hook can observe what the LLM actually emitted. So full enforcement is
**layered**: a host-agnostic binary gate that halts progress and re-emits, plus a
host hook that verifies actual surfacing.

## Design (three layers)

### L1 — Go cross-command hard gate (host-agnostic, load-bearing)

`loopcoder dispatch` and `loopcoder loopreview` already write a local ledger under
`.loopcoder/relay/*.attest`. Add a **pending relay** record (nonce + role + PR +
the verbatim pretty block). Introduce a small `relaygate` package:

- Every **mechanical** subcommand — `dispatch`, `dispatch-wave`, `loopreview`,
  `ready-set`, `status`, `verify-local`, `recover`, and the promote/merge helpers —
  calls `relaygate.Check(cwd)` at startup. While an **unacknowledged** pending relay
  exists, the command **exits non-zero with a reserved exit code** after printing the
  pending block(s) verbatim to stdout plus how to clear them.
- `loopcoder relay flush` prints **all** pending blocks verbatim to stdout and clears
  them — the single sanctioned surfacing point. `loopcoder relay list` shows pending
  without clearing.

Effect: the conductor cannot dispatch the next slice, review, check status, or merge
until it has run a command whose stdout **is** the pending block. This is independent
of host, tool type, and background execution — it closes both holes above by
construction.

### L2 — Host surfacing verification (Claude Code hook)

Fix `conductor-relay-guard` so it actually fires and verifies the block reached the
visible transcript:

- `isShellTool` and the installed `.claude/settings.json` PostToolUse matcher must
  recognize **PowerShell** and **pwsh** (and keep Bash / `run_shell_command` /
  `shell_command`).
- A dispatch/loopreview whose tool response indicates **background execution** is
  recorded `pending` and stays pending until a later turn surfaces the block; the
  Stop hook still blocks + prints pending blocks verbatim, honoring the
  `stop_hook_active` escape valve.

L2 catches a conductor that runs `relay flush` in the background and then
summarizes: the block never appears in the visible transcript, so the Stop gate
blocks.

### L3 — Remove the incentive to background

Provide a **foreground streaming** `dispatch-wave` that runs workers concurrently but
streams each worker's pretty attestation block to stdout **synchronously as it
completes**, interleaved with its result. Parallelism no longer requires
`run_in_background`, so the blocks stay in visible output in the first place.

## Invariants

- **R1** — Every mechanical subcommand refuses to start while an unacknowledged
  pending relay exists (fail-closed **for mechanical progress only**), printing the
  pending block(s) verbatim + clear instructions and exiting with the reserved code.
- **R2** — `loopcoder relay flush` prints all pending blocks verbatim to stdout and
  clears them; `loopcoder relay list` shows pending without clearing.
- **R3** — `dispatch` / `loopreview` write the pending relay atomically (nonce, role,
  PR, verbatim block) to the gitignored `.loopcoder` ledger.
- **R4 (NO LOCKOUT)** — The gate exempts the commands needed to clear or inspect it
  (`relay flush`, `relay list`, `doctor`, `attest`, `--help`, `--version`) and
  **fails open** on any gate-state read error. A valid run must never be permanently
  refused; `relay flush` always succeeds. Works headless / in CI.
- **R5** — `conductor-relay-guard` covers PowerShell/pwsh + Bash and background runs;
  the Stop hook blocks + prints pending blocks verbatim, honoring `stop_hook_active`.
- **R6** — `dispatch-wave` can stream each worker pretty block to stdout synchronously
  (foreground parallel) with no `run_in_background`.
- **R7** — Blocks remain **local-only** (spec 0316 unchanged): never copied into PR
  bodies, issues, comments, commits, merge artifacts, docs, fixtures, or any tracked
  file. `relay flush` writes only to stdout.

## Non-goals

- **Cannot** force an adversarial host LLM to type into the user channel with zero
  host cooperation. L1 halts all mechanical progress and re-emits the block at a
  controlled point; L2 verifies surfacing on Claude Code. On hosts without hooks,
  L1 + the manual-honesty note in SKILL.md are the floor. This ceiling is documented
  honestly, not papered over.
- No change to attestation **content** or verification (spec 0146), nor to the
  local-only rule (spec 0316), beyond enforcement.
- The gate is on `loopcoder` subcommands, not on the host session; it never blocks the
  user from running `relay flush` to recover.

## Seams (current code)

- Ledger + relay state: `internal/conductorhooks/relay_guard.go` (discovery,
  `handleToolComplete:108`, `handleStop:179`, `containsSurfacedAttestation:470`),
  `.loopcoder/relay/*.attest` writers in the dispatch/loopreview paths.
- Tool/scope gates to fix: `internal/conductorhooks/shared.go:345` (`isShellTool`),
  `.claude/settings.json` PostToolUse `matcher`, `hooks/claude-settings.snippet.json`,
  `internal/claudehooks/settings.go`.
- Subcommand entry points to gate: `internal/cli/cli.go` (`runDispatch`,
  `runDispatchWave`, `runLoopreview`, `runReadySet`, `runStatus`, `runVerifyLocal`,
  `runRecover`, `runPromote`), plus a new `relay` command group.
- Reserved exit code: extend the H5 exit-code scheme in
  `internal/loopreview/loopreview.go` / `internal/cli/cli.go` with a distinct
  relay-gate code (do not collide with 0/1/2/3).
- Wave streaming: `internal/orchestration/dispatch_wave.go` + `runDispatchWave`.

## Slices

- **S1** — this spec (doc-only).
- **S2 (L1)** — `relaygate` package: pending-relay writers, `Check()` wired into all
  mechanical subcommands, `relay flush` / `relay list`, reserved exit code. Requires
  an **adversarial security review** — lockout/deadlock is the primary risk (R4).
- **S3 (L2)** — `conductor-relay-guard` PowerShell/pwsh + background coverage;
  settings matcher; tests for the tool-name and background paths.
- **S4 (L3)** — foreground streaming `dispatch-wave`; docs (SKILL.md relay section,
  README, usage) + CHANGELOG for the relay-enforcement feature.

## Testing

- L1: gate blocks every mechanical subcommand while pending; exempt commands run;
  `relay flush` prints verbatim + clears; `relay list` non-destructive; **no-lockout**
  cases (missing/corrupt state → fail open; flush always works; headless); reserved
  exit code asserted.
- L2: PowerShell/pwsh tool names recorded; background dispatch/loopreview stays
  pending until surfaced; Stop blocks + prints; `stop_hook_active` escape honored.
- L3: wave streams each block to stdout as workers finish; order deterministic; no
  `run_in_background` needed; blocks stay local-only.
