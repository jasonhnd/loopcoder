# LoopCoder v0.9.0 Issue Index

Status: **READY FOR CATALOG PUBLICATION AND ORDINARY DEVELOPMENT**; assignment
and implementation remain owner-controlled.

This index is the merge-order map for the ordinary-development v0.9.0 program.
It is not input to `loopcoder compile`, and no row authorizes issue publication,
provider dispatch, implementation, merge, or release.

Every issue has one primary behavior, one PR, at most five acceptance criteria,
and an expected size of one half to two ordinary developer days. Dependencies
mean that the named issue's accepted PR must be merged before implementation
starts. An issue may be drafted or reviewed earlier.

Size guide:

- **XS:** up to one ordinary developer day, usually one package or documentation
  boundary;
- **S:** one to one-and-a-half days, usually one state transition or adapter; and
- **M:** up to two days, usually one bounded integration with deterministic
  fixtures.

## P0 - Development Foundation

| ID | Title | Kind | Size | Depends on | Primary boundary |
| --- | --- | --- | --- | --- | --- |
| V090-001 | Darwin-only scope cleanup and R1.3 foundation conformance | code | S | none | `internal/store`, platform inventory |
| V090-002 | CI evidence tiers and pre-push time budget | code/docs | S | none | hooks and CI policy |
| V090-084 | Threat model, data classes, and permission enforcement boundary | docs/code | S | none | security contract and enforcement inventory |
| V090-085 | Configuration authority, precedence, and immutable effective-policy snapshot | code/docs | M | V090-084 | config loading and provenance |
| V090-003 | v0.9 acceptance fixture and evidence harness | code | M | V090-001, V090-002, V090-084, V090-085 | deterministic test fixtures |

Checkpoint: ordinary development can change and verify the product without
self-bootstrap, unsupported-platform work, unbounded local checks, or hidden
real-provider dependencies.

## P1 - Local Authority

| ID | Title | Kind | Size | Depends on | Primary boundary |
| --- | --- | --- | --- | --- | --- |
| V090-004 | Machine-store and project-store topology contract | code | S | V090-003 | store facade and open options |
| V090-005 | Global home layout and owner-only directory creation | code | S | V090-004 | `$LOOPCODER_HOME` paths |
| V090-006 | Stable project identity, aliases, and automatic registration | code | M | V090-005 | project registry |
| V090-007 | Machine authority store for provider and resource facts | code | M | V090-004, V090-005 | `machine.db` schema |
| V090-008 | Project authority store and append-only event schema | code | M | V090-004, V090-006 | `project.db` schema |
| V090-009 | Atomic event append and idempotency contract | code | S | V090-008 | event writer |
| V090-010 | Ordered cursor replay and projection checkpoints | code | M | V090-009 | event reader/projection |
| V090-011 | SQLite contention, restart, integrity, and backup behavior | code | M | V090-005, V090-007, V090-010 | store resilience |

Checkpoint: all runtime state lives under `$LOOPCODER_HOME`, project identity
survives path changes, append/replay is deterministic, and crash/reopen retains
authoritative state.

## P2 - Visible Safe Runtime

| ID | Title | Kind | Size | Depends on | Primary boundary |
| --- | --- | --- | --- | --- | --- |
| V090-012 | Runtime facade over existing agent and supervised execution | code | S | V090-011 | runtime interface |
| V090-013 | Darwin process-tree identity and liveness | code | M | V090-012 | process ownership |
| V090-014 | Bounded output capture and log lifecycle | code | S | V090-012 | stdout/stderr/log retention |
| V090-015 | CPU, RSS, and process-count sampling | code | S | V090-013 | runtime telemetry |
| V090-016 | Machine-global admission and resource reservations | code | M | V090-007, V090-015 | host resource authority |
| V090-017 | Stop, join, escalation, and guardian cleanup | code | M | V090-012, V090-013, V090-016 | termination lifecycle |
| V090-018 | Recovery and adoption under ambiguous process authority | code | M | V090-010, V090-013, V090-017 | restart recovery |
| V090-019 | Evidence collectors for runtime and delivery progress | code | M | V090-010, V090-012, V090-014, V090-015 | truthful evidence |
| V090-020 | Five-minute report scheduler and no-progress policy | code | S | V090-019 | progress timing |
| V090-021 | Current status projection and cursor-based event follow | code | M | V090-010, V090-019, V090-020 | status/events CLI core |
| V090-022 | UI-neutral report envelope and human view model | code/docs | M | V090-021 | `loopcoder.ui.v1` report projection |
| V090-023 | Durable UI subscription, cursor, and acknowledgement ledger | code | M | V090-010, V090-022 | report delivery authority |
| V090-088 | Terminal reference UI and bounded human/JSONL rendering | code | S | V090-022, V090-023 | reference UI client |
| V090-089 | Local HTTP/SSE UI bridge and capability handshake | code | M | V090-022, V090-023 | generic UI transport |
| V090-090 | Attention lifecycle and authorized operator action API | code | M | V090-010, V090-022 | attention authority |
| V090-091 | Required report-client gate, delivery degradation, and fallback policy | code | M | V090-088, V090-089, V090-090 | delivery enforcement |
| V090-092 | Generic UI conformance runner and golden transcripts | test/docs | M | V090-088, V090-089, V090-090, V090-091 | UI compatibility contract |
| V090-093 | Paseo reference adapter and real public-surface smoke | code/test | M | V090-092 | first external UI client |
| V090-094 | Foreground and explicit-detach supervisor ownership | code | M | V090-017, V090-018, V090-023, V090-091 | run attachment lifecycle |
| V090-024 | Twelve-minute silent-worker multi-UI visibility and cleanup canary | test | M | V090-016, V090-017, V090-018, V090-019, V090-020, V090-021, V090-036, V090-092, V090-094 | hardened visibility acceptance |

