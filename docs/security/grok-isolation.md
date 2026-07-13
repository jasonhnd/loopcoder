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
