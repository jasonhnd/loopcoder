# Provider And Host Runtime Capabilities

This document is loopcoder's runtime capability contract. It describes what the
native binary expects from provider CLIs that run Worker, Verifier, and audit
LLM invocations, and what it expects from agent hosts that invoke `loopcoder` as
the Conductor.

This is a static declaration and compatibility contract. A `yes` capability or
`supported` compatibility row does not prove a reachable v0.8.0 product path,
a successful real-provider invocation, or production support. The binding
release status is the
[`v0.8.0 capability and support matrix`](v0.8.0-capability-matrix.md).

This contract is separate from Worker and Verifier model selection. Provider,
model, and depth still resolve from command flags, `.delivery.yml`, and the
static model registry. Runtime capabilities describe whether the selected
provider or host can safely satisfy a requested invocation mode.

Host negotiation has two independent axes:

1. **Host invocation capability**: whether the current host can run the
   foreground `loopcoder` subprocess, preserve stdout/stderr, pass JSON through
   unchanged, and allow loopcoder's own timeout/cancellation supervision to
   work.
2. **Progress transport capability**: whether the host declares durable
   polling, resumable follow, callbacks, wake-up, acknowledgment, host-managed
   background work, detached steering/cancellation, and explicit payload/rate
   limits for progress delivery.

A host profile name or environment marker can only propose a profile. Active
callback, wake-up, acknowledgment, and detached control support require
declared handshake evidence in the versioned host negotiation request. LoopCoder
must not infer those capabilities from `codex-cli`, `claude-code`, a process
environment variable, or the Worker/Verifier provider choice.

Future provider declarations use the same contract and must follow
[`future-provider-adapters.md`](future-provider-adapters.md) before they can
participate in provider inventory or later routing.

## Provider Capability Fields

Each provider runtime is represented internally with these fields:

| Field | Meaning |
| --- | --- |
| `name` | Stable provider key used by loopcoder commands, such as `codex`. |
| `executable` | Local command that must resolve on `PATH`, such as `codex` or `agy`. |
| `read_only` | Provider has a verified local read-only mode suitable for `loopreview` and audit review. |
| `nested_subagents` | Provider can expose nested sub-agent behavior when loopcoder intentionally enables it. |
| `json_output` | Provider can return machine-readable JSON or accept schema-enforced structured output for verifier-style calls. |
| `mcp_config` | Provider adapter can inject selected MCP server configuration for the invocation. |
| `cancellation` | Provider process can be cancelled by loopcoder's context, timeout, or kill-group supervision. |
| `token_usage_reporting` | Provider output exposes stable parseable token usage, or loopcoder has a verified parser for it. |
| `provider_idempotency` | Provider has a native request idempotency mechanism that loopcoder can pass for a logical child operation. |
| `auth_probe_command` | Optional read-only command that can check provider authentication readiness. |
| `known_limitations` | Human-readable limitations that must appear in actionable failure paths when relevant. |

Unsupported capabilities must fail before provider launch when loopcoder can
detect the mismatch locally. The error should name the provider, the missing
capability, supporting alternatives when known, and the local fix.

## Provider Compatibility

| Provider | Executable | Read-only | Nested sub-agents | JSON output | MCP config | Cancellation | Token usage | Auth probe | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `codex` | `codex` | yes | no | yes | yes | yes | yes | `codex login status` | Default Worker provider; local status text only. |
| `claude` | `claude` | yes | yes | yes | yes | yes | yes | `claude auth status --json` | Verified Worker and Verifier provider; local machine-readable status fields only. |
| `gemini` | `gemini` | yes | no | yes | yes | yes | yes | reference existence only | Experimental direct Gemini path; checks declared auth artifact and environment-name existence without reading values. |
| `antigravity` | `agy` | no | no | no | no | yes | no | `agy models` | Worker-only path. Declared network probe is skipped by default. |
| `grok` | `grok` | yes | no | yes | no | yes | yes | `grok models` | Official Grok Build CLI only; auth/catalog probing is network-declared and skipped unless explicitly granted. |

`antigravity` is the clearest partial provider today. If selected for a
read-only invocation, an MCP-backed invocation, or schema-enforced JSON output,
loopcoder must fail closed before launching `agy` and point the user to a
supporting provider such as `codex` or `claude`.

