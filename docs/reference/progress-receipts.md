# Progress Receipts

`ProgressReceipt` is the v0.8 provider-neutral progress record. It is persisted
in project-scoped SQLite schema v25 as an append-only, redacted
`loopcoder.progress_receipt.v1` payload plus query columns for run, task,
attempt, correlation, phase, status, provider, model, heartbeat age, and
progress age.

This document describes the durable record, outbox, and transport contracts.
It does not claim that every v0.8.0 command wires them into active work or that
the initiating host receives unsolicited progress. The binding shipped status
is in the
[`v0.8.0 capability and support matrix`](v0.8.0-capability-matrix.md).

Duplicate writes with the same project, delivery run, correlation ID, and
semantic fingerprint are idempotent. Distinct phase, status, count, evidence,
quota/budget, blocker, next-action, heartbeat, or progress transitions produce
new rows. Replay is deterministic by `occurred_at`, `correlation_id`,
`correlation_sequence`, and the SQLite `storage_order` tie-breaker.

LoopCoder-owned supervisors emit receipts through the progress emitter without
calling provider/model adapters. Consequential supervisor observations emit
immediately, and active runs default to a five-minute maximum receipt-generation
silence interval. The interval is configurable only within one second and one
hour; values outside that range are rejected. Periodic receipts describe only
known supervisor state, such as alive with no meaningful progress, waiting for
CI, waiting for approval, quota blocked, fallback in progress, host offline, or
delivery pending. A generated receipt is not evidence of worker progress, host
delivery, user visibility, delivery acknowledgment, or renewal of any lease,
claim, reservation, budget, quota window, or stall watchdog deadline.

In v0.8.0, durable inspection through `status` and `attach` is available, but
the active delivery path lacks exact-artifact proof of an attached progress
sink and host visibility. The five-minute policy and persistence mechanics are
therefore not a promise of unsolicited user-visible receipts for every run.

Rollback behavior is fail-closed. Schema v25 is committed atomically with its
migration row, and older binaries that only support v24 reject the database as a
future schema. Downgrade by restoring a pre-v25 database copy or exporting local
runtime state before opening it with an older binary; the v25 migration does
not rewrite existing tables.

Schema v28 adds the provider-neutral durable delivery outbox for progress
receipts. Delivery obligations, bounded attempts, negotiated acknowledgments,
and per-origin replay cursors are stored as separate project-scoped facts with
stable semantic identities. Claim owner, claim generation, and lease expiry
fence every result, acknowledgment, and cursor movement; stale claimants cannot
advance delivery state. Acknowledgment requires typed host/transport evidence
matching the obligation contract, and stdout bytes alone are not acknowledgment
unless the explicit transport contract says so.

Host progress transport is negotiated by the provider-neutral runtime
capability contract in [`runtime-capabilities.md`](runtime-capabilities.md).
Negotiation distinguishes receipt generation, transport write, host acceptance,
user visibility, and acknowledgment, but it is only a capability/evidence
policy. It never mutates the outbox and never proves that a push, visibility
event, or acknowledgment happened. Unsupported active push paths must downgrade
to durable follow/poll and then next-invocation replay without promoting a
delivery obligation beyond the exact evidence recorded in the outbox.

Retryable delivery failures persist `next_attempt_at` on both the obligation
and attempt history. When a caller does not provide an explicit future time,
the outbox schedules deterministic bounded backoff from the injected store clock
starting at 15 seconds and capped at five minutes. Pending obligations are
claimable immediately; retryable failures are claimable only at or after
`next_attempt_at`; delivered, terminal, acknowledged, unsupported, expired, and
superseded obligations are not claimable retry work.

Codex and Claude Code host replay is bounded and cursor ordered. Before
dispatch starts worker mechanics, an explicit `--run-id` replays only that
run's exact host origin binding; an ordinary later dispatch without `--run-id`
scans at most the host replay limit of prior project delivery-run candidates
with pending or due retryable host obligations for the current redacted stable
host origin reference, ordered by first eligible obligation `created_at`, run
ID, origin reference, and sink ID. Candidate discovery is cursor-aware: groups whose
per-run replay cursor has already passed all eligible obligations do not consume
the bounded candidate window, while a non-empty cursor whose anchor cannot be
read remains discoverable for the fail-closed diagnostic instead of "start from
zero." Each candidate replays only the exact persisted run-scoped `sink_id`
binding, so other hosts, projects, runs, and non-matching chatter are ignored.
Within a run, receipts replay in obligation `created_at, obligation_id` order
and advance a per-origin cursor only after a claim-fenced human emit succeeds.

