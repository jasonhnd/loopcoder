# loopcoder Security Audit Rubric

This file extends the built-in `loopcoder audit` threat model. It does not
replace or weaken the built-in split between operator-trusted inputs and
untrusted repository, worktree, remote, and shared-host inputs.

Additional checks for the loopcoder repository:

- Confirm worker prompts, recovery briefs, logs that may include provider
  output, and local run state do not disclose sensitive material through
  group/world-readable modes.
- Treat `git`, `gh`, provider CLIs, and configured SAST tools as
  operator-trusted commands, but verify they are invoked without shell strings
  unless the command surface explicitly documents a shell command.
- Confirm release, upgrade, and state-branch flows keep downloaded artifacts,
  checksums, and scrubbed state distinct from trusted source files.
- Confirm verifier and audit-review invocations remain read-only and receive
  only MCP servers locally classified as read-only.
- Confirm audit, dispatch, loopreview, status, and reporter outputs stay on
  local surfaces unless an explicit tracked artifact is part of the command's
  documented contract.
