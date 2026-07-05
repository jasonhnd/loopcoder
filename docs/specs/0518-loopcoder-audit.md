---
id: 518
title: loopcoder audit (built-in security audit)
status: draft
date: 2026-07-05
issue: 518
pr: null
supersedes: []
superseded_by: []
---

# loopcoder audit (built-in security audit)

This is a design-only spec for loopcoder **0.5.3**. This PR adds only this
document: no Go code, no `.delivery.yml` change, no command behavior change, and
no new dependency. Code slices are filed only AFTER this spec merges, per
[`docs/PROCESS.md`](../PROCESS.md).

0.5.3 adds a read-only `loopcoder audit` command that institutionalizes the
kind of security and robustness review that led to the 0.5.1 hardening release.
The command must help loopcoder audit any repository, including itself, on demand
and in CI. It must catch both deterministic static-analysis findings and the
design-level trust-boundary mistakes that ordinary SAST misses.

The command has exactly two audit layers:

1. a deterministic SAST floor that is CI-gateable; and
2. an LLM security-review lens that is adversarial, language-agnostic,
   read-only, and attested.

No third audit layer, auto-fix mode, promotion red-line, or new provider role is
part of 0.5.3.

## Goals

- Add `loopcoder audit` as a standalone, read-only command.
- Run a deterministic SAST floor with sensible Go defaults for this repository
  and configurable commands for other repositories.
- Add a language-agnostic LLM security-review lens that reuses
  `agent.Runner` with `Invocation.ReadOnly = true`.
- Emit structured findings with a stable schema and H5-style exit codes that
  distinguish clean audit verdicts from command/runtime failures.
- Dogfood 0.5.0 domain-profile ideas by making the SAST command set and
  security rubric configurable through additive `.delivery.yml` fields.
- Wire the deterministic audit floor into CI as a required `audit` check and
  into `loopcoder doctor` readiness reporting.
- Keep output and attestation local-only. The command reports; it does not write
  PR comments, issue comments, commits, merge artifacts, or tracked files.

## Threat model and rubric

The built-in security rubric starts with the same trust split used by the 0.5.1
hardening work.

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

The LLM lens must evaluate issues through that split, not through the false
assumption that every operator-authored command string is attacker input. The
rubric is language-agnostic and must cover, at minimum:

- supply-chain integrity and release-artifact verification;
- shared-host disclosure through files, logs, prompts, argv, or local state;
- path traversal, symlink escape, and path-confinement gaps;
- execution of trusted tools inside untrusted worktrees;
- read-only verifier boundaries, including MCP server classification;
- honest failure reporting and exit-code separation;
- local-only attestation and relay obligations;
- bounded local file reads, output sizes, and scan scope;
- additive config compatibility and absent-config behavior.

Repositories may provide an additional rubric file through `.delivery.yml`. The
configured rubric augments the built-in rubric; it cannot remove the trust-model
floor above.

## Layer 1 - deterministic SAST floor

Layer 1 is deterministic and read-only. It runs configured static-analysis
commands, normalizes their output into the audit finding schema, runs native
loopcoder scans, and returns an aggregate audit verdict.

The default Go SAST set for a repository with `go.mod` is:

- `govulncheck -json ./...`
- `staticcheck -f json ./...`
- `gosec -fmt json -quiet ./...`
- native secret-pattern scan
- native file-permission and sensitive-write scan

The native secret scan must flag obvious hardcoded-secret patterns while
redacting secret values from evidence. It should record only bounded context and
a stable fingerprint, not raw credentials.

The native file-permission and sensitive-write scan has two jobs:

- inspect relevant local files and generated loopcoder state that the command is
  allowed to read, reporting sensitive files that are group/world readable or
  otherwise overly permissive; and
- scan source for obvious sensitive-write patterns, such as prompt, schema,
  summary, log, recovery, token, or key material written with modes broader than
  `0o600`.

The source scan is intentionally small and conservative. It is not a general Go
taint analyzer; it is a deterministic floor for the class of shared-host
disclosure issues 0.5.1 fixed and must prefer clear findings over noisy guesses.

