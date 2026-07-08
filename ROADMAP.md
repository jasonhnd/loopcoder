# ROADMAP

<!--
Format for loopcoder work units:
- Each ## heading is one topic or unit.
- Each "- doc:" or "- code:" list item is one slice and becomes one issue.
- code slices depend on the doc slices in the same unit unless "(needs: ...)" is set.
- Slice refs are <unit-slug>/<kind>-<n>; within the same unit, <kind>-<n> works.
- Use "## [epic] ..." for a slice DAG; add "- doc:" / "- code:" lines for explicit slices.
-->

## 0.6.1 — Customer-ready bridge release — current public 0.6 target

Shipping target. v0.6.1 is the customer-ready bridge for the public 0.6 line.
It packages the 0.6 capabilities for external consumers while keeping larger
SQLite/global-project-registry and native sub-agent orchestration work out of
scope for a later 0.7.0 line.

- docs: release truth across README, usage, stability policy, changelog,
  roadmap, and v0.6.1 release note.
- docs: customer quickstart in install/version/init/skill-install/doctor/report
  order, including the command side-effect table and `.loopcoder/` local-state
  boundary.
- code: local `.loopcoder/` protection through `.git/info/exclude` in
  `init --repo` and `skill install --repo`.
- code: first-run `init --repo/--gate`, defaulting new scaffolds to
  `adapters.gate: human-merge`.
- code: `doctor --format text|json`, local-state checks, reportquery
  readability, installed-skill and hook checks.
- code: `report --format json` compatibility array plus richer `records`
  entries with source, run id, and local path.

## 0.6.0 — Model & depth selection: discovery, validation, defaults (+ agy provider)

Implemented for the 0.6 line and released to customers through v0.6.1. Make models and their depth tiers discoverable, validated, and defaulted so operators
choose without guessing. Target models: **claude, gpt (codex), gemini** — gemini reached **via
the Antigravity (`agy`) provider**, since the direct gemini CLI is dead for personal accounts
(Google `IneligibleTier`). Depth is a **per-model list of valid tokens that may be empty** (not a
cross-provider scale), wired per provider: claude `--effort`, codex `-c model_reasoning_effort=`
(both **pass-through today — loopcoder does no effort validation yet**), agy = whatever
`agy models` lists per model (`Gemini 3.1 Pro`→[Low,High], `Opus 4.6`→[Thinking],
`GPT-OSS 120B`→[Medium]). The registry's per-model tiers are **loopcoder-curated** (not
provider-derived); the parse-time membership check is **net-new** (today effort is unvalidated
pass-through). All three target models have depth — gemini's comes from agy's tiers. Effort
against an empty list is a warning.

Selection is **per-role**: worker and verifier each choose their own model+depth (config;
per-role override already shipped in spec 0215) and both are validated against the registry. The
conductor is the host session driving loopcoder, not a dispatched subprocess — its model is the
operator's host choice, so loopcoder does not switch it.

Dispatch is **natural-language**: the operator tells the conductor "use gemini, deeper" and the
conductor translates it to `--provider/--model/--effort`. The registry is the conductor's
ground-truth for valid model+depth (translation targets real options) and validation rejects
invalid names; the reporter's recorded model+depth per work-ID is the operator's confirmation
that the translation matched intent — validation catches *invalid* picks, the reporter catches
*wrong-but-valid* ones.

- doc: spec — static model registry (per provider: models × depth tiers + defaults);
  `loopcoder models [--provider]`; parse-time validation (warn default, `--strict` rejects);
  provider defaults; agy provider + its OAuth-login prerequisite.
- code: `internal/models` **leaf package** (static registry + pure validation + defaults;
  imports no orchestration/config/agent — they import it) + provider default model/depth.
- code: `loopcoder models [--provider]` command — print models × depth × default per provider
  from the static registry. (dynamic `--refresh` / `agy models` reconcile deferred to a later
  version; the static registry alone delivers discovery/validation/defaults.)
- code: parse-time validation of worker/verifier `model`+`reasoning_effort` vs registry (warn
  by default; `.delivery.yml`/CLI `--strict` escalates to reject).
- code: `internal/agent/antigravity.go` agy runner (close stdin, `-p`, plain-text summary,
  self-reported model, vendor "Google Antigravity"); register `antigravity`; depth via
  `model`+`reasoning_effort` → `"<model> (<Depth>)"`; **MUST pin the worktree as agy's workspace
  via `--add-dir <worktree>` (verified fix) — agy otherwise ignores process CWD and writes to its
  own `~/.gemini/antigravity-cli/scratch` (silent wrong-directory writes, exit 0)**; `loopcoder
  doctor` checks agy OAuth login so a missing login fails clearly.
- code: docs/reference — `loopcoder models` usage, model/depth config, agy setup + login.

## 0.6.0 — reporter (attestation → reporter rename + light strengthening)

