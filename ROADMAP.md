# ROADMAP

<!--
Format for loopcoder work units:
- Each ## heading is one topic or unit.
- Each "- doc:" or "- code:" list item is one slice and becomes one issue.
- code slices depend on the doc slices in the same unit unless "(needs: ...)" is set.
- Slice refs are <unit-slug>/<kind>-<n>; within the same unit, <kind>-<n> works.
- Use "## [epic] ..." for a slice DAG; add "- doc:" / "- code:" lines for explicit slices.
-->

## 0.5.3 — loopcoder audit (built-in security audit) — ✅ shipped v0.5.3 (2026-07-06)

Shipped. `loopcoder audit` is a read-only, built-in security audit that institutionalizes
catching the class of issue the external audit surfaced — on demand and in CI. Two layers: a
deterministic SAST floor (govulncheck/staticcheck/gosec + native secret & file-permission
scans, CI-gateable) and an adversarial LLM security-review lens (read-only verifier path,
attested, needs-human on failure). Configurable via `.delivery.yml audit`; wired as a required
CI `audit` check that loopcoder runs against itself; reported by `loopcoder doctor`.

- Design/spec: [`docs/specs/0518-loopcoder-audit.md`](docs/specs/0518-loopcoder-audit.md)
- Release notes: [`CHANGELOG.md`](CHANGELOG.md) — `## [0.5.3]`
- Guide + example rubric: [`docs/reference/audit.md`](docs/reference/audit.md),
  [`docs/security/`](docs/security/)

Built by loopcoder itself under the self-hosting guard (spec + C1 command/SAST floor →
C2 LLM lens → C3 CI/doctor/docs, serial). Wiring the self-audit surfaced and fixed real
findings: the worker-layer prompt/recovery `0o600` gap (a 0.5.1 A1-scope miss) and a
`golang.org/x/sys` dependency vulnerability. The E1 ReadOnly boundary, H5 exit-code split,
self-hosting guard, 0.5.1 hardening, and 0.5.2 behavior-preservation are all preserved.

## 0.5.2 — Core refactor (behavior-preserving) — ✅ shipped v0.5.2 (2026-07-05)

Shipped. Behavior-preserving internal restructuring for readability, testability, and reduced
drift, with **zero observable behavior change** (proven by golden/inventory tests and
independent verifier path-tracing, gated by the full CI suite incl. `-race`/staticcheck/
govulncheck).

- Design/spec: [`docs/specs/0507-core-refactor.md`](docs/specs/0507-core-refactor.md)
- Release notes: [`CHANGELOG.md`](CHANGELOG.md) — `## [0.5.2]`

Delivered (B1–B4): `worker.Dispatch` decomposed into focused helpers behind the unchanged
entrypoint; orchestration state/render split (byte-identical tick/promote/dispatch-wave
output); MCP validation consolidated into one shared parse-time validator (unchanged
accept/reject set, byte-identical provider argv); defaults/limits centralized into a new
`internal/defaults` leaf package with no value tuning.

## 0.5.1 — Security & robustness hardening — ✅ shipped v0.5.1 (2026-07-05)

Shipped. Fixes every verified finding from the external security audit. loopcoder is a local
single-operator dev CLI, so most were Low–Medium hardening rather than active-exploit fixes;
all are closed. No behavior change to the code profile.

- Design/spec: [`docs/specs/0484-security-robustness-hardening.md`](docs/specs/0484-security-robustness-hardening.md)
- Release notes: [`CHANGELOG.md`](CHANGELOG.md) — `## [0.5.1]`

Delivered (A1–A9): cosign-signed `SHA256SUMS` verified before checksum in install/upgrade;
`govulncheck` + `staticcheck` required CI checks + all Actions SHA-pinned + Dependabot; `0o600`
file modes and Gemini prompt via stdin (shared-host disclosure); statebranch path confinement;
additive no-shell `argv` command form for the evidence producer + custom liveness; honest
failure reporting (runJSON / CreateIssue-UpdateIssue / codex log); and bounded hook / runstatus
/ worktree-liveness I/O.

## 0.5.0 — Generalize loopcoder beyond code (domain profiles) — ✅ shipped v0.5.0 (2026-07-04)

Shipped. loopcoder is now a general autonomous-delivery engine for any verifiable,
repo-based, AI-doable work (documents, content, data…), not only code — via purely-additive
**domain profiles**, with the core engine (tick / dispatch / loopreview / risk-gate /
promote / guardrails / watchdog / relay) unchanged. Code is the first of several domains.

Built by loopcoder itself under the self-hosting guard (human merge gate): the spec merged
first, then nine code slices in dependency order, each worker → PR → read-only verifier →
CI → human-merge.

- Design/spec: [`docs/specs/0459-domain-profiles.md`](docs/specs/0459-domain-profiles.md)
- Release notes: [`CHANGELOG.md`](CHANGELOG.md) — `## [0.5.0]`
- Domain-profile guide + worked example: [`docs/domains.md`](docs/domains.md),
  [`examples/docs-domain/`](examples/docs-domain/)

Plug points delivered: configurable skill sources; injectable verification rubric +
review-packet ordering; rendered-artifact evidence producer; append-only domain red-lines;
MCP servers (local stdio + external HTTP) on `agent.Invocation`; and domain-configurable
partial-work / liveness (the 0.4.2 H1/H2 fold-ins). An absent `domain` section behaves
exactly like the 0.4.x code profile; the ReadOnly boundary (spec 0161 E1) and the 0.4.2 H5
exit-code contract are preserved. Validation target proven via a self-contained `docs`
domain profile fixture (mirroring the corporate IR document-production shape); the private
SB_Glome repo is the real-world reference.
