# loopcoder Audit Rubric Extension

This rubric extends the built-in `loopcoder audit` threat model. It does not
replace the built-in operator-trusted versus untrusted-input split.

Use this extension when auditing loopcoder itself:

- Treat `.delivery.yml`, command argv written by the operator, and installed
  local tools as operator-trusted inputs.
- Treat worktree contents, PR diffs, provider output, MCP server data, logs, and
  locally persisted run records as potentially untrusted or sensitive.
- Flag prompt, recovery, summary, schema, token, credential, or log material
  written with permissions broader than owner-only.
- Flag subprocess execution only when untrusted worktree data controls the
  executable or argument shape. Do not report ordinary operator-authored argv
  arrays as a finding by themselves.
- Flag file reads only when a path can escape the intended repository or expose
  sensitive local state beyond the operator's selected repo.
- Preserve local-only attestation: no Worker, Verifier, Conductor, or audit
  attestation JSON or pretty block belongs in PR bodies, issue comments, commit
  messages, merge artifacts, docs, examples, fixtures, or tracked files.
- Preserve H5-style exit separation: audit findings, `needs-human`, and command
  failures must remain distinct.
- Check release and upgrade paths for checksum/signature verification before
  trusting downloaded artifacts.
- Check that configured MCP servers offered to read-only verifier roles are
  locally classified read-only.
