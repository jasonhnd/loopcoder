---
title: Host progress visibility contracts
status: normative for v0.8.1
---

# Host progress visibility contracts

LoopCoder is host-neutral. Progress visibility is based on **negotiated
transport and observed delivery**, not on a provider model composing prose.

Active progress delivery **never** requires a model or provider call. Durable
generation uses supervisor state only (`progress.Emitter` /
`progress.Supervisor`).

## Profiles

| Profile | Detection (examples) | Foreground active sink | Detached / reconnect | Strict JSON |
|---------|----------------------|------------------------|----------------------|-------------|
| `codex-cli` | `LOOPCODER_HOST=codex-cli`, `CODEX_THREAD_ID` | terminal-human on stderr | outbox + `status --receipts` / `attach`; host-run-origin replay when thread binds | progress never writes command stdout |
| `claude-code` | `LOOPCODER_HOST=claude-code`, `CLAUDE_CODE_SESSION_ID` | terminal-human on stderr | outbox + attach/status; host-run-origin when session binds | same |
| `paseo-style` | `LOOPCODER_HOST=paseo`, `PASEO_AGENT_ID` | terminal-human on stderr | outbox + attach/status; host-run-origin when agent binds | same; core has no Paseo UI dependency |
| `generic-cli` | default / unknown | terminal-human on stderr; optional JSONL via `LOOPCODER_PROGRESS_JSONL=1` with a dedicated event writer | outbox + attach/status only | same |

Unknown hosts **always** degrade to `generic-cli` and never claim host-callback
or host-run-origin.

## Negotiation order

`progress.NegotiateSink` / `progresshost.NegotiateActiveSink`:

1. host-callback (only when an explicit deliver function is supplied **and** the host is not generic)
2. JSONL event stream (when preferred and EventWriter is set)
3. terminal-human (stderr / warnings)
4. outbox-only (durable path; Deliver is a no-op)

Host detection **cannot** grant repository write, orchestrate permission, or
provider-native delegation.

## Observable five-minute receipt

For a registered project, Worker dispatch starts a progress emitter that:

1. persists receipts at least every five minutes while the attempt is active;
2. delivers each receipt through the negotiated sink (typically a one-line
   stderr summary: phase, next, blocker, sequence);
3. always enqueues a durable delivery obligation for `status` / `attach` / host
   replay.

Users watching a foreground terminal see active lines **without polling**.
Detached runs require `attach` or `status --receipts` (documented degradation).

## Related

- [`progress-receipts.md`](progress-receipts.md) — durable record and outbox
- [`v0.8.0-capability-matrix.md`](v0.8.0-capability-matrix.md) — shipped status