`grok` runs only with an approved execution profile. The adapter probes
`grok version` and `grok --help` before launch, requires `-p`, `--cwd`,
`--output-format`, `--no-auto-update`, `--no-alt-screen`, `--sandbox`,
`--permission-mode`, `--allow`, and `--deny`, and fails closed when the
installed CLI does not advertise the requested read-only or strict write mode.
The adapter canonicalizes the worktree path, rejects workspace symlinks that
resolve outside that physical workspace, replaces user home/config/temp roots
with private per-attempt directories, passes only an environment allowlist plus
`XAI_API_KEY`, disables memory, subagents, web search, auto-update, Bash,
WebFetch, and MCP tools through CLI flags, and rejects known project-local
Claude/Grok configuration sources before launch. Provider-controlled account,
server-side conversation, and model state may still exist behind the official
CLI; loopcoder records only redacted session references and normalized
receipts. The detailed inherited-source inventory is in
[`../security/grok-isolation.md`](../security/grok-isolation.md).
Grok model discovery is dynamic when the operator grants
`provider:grok/action:auth-catalog-inventory` style inventory access; otherwise
the provider-machine catalog is recorded as unavailable rather than guessed.
LoopCoder does not read Grok credential files or secret environment values,
does not perform login, does not auto-install or auto-update Grok Build, and
does not call a direct xAI API.

## Compatibility Smoke Matrix

`doctor --format json` includes a `provider_compatibility[]` smoke matrix built
from the runtime capability contract. Each entry has `provider`, `host`, `role`,
`support`, `status`, `code`, and the required or missing provider capabilities.
The matrix is local and static; it does not log in, launch paid remote calls, or
try to automate provider authentication.

Provider-native sub-agent eligibility is stricter than a static adapter claim.
An approved future product bridge requires fresh durable provider inventory,
task requirements, budget reservation, scope, ownership lock, and plan/policy
fingerprint authority before a native child may launch. No provider-native
child path is approved in v0.8.0. Grok's ordinary-worker conformance does not
imply native federation or provider-native subagent support.

`doctor --format json` also includes `provider_inventory`, and
`loopcoder providers refresh --repo .` persists the same bounded installation,
auth-readiness, and model catalog inventory in machine-local SQLite.
ProviderInstallation, ProbeResult, AccountProfile, AuthReadiness,
ModelCatalogSnapshot, and ModelCapability records show discovery source,
redacted executable/profile provenance, captured time, freshness, confidence,
probe outcome, output bounds, and `usable_for_invocation: "unknown"` from
installation evidence alone.
Provider installation probes pass only a bounded environment allowlist for
location and platform facts; script shims may receive variables such as
`LOCALAPPDATA`, `APPDATA`, `SystemRoot`, `ComSpec`, `PATH`, `PATHEXT`, `TMPDIR`,
`LANG`, and `LC_ALL`, but credential-like variable names are denied regardless
of allowlist membership.

Auth readiness probes are credential-blind. Unsupported providers emit
`readiness_state: "unknown"` with an explicit reason. A runtime-declared
network auth probe is skipped unless a future permission path grants network
access; today Antigravity's `agy models` is recorded with
`network_declared: true`, `network_permission: "denied"`, and
`network-permission-denied`. No current adapter exposes a safe
machine-readable expiry value, so the implementation does not emit or persist
an `expires_at` field.

Built-in local declarations are part of `internal/runtimecap`: Codex uses
`codex login status` as sanctioned local status text, Claude uses
`claude auth status --json` as declared non-secret machine-readable status,
Gemini reports only auth-reference existence, Antigravity declares
`agy models` as network-capable so it is recorded but not run by default, and
Grok declares `grok models` as the network-capable auth/catalog surface.
Quota telemetry for Grok remains confidence-scoped to what the official CLI
returns. Exact account quota, subscription limits, reset windows, or
provider-wide allowance are not inferred when the CLI does not report them.

