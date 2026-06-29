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
  command stderr exactly as required by [`SKILL.md`](SKILL.md).
- Verification still delegates the primary adversarial review to
  `loopcoder loopreview`; the verifier provider SHOULD differ from the worker
  provider, and human merge remains the final gate.
- Before completing a delivery or merge turn, run
  `loopcoder attest --role conductor ...` at least once per active host session
  with the real Codex host model,
  timing, action, and usage available for the session. Stamp the emitted
  `[attestation] ...` header or canonical JSON into durable artifacts the
  Conductor produces, such as PR merge comments or merge commit messages; chat
  alone is not durable enough.
- Codex hook enforcement is best-effort in this repository. Codex CLI exposes
  hook events that are similar to the Claude Code `PostToolUse` and `Stop`
  events, and `hooks/conductor-attest.js` accepts those event names when wired
  in, but this repository ships and tests the Claude Code registration only.
  If you opt in to Codex hooks, register the same command for Bash
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
