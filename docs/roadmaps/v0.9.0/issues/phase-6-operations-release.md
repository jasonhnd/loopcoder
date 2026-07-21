# v0.9.0 Issue Drafts: P6 Operations And Release

Status: development-ready issue drafts; owner publication/assignment required

Publish only after V090-065 passes and owner approval. P6 qualifies ordinary
multi-project use, public/private repositories, terminal GitHub-based handoff
between Macs, v0.8 import, old-path deletion, documentation, packaging, and the
single public `v0.9.0` release. It does not synchronize live local databases or
permit two machines to execute the same local attempt.

## V090-066: Multi-project global admission and isolation

**Metadata:** code/test, size M; depends on V090-016, V090-055, V090-065; labels
`v0.9.0`, `multi-project`, `resources`, `isolation`; exclusive in machine-wide
operations.

### Outcome and rationale

Prove one installed LoopCoder can manage many local repositories while sharing
provider/resource observations safely and isolating every project's execution,
logs, events, routes, and private content.

### Scope and constraints

- Exercise at least two projects with same short name/different owners plus a
  third local-only project under one machine store.
- Enforce global worker/verifier/test/process/CPU/RSS/provider concurrency budgets
  across project stores.
- Scope reservations and status summaries without exposing another project's
  issue text, paths, prompts, output, or private repo identity by default.
- Reconcile abandoned reservations with each project's process evidence.
- Support project removal/archival as an explicit non-destructive operation.

### Acceptance criteria

1. Project IDs/stores/paths/events/logs/branches/routes never collide for same-name
   or simultaneous repositories.
2. Concurrent admission across projects cannot exceed global or provider limits;
   denied project receives an explainable wait/attention decision.
3. Machine summary exposes bounded identity/status/resource facts only and redacts
   project-private content by default.
4. Restart reconciles reservations and project attempts without cross-project
   adoption, release, or terminalization.
5. Archiving one project preserves or deletes only its explicitly selected global
   payload and registry aliases, never another project or customer repo.

### Verification and boundaries

Multi-project fixtures, concurrent claims, same-name/path move, archive, restart,
and remote race. No real provider/private repo. No live cross-Mac operation. Done
when one machine remains responsive and isolated under multiple registered repos.

---

## V090-067: Private-repository redaction and consumer canary

**Metadata:** code/test, size M; depends on V090-036 and V090-055; labels
`v0.9.0`, `privacy`, `security`, `github`; exclusive in privacy conformance.

### Outcome and rationale

Qualify direct/provider paths for private repositories and prove that global
state, logs, host reports, diagnostics, PR metadata, and release evidence do not
leak private issue/code/prompt/path/account content.

### Scope and constraints

- Define data classes and allowed destinations for public identity, project-private
  metadata, code/prompt/output, credentials, quota/account, and diagnostics.
- Add centralized redaction/conformance fixtures with synthetic secret markers.
- Exercise status/events/UI clients/machine summary/errors/PR body/evidence
  manifest and deletion/retention boundaries.
- Test least GitHub permissions and fail closed on repo visibility/authorization
  ambiguity.
- A bounded owner-controlled real private canary may run only at release gate.

### Acceptance criteria

1. Synthetic private markers never appear in machine-global DB, default global
   status, unrelated project, host diagnostics, CI artifact, or release manifest.
2. Credentials/auth tokens/keychain material are never persisted or rendered in
   any project/machine output.
3. Project-scoped events/logs retain only policy-authorized bounded content with
   owner-only permissions and documented retention.
4. GitHub operations use least required permission and wrong/private repo access
   fails before provider launch.
5. Automated scan covers database fields, files, logs, JSON/JSONL/human output,
   error paths, and PR templates.

### Verification and boundaries

Synthetic canary tokens and fake GitHub; scan failures show field/location but not
secret value. No real private content in PR CI. No encryption-at-rest claim or
credential manager. Done when the release can truthfully document privacy limits.

---

## V090-068: Cross-Mac GitHub rehydration after terminal handoff

**Metadata:** code/test, size M; depends on V090-006, V090-032, V090-036; labels
`v0.9.0`, `handoff`, `github`, `multi-machine`; exclusive in terminal rehydrate.

### Outcome and rationale

Let the owner finish issue/PR work on Mac A and continue later on Mac B by reading
GitHub and creating fresh local state. Do not copy/merge SQLite files, local
leases, process identity, or in-flight scheduler state.

### Scope and constraints

- Define rehydrate inputs from normalized repo, issue/PR, commits, checks, reviews,
  and explicit selected local checkout.
- Create/reuse stable project identity on the new machine and append a local
  rehydration event referencing remote evidence.
- Support terminal delivered/gated/merged/closed states and delivery-only
  continuation if GitHub evidence proves completed worker output.
- Reject adoption of an in-flight local process/attempt from another machine.
- Detect remote divergence/ambiguity and require explicit reconciliation.

### Acceptance criteria

