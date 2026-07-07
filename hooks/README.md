# Conductor Hooks

loopcoder ships two Claude Code conductor hooks for the local-only obligations
described in [`docs/specs/0316-conductor-local-enforcement.md`](../docs/specs/0316-conductor-local-enforcement.md)
and renamed to reporter wording in
[`docs/specs/0567-reporter.md`](../docs/specs/0567-reporter.md).
The hook logic is embedded in the loopcoder binary and invoked as a subcommand;
there are no `.js` hook files and no Node dependency.

- `loopcoder hook conductor-reporter` records a successful
  `loopcoder attest --role conductor ...` Bash tool call and blocks `Stop`
  until the Conductor has self-reported before finishing a delivery or merge
  turn. The old `conductor-attest` hook command remains a one-version
  compatibility alias during the reporter transition.
- `loopcoder hook conductor-relay-guard` checks local command output from
  `loopcoder dispatch` and `loopcoder loopreview` and backstops missing Worker
  or Verifier report blocks from gitignored relay state.

Both hooks fail open on malformed hook input or state errors so they do not
break unrelated work. They write only under gitignored `.loopcoder/` hook or
relay state.

## Claude Code Install

Install or refresh the playbook and project hook settings with:

```sh
loopcoder skill install --repo <project>
```

From the target repository, `loopcoder skill install --repo .` writes the
bundled `SKILL.md` and `AGENTS.md` files to the Claude Code loopcoder skill
directory and structurally merges both conductor hooks into the project
`.claude/settings.json`. It preserves unrelated settings and user hooks, and
writes a gitignored `.loopcoder/conductor-workspace` marker that activates
auto-enforcement in installed repos. `loopcoder doctor --repo <project>` warns
when either conductor hook command is missing or when `loopcoder` does not
resolve on `PATH`, and points back to `loopcoder skill install`.

The hooks are invoked as `loopcoder` subcommands, so the merge upgrades any
stale `node hooks/*.js` entries to the command form below idempotently and the
hooks resolve regardless of the working directory.

Do not edit a user's global Claude Code settings unless they explicitly choose
that install location. The project-scoped settings shape is:

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "loopcoder hook conductor-reporter",
            "timeout": 10
          },
          {
            "type": "command",
            "command": "loopcoder hook conductor-relay-guard",
            "timeout": 10
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "loopcoder hook conductor-reporter",
            "timeout": 10
          },
          {
            "type": "command",
            "command": "loopcoder hook conductor-relay-guard",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

## Required Conductor Flow

Before completing a delivery or merge turn, run a real Conductor report:

```sh
loopcoder attest --role conductor --provider <provider> --model <model> --permission orchestrate --action "<delivery action>" --duration-ms <ms> --total-tokens <tokens>
```

Never swallow Worker or Verifier report output: do not redirect, hide, or
suppress stderr from `dispatch`, `dispatch-wave`, or `loopreview`. Relay every
pretty report block verbatim and never summarize, merge, or hand-format
it. Report delivery run status by running `loopcoder status` and relaying its
program-rendered local output instead of hand-typing a status table.

Reporter and status surfaces are local-only. Keep them in command output and
gitignored `.loopcoder/` records for recovery; do not copy them into PR bodies,
issue or PR comments, commit messages, merge commits, merge comments, docs,
examples, fixtures, or tracked files.

## Scope And State

`loopcoder hook conductor-reporter` writes state under
`.loopcoder/hooks/conductor-reporter/` by default and reads old
`.loopcoder/hooks/conductor-attest/` state if new state is absent during the
0.6.0 transition. Set `LOOPCODER_CONDUCTOR_REPORTER_STATE_DIR` to use a
different state directory. Set `LOOPCODER_CONDUCTOR_REPORTER_SCOPE=always` to
enforce outside auto-detected loopcoder conductor workspaces, or
`LOOPCODER_CONDUCTOR_REPORTER_SCOPE=off` to disable the reporter hook. The old
`LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR` and `LOOPCODER_CONDUCTOR_ATTEST_SCOPE`
environment keys remain one-version aliases.

`loopcoder hook conductor-relay-guard` writes state under
`.loopcoder/hooks/conductor-relay-guard/` and reads local relay ledgers under
`.loopcoder/relay/`. Set `LOOPCODER_RELAY_GUARD_SCOPE=always` to enforce
outside auto-detected loopcoder conductor workspaces, or
`LOOPCODER_RELAY_GUARD_SCOPE=off` to disable the relay guard.

Auto-enforcement also activates when the gitignored
`.loopcoder/conductor-workspace` marker is present, which
`loopcoder skill install` writes into installed repos.

## Test

The hook logic lives in Go. Run the tests with:

```sh
go test ./internal/conductorhooks/...
```
