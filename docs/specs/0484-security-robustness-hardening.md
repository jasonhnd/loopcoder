---
id: 484
title: Security and robustness hardening
status: draft
date: 2026-07-05
issue: 484
pr: null
supersedes: []
superseded_by: []
---

# Security and robustness hardening

This is a design-only spec for loopcoder **0.5.1**. This PR adds only this
document: no Go code, no `.delivery.yml` change, no command behavior change, and
no new dependency. Code slices are filed only AFTER this spec merges, per
[`docs/PROCESS.md`](../PROCESS.md).

0.5.1 fixes the verified findings from an external security audit of the
loopcoder codebase. loopcoder is a local, single-operator development CLI: the
operator already chooses the repository, configuration, tools, and shell in which
loopcoder runs. Under that model, most findings are Low to Medium severity
hardening rather than active-exploit fixes, but they are real issues worth
closing because loopcoder consumes untrusted PR contents, release artifacts,
remote MCP servers, and local files on machines that may be shared.

The audit used two Critical labels that are overstated after inspecting the real
code. `scripts/install.sh` extracts the downloaded archive only after checksum
comparison, extracts into an isolated temporary directory, and copies only the
top-level binary into the install target. `statebranch` log paths are currently
loopcoder-authored run metadata and the pushed content is passed through
`recovery.Scrub`. Those facts lower the real severity; they do not remove the
need to add signature verification, tighter file modes, and path confinement.

## Threat model

Operator-trusted inputs:

- the operator's own `.delivery.yml`;
- the repository in which the operator intentionally runs loopcoder;
- local tools the operator has installed and configured;
- command strings explicitly authored by the operator.

These inputs are trusted because the operator already has a shell in that repo.
"Operator config drives a shell command" is therefore not command injection by
itself.

Untrusted inputs:

- PR and worktree file contents, including build scripts and generated files;
- downloaded release artifacts and checksum files;
- remote MCP servers and data returned by them;
- local users on a shared host who can read permissive temp files, process
  arguments, logs, or state records.

Every finding below is judged under this split. The real risks are
supply-chain integrity, shared-host disclosure, untrusted worktree effects, and
robust failure reporting, not treating the operator's config file as an attacker.

## Findings and fixes

### S1 - Supply-chain integrity

Severity: Medium.

Code anchors:

- `.github/workflows/release.yml:119-124` generates `SHA256SUMS` and contains an
  explicit `Signing TODO`.
- `scripts/install.sh:234-245` downloads `SHA256SUMS` and verifies the archive
  against the unsigned checksum before extracting.
- `scripts/install.ps1:194` downloads `SHA256SUMS`; the PowerShell installer
  trusts that unsigned checksum in the same way.
- `internal/upgrade/upgrade.go:244-258` downloads the release archive plus
  `SHA256SUMS`, then calls `VerifyChecksum` before replacement.
- `.github/workflows/ci.yml:108-125` runs Go build, vet, and tests but has no
  `govulncheck` or `staticcheck` job.
- `.github/workflows/ci.yml:17,111,113` and
  `.github/workflows/release.yml:42,44,97,108` use mutable action version tags
  such as `@v7`, `@v6`, and `@v8`.

Real risk:

- An attacker who can alter release assets can replace both an archive and an
  unsigned checksum file.
- Mutable GitHub Action tags can move after review.
- Missing SAST leaves known-vulnerability and static-analysis regressions to
  manual discovery.

Normative fix:

- Release must publish a signature for `SHA256SUMS` and a documented trust root
  or verifier identity. The implementation may use cosign or minisign, but it
  must verify the signature before trusting any checksum entry.
- `scripts/install.sh`, `scripts/install.ps1`, and `internal/upgrade/upgrade.go`
  must fail closed when the signature asset is missing, malformed, unverifiable,
  or signed by the wrong identity/key. Only after signature verification may
  checksum verification and extraction/replacement proceed.
- CI must add stable `govulncheck` and `staticcheck` checks.
- All GitHub Actions in CI and release workflows must be pinned to full commit
  SHAs, with Dependabot configured to keep those pins current.

### S2 - Shared-host disclosure

Severity: Low-Medium.

Code anchors:

- `internal/agent/codex.go:57` writes the output schema with `0o644`.
- `internal/agent/codex.go:62` writes the prompt with `0o644`.
- `internal/agent/codex.go:71` creates the provider log through `os.Create`,
  which uses the process default file mode.