1. A fresh home on Mac B reconstructs project/work/delivery status from fixture
   GitHub evidence without reading/copying Mac A database or state branch.
2. Terminal PR/commit/check/review identities and route evidence references remain
   linked while Mac B creates new local execution identity.
3. In-flight/ambiguous remote state cannot be treated as a live local attempt or
   automatically relaunched.
4. Same-name repositories and private/public visibility remain correctly isolated.
5. Repeating rehydrate is idempotent and reports remote/local differences before
   mutation.

### Verification and boundaries

Two isolated home directories and fake GitHub histories; no physical second Mac
required in PR CI, followed by release consumer smoke on two Macs if available.
No database sync, Dolt, distributed lease, or simultaneous same-attempt work. Done
when terminal GitHub handoff is sufficient and documented.

---

## V090-086: Machine-authority rebuild and reservation reconciliation

**Metadata:** code/test, size M; depends on V090-006, V090-007, V090-011, and
V090-066; labels `v0.9.0`, `recovery`, `storage`, `operations`; exclusive in
machine-store disaster recovery.

### Outcome and rationale

Recover safely when `machine.db` is missing or corrupt without merging/copying a
live database or losing independently healthy project history. Machine inventory
and reservations are current-Mac authority; project stores and reviewed local
configuration must contain enough identity to rebuild references conservatively.

### Scope and constraints

- Give each project store self-identifying project/repository/schema metadata and
  a machine-registration generation independent of mutable local path.
- Scan only validated `$LOOPCODER_HOME/projects` children, reject symlink/path/file
  anomalies, and rebuild a new machine store beside the damaged one.
- Re-register project aliases, refresh provider observations through normal
  probes, and reconcile resource/quota reservations against live process evidence.
- Preserve the damaged database read-only for diagnostics/backup; never silently
  overwrite or salvage uncertain rows into authority.
- Unknown live process or reservation ownership becomes attention, not automatic
  release/adoption.

### Acceptance criteria

1. Missing machine store rebuilds registry references for valid project stores
   without changing project events or customer repositories.
2. Corrupt/partial/symlinked/wrong-owner/duplicate project candidates are isolated
   with precise diagnostics and cannot poison the new authority.
3. Provider facts are refreshed with provenance; credentials and stale serialized
   snapshots are not reconstructed as current truth.
4. Reservations reconcile to released, live-owned, or attention-required using
   OS/process evidence and never double-admit uncertain capacity.
5. Repeating rebuild against unchanged evidence is idempotent and records one
   redacted manifest plus backup path/digest.

### Verification and boundaries

Disposable homes with missing/corrupt/partial/duplicate/symlink stores, live/dead
process fixtures, crash during rebuild, and remote race. No database merge,
cross-Mac state copy, credential recovery, or automatic deletion of damaged data.

---

## V090-087: Event, log, runtime-file retention and garbage collection

**Metadata:** code/docs, size M; depends on V090-010, V090-014, V090-064, and
V090-086; labels `v0.9.0`, `storage`, `retention`, `cleanup`; exclusive in local
retention and garbage collection.

### Outcome and rationale

Bound append-only events, logs, output excerpts, UI delivery evidence, temporary
files, stale worktrees, and backups without deleting lifecycle truth or customer
Git data. Append-only does not mean unlimited disk growth.

### Scope and constraints

- Define per-class default retention, size/count caps, archive eligibility,
  minimum audit evidence, active/attention holds, and owner overrides.
- Separate projection rebuild/checkpoint compaction from immutable event deletion.
- Provide dry-run inventory and explicit archive/delete plan with project/store
  generation, path containment, expected reclaimed bytes, and backup rule.
- Clean only exactly owned terminal resources; active, ambiguous, unacknowledged,
  migration, and attention records are held.
- Never delete customer repos, branches, commits, PRs, provider credentials, or
  unknown files; never run GC implicitly during a fragile recovery transaction.

### Acceptance criteria

1. Dry-run deterministically lists each candidate, reason, hold, byte estimate,
   and retained authority before any deletion.
2. Active/nonterminal/attention/unacknowledged/migration/ambiguous resources cannot
   be collected regardless of age or size pressure.
3. Approved GC is idempotent, path-contained, crash-resumable, and preserves
   enough events/checkpoints to rebuild advertised current/history views.
4. Disk-full policy stops new admission or prunes only preapproved expendable
   classes; it never silently deletes audit/project truth.
5. Retention/GC reports are bounded/redacted and expose no private payload or
   machine-identifying absolute path by default.

### Verification and boundaries

Injected-time/size disposable homes, active/terminal/attention holds, symlink/
path attacks, crash barriers, rebuild after GC, and remote race. No cloud archive,
repo history rewrite, credential cleanup, or automatic destructive default.

---

## V090-069: Read-only v0.8 state exporter

**Metadata:** code, size M; depends on V090-011; labels `v0.9.0`, `migration`,
`v0.8-compat`; exclusive in legacy state reading.

### Outcome and rationale

