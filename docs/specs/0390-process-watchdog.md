---
id: 390
title: Process Watchdog
status: accepted
date: 2026-07-03
issue: 390
pr: null
supersedes: []
superseded_by: []
---

# Process Watchdog

This spec folds into loopcoder 0.4.0. It is the F4 failsafe hardening that lets
the autonomous delivery loop run unattended without a hung subprocess silently
freezing a tick or leaking an unsupervised process. Code slices are filed as
follow-on issues and land on the `feat/0.4.0` integration branch, per
[`docs/PROCESS.md`](../PROCESS.md).

## Goal

No CLI subprocess loopcoder spawns may hang forever. Every spawned process is
bounded by a hard wall-clock cap; the two long-running LLM roles (worker and
verifier) additionally get output-stall detection. A detected hang is killed and
routed into the existing recovery loop as a distinct `hung` outcome — retried
within the bounded attempt budget and, on exhaustion, escalated to `needs-human`.
A hang is never silently dropped.

## Background

The delivery loop already has most of the substrate but leaves the loop itself
unbounded at its most critical point:

- **Worker dispatch has no timeout.** `internal/cli/cli.go` dispatches the worker
  with `context.Background()`, so a provider CLI (`claude`/`codex`/`gemini`) that
  stalls inside `cmd.Run()` blocks the whole tick forever. Because a dispatch
  wave waits on every worker, one hung worker freezes the entire tick and there
  is no "next tick" for the existing passive hang detection to fire on.
- **The verifier is bounded but blind to stalls.** `internal/loopreview` applies
  a 600s `context.WithTimeout`, but has no detection for a process that is alive
  yet producing no output — the most common LLM CLI hang (connection stalled, no
  tokens flowing).
- **Other exec sites are caller-unbounded.** `git`, `gh`, `go list`, and the
  user-defined verify commands run through `exec.CommandContext` but inherit
  whatever context the caller passes; several callers pass `context.Background()`.
- **The liveness check itself is unbounded.** `internal/process/process_windows.go`
  spawns `tasklist` with `exec.Command` (no context).

Reusable substrate that this spec builds on rather than replaces:

- Per-worker `attempt.json` records (`internal/state`) with `Status`,
  `HeartbeatAt`, `LastProgressAt`, `PID`, `ExitCode`.
- Passive hang classification in `internal/orchestration/ready_set.go`
  (`StaleAfterSeconds`, `HungAfterSeconds`) plus PID liveness in
  `internal/process`, consumed by `resume` to recover orphaned attempts.
- The recovery loop (`internal/recovery`): bounded attempts, same-config then
  upgraded, then `needs-human`.
- The `MaxIterations` guardrail (`internal/orchestration/trigger.go`, invariant
  F4).

The active watchdog defined here covers the **parent-alive** failure mode (a hang
while loopcoder is still running the tick). The existing passive
resume/heartbeat path continues to cover the **parent-dead** orphan mode. The two
mechanisms are complementary and must not be merged.

## Decisions

### 1. Dual-signal detection

A subprocess is judged unresponsive by either signal:

1. **Output stall** — no new output for `StallTimeout`. Progress is measured by
   growth of the process's own log file (see Decision 4). Stall detection is
   enabled only for the LLM tier.
2. **Hard cap** — total wall-clock exceeds `HardCap`. Enforced for every
   supervised process.

The hard cap is the absolute backstop that guarantees "never hangs forever". The
stall signal catches the common alive-but-silent hang early, before the hard cap.

### 2. The `internal/supervisedexec` helper

A single new package owns bound-and-supervise for all subprocesses. Callers set
up the `*exec.Cmd` exactly as they do today (args, stdin, stdout/stderr, dir) and
replace `cmd.Run()` with `supervisedexec.Run(ctx, cmd, opts)`.

```go
package supervisedexec

type Outcome int

const (
    OutcomeCompleted Outcome = iota // process exited on its own (any exit code)
    OutcomeStalled                  // killed: no log growth for StallTimeout
    OutcomeDeadline                 // killed: exceeded HardCap
)

type Options struct {
    HardCap      time.Duration              // required backstop; 0 is normalized to a safe default, never unbounded
    StallTimeout time.Duration              // 0 disables stall detection (hard-cap-only tier)
    LogPath      string                     // file whose growth is the progress signal; required when StallTimeout > 0
    StallGrace   time.Duration              // optional: after first stall, warn and wait this long before killing
    OnStall      func(silentFor time.Duration) // optional: emit a progress/log event on first detected stall
}

type Result struct {
    Outcome  Outcome
    ExitCode int           // valid when OutcomeCompleted
    Killed   bool          // true for OutcomeStalled and OutcomeDeadline
    Elapsed  time.Duration
}

func Run(ctx context.Context, cmd *exec.Cmd, opts Options) (Result, error)
```