- `internal/agent/gemini.go:29` passes the full prompt as `--prompt` argv,
  making it visible through process inspection on many systems.
- `internal/agent/gemini.go:56` creates the Gemini log through `os.Create`.
- `internal/agent/claude.go:75` creates the Claude log through `os.Create`.
- `internal/statebranch/statebranch.go:886` writes scrubbed log tails with
  `0o644`.

Real risk:

- On a shared host, another local user may observe prompts, output schemas,
  summaries, provider logs, or argv containing issue context and repo details.
- Codex already feeds the prompt to the CLI via stdin from a prompt file; the
  risk there is the prompt/schema/summary/log file mode, not argv exposure.

Normative fix:

- Provider prompts, schemas, summaries, settings, logs, and statebranch log
  tails that may contain user/repo context must be written `0o600`.
- Existing files should be created with restrictive permissions from the start;
  code must not rely on chmod after writing sensitive bytes.
- Gemini must stop placing the full prompt in argv. It must pass the prompt via
  stdin or via a `0o600` prompt file supported by the runner.
- Codex must keep the stdin prompt flow and tighten only the on-disk modes.

### S3 - Statebranch path confinement

Severity: Low-Medium.

Code anchors:

- `internal/statebranch/statebranch.go:863-906` writes scrubbed log artifacts
  into the state branch.
- `internal/statebranch/statebranch.go:908-935` discovers log sources by reading
  run metadata and raw log-like files.
- `internal/statebranch/statebranch.go:915-916` joins relative paths to the run
  directory but accepts absolute paths as-is.
- `internal/statebranch/statebranch.go:1235-1246` already contains a safe
  root-relative removal guard pattern that rejects paths outside a root.

Real risk:

- Today the paths come from loopcoder-authored local run records, and pushed
  content is scrubbed. That makes this defense-in-depth, not arbitrary
  exfiltration.
- The remaining risk is that malformed, stale, symlinked, or locally tampered
  run metadata could cause `statebranch` to read a path outside the intended run
  or scratch area and push a scrubbed tail plus metadata about it.

Normative fix:

- `discoverLogSources` must confine log source reads to the run directory and
  explicitly configured scratch roots.
- It must reject absolute paths that are not under an allowed root, paths whose
  clean relative form escapes with `..`, and symlink sources that resolve
  outside an allowed root.
- Rejection must be diagnosable in the statebranch result or manifest without
  leaking raw sensitive content.
- Regression tests must cover absolute paths, `..` escapes, symlink escapes,
  valid run-local logs, and valid configured scratch-root logs.

### S4 - Config-command hardening

Severity: Low.

Code anchors:

- `internal/config/config.go:185-194` currently models
  `domain.evidence.producer.command` as a scalar string.
- `internal/config/config.go:206-208` models `domain.liveness` with `mode` only.
- `internal/worker/worker.go:895-913` separately re-parses
  `domain.liveness.command` from YAML instead of using the config struct.
- `internal/loopreview/loopreview.go:1119-1138` loads the evidence producer from
  operator config and runs it in the PR worktree.
- `internal/loopreview/loopreview.go:1542-1576` runs the evidence producer by
  wrapping the configured command in `sh -c` or `cmd /c`.
- `internal/supervisedexec/supervisedexec.go:418-443` runs custom liveness
  through `sh -c` or `cmd /c` with `cmd.Dir` set to the worktree.

Real risk:

- The configured command string is operator-authored, so this is not
  command-string injection.
- The residual risk is that a trusted command runs with its working directory set
  to an untrusted PR worktree. Build files, scripts, and tools from that PR can
  execute as part of the selected command. That is inherent to building
  untrusted code, but loopcoder should avoid unnecessary shell interpretation and
  make the trust boundary clear.

Normative fix:

- Add the additive argv-array command form defined below for both evidence
  producer and custom liveness.
- The argv form must run directly with no shell, no shell expansion, and no
  command-string parsing.
- The existing scalar `command:` string must remain supported with its current
  shell semantics for backward compatibility.
- Evidence producer execution must move under the `supervisedexec` process-group
  machinery and a hard timeout, so child processes are reaped consistently with
  provider and liveness commands.
- Documentation and errors must state the trusted-command-in-untrusted-worktree
  caveat plainly.

### R1 - Robustness and honest failure reporting

Severity: Low-Medium.