Extract supported facts from v0.8 global/repo-local schema and payloads into a
versioned neutral export without letting v0.9 write, repair, upgrade, or execute
through the old store.

### Scope and constraints

- Inventory supported v0.8 schema versions and locations; open immutable/read-only
  with no migration pragmas or side effects.
- Export normalized project identities/aliases and selected terminal work/run/
  delivery/report evidence needed for history; classify unsupported/ambiguous data.
- Include source schema/version/digests/counts/warnings and redact credentials/
  private content according to policy.
- Never import provider auth or stale local process authority.
- Preserve original files byte-for-byte and produce export outside customer repo.

### Acceptance criteria

1. Supported v0.8 fixtures export deterministic versioned records and manifest
   while source file hashes/modes/contents remain unchanged.
2. Unknown/newer/corrupt/partial schema fails or reports unsupported records without
   auto-migration, repair, deletion, or recreation.
3. No credential, token, live lease, PID authority, or unsafe raw payload enters
   the export.
4. Duplicate/legacy paths are reconciled by evidence with conflicts surfaced, not
   silently merged.
5. Export is bounded, owner-only, resumable/idempotent, and contains source/digest
   evidence sufficient for V090-070.

### Verification and boundaries

Golden fixtures across supported/corrupt/newer/malformed v0.8 versions; immutable
source hash assertions. No real user database in CI. No v0.9 write/import or old
execution. Done when compatibility reading has one audited port.

---

## V090-070: v0.9 project-state importer and migration report

**Metadata:** code, size M; depends on V090-011 and V090-069; labels `v0.9.0`,
`migration`, `storage`; exclusive in v0.9 import.

### Outcome and rationale

Import the neutral v0.8 export into machine/project v0.9 stores transactionally,
preserving useful terminal history and provenance while refusing unsafe live-state
claims. Produce a human/JSON migration report and rollback limitation.

### Scope and constraints

- Validate export version/source digest and map projects/aliases/history/evidence
  to v0.9 records with stable import idempotency keys.
- Import no running process, claim, resource reservation, auth, credential, or
  implicit route eligibility.
- Support dry-run counts/conflicts/actions and per-project transactional commit.
- Preserve source evidence references and classify omitted/unsupported records.
- Never delete/move old state automatically.

### Acceptance criteria

1. Dry-run deterministically reports projects, records, conflicts, omissions,
   required space, and target paths without mutation.
2. Import of the same export is idempotent and cannot duplicate events/history or
   overwrite newer v0.9 records.
3. Each project import commits atomically; one failed project does not corrupt
   machine store or successfully imported projects.
4. Imported nonterminal/live v0.8 records become historical/attention records and
   never authorize process adoption or execution.
5. Migration report includes source/target versions/digests/counts/warnings and a
   clear statement that post-write rollback requires restoring backup/new stores.

### Verification and boundaries

Golden dry-run/import/reimport/conflict/failure fixtures and backup restore. No
real user data. No automatic old-state deletion or binary rollback. Done when
supported history is visible through v0.9 readers and migration is auditable.

---

## V090-071: Compatibility shims and old/new writer isolation

**Metadata:** code, size M; depends on V090-070; labels `v0.9.0`, `compatibility`,
`migration`, `safety`; exclusive in legacy command boundary.

### Outcome and rationale

Keep only narrowly required v0.8 read/view/export compatibility for one release
while guaranteeing legacy and v0.9 paths cannot both mutate the same project or
present conflicting authority.

### Scope and constraints

- Classify legacy commands as removed, read-only compatibility, explicit exporter,
  or unsupported with replacement guidance.
- Route all new commands exclusively to v0.9 stores/events/runtime.
- Add writer-lock/authority metadata preventing old mutation once a project has
  accepted v0.9 writes; old reads remain isolated.
- Prefix compatibility output clearly and exclude it from v0.9 status/gates.
- Define deprecation/removal schedule and support matrix.

### Acceptance criteria

1. No command can write both v0.8 and v0.9 state for one operation or project.
2. After v0.9 authority acceptance, any legacy mutating command fails closed with
   replacement/export guidance before side effects.
3. Supported legacy reads are immutable, labeled compatibility-only, and cannot
   affect route, runtime, delivery, verification, or release decisions.
4. Command/help/capability inventory has an explicit disposition for every v0.8
   surface.
5. Mixed-version fixture attempts prove no dual writer, silent fallback, or
   repo-local new state.

### Verification and boundaries

Command inventory/golden tests, old/new writer conflict, migration/rollback
fixtures. No feature parity expansion of old commands. Done when V090-072 through
V090-079 can remove only paths with accepted replacements.

---

## V090-072: Remove repository-local runtime fallbacks and sidecars

**Metadata:** code, size S; depends on V090-005, V090-006, V090-071; labels
`v0.9.0`, `cleanup`, `paths`, `breaking-change`; exclusive in runtime path
resolution and repository-side payload writers.

### Outcome and rationale

