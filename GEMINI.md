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
- Verification still delegates the primary adversarial review to
  `loopcoder loopreview`; the verifier provider SHOULD differ from the worker
  provider, and human merge remains the final gate.
