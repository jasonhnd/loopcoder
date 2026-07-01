# loopcoder Codex CLI Entrypoint

This is the Codex CLI entrypoint for loopcoder conductor work.

The canonical conductor playbook is [`SKILL.md`](SKILL.md). Follow that file as
the single source of truth for planning, doc-first ordering, dispatch,
verification, reporting, and the human merge gate. Do not copy or fork the
procedure here.

Codex host specifics:

- Treat the active Codex CLI session as the conductor session and keep it open
  while the loop runs.
- When the user invokes `/loopcoder <need>` or states a delivery/build request,
  follow [`SKILL.md`](SKILL.md).
- Do not implement work items directly in the conductor session. Use the
  resolved `loopcoder` binary for mechanical work.
- Do not choose model or effort settings for the user. Follow the configured
  `adapters.worker` and any explicit user overrides exactly as described in
  [`SKILL.md`](SKILL.md).
- Relay the verbatim pretty attestation block per Worker and per Verifier from
  command stderr exactly as required by [`SKILL.md`](SKILL.md). Never redirect,
  hide, or suppress `loopcoder dispatch`, `loopcoder dispatch-wave`, or
  `loopcoder loopreview` stderr; every attestation block must be locally
  visible and relayed verbatim, never summarized. For `dispatch` and
  `loopreview`, this is locally enforced by `conductor-relay-guard` where hooks
  are active.
- Report delivery run status by running `loopcoder status` and relaying its
  program-rendered local output, not by hand-typing a status table.
- Verification still delegates the primary adversarial review to
  `loopcoder loopreview`; the verifier provider SHOULD differ from the worker
  provider, and human merge remains the final gate.
- Before completing a delivery or merge turn, run
  `loopcoder attest --role conductor ...` at least once per active host session
  with the real Codex host model,
  timing, action, and usage available for the session. Treat the emitted
  attestation as local-only: keep it in command output and gitignored
  `.loopcoder/` run records for recovery, and do not copy it into PR bodies,
  issue or PR comments, commit messages, merge commits, merge comments, docs,
  examples, fixtures, or tracked files. This is locally enforced by
  `conductor-attest` where hooks are active.
- Install conductor hooks into the active project `.claude/settings.json` with
  `loopcoder skill install --repo <project>`. The install path wires both
  `hooks/conductor-attest.js` and `hooks/conductor-relay-guard.js`;
  `loopcoder doctor` warns when either hook is missing.
- Codex hook enforcement is best-effort in this repository. Codex CLI exposes
  hook events that are similar to the Claude Code `PostToolUse` and `Stop`
  events, and the conductor hook scripts accept those event names when wired
  in, but this repository ships and tests the Claude Code registration only.
  If you opt in to Codex hooks manually, register both commands for Bash
  `PostToolUse` and `Stop` events from the repo root:

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