SAST command execution rules:

- Commands run from the repository root using argv arrays, not shell strings.
- Command output is bounded. Oversized output is a command/runtime failure unless
  a tool-specific parser can safely truncate while preserving complete findings.
- A SAST tool may use its own normal cache locations. It must not modify tracked
  repository files. If the worktree changes during audit, the audit exits as a
  command/runtime failure and reports the changed paths locally.
- A parseable non-zero SAST exit that represents findings is not a command
  failure. Missing tools, bad flags, unparseable output, process launch failure,
  timeout, or invalid config are command/runtime failures.
- Layer 1 must not call an LLM and must not use the network except for normal
  vulnerability-database access by tools such as `govulncheck`.
- Findings are sorted deterministically by severity rank, file, line, rule, and
  evidence fingerprint. Timestamps must not affect finding identity.

The CI-required audit check for loopcoder must use this deterministic layer. It
may run only Layer 1 so that the required check is stable and does not depend on
LLM credentials, model availability, or nondeterministic review output.

## Layer 2 - LLM security-review lens

Layer 2 is an adversarial design-level security review. It exists to catch
trust-boundary, architecture, and process issues that deterministic SAST cannot
reliably find.

Layer 2 must:

- reuse `agent.Runner` and `agent.Invocation`;
- set `Invocation.ReadOnly = true`;
- use the existing verifier provider configuration unless the command receives
  an explicit one-off provider/model/effort override already supported by the
  command surface;
- provide the built-in threat model and any configured rubric file in the prompt;
- produce findings in the same schema as Layer 1;
- preserve the closed audit verdict set: `clean`, `findings`, or `needs-human`;
- degrade to `needs-human` on provider timeout, provider infrastructure error,
  unreadable configured rubric, malformed JSON, schema violation, missing
  attestation, or relay-write failure;
- emit and validate attestation exactly like a read-only verifier invocation:
  role `verifier`, permission `read-only`, provider, model, effort, action,
  duration, usage, and `verified: true`;
- keep the pretty attestation block and any relay records local-only.

Layer 2 may read repository files and Git metadata needed to form the review
packet, subject to existing packet bounds. It must not write tracked files,
comments, commits, PR bodies, issue bodies, or merge artifacts. If configured
MCP servers are available, only servers locally classified as read-only may be
offered to the read-only invocation, following the existing 0.5.0 MCP rules.

The command should support selecting layers, for example `sast`, `llm`, or
`all`. The exact flag spelling is a code-slice decision, but C3's CI check must
call the deterministic SAST floor explicitly. A full local audit can run both
layers and use the aggregate verdict rules below.

## Findings schema

`loopcoder audit` emits one normalized result object. Text output is rendered
from that object; JSON output uses the same object.

Required top-level fields:

```json
{
  "schema_version": 1,
  "repo": ".",
  "layers": ["sast", "llm"],
  "threshold": "medium",
  "verdict": "findings",
  "findings": [],
  "tool_results": [],
  "needs_human": []
}
```

The `verdict` enum is:

- `clean`: the audit completed, no unwaived finding is at or above the configured
  severity threshold, and no layer requires human judgment;
- `findings`: the audit completed and at least one unwaived finding is at or
  above the configured threshold;
- `needs-human`: the audit command completed its own orchestration, but a layer
  could not produce reliable evidence or a required human judgment remains.

Each finding must use this schema:

```json
{
  "id": "audit-native-file-permission:internal/worker/worker.go:123",
  "layer": "sast",
  "tool": "native",
  "severity": "medium",
  "file": "internal/worker/worker.go",
  "line": 123,
  "rule": "native:file-permission",
  "category": "shared-host-disclosure",
  "message": "Sensitive prompt material is written with permissions broader than 0600.",
  "evidence": "os.WriteFile(..., 0o644) near prompt.txt",
  "fingerprint": "sha256:...",
  "waived": false
}
```

Required finding fields:

- `id`: stable within a repo for the same finding class and location;
- `layer`: closed enum `sast` or `llm`;
- `severity`: closed enum `critical`, `high`, `medium`, `low`, or `info`;
- `file`: repo-relative path when a path exists, otherwise an empty string;
- `rule`: tool rule ID or native rule ID;
- `category`: broad security category;
- `message`: concise human-readable explanation;
- `evidence`: bounded supporting detail.

Optional finding fields:

- `tool`: command or native scanner name;
- `line`: 1-based line number when known;
- `column`: 1-based column number when known;
- `fingerprint`: stable hash over layer, rule, path, location, and normalized
  evidence;
- `waived`: whether an explicit baseline/waiver suppressed the finding from the
  threshold gate;
- `waiver_id`: the matched waiver when `waived` is true.

Evidence rules:

- Evidence must be bounded and locally rendered.
- Evidence must not include raw secret values. Secret findings use redacted
  snippets and fingerprints.
- Findings must not include local-only attestation JSON or pretty blocks.
- Findings must not be copied by the tool into repo-visible artifacts.

`tool_results[]` records command-level facts such as tool ID, argv, parser,
duration, exit status, output truncation, and parse status. It must not replace
the normalized `findings[]` list.

`needs_human[]` records layer-level ambiguity, for example an LLM timeout,
unreadable rubric file, expired waiver, or parser output that is syntactically
valid but semantically incomplete.

## Exit codes

Audit exit codes mirror the H5 discipline: clean audit verdict codes are
distinct from command/runtime failures.

- `0`: audit verdict `clean`.
- `1`: audit verdict `findings`; at least one unwaived finding is at or above
  the configured severity threshold.
- `2`: audit verdict `needs-human`; the command orchestrated the audit, but the
  result is not reliable enough to pass or fail automatically.
- `3`: command/runtime failure; examples include bad flags, unreadable repo,
  invalid config, missing required SAST tool, process launch failure, SAST tool
  timeout, unparseable required SAST output, output write failure, dirtying the
  worktree, or internal error.

If more than one condition applies, precedence is:

1. command/runtime failure (`3`);
2. needs-human (`2`);
3. threshold findings (`1`);
4. clean (`0`).

The existing relay hard gate may still refuse command execution with its
reserved code before an audit starts. That is not an audit verdict and must not
be overloaded as one.

The default severity threshold is `medium`. Findings below the threshold are
still printed and included in JSON, but they do not produce exit code `1`.
Configured thresholds use the same severity enum. A lower threshold such as
`low` makes more findings gate CI; a higher threshold such as `high` makes only
high and critical findings gate CI.

## Configuration

0.5.3 adds an optional additive `.delivery.yml` surface under `audit`. The
fields are snake_case, `omitempty`-safe, `Default()`-safe, and
unknown-field-tolerant on read, preserving 0161 M3.

Example:

```yaml
audit:
  severity_threshold: medium
  sast:
    commands:
      - id: govulncheck
        argv: ["govulncheck", "-json", "./..."]
        parser: govulncheck-json
        timeout_seconds: 300
      - id: staticcheck
        argv: ["staticcheck", "-f", "json", "./..."]
        parser: staticcheck-json
        timeout_seconds: 300
      - id: gosec
        argv: ["gosec", "-fmt", "json", "-quiet", "./..."]
        parser: gosec-json
        timeout_seconds: 300
    native:
      secrets: true
      file_permissions: true
      include:
        - "**/*"
      exclude:
        - ".git/**"
        - "vendor/**"
        - "dist/**"
  review:
    rubric_path: docs/security/audit-rubric.md
  baseline:
    path: docs/security/audit-baseline.yml
```

Normative config rules:

- If `audit` is absent and the repo has `go.mod`, Layer 1 uses the Go defaults
  listed above plus native scans.
- If `audit.sast.commands` is present, it replaces the language default command
  set. Native scans still run unless explicitly disabled.
- Non-Go repositories should configure `audit.sast.commands` for their own
  deterministic tools. If no language defaults apply and no commands are
  configured, only native scans run and `doctor` reports that no language SAST
  tools are configured.
- `argv` is required for configured SAST commands. The audit config does not add
  a shell-command string form.
- `argv` must be a non-empty array of non-empty strings. `argv[0]` is the
  executable.