Run mechanics, in-process and race-free:

1. `cmd.Start()`; the helper holds the `cmd.Process` handle. It never scans the
   machine for PIDs.
2. It waits on `cmd.Wait()` in a goroutine, arms a `HardCap` timer, and — when
   `StallTimeout > 0` — a stall ticker that periodically stats `LogPath`
   (size and mtime).
3. First event wins: `Wait` returns first ⇒ `OutcomeCompleted`; hard cap fires ⇒
   `OutcomeDeadline`; stall fires ⇒ `OutcomeStalled` (after the optional
   `OnStall`/`StallGrace` soft step).
4. To kill, the helper calls `cmd.Process.Kill()` and then drains `Wait()`, so a
   process that exits concurrently with the kill decision cannot leave a zombie
   or a read-then-kill race.
5. The typed `Result` lets callers distinguish "exited on its own" from "stalled"
   from "hit the cap".

The passed `ctx` is still honored: a cancelled parent context also terminates the
process. `HardCap` and the stall signal are additional bounds, not replacements.

### 3. Two tiers, one function

The same `Run` serves both tiers via `Options`:

- **LLM tier** (worker's three agent runners and the verifier): `HardCap` +
  `StallTimeout` + `LogPath` pointing at the log file the runner already writes.
- **Hard-cap tier** (every other exec site): `HardCap` only (`StallTimeout = 0`).

Routing all spawn sites through this one helper structurally eliminates the
"caller passes `context.Background()` ⇒ unbounded" hole.

### 4. Stall signal is log-file growth

The progress signal is the size (and mtime) of the process's own log file, polled
on the stall ticker. All three provider runners already write stdout/stderr to a
log file (`codex` directly; `claude`/`gemini` via an `io.MultiWriter`), so the
signal is obtained without changing how output is captured and without any
cooperation from the subprocess. If the file has not grown within `StallTimeout`,
the process is stalled.

The stall threshold must be set generously (total silence, not slow throughput),
because a process legitimately mid-work may be briefly silent on stdout. A
process that is slow but still emitting output must not trip the stall signal;
only true silence does.

### 5. Hard cap is always non-zero

`supervisedexec.Run` normalizes a zero or unset `HardCap` to a safe non-zero
default rather than treating it as "unbounded". This is the structural guarantee,
implemented in exactly one place, that no supervised process can run forever —
regardless of what context a caller passes.

### 6. A hang feeds the existing recovery loop

A `Result` with `Outcome` of `OutcomeStalled` or `OutcomeDeadline` is surfaced by
the worker/verifier layer as a distinct `hung` failure classification, separate
from a normal non-zero exit, a `fail` verdict, and a `needs-human` verdict.

Recovery treats `hung` as a retryable failure with a targeted strategy:

- It **counts against the same bounded attempt budget** as other failures. There
  is no separate or unbounded hang-retry budget.
- It is **retried same-config**. A hang is an environmental condition (network,
  quota, provider stall), not a capability gap, so the retry does not upgrade the
  model or effort even on the attempt that would normally upgrade. This is the
  one behavioral difference from the `fail` path's same-then-upgraded ladder.
- On budget exhaustion it escalates to `needs-human`, ledgered with
  `reason=hung`.

A hung attempt is always recorded (in `attempt.json` and the run event ledger)
and surfaced by `report`; it is never silently dropped.

### 7. Per-project configurable thresholds

`HardCap` and `StallTimeout` are per-project configurable (via loopcoder config /
`.delivery.yml`) with baked defaults. Defaults:

| Site | Tier | HardCap default | StallTimeout |
| --- | --- | --- | --- |
| worker: `agent` codex/claude/gemini | LLM | 30m | 120s total silence |
| verifier: `loopreview` | LLM | 600s (unchanged) | 120s |
| `gitutil` git | hard-cap | 60s | — |
| `vcs/github` + `scaffold` gh | hard-cap | 60s | — |
| verify user commands (`.delivery.yml`) | hard-cap | 15m (most-tuned) | — |
| `doctor` diagnostics | hard-cap | 60s | — |
| `upgrade` skill refresh | hard-cap | 60s | — |
| `compile` `go list` | hard-cap | 120s | — |
| `process` liveness (`tasklist`) | hard-cap | 5s | — |

The verify user-command cap is the one operators most need to tune (large test
suites); its default is generous and per-project overridable.

### 8. Multi-instance safety by construction

Multiple loopcoder instances may run concurrently on one machine, one per
project. Supervision is per-invocation and in-process: each instance supervises
only the children it started, through the `cmd.Process` handle it holds. There is
no machine-wide PID scan, no central watchdog daemon, and no cross-instance shared
state. An instance can therefore never kill another project's process, and
per-project `.loopcoder/` state does not collide. This inherits the "single
project, stateless tick" property and requires no cross-instance coordination.

### 9. Orphan handling on parent death

If loopcoder exits while a child is mid-run, the in-process supervisor exits with
it, so the child could be orphaned without a watchdog. This is handled in two
layers:

- **Graceful cleanup:** loopcoder traps `SIGINT`/`SIGTERM` and kills its
  supervised children before exiting. This covers Ctrl-C and normal termination.
- **Passive resume:** on a hard crash, the existing resume/heartbeat path detects
  the orphaned `attempt.json` (status `running`, stale heartbeat) on the next run
  and routes it into recovery.

Both layers use the loopcoder-managed **kill-group** of Decision 11: graceful
cleanup terminates the whole group, so a child that spawned its own subtree — a
provider CLI with helper processes — is fully reaped rather than orphaned (the
exact failure that motivated this spec). The group's kill-on-close (Windows Job
Object) and `PR_SET_PDEATHSIG` (Linux) reap orphans even on a hard crash; macOS,
which has no death signal, still falls back to passive resume for crash orphans.
Passive resume remains the recovery path for the abandoned *work* on any platform.

