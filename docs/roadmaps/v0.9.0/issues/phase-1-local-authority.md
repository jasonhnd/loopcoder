# v0.9.0 Issue Drafts: P1 Local Authority

Status: development-ready issue drafts; owner publication/assignment required

These are ordinary-development issue bodies. Publish only after owner approval.
All storage is local to `$LOOPCODER_HOME`; GitHub remains the collaboration and
cross-Mac handoff authority. No issue may write runtime state into a customer
repository or make the legacy and v0.9 stores concurrent writers.

## V090-004: Machine-store and project-store topology contract

**Metadata:** code, size S; depends on V090-003; labels `v0.9.0`, `storage`,
`architecture`; exclusive in the v0.9 store facade.

### Outcome and why

Introduce one small v0.9 store-opening facade that distinguishes machine-scoped
authority from project-scoped authority. The current code has both
`internal/storage` schema v31 and the new `internal/store` foundation; without a
single facade, later issues could create a third ambiguous writer.

### Current evidence

- `internal/store` has reusable SQLite open, metadata, migration, integrity, and
  permission primitives.
- `internal/storage` owns broad v0.8 state and must become compatibility-only.
- The accepted topology is `data/machine.db` plus one
  `projects/<project-id>/project.db` per project.

### Scope and constraints

- Define typed `MachineStore` and `ProjectStore` open options, schema identities,
  lifecycle, errors, and read/write ownership.
- Reuse the compact-store foundation; do not add domain tables yet.
- Make legacy store access an explicit read-only compatibility port.
- Reject opening one file as both machine and project authority.
- Do not add cross-database transactions, Dolt, CGO, or a database daemon.

### Suggested sequence

1. Audit all current store-open entry points and document intended disposition.
2. Add typed paths/options and authority metadata.
3. Add wrong-role, double-open, close, and legacy-read-only fixture tests.
4. Move no callers beyond the minimum conformance fixture in this issue.

### Acceptance criteria

1. Callers must explicitly open a machine or project store; role mismatch fails
   with a typed error before mutation.
2. Both roles reuse one compact SQLite foundation but have independent schema
   names, versions, migration ledgers, and lifecycle handles.
3. v0.8 storage is reachable only through a read-only compatibility interface.
4. No cross-database transaction or repo-local fallback exists.
5. Focused tests prove open, role mismatch, reopen, concurrent handles, and
   idempotent close.

### Verification, failure, and safety

Run focused store/facade tests and remote affected-package race. Opening the
wrong role must make no file changes. This issue contains no migration and can
be reverted without data conversion. Tests use synthetic paths and redact SQL
payloads. One developer/test process; no provider, network, or full local suite.

### Non-goals and done

No project registration, provider tables, event schema, or CLI changes. Done
when the merged PR establishes the only permitted v0.9 store entry point and the
disposition map names all remaining direct legacy opens.

---

## V090-005: Global home layout and owner-only directory creation

**Metadata:** code, size S; depends on V090-004; labels `v0.9.0`, `storage`,
`security`, `paths`; exclusive in `internal/home` and path creation.

### Outcome and why

Make the accepted global layout real and secure before any schema writes:

```text
$LOOPCODER_HOME/data/machine.db
$LOOPCODER_HOME/projects/<project-id>/{project.db,runs,logs,tmp,recovery}
```

v0.8 still has code paths that can fall back to `<repo>/.loopcoder`. That is
unacceptable for a local development tool and risks accidental Git publication.

### Scope and constraints

- Add canonical typed path builders and owner-only, symlink-safe directory
  creation for the supported Darwin platform.
- Validate `LOOPCODER_HOME`, project IDs, containment, permissions, and file type
  before opening SQLite or logs.
- Reject path traversal, absolute child IDs, symlink escape, and repo-root targets.
- Inventory and test that new-path code cannot call repo-local fallback helpers.
- Do not automatically move or delete legacy state.

### Suggested sequence

1. Define one typed layout value returned after validation.
2. Add containment and permission tests using V090-003 fixtures.
3. Wire V090-004 opens through the validated layout.
4. Add a repository scan assertion after representative file creation.

### Acceptance criteria

1. Every new v0.9 database, run, log, temp, and recovery path resolves beneath
   the validated global home and never beneath the customer repo/worktree.
2. New directories and files are owner-only on Darwin; unsafe existing modes,
   owners, symlinks, and non-directory path components fail closed.
3. Malformed or escaping project IDs fail with typed errors before creation.
4. A first-run fixture creates the complete minimum layout idempotently.
5. A recursive customer-repo scan after the fixture finds no LoopCoder runtime
   file, hidden directory, sidecar, relay, log, or temporary payload.

