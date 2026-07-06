---
id: 533
title: audit consumer-repo usability
status: draft
date: 2026-07-06
issue: 533
pr: null
supersedes: []
superseded_by: []
---

# audit consumer-repo usability

This is a design-only spec for loopcoder **0.5.4**. This PR adds only this
document: no Go code, no `.delivery.yml` change, no `ROADMAP.md` change, no
command behavior change, and no new dependency. Code slices are filed only AFTER
this spec merges, per [`docs/PROCESS.md`](../PROCESS.md).

0.5.4 makes `loopcoder audit` usable on non-Go consumer repositories without
weakening the self-audit posture specified in
[`0518-loopcoder-audit.md`](0518-loopcoder-audit.md). Issue #532 showed a
typical JS/TS repository producing 492 findings with 0 true positives. That is a
scope gap that manifests as real default-behavior bugs, not a regression:
0.5.3 audit was designed and tuned first to self-audit loopcoder's own Go
repository, while non-Go repositories were given a configuration path through
`audit.sast.commands`. The 0.5.3 spec did not design native scanning for a
foreign JS/TS tree and was silent on `.gitignore`, `node_modules`,
`process.env`, and Windows permission-bit behavior.

This spec narrows the native audit defaults, improves secret precision, makes
the CI gate adoptable through baseline-diff semantics, and makes the existing
`audit:` config discoverable. It preserves the 0.5.3 invariants: audit remains
read-only, exit codes keep the H5-style verdict/runtime split, and the LLM lens
remains an attested read-only verifier invocation.

## Goals

- Make `loopcoder audit` usable by default on consumer repositories, including
  JS/TS repositories, without scanning ignored dependency and build trees.
- Treat #532 as a 0.5.3 scope gap whose default behavior is buggy in consumer
  repositories, not as a regression from an earlier supported JS/TS native scan.
- Make the native Layer 1 file set git-tracked by default in Git repositories.
- Keep non-Git directories supported by falling back to `filepath.WalkDir` with
  conservative default excludes.
- Improve native secret detection so high-confidence signatures gate CI while
  generic keyword assignments are entropy-filtered and warn by default.
- Make the Windows file-permission scanner produce zero false positives out of
  the box instead of treating synthesized Unix bits as evidence.
- Make the existing `audit:` config schema discoverable in `loopcoder init` and
  the bundled operator playbook.
- Preserve loopcoder's own self-audit-green posture and all 0518 invariants.

## Threat model / what changed and why

The 0518 threat split remains the floor.

Operator-trusted inputs:

- the operator's own `.delivery.yml`;
- the repository in which the operator intentionally runs loopcoder;
- local tools the operator has installed and configured;
- command argv explicitly authored by the operator.

Untrusted inputs:

- PR and worktree file contents, including build scripts and generated files;
- downloaded release artifacts and checksum/signature files;
- remote MCP servers and data returned by them;
- local users on a shared host who can read permissive temp files, process
  arguments, logs, or state records.

The new consumer-repo usability threat is scanner noise that makes the audit
gate unusable. A scanner that emits hundreds of false positives is not merely
annoying; it pushes operators to disable the gate entirely, hide real
high-confidence findings, or stop running the command outside loopcoder's own Go
tree. The remedy is precision by construction, not broader waivers.

Confirmed current defects that this spec designs fixes for:

- `internal/audit/native.go` `auditFiles` uses `filepath.WalkDir` and never
  reads `.gitignore`. In #532, 485 of 492 findings came from git-ignored
  `node_modules/`, build directories, and loopcoder's own `.loopcoder/`. Layer 2
  already has the needed capability in `internal/audit/review.go`
  `listAuditReviewFiles` / `gitTrackedFiles`, which uses `git ls-files`.