Code anchors:

- `internal/vcs/github/github.go:983-991` treats empty `gh` JSON output as
  success and skips JSON parsing, returning the target's zero value.
- `internal/vcs/github/github.go:917-945` creates an issue, then swallows a
  follow-up `ViewIssue` failure by returning a synthesized issue and nil error.
- `internal/vcs/github/github.go:948-975` does the same after issue update.
- `internal/vcs/github/github.go:263-294` lists issues and PRs with a fixed
  `--limit 1000`, which can silently truncate.
- `internal/agent/codex.go:86` ignores the log-read error before parsing model
  and token usage.
- `internal/cli/hook.go:48-58` reads hook stdin with unbounded `io.ReadAll`.
- `internal/runstatus/runstatus.go:321-349` scans event lines with a fixed
  scanner buffer, and `internal/runstatus/runstatus.go:352-375` walks arbitrary
  run JSON files and reads each whole file.

Real risk:

- Empty or truncated GitHub data can look like a valid empty result and drive
  wrong scheduling or reporting.
- A successful mutation followed by a failed readback can be reported as fully
  verified when it is only partially known.
- Missing Codex usage can become a misleading zero-token or absent-token
  attestation without distinguishing command failure from parse failure.
- Unbounded hook and status reads can consume memory or hang status rendering on
  corrupt local state.

Normative fix:

- `runJSON` must return an error when JSON is expected and stdout is empty,
  unless a call site explicitly opts into allowing empty output.
- `CreateIssue` and `UpdateIssue` must return the partial issue plus an error
  when the mutation succeeds but the follow-up view fails.
- Issue and PR list operations must paginate all results or report clear
  truncation when the current `--limit` is hit.
- Codex log-read errors must be surfaced. Metadata parse failure must be
  distinguishable from provider exec failure so attestation cannot silently imply
  zero usage.
- Hook stdin must be capped with `io.LimitedReader` or equivalent.
- Runstatus must bound files by known names, size, depth, and mtime, and report
  diagnosable oversize/corrupt-state errors instead of reading arbitrary local
  files wholesale.

### P1 - Local status and liveness performance

Severity: Low.

Code anchors:

- `internal/supervisedexec/supervisedexec.go:349-365` observes worktree mtime
  with `filepath.WalkDir`; it skips `.git` but still may traverse large or
  ignored trees.
- `internal/runstatus/runstatus.go:352-375` scans all run JSON-like files under
  the run path.

Real risk:

- Large repositories or accumulated local run state can make liveness and status
  checks expensive enough to become their own source of stalls.

Normative fix:

- Worktree liveness must skip `.git` and ignored/generated directories, stop as
  soon as a newer mtime is found, and cap the number of files examined.
- If the cap is exceeded without finding progress, liveness must fall back in a
  documented, diagnosable way instead of walking forever.
- Runstatus must scan known filenames and bounded directories only, applying the
  same mtime/size/depth constraints required by R1.

## Additive command schema

0.5.1 adds exactly one config-schema capability: an argv-array command form for
the 0.5.0 domain evidence producer and custom liveness plug points. This extends
[`0459-domain-profiles.md`](0459-domain-profiles.md) plug point 3 and its H2
liveness fold-in.

Schema shape:

```yaml
domain:
  evidence:
    producer:
      # Legacy shell form, preserved for backward compatibility.
      command: make render-ir-pdf

      # New no-shell form. Prefer this for new configs.
      argv: ["make", "render-ir-pdf"]

      outputs:
        - out/report.pdf
      timeout_seconds: 300
      include_in_loopreview: true

  liveness:
    mode: custom

    # Legacy shell form, preserved for backward compatibility.
    command: ./tools/liveness.sh --worktree .

    # New no-shell form. Prefer this for new configs.
    argv: ["./tools/liveness", "--worktree", "."]
```

Implementation structs may share an internal `CommandSpec`, but the public YAML
fields are:

```go
type DomainEvidenceProducer struct {
    Command             string   `yaml:"command,omitempty"`
    Argv                []string `yaml:"argv,omitempty"`
    Outputs             []string `yaml:"outputs,omitempty"`
    TimeoutSeconds      int      `yaml:"timeout_seconds,omitempty"`
    IncludeInLoopreview *bool    `yaml:"include_in_loopreview,omitempty"`
}

type DomainLiveness struct {
    Mode    string   `yaml:"mode,omitempty"`
    Command string   `yaml:"command,omitempty"`
    Argv    []string `yaml:"argv,omitempty"`
}
```

