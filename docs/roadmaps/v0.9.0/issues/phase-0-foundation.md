# v0.9.0 Issue Drafts: P0 Development Foundation

Status: development-ready issue drafts; owner publication/assignment required

Publish only after owner approval. These issues are ordinary GitHub development
work. They must not be compiled, dispatched, monitored, verified, or merged by
LoopCoder. The assignee must work from the current `pre-prod` base and recheck
all named evidence before editing.

## V090-001: Darwin-only scope cleanup and R1.3 foundation conformance

**Metadata**

- Milestone: `v0.9.0`
- Type: code and test
- Size: S, one ordinary developer day
- Depends on: none
- Suggested labels: `v0.9.0`, `foundation`, `platform`, `store`
- Parallelism: may run in parallel with V090-002; must finish before V090-003

### Outcome

Make the already-merged compact store foundation accurately support the v0.9
product platform: macOS on Apple Silicon only. Remove unsupported Windows/Linux
implementation from the new `internal/store` path, preserve the Darwin security
contract, and record a conformance baseline before later schemas depend on it.

### Why now

R1.3 merged a useful SQLite foundation, but it also added a substantial Windows
ACL implementation and cross-platform tests after the product scope had already
contracted to `darwin/arm64`. Carrying unsupported paths increases review, race,
and release cost and can make green cross-compilation look like product support.

### Current evidence

- `internal/store` contains schema metadata, migration ledger, integrity checks,
  owner-only permissions, and idempotent close behavior worth preserving.
- `internal/store/permissions_windows.go` and its tests account for hundreds of
  lines with no v0.9 release target.
- The current release intent supports one `darwin/arm64` artifact only.

### Scope

- Inventory build tags, platform files, tests, workflows, release scripts, and
  documentation that imply v0.9 Windows or Linux support.
- Remove Windows/Linux production code from `internal/store` and make unsupported
  builds fail with a deliberate, documented boundary rather than partial support.
- Preserve and tighten Darwin directory ownership, file mode, symlink, close,
  integrity, and migration-ledger tests.
- Add a small conformance test proving open, reopen, integrity check, and clean
  close on the supported platform.

### Implementation constraints

- Do not redesign schemas or introduce machine/project tables in this issue.
- Do not remove legacy v0.8 platform files outside the new v0.9 path unless they
  are release metadata that falsely advertises v0.9 support.
- Keep `modernc.org/sqlite`; do not add CGO or a server database.
- Unsupported-platform behavior must be explicit in source and docs.

### Suggested implementation sequence

1. Produce a tracked inventory of platform-specific files and CI/release claims.
2. Add or update Darwin conformance tests before deleting unsupported files.
3. Remove unsupported store code and narrow build/release declarations.
4. Run focused store tests and a Darwin compile; let remote CI own broad checks.

### Acceptance criteria

1. The v0.9 store path has no Windows or Linux permission implementation and no
   documentation or release metadata that claims those platforms are supported.
2. Darwin tests cover owner-only creation, unsafe path rejection, reopen,
   integrity check, migration-ledger stability, and idempotent close.
3. An unsupported target fails explicitly at build or startup with a stable
   unsupported-platform error; it does not silently run a weaker path.
4. The supported store opens without CGO and passes focused tests on Apple
   Silicon.
5. The PR includes a short conformance inventory naming what was kept, removed,
   and intentionally left as v0.8 compatibility code.

### Verification

- `gofmt` and `git diff --check`.
- Focused `internal/store` tests on Darwin, including reopen and unsafe paths.
- One unsupported-target compile assertion if it can complete quickly.
- Remote CI remains authoritative for repository-wide checks.

### Failure and rollback

This issue changes no persisted schema. Reverting the PR restores the previous
platform files. If Darwin behavior cannot be preserved without a broader store
redesign, stop and open a separate design correction; do not retain untested
Windows code as a workaround.

### Privacy and security

Tests use temporary directories and synthetic owners only. Diagnostics must not
print real usernames, home paths, environment values, or database contents.

### Resource ceiling

One developer process, one focused test process, no provider probes, no
sub-agents, and no full local race or repository suite.

### Non-goals