ModelCapability records reuse this same capability vocabulary rather than
creating a parallel model-tier scheme. `read_only`, `json_output`,
`nested_subagents`, `mcp_config`, `cancellation`, and
`token_usage_reporting` are `true`, `false`, or `unknown` facts with
provenance and freshness. Unknown or stale values do not satisfy hard
requirements; routing policies must use the exact `model_catalog_snapshot_id`
and `model_capability_id` when they consume catalog evidence.

Static compatibility levels used by `doctor`:

| Level | Meaning |
| --- | --- |
| `supported` | The provider and host combination satisfies the static local capability contract for that role; this is not a release support decision. |
| `experimental` | The combination is usable but still has known limitations or is not part of the fully verified path. |
| `unsupported` | The combination must fail closed before provider launch when selected for that role. |

Current static role-compatibility matrix:

| Provider | Worker | Verifier / read-only | Nested sub-agents |
| --- | --- | --- | --- |
| `codex` | supported | supported | unsupported |
| `claude` | supported | supported | supported |
| `gemini` | experimental | experimental | unsupported |
| `antigravity` | experimental | unsupported | unsupported |
| `grok` | experimental | experimental | unsupported |

In particular, the `claude` nested declaration is adapter capability metadata;
it does not make real-provider nested execution supported in v0.8.0. Codex and
Claude Worker/Verifier rows describe local invocation compatibility and
historical mechanism evidence, not protected exact-artifact canaries or routed
Verifier independence.

Current host-invocation compatibility matrix:

| Host profile | Support level | Notes |
| --- | --- | --- |
| `codex-cli` | supported | Hook enforcement is best-effort unless manually wired by the host. |
| `claude-code` | supported | Project hook install writes conductor reporter and relay guard commands. |
| `paseo-style` | supported | The host owns session lifetime and stderr visibility. |
| `generic-local` | experimental | Fallback when no explicit host profile or known host signal is available. |

These host rows mean the foreground subprocess contract can be represented.
They do not prove callback, targeted wake, user visibility, acknowledgment, or
unsolicited delivery during detached work.

Doctor adds selected-provider checks for the active Worker and Verifier roles.
Those checks distinguish `missing_executable`, `unauthenticated_provider`,
`unsupported_read_only_mode`, and `unsupported_nested_agents`. Unavailable
optional providers do not hard-fail unless they are selected by `.delivery.yml`
or command defaults.

Current provider idempotency support is metadata-only for the local CLI
providers. Loopcoder threads a durable logical child idempotency key through the
Worker and provider invocation request, and includes it in the worker prompt.
No current CLI adapter exposes a verified native idempotency API, so this is
not treated as proof of exactly-once provider-side execution. A durable
`provider_receipt` must come from a real provider response, external resource
ID, or verifiable local execution record.

## Host Invocation Contract

The Conductor host is the active human or automation session that calls the
`loopcoder` binary. The host is not selected by Worker or Verifier model
configuration, and loopcoder must not depend on private host APIs.

Required host behavior:

| Requirement | Contract |
| --- | --- |
| Local subprocess | The host can run `loopcoder` from the selected working directory and pass arguments without rewriting them. |
| Stdout visibility | The host preserves stdout records exactly enough for JSON result parsing and local status reporting. |
| Stderr visibility | The host keeps provider, Worker, Verifier, and pretty report stderr visible to the Conductor. |
| JSON pass-through | The host does not wrap or edit machine-readable JSON stdout records before loopcoder consumers parse them. |
| Cancellation | The host can interrupt the foreground `loopcoder` process, or leave loopcoder's own timeout and kill-group supervision in control. |
| Timeouts | The host can keep the session open long enough for configured hard caps, or terminate cleanly and let recovery re-derive state. |
| No private API dependency | Host integration may use documented hooks or local subprocess behavior, but core delivery cannot require private host APIs. |

The host negotiation record is `loopcoder.host_negotiation.v1`. It is pure
data: evaluating it must not launch providers, collect provider inventory,
write delivery records, open callbacks, wake hosts, perform network I/O, or
start background supervisors.

Progress delivery records stay provider-neutral. Receipt generation, transport
write, host acceptance, user visibility, and acknowledgment are distinct states
with distinct evidence requirements. Negotiation may say that a stage requires
evidence, is local-only, is replay-only, or is unsupported, but it must not
claim acceptance, visibility, or acknowledgment until the progress outbox has
the exact matching transport evidence.

