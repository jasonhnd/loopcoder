# Host Invocation Profiles

Host profiles describe the interactive agent host that is calling the
`loopcoder` binary. They are separate from Worker and Verifier providers:
`adapters.worker`, `adapters.verifier`, model, and depth selection still decide
which provider CLI performs work or review.

## Selection

Resolution is explicit first, detection second:

1. `LOOPCODER_HOST` when set.
2. `.delivery.yml` `host.profile` when set to a value other than `auto`.
3. Environment detection.
4. `generic`.

Supported explicit values are `generic`, `codex`, `claude`, `claudecode`,
`claude-code`, and `paseo`. `claudecode` and `claude-code` normalize to
`claude`.

```yaml
host:
  profile: auto
```

Use an environment override for one invocation:

```text
LOOPCODER_HOST=codex loopcoder doctor --repo . --format json
```

## Profiles

| Profile | Typical signals | Hook expectation | Notes |
| --- | --- | --- | --- |
| `codex` | `CODEX_THREAD_ID`, `CODEX_CLI`, `CODEX_SANDBOX` | best-effort | Codex-style hosts should keep JSON command stdout machine-clean and send diagnostics to stderr. |
| `claude` | `CLAUDECODE`, `CLAUDE_CODE_SESSION_ID`, `CLAUDE_CODE_ENTRYPOINT` | `.claude/settings.json` | Claude Code-style hosts can install conductor reporter and relay hooks with `loopcoder skill install --repo .`. |
| `paseo` | `PASEO_AGENT_ID`, `PASEO_HOST`, `PASEO_WORKTREE` | host-managed | Paseo-style hosts may orchestrate nested agents externally; loopcoder does not depend on private host APIs. |
| `generic` | none | unknown | Safe fallback when no known host signal is present. |

All profiles use the same command contract:

- `--repo` is normalized to an absolute directory path before execution.
- Caller environment is preserved; `LOOPCODER_*` variables are loopcoder-owned.
- Human output goes to stdout, diagnostics go to stderr, and `--format json`
  outputs emit only JSON on stdout.
- No profile modifies credentials automatically.

`loopcoder doctor --repo . --format json` exposes the resolved profile in the
top-level `host_profile` object and includes the same state in the `conductor
runtime` check.
