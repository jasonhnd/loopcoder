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

Rollback behavior is fail-closed. Schema v25 is committed atomically with its
migration row, and older binaries that only support v24 reject the database as a
future schema. Downgrade by restoring a pre-v25 database copy or exporting local
runtime state before opening it with an older binary; the v25 migration does
not rewrite existing tables.
