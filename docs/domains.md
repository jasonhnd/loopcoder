# Domain Profiles

Domain profiles let a repository describe non-code delivery work while keeping
the loopcoder engine's ordering and safety gates unchanged. The design record is
[`specs/0459-domain-profiles.md`](specs/0459-domain-profiles.md), and the
self-contained docs-domain fixture is
[`../examples/docs-domain/`](../examples/docs-domain/).

The default remains the code profile. If `.delivery.yml` has no top-level
`domain` section, loopcoder keeps the existing source-first review packet,
default skill discovery, risk-gate floor, partial-work behavior, and liveness
mode.

## Docs Profile Shape

A docs-domain repository uses `.delivery.yml` to configure the same loop stages:

- `domain.skills` points worker prompt construction at repo-local document
  skills and optional machine-readable skill libraries.
- `domain.verification.rubric` injects QA checklist files and inline checklist
  items into the verifier packet.
- `domain.verification.review_packet_order` can place `rendered_artifact` and
  `rubric` before source sections so document evidence is reviewed first.
- `domain.evidence.producer` declares a deterministic render command and the
  allowed output paths to collect for `loopreview`.
- `domain.red_lines` appends domain-specific vetoes, such as
  `disclosure-compliance`, without lowering built-in destructive,
  build-not-green, or loopcoder-core red lines.
- `domain.partial_work.mode` and `domain.liveness.mode` configure recovery and
  watchdog classification without changing hard caps, relay, or promotion
  authority.

The docs-domain example renders a text report instead of a real PDF and includes
a no-op local MCP declaration. It is intentionally dependency-free so operators
can inspect the shape and tests can validate the profile without private
corporate IR material or external services.
