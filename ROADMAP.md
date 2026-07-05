# ROADMAP

<!--
Format for loopcoder work units:
- Each ## heading is one topic or unit.
- Each "- doc:" or "- code:" list item is one slice and becomes one issue.
- code slices depend on the doc slices in the same unit unless "(needs: ...)" is set.
- Slice refs are <unit-slug>/<kind>-<n>; within the same unit, <kind>-<n> works.
- Use "## [epic] ..." for a slice DAG; add "- doc:" / "- code:" lines for explicit slices.
-->

## Execution model (security hardening line: 0.5.1 → 0.5.2 → 0.5.3)

These three units come from an external security audit of the codebase and the decision to
build a security-audit capability into loopcoder itself. They ship as three sequential
releases so loopcoder can execute them smoothly and long-term:

1. **Sequential releases**: `0.5.1 hardening` → `0.5.2 refactor` → `0.5.3 audit`. Each is
   independently shippable, tagged, and verified against its real downloaded artifact.
2. **Doc-first**: each unit's spec merges before any of that unit's code issues open.
3. **Wave batching by file ownership**: slices are cut along file boundaries so the ready
   set can dispatch file-disjoint slices in parallel with zero merge conflicts; hot-file or
   real-dependency edges are serialized with `(needs: ...)`. (Lesson from the 0.5.0
   `config.go` collision between two parallel slices.)
4. **Self-hosting guard checkpoints**: every slice changes loopcoder-core, so each routes
   needs-human and merges by hand; after a release fully merges, rebuild the dev binary and
   restart tick before starting the next release, so the mechanics in use are the ones just
   verified.
5. **Per-slice gate**: worker → PR → loopreview (read-only verifier) → green CI → human
   merge, with attestation relayed verbatim.

## 0.5.1 — Security & robustness hardening

Fix the verified findings from the external security audit. loopcoder is a local
single-operator dev CLI, so these are hardening (mostly Low–Medium under that threat model),
not active-exploit fixes — but all are real and worth closing. The spec states the threat
model explicitly: operator-trusted config vs. UNTRUSTED input (PR/worktree contents, release
artifacts, remote MCP, shared-host local users). Preserve the H5 exit-code contract, the
ReadOnly verifier boundary, the self-hosting guard, and zero behavior change to the code
profile.

Waves: `doc` first → Wave-1 parallel {A1, A2, A3, A4, A5, A6} (six file-disjoint slices) →
Wave-2 {A7} (A7 and A6 both touch `release.yml`, so A7 needs A6).

- doc: 0.5.1 hardening spec — enumerate S1–S4/R1/P1 with code anchors, the threat model, the
  one additive config-schema change (argv-array command form for evidence-producer +
  custom-liveness), and the preserved invariants.
- code A1 — agent hardening. owns `internal/agent/{codex,claude,gemini}.go`. Write
  prompt/schema/summary files `0o600`; pass the Gemini prompt via stdin/0600 file instead of
  argv; surface codex log-read errors and distinguish parse-failure from exec-failure so
  attestation never silently reports 0 tokens. (needs: doc)
- code A2 — statebranch hardening. owns `internal/statebranch/statebranch.go`. Write log
  tails `0o600`; confine `discoverLogSources` to the run dir / configured scratch roots and
  reject absolute, `..`-escaping, and symlink sources; add regression tests. (needs: doc)
- code A3 — github robustness. owns `internal/vcs/github/github.go`. Make `runJSON` empty
  output an error unless an explicit allowEmpty is set; return partial/error from
  CreateIssue/UpdateIssue when the follow-up ViewIssue fails; paginate `gh` list calls or
  report truncation when they hit `--limit`. (needs: doc)
- code A4 — hook + runstatus bounds. owns `internal/cli/hook.go`,
  `internal/runstatus/runstatus.go`. Cap hook stdin with `io.LimitedReader`; scan runstatus
  by known filenames and bound by mtime/size/depth with diagnosable errors on oversize.
  (needs: doc)
- code A5 — config-command + supervisedexec hardening. owns `internal/config/config.go`,
  `internal/loopreview/loopreview.go`, `internal/supervisedexec/supervisedexec.go`. Add the
  additive argv-array command schema; make evidence-producer + custom-liveness accept argv
  and default to no-shell (shell only behind an explicit opt-in); run the producer under the
  supervisedexec process-group + hard timeout; make the worktree-liveness WalkDir skip
  `.git`/ignored dirs, early-exit on first newer mtime, and cap file count. (needs: doc)
