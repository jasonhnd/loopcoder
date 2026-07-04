# Docs Domain Example

This fixture mirrors a corporate IR document-production repository without
calling external services or rendering a real PDF. It exists to prove the
0.5.0 domain-profile plug points from
[`docs/specs/0459-domain-profiles.md`](../../docs/specs/0459-domain-profiles.md)
with small text files.

The `.delivery.yml` profile maps the docs domain to loopcoder as follows:

- `domain.skills` discovers the governance skill under `governance/**/skill.md`
  and the machine-readable disclosure skill library under `machine-library/`.
- `domain.verification.rubric` loads `governance/qa-checklist.md` plus inline
  checklist items, and `review_packet_order` puts rendered artifact evidence
  before rubric, changed files, diff, issue, and spec.
- `domain.evidence.producer` runs `go run ./tools/docs_domain_tool.go render`
  and allows only `out/report.txt` into the rendered-artifact packet.
- `domain.red_lines` adds a disclosure-compliance veto for `disclosure/**`
  without changing the built-in destructive, build-not-green, or loopcoder-core
  red lines.
- `domain.partial_work.mode` is `report-only`, and
  `domain.liveness.mode` keeps the code profile's worktree-mtime signal because
  this document workflow writes repo files while it progresses.

An operator can copy this fixture to a test repository, authenticate `gh`, and
run loopcoder normally:

```bash
loopcoder dispatch --repo . --issue-number 1 --issue-title "Update IR packet" --issue-body "Implement per docs/specs/0459-domain-profiles.md." --provider codex
loopcoder loopreview --repo . --pr-number 1 --provider claude
```

The MCP entry is a no-op local stdio shape for worker and verifier wiring. The
validation test does not require a real external MCP server.