- Designing `machine.db` or `project.db` tables.
- Migrating v0.8 state.
- Restoring multiplatform release support.
- Changing provider, routing, worker, or host behavior.

### Definition of done

The accepted PR is merged to `pre-prod`, the exact merge SHA passes required
remote checks, and V090-003 can use the supported store without platform skips.

---

## V090-002: CI evidence tiers and pre-push time budget

**Metadata**

- Milestone: `v0.9.0`
- Type: code, workflow, and documentation
- Size: S, one ordinary developer day
- Depends on: none
- Suggested labels: `v0.9.0`, `foundation`, `ci`, `developer-experience`
- Parallelism: may run in parallel with V090-001; must finish before V090-003

### Outcome

Define and enforce a fast local evidence tier and authoritative remote evidence
tier so ordinary development does not freeze a developer's Mac or spend most of
its time rerunning repository-wide checks before every push.

### Why now

v0.8 development repeatedly ran `go test ./...`, full race tests, repeated CI
pollers, and long pre-push hooks on the local computer. That made small changes
take hours, created excess processes, and mixed developer evidence with release
evidence. A clear ownership boundary is required before v0.9 implementation.

### Current evidence

- Existing pre-push behavior can exceed the old delivery timeout.
- Full race jobs have taken tens of minutes and are inappropriate as a local
  push prerequisite.
- GitHub branch protection and the release workflow are the durable authority,
  not a local hook exit alone.
- Greptile Review is useful optional evidence but is not a required release gate.

### Scope

- Define `local-focused`, `pull-request`, `merge-sha`, `release-artifact`, and
  `consumer-canary` evidence tiers in contributor/release documentation.
- Limit pre-push to formatting, generated-file consistency, a deterministic
  sentinel, and checks that complete within 60 seconds on the reference Mac.
- Move repository-wide tests, affected-package race, static analysis, security,
  packaging, signing, and exact-archive smoke to their documented remote owner.
- Remove hard-coded waits for optional review bots and derive required checks
  from the current repository policy.

### Implementation constraints

- Do not weaken required GitHub or release checks.
- Do not hide failures by adding blanket `continue-on-error` or unconditional
  skips.
- Do not add a long-running local daemon or watcher.
- CI scripts must be directly runnable and must identify the exact SHA tested.

### Suggested implementation sequence

1. Measure the current pre-push commands on a clean checkout and classify them.
2. Write the evidence ownership table and required-check discovery rule.
3. Reduce the hook to a deterministic under-60-second sentinel.
4. Confirm every removed local check has a named remote owner and artifact.

### Acceptance criteria

1. Pre-push completes within 60 seconds on the reference Apple Silicon Mac for
   a no-op or documentation-only change and never runs `go test ./...` or a full
   race suite.
2. Required PR checks are determined from repository policy; an absent optional
   Greptile Review cannot block merge readiness.
3. Full test, race, security, package, signing, and exact-artifact smoke each
   have one documented authoritative remote stage.
4. Every check records the tested commit SHA, and release checks record the
   archive digest they exercised.
5. Contributor documentation explains what developers run locally and what
   evidence they must wait for remotely.

### Verification

- Time the pre-push sentinel three times on a clean reference checkout.
- Exercise required-check discovery with fixtures containing required, optional,
  pending, failed, and missing checks.
- Validate workflow syntax and trigger one ordinary PR run.

### Failure and rollback

If remote capacity is temporarily unavailable, do not move heavy gates back to
pre-push. Keep the PR unmergeable and document the infrastructure blocker.
Reverting this PR restores prior hooks but must be treated as a temporary safety
regression.

### Privacy and security

CI logs must redact credentials and local paths. Fixture check names and SHAs
are synthetic. No provider authentication or quota probe belongs in pre-push.

### Resource ceiling

One local shell, one sentinel test process, under 60 seconds, no provider calls,
no hosted-runner polling loop, and no sub-agents.

### Non-goals

- Changing product runtime resource admission.
- Removing required remote quality gates.
- Making Greptile Review a required check.
- Implementing release packaging.

### Definition of done

The accepted PR is merged, a normal PR demonstrates the new evidence tiers, and
the measured local budget is attached without host-identifying information.

---