Validation and precedence:

- `command` is the legacy shell form. It runs through the platform shell exactly
  as today and is retained only for compatibility.
- `argv` is the new no-shell form. `argv[0]` is the executable and later
  elements are passed as arguments. There is no shell, glob expansion, pipe,
  variable expansion, or command chaining.
- For one command site, `command` and `argv` are mutually exclusive. Setting both
  is a configuration error; there is no precedence rule that silently chooses
  one.
- `argv` must be a non-empty array of non-empty strings. Empty elements are a
  configuration error.
- `domain.evidence.producer` is configured when either `command` or `argv` is
  present. If configured, it must still declare at least one output.
- `domain.liveness.mode: custom` requires exactly one of `command` or `argv`.
  Non-custom liveness ignores absent command fields and errors if command fields
  would otherwise create ambiguous behavior.
- Absent `argv` and absent new struct fields are `omitempty`-safe,
  `Default()`-safe, unknown-field-tolerant on read, and preserve today's
  behavior byte-for-byte.

This schema is additive under 0161 M3. It does not change the code profile:
repos with no `domain` section, no evidence producer, and no custom liveness
continue to behave exactly as they do before 0.5.1.

## Preserved invariants

- H5 loopreview verdict and exit-code contract is unchanged: clean verifier
  verdicts remain `pass=0`, `fail=1`, and `needs-human=2`, distinct from command
  failure.
- 0161 E1 is unchanged: `agent.Runner` remains the provider door,
  `agent.Invocation` remains the provider-neutral request, and
  `Invocation.ReadOnly` remains the one permission boundary.
- 0161 M2 self-hosting guard remains first. Every A1-A7 code slice changes
  loopcoder core or release mechanics, routes `needs-human` when loopcoder works
  on itself, and takes effect only after human merge, rebuild, and tick restart.
- 0161 M3 additive/byte-stable config rules apply to the argv schema.
- 0161 F1-F5 are unchanged: no tick production merge, verifier remains read-only,
  promotion is distinct, guardrails still gate dispatch waves, and all roles
  still attest.
- No behavior changes for the absent code profile. If the new config fields and
  release signature assets are absent before the relevant code slices land,
  current behavior remains unchanged.

## Follow-up code slices

The wave plan is: doc first, then Wave 1 parallel `{A1, A2, A3, A4, A5, A6}`,
then Wave 2 `{A7}` after A6 because both A6 and A7 touch
`.github/workflows/release.yml`.

### A1 - Agent hardening

Owned files: `internal/agent/codex.go`, `internal/agent/claude.go`,
`internal/agent/gemini.go`, and focused tests under `internal/agent/*_test.go`.

Acceptance:

- Prompt, schema, summary, settings, and provider log files that may contain
  invocation context are created `0o600`.
- Codex continues to pass the prompt through stdin and no longer leaves
  prompt/schema/summary files world-readable.
- Gemini no longer passes the prompt in argv; tests assert the full prompt is
  absent from `BuildGeminiArgs`.
- Codex log-read errors are returned or recorded as errors, not ignored.
- Attestation parsing distinguishes provider exec failure from metadata parse
  failure; missing usage cannot silently become a misleading zero-token success.

### A2 - Statebranch hardening

Owned files: `internal/statebranch/statebranch.go` and focused statebranch tests.

Acceptance:

- Scrubbed log tail artifacts are written `0o600`.
- `discoverLogSources` accepts only run-local files and files under configured
  scratch roots.
- Absolute paths, `..` escapes, and symlink escapes outside allowed roots are
  rejected with a diagnosable manifest/result entry.
- Regression tests cover accepted and rejected path cases.

### A3 - GitHub robustness

Owned files: `internal/vcs/github/github.go` and focused GitHub client tests.

Acceptance:

- Empty output where JSON is expected is an error unless a call site explicitly
  opts into `allowEmpty`.
- `CreateIssue` and `UpdateIssue` return partial objects plus errors when
  mutation succeeds but follow-up `ViewIssue` fails.
- Issue and PR list calls paginate all results or return an explicit truncation
  error when the current `--limit` is reached.
- Tests cover empty JSON, mutation-with-readback-failure, and pagination or
  truncation behavior.

### A4 - Hook and runstatus bounds

