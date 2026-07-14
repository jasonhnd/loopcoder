# Grok Isolation Inventory

LoopCoder treats `grok` as an official provider CLI that may have Claude-like
configuration surfaces. The adapter must prove that a Worker, Verifier, or
audit invocation cannot silently inherit authority from the operator's user
profile, the target project, or provider-controlled state.

## Supported Surface

The bounded Grok runner supports Grok Build CLI versions that report at least
`0.1.0` and whose `grok --help` output advertises all required launch controls:
`-p`, `--cwd`, `--output-format`, `--no-auto-update`, `--no-alt-screen`,
`--sandbox`, `--permission-mode`, `--allow`, and `--deny`. Read-only calls also
require `read-only` and `dontAsk`; write calls require `strict` and `dontAsk`.

If any required primitive is missing or cannot be probed, the adapter exits
before launch with `unsupported-capability`.

LoopCoder uses Grok only through the official local Grok Build CLI. It does not
install or update the CLI, read credential files, perform login, call a direct
xAI API, or assert exact subscription quota. Required tests use fake Grok
Build/headless/ACP surfaces and do not consume provider credits. Live Grok
smoke is an operator-owned diagnostic, disabled by default, and must be enabled
explicitly with the documented environment gates in the test command.

## Inherited Source Inventory

| Inherited source | Enforcement |
| --- | --- |
| User home config under `HOME` or `USERPROFILE` | Replaced with a private per-attempt home directory. The real path is not passed to Grok. |
| XDG, AppData, LocalAppData, temp, and cache roots | Replaced with private per-attempt config/cache/data/temp directories. |
| Credential-like environment variables | Denied by default. `XAI_API_KEY` is the only provider credential variable passed through. |
| User/project plugins | User plugins are hidden by the isolated home/config roots. Project `.claude/plugins`, `.claude/plugins.json`, and `.grok/plugins` cause fail-closed output. |
| User/project hooks | User hooks are hidden by the isolated home/config roots. Project `.claude/hooks` and `.grok/hooks` cause fail-closed output. |
| User/project agents, commands, and skills | User agents are hidden by the isolated home/config roots. Project `.claude/agents`, `.claude/commands`, `.claude/skills`, and `.grok/agents` cause fail-closed output. |
| MCP configuration | Invocation MCP is unsupported for Grok and fails before launch. Project `.mcp.json`, `.claude/mcp.json`, and `.grok/mcp.json` also fail closed. Grok receives `--deny MCPTool(*)`. |
| Memory and instruction files | Grok receives `--no-memory`. Project `CLAUDE.md`, `CLAUDE.local.md`, `GROK.md`, `.claude/memory`, and `.grok/memory` cause fail-closed output. |
| Project settings | Project `.claude/settings.json`, `.claude/settings.local.json`, `.grok/settings.json`, and `.grok/config.json` cause fail-closed output. |
| Web or shell authority | Grok receives `--disable-web-search`, `--deny WebFetch(*)`, and `--deny Bash(*)`. No shell is used to launch Grok. |
| Workspace aliases | The worktree is canonicalized with physical path identity before launch. The physical path is used for both process directory and `--cwd`. |
| Symlink and junction escapes | Workspace symlinks that resolve outside the accepted physical workspace cause fail-closed output before launch. |
| Cancellation | Grok runs under LoopCoder supervised execution. Unix launches use a dedicated process group; Windows launches use a Job Object when available. Context cancellation, hard-cap expiry, and stall handling kill the full process tree. |
| Durable session records | Streaming output is normalized and redacted before it is written. External session references are stored as redacted provider references only, not credentials or raw transcripts. |
| Provider-native subagents | Not supported or required for Grok ordinary-worker conformance. Grok receives `--no-subagents`; any future native subagent support needs separate accepted capability evidence. |

## Residual Limitations

The official Grok CLI may still use provider-side account, model routing,
conversation, policy, billing, or abuse-prevention state after the local
request reaches xAI. LoopCoder cannot inspect or reset that provider-controlled
state. The adapter records the installed CLI version, normalized model and
usage fields when supplied, the selected permission mode, and redacted external
session references so operators can audit what LoopCoder controlled locally.

If future Grok versions expose documented flags for ignoring project
configuration or selecting an explicit empty configuration file, the adapter
can replace project-config fail-closed behavior with that explicit profile.
Until then, project-local inherited configuration is blocked rather than
trusted.

Workspace boundary inspection is path based. Symlinks and junctions are
resolved before launch and any broken link or link resolving outside the
accepted physical workspace fails closed. This can reject legitimate broken or
external links and can be expensive in very large trees, but it preserves the
bounded workspace contract without trusting provider-side filtering. Filesystem
hard links are not distinguishable through this scan, so the adapter cannot
prove that a workspace file is not another hard link to content reachable by a
different path.

On Windows, user-writable profile/config roots are replaced, but machine-wide
operating system roots required for process startup and normal CLI execution
remain inherited. In particular, `ALLUSERSPROFILE` and `ProgramData` may still
point at host-level configuration locations. Operators should not treat the
bounded Grok profile as a full machine-wide Windows configuration sandbox.