- `internal/audit/native.go` `secretAssignmentPattern` matches
  `NAME = <value>` for names such as `api_key`, `secret`, `token`, `password`,
  and `private_key`, but does not filter env reads such as `process.env.X`,
  `os.Getenv`, `os.environ`, or `System.getenv`; it also lacks an entropy floor
  and does not skip `*.example`, `*.sample`, or `*.template` files.
- The `audit:` config schema exists in `internal/config/config.go`, including
  `exclude`, native toggles, and `baseline`, but `internal/scaffold/scaffold.go`
  emits no `audit:` block, initializes `ci.checks: []`, and the bundled
  `SKILL.md` / entrypoint docs do not make the remedy discoverable.
- The native file-permission scanner is effectively broken on Windows because
  Go synthesizes Unix permission bits. Checking `mode & 0o077` is near-always
  non-zero for matched sensitive paths, so Windows can report false positives
  for every matched file.

## Layer 1 - deterministic SAST floor

Layer 1 remains deterministic and read-only. The configured SAST command runner
from 0518 does not change in this line of work. The 0.5.4 changes are limited
to the native scan file set, native secret precision, native file-permission
semantics, and gate classification.

Native file selection rules:

- In a Git repository, the native layer MUST scan only git-tracked files.
- The native layer MUST reuse the existing `gitTrackedFiles` / `git ls-files`
  approach already used by `internal/audit/review.go` for Layer 2. The
  implementation MUST NOT introduce a new third-party dependency for this.
- In a Git repository, if `git ls-files` cannot produce a trustworthy tracked
  file list, the native scan MUST NOT silently fall back to walking the whole
  tree. It must return a command/runtime failure or `needs-human` result,
  according to the command layer's existing error taxonomy.
- In a non-Git directory, the native layer MUST fall back to `filepath.WalkDir`
  with default exclude globs.
- The default native exclude set MUST always exclude `.git/**`, `.loopcoder/**`,
  `node_modules/**`, `vendor/**`, `dist/**`, and common generated build output
  directories such as `build/**`, `coverage/**`, `.next/**`, and `out/**`.
- loopcoder's own `.loopcoder/` directory MUST always be excluded, regardless
  of user include globs.
- User include/exclude config MUST be applied after the tracked-file source is
  chosen. Include globs narrow the candidate set; exclude globs remove from it.
- A heavyweight gitignore-aware walker, such as `gocodewalker`, is deferred
  future work only. It MUST NOT be added in this implementation line.

Native secret-detection rules:

- Secret detection MUST be signature-first.
- Known-format tokens are the high-confidence tier and MUST gate CI when net
  new. The initial signature set MUST include GitHub classic tokens (`ghp_`),
  GitHub fine-grained tokens (`github_pat_`), Stripe live keys (`sk_live_`), AWS
  access keys (`AKIA...`), PEM private-key blocks (`BEGIN ... PRIVATE KEY`), and
  JWT-looking values beginning with `eyJ`.
- High-confidence signature findings MUST redact raw values from evidence and
  fingerprints. Evidence may identify the token family and bounded location, but
  it MUST NOT print the credential.
- The generic `NAME=<value>` / `NAME: <value>` rule MUST be downgraded behind a
  Shannon-entropy floor and MUST NOT gate CI by default.
- The generic rule MUST drop env reads, including at least `process.env`,
  `os.Getenv`, `os.environ`, and `System.getenv`.
- The generic rule MUST drop templated or placeholder values, including
  `${...}`, `{{...}}`, and `<...>` placeholders.
- The generic rule MUST drop example and template material under
  `*.example`, `*.sample`, `*.template`, and test-fixture paths.
- Entropy/keyword-tier findings SHOULD remain visible in human and JSON output
  as warnings so users can inspect them without making the default gate
  unusable.

Native file-permission rules:

- The sensitive-write source scan from 0518 remains cross-platform because it
  scans source code for broad mode literals.
- The file-permission scan that inspects actual filesystem mode bits MUST NOT
  be near-always-true on Windows.