## V090-084: Threat model, data classes, and permission enforcement boundary

**Metadata:** docs/code, size S; depends on none; labels `v0.9.0`, `security`,
`architecture`; may run with V090-001 and V090-002; owns the v0.9 threat and
enforcement inventory, not provider-specific implementation.

### Outcome and rationale

Define what LoopCoder trusts, what it persists, what it may execute, and which
existing enforcement mechanisms protect each boundary. The issue body, provider
output, repository content, Git configuration, hooks, environment, host/UI
messages, and recovered local records are all untrusted inputs. Security cannot
remain a repeated prose reminder added after schemas and commands are built.

### Scope and constraints

- Classify credentials, private source/issue content, operator reports, raw logs,
  paths, route evidence, quota observations, and public metadata.
- Map read-only, bounded-write, network, Git/GitHub, process-control, native
  delegation, and UI-action permissions to one deny-by-default capability model.
- Inventory reusable enforcement in `readonlyexec`, `writeexec`, `guardrails`,
  `sanitize`, Git environment isolation, and process supervision.
- Define prompt-injection, path/symlink, inherited environment, Git hook/config,
  localhost bridge, replay, and stale-authority threats.
- Do not create credentials, a sandbox product, or provider-specific policy.

### Acceptance criteria

1. Every v0.9 persisted/rendered field has a named data class, allowed scope,
   retention owner, and default redaction behavior.
2. Every mutation or external side effect maps to a required capability and an
   existing or planned enforcement owner; an unmapped capability fails closed.
3. Repository/issue/provider/UI content is explicitly untrusted and cannot alter
   immutable route, permission, base, project, or policy snapshots.
4. Threat fixtures cover path escape, symlink swap, poisoned `GIT_*`, hook/config
   redirection, secret-shaped output, forged UI ack, and stale replay action.
5. The document names unresolved enforcement gaps as blocking issue dependencies
   rather than declaring them safe by prose.

### Verification and boundaries

Review against current code at the implementation base, add small policy-vector
tests only where an existing enforcement API can consume them, and run focused
security tests remotely. Use synthetic paths/tokens. No real credential files,
private repository text, host conversations, provider calls, or broad refactor.
Done when V090-085 and V090-003 can reference one accepted security vocabulary.

---

## V090-085: Configuration authority, precedence, and immutable effective-policy snapshot

**Metadata:** code/docs, size M; depends on V090-084; labels `v0.9.0`, `config`,
`policy`, `provenance`; exclusive in effective configuration authority.

### Outcome and rationale

Create one versioned, explainable configuration resolver so route pins, resource
limits, report clients, retention, Git behavior, and project policy cannot be
silently changed by an environment variable, stale compatibility file, host
profile, or provider default after a run begins.

### Scope and constraints

- Define precedence: explicit CLI input, approved run request, project policy,
  user-local configuration, then compiled safe defaults.
- Freeze one redacted effective-policy snapshot and digest before side effects.
- Record value provenance and distinguish absent, defaulted, invalid, and
  compatibility-derived values.
- Keep credentials and provider auth outside LoopCoder configuration.
- Allow an optional reviewed project policy file only when its location and Git
  tracking behavior are explicit; runtime state remains outside the repository.

### Acceptance criteria

1. The same versioned inputs produce the same normalized effective configuration,
   provenance, digest, and validation result.
2. Explicit provider/model/effort/permission/report-client inputs cannot be
   overridden by environment, host detection, automatic routing, or defaults.
3. Unknown keys, incompatible schema versions, unsafe paths, and invalid limits
   fail typed before provider, worktree, UI bridge, or GitHub side effects.
4. The persisted run snapshot is immutable; an approved configuration change
   creates a successor attempt or new run rather than rewriting history.
5. Human and JSON explain output names every effective value's source without
   exposing credentials, private content, or host-identifying absolute paths.

### Verification and boundaries

Use table-driven precedence/conflict/redaction vectors, deterministic config
files under disposable homes, and remote race for concurrent readers. Rollback
removes only the new resolver; no migration writes legacy config. No routing
score, provider probe, report transport, or credential management. Done when all
later issues consume one immutable effective-policy snapshot.

---