Checkpoint: a silent worker is continuously observable without provider polls,
is bounded and cancellable, survives host reconnect, and leaves no owned child.

## P3 - Direct Run

| ID | Title | Kind | Size | Depends on | Primary boundary |
| --- | --- | --- | --- | --- | --- |
| V090-095 | Minimal provider execution contract and reference adapter | code | M | V090-003, V090-012 | direct-path provider boundary |
| V090-025 | `loopcoder run` command contract and CLI shell | code/docs | S | V090-010, V090-012, V090-022, V090-023, V090-088, V090-089, V090-095 | primary command |
| V090-026 | First-run doctor and project preflight | code | S | V090-006, V090-007, V090-010, V090-085, V090-095 | readiness check |
| V090-027 | GitHub issue intake and immutable policy snapshot | code | M | V090-025, V090-026 | work request intake |
| V090-028 | Immutable explicit provider, model, and effort pin | code | S | V090-027, V090-095 | route input authority |
| V090-029 | Idempotent worktree and branch claim | code | M | V090-006, V090-027 | Git workspace ownership |
| V090-030 | Worker attempt lifecycle on the direct path | code | M | V090-012, V090-017, V090-019, V090-028, V090-029, V090-091 | worker execution |
| V090-031 | Focused local verification plan | code | S | V090-030 | local check selection |
| V090-032 | Idempotent local commit stage | code | S | V090-030, V090-031 | local Git delivery |
| V090-096 | Customer Git-hook policy and bounded hook reconciliation | code/docs | S | V090-029, V090-031 | hook execution policy |
| V090-097 | Idempotent remote branch push stage | code | S | V090-032, V090-096 | remote Git delivery |
| V090-098 | Idempotent pull-request creation and reconciliation | code | S | V090-027, V090-097 | GitHub PR delivery |
| V090-033 | Zero-model CI and approval watcher | code | M | V090-021, V090-098 | remote wait state |
| V090-034 | Independent verifier and human merge gate | code | M | V090-033, V090-098 | verification authority |
| V090-035 | Delivery-only resume without worker replay | code | M | V090-018, V090-033, V090-034, V090-097, V090-098 | stage recovery |
| V090-036 | Documentation and Go-code visible direct-path canaries | test/docs | M | V090-025, V090-026, V090-027, V090-028, V090-029, V090-030, V090-031, V090-032, V090-033, V090-034, V090-035, V090-088, V090-089, V090-090, V090-091, V090-092, V090-095, V090-096, V090-097, V090-098 | first usable source-build checkpoint |

Checkpoint: one explicit issue/provider/model route reaches one stable PR and
human gate, and interrupted delivery resumes without repeating provider work.

## P4 - Provider Plane And Deterministic Router