- `parser` is required for configured commands unless the command is declared as
  a generic line-oriented tool supported by the implementation. The initial
  required parsers are `govulncheck-json`, `staticcheck-json`, and `gosec-json`.
- `timeout_seconds` defaults to a bounded command timeout. A timeout is a
  command/runtime failure unless a parser has already produced a complete,
  trustworthy result.
- `audit.review.rubric_path`, when present, must be repo-relative and readable.
  Missing or unreadable rubric files make Layer 2 return `needs-human`.
- `audit.baseline.path`, when present, points to explicit waivers for known
  findings. Missing, expired, malformed, or mismatched waivers do not silently
  suppress findings; they are reported in `needs_human[]` or as active findings.

The config surface does not change any existing command's behavior when absent.
It also does not weaken existing `ci.checks`, verifier settings, domain profile
settings, MCP rules, or guardrails.

## Baselines and pre-existing findings

The 0.5.3 CI integration must make the audit gate meaningful on loopcoder's own
current tree. If self-audit finds pre-existing issues, C3 must handle them in one
of two ways:

1. Fix the issue in C3 when the fix is small, behavior-safe, and directly needed
   for the audit check to pass.
2. Add an explicit baseline/waiver with a recorded justification when the fix is
   not safe to bundle into C3.

Waivers are not silent passes. A waived finding remains in output with
`waived: true`, is counted separately, and is excluded from the threshold gate
only when the waiver matches the finding fingerprint and is valid.

Each waiver must record:

- a stable waiver ID;
- matching rule and file or path glob;
- the expected finding fingerprint or enough normalized evidence to prevent
  broad suppression;
- original severity;
- justification;
- date added;
- review-by or expiry date.

Critical findings should not be waived except behind an explicit
`needs-human` result or a narrowly justified temporary waiver. Expired waivers
are active findings again. A waiver that no longer matches any finding is stale
and should be reported by `doctor` or audit text output so the repo can clean it
up.

The issue body names one known possible self-finding: worker-layer prompt or
recovery material written with `0o644`. If that or a similar finding appears
when C3 wires the self-audit check, C3 must either fix it or baseline it with
the rules above.

## Command and output behavior

The command entrypoint is `loopcoder audit`. The exact flag spelling is left to
the code slice, but the command must support:

- selecting repo path, defaulting to the current directory;
- selecting output format, at least human text and JSON;
- selecting layers so CI can run only deterministic SAST and local operators can
  run the full SAST + LLM review;
- overriding the severity threshold for one run without editing config;
- using the existing config-from-base behavior if that is needed for parity with
  other loopcoder commands.

Human text output must be program-rendered from the structured result. JSON
output must be deterministic enough for CI and tests. The command must not write
tracked files, comments, PR bodies, issue bodies, merge artifacts, or commits.

When Layer 2 runs, its pretty attestation block is emitted to stderr by default
using the same local-only convention as `loopreview`. The stable attestation
header, canonical JSON, result JSON, and relay ledger remain local machine
contracts and must not be copied into repository-visible artifacts.

## CI integration

C3 wires `audit` into loopcoder's CI as a required check and adds `audit` to
`.delivery.yml ci.checks`.

CI requirements:

- The check name is stable: `audit`.
- The check runs the deterministic SAST floor explicitly.
- The check uses the just-built or source-run loopcoder command in a way
  consistent with the existing CI workflow.
- The check installs or otherwise makes available required SAST tools for the
  configured Go default set.
- The check fails on exit code `1`, `2`, or `3`.
- The check output remains local to CI logs/artifacts and is not committed back
  to the repo.
- Any self-findings discovered while adding the check are fixed or explicitly
  baselined as described above.

The LLM lens is not a required hosted CI dependency in 0.5.3 unless a repository
explicitly opts into running it in CI with provider credentials. The required
loopcoder self-check must remain deterministic.

Promotion red-lines based on audit status are deferred. `tick`, `risk-gate`, and
`promote` behavior do not change in 0.5.3.

## Doctor integration

`loopcoder doctor` must report audit readiness locally. It should check:

- whether the audit config parses;
- the effective severity threshold;
- which SAST commands will run;
- whether required tools are available on `PATH`;
- whether configured parsers are recognized;
- whether the configured rubric path exists;
- whether baseline files parse and contain no expired/stale broad waivers;
- whether the CI-required `audit` check is present for loopcoder's own repo;
- whether Layer 2 can resolve a read-only verifier provider when requested.

Doctor diagnostics are advisory unless they reflect invalid config that would
make `loopcoder audit` fail. Doctor must not run the LLM review itself.

## Preserved invariants

- 0161 E1 is preserved: `agent.Runner` remains the provider door,
  `agent.Invocation` remains the provider-neutral request, and
  `Invocation.ReadOnly` remains the permission boundary.
- 0161 M2 self-hosting guard is preserved. Every C-slice changes loopcoder core
  or CI and routes `needs-human` when loopcoder works on itself; it takes effect
  only after human merge, rebuild, and tick restart.
- 0161 M3 is preserved. The audit config is additive, optional, snake_case,
  default-safe, and unknown-field-tolerant.
- F1-F5 are preserved: tick does not merge production, verifier remains
  read-only, promotion is distinct, guardrails continue to gate dispatch waves,
  and all LLM roles still attest.
- The 0.4.2 H5 discipline is mirrored: clean audit verdict exit codes are
  distinct from command/runtime failures.
- 0.5.1 hardening must not regress. Audit implementation must preserve release
  signing, restrictive sensitive file modes, path confinement, no-shell argv
  command forms, honest failure reporting, hook/runstatus bounds, and bounded
  worktree liveness.
- 0.5.2 refactor behavior-preservation must not be bypassed. Audit code should
  reuse centralized defaults and helpers when those exist instead of
  reintroducing scattered constants.
- Local-only attestation remains local-only. Audit must not copy worker,
  verifier, conductor, or audit-review attestation data into repository-visible
  artifacts.
- Relay obligations apply to Layer 2 exactly as they apply to loopreview's
  read-only verifier invocation.

## Follow-up code slices

The wave plan is serial:

1. Doc first: merge this spec.
2. C1: audit command plus deterministic SAST runner.
3. C2: LLM security-review lens.
4. C3: CI, doctor, and docs integration.
5. Checkpoint: C1-C3 merged, tag `v0.5.3`, verify the real artifact by running
   `loopcoder audit` as a self-audit.

C1, C2, and C3 are serial because they share the new `internal/audit` package
types and build on each other.

### C1 - audit command and deterministic SAST runner

Owned files: new `internal/audit/*`, new `internal/cli/audit.go`, and
`internal/cli/cli.go` for command registration.

Acceptance:

- `loopcoder audit` is registered and supports at least deterministic SAST
  execution, text output, JSON output, repo selection, and threshold selection.
- Optional `audit` config parses from `.delivery.yml` with the schema above.
- Absent audit config in a Go repo produces the Go defaults:
  `govulncheck`, `staticcheck`, `gosec`, native secret scan, and native
  file-permission/sensitive-write scan.
- Configured SAST commands use argv arrays and no shell.
- Known Go tool outputs are parsed into the normalized finding schema.
- Parseable tool findings produce audit verdicts, not command failures.
- Missing tools, bad config, unparseable required output, timeouts, dirtying the
  worktree, or command launch failures use command/runtime failure exit code
  `3`.
- Severity thresholding implements the exit-code map in this spec.
- Findings are sorted deterministically and redact raw secrets.
- The command does not write tracked files, issue comments, PR comments,
  commits, or merge artifacts.
- Tests cover config parsing, default Go command resolution, threshold behavior,
  exit-code precedence, parser adapters, native scans, redaction, worktree
  mutation detection, and deterministic JSON/text rendering.

### C2 - LLM security-review lens

Owned files: new `internal/audit/review.go` plus focused audit review tests and
any minimal rubric/config wiring needed to connect with C1's command surface.

Acceptance:

- `loopcoder audit` can run Layer 2 by explicit layer selection.
- Layer 2 builds a bounded review packet with the built-in threat model and any
  configured rubric file.