### Verification, failure, and safety

Use temp homes/repos, symlink/traversal fixtures, mode checks, and remote race.
On partial creation failure, remove only empty directories created by that call;
never recursively delete preexisting paths. Diagnostics show logical path kinds,
not real usernames or full home paths. No provider/network use.

### Non-goals and done

No project identity, migration, retention, or backup policy. Done when all new
store opens require this layout and the no-repo-state test is a reusable gate.

---

## V090-006: Stable project identity, aliases, and automatic registration

**Metadata:** code, size M; depends on V090-005; labels `v0.9.0`, `project`,
`registry`, `github`; exclusive in project identity and registry writes.

### Outcome and why

Assign each repository a stable project ID that distinguishes repositories with
the same short name, survives local path changes, and automatically registers a
valid first-run repository without writing into it.

The repository name alone is not globally unique. GitHub owner/repo identity is
preferred when available; a local-only repository still needs a stable machine
identity and alias history.

### Current evidence

`internal/registry`, `internal/gitremote`, `internal/pathid`, and `internal/gitutil`
already contain useful normalization and environment-scrubbing behavior. They
must be consolidated rather than replaced by ad hoc path hashes.

### Scope and constraints

- Define canonical project identity inputs, precedence, normalized GitHub remote,
  local-only fallback, stable ID derivation, aliases, and conflict handling.
- Store registry facts in `machine.db`; place project state under the derived ID.
- Auto-register on first valid run; do not require a repo-local marker.
- Preserve identity when a checkout moves or a second checkout is added.
- Never merge two projects solely because their short repository names match.

### Suggested sequence

1. Specify identity precedence and normalization vectors.
2. Reuse existing remote/path parsers behind one resolver.
3. Add atomic registration and alias update behavior.
4. Test same-name, moved-path, remote-change, local-only, and conflicting cases.

### Acceptance criteria

1. `owner-a/app` and `owner-b/app` receive different stable project IDs.
2. Moving or recloning the same normalized GitHub repository reuses its project
   identity while recording the new local alias.
3. A valid unregistered repository auto-registers under `$LOOPCODER_HOME` and
   leaves the repository byte-for-byte free of runtime state.
4. Ambiguous remote changes or conflicting identities fail closed with an
   explainable reconciliation action; they are never silently merged.
5. Git commands run with scrubbed inherited repository environment variables.

### Verification, failure, and safety

Use synthetic bare remotes and local checkouts; no GitHub network access. A
failed registration transaction leaves neither a registry row nor a project
directory claimed as authoritative. Redact remote credentials and local paths.
Run focused registry/path tests and remote race.

### Non-goals and done

No cross-Mac database sync, GitHub API intake, or migration of v0.8 aliases.
Done when one resolver owns all v0.9 project identity and later issues can open a
project store from a stable ID only.

---

## V090-007: Machine authority store for provider and resource facts

**Metadata:** code, size M; depends on V090-004 and V090-005; labels `v0.9.0`,
`storage`, `provider`, `resources`; exclusive in `machine.db` schema.

### Outcome and why

Create the compact machine-scoped schema for project registry references,
provider/model observations, quota windows, health/cooldown, and host resource
reservations. This separates facts shared by many local projects from project
execution truth.

### Scope and constraints

- Add only machine-scoped tables, typed records, schema migration, and narrow
  repository interfaces named in the storage contract.
- Record source, observed time, freshness, confidence, expiry/reset, and digest
  for provider/quota evidence; never store credentials.
- Model resource reservations with owner, budget, lease/expiry, and terminal
  release state, but do not implement the admission policy yet.
- Use foreign keys, uniqueness, checked enums, and bounded payloads.
- Do not add jobs, attempts, events, PRs, or project status to `machine.db`.

### Suggested sequence

1. Convert the accepted machine-domain nouns into minimal normalized tables.
2. Add one versioned migration and repository interfaces.
3. Add insert/read/supersede/expire tests with injected time.
4. Add a schema inventory assertion preventing project-domain columns.

### Acceptance criteria

1. `machine.db` persists project registry references, provider installations,
   model capabilities, quota observations, health/cooldown, and resource
   reservations with explicit provenance and timestamps.
2. No credential, prompt, issue body, worktree, job, attempt, or project event is
   accepted by the machine schema.
3. Duplicate immutable observations are idempotent; conflicting identities fail
   typed and preserve the prior record.
4. Migrations are transactional, forward-only, and leave a durable ledger tied
   to schema digest and application version.
5. Focused tests prove reopen, foreign keys, payload limits, expiry, and
   concurrent readers with one writer.