### 10. Soft stall step

On the first detected stall, the supervisor fires `OnStall` (a progress/log
event, e.g. "worker N silent for Xs") and, if `StallGrace > 0`, waits that grace
before killing. This gives a short, bounded soft window before the kill; the hard
cap remains the absolute backstop regardless of the soft step.

### 11. Process ownership: loopcoder-managed marker and kill-group

loopcoder must identify and terminate its OWN spawned processes without ever
touching unrelated processes that share a binary name. On a real machine the
user's Claude and Codex **desktop apps**, other CLI sessions, and the loopcoder
**CLI session itself** all appear as `claude` / `codex` / `loopcoder` processes.
Killing by process name is therefore **forbidden** — it kills the user's apps and
can kill the controlling session itself.

Every process loopcoder spawns (the worker/verifier LLM CLIs and every exec-site
subprocess) is tagged and grouped at spawn:

- **Env marker:** `LOOPCODER_MANAGED=1`, `LOOPCODER_RUN_ID=<run>`, and
  `LOOPCODER_ROLE=worker|verifier|git|gh|...` are set on the child environment.
- **Kill-group:** the child is placed in a per-run kill-group — a named **Job
  Object** on Windows (`loopcoder/<run>`, `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`)
  and its own **process group** on Unix (`Setpgid`, plus `PR_SET_PDEATHSIG` on
  Linux). The group is the single handle that terminates the whole subtree at
  once and reaps orphans on parent close.

Two operator commands consume this:

- `loopcoder ps` — lists only loopcoder-managed processes (from the tracked
  attempt PIDs and the kill-groups); never unrelated `claude`/`codex` processes.
- `loopcoder kill [--run <id> | --all]` — terminates only loopcoder-managed
  processes, by group / PID-tree; never by bare process name.

This makes "identify and safely reap loopcoder's own processes" a first-class,
collision-free capability, and is the mechanism Decision 9's graceful cleanup and
crash-safe reaping are built on.

## Invariants

| Invariant | How this feature satisfies it |
| --- | --- |
| **F4 — guardrail bounds** (primary) | Hung-retry counts against the bounded attempt budget (no infinite kill-retry); `HardCap` normalized non-zero (no unbounded process); the watchdog is per-invocation, not a persistent daemon. |
| **No-silent-failure** | A hang that exhausts the budget becomes an explicit `needs-human`, ledgered with `reason=hung`. |
| **F1 / F2 / F3** | Unaffected. The watchdog never merges or promotes; the verifier stays ReadOnly and only its runtime is bounded. |
| **M — attestation** | A killed invocation has no completed attestation; this is represented as a `hung` run event, not treated as a crash or a malformed record. |
| **Multi-project (parked)** | Per-instance, in-process, stateless supervision, consistent with the single-project stateless tick. |