- On Windows, the implementation MUST skip the Unix mode-bit check unless it can
  key off a real ACL signal. The default Windows behavior MUST produce zero
  file-permission false positives out of the box.
- Unix behavior MUST remain intact: sensitive local files that are group/world
  readable or writable still produce findings when real Unix mode bits show
  permissions broader than owner-only.

Gate classification rules:

- Signature-tier native findings are gate findings.
- Entropy/keyword-tier native findings are warning findings by default.
- SAST tool findings keep their configured parser severity and threshold
  behavior from 0518.
- Sorting, redaction, stable fingerprints, bounded evidence, and read-only
  behavior from 0518 remain mandatory.

## Layer 2 - LLM security-review lens

Layer 2 remains an adversarial, language-agnostic, read-only verifier lens. This
spec does not add a new provider role, prompt layer, or required hosted CI
dependency.

Layer 2 already demonstrates the desired file-selection posture:
`internal/audit/review.go` `listAuditReviewFiles` uses `gitTrackedFiles` /
`git ls-files` when a repository is Git-backed and falls back to a bounded walk
for non-Git directories. 0.5.4 should reuse or extract that capability for
Layer 1 instead of inventing a second mechanism.

Layer 2 MUST continue to:

- reuse `agent.Runner` and `agent.Invocation`;
- set `Invocation.ReadOnly = true`;
- use read-only verifier semantics and local-only attestation;
- preserve the closed audit verdict set: `clean`, `findings`, or
  `needs-human`;
- degrade to `needs-human` on provider timeout, provider infrastructure error,
  unreadable configured rubric, malformed JSON, schema violation, missing
  attestation, or relay-write failure;
- keep pretty attestation blocks, relay records, logs, and audit review records
  out of repository-visible artifacts.

Layer 2 MUST NOT become the default hosted CI dependency for consumer-repo
adoption. CI may opt into it explicitly, but the default required gate remains
deterministic Layer 1.

## Findings schema

The 0518 normalized finding schema remains in force. 0.5.4 adds only
classification expectations for native secret findings:

- Signature-tier secret findings MUST use a rule or metadata value that lets
  renderers and gates distinguish them from entropy/keyword findings.
- Entropy/keyword findings MUST be representable as warnings below the default
  gate posture without hiding them from JSON or human output.
- Findings MUST NOT include raw secret values, local-only attestation JSON,
  pretty blocks, or unbounded file content.
- Fingerprints MUST be stable across machines and MUST NOT include the raw
  credential.

The implementation may extend optional metadata only when it remains additive
and unknown-field-tolerant for existing consumers. It MUST NOT break the
required top-level result object or required finding fields from 0518.

## Exit codes

The 0518 exit-code contract remains the command contract:

- `0`: audit verdict `clean`.
- `1`: audit verdict `findings`.
- `2`: audit verdict `needs-human`.
- `3`: command/runtime failure.

0.5.4 changes which native findings are gate findings by default. Net-new
signature-tier findings are threshold findings and can produce exit code `1`.
Entropy/keyword-tier native findings are warnings by default and MUST NOT
produce exit code `1` unless a user explicitly configures a stricter posture.

Baseline-diff gating MUST be evaluated before the exit code is selected. A
pre-existing baselined signature finding remains printed and counted as waived;
it does not fail the gate until the waiver expires, becomes invalid, no longer
matches narrowly, or a net-new matching finding appears outside the baseline.

Command/runtime failures still take precedence over `needs-human`, findings, and
clean verdicts. Relay hard-gate exit code `4` remains outside the audit verdict
space and MUST NOT be overloaded.

## Configuration

The existing additive `.delivery.yml` `audit:` schema from 0518 and
`internal/config/config.go` remains the configuration surface. 0.5.4 MUST NOT
invent a second schema for consumer repositories.

Example discoverable scaffold block:

```yaml
# audit:
#   # Optional. Absent = medium.
#   severity_threshold: medium
#   sast:
#     # Optional. Configure repository-native SAST commands for non-Go repos.
#     # commands:
#     #   - id: eslint-security
#     #     argv: ["npm", "run", "lint:security", "--", "--format", "json"]
#     #     parser: "<supported-parser>"
#     #     timeout_seconds: 300
#     native:
#       # Optional. Absent = true.
#       secrets: true
#       # Optional. Absent = true; Unix mode-bit checks are skipped on Windows
#       # unless a future implementation uses real ACL evidence.
#       file_permissions: true
#       # Optional. Native scans use git-tracked files in Git repositories.
#       include:
#         - "**/*"
#       exclude:
#         - ".git/**"
#         - ".loopcoder/**"
#         - "node_modules/**"
#         - "vendor/**"
#         - "dist/**"
#         - "build/**"
#         - "coverage/**"
#         - ".next/**"
#         - "out/**"
#   review:
#     # Optional. Repo-relative rubric extending the built-in threat model.
#     rubric_path: docs/security/audit-rubric.md
#   baseline:
#     # Optional. Known findings file used for net-new CI gating.
#     path: docs/security/audit-baseline.yml
```

Normative config and discoverability rules:

- `audit.sast.native.exclude` MUST support excluding generated and ignored
  paths, but git-tracked file selection MUST be the default in Git repos even
  when no exclude list is configured.
- `audit.sast.native.secrets` and `audit.sast.native.file_permissions` MUST
  remain the native toggles.
- `audit.baseline.path` MUST remain the baseline path field for known findings.
- `loopcoder init` MUST scaffold a commented `audit:` block showing exclude
  globs, native toggles, and baseline path.
- `loopcoder init` MUST NOT silently add `audit` to `ci.checks`; the scaffold
  may show how to configure it, but CI gate adoption remains an operator choice
  for consumer repositories.
- The bundled `SKILL.md` MUST document the `audit:` schema enough for operators
  to discover the native excludes, native toggles, and baseline path. Entrypoint
  docs such as `AGENTS.md` may point back to `SKILL.md` rather than duplicate
  the schema.
- The config surface remains additive, optional, snake_case, default-safe, and
  unknown-field-tolerant.

## Baselines & pre-existing findings

0.5.4 deliberately defaults CI gating to net-new findings through
baseline-diff. This differs from #532's dismissal of baselines. Net-new gating
is the standard way to make a scanner adoptable as a gate: it prevents existing
noise from blocking first adoption while still failing new high-confidence
regressions.

Baseline rules:

- The gate MUST fail only on net-new gate findings.
- Signature-tier findings are gate findings. A net-new signature-tier finding
  MUST fail the gate.
- Entropy/keyword-tier native findings are warning findings by default. They
  MUST NOT fail the gate unless the repository explicitly opts into stricter
  gating.
- A baseline MUST be narrow. It must match stable fingerprint evidence or an
  equivalently specific rule/path/evidence tuple.
- A baseline MUST NOT broadly suppress all findings under generated,
  dependency, or ignored directories. Those directories should be removed from
  the native file set instead.
- Missing, malformed, expired, or overly broad waivers MUST NOT silently
  suppress findings. They must be reported as active findings or
  `needs-human`, following the 0518 baseline rules.
- Waived findings remain visible in output with `waived: true` and are counted
  separately.
- Stale waivers should be reported so repositories can delete unnecessary
  baseline entries.

loopcoder's own self-audit must stay green. If 0.5.4 implementation finds a real
pre-existing issue in loopcoder itself, the code slice must either fix it when
small and behavior-safe or baseline it narrowly with a recorded justification,
date, and review/expiry date. It MUST NOT hide self-findings through broad
excludes, generated noise, or silent waiver expansion.

## Command and output behavior

`loopcoder audit` remains the command entrypoint. The exact flag spelling for
new baseline-diff or warning-tier controls is a code-slice decision, but the
behavioral contract is fixed:

- The command remains read-only and MUST NOT write tracked files, comments, PR
  bodies, issue bodies, merge artifacts, or commits.
- Human text output MUST distinguish gate findings, warnings, waived findings,
  and `needs-human` items.
- JSON output MUST expose enough classification for CI and downstream scripts to
  distinguish signature-tier gate findings from entropy/keyword warnings.
- Output MUST explain when the native file source is `git ls-files` versus the
  non-Git fallback walk.
- Output MUST NOT include raw secret values.
- Output MUST NOT include local-only worker, verifier, conductor, or audit
  attestation data in repository-visible artifacts.

## CI integration

The default CI posture for consumer repositories is baseline-diff gating:

- Required CI gates SHOULD run deterministic Layer 1 explicitly.
- Required CI gates MUST fail on net-new gate findings, `needs-human`, and
  command/runtime failures.
- Required CI gates MUST NOT fail on pre-existing validly baselined findings.
- Required CI gates MUST NOT fail on entropy/keyword-tier native warnings unless
  the repository explicitly opts into stricter gating.
- Signature-tier findings MUST fail the gate when net new.
- CI output remains local to logs/artifacts and MUST NOT be committed back to
  the repo.

For loopcoder's own repository, the required `audit` check from 0518 remains in
force and must stay green. This 0.5.4 line MUST preserve the current self-audit
posture on loopcoder's Go repo while making consumer-repo defaults quieter and
more precise.

Promotion red-lines based on audit status remain deferred. `tick`,
`risk-gate`, and `promote` behavior do not change in this line of work.

## Doctor integration

`loopcoder doctor` should make the consumer-repo audit posture legible locally.
It should report:

- whether the audit config parses;
- the effective severity threshold;
- whether native scans will use git-tracked files or non-Git fallback walking;
- the effective native exclude set, including `.loopcoder/**`;
- whether configured SAST commands will run;
- whether required command tools are available on `PATH`;
- whether configured parsers are recognized;
- whether the configured rubric path exists;
- whether the baseline file parses and contains no expired, stale, or broad
  waivers;
- whether the current platform skips Unix file-permission checks or uses real
  permission evidence.

Doctor diagnostics are advisory unless they reflect invalid config or a
condition that would make `loopcoder audit` fail. Doctor MUST NOT run the LLM
review itself.

## Preserved invariants

- 0518 read-only audit is preserved. Audit reports findings; it does not mutate
  tracked repository files or publish comments, commits, PR bodies, issue
  bodies, or merge artifacts.
- 0518's two-layer model is preserved: deterministic Layer 1 and optional
  read-only LLM Layer 2.
- 0161 E1 is preserved: `agent.Runner` remains the provider door,
  `agent.Invocation` remains the provider-neutral request, and
  `Invocation.ReadOnly` remains the permission boundary.
- 0161 M2 self-hosting guard is preserved. This line changes loopcoder core
  behavior only after human merge, rebuild, and tick restart.
- 0161 M3 is preserved. The audit config remains additive, optional,
  snake_case, default-safe, and unknown-field-tolerant.
- F1-F5 are preserved: tick does not merge production, verifier remains
  read-only, promotion is distinct, guardrails continue to gate dispatch waves,
  and all LLM roles still attest.
- The 0.4.2 H5 discipline is preserved: clean audit verdict exit codes are
  distinct from command/runtime failures.
- 0.5.1 hardening must not regress. Audit implementation must preserve release
  signing, restrictive sensitive file modes where meaningful, path confinement,
  no-shell argv command forms, honest failure reporting, hook/runstatus bounds,
  and bounded worktree liveness.
- 0.5.2 refactor behavior-preservation must not be bypassed. Audit code should
  reuse centralized defaults and helpers when those exist instead of
  reintroducing scattered constants.
- Local-only attestation remains local-only. Audit must not copy worker,
  verifier, conductor, or audit-review attestation data into repository-visible
  artifacts.
