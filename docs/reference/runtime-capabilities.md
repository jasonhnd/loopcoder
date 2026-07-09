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