### Verification, failure, and safety

Run schema golden, migration rollback injection, reopen, and remote race tests.
On migration failure, the previous version remains readable and no partial table
is authoritative. Synthetic provider/account IDs only; payload renderers redact
unexpected secret-shaped fields. No provider probes or network.

### Non-goals and done

No inventory discovery, quota collection, route scoring, or admission decision.
Done when later provider/resource issues can use typed repositories without raw
SQL or adding machine facts to project stores.

---

## V090-008: Project authority store and append-only event schema

**Metadata:** code, size M; depends on V090-004 and V090-006; labels `v0.9.0`,
`storage`, `events`, `project`; exclusive in `project.db` schema.

### Outcome and why

Create the compact project-scoped schema that will own work, jobs, attempts,
events, route evidence references, delivery state, and UI client cursors. Establish
one append-only event family as the lifecycle truth instead of parallel report,
relay, outbox, and status writers.

### Scope and constraints

- Add minimal project, work item, job, attempt, event, projection checkpoint,
  external evidence reference, and UI client cursor/acknowledgement tables.
- Keep domain payloads versioned and bounded; normalize identity and lifecycle
  fields required for queries and constraints.
- Events are immutable after commit. Corrections append a new event.
- Store machine evidence IDs/digests used by a route; do not create a cross-DB
  foreign key or transaction.
- Do not implement append/replay APIs, graph scheduling, or runtime behavior yet.

### Suggested sequence

1. Derive tables from the accepted domain/state and storage contracts.
2. Define checked states, keys, indexes, and event envelope version.
3. Add one migration and schema conformance fixtures.
4. Prove project isolation using two project stores simultaneously.

### Acceptance criteria

1. The schema represents the accepted project-domain nouns without provider
   credentials or machine-global resource/inventory rows.
2. Event rows have stable event ID, project ID, aggregate identity, kind,
   schema version, sequence, recorded time, idempotency key, bounded payload,
   and causal/evidence references.
3. Event mutation and deletion are unavailable through the production interface;
   corrections require a new event.
4. Two project stores with overlapping issue/run numbers remain fully isolated.
5. Schema migration, reopen, foreign-key, enum, index, and payload-bound tests
   pass against disposable files.

### Verification, failure, and safety

Use schema golden and malformed/oversized fixture tests plus remote affected
race. Migration failure rolls back atomically. Event fixture payloads contain no
personal data, credentials, real issue text, or absolute paths. One writer per
project test; no network/provider work.

### Non-goals and done

No lifecycle transitions, event append API, projections, provider collection,
or v0.8 import. Done when the schema has one documented owner and V090-009 can
implement append without altering the domain shape.

---

## V090-009: Atomic event append and idempotency contract

**Metadata:** code, size S; depends on V090-008; labels `v0.9.0`, `events`,
`idempotency`, `storage`; exclusive in the event writer.

### Outcome and why

Implement the single project-event writer with monotonic sequence allocation,
idempotent retry, conflict detection, and transactional aggregate checkpoint
updates. This is the foundation for truthful progress and safe recovery.

### Scope and constraints

- Append one event in an immediate write transaction and allocate its project
  sequence without a read-then-write race.
- Treat identical `(scope, idempotency_key, canonical digest)` retries as reuse.
- Treat the same key with different canonical content as a typed conflict.
- Optionally update only the event writer's aggregate revision/checkpoint in the
  same transaction; no business projection logic yet.
- Map SQLite busy/constraint errors to stable typed results, never strings.

### Suggested sequence

1. Specify canonical digest and idempotency scope.
2. Implement transaction, sequence, insert, and retry result.
3. Add concurrent writers and crash-injection fixtures.
4. Expose a narrow API that returns persisted event evidence.

### Acceptance criteria

1. Successful appends receive strictly increasing project sequences and become
   visible atomically with the returned evidence.
2. Repeating an identical request returns the original event without allocating
   a new sequence.
3. Reusing a key with different content fails typed and changes no event or
   checkpoint row.
4. Two independent store handles appending concurrently produce one gap-free,
   duplicate-free committed order for successful operations.
5. Busy, cancellation, constraint, and injected-commit failures have documented
   retryability and never report success before durable commit.

### Verification, failure, and safety

Run barrier-based concurrent tests, crash/failure injection, cancellation, and
remote race. Avoid wall-clock timing. Rollback leaves no sequence or checkpoint
advance. Payloads are bounded and redacted before diagnostic rendering. No model,
GitHub, or host integration.

### Non-goals and done

