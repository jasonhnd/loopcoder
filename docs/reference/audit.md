# Built-In Audit

`loopcoder audit` is a read-only repository security audit. It has two layers:

- `sast`: deterministic static analysis and native local scans. This layer is
  suitable for CI.
- `llm`: an adversarial read-only security review through the configured
  verifier provider. This layer is local operator review, not a required hosted
  CI dependency by default.

The default layer is `sast`:

```text
loopcoder audit --repo .
loopcoder audit --repo . --layer sast --format json
loopcoder audit --repo . --layers all --pretty
```

## Exit Codes

- `0`: clean audit verdict.
- `1`: findings at or above the configured threshold.
- `2`: `needs-human`; the command ran but a layer could not produce reliable
  evidence.
- `3`: command/runtime failure, such as bad flags, invalid config, missing SAST
  tools, unparseable required output, timeout, or worktree mutation.
- `4`: pending local relay block before audit starts; run
  `loopcoder relay flush --repo <path>`.

## Configuration

The optional `.delivery.yml audit` section is additive. If it is absent in a Go
repo, Layer 1 uses the built-in Go defaults:

- `govulncheck -json ./...`
- `staticcheck -f json ./...`
- `gosec -fmt json -quiet ./...`
- native secret-pattern scan
- native sensitive-file and sensitive-write scan

Configured `audit.sast.commands` replace the language default command set.
Commands are argv arrays, not shell strings. Native scans still run unless
disabled:

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
  review:
    rubric_path: docs/security/audit-rubric.md
  baseline:
    path: docs/security/audit-baseline.yml
```

The threshold enum is `critical`, `high`, `medium`, `low`, or `info`.
Findings below the threshold are still printed and included in JSON, but do not
produce exit code `1`. Use `--severity-threshold <level>` for a one-run
override.

## Baselines And Waivers

Prefer fixing real self-findings when the fix is small and behavior-safe. Use a
baseline only for a known finding that should remain visible but not gate the
threshold temporarily. A waiver should be narrow and include:

- stable `id`;
- `rule`;
- `file`, `path`, or `path_glob`;
- expected `fingerprint`;
- `original_severity`;
- `justification`;
- `date_added`;
- `review_by` or `expires_at`.

Expired, malformed, stale, or overly broad waivers should not silently suppress
findings. `loopcoder doctor` reads the configured baseline path and reports
invalid, expired, or broad entries; `loopcoder audit` marks exact matches as
`waived: true` and moves stale or expired waivers to `needs-human`. loopcoder's
own current baseline keeps residual self-audit findings visible after the 0.5.3
worker prompt/recovery file-mode fix.

## CI Usage

Hosted CI should run only the deterministic floor unless a repository
explicitly opts into provider credentials for Layer 2:

```text
loopcoder audit --repo . --layer sast --format text
```

loopcoder's own CI installs `govulncheck`, `staticcheck`, and `gosec`, builds
the current source, and runs that command as the stable `audit` job. The job is
listed in `.delivery.yml ci.checks` so the normal check-mapping guard treats it
as required.

## LLM Review And Attestation

Layer 2 uses the configured verifier provider through `agent.Runner` with a
read-only invocation. It includes the built-in threat model and any configured
rubric file. Provider timeouts, provider infrastructure failures, malformed
JSON, missing attestation, unreadable rubric files, or relay-write failures
produce `needs-human`.

Layer 2 verifier attestation is local-only, matching `loopreview`: pretty
blocks, relay records, result JSON, and gitignored `.loopcoder/` state are local
diagnostics. Audit does not write PR comments, issue comments, commits, merge
artifacts, docs, or tracked files with attestation data.

## Doctor

`loopcoder doctor --repo .` reports audit readiness locally:

- audit config parse and effective severity threshold;
- planned SAST commands and native scans;
- required SAST tools on `PATH`;
- recognized parser names;
- configured rubric path readability;
- baseline parse health and expired/broad waiver checks;
- required `audit` CI check presence;
- read-only verifier provider resolution for Layer 2.

`doctor` does not run the LLM review.