Owned files: `internal/cli/hook.go`, `internal/runstatus/runstatus.go`, and
focused tests.

Acceptance:

- Hook stdin reads are capped with `io.LimitedReader` or equivalent.
- Oversized hook payloads fail open or report according to the current hook
  contract without unbounded memory growth.
- Runstatus scans only known run record filenames and bounded directories.
- Runstatus enforces size, mtime, and depth limits with clear local diagnostics
  for skipped or oversized files.

### A5 - Config-command and supervisedexec hardening

Owned files: `internal/config/config.go`, `internal/loopreview/loopreview.go`,
`internal/supervisedexec/supervisedexec.go`, and focused tests.

Acceptance:

- `domain.evidence.producer.argv` and `domain.liveness.argv` are parsed through
  the typed config schema with the validation rules above.
- Existing scalar `command:` remains supported with shell semantics.
- The argv form executes directly with no shell for both evidence producer and
  custom liveness.
- Evidence producer execution uses `supervisedexec` process-group cleanup and a
  hard timeout.
- Worktree liveness skips `.git` and ignored/generated directories, early-exits
  on first newer mtime, and caps file count with diagnosable fallback.
- Tests prove legacy compatibility, no-shell argv execution, mutual-exclusion
  validation, process cleanup, and bounded worktree walking.

### A6 - Release signing and verification

Owned files: `.github/workflows/release.yml`, `scripts/install.sh`,
`scripts/install.ps1`, `internal/upgrade/upgrade.go`, and focused tests.

Acceptance:

- Release publishes a signature for `SHA256SUMS` using the selected signing
  mechanism and documented trust root/verifier identity.
- Shell installer, PowerShell installer, and `loopcoder upgrade` verify the
  signature before trusting the checksum.
- Missing, malformed, mismatched, or wrong-identity signatures fail closed before
  archive extraction or binary replacement.
- Existing checksum verification remains in place after signature verification.
- Tests or scripted fixtures cover valid signature, missing signature, bad
  signature, and checksum mismatch.

### A7 - CI SAST and action pinning

Owned files: `.github/workflows/ci.yml`, `.github/workflows/release.yml`,
`.github/dependabot.yml`, and focused workflow validation.

Needs: doc and A6.

Acceptance:

- CI exposes stable `govulncheck` and `staticcheck` checks.
- GitHub Actions in CI and release workflows are pinned to full commit SHAs.
- Dependabot is configured to update GitHub Actions pins and Go module updates
  on a controlled cadence.
- The A6 release signing flow remains intact after action pinning.
- Workflow validation proves the required job names remain present.

## Relationship to existing specs

- [`0161-autonomous-delivery-loop.md`](0161-autonomous-delivery-loop.md) is the
  parent for M1-M4, F1-F5, and E1. This spec preserves those invariants and
  treats all A1-A7 changes as loopcoder-core or release-mechanics changes under
  the self-hosting guard.
- [`0459-domain-profiles.md`](0459-domain-profiles.md) introduced
  `domain.evidence.producer` and `domain.liveness`. This spec adds the
  argv-array command form to those existing plug points without adding a new
  scheduler, worker type, or domain model.
- [`0423-operational-reliability-hardening.md`](0423-operational-reliability-hardening.md)
  established H5's clean-verdict exit-code contract and worktree liveness. This
  spec preserves H5 while bounding and hardening the liveness/status paths.
- [`0146-attestation.md`](0146-attestation.md),
  [`0306-local-only-attestation.md`](0306-local-only-attestation.md), and
  [`0447-relay-enforcement-hardgate.md`](0447-relay-enforcement-hardgate.md)
  remain unchanged. Hardening must not copy local-only attestation data into
  repository artifacts.

## Non-goals

- No Go code, workflow, installer, `.delivery.yml`, or behavior change in this
  design-doc PR.
- No implementation of `loopcoder audit`; that is the separate 0.5.3 unit and
  gets its own spec.
- No claim that building untrusted PR code is safe. 0.5.1 reduces unnecessary
  shell interpretation and improves failure bounds, but executing a build in an
  untrusted worktree still executes untrusted code selected by the operator's
  command.
- No weakening of the self-hosting guard, ReadOnly verifier boundary, red-line
  floor, relay obligations, H5 exit-code contract, or F1-F5.
- No redesign of dispatch, tick, loopreview, promote, relay, guardrails, or
  recovery.