Delete every production fallback that writes `.loopcoder`, run sidecars, relay,
recovery, log, or temporary payloads inside customer repositories/worktrees. The
new global/project layout is already proven, so retaining fallback creates a
second unsafe state location and accidental Git-publication risk.

### Scope and constraints

- Inventory repo-local writers/readers and map each to global path, read-only v0.8
  export, explicit policy file, or removal.
- Delete production fallback and obsolete sidecar writers; retain only read-only
  migration discovery where V090-069 requires it.
- Replace fallback-on-registration-error with typed fail/repair guidance.
- Preserve user-authored project policy files as read-only input.
- Do not delete any existing repo-local file automatically.

### Acceptance criteria

1. New-path runtime contains no production write to a customer repo/worktree other
   than the worker's intended code changes and Git metadata operations.
2. Unregistered/invalid project identity fails or auto-registers globally; it never
   chooses `<repo>/.loopcoder`.
3. Legacy repo-local state is opened only by the read-only exporter and cannot
   become v0.9 authority.
4. Repository scan canaries remain clean across direct, provider, workflow, cancel,
   resume, and failure paths.
5. Removal manifest lists every deleted/retained repo-local path and migration
   disposition.

### Verification and boundaries

Path/inventory and consumer canaries plus required remote checks. No migration,
storage-schema, progress, or CLI pruning in this issue. Revert is code-only and
does not modify customer files. Done when no runtime fallback remains.

---

## V090-073: Retire legacy v0.8 storage mutation paths

**Metadata:** code, size M; depends on V090-069, V090-070, V090-071, V090-072;
labels `v0.9.0`, `cleanup`, `storage`, `breaking-change`; exclusive in legacy
store mutation and schema-open behavior.

### Outcome and rationale

Remove v0.8 schema migration/write entry points from the v0.9 binary after the
read-only exporter and v0.9 importer pass. Keep only the smallest audited
immutable reader required for one-release migration support.

### Scope and constraints

- Inventory direct `internal/storage`, `migration`, and `migrate` writers and all
  command/service callers.
- Delete or compile out old open-for-write, schema migration, transaction, and
  repair paths from v0.9 command reachability.
- Retain read-only exporter code behind the compatibility port with immutable
  filesystem/SQLite options.
- Remove old tables only from code; never mutate/delete a user's existing DB.
- Update schema/command disposition and migration tests.

### Acceptance criteria

1. No supported v0.9 command can open v0.8 state writable or run a v0.8 schema
   migration/repair.
2. The exporter still reads every supported immutable fixture and source hashes
   remain unchanged.
3. New-path package graph has no dependency on legacy storage transaction/write
   interfaces.
4. Unknown/corrupt/newer old state fails non-destructively with exporter guidance.
5. Direct/provider/workflow/import canaries pass after deletion.

### Verification and boundaries

Dependency/inventory, old-fixture immutable-read, importer, and remote checks. No
event/progress/provider/CLI deletion. Revert restores code reachability only. Done
when old storage is provably export-only.

---

## V090-074: Retire parallel progress, report, relay, and outbox lifecycle writers

**Metadata:** code, size M; depends on V090-023, V090-036, V090-071; labels
`v0.9.0`, `cleanup`, `progress`, `breaking-change`; exclusive in lifecycle report
ownership.

### Outcome and rationale

Remove v0.8 progress/report/relay/outbox code that writes lifecycle truth in
parallel with project events. Retain reusable redaction/rendering only as pure
projection helpers where the `loopcoder.ui.v1` status/report path still uses
them.

### Scope and constraints

- Inventory lifecycle writes in `progress`, `report`, `reportquery`, `reporter`,
  `relay`, `relaygate`, `progresshost`, and CLI callers.
- Delete claims/outbox/ack/relay gates superseded by event cursor and UI-client
  acknowledgement.
- Move any retained renderer/redactor behind pure event/projection input.
- Remove compatibility command paths that can create/flush/close lifecycle state.
- Do not remove historical v0.8 export parsing needed by V090-069.

### Acceptance criteria

1. One project event writer is the only v0.9 lifecycle write authority.
2. Status/report/UI output derives from event/projection data and cannot create a
   second terminal or progress record family.
3. No `relay flush` or pending-relay gate is required for direct/workflow terminal
   completion or user-visible replay.
4. Retained redaction/rendering helpers are side-effect-free and covered by final-
   mile conformance tests.
5. Silent-worker, direct-run, host, resume, and private redaction canaries remain
   green after removal.

### Verification and boundaries

Dependency/inventory plus P2/P3/private remote acceptance. No store/provider/
workflow/CLI-wide cleanup. Done when reports are projections, not competing truth.

---

## V090-075: Retire duplicate provider inventory, invocation, and router writers

**Metadata:** code, size M; depends on V090-055, V090-071; labels `v0.9.0`,
`cleanup`, `provider`, `routing`; exclusive in legacy provider/router ownership.

### Outcome and rationale

Remove old new-path-callable provider inventory, agent adapters, quota snapshots,
route writers, and fallback decision paths after official adapter/router
conformance. Keep only low-level helpers explicitly reused by the accepted facade.

