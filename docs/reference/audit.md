# loopcoder Audit

`loopcoder audit` is a read-only repository security audit. It reports findings
locally and exits with deterministic verdict codes; it does not write comments,
commits, PR bodies, issues, merge artifacts, or tracked files.

## Layers

The command has two layers:

- Layer 1, `sast`: deterministic static analysis plus native loopcoder scans.
  In Go repositories the default command set is `govulncheck -json ./...`,
  `staticcheck -f json ./...`, `gosec -fmt json -quiet ./...`, native secret
  scanning, and native sensitive-file permission scanning.
- Layer 2, `llm`: an adversarial read-only security-review lens. It reuses the
  configured verifier provider, passes the built-in threat model plus any
  configured rubric, requires structured findings, and emits a verifier report
  locally.

Run only the deterministic floor:

```text
loopcoder audit --repo . --layer sast
```

Run both layers locally:

```text
loopcoder audit --repo . --layer all --provider claude --pretty
```

## Exit Codes

- `0`: verdict `clean`.
- `1`: verdict `findings`; at least one unwaived finding is at or above the
  severity threshold.
- `2`: verdict `needs-human`; the command ran, but a layer or waiver needs
  human judgment.
- `3`: command or runtime failure, such as invalid config, missing tools,
  unreadable output, timeout, or tracked worktree mutation.
- `4`: relay hard gate before audit starts; run `loopcoder relay flush --repo .`.

Failure precedence is command failure, then `needs-human`, then threshold
findings, then clean.

## Configuration

The optional `.delivery.yml` surface is additive:

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

Configured SAST commands use argv arrays, not shell strings. Supported parsers
are `govulncheck-json`, `staticcheck-json`, and `gosec-json`.

## Thresholds

`audit.severity_threshold` defaults to `medium`. Findings below the threshold
are still printed and included in JSON, but they do not cause exit code `1`.
Use `--severity-threshold` or `--threshold` for a one-run override.

## Baselines

Baselines are explicit waivers for known findings. A valid waiver records an
`id`, `rule` or `matching_rule`, `path` or `path_glob`, `original_severity`,
`justification`, `date_added`, and `review_by` or `expires`. It must include
either `fingerprint` or `normalized_evidence`.

Expired or malformed waivers produce `needs-human`. A valid waiver that no
longer matches a current finding is reported as stale but does not gate the
audit verdict. Critical findings should not be waived except as a narrow,
temporary human-reviewed exception.

## CI

The loopcoder repository has a required `audit` workflow job. It builds the
current source tree, installs `govulncheck`, `staticcheck`, and `gosec`, then
runs:

```text
loopcoder audit --repo . --layer sast
```

Layer 2 is not required in hosted CI because it depends on provider credentials
and nondeterministic model output.

## Reporter

Layer 2 uses verifier semantics: role `verifier`, permission `read-only`, and a
validated local-only report. Default text mode is for people and emits the
concise receipt only when the LLM layer produces a report; `--format json` is
for machines and emits one audit result JSON value with no receipt or reporter
header; `--verbose` is for local debugging and keeps raw audit details in text
mode. Pretty reports, canonical JSON, relay records, and audit logs are local
machine surfaces only. Do not copy them into PR bodies, issue or PR comments,
commits, merge comments, docs, examples, fixtures, or other tracked artifacts.
