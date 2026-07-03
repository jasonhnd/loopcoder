---
id: 408
title: Verifier stream-json output (stall-watchdog reliability)
status: accepted
date: 2026-07-03
issue: 408
pr: 409
supersedes: []
superseded_by: []
---

# Verifier stream-json output (stall-watchdog reliability)

This is a design-only spec. This PR adds only this document: no Go code and no
behavior change. Code follows after merge per [`docs/PROCESS.md`](../PROCESS.md).

This fixes a loopcoder 0.4.0 verifier RELIABILITY defect. It is orthogonal to the
0.4.1 / E2 auto-promote work and targets `main`.

## Root cause (diagnosed 2026-07-03)

`loopreview` runs the claude verifier via `--output-format json`
(`internal/agent/claude.go:35`). That mode is **non-streaming**: claude runs its
entire agentic loop (reasoning + read-only tool calls) internally and emits ONE
JSON object only at the very end; the process log stays silent for the whole run.

The `supervisedexec` stall watchdog (`internal/supervisedexec/supervisedexec.go`)
kills a provider whose log file shows no growth for `StallTimeout`
(`silentFor >= opts.StallTimeout`, `OutcomeStalled`; verifier default 120s). So any
review whose silent stretch exceeds `StallTimeout` is **false-killed** and mapped to
verdict `needs-human` ("hung").

Evidence:
- A doc-only PR review finished in 98s (< 120s) and passed.
- A code PR review genuinely needed 235s with silent gaps > 120s; it was killed at
  `StallTimeout` 120s AND 300s, and passed only when `StallTimeout` was raised to
  900s.
- Ruled out: tools/permissions (read-only `--allowedTools "Read Grep Glob"` executes
  a Read in ~12s with zero permission denials), model/rate-limits (both default opus
  and `claude-opus-4-8[1m]` answer a tiny prompt in ~11s), and diff size (549 lines).

The defect is a mismatch between a non-streaming provider mode and a
log-growth-based stall detector — not a claude, tool, model, or network problem.

## Fix

Make the claude provider **stream** so the log grows continuously during a healthy
review. The stall watchdog then observes real progress and still catches a genuinely
hung provider (one that emits nothing).

1. `BuildClaudeArgs` (`claude.go`): replace `--output-format json` with
   `--output-format stream-json`. Add `--verbose` if the installed claude CLI
   requires it for `stream-json` in `--print` mode (verify against the pinned CLI).
   Apply to BOTH the read-only (verifier) and write (worker) branches so every claude
   invocation streams.
2. Output parser (`parseClaudeSummary` / `parseClaudeInvocation`): consume the
   newline-delimited JSON event stream and extract the final `result` event
   (`type == "result"`), reading the same fields as today — `result`,
   `structured_output`, `usage`, `modelUsage`. Tolerate interleaved non-result events
   and a possibly-partial trailing line.

## Invariants preserved

- **F2 (verifier read-only).** The read-only argument set
  (`--safe-mode --no-session-persistence --allowedTools "Read Grep Glob"`) is
  unchanged; no write capability is added. Only the output-format and its parser
  change.
- **Attestation contract (F5).** `agent.Result` and the emitted `AttestationRecord`
  keep the same fields and semantics; only their extraction path changes from a
  single JSON object to the stream's `result` event.
- **Stall watchdog unchanged.** `supervisedexec` is NOT modified; it keeps killing on
  no-log-growth. Streaming simply gives it truthful progress, restoring its
  correctness (long healthy reviews live; genuinely silent hangs still die).
- **Self-hosting.** `internal/agent/claude.go` is loopcoder core; this change is a
  red-line item and takes effect only after a human rebuild and restart.

## Scope / non-goals

- Only `internal/agent/claude.go` (`BuildClaudeArgs`, `Run`, the two parsers) and
  `claude_test.go`.
- NOT a change to `supervisedexec`, the resilience config defaults, or any 0.4.1 / E2
  code. NOT a change to the codex or gemini providers (codex already streams via its
  own surface; gemini is out of scope here).

## Follow-up code issue (filed after this spec merges)

Implement per this spec: `BuildClaudeArgs` stream-json (+ `--verbose` if required),
`Run` stream consumption, `parseClaudeSummary` / `parseClaudeInvocation` result-event
extraction, and `claude_test.go` updates (argv assertion + stream-json parser tests,
including a genuinely-silent-hang case that still yields no output).

## Relationship to existing specs

- [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md) —
  the reliable verifier; this hardens it against the non-streaming stall false-kill.
- [`0390-process-watchdog.md`](0390-process-watchdog.md) — the process watchdog whose
  stall detector this aligns with streaming providers.