### Scope and constraints

- Inventory `providerinventory`, legacy `agent` entry points, `routing`, quota,
  reconciliation, and CLI callers against V090-037 through V090-055.
- Delete duplicate registration/source/refresh/decision/fallback writers and raw
  SQL repositories superseded by machine/project stores.
- Retain process invocation mechanics only behind official provider adapters.
- Preserve explicit pin and historical route import readers.
- Update capability/disposition and future-provider conformance.

### Acceptance criteria

1. One provider descriptor registry, one observation store/refresh path, and one
   route decision writer serve every supported provider.
2. No legacy adapter/router can be selected by direct/workflow commands or silently
   substitute a route.
3. Official Codex/Claude/Gemini/Grok and optional CodexBar conformance/canaries
   remain green.
4. Historical route/import readers cannot create current eligibility or health.
5. Package dependency graph and deletion manifest show every retained low-level
   helper and its new owner.

### Verification and boundaries

Provider conformance/smart-routing/private canaries and dependency checks. No
autonomous scheduler/nested/CLI-wide deletion. Done when provider/routing have one
current writer and one compatibility reader boundary.

---

## V090-076: Remove autonomous compile, tick, trigger, and promotion entry points

**Metadata:** code, size M; depends on V090-036, V090-065, V090-071; labels
`v0.9.0`, `cleanup`, `orchestration`, `breaking-change`; exclusive in autonomous
entry points.

### Outcome and rationale

Remove v0.8 commands/services that compile ROADMAP markers, synthesize epic DAGs,
continuously tick/trigger work, or promote/merge without the explicit v0.9 direct
run/workflow and human gate. These paths caused accidental issue creation and
unbounded control loops during self-development.

### Scope and constraints

- Inventory `compile`, orchestration tick/trigger/conductor/promotion/risk-gate,
  autonomous CLI, schedules, and generated state callers.
- Delete production issue synthesis and default autonomous loops from v0.9.
- Preserve deterministic zero-model watchers only through their accepted facade.
- Keep explicit bounded workflow scheduler and human/release gates.
- Historical roadmap markers remain inert documentation.

### Acceptance criteria

1. No v0.9 command parses ROADMAP lifecycle markers or creates synthetic GitHub
   issues/graphs from epic headings.
2. No always-running tick/trigger loop can launch providers, mutate work, promote,
   merge, or release outside explicit direct/workflow invocation.
3. CI/approval/quota/host waits remain local, bounded, restartable, and zero-model.
4. Root help and capability matrix contain no autonomous/self-bootstrap support
   claim.
5. Direct/workflow/resume/human-gate canaries remain green after removal.

### Verification and boundaries

Command/dependency inventory and P3/P5 remote canaries. No provider/store/nested
deletion. Done when automatic execution cannot be entered accidentally.

---

## V090-077: Remove nested, federation, state-branch, and cross-machine lease systems

**Metadata:** code, size M; depends on V090-065, V090-068, V090-071; labels
`v0.9.0`, `cleanup`, `workflow`, `breaking-change`; exclusive in superseded
multi-agent/multi-machine ownership.

### Outcome and rationale

Remove old nested/federation/state-branch/conductor-lease machinery after bounded
Work Graph and terminal GitHub handoff prove the required behavior. v0.9 does not
support distributed local DB peers or concurrent multi-Mac ownership.

### Scope and constraints

- Inventory nested plan/scope/resource/recovery, agent federation tables/locks,
  state branches/publication, conductor leases, CLI/docs/spec callers.
- Delete execution/write paths and obsolete tables from new schema/code reachability;
  retain only explicit v0.8 export readers if required.
- Keep provider-native child containment and cross-provider WorkItems from P5.
- Keep terminal GitHub rehydration; remove state-DB push/pull/merge concepts.
- Update unsupported capability and migration reports.

### Acceptance criteria

1. Bounded workflow/native-child/cross-provider behavior uses only P5 graph/attempt
   ownership, not old nested/federation locks or state.
2. Cross-Mac continuation uses terminal GitHub evidence and fresh local state; no
   state branch, DB sync, or distributed lease remains supported.
3. Legacy records remain exportable/read-only where documented but cannot authorize
   current execution.
4. Package/schema/command inventory contains no unresolved duplicate workflow or
   cross-machine owner.
5. Workflow/restart/cross-Mac/migration canaries remain green after removal.

### Verification and boundaries

Dependency/schema/command inventory plus P5/cross-Mac/import remote canaries. No
provider/progress/CLI-wide cleanup. Done when the accepted topology is the only
execution ownership model.

---

## V090-078: Prune legacy CLI commands and superseded specifications

**Metadata:** code/docs, size M; depends on V090-072, V090-073, V090-074,
V090-075, V090-076, and V090-077; labels
`v0.9.0`, `cleanup`, `cli`, `documentation`; exclusive in public/compatibility
command and spec surface.