- The invocation uses `agent.Runner` with `Invocation.ReadOnly = true`.
- Only read-only MCP servers may be passed to the invocation.
- The provider must return structured findings in the audit schema.
- Provider timeout, infrastructure failure, unreadable rubric, malformed JSON,
  schema violation, missing attestation, or relay-write failure produces
  `needs-human`, not `clean`.
- The Layer 2 attestation uses verifier semantics with permission `read-only`,
  validates successfully, emits the pretty block locally, and records relay
  state consistently with loopreview.
- Local-only attestation does not enter repo-visible artifacts.
- Tests use fake agent runners to prove prompt/rubric inclusion, read-only
  invocation, finding parsing, malformed-output handling, needs-human fallback,
  attestation validation, and relay/local-only behavior.

### C3 - CI, doctor, and docs integration

Owned files: `.github/workflows/ci.yml`, `.delivery.yml`,
`internal/doctor/doctor.go`, and focused documentation under `docs/*`.

Acceptance:

- CI has a stable required `audit` check.
- `.delivery.yml ci.checks` includes `audit`.
- The CI check runs the deterministic SAST floor and is green on the current
  loopcoder tree.
- Required Go SAST tools are installed or otherwise made available in CI.
- Any audit finding raised against loopcoder itself is fixed when small and
  behavior-safe, or explicitly baselined/waived with a recorded justification,
  fingerprint, date, and review/expiry date.
- `loopcoder doctor` reports audit readiness, including missing tools, invalid
  config, missing rubric files, invalid/expired baseline entries, and whether
  the required `audit` check is configured.
- User docs describe `loopcoder audit`, the two layers, exit codes, config,
  thresholding, baselines/waivers, CI usage, and the local-only attestation
  behavior.
- Docs include an example security rubric that extends the built-in threat
  model without replacing it.
- No promotion red-line is added in C3.

## Relationship to existing specs

- [`0161-autonomous-delivery-loop.md`](0161-autonomous-delivery-loop.md) remains
  the parent for E1, M2, M3, and F1-F5. Audit adds a command and config surface
  without changing the delivery loop or permission boundary.
- [`0459-domain-profiles.md`](0459-domain-profiles.md) introduced configurable
  rubrics and domain plug points. Audit dogfoods those ideas through its
  configurable SAST set and security rubric.
- [`0484-security-robustness-hardening.md`](0484-security-robustness-hardening.md)
  is the 0.5.1 hardening foundation. Audit institutionalizes finding that class
  of issue in future work.
- [`0507-core-refactor.md`](0507-core-refactor.md) is the 0.5.2 foundation.
  Audit must build on the refactored core without reintroducing drift or
  behavior changes outside its own command.
- [`0194-reliable-loopreview-verifier.md`](0194-reliable-loopreview-verifier.md)
  provides the reliable read-only verifier pattern. Layer 2 reuses that pattern
  for security review instead of inventing a second provider path.
- [`0423-operational-reliability-hardening.md`](0423-operational-reliability-hardening.md)
  established the H5 exit-code discipline. Audit mirrors that split for audit
  verdicts versus command failures.
- [`0146-attestation.md`](0146-attestation.md),
  [`0306-local-only-attestation.md`](0306-local-only-attestation.md), and
  [`0447-relay-enforcement-hardgate.md`](0447-relay-enforcement-hardgate.md)
  remain in force for the Layer 2 LLM review.

## Non-goals

- No Go implementation, workflow change, `.delivery.yml` change, docs update
  outside this spec, command behavior change, or dependency addition in this
  design-doc PR.
- No auto-fix mode. Audit reports findings only.
- No promotion red-line, risk-gate rule, or `promote` behavior change in 0.5.3.
- No new provider abstraction, provider role, or LLM runner.
- No weakening of the self-hosting guard, read-only verifier boundary,
  guardrails, relay obligations, local-only attestation, H5-style exit-code
  split, 0.5.1 hardening, or 0.5.2 behavior-preservation line.
- No claim that deterministic SAST proves a repository secure. The SAST floor is
  a minimum gate; Layer 2 and human review still matter for design-level risk.