Adapters and recovery readers must use project/delivery-run scoped read APIs:
`ReadDeliveryObligation`, `ListDeliveryObligations`,
`ListDeliveryAttempts`, `ListDeliveryAcknowledgments`, and
`ListDeliveryReplayCursors`. Each list API uses stable ordering plus a bounded
limit and fails closed with the canonical typed unknown-version error when a
future payload version is encountered.

The acknowledgment contract is `ack_policy`. `required-ack` obligations require
a matching typed acknowledgment before becoming terminal. `no-ack` obligations
become terminal when delivery is durably recorded and do not create an
acknowledgment row; the legacy `required_ack` column is a derived compatibility
view of that typed policy.

## Codex CLI, Claude Code, And Paseo-Style Host Delivery

The Codex CLI, Claude Code, and Paseo-style host adapters use only documented
or LoopCoder-owned local host surfaces: foreground stdout/stderr, machine JSON
stdout, durable local polling through `loopcoder status --receipts`, and
resumable following through `loopcoder attach`. For Paseo-style hosts, durable
poll/follow is LoopCoder-local replay/follow, not a claimed documented Paseo
poll/follow API. Codex additionally advertises detached-run cancellation
through `loopcoder cancel`; Claude Code and Paseo-style hosts leave detached
cancellation unknown because LoopCoder has no documented host-owned
cancellation proof for the original conductor session. These adapters declare
callback, wake-up, host-managed background ownership, and acknowledgment
unsupported unless an opt-in future integration fixture proves targeted
terminal delivery to the original host session. A host environment marker can
identify an origin candidate, but it is not capability proof and does not create
callback, wake-up, visibility, steering, or acknowledgment evidence.

When `CODEX_THREAD_ID` / `CODEX_SESSION_ID` or `CLAUDE_CODE_SESSION_ID` is
present, LoopCoder binds the active host origin by hashing the opaque
thread/session value and storing only the redacted binding, marker key names,
and bounded metadata digest. It does not persist host credentials, bearer
material, prompts, raw provider output, transcript contents, or raw local paths.
If host origin metadata is absent, automatic origin-bound replay is unavailable;
use explicit `status --receipts`, `status --follow`, or `attach --run` to
inspect the durable receipts.

Foreground `dispatch` keeps machine JSON stdout pure. Human progress replay and
diagnostics are written to stderr, while `status --receipts --format jsonl` and
`attach --format jsonl` write one receipt view per stdout line. Text status and
attach render the same durable receipt views for humans.

Host-profiled non-interactive `dispatch`, `dispatch-wave`, and `tick` default
to detached supervision. Explicit `--detach` uses the same contract, and
`--foreground` is the deterministic opt-out. A detached launch returns a run ID
plus explicit `status`, `attach`, and `cancel` commands. Detached supervisor
receipts are persisted to the progress receipt store and the delivery outbox.

The local CI watcher remains provider-free while GitHub queues or runs required
checks. Its default wait is bounded at two hours so scarce macOS arm64 runner
queues do not become false 30-minute failures; five-minute receipt cadence is
unchanged. The explicit 30-minute acceptance fixture remains the proof that a
long wait performs zero provider calls and emits every policy receipt.

If the host goes offline, the run remains observable through
`loopcoder status --repo . --run <run-id> --receipts` and
`loopcoder attach --repo . --run <run-id>`; no notification is claimed.

On a later invocation with the same project and host origin reference,
`dispatch` replays pending terminal and consequential receipts for matching
run-scoped bindings before launching new worker mechanics. Replay advances a
bounded per-origin cursor and leaves the delivery obligation pending and
unacknowledged. Replaying, polling, following, or attaching never records host
acceptance, user visibility, wake-up, or acknowledgment.

Origin mismatch produces no automatic replay. Stale cursors are bounded and
safe: the next explicit status/follow command can still read receipts by run ID,
and duplicate automatic replay is suppressed by the per-origin cursor. If a run
is cancelled, subsequent receipts surface the cancellation state through the
same status, follow, attach, and matching-origin replay paths.
