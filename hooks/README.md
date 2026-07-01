# Conductor Hooks

loopcoder ships two Claude Code conductor hooks for the local-only obligations
described in [`docs/specs/0316-conductor-local-enforcement.md`](../docs/specs/0316-conductor-local-enforcement.md):

- `conductor-attest.js` records a successful
  `loopcoder attest --role conductor ...` Bash tool call and blocks `Stop`
  until the Conductor has self-attested before finishing a delivery or merge
  turn.
- `conductor-relay-guard.js` checks local command output from
  `loopcoder dispatch` and `loopcoder loopreview` and backstops missing Worker
  or Verifier attestation blocks from gitignored relay state.

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
`.claude/settings.json`. It preserves unrelated settings and user hooks.
`loopcoder doctor --repo <project>` warns when either conductor hook is missing
and points back to `loopcoder skill install`.

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
            "command": "node hooks/conductor-attest.js",
            "timeout": 10
          },
          {
            "type": "command",
            "command": "node hooks/conductor-relay-guard.js",
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
            "command": "node hooks/conductor-attest.js",
            "timeout": 10
          },
          {
            "type": "command",
            "command": "node hooks/conductor-relay-guard.js",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

## Required Conductor Flow

Before completing a delivery or merge turn, run a real Conductor attestation:

```sh
loopcoder attest --role conductor --provider <provider> --model <model> --permission orchestrate --action "<delivery action>" --duration-ms <ms> --total-tokens <tokens>
```

Never swallow Worker or Verifier attestation output: do not redirect, hide, or
suppress stderr from `dispatch`, `dispatch-wave`, or `loopreview`. Relay every
pretty attestation block verbatim and never summarize, merge, or hand-format
it. Report delivery run status by running `loopcoder status` and relaying its
program-rendered local output instead of hand-typing a status table.

Attestation and status surfaces are local-only. Keep them in command output and
gitignored `.loopcoder/` records for recovery; do not copy them into PR bodies,
issue or PR comments, commit messages, merge commits, merge comments, docs,
examples, fixtures, or tracked files.

## Scope And State

`conductor-attest.js` writes state under
`.loopcoder/hooks/conductor-attest/` by default. Set
`LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR` to use a different state directory.
Set `LOOPCODER_CONDUCTOR_ATTEST_SCOPE=always` to enforce outside auto-detected
loopcoder conductor workspaces, or `LOOPCODER_CONDUCTOR_ATTEST_SCOPE=off` to
disable the attestation hook.

`conductor-relay-guard.js` writes state under
`.loopcoder/hooks/conductor-relay-guard/` and reads local relay ledgers under
`.loopcoder/relay/`. Set `LOOPCODER_RELAY_GUARD_SCOPE=always` to enforce
outside auto-detected loopcoder conductor workspaces, or
`LOOPCODER_RELAY_GUARD_SCOPE=off` to disable the relay guard.

## Test

```sh
node --test hooks/conductor-attest.test.js
node --test hooks/conductor-relay-guard.test.js
```