| ID | Title | Kind | Size | Depends on | Primary boundary |
| --- | --- | --- | --- | --- | --- |
| V090-037 | Provider descriptor registry and conformance harness | code | M | V090-024, V090-095 | provider SPI |
| V090-038 | Ordered observation-source plans and provenance snapshots | code | M | V090-037 | inventory evidence |
| V090-039 | Adaptive refresh, health, and cooldown state | code | M | V090-038 | provider availability |
| V090-040 | Codex discovery and model-catalog consolidation | code | S | V090-037, V090-038, V090-039 | Codex observation adapter |
| V090-103 | Codex invocation consolidation | code | S | V090-040, V090-095 | Codex execution adapter |
| V090-041 | Codex quota-window adapter | code | S | V090-040 | Codex quota evidence |
| V090-042 | Claude Code discovery and model-catalog consolidation | code | S | V090-037, V090-038, V090-039 | Claude observation adapter |
| V090-104 | Claude Code invocation consolidation | code | S | V090-042, V090-095 | Claude execution adapter |
| V090-043 | Claude Code quota-window adapter | code | S | V090-042 | Claude quota evidence |
| V090-044 | Gemini CLI discovery and model-catalog consolidation | code | S | V090-037, V090-038, V090-039 | Gemini observation adapter |
| V090-105 | Gemini CLI invocation consolidation | code | S | V090-044, V090-095 | Gemini execution adapter |
| V090-045 | Gemini CLI quota-window adapter | code | S | V090-044 | Gemini quota evidence |
| V090-106 | Antigravity discovery and model-catalog adapter | code | S | V090-037, V090-038, V090-039 | Antigravity observation adapter |
| V090-107 | Antigravity invocation consolidation | code | S | V090-095, V090-106 | Antigravity execution adapter |
| V090-108 | Antigravity quota-window adapter | code | S | V090-106 | Antigravity quota evidence |
| V090-046 | Grok discovery and model-catalog consolidation | code | S | V090-037, V090-038, V090-039 | Grok observation adapter |
| V090-109 | Grok invocation consolidation | code | S | V090-046, V090-095 | Grok execution adapter |
| V090-047 | Grok quota-window adapter | code | S | V090-046 | Grok quota evidence |
| V090-048 | Optional CodexBar observation bridge | code | S | V090-038, V090-041, V090-043, V090-045, V090-047, V090-108 | optional telemetry source |
| V090-049 | Future-provider registration kit | code/docs | S | V090-037 | fifth-provider extension |
| V090-050 | Task risk classes and Luna, Tera, and Soul capability mapping | code/docs | S | V090-036 | task-to-capability policy |
| V090-051 | Hard eligibility and immutable-pin precedence | code | M | V090-028, V090-037, V090-039, V090-050 | route admission |
| V090-052 | Quota normalization, burn urgency, reserve, and reliability policy | code | M | V090-041, V090-043, V090-045, V090-047, V090-051, V090-108 | quota policy |
| V090-099 | Quota policy modes, soft reservations, and usage attribution | code | M | V090-016, V090-052 | machine-wide quota spending authority |
| V090-053 | Persisted route decision and `route explain` | code | M | V090-038, V090-051, V090-052, V090-099 | route decision |
| V090-054 | Successor attempt and fallback boundary | code | M | V090-032, V090-053 | failover lifecycle |
| V090-055 | Smart-routing end-to-end acceptance canary | test/docs | M | V090-040, V090-041, V090-042, V090-043, V090-044, V090-045, V090-046, V090-047, V090-049, V090-050, V090-051, V090-052, V090-053, V090-054, V090-099, V090-103, V090-104, V090-105, V090-106, V090-107, V090-108, V090-109 | phase acceptance |

Checkpoint: installed companies and models are discovered with provenance;
explicit pins are immutable; automatic routes are deterministic, explainable,
quota-aware, and replayable.

## P5 - Bounded Workflows

| ID | Title | Kind | Size | Depends on | Primary boundary |
| --- | --- | --- | --- | --- | --- |
| V090-056 | Work Graph public contract and materialization boundary | code/docs | S | V090-055 | workflow API |
| V090-057 | Work item and dependency schema | code | M | V090-011, V090-056 | project graph storage |
| V090-058 | Graph validation and deterministic ready set | code | M | V090-057 | dependency semantics |
| V090-059 | Atomic work claim and guarded close | code | M | V090-057, V090-058 | ownership transition |
| V090-060 | Explicit workflow definition and materialization | code | M | V090-056, V090-058 | workflow creation |
| V090-061 | Deterministic bounded-wave scheduling | code | M | V090-059, V090-060 | bounded scheduler |
| V090-100 | Ordered integration receipts and conflict boundary | code | M | V090-032, V090-061, V090-098 | integration authority |
| V090-062 | Provider-native sub-agent containment and resource aggregation | code | M | V090-016, V090-017, V090-019, V090-060 | native children |
| V090-063 | Cross-provider child-attempt isolation | code | M | V090-037, V090-053, V090-059, V090-062 | routed children |
| V090-064 | Workflow cancellation, restart, and terminal compaction | code | M | V090-010, V090-017, V090-059, V090-061, V090-062, V090-063, V090-100 | workflow recovery |
| V090-065 | Bounded-workflow end-to-end acceptance canary | test/docs | M | V090-061, V090-062, V090-063, V090-064, V090-100 | phase acceptance |

Checkpoint: explicit small graphs use one writer per worktree, bounded waves,
contained children, ordered integration, deterministic restart, and full cleanup.

## P6 - Operations And Release

