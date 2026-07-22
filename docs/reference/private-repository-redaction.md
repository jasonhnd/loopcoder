# Private-repository redaction and consumer canary (V090-067)

Package: [`internal/privacy`](../../internal/privacy)  
Issue: [#1179](https://github.com/jasonhnd/loopcoder/issues/1179)

## Purpose

Qualify direct/provider paths for private repositories and prove that global
state, logs, host reports, diagnostics, PR metadata, and release evidence do not
leak private issue/code/prompt/path/account content.

This package is **policy + pure fixtures only**. It does not claim
encryption-at-rest, does not manage credentials, and does not run against real
private content in CI. A bounded owner-controlled real private canary may run
only at the release gate.

## Data classes

| Class | Meaning | Global / host / CI / release | Owning project events/logs |
| --- | --- | --- | --- |
| `public_identity` | project_id, short name, owner, provider family | allowed | allowed |
| `project_private_metadata` | issue numbers, status codes, attempt ids | not on global/host/CI | allowed |
| `code_prompt_output` | issue body, code, prompts, outputs, full paths | never | policy-bounded only |
| `credentials` | tokens, API keys, keychain material | **never** | **never** |
| `quota_account` | account identifiers | never (aggregates only) | bounded |
| `diagnostics` | support bundles, manifests | redacted to public identity | redacted |

Unknown destinations fail closed.

## Destinations under conformance

- `machine_global_db`, `global_status`, `machine_summary`
- `unrelated_project`
- `host_diagnostics`, `ci_artifact`, `release_manifest`
- `pr_body`, `error_path`, `json_human_output`
- `project_events`, `project_logs` (owning project only)

## Redaction

`RedactFor(dest, text)`:

1. Runs credential/path sanitization (`internal/sanitize`).
2. Replaces every synthetic private marker with a class-level redaction token.
3. On global/host/CI/release surfaces, collapses residual absolute paths.

Machine summaries expose only `PublicProjectFact` (id, short name, owner,
path basename). Full paths, issue text, prompts, outputs, and credentials are
never present.

## Synthetic markers

Canary fixtures use fixed `SYN_PRIVATE_*_v090067_*` markers. Automated scans
report **location + label only** and never echo the marker value in findings.

## GitHub least privilege and fail-closed access

Default permissions for ordinary private-repo paths:

- `metadata:read`, `contents:read`, `pull_requests:write`, `issues:write`, `checks:read`

Forbidden by default: `admin`, `secrets`, `actions:write`.

`EvaluateRepoAccess` fails closed when:

- repository owner/name missing
- visibility is `unknown` / empty
- repository is not authorized for the project
- requested permissions include forbidden grants

`WrongRepoAccess` fails closed when the requested owner/name does not match the
project registration. Both checks run **before provider launch**.

## Consumer canary

```bash
go test ./internal/privacy/ -run TestConsumerCanaryPasses -count=1
```

`RunConsumerCanary` / `ReportCanary`:

1. Builds a contaminated surface bundle containing every synthetic marker.
2. Redacts the bundle for each destination.
3. Scans machine-global DB, global status, unrelated project, host diagnostics,
   CI artifact, release manifest, PR body, error paths, machine summary, and
   project events/logs — asserting zero marker residue.
4. Asserts GitHub unknown visibility, unauthorized repo, admin permission, and
   wrong-repo paths fail closed; least-privilege private access is allowed.
5. Asserts credentials are never allowed on any destination.

## Privacy limits (honest)

- No encryption-at-rest claim.
- No credential manager / keychain integration in this package.
- No real private repository content in PR CI.
- Real private canaries are owner-only at the release gate.
- PR CI executes PR-branch code; policy strength depends on branch protection
  and CODEOWNERS as with other ordinary-dev gates.

## Verification

```bash
go test ./internal/privacy/
```
