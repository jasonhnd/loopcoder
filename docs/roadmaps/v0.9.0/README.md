# LoopCoder v0.9.0 Ordinary-Development Roadmap

Status: **PUBLISHED FOR ORDINARY DEVELOPMENT**; owner authorization is still
required to assign or implement each issue.

This document is the authoritative execution plan for v0.9.0. It supersedes the
earlier self-bootstrap Gate A-H execution model while retaining the accepted
product, storage, domain, and operational contracts.

Nothing in this document authorizes provider dispatch, implementation, merge,
release, or self-development. The reviewed GitHub catalog was published manually
after explicit owner approval. Publication did not assign work or authorize an
implementation route.

The live catalog is the [`v0.9.0` milestone](https://github.com/jasonhnd/loopcoder/milestone/4).
The stable ID-to-issue map is recorded in
[`published-issues.md`](published-issues.md).

## Decision Summary

v0.9.0 is developed as an ordinary software project:

- LoopCoder is the product being changed, not the controller changing itself.
- v0.8.1 is a predecessor and migration fixture, not the v0.9 controller.
- A human developer or explicitly selected external coding agent implements a
  reviewed issue in a normal branch or worktree.
- The owner selects the developer or coding agent. If an agent is used, the
  provider, model, and effort are owner inputs and must not be substituted.
- No task runs `loopcoder compile`, `loopcoder dispatch`, `loopcoder tick`, or
  another LoopCoder orchestration command against this repository.
- No roadmap marker creates, activates, closes, or reorders work.
- GitHub issue, PR, commit, check, review, and milestone state are the durable
  development record.
- Self-bootstrap is deferred until after v0.9.0 is production-usable and would
  require a new owner-approved plan.

The public release remains a single `v0.9.0`. The phases below are merge-order
and evidence checkpoints only. They are not intermediate versions or packages.

## Product Outcome

The first-screen user experience of v0.9.0 is one command:

```text
loopcoder run --repo <repo> --issue <number> \
  --provider <provider> --model <model> --effort <effort>
```

The direct path must:

1. resolve or register the project without writing runtime state into the repo;
2. freeze the GitHub issue and explicit route inputs;
3. admit one bounded worker process tree;
4. create one isolated worktree and branch;
5. expose start, state-change, five-minute, blocker, and terminal reports;
6. stop and join every owned process on completion or cancellation;
7. run only focused local checks;
8. commit, push, and open one PR idempotently;
9. wait for CI and approval with zero model calls;
10. run an independent verifier only after worker and delivery state are stable;
11. stop at a human merge gate by default; and
12. resume the exact failed stage after interruption without repeating completed
    provider or GitHub side effects.

Automatic provider/model selection is added only after this explicit-route path
is production-proven. Bounded workflows and sub-agents are added only after the
single-issue path is production-proven.

## Product Authority

| Concern | Authority | Not authority |
| --- | --- | --- |
| Requested work and collaboration | GitHub issue, PR, review, checks, and commits | Chat transcript or local task cache |
| Current local execution | Project SQLite events plus observed process state | Provider prose alone |
| Provider credentials | Provider CLI, keychain, or supported auth system | LoopCoder database |
| Provider quota | Fresh source evidence with provenance and confidence | Guessed remaining capacity |
| Route selection | Explicit operator pin, otherwise deterministic LoopCoder policy | A worker deciding its own provider |
| User-visible progress | A required UI client's proven `rendered` acknowledgement under `loopcoder.ui.v1` | SQLite write, hidden stderr, or transport acceptance |
| Cross-computer continuation | Terminal GitHub issue/PR/commit state | Copied local database or state branch |
| Merge and release | Protected remote evidence and explicit human/release gate | Worker exit code |

## Storage Topology

v0.9.0 uses pure-Go SQLite through `database/sql` and
`modernc.org/sqlite`. It does not use Dolt.

```text
$LOOPCODER_HOME/
  data/
    machine.db
  projects/
    <project-id>/
      project.db
      runs/
      logs/
      tmp/
      recovery/
```

`machine.db` owns machine-scoped facts:

- normalized project identities and local aliases;
- installed provider and model observations;
- quota windows and source provenance;
- provider/model health and cooldown;
- global CPU, RSS, process, worker, verifier, and local-test reservations; and
- schema and migration metadata for the machine store.

Each `project.db` owns only one project's execution facts:

- work items and dependencies;
- jobs and attempts;
- append-only events and current projections;
- route decisions and selected snapshot evidence;
- worktree, commit, PR, CI, verifier, and gate evidence;
- UI client registrations, delivery cursors, acknowledgements, attention, and
  authorized action receipts; and
- schema and migration metadata for that project store.

No LoopCoder database, hidden state directory, run sidecar, relay queue,
recovery file, log, or temporary payload may exist in a customer repository or
worktree. An explicitly user-authored repository policy file may be read, but
runtime operation must not mutate it.

The split is intentional. Provider and resource facts are shared across local
projects, while project execution corruption, deletion, backup, and retention
remain project-scoped. Route decisions copy the exact machine snapshot IDs and
digests they used into the project event stream; no cross-database transaction
is required to explain a past decision.

## Why SQLite, Not Dolt

Dolt is useful when independently writable database histories must branch,
merge, push, and pull like Git. That is not the v0.9 topology. The owner moves
between computers only after the active issue or PR is terminal, and GitHub is
the handoff surface. Another developer collaborates through ordinary GitHub
branches and PRs, not a shared LoopCoder scheduler database.

Adopting Dolt would add database commit semantics, server or embedded lifecycle,
remotes, credentials, branch/merge policy, garbage collection, backup, and
recovery work without solving a current requirement. The decision should be
reopened only if LoopCoder later requires concurrent offline writers to the
same work database whose table histories must merge.

## Reuse Before Rewrite

v0.8.1 contains valuable implementation and excessive duplication at the same
time. v0.9 follows these disposition verbs:

- **keep:** behavior and package can remain after a focused conformance audit;
- **reuse behind facade:** proven mechanics remain, but callers use a smaller
  v0.9-owned interface;
- **consolidate:** several packages become one responsibility and one writer;
- **compatibility-only:** old command remains for one release but is absent from
  the new path;
- **remove after parity:** delete only after replacement evidence and migration
  tests exist; and
- **reject:** do not carry the behavior into v0.9.

The package-level map is in
[`v0.8-disposition.md`](v0.8-disposition.md). No issue may create a second
implementation before naming the existing implementation it reuses or retires.

## External Research Boundary

The architecture was checked against pinned revisions of Gas City, Gas Town,
Beads, Plexus, Orca, CodexBar, Paseo, and Dolt. The accepted ideas and rejected
assumptions are recorded in
[`../../architecture/v0.9.0-external-inspiration-ledger.md`](../../architecture/v0.9.0-external-inspiration-ledger.md).

The implementation is an independent Go design. Source, tests, fixtures,
schemas, prompts, generated clients, and prose are not copied or translated.
Paseo's AGPL license receives an especially strict separation: LoopCoder may
interoperate through public behavior or an independently specified adapter, but
no Paseo implementation is moved into this MIT repository.

## Universal UI And Mandatory Report Contract

LoopCoder is a headless execution core. No desktop application, chat product,
terminal, or editor owns its execution truth. Every user interface consumes the
same public [`loopcoder.ui.v1` report and attention protocol](../../architecture/v0.9.0-ui-report-protocol.md):

- the terminal renderer is the reference UI and must work without another app;
- a local loopback HTTP/SSE plus JSON bridge is the generic integration for any
  desktop, web, editor, or agent host;
- Paseo is the first external conformance adapter, not a required dependency or
  privileged API;
- clients resume by durable cursor and acknowledge `accepted`, `rendered`, or
  `seen` only when they can prove that stage;
- an attempt cannot launch its provider until its required start report is
  durably persisted and rendered by at least one required client; and
- missed mandatory delivery creates durable attention and a bounded stop/detach
  decision. Silent continuation is never a successful or healthy state.

This is a product contract, not presentation polish. Provider execution, report
generation, follow/replay, and attention actions remain usable through the CLI
and generic bridge even when Paseo is absent.

## Ordinary Development Contract

### Issue size

Every implementation issue must satisfy all of these defaults:

- one primary user-visible or operator-visible behavior;
- one ownership boundary;
- no more than five acceptance criteria;
- one migration concern at most;
- one PR;
- expected effort from one half to two ordinary developer days;
- no more than two to five production files unless the issue is a mechanical
  deletion or fixture inventory;
- focused local verification only; and
- a delivery resume point that does not require rerunning completed provider
  work.

Split an issue before implementation when it spans two state machines, two
provider families, two independent host integrations, schema plus migration
plus feature behavior, or more than one public command.

### Required issue sections

Every issue draft in this roadmap contains:

1. outcome;
2. why now;
3. current evidence;
4. scope;
5. implementation constraints;
6. suggested implementation sequence;
7. acceptance criteria;
8. verification plan;
9. failure and rollback behavior;
10. privacy and security boundary;
11. resource ceiling;
12. non-goals;
13. dependencies; and
14. definition of done.

The implementation sequence is guidance, not permission to ignore evidence in
the current base. A developer must re-read the named packages and contracts at
the PR's actual base SHA.

### Work in progress

- One implementation PR may be active in a shared core ownership area.
- A second PR is allowed only when both issues explicitly declare disjoint
  packages, migrations, and generated artifacts.
- Provider-specific conformance PRs may proceed in parallel only after the core
  adapter contract and fixtures merge.
- No two writers share one worktree.
- A verifier reviews a stable commit and never runs concurrently with a worker
  that can still change that commit.
- Dependents are not opened as implementation PRs before their prerequisite
  contract and migration merge.

### Developer or agent selection

Ordinary development does not give LoopCoder route authority. The owner selects
the human or agent. For agent work, the handoff must state the exact provider,
model, effort, permission, base branch, issue number, and whether provider-native
sub-agents are permitted. The assignee must report the actual selection before
editing. A different provider/model requires renewed owner approval.

### Status during development

The current development host, not LoopCoder, must give a concise progress update
at least every five minutes while an agent is actively working. An update names
the current issue, stage, elapsed time, last concrete evidence, active process
count, remote gate, blocker, next timeout, and next action. Two intervals with no
concrete evidence require stopping or detaching the task and returning control.

This is a development safety rule. It does not count as proof that the v0.9
product report path works; product visibility is proven separately by P2
canaries.

## Verification Ownership

| Boundary | Evidence owned by that boundary |
| --- | --- |
| Developer | formatting, build of changed package, focused unit/integration tests |
| PR CI | repository test, affected-package race, verify, static/security checks |
| Merge SHA | protected remote integration result on the exact merged SHA |
| Release workflow | full race, one build, signing/checksum, exact-archive smoke |
| Consumer canary | install, first run, direct path, migration, private-repo redaction, cleanup |

Pre-push must complete within 60 seconds. It may run formatting, generated-file
checks, and a small deterministic sentinel. It must not run `go test ./...`, a
full race suite, provider probes, package installation, or release smoke.

Greptile Review is optional evidence. Required checks are derived from current
branch protection and the accepted release policy, not a hard-coded bot name.

## Local Resource Budget

Until v0.9 enforces the same limits itself, ordinary development obeys these
host limits:

| Resource | Default |
| --- | ---: |
| Active coding-agent processes | 1 |
| Active verifier processes | 0 while a coding agent is active |
| Provider-native sub-agents | 0 unless the issue explicitly allows them |
| Local test processes | 1 |
| Aggregate child processes for the task | 8 |
| Aggregate RSS for the task | 2 GiB |
| Sustained CPU for the task | 150 percent |
| Local command soft/hard deadline | 10 / 15 minutes |
| Automatic retry per failed stage | 1 |

Full repository tests, full race, security scans, package builds, signing, and
release smoke run remotely. An issue may request a real-host canary only when it
cannot be represented by a deterministic fixture and its time/resource budget
is stated in advance.

## Capability Gates And Phase Ownership

Phases group packages and issue files. They do not impose a single late
waterfall. Development advances through dependency-backed capability gates:

| Gate | Required accepted evidence | Unblocks |
| --- | --- | --- |
| F - Foundation | V090-001, V090-002, V090-003, V090-084, V090-085 | durable local authority |
| D - Durable authority | V090-004 through V090-011 | visible runtime foundations |
| U - Universal visibility | V090-012 through V090-023, V090-088 through V090-092, and V090-095 on the direct-path DAG | provider launch with mandatory reports |
| R - Visible direct run | V090-025 through V090-036 and V090-096 through V090-098 | first usable source build |
| H - Hardened multi-UI runtime | V090-024 | automatic provider routing |
| P - Provider/router | V090-037 through V090-055, V090-099, V090-103 through V090-109 | bounded workflows |
| W - Workflow | V090-056 through V090-065 and V090-100 | migration and final removal |
| X - Release | V090-066 through V090-083, V090-086, V090-087, V090-101, V090-102 | one public v0.9.0 artifact |

Only dependencies listed in the issue index are normative. A developer may
prepare a later draft, but implementation starts only when every named
predecessor is accepted. The first usable source-build checkpoint is Gate R;
Gate H then proves the longer multi-UI silent-worker case before automatic
routing can begin.

## Ordinary-Development Start Set

The first recommended issue is **V090-002, CI evidence tiers and pre-push time
budget**. It removes the development behavior that repeatedly ran heavy local
checks, blocked pushes, consumed the owner Mac, and obscured whether product
work or orchestration overhead was responsible. The change is narrow, has no
schema dependency, and makes every later ordinary-development issue safer.

The initial foundation closes in this dependency order:

1. V090-002 CI evidence tiers and pre-push budget;
2. V090-001 Darwin-only foundation conformance;
3. V090-084 threat/data/permission boundary;
4. V090-085 configuration authority and immutable effective-policy snapshot;
   and
5. V090-003 disposable acceptance fixture/evidence harness.

V090-001 and V090-002 have no technical dependency on V090-084 and may run in
parallel only when the owner deliberately assigns disjoint developers and the
resource budget permits it. The conservative default is the serial order above.
No P1 implementation starts until V090-003 is accepted. Creating these issues
does not authorize an agent, provider, model, or merge action.

## Catalog Integrity Rules

Before publication or after any roadmap edit, the catalog must prove:

- exactly 109 unique stable IDs in both the index and issue bodies;
- identical titles and explicit dependency sets in index and body;
- every dependency references an existing ID, with no self-edge or cycle;
- every issue contains one outcome/scope, exactly five acceptance criteria, and
  a verification boundary, with the global publication contract attached;
- no active compiler markers, personal paths, account identifiers, secrets, or
  private-repository content;
- all relative documentation links and every backticked current internal-package
  evidence path resolve at the reviewed base SHA; and
- `git diff --check` passes.

Any failed catalog check returns the roadmap to draft status. It is not waived
by a coding agent's prose or by unrelated green CI.

## Phase Plan

### P0 - Development foundation

Goal: prevent the development process from recreating the failures that made
v0.8 slow and unsafe.

Issues V090-001 through V090-003, V090-084, and V090-085:

- remove unsupported Windows/Linux work from the new v0.9 package path and
  audit the already-merged R1.3 foundation;
- make local checks small and remote evidence authoritative; and
- create disposable acceptance fixtures that prove behavior without modifying
  LoopCoder through LoopCoder;
- define data classes, trust boundaries, owner-only permissions, and hostile
  input behavior before storage or local transports expand; and
- freeze configuration precedence and preserve the effective policy snapshot
  that governed each attempt.

Exit evidence: platform inventory is clean, pre-push is under 60 seconds, all
four required hosted checks pass on the fixture harness, and no self-bootstrap
command was used.

### P1 - Local authority

Goal: establish one machine store, one store per project, stable project
identity, append-only events, ordered cursors, and restart integrity.

Issues V090-004 through V090-011.

Exit evidence: two repositories with the same short name but different owners
receive different project IDs; one repository moved on disk retains identity;
unregistered first run creates state under `$LOOPCODER_HOME`; a repository scan
finds no LoopCoder runtime files; duplicate append is idempotent; conflicting
append fails typed; cursor replay is ordered; crash/reopen preserves truth.

### P2 - Visible safe runtime

Goal: supervise one process tree and make its truthful state visible without
asking the provider to narrate progress.

Issues V090-012 through V090-024 and V090-088 through V090-094.

Exit evidence: the terminal, generic bridge, and independent conformance client
consume the same versioned reports; V090-093 separately proves the Paseo
external-client path for release evidence; a required start report is rendered
before provider launch; attention/action semantics are durable; and a
deterministic 12-minute silent worker emits start and five-minute reports,
remains within CPU/RSS/process/output budgets, can be cancelled, leaves no
child, replays from a cursor after UI disconnect, and never continues silently
after the configured delivery threshold.

### P3 - Direct run

Goal: make one explicitly selected issue/provider/model run reach one PR and
human gate reliably.

Issues V090-025 through V090-036 and V090-095 through V090-098.

Exit evidence: disposable documentation and Go-code repositories complete the
full direct path; the requested route equals the actual route; CI waiting uses
zero model calls; verifier starts only after stable worker delivery; a simulated
push timeout resumes delivery without rerunning the worker; cancellation cleans
all processes and retains recoverable evidence.

This is the first source-build usability checkpoint. It is not a public release
and it does not authorize self-bootstrap.

### P4 - Provider plane and deterministic router

Goal: discover the user's installed provider companies and model capabilities,
read supported quota windows honestly, and choose an eligible route through a
deterministic explainable policy.

Issues V090-037 through V090-055, V090-099, and V090-103 through V090-109.

The built-in v0.9 provider set is Codex, Claude Code, Gemini through the accepted
Gemini/Antigravity surfaces, and Grok. A future-provider kit proves that a fifth
provider can be added without scheduler edits. CodexBar is an optional bounded
observation source, never a required dependency or credential authority.

Routing order is:

1. installation, auth, permission, context, tool, host, and policy eligibility;
2. immutable explicit operator pin;
3. required Luna/Tera/Soul capability envelope and task risk;
4. fresh quota windows, reset time, and confidence;
5. configured reserve for high-risk work;
6. persistent cooldown and recent reliability;
7. local resource admission; and
8. deterministic tie-break.

The router does not simply choose the largest percentage. It computes burn
urgency only among eligible routes and prefers usable capacity that expires
sooner while protecting configured reserves and completion headroom. Unknown is
not zero, stale is not fresh, and unsupported quota is not fabricated.

Exit evidence: the same frozen inputs replay the same decision; every excluded
candidate has a reason; explicit Grok remains Grok; short and weekly windows are
separate; failover occurs only after the old process is stopped and joined;
ambiguous execution returns `needs-human`.

### P5 - Bounded workflows and sub-agents

Goal: add explicit dependency-aware workflows without weakening the proven
single-issue path.

Issues V090-056 through V090-065 and V090-100.

The default remains one issue and one worker. Workflow mode requires an
operator-visible materialized plan with depth <= 3, fan-out <= 3, and one active
top-level worker by default. Provider-native sub-agents remain descendants of
one attempt and never receive independent GitHub, branch, PR, merge, or task
authority. Cross-provider child work is represented as explicit WorkItems with
separate claims and worktree ownership.

Exit evidence: graph cycles and scope escapes fail before launch; ready-set and
claim are deterministic and atomic; wave execution may finish out of order but
integrates in declared order; parent cancellation stops descendants; child
completion cannot falsely close a parent; restart and compaction preserve
terminal provenance.

### P6 - Operations, migration, and release

Goal: make v0.9 safe for many local public/private repositories, sequential
cross-Mac handoff through GitHub, migration from v0.8.1, honest compatibility,
and one supported macOS arm64 release.

Issues V090-066 through V090-083, V090-086, V090-087, V090-101, and V090-102.
Legacy removal is deliberately divided into
eight issues: repo-local state, old storage writers, parallel progress/report
writers, duplicate provider/router writers, autonomous entry points,
nested/federation/cross-machine ownership, public CLI/spec pruning, and a final
mechanical dependency/schema sweep. Replacement and deletion never share one PR.

Exit evidence: project state and private data remain isolated; completed work
rehydrates from GitHub on a second Mac without copying SQLite; v0.8 export is
read-only; v0.9 import is planned, backed up, idempotent, and verifiable; old and
new writers never touch one work item; superseded packages are deleted only
after parity; README begins with the usable path; exact staged artifact passes
install, migration, consumer, cleanup, checksum/signature, and publication
verification.

## Stop Conditions

Development stops for owner review when:

- an issue no longer fits its stated behavior or file boundary;
- two implementation attempts fail for the same primary cause;
- a migration or external side effect is ambiguous;
- a developer wants to change the selected provider/model without approval;
- local resource ceilings are exceeded;
- required remote evidence is red for an unexplained reason;
- a PR needs to add a second source of truth;
- a host claim cannot be proven end to end;
- an upstream license or provenance boundary is uncertain;
- a P0/P1 defect appears outside the current issue; or
- two release candidates fail.

Stopping is a correct safety result. It is not permission to open a larger issue
or switch providers automatically.

## Release Scope

v0.9.0 supports native macOS Apple Silicon only. It does not support Windows,
Linux, WSL, containers as runtime hosts, Intel macOS, remote execution farms,
concurrent multi-Mac ownership of one active issue, provider credential custody,
a general API proxy, a full IDE, a mobile client, or automatic production merge
by default.

Self-bootstrap remains outside release acceptance. The release can be used in
consumer repositories after its exact artifact passes all P6 evidence. A future
self-development experiment starts from that released artifact under a new
roadmap, not from an in-progress v0.9 build.

## Effort Estimate

The 109 drafts intentionally expose the real size of the architecture
replacement. They are not 109 parallel work streams. The count increased after
final review split report delivery, UI compatibility, provider method families,
Git delivery, quota accounting, workflow integration, disaster recovery, and
legacy deletion into bounded ownership groups. Retaining the former large
issues would violate this roadmap's own issue-size rule.

- One developer, mostly serial: approximately 20 to 30 working weeks.
- Two developers with explicit disjoint ownership after durable authority:
  approximately 12 to 18 working weeks.
- Visible direct-run source-build checkpoint: approximately 7 to 11 working
  weeks.
- Stabilization and release qualification after feature completion: two to four
  additional weeks.

These are planning ranges, not deadlines. Reuse of proven v0.8 mechanics may
shorten them; hidden coupling, provider surface changes, or real-UI adapter
work may lengthen them. The roadmap forbids compressing the estimate by creating
oversized issues, running uncontrolled local parallelism, or hiding work in
provider-native sub-agents.

## Issue Catalog

The complete index is [`issue-index.md`](issue-index.md). GitHub-ready issue
bodies are split by phase under [`issues/`](issues/). The mandatory ordinary-
development, resource, reporting, privacy, PR, and publication rules are in the
[`issue publication contract`](issues/README.md). The contract is part of every
published issue even when a phase draft combines headings for readability.

Drafts by phase:

- [`P0 - Development foundation`](issues/phase-0-foundation.md)
- [`P1 - Local authority`](issues/phase-1-local-authority.md)
- [`P2 - Visible safe runtime`](issues/phase-2-visible-runtime.md)
- [`P3 - Direct run`](issues/phase-3-direct-run.md)
- [`P4 - Provider plane and deterministic router`](issues/phase-4-provider-router.md)
- [`P5 - Bounded workflows`](issues/phase-5-workflows.md)
- [`P6 - Operations and release`](issues/phase-6-operations-release.md)

They remain drafts only until the owner approves publication. Publication does
not authorize implementation; assignment is a separate owner action.
