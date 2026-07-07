# loopcoder Gemini CLI Entrypoint

This is the Gemini CLI entrypoint for loopcoder conductor work.

The canonical conductor playbook is [`SKILL.md`](SKILL.md). Follow that file as
the single source of truth for planning, doc-first ordering, dispatch,
verification, reporting, and the human merge gate. Do not copy or fork the
procedure here.

Gemini host specifics:

- Treat the active Gemini CLI session as the conductor session and keep it open
  while the loop runs.
- When the user invokes `/loopcoder <need>` or states a delivery/build request,
  follow [`SKILL.md`](SKILL.md).
- Do not implement work items directly in the conductor session. Use the
  resolved `loopcoder` binary for mechanical work.
- Do not choose model or effort settings for the user. Follow the configured
  `adapters.worker` and any explicit user overrides exactly as described in
  [`SKILL.md`](SKILL.md).
- Relay the verbatim pretty report block per Worker and per Verifier from
  command stderr exactly as required by [`SKILL.md`](SKILL.md). Never redirect,
  hide, or suppress `loopcoder dispatch`, `loopcoder dispatch-wave`, or
  `loopcoder loopreview` stderr; every report block must be locally
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
  with the real Gemini host model,
  timing, action, and usage available for the session. Treat the emitted
  report as local-only: keep it in command output and gitignored
  `.loopcoder/` run records for recovery, and do not copy it into PR bodies,
  issue or PR comments, commit messages, merge commits, merge comments, docs,
  examples, fixtures, or tracked files. This is locally enforced by
  `conductor-reporter` where hooks are active. The `loopcoder attest` verb and
  old `conductor-attest` hook name remain one-version compatibility aliases.
- Install conductor hooks into the active project `.claude/settings.json` with
  `loopcoder skill install --repo <project>`. The install path wires both
  `loopcoder hook conductor-reporter` and `loopcoder hook conductor-relay-guard`;
  `loopcoder doctor` warns when either hook command is missing or when
  `loopcoder` does not resolve on `PATH`.
- Gemini hook enforcement is best-effort in this repository. Gemini CLI exposes
  `AfterTool` and `AfterAgent` hooks, and the conductor hook commands accept
  those event names when wired in, but this repository ships and tests the
  Claude Code registration only. If you opt in to Gemini hooks manually,
  register both commands for shell-tool `AfterTool` and `AfterAgent` events:

  ```json
  {
    "hooks": {
      "AfterTool": [
        {
          "matcher": "run_shell_command",
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
      "AfterAgent": [
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