Progress transport fallback order is deterministic:

1. `acknowledged-streaming` when callbacks, wake-up, and acknowledgment are all
   declared supported.
2. `unacknowledged-streaming` when callbacks and wake-up are declared supported
   but acknowledgment is not.
3. `durable-follow-poll` when durable polling or resumable follow is declared
   supported.
4. `known-origin-next-invocation-replay` when an opaque run origin is bound but
   no active wake path is declared.
5. `next-invocation-replay` for generic, unknown, partial, or unsupported
   active transport declarations.

The optional run origin is bound with
`loopcoder.host_run_origin.v1`. The binding is scoped to exactly one
`project_id`, `delivery_run_id`, and `correlation_id`, and it persists only
redacted identifiers, metadata keys, and digests. Raw opaque host tokens,
credentials, local paths, and secret-like values are not authority and must not
be persisted or rendered. Replaying the same origin in the same scope produces
the same binding; replaying it for another project, run, or correlation produces
a different binding and cannot authorize delivery for the original scope.

Current host profiles:

| Host profile | Invocation style | Hooks | Notes |
| --- | --- | --- | --- |
| `codex-cli` | Interactive Codex CLI conductor session calls `loopcoder` as a local subprocess. | best-effort/manual | Codex hook enforcement is best-effort unless manually wired. |
| `claude-code` | Claude Code skill or conductor session calls `loopcoder` as a local subprocess. | supported | Project hook install writes conductor reporter and relay guard commands. |
| `paseo-style` | External conductor or agent supervisor calls `loopcoder` as a local subprocess. | host-owned | Optional host transport. The current adapter records only LoopCoder-local durable status/follow plus matching-origin next-invocation replay; no documented Paseo callback, targeted wake, visibility, or acknowledgment surface has been proven. |
| `generic-local` | Unknown local agent host calls `loopcoder` as a subprocess. | unknown | Fallback when no explicit profile or known host signal is available. |

Progress delivery capabilities are declared per host surface, not inferred from
provider/model selection. Codex CLI and Claude Code support foreground
stdout/stderr, JSON pass-through, durable `status --receipts`, and resumable
`attach` follow as LoopCoder local surfaces. Paseo-style hosts currently inherit
only those LoopCoder-local durable status/follow and matching-origin replay
surfaces; LoopCoder has not found documented Paseo poll/follow, callback,
targeted wake, visibility, or acknowledgment evidence. Claude Code also supports
documented project hooks for observing local tool events, but hook invocation is
only hook evidence; it is not evidence that the original user saw a message,
that a session woke up, or that a progress receipt was acknowledged. Until a
documented targeted wake or callback path is proven by an opt-in integration
fixture, these hosts fall back to local durable follow/poll and matching-origin
next-invocation replay for terminal or consequential detached progress.

### Paseo Host Delivery

Paseo is optional. If it is absent, disconnected, incompatible, or not selected
by `LOOPCODER_HOST`, `.delivery.yml` `host.profile`, or known environment
markers, LoopCoder continues with the normal host profile resolution and durable
local receipt surfaces. No Paseo package is linked into LoopCoder core.

When `PASEO_AGENT_ID` is present, LoopCoder may bind the run to a redacted
Paseo origin for durable next-invocation replay. The raw agent id is never
persisted; only a scoped digest, origin reference, and bounded marker-key
evidence such as `env.PASEO_AGENT_ID` are stored. `PASEO_HOST` is a presence
marker only. By itself it can help detect a Paseo-style host, but it does not
create an origin binding and is not evidence of a callback, managed task,
targeted wake, user visibility, or acknowledgment capability.

The current negotiated Paseo transport is `durable-follow-poll` with `no-ack`,
where durable follow/poll means LoopCoder's local receipt store, `status`, and
`attach` surfaces. It is not evidence of a documented Paseo polling or following
API.
`callbacks`, `wake-up`, `acknowledgment`, and LoopCoder-managed background
delivery through Paseo are advertised as unsupported until a documented Paseo
surface and an opt-in macOS Apple Silicon integration fixture prove targeted
progress and terminal delivery to the original session after the foreground
turn ends. The credential-free real smoke fixture checks only that a supported
Paseo CLI can be inspected and that LoopCoder does not claim wake/ack from CLI
presence alone; `paseo --version` is not wake evidence.

