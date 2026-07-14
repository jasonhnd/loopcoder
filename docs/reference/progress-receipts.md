# Progress Receipts

`ProgressReceipt` is the v0.8 provider-neutral progress record. It is persisted
in project-scoped SQLite schema v25 as an append-only, redacted
`loopcoder.progress_receipt.v1` payload plus query columns for run, task,
attempt, correlation, phase, status, provider, model, heartbeat age, and
progress age.

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

Retryable delivery failures persist `next_attempt_at` on both the obligation
and attempt history. When a caller does not provide an explicit future time,
the outbox schedules deterministic bounded backoff from the injected store clock
starting at 15 seconds and capped at five minutes. Pending obligations are
claimable immediately; retryable failures are claimable only at or after
`next_attempt_at`; delivered, terminal, acknowledged, unsupported, expired, and
superseded obligations are not claimable retry work.

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