- code A6 — release signing + verify. owns `.github/workflows/release.yml`,
  `scripts/install.sh`, `scripts/install.ps1`, `internal/upgrade/upgrade.go`. Sign
  `SHA256SUMS` (cosign/minisign); verify the signature in install/upgrade before trusting the
  checksum; fail closed when the signature is missing. (needs: doc)
- code A7 — CI SAST + action pinning. owns `.github/workflows/ci.yml`,
  `.github/workflows/release.yml`, `.github/dependabot.yml`. Add `govulncheck` + `staticcheck`
  as required CI checks; pin all GitHub Actions to a commit SHA; add Dependabot. (needs: doc, A6)

Checkpoint: A1–A7 merged → tag `v0.5.1` → verify the real artifact (version stamp, signature
verifies, doctor green) → rebuild the dev binary.

## 0.5.2 — Core refactor (behavior-preserving)

Decompose the god-functions and centralize scattered constants. Explicitly no behavior
change: each split ships with per-stage tests and the verifier confirms behavior-preservation.
Sequenced after 0.5.1 so it refactors already-fixed code.

Waves: `doc` first → Wave-1 parallel {B1, B2, B3} (worker / orchestration / config+agent are
disjoint) → Wave-2 {B4} solo (the defaults package cuts across many files, so it runs alone
and needs B1–B3).

- doc: refactor design — the target decomposition of `worker.Dispatch`, of orchestration
  `runTick`/`Promote`/`runDispatchWave` plus the `RenderTickText` split, a defaults/limits
  package, a shared parse-time MCP validator, and the "no behavior change" invariant + test
  strategy.
- code B1 — worker.Dispatch decomposition. owns `internal/worker/*`. Split into
  prepareWorktree / buildInvocation / runAgent / commitAndOpenPR / writeRecovery / cleanup,
  each independently tested; end-to-end behavior unchanged. (needs: doc)
- code B2 — orchestration decomposition. owns
  `internal/orchestration/{tick,promote,dispatch_wave}.go` (+ new render files). Separate
  state progression from text/JSON rendering; orchestration returns structured results only;
  behavior unchanged. (needs: doc)
- code B3 — MCP validation consolidation. owns `internal/config/config.go`,
  `internal/agent/*` (MCP validation sites). Consolidate name/transport/url/auth validation
  into a shared parse-time validator that surfaces config errors early; behavior unchanged.
  (needs: doc)
- code B4 — defaults/limits centralization. owns new `internal/defaults` + the magic-value
  call sites (cross-cutting). Export timeouts/retries/concurrency/default-branch from a single
  struct; no scattered drift. (needs: doc, B1, B2, B3)

Checkpoint: B1–B4 merged → tag `v0.5.2` → verify the real artifact → rebuild the dev binary.

## 0.5.3 — loopcoder audit (built-in security audit)

A read-only `loopcoder audit` command that institutionalizes catching this class of issue.
Two layers: a deterministic SAST floor (CI-gateable) and an LLM security-review lens
(design-level, like the external audit that started this line of work). It runs as a command
and as a required CI check; a promotion red-line is deferred. It dogfoods 0.5.0 domain
profiles: the SAST tool set is configurable (default Go set for this repo) and the LLM lens
uses an injectable security rubric.

Waves: `doc` first → C1 → C2 → C3 serial (they share the new `internal/audit` package types
and build on each other).

- doc: loopcoder audit spec — the two layers; a findings schema (severity/file/rule/evidence)
  with H5-style exit codes (clean verdict vs. command failure); a language-agnostic LLM
  threat-model rubric; configurable SAST commands via `.delivery.yml`; the CI-required
  integration; read-only + attestation; local-only output.
- code C1 — audit command + SAST runner. owns new `internal/audit/*`, `internal/cli/audit.go`,
  `internal/cli/cli.go` (registration). `loopcoder audit` runs the configured SAST set
  (govulncheck/staticcheck/gosec + a native secret & file-permission scan) and emits
  structured findings with exit codes. (needs: doc)
- code C2 — LLM security-review lens. owns new `internal/audit/review.go` (+ rubric config).
  Reuse `agent.Runner` (ReadOnly) with the security rubric + threat model; attested; degrade
  to needs-human on infrastructure failure. (needs: doc, C1)
- code C3 — CI / doctor / docs integration. owns `.github/workflows/ci.yml`, `.delivery.yml`,
  `internal/doctor/doctor.go`, `docs/*`. Wire audit into CI as a required check (loopcoder
  audits itself); report it in doctor; ship docs + an example security rubric. (needs: doc,
  C1, C2)

Checkpoint: C1–C3 merged → tag `v0.5.3` → verify the real artifact (run `loopcoder audit` as a
self-audit).

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