- Relay obligations apply to Layer 2 exactly as they apply to loopreview's
  read-only verifier invocation.
- loopcoder's own self-audit-green posture is preserved.

## Follow-up code slices

The implementation is intentionally decomposed into separate future code issues.
The dependency order is:

1. Doc first: merge this spec.
2. C1: native file selection and default excludes.
3. C2: native secret precision and finding tiers.
4. C3: Windows file-permission behavior.
5. C4: baseline-diff gate posture and output classification.
6. C5: scaffold, playbook, doctor, and user-doc discoverability.
7. Checkpoint: run loopcoder's self-audit and a representative consumer-repo
   audit fixture to verify both the Go self-audit posture and JS/TS usability.

C1-C5 are serial because later slices depend on the native file set, finding
classification, and baseline behavior introduced by earlier slices.

### C1 - native file selection and default excludes

Owned files: `internal/audit/native.go`, any shared audit file-list helper,
native scan tests, and focused config/default tests.

Acceptance:

- In Git repositories, native scans use `git ls-files` and scan only tracked
  files.
- The implementation reuses or extracts the existing `gitTrackedFiles` pattern
  from `internal/audit/review.go`.
- In Git repositories, `git ls-files` failure does not silently fall back to a
  full filesystem walk.
- In non-Git directories, native scans fall back to `filepath.WalkDir` with the
  default exclude set.
- `.loopcoder/**` is always excluded.
- The default exclude set covers `.git/**`, `.loopcoder/**`, `node_modules/**`,
  `vendor/**`, `dist/**`, `build/**`, `coverage/**`, `.next/**`, and `out/**`.
- User include/exclude globs are applied after the file source is chosen.
- No third-party file walker is added.
- Tests prove ignored `node_modules/`, build output, and `.loopcoder/` files are
  not scanned in a Git-backed fixture.

### C2 - native secret precision and finding tiers

Owned files: `internal/audit/native.go`, finding classification types/rendering
as needed, and native secret tests.

Acceptance:

- Signature-first detection covers at least `ghp_`, `github_pat_`, `sk_live_`,
  `AKIA...`, PEM private-key blocks, and JWT-looking `eyJ...` values.
- Signature-tier findings are gate findings when net new.
- Generic `NAME=<value>` findings require a Shannon-entropy floor.
- Generic findings drop env reads for `process.env`, `os.Getenv`, `os.environ`,
  and `System.getenv`.
- Generic findings drop `${...}`, `{{...}}`, and `<...>` placeholders.
- Generic findings drop `*.example`, `*.sample`, `*.template`, and test-fixture
  paths.
- Raw secret values are never printed in evidence, JSON, or fingerprints.
- Tests cover true signature examples, env-read false positives, placeholders,
  examples/templates, low-entropy keyword assignments, and redaction.

### C3 - Windows file-permission behavior

Owned files: `internal/audit/native.go` and platform-specific native permission
tests.

Acceptance:

- Unix mode-bit file-permission checks are skipped on Windows unless a real ACL
  signal is implemented.
- Windows default audit produces zero file-permission false positives for
  sensitive paths solely because Go synthesized mode bits.
- Unix behavior remains unchanged for real group/world-readable sensitive
  files.
- Sensitive-write source scanning remains cross-platform.
- Tests cover Windows skip semantics and Unix mode behavior with
  platform-appropriate build tags or fakes.

### C4 - baseline-diff gate posture and output classification

Owned files: `internal/audit/baseline.go`, audit result/rendering code, CLI gate
logic, and focused tests.

Acceptance:

- The default gate fails only on net-new gate findings.
- Signature-tier findings fail when net new.
- Entropy/keyword-tier native findings warn by default and do not fail the gate.
- Valid baselined findings remain visible with `waived: true`.
- Missing, malformed, expired, broad, or mismatched waivers do not silently
  suppress findings.