## V090-003: v0.9 acceptance fixture and evidence harness

**Metadata**

- Milestone: `v0.9.0`
- Type: test infrastructure
- Size: M, up to two ordinary developer days
- Depends on: V090-001, V090-002, V090-084, V090-085
- Suggested labels: `v0.9.0`, `foundation`, `testing`, `fixtures`
- Parallelism: exclusive in fixture/harness ownership; blocks P1

### Outcome

Create deterministic disposable fixtures that later issues can use to prove
store, process, host, Git, and GitHub behavior without invoking a real model or
using LoopCoder to develop itself.

### Why now

The earlier development process discovered defects only after real provider
runs, temporary worktrees, long CI waits, and manual recovery. That makes failures
expensive and ambiguous. v0.9 needs reusable acceptance evidence before adding
new state machines.

### Current evidence

- Existing tests contain useful fakes but no single ordinary-development harness
  for the v0.9 direct path.
- Real provider output is not deterministic and cannot be a unit-test oracle.
- Host visibility, push timeout, duplicate delivery, process-tree cleanup, and
  crash/reopen all need controllable fault injection.

### Scope

- Add fixture repositories for documentation-only and small Go-code changes,
  each with deterministic issue, branch, commit, PR, and check data.
- Add a fake provider executable that can emit output, remain silent, spawn a
  child, exit, hang, ignore graceful stop, and return a fixed completion record.
- Add fake GitHub and UI clients with ordered evidence, replay cursors, duplicate
  calls, timeouts, disconnects, and acknowledgements.
- Add injected clock, filesystem, process-observer, and failure-point helpers
  that later packages can reuse without sleeping on wall clock.
- Define an evidence manifest format that records scenario, inputs, expected
  events, process cleanup, side effects, and tested SHA.

### Implementation constraints

- The harness must not import production package internals in ways a real caller
  cannot; prefer public or package-local test ports.
- No fixture may contact GitHub, a model provider, keychain, browser, or network.
- Time-based tests use injected clocks and explicit synchronization, not timing
  margins.
- Test repositories and output stay under test temp directories.
- Keep the harness composable; do not build a second orchestration framework.

### Suggested implementation sequence

1. Specify the scenario/evidence manifest and minimal test ports.
2. Add fake provider and process-tree modes.
3. Add repository, GitHub, host, clock, and failure fixtures.
4. Prove one golden scenario and one failure/resume scenario in remote CI.

### Acceptance criteria

1. A test can create a disposable repo, run a deterministic fake worker, and
   assert ordered events, Git side effects, host acknowledgements, and zero
   surviving children without network or provider access.
2. Fixture controls cover silence, child spawn, output flood, nonzero exit,
   graceful-stop refusal, UI disconnect, push timeout, and duplicate request.
3. The harness uses injected time or explicit barriers; its acceptance tests
   contain no correctness dependency on wall-clock sleeps.
4. Every scenario emits a bounded evidence manifest tied to the tested commit
   and contains no machine-specific paths or credentials.
5. Focused local tests fit the V090-002 budget and the required hosted checks
   pass on the exact PR head.

### Verification

- Run the harness golden and fault scenarios repeatedly in focused tests.
- Run affected-package race remotely.
- Scan fixtures and manifests for absolute user paths, tokens, and environment
  leakage.
- Confirm tests pass with network access disabled.

### Failure and rollback

The harness writes no product state. If a fixture API proves too broad, revert
or split it before product packages depend on it. A flaky scenario blocks P1;
do not add retries that conceal nondeterminism.

### Privacy and security

All identities, repository names, issue content, tokens, paths, and provider
responses are synthetic. Fixtures must actively clear inherited Git/provider
environment variables before spawning commands.

### Resource ceiling

One test process, at most eight fixture child processes, under 512 MiB RSS for
focused scenarios, no provider calls, no sub-agents, and no local full suite.

### Non-goals

- Implementing machine or project schemas.
- Testing real provider authentication or quota.
- Acting as a production process supervisor.
- Running LoopCoder against its own repository.

### Definition of done

The harness PR is merged, required remote checks pass, its public test helpers
are documented, and P1 issues can cite stable fixture APIs instead of creating
new one-off fakes.