| ID | Title | Kind | Size | Depends on | Primary boundary |
| --- | --- | --- | --- | --- | --- |
| V090-066 | Multi-project global admission and isolation | code/test | M | V090-016, V090-055, V090-065 | machine-wide operations |
| V090-067 | Private-repository redaction and consumer canary | code/test | M | V090-036, V090-055 | privacy boundary |
| V090-068 | Cross-Mac GitHub rehydration after terminal handoff | code/test | M | V090-006, V090-032, V090-036 | machine handoff |
| V090-086 | Machine-authority rebuild and reservation reconciliation | code/test | M | V090-006, V090-007, V090-011, V090-066 | machine disaster recovery |
| V090-087 | Event, log, runtime-file retention and garbage collection | code/docs | M | V090-010, V090-014, V090-064, V090-086 | bounded local lifecycle |
| V090-069 | Read-only v0.8 state exporter | code | M | V090-011 | legacy extraction |
| V090-070 | v0.9 project-state importer and migration report | code | M | V090-011, V090-069 | migration |
| V090-071 | Compatibility shims and old/new writer isolation | code | M | V090-070 | compatibility boundary |
| V090-072 | Remove repository-local runtime fallbacks and sidecars | code | S | V090-005, V090-006, V090-071 | repo-local state removal |
| V090-073 | Retire legacy v0.8 storage mutation paths | code | M | V090-069, V090-070, V090-071, V090-072 | old store writer removal |
| V090-074 | Retire parallel progress, report, relay, and outbox lifecycle writers | code | M | V090-023, V090-036, V090-071 | progress writer removal |
| V090-075 | Retire duplicate provider inventory, invocation, and router writers | code | M | V090-055, V090-071 | provider/router writer removal |
| V090-076 | Remove autonomous compile, tick, trigger, and promotion entry points | code | M | V090-036, V090-065, V090-071 | autonomous entrypoint removal |
| V090-077 | Remove nested, federation, state-branch, and cross-machine lease systems | code | M | V090-065, V090-068, V090-071 | old ownership removal |
| V090-078 | Prune legacy CLI commands and superseded specifications | code/docs | M | V090-072, V090-073, V090-074, V090-075, V090-076, V090-077 | public surface removal |
| V090-079 | Final dependency, schema, and dead-code sweep after parity | code | M | V090-072, V090-073, V090-074, V090-075, V090-076, V090-077, V090-078 | mechanical cleanup |
| V090-080 | Doctor, capability matrix, and README quickstart | docs/code | M | V090-023, V090-036, V090-055, V090-065, V090-079 | product documentation |
| V090-101 | Redacted diagnostic support bundle and no-telemetry default | code/docs | M | V090-080, V090-087 | supportability and privacy |
| V090-081 | Darwin arm64 packaging, signing, and update metadata | release | M | V090-001, V090-080 | release artifact |
| V090-082 | Exact-artifact install, migration, and cleanup smoke | test/release | M | V090-070, V090-071, V090-081, V090-086, V090-087, V090-101 | release qualification |
| V090-102 | Release SLO scorecard and GO/NO-GO evidence compiler | docs/test | S | V090-024, V090-036, V090-055, V090-065, V090-082, V090-093 | measurable release decision |
| V090-083 | Release-candidate consumer canary and GO/NO-GO record | release/docs | M | V090-066, V090-067, V090-068, V090-069, V090-070, V090-071, V090-072, V090-073, V090-074, V090-075, V090-076, V090-077, V090-078, V090-079, V090-080, V090-081, V090-082, V090-101, V090-102 | publication gate |

Checkpoint: the exact signed `darwin/arm64` archive works across public/private
repositories and multiple local projects, imports supported v0.8 state, leaves
no children or repo-local runtime files, and has an explicit GO/NO-GO record.

## Publication And Ordering Rules

1. The owner may publish the complete reviewed catalog as planned milestone
   entries. Phase files organize ownership; they do not authorize implementation
   of an entire phase at once.
2. Catalog publication is not activation. Issues with unmet dependencies receive
   `status:planned`; only dependency-ready issues receive `status:ready`, and
   assignment still requires a separate owner decision.
3. No implementation starts merely because an issue exists. The owner selects
   the developer or agent, provider, model, effort, permissions, and base.
4. Do not use LoopCoder to create, route, dispatch, monitor, verify, or merge any
   v0.9 issue.
5. A dependency change requires an explicit roadmap PR; do not silently skip a
   predecessor in an implementation PR.
6. If an issue exceeds two days, five acceptance criteria, one state machine,
   or one primary package boundary, stop and split it before continuing.
7. Capability canaries are hard checkpoints. The direct-run canary V090-036 is
   the first usable source-build checkpoint. The longer multi-UI canary V090-024
   must then pass before provider-plane work starts. Later work follows the
   explicit dependency DAG, not numerical phase order alone.