- Human text and JSON output distinguish gate findings, warnings, waived
  findings, and `needs-human` items.
- Exit-code precedence from 0518 is preserved.
- Tests cover clean baseline diff, net-new signature failure, warning-only
  generic findings, expired waiver behavior, stale waiver reporting, and
  command/runtime precedence.

### C5 - scaffold, playbook, doctor, and user-doc discoverability

Owned files: `internal/scaffold/scaffold.go`, `SKILL.md`, entrypoint docs as
needed, `internal/doctor/doctor.go`, and focused user docs under `docs/`.

Acceptance:

- `loopcoder init` scaffolds a commented `audit:` block with native toggles,
  exclude globs, and baseline path.
- The scaffold keeps `ci.checks: []` unless the operator explicitly configures
  checks.
- The bundled `SKILL.md` documents the `audit:` schema and consumer-repo
  adoption path.
- Entrypoint docs such as `AGENTS.md` do not fork the schema; they either remain
  pointers or link back to `SKILL.md`.
- Doctor reports whether native scans use git-tracked files or non-Git fallback
  walking, the effective native exclude set, baseline health, parser/tool
  readiness, and Windows permission-scan posture.
- User docs explain git-tracked native scans, secret tiers, baseline-diff CI
  gating, Windows permission behavior, and consumer-repo configuration.
- No `ROADMAP.md` change is bundled into this implementation line.

## Relationship to existing specs

- [`0518-loopcoder-audit.md`](0518-loopcoder-audit.md) remains the parent audit
  spec. This spec extends it for 0.5.4 consumer-repo usability and does not
  replace its command model, finding schema, exit-code contract, read-only
  behavior, or attested LLM lens.
- [`0161-autonomous-delivery-loop.md`](0161-autonomous-delivery-loop.md) remains
  the parent for E1, M2, M3, and F1-F5. This spec changes audit defaults and
  discoverability without changing the delivery loop.
- [`0459-domain-profiles.md`](0459-domain-profiles.md) remains relevant because
  consumer repositories still configure their own deterministic SAST commands
  and rubric extensions.
- [`0484-security-robustness-hardening.md`](0484-security-robustness-hardening.md)
  remains the 0.5.1 hardening foundation. 0.5.4 must not weaken the shared-host
  disclosure, path confinement, release integrity, or local-only output line.
- [`0507-core-refactor.md`](0507-core-refactor.md) remains the 0.5.2 foundation.
  Audit changes should reuse centralized helpers instead of reintroducing
  scattered constants.
- [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md)
  remains the reliable read-only verifier pattern for Layer 2.
- [`0423-operational-reliability-hardening.md`](0423-operational-reliability-hardening.md)
  remains the H5 exit-code foundation. 0.5.4 preserves the verdict/runtime
  split.
- [`0146-attestation.md`](0146-attestation.md),
  [`0306-local-only-attestation.md`](0306-local-only-attestation.md), and
  [`0447-relay-enforcement-hardgate.md`](0447-relay-enforcement-hardgate.md)
  remain in force for the Layer 2 LLM review.

## Non-goals

- No Go implementation, workflow change, `.delivery.yml` change, `ROADMAP.md`
  change, command behavior change, or dependency addition in this design-doc PR.
- No new third-party file walker in this line of work.
- No built-in JS/TS SAST command suite. Consumer repositories still configure
  their own language SAST tools through `audit.sast.commands`.
- No auto-fix mode. Audit reports findings only.
- No promotion red-line, risk-gate rule, or `promote` behavior change.
- No new provider abstraction, provider role, or LLM runner.
- No weakening of the self-hosting guard, read-only verifier boundary,
  guardrails, relay obligations, local-only attestation, H5-style exit-code
  split, 0.5.1 hardening, or 0.5.2 behavior-preservation line.
- No claim that deterministic SAST proves a repository secure. The SAST floor is
  a minimum gate; Layer 2 and human review still matter for design-level risk.
