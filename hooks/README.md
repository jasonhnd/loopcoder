# Conductor Attestation Hook

`conductor-attest.js` is the Claude Code hook for the Conductor attestation
gate described in `docs/specs/0146-attestation.md`. It records a successful
`loopcoder attest --role conductor ...` Bash tool call in a bounded per-session
state file and blocks `Stop` until that state exists. On malformed hook input or
state parse errors it allows the session to continue, so it fails open instead
of breaking unrelated work.

The script writes state under `.loopcoder/hooks/conductor-attest/` by default,
which is already ignored by this repository. Set
`LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR` to use a different state directory.

## Claude Code install

Register the same command for successful Bash tool completion and for `Stop`.
Add this to the project `.claude/settings.json` for the loopcoder conductor
workspace; do not edit a user's global settings unless they explicitly choose
that install location.

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
          }
        ]
      }
    ]
  }
}
```

The hook auto-enforces only in a workspace that looks like a loopcoder conductor
workspace. For an intentionally broader install, set
`LOOPCODER_CONDUCTOR_ATTEST_SCOPE=always` in the hook command environment.

## Required Conductor flow

Before completing a delivery or merge turn, run a real Conductor attestation:

```sh
loopcoder attest --role conductor --provider <provider> --model <model> --permission orchestrate --action "<delivery action>" --duration-ms <ms> --total-tokens <tokens>
```

Then stamp either the emitted `[attestation] ...` header or the canonical JSON
into durable artifacts the Conductor produces, such as PR merge comments or
merge commit messages.

## Test

```sh
node --test hooks/conductor-attest.test.js
```