Implemented for the 0.6 line and released to customers through v0.6.1. Rename `attestation`→`reporter` **including the operator-visible
`[attestation]`→`[reporter]` token** (a rename nobody can see is pointless) — Go package, type,
emitted header, and all human prose. The hard part is the relay hard-gate: the token is emitted,
matched (`relay_guard.go`), and instructed for verbatim relay (SKILL.md, host-hook templates,
GEMINI.md/AGENTS.md); a naive swap risks relay lock-out / fail-open on upgrade-lag between
binary, skill manual, and host hooks. So it ships with a transition window, not a raw swap.
Sequence: land Unit A (incl. `antigravity.go`) before this rename sweep, so the sweep renames
the new agy file in one pass instead of colliding with it.

- doc: spec — rename map + **full consumer inventory** (grep: ~1068 refs / 60 files across
  emit + match + manual: cli.go, worker.go, audit/*, agent/* providers, claudehooks,
  cli/hook.go, cli/pretty.go, doctor, guardrails, loopreview, conductor hooks, `relay_guard.go`,
  SKILL.md, GEMINI.md, AGENTS.md, hooks/*); **freeze CHANGELOG + shipped `docs/specs/*` history,
  CanonicalJSON field names, and the `.attest` ledger extension** (invisible machinery — same
  rationale as freezing schema fields: changing them adds transition risk for zero operator-
  visible gain); invariant: `Validate()` keeps accepting agy self-reported model + absent tokens.
- code: emit `[reporter]`; rename `internal/attestation`→`internal/reporter`,
  `AttestationRecord`→`Report`, all Go identifiers + pretty wording + current `docs/reference/*`;
  sweep every emit + match + manual site in lockstep; update golden/inventory tests.
- code: **transition safety** — `relay_guard.go` (`ledgerHeaderRe`, `rolePattern`) **and
  `conductorhooks/attest.go` (`conductorHeaderRe`)** accept BOTH `[attestation]` and `[reporter]`
  for this release so upgrade-lag (binary vs propagated skill manual vs host-side hooks) can
  neither lock out nor fail open; drop `[attestation]` acceptance one version later. (relay
  hard-gate spec 0447 + the "blocking gate must not lock out" rule)
- code: strengthen — model+depth display (`Gemini 3.1 Pro (High)`); **work-ID = the worker's
  internal `RunID` (spec 0390; unique per dispatch, set as `LOOPCODER_RUN_ID`), surfaced in every
  report — note dispatch's `--run-id` flag is currently ignored (0.5.4 learning), so the ID is
  loopcoder-generated, not user-set**; add `issue`/`branch`/
  `worktree`/`round` context fields (dispatch/tick-filled, optional); **prefer loopcoder-observed
  ground-truth** (dispatched model+depth, process timing, parsed tokens) over agent self-report,
  marking `self-reported` only where unobservable (e.g. agy); pretty grouping
  (who·what·result·cost); must not re-tighten validation against agy. (needs: 0.6.0 models
  registry for depth display)
- code: `loopcoder report` — on-demand, read-only command to list/query recent per-work reports
  (work-ID, role, model+depth, start/end, tokens) from the persisted log; this is how the
  reporter subsystem "reports at any time".
- code: docs/reference — reporter concept + attestation→reporter prose in usage.md/worker.md
  (CHANGELOG + shipped specs frozen).

## 0.6.0 — Upgrade, migration & operational health (doctor)

Implemented for the 0.6 line and released to customers through v0.6.1. 0.6.0 is the first BREAKING release (reporter rename), so it must ship a clean upgrade
from 0.5.x and a defined self-check. Everything we froze (`.attest` ledgers, CanonicalJSON field
names) migrates as a no-op; only renamed config keys and stale logs need real handling.

- doc: spec — upgrade/migration plan (old-version detection, config-key migration map, log
  retention policy) + `loopcoder doctor` definition (who runs it, what it checks, which roles).
- code: `loopcoder upgrade` 0.5.x→0.6.0 — detect old version; migrate any renamed config keys
  (from the reporter rename); keep frozen machinery intact; idempotent, no data loss; the relay
  dual-token window (Unit B) must stay valid across the version boundary.
- code: old-file handling — config-key migration; **bounded cleanup of stale logs** (run logs,
  worktree-liveness, relay state) under a retention policy; leave `.attest` ledgers + schema
  untouched.
- code: `loopcoder doctor` — **operator-run; default diagnoses only (read-only, safe anytime)**:
  git + provider CLIs present; **per-provider auth/reachability** (codex, claude, agy OAuth
  login); config validity (worker/verifier model+depth vs registry); reporter/relay wiring sane;
  version + upgrade status — reports each problem + its fix command, changing nothing. **`--fix`
  (explicit opt-in) performs the mutations** (upgrade, stale-log cleanup, config-key migration);
  doctor is the operational-health entry point but never changes state by default. (Doctor is
  not a role — it verifies readiness FOR worker/verifier dispatch and conductor/relay wiring.)
- code+docs: **0.6.0 CHANGELOG + Release Note + README** — detailed feature + how-to-use docs;
  and establish a standing per-release rule in `docs/reference/releasing.md`: every version bump
  rewrites all three (changelog / release note / README), as complete and detailed as possible.

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