No projection reducer, cross-project ordering, event compaction, or distributed
writer. Done when all later lifecycle writers must call this API and no second
production append path exists.

---

## V090-010: Ordered cursor replay and projection checkpoints

**Metadata:** code, size M; depends on V090-009; labels `v0.9.0`, `events`,
`projection`, `recovery`; exclusive in event readers/projection checkpoints.

### Outcome and why

Provide deterministic cursor replay and rebuildable compact projections so CLI
status and UI clients read one event truth instead of maintaining parallel
lifecycle records.

### Scope and constraints

- Define an opaque cursor containing project identity, last accepted sequence,
  and format version; reject cross-project or future-version cursors.
- Read events strictly after a cursor in bounded pages and stable order.
- Add reducer/checkpoint primitives that atomically record reducer name/version,
  input sequence, output digest, and compact payload.
- Support full rebuild into a new checkpoint generation before swapping current.
- Do not implement specific runtime/status reducers or event deletion.

### Suggested sequence

1. Specify cursor encoding and validation vectors.
2. Implement bounded replay, empty tail, and pagination.
3. Implement checkpoint compare-and-swap and generation rebuild.
4. Test disconnect/reconnect, reducer upgrade, and corrupt checkpoint recovery.

### Acceptance criteria

1. Replay after a valid cursor returns each committed event once, in sequence,
   across page boundaries and reopen.
2. Invalid, stale-generation, future-version, and wrong-project cursors fail
   typed without falling back to sequence zero silently.
3. Projection checkpoints advance only from their expected input revision and
   preserve reducer version and output digest.
4. A projection can rebuild from sequence zero into a new generation and swap
   atomically without blocking event append for the full rebuild duration.
5. Corrupt or missing projection data never alters events and yields a clear
   rebuild action.

### Verification, failure, and safety

Use deterministic multi-page fixtures, concurrent append/replay, reducer upgrade,
and injected checkpoint failure tests plus remote race. Cursor rendering must not
include local paths or payload content. Reverting leaves immutable events usable;
projections are disposable.

### Non-goals and done

No host networking, status UI, event compaction, or workflow reducer. Done when
events are sufficient to rebuild projections and V090-021 can consume this API
without raw SQL.

---

## V090-011: SQLite contention, restart, integrity, and backup behavior

**Metadata:** code, size M; depends on V090-005, V090-007, and V090-010; labels
`v0.9.0`, `storage`, `reliability`, `recovery`; exclusive in store resilience.

### Outcome and why

Qualify the machine/project SQLite topology under realistic local contention,
process interruption, abrupt close, disk errors, and backup/restore so P2 does
not build runtime truth on an unproven persistence layer.

### Scope and constraints

- Define busy timeout/backoff, connection-pool bounds, WAL/checkpoint policy,
  integrity checks, clean/unclean-open markers, and backup snapshot behavior.
- Use independent bounded cleanup contexts for rollback/close where caller
  cancellation would otherwise contaminate the connection.
- Quarantine rather than overwrite a database that fails integrity or schema
  authority checks.
- Produce a redacted diagnostic and operator action for disk-full, permission,
  busy-exhausted, corruption, and incompatible-version cases.
- Keep one writer policy; do not implement distributed locking or DB sync.

### Suggested sequence

1. Document operating pragmas and pool limits for machine and project roles.
2. Add contention and cancellation fixtures with multiple store handles.
3. Add crash/reopen, WAL, integrity, backup, and restore scenarios.
4. Add redacted failure classification and quarantine behavior.

### Acceptance criteria

1. Concurrent readers and bounded writers complete or return a typed retryable
   busy result without indefinite waits or connection-pool growth.
2. Cancellation during a write cannot return a poisoned transaction/connection
   to the pool; rollback and close use bounded independent cleanup.
3. Abrupt termination followed by reopen preserves committed events, discards
   uncommitted work, and reports whether recovery occurred.
4. Integrity or incompatible-schema failure prevents writes and produces a
   non-destructive backup/quarantine action; it never auto-recreates over data.
5. A consistent online backup restores into a separate path and reproduces
   schema metadata, events, and projection digests.

### Verification, failure, and safety

Run deterministic contention, cancellation, kill/reopen, disk/permission fault,
backup/restore, and remote race tests. Stress iteration counts remain bounded and
are lower under race when necessary, but synchronization assertions stay intact.
Diagnostics are redacted and backups are owner-only. No provider/network calls.

### Non-goals and done

No Dolt, replication, concurrent cross-Mac writers, automatic destructive repair,
retention deletion, or v0.8 migration. Done when P1 exit fixtures pass and both
stores have documented operational limits and recovery actions.