Fallback behavior is intentionally local and provider-neutral:

- Use `loopcoder status --repo . --run <run-id> --receipts` for durable receipt
  history.
- Use `loopcoder status --repo . --follow --run <run-id>` or
  `loopcoder attach --repo . --run <run-id>` for follow/poll.
- On a later invocation from the same redacted Paseo origin, pending terminal or
  consequential receipts replay exactly once before new dispatch work starts.
- Host delivery failure, callback timeout/unsupported status, host disconnect,
  daemon restart, session refresh, or the upstream Paseo #2034 Claude refresh
  race must not cancel a healthy worker, renew its watchdog, alter run state, or
  change provider/model routing.

Paseo is a host transport here, not a Worker or Verifier provider. Worker and
Verifier provider selection remains controlled by `.delivery.yml` and command
flags, and may use Codex, Claude, Gemini, Grok, or future providers while the
Paseo adapter only scopes host progress replay.

## Host Profile Resolution

Host profile selection is separate from provider and model selection. The
resolution order is:

1. `LOOPCODER_HOST`, when set.
2. `.delivery.yml` `host.profile`, when set.
3. Known host environment detection.
4. `generic-local` fallback.

Supported explicit values are the canonical profile names above plus common
aliases such as `codex`, `claude`, `claudecode`, `paseo`, and `generic`.
Unknown explicit values fail before host assumptions are used, and the error
names the source (`LOOPCODER_HOST` or `host.profile`) plus the known profiles.

Example explicit Codex-style invocation:

```text
LOOPCODER_HOST=codex-cli loopcoder doctor --repo . --format json
```

Example repository config:

```yaml
host:
  profile: claude-code
```

Known host detection currently recognizes ordinary environment markers for the
supported styles, including Codex CLI (`CODEX_CLI`, `CODEX_THREAD_ID`), Claude
Code (`CLAUDECODE`, `CLAUDE_CODE_SESSION_ID`,
`CLAUDE_CODE_ENTRYPOINT`), and paseo-style supervisors (`PASEO_AGENT_ID`,
`PASEO_HOST`). Detection is best-effort; set `LOOPCODER_HOST` or
`host.profile` when a wrapper exposes multiple host markers.

## Output Rules

`loopcoder dispatch` writes stable Worker report records and the final dispatch
result JSON on stdout. `loopcoder loopreview` writes the verifier verdict JSON
on stdout. Human-readable pretty reports are local diagnostics on stderr and
must not be copied into PR bodies, issue comments, commit messages, docs,
fixtures, or tracked state.

Machine consumers should parse the documented stdout records, not stderr pretty
text. Agent hosts must not suppress stderr for `dispatch`, `dispatch-wave`, or
`loopreview`, because local relay obligations depend on those report blocks
remaining visible.

`doctor --format json` includes the resolved `host_profile` object at the JSON
root. Human-readable doctor checks remain in `checks[]`; no pretty or prose
host report is written to stdout in JSON mode.

## Cancellation And Timeout Behavior

Provider invocations are supervised by loopcoder with context cancellation,
hard caps, stall detection, and platform kill-group behavior where available.
A host interrupt should stop the foreground loopcoder process; recovery then
uses GitHub state plus `.loopcoder/runs/<RunId>/` sidecars to decide whether an
attempt completed, failed, became stale, or needs bounded recovery.

Provider capability `cancellation` means loopcoder can locally supervise and
terminate the process it launches. It does not mean the provider can guarantee a
remote model-side cancellation after the local process has already exited or
lost connectivity.

## Read-only Support

Read-only support is required for Verifier and Layer 2 audit-review provider
invocations. A provider with `read_only: false` must fail closed before launch
when the invocation requests read-only mode. The failure should include the
missing capability and a fix, for example selecting `codex` or `claude` for the
Verifier while keeping `antigravity` for write-mode Worker dispatch.

MCP servers selected for read-only invocations must also be locally classified
as read-only. A read-only provider does not make a write-capable MCP server
safe.