## Non-Goals

- Not a machine-wide resource scheduler or cross-instance concurrency throttle.
  Limiting how many LLM CLIs run at once across projects is the parked
  multi-project scheduler's job. The watchdog only detects and handles hangs.
- No central watchdog daemon and no global machine-wide PID scan.
- **Never terminate a process by bare name** (`claude`/`codex`/`loopcoder`) —
  those names collide with the user's desktop apps and the controlling CLI
  session. Only loopcoder-managed processes are terminated, by kill-group /
  PID-tree (Decision 11).
- No new runtime dependency; cross-platform Go only.
- No change to the human merge/promote gate.

## Follow-On Slices

Filed as code issues on `feat/0.4.0`. Order: **W1 → (W2 ∥ W3) → W4**.

1. **W1 — `internal/supervisedexec` package + unit tests.** The helper, both
   tiers, injectable clock/ticker so tests do not wait in real time, typed
   `Outcome`. Pure new package; no spawn-site changes. Depends on nothing.
2. **W2 — LLM-tier wiring + recovery `hung` mapping.** Route the three agent
   runners and the verifier through `supervisedexec.Run` with the LLM tier; map
   `Stalled`/`Deadline` into recovery as `hung` (same-config retry, counts
   against the budget, exhaust → `needs-human`). Depends on W1.
3. **W3 — hard-cap sweep.** Route `gitutil`, `vcs/github`, `scaffold`, `verify`,
   `doctor`, `upgrade`, `compile/ordering`, and `process` through the hard-cap
   tier; fix the `process_windows` no-context liveness spawn. Depends on W1;
   file-disjoint from W2, so it runs in parallel with W2.
4. **W4 — process ownership, cleanup, config, report.** Tag every spawned child
   (`LOOPCODER_MANAGED`/`LOOPCODER_RUN_ID`/`LOOPCODER_ROLE`) and place it in a
   per-run kill-group (Windows Job Object with kill-on-close; Unix process group
   + Linux `PR_SET_PDEATHSIG`); graceful-shutdown on `SIGINT`/`SIGTERM` kills the
   group; add `loopcoder ps` and `loopcoder kill [--run|--all]` acting only on
   loopcoder-managed processes (never by bare name); per-project
   `HardCap`/`StallTimeout` config with the Decision 7 defaults; `report`
   surfacing of `hung` attempts. Depends on W2.

Each slice is cross-platform Go, adds no runtime dependency, and preserves the
human merge gate.

## Acceptance Criteria For Implementation

- No supervised subprocess can run past its `HardCap`; a zero/unset cap is
  normalized to a non-zero default, never treated as unbounded.
- The worker is bounded: a stalled or over-cap worker CLI is killed rather than
  blocking the tick indefinitely.
- The LLM tier detects output stall (no log growth for `StallTimeout`) and kills;
  a process that keeps emitting output is not killed by the stall signal.
- A killed hang is classified `hung`, retried same-config within the bounded
  attempt budget, and escalates to `needs-human` on exhaustion — recorded in
  `attempt.json`, the ledger, and `report`, never silently dropped.
- Every other exec site (`git`, `gh`, `go list`, verify commands, doctor,
  upgrade, liveness check) runs under a hard cap; none is unbounded when the
  caller passes a background context.
- Thresholds are per-project configurable with the Decision 7 defaults.
- Concurrent loopcoder instances supervise only their own children; no instance
  can kill another project's process.
- Every spawned child carries `LOOPCODER_MANAGED`/`LOOPCODER_RUN_ID` and is
  placed in a per-run kill-group (Windows Job Object; Unix process group).
- On `SIGINT`/`SIGTERM`, loopcoder terminates its kill-group (the whole subtree,
  including a child's own helper processes) before exiting; on a hard crash,
  Windows Job Object kill-on-close and Linux `PR_SET_PDEATHSIG` reap orphans, and
  passive resume recovers the abandoned work.
- `loopcoder ps` / `loopcoder kill` act only on loopcoder-managed processes;
  neither ever targets a process by bare `claude`/`codex`/`loopcoder` name.
- Kill terminates the process and drains its wait with no zombie and no
  read-then-kill race; behavior holds on both Windows and Unix.