### Outcome and rationale

Remove command wiring, help, examples, specs, and generated artifacts for deleted
systems so users do not see dozens of unsupported choices or accidentally invoke
compatibility internals.

### Scope and constraints

- Use V090-071 inventory to keep only supported v0.9 commands and explicitly named
  read-only migration/compatibility commands.
- Delete handlers/options/help/completions/examples/specs for removed write paths.
- Move valuable historical architecture to clearly non-authoritative records or
  rely on Git history; no compiler-active markers remain.
- Update root help snapshots, generated docs, and command capability tests.
- Do not remove a command whose implementation owner has not passed its deletion
  issue and replacement evidence.

### Acceptance criteria

1. Root help leads only with the accepted v0.9 usable path and clearly groups the
   small remaining compatibility surface.
2. Removed commands cannot be invoked through aliases, hidden flags, generated
   completions, RPC/MCP wrappers, or stale docs.
3. No authoritative spec/document instructs self-bootstrap, active-slice compile,
   old autonomous loops, repo-local state, or distributed DB ownership.
4. Help/docs/command inventory is generated deterministically and matches the
   implemented capability matrix.
5. Migration/export and all P2-P5 product canaries remain green.

### Verification and boundaries

CLI/help/docs/link/generated inventory plus relevant remote canaries. No final
README rewrite or package dead-code sweep. Done when users cannot enter deleted
systems through public surfaces.

---

## V090-079: Final dependency, schema, and dead-code sweep after parity

**Metadata:** code, size M; depends on V090-072, V090-073, V090-074, V090-075,
V090-076, V090-077, and V090-078; labels
`v0.9.0`, `cleanup`, `dependencies`, `schema`; exclusive in final mechanical
unreachability/dead-code cleanup.

### Outcome and rationale

Prove all superseded owners are unreachable, then remove residual packages,
tables/migrations, generated assets, dependencies, flags, and tests that could not
be deleted until earlier bounded groups merged.

### Scope and constraints

- Generate before/after package/command/schema/dependency inventory from the
  disposition map and accepted deletion PRs.
- Remove unreachable residual code and dependencies in mechanically reviewable
  commits; preserve migration fixture readers and required license notices.
- Do not combine new behavior or semantics with the sweep.
- Keep old user files untouched; schema-code deletion is not a destructive DB
  migration.
- Re-run all accepted phase canaries on the exact merge SHA.

### Acceptance criteria

1. Disposition map has no unresolved current writer, autonomous entry point,
   repo-local fallback, or duplicate store/runtime/provider/router/report/workflow
   owner.
2. Build/dependency/command/schema inventories contain no unreachable deleted-system
   package, dependency, flag, generated asset, or writable table owner.
3. Required read-only migration fixtures and third-party notices remain present and
   verifiable.
4. No production behavior change appears beyond removal of already unsupported or
   compatibility-expired surfaces.
5. P1 through P5 and migration/private/multi-project/cross-Mac canaries pass on the
   exact merge SHA.

### Verification and boundaries

Mechanical inventory, build, required remote checks, and all acceptance canaries.
Revert if any retained path was still reachable. No new feature, docs quickstart,
packaging, or publication. Done when v0.9 has one coherent implementation graph.

---

## V090-080: Doctor, capability matrix, and README quickstart

**Metadata:** docs/code, size M; depends on V090-023, V090-036, V090-055,
V090-065, V090-079; labels `v0.9.0`, `documentation`, `doctor`, `ux`; exclusive in
public product guidance.

### Outcome and rationale

Make v0.9 understandable and usable by another customer without reading dozens
of internal commands. Documentation and doctor claims must match exact tested
platform/provider/UI/workflow/migration evidence.

### Scope and constraints

- Rewrite README around install, first-run doctor, explicit direct run, status,
  events/follow, cancel, providers, route explain, resume, and human merge gate.
- Publish capability matrix for Darwin arm64, provider adapters/quota sources,
  UI protocol/final-mile stages, direct/workflow modes, private repos, and
  migration.
- Document global/project storage, no repo-local state, backup/retention/privacy,
  cross-Mac terminal handoff, unsupported paths, and troubleshooting.
- Align `doctor --json` codes/remediation with docs and release artifact.
- Mark fixture-only/experimental/unknown capabilities honestly.

### Acceptance criteria

1. A new user can install a source build, run doctor, execute one explicit issue,
   watch status/events, cancel/resume, and identify the human gate from README.
2. Capability claims link to accepted canary/release evidence and distinguish
   real, fixture-only, degraded, optional, and unsupported.
3. Documentation contains no self-bootstrap/compile/active-slice workflow as the
   v0.9 development or default product path.
4. Storage/privacy/migration/backup/cross-Mac/cleanup limitations are explicit and
   consistent with code/errors.
5. Commands/examples pass a documentation smoke against the exact source tree and
   contain no personal paths, accounts, tokens, or private repository names.

### Verification and boundaries

