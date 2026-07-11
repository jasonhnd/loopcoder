# Provider And Host Runtime Capabilities

This document is loopcoder's runtime capability contract. It describes what the
native binary expects from provider CLIs that run Worker, Verifier, and audit
LLM invocations, and what it expects from agent hosts that invoke `loopcoder` as
the Conductor.

This contract is separate from Worker and Verifier model selection. Provider,
model, and depth still resolve from command flags, `.delivery.yml`, and the
static model registry. Runtime capabilities describe whether the selected
provider or host can safely satisfy a requested invocation mode.

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

`antigravity` is the clearest partial provider today. If selected for a
read-only invocation, an MCP-backed invocation, or schema-enforced JSON output,
loopcoder must fail closed before launching `agy` and point the user to a
supporting provider such as `codex` or `claude`.

## Compatibility Smoke Matrix

`doctor --format json` includes a `provider_compatibility[]` smoke matrix built
from the runtime capability contract. Each entry has `provider`, `host`, `role`,
`support`, `status`, `code`, and the required or missing provider capabilities.
The matrix is local and static; it does not log in, launch paid remote calls, or
try to automate provider authentication.

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
Gemini reports only auth-reference existence, and Antigravity declares
`agy models` as network-capable so it is recorded but not run by default.

ModelCapability records reuse this same capability vocabulary rather than
creating a parallel model-tier scheme. `read_only`, `json_output`,
`nested_subagents`, `mcp_config`, `cancellation`, and
`token_usage_reporting` are `true`, `false`, or `unknown` facts with
provenance and freshness. Unknown or stale values do not satisfy hard
requirements; routing policies must use the exact `model_catalog_snapshot_id`
and `model_capability_id` when they consume catalog evidence.

Support levels:

| Level | Meaning |
| --- | --- |
| `supported` | The provider and host combination satisfies the local capability contract for that role. |
| `experimental` | The combination is usable but still has known limitations or is not part of the fully verified path. |
| `unsupported` | The combination must fail closed before provider launch when selected for that role. |

Current role matrix:

| Provider | Worker | Verifier / read-only | Nested sub-agents |
| --- | --- | --- | --- |
| `codex` | supported | supported | unsupported |
| `claude` | supported | supported | supported |
| `gemini` | experimental | experimental | unsupported |
| `antigravity` | experimental | unsupported | unsupported |

Current host matrix:

| Host profile | Support level | Notes |
| --- | --- | --- |
| `codex-cli` | supported | Hook enforcement is best-effort unless manually wired by the host. |
| `claude-code` | supported | Project hook install writes conductor reporter and relay guard commands. |
| `paseo-style` | supported | The host owns session lifetime and stderr visibility. |
| `generic-local` | experimental | Fallback when no explicit host profile or known host signal is available. |

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

Current host profiles:

| Host profile | Invocation style | Hooks | Notes |
| --- | --- | --- | --- |
| `codex-cli` | Interactive Codex CLI conductor session calls `loopcoder` as a local subprocess. | best-effort/manual | Codex hook enforcement is best-effort unless manually wired. |
| `claude-code` | Claude Code skill or conductor session calls `loopcoder` as a local subprocess. | supported | Project hook install writes conductor reporter and relay guard commands. |
| `paseo-style` | External conductor or agent supervisor calls `loopcoder` as a local subprocess. | host-owned | The host owns session lifetime and must keep stderr visible for relay obligations. |
| `generic-local` | Unknown local agent host calls `loopcoder` as a subprocess. | unknown | Fallback when no explicit profile or known host signal is available. |

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
