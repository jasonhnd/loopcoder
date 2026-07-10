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
| `auth_probe_command` | Optional read-only command that can check provider authentication readiness. |
| `known_limitations` | Human-readable limitations that must appear in actionable failure paths when relevant. |

Unsupported capabilities must fail before provider launch when loopcoder can
detect the mismatch locally. The error should name the provider, the missing
capability, supporting alternatives when known, and the local fix.

## Provider Compatibility

| Provider | Executable | Read-only | Nested sub-agents | JSON output | MCP config | Cancellation | Token usage | Auth probe | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `codex` | `codex` | yes | no | yes | yes | yes | yes | none | Default Worker provider. |
| `claude` | `claude` | yes | yes | yes | yes | yes | yes | none | Verified Worker and Verifier provider. |
| `gemini` | `gemini` | yes | no | yes | yes | yes | yes | none | Experimental direct Gemini path, outside the static model registry. |
| `antigravity` | `agy` | no | no | no | no | yes | no | `agy models` | Worker-only path. Uses plain text summary capture and self-reported selected model string. |

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

## Nested Execution Ownership

Nested child execution uses durable storage ownership before provider launch.
For each child run, `run_claims` records the current `executor_id`,
`claim_generation`, `claimed_at`, `lease_expires_at`, and `heartbeat_at`.
Claim acquisition, child run status, child edge status, and the transition event
are written in one immediate SQLite write transaction with bounded whole
transaction retry on lock contention.

Only a `claimed` result may launch the provider. A scheduler that sees another
active owner returns an in-progress observation with the owner, generation,
lease expiry, and replay action in the child result. A terminal child is reused
from durable state. Blocked or ambiguous ownership fails closed as needs-human.

Terminal completion is fenced by `run_id`, `executor_id`, and
`claim_generation`. If a stale worker finishes after a newer generation takes
over, its terminal write is rejected instead of overwriting the newer owner.
Lease expiry allows controlled takeover with a higher generation; it does not
prove that external side effects did or did not happen.

Crash behavior is intentionally conservative:

- Crash after claim but before provider launch leaves a running child with a
  lease for observers or later takeover.
- Crash during provider execution leaves durable ownership evidence but no
  exactly-once guarantee for external commands.
- Crash after external side effects but before terminal persistence requires
  receipts or other proof before publishing success; otherwise recovery reports
  needs-human.
- Cancellation while observing another owner does not grant execution rights.

Runtime output must expose owner, generation, lease expiry, and replay action
without including local secrets.

## Read-only Support

Read-only support is required for Verifier and Layer 2 audit-review provider
invocations. A provider with `read_only: false` must fail closed before launch
when the invocation requests read-only mode. The failure should include the
missing capability and a fix, for example selecting `codex` or `claude` for the
Verifier while keeping `antigravity` for write-mode Worker dispatch.

MCP servers selected for read-only invocations must also be locally classified
as read-only. A read-only provider does not make a write-capable MCP server
safe.