Link/command/help/golden docs tests plus a novice runbook review. No marketing
claim beyond evidence. Done when another developer can operate v0.9 without the
historical roadmap or internal architecture docs.

---

## V090-101: Redacted diagnostic support bundle and no-telemetry default

**Metadata:** code/docs, size M; depends on V090-080 and V090-087; labels
`v0.9.0`, `diagnostics`, `privacy`, `support`; exclusive in support evidence.

### Outcome and rationale

Let a customer produce a bounded, reviewable diagnostic bundle when a run or UI
integration fails, without uploading private code, prompts, credentials, raw
provider output, or machine identity. External telemetry remains disabled by
default; support evidence is explicit and owner-controlled.

### Scope and constraints

- Add `loopcoder diagnose` inventory and optional archive modes with run/project
  filter, time range, size cap, dry-run manifest, and output destination.
- Include versions, capability matrix, schema/integrity summaries, redacted
  event/report/ack transitions, process/resource terminal evidence, check names,
  and typed diagnostics.
- Exclude source, issue/PR body, prompt, auth files, environment, absolute home
  paths, raw logs, tokens, and provider responses by default.
- Apply deterministic pseudonyms and secret/path/private-content scanners before
  archive creation; let the owner inspect the manifest first.
- Perform no network upload, analytics, crash reporting, or phone-home behavior.

### Acceptance criteria

1. Dry-run lists included/excluded classes, estimated size, redactions, and reason
   without creating an archive or reading prohibited files.
2. Bundle is bounded, self-describing, integrity-hashed, and contains only
   allowlisted fields.
3. Synthetic credentials, private text, prompts, absolute paths, usernames,
   hostnames, session IDs, and raw outputs are absent after scanning.
4. Missing/corrupt stores and permission failures produce partial diagnostics
   honestly without mutating or repairing authority.
5. Network instrumentation proves diagnose and default product make zero external
   telemetry/upload calls.

### Verification and boundaries

Golden bundle, secret/path/private-text canaries, corrupt store, permission,
size-cap, cancellation, and no-network tests. No automatic upload, remote support
service, source attachment, credential collection, or silent telemetry opt-in.

---

## V090-081: Darwin arm64 packaging, signing, and update metadata

**Metadata:** release, size M; depends on V090-001 and V090-080; labels `v0.9.0`,
`release`, `darwin-arm64`, `security`; exclusive in release artifact production.

### Outcome and rationale

Build one reproducible macOS Apple Silicon archive, generate checksums/SBOM,
sign/attest through the accepted release identity, and publish update metadata as
a draft without claiming Windows/Linux support.

### Scope and constraints

- Build once from an exact protected commit in a clean hosted environment.
- Include binary, license/notices, README/quickstart, version/commit metadata, and
  required static assets only.
- Generate SHA-256 checksums, SBOM, provenance/attestation/signature, and draft
  release metadata tied to one archive digest.
- Separate draft/build from publication approval; no local developer artifact may
  be promoted.
- Remove unsupported platform assets/links/installation claims.

### Acceptance criteria

1. One `darwin/arm64` archive is produced from the exact release candidate SHA and
   is never rebuilt between qualification and publication.
2. Binary reports version/commit/build source matching archive metadata.
3. Checksum, SBOM, license/notices, provenance, and accepted signature/attestation
   verify against the exact archive.
4. Release remains draft until V090-082/V090-083 and explicit publication
   approval.
5. No Windows/Linux artifact or support claim is generated.

### Verification and boundaries

Clean hosted build, reproducibility comparison where feasible, checksum/signature/
SBOM verification. Credentials remain in protected release environment and logs
are redacted. No publication, migration smoke, or consumer execution. Done when
one immutable draft artifact is ready for exact-artifact testing.

---

## V090-082: Exact-artifact install, migration, and cleanup smoke

**Metadata:** test/release, size M; depends on V090-070, V090-071, V090-081,
V090-086, V090-087, and V090-101; labels `v0.9.0`, `release`, `smoke`,
`migration`; exclusive in packaged artifact qualification.

### Outcome and rationale

Install and exercise the exact draft archive in clean and v0.8-upgrade consumer
environments. Source tests do not prove packaging, PATH, permissions, migration,
UI output, or process cleanup of what customers download.

### Scope and constraints

- Verify archive/checksum/signature, install, version, help, doctor, global layout,
  clean direct fixture run, status/events/cancel/resume, and uninstall guidance.
- Export/import representative v0.8 fixtures, inspect report, rerun idempotently,
  and verify old source remains unchanged.
- Exercise no-repo-state, private redaction markers, host final-mile capability,
  process/resource cleanup, and database integrity after abrupt interruption.
- Run only the exact V090-081 archive; never rebuild during smoke.
- Archive bounded redacted evidence and tested digest.

### Acceptance criteria

1. Fresh install from the exact archive completes doctor and direct fixture path
   without source checkout or repo-local runtime state.
2. Supported v0.8 export/import preserves source, is idempotent, reports omissions,
   and produces healthy v0.9 stores/history.
3. Cancellation/crash/reopen leaves zero owned child, released reservations,
   valid databases, and resumable delivery evidence.
4. Archive metadata/checksum/signature/digest exactly match the draft release and
   all evidence artifacts.
5. Uninstall/cleanup removes only documented LoopCoder global paths and never
   customer repositories, Git branches, commits, or PRs.

### Verification and boundaries

Clean Darwin arm64 VM/host plus upgrade fixture; no developer-source binary.
Failure rejects the candidate artifact; fix requires new SHA/archive and complete
rerun. No publication. Done when exact-artifact evidence is accepted.

---

## V090-102: Release SLO scorecard and GO/NO-GO evidence compiler

**Metadata:** docs/test, size S; depends on V090-024, V090-036, V090-055,
V090-065, V090-082, and V090-093; labels `v0.9.0`, `release`, `slo`, `evidence`; exclusive
in measurable release criteria.

### Outcome and rationale

Compile accepted canary and artifact evidence into one deterministic scorecard so
release is decided by product behavior, visibility, safety, and cleanup rather
than issue count, agent prose, or unrelated green checks.

### Scope and constraints

- Define thresholds for run-ID/start-report latency, report interval, rendered-
  ack latency, status freshness, stop/join, process leaks, repo-local state, route
  substitution, delivery replay, resources, redaction, migration, and artifact.
- Read only manifests tied to the candidate SHA/archive digest.
- Distinguish pass, fail, not-run, stale, unsupported, waiver-requested, and
  waiver-approved; no missing metric defaults to pass.
- Produce human and JSON scorecards with evidence links and calculation version.
- A waiver requires owner, rationale, scope, expiry, and documented release risk.

### Acceptance criteria

1. Same accepted manifests/policy produce byte-stable metric results and overall
   GO/NO-GO recommendation.
2. Missing, stale, wrong-SHA/digest, malformed, or threshold-failing evidence is
   explicit and cannot be hidden by another passing check.
3. Report metrics require real `rendered` evidence from terminal, generic bridge,
   and claimed Paseo profile; persistence alone never passes.
4. Zero route substitution, zero repo runtime state, and zero owned process leak
   are hard non-waivable v0.9.0 defaults.
5. Scorecard contains no private content, credentials, machine identity, raw logs,
   or personal paths.

### Verification and boundaries

Golden pass/fail/missing/stale/wrong-digest/waiver manifests and docs link tests.
No canary rerun, release publication, metric fabrication, or model-generated
release verdict. Done when V090-083 consumes one accepted scorecard.

---

## V090-083: Release-candidate consumer canary and GO/NO-GO record

**Metadata:** release/docs, size M; depends on V090-066, V090-067, V090-068,
V090-069, V090-070, V090-071, V090-072, V090-073, V090-074, V090-075,
V090-076, V090-077, V090-078, V090-079, V090-080, V090-081, V090-082,
V090-101, and V090-102; labels `v0.9.0`, `release`, `go-no-go`; exclusive final
publication gate.

### Outcome and rationale

Run the final consumer-oriented evidence set and produce one explicit owner
GO/NO-GO decision for publishing the single v0.9.0 release. Completion is based
on product usability and exact artifact truth, not issue count or worker reports.

### Scope and constraints

- Reconcile all 109 accepted outcomes or owner-approved scope-change records,
  required remote checks on exact SHA, open P0/P1 defects, security/advisory/SBOM,
  docs/capability claims, migration, and artifact smoke.
- Run bounded public/private, multi-project, explicit route, smart route, workflow,
  host visibility, cross-Mac terminal handoff, cancel/recovery, and cleanup canaries.
- Record artifact SHA/digest, evidence links, known limitations, deferred issues,
  rollback limitations, operator approval, and publication steps.
- Publish tag/release only after explicit GO and protected environment approval.
- Close historical roadmap/release tracking only after public asset verification.

### Acceptance criteria

1. Exact candidate SHA and exact archive pass all required checks, install,
   migration, direct, routing, workflow, private, multi-project, host, and cleanup
   gates with linked redacted evidence.
2. No unresolved P0/P1 defect, unexplained flake, leaked process, dual writer,
   repo-local runtime file, silent route substitution, or false host capability
   claim remains.
3. Known limitations and unsupported paths, including no self-bootstrap and no
   Windows/Linux support, are explicit in release notes/capability matrix.
4. GO/NO-GO record names approver, time, commit, artifact digest, evidence,
   rollback limitations, and publication authorization.
5. After GO, published tag/archive/checksum/signature/SBOM/notes are verified from
   the public endpoint and match the qualified artifact byte-for-byte.

### Verification, failure, and non-goals

The owner reviews evidence; release automation performs only authorized steps.
Any artifact/SHA change resets qualification. NO-GO leaves the release draft and
opens bounded defects without weakening gates. Self-bootstrap is explicitly not a
release criterion or post-publish automatic action. Done only after public asset
verification and the release is usable by an external consumer.
