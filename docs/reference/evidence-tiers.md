# Evidence Tiers and Pre-Push Budget

This living reference defines who owns each class of verification evidence for
ordinary LoopCoder development on `darwin/arm64`. It implements the ownership
boundary described in the v0.9.0 ordinary-development roadmap and GitHub issue
`#1092` (`V090-002`).

Local machines must stay responsive. Remote CI, branch protection, and the
release workflow remain the durable authority for merge and release decisions.

## Evidence tiers

| Tier | Owner | What it proves | Typical commands / surfaces |
| --- | --- | --- | --- |
| `local-focused` | Developer machine, pre-push sentinel | Formatting, generated-file consistency, and a small deterministic smoke that fits the local budget | `bash scripts/pre-push-sentinel.sh`, optional focused package tests the developer chooses |
| `pull-request` | GitHub Actions PR CI | Repository policy, full unit suite, affected-package race, static analysis / security | Jobs `verify`, `test`, `race`, `security` in `.github/workflows/ci.yml` |
| `merge-sha` | Protected remote integration on the exact merged commit | The integrated SHA still satisfies required checks after merge | Branch protection required contexts on `main` / configured protected branch |
| `release-artifact` | Release workflow on a version tag | Full race, one `darwin/arm64` build, checksum/signature, exact-archive smoke | `.github/workflows/release.yml` jobs `build`, `sign`, `draft`, `smoke`, `publish` |
| `consumer-canary` | Explicit owner-gated release or post-release canary | Install, first run, migration, redaction, or cleanup on a disposable consumer host | Release canary scripts and GO/NO-GO records, never pre-push |

## Ownership table for heavy gates

Every heavy gate has exactly one authoritative remote stage. Pre-push must not
absorb a gate merely because remote capacity is temporarily unavailable.

| Gate | Authoritative remote stage | Artifact / evidence |
| --- | --- | --- |
| Full unit suite (`go test ./...`) | `pull-request` job `test` | Workflow run on the PR head SHA |
| Affected-package race | `pull-request` job `race` via `scripts/ci-race-changed.sh` | Workflow run on the PR head SHA |
| Full race suite | `release-artifact` job `build` via `scripts/ci-full-race.sh` | Tag build log for the release commit |
| Security / static analysis (`staticcheck`, `govulncheck`, `audit`) | `pull-request` job `security` | Workflow run on the PR head SHA |
| Package / archive build | `release-artifact` job `build` | `loopcoder_<version>_darwin_arm64.tar.gz` for the tagged SHA |
| Signing / checksum | `release-artifact` job `sign` | `SHA256SUMS` and Sigstore bundle for that archive digest |
| Exact-archive smoke | `release-artifact` job `smoke` | Smoke evidence tied to the same archive digest |
| Consumer install / first-run canary | `consumer-canary` | Owner-recorded canary evidence; optional and never a pre-push gate |

## Required-check discovery

Merge readiness for required checks is derived from repository policy, not from
hard-coded review-bot names.

1. Load `.delivery.yml` `ci.checks` as the required PR check names for this
   repository. Today that set is exactly `verify`, `test`, `race`, and
   `security`.
2. Treat only those names as merge-blocking required checks.
3. Treat known optional review bots (including `Greptile Review` and names that
   start with `Greptile`) as optional evidence unless an owner deliberately
   adds the exact name to `ci.checks` and branch protection.
4. An absent, pending, or failed optional bot never blocks merge readiness on
   its own.
5. A missing, failed, cancelled, timed-out, skipped, or still-pending required
   check keeps the PR unmergeable.

The pure helper lives in package `internal/evidence`. Callers that wait for
checks must use that policy-derived set rather than embedding bot names.

## Commit SHA and archive digest recording

Every automated check must identify what it exercised:

| Surface | Identity recorded |
| --- | --- |
| Local pre-push sentinel | Current `HEAD` commit SHA |
| PR CI jobs | `GITHUB_SHA` / tested commit SHA printed at the start of each job |
| Merge-SHA protection | The exact protected commit under evaluation |
| Release build / race | Tag commit SHA (`github.sha` / `COMMIT_SHA`) |
| Release signing and smoke | Archive SHA-256 digest from `SHA256SUMS` plus the tag commit SHA |

CI and release workflows print machine-readable lines:

```text
tested_commit_sha=<40-char-or-full-git-sha>
archive_digest_sha256=<sha256>   # release artifact stages only
```

## Local pre-push budget

Pre-push is the only automatic local gate. It must:

- complete within **60 seconds** on a reference Apple Silicon Mac for a no-op or
  documentation-only change;
- run formatting (`gofmt -l` on Go files changed versus the push base or working
  tree), whitespace/conflict checks (`git diff --check`), embedded playbook
  consistency, and the deterministic evidence-sentinel tests;
- never run `go test ./...`;
- never run a full race suite or `go test -race ./...`;
- never call a model provider, poll hosted runners, install packages, or run
  release smoke; and
- never start a long-running local daemon or watcher.

Install the repository hook path once per clone (preferred — overrides any
global full-repository pre-push hook):

```bash
git config core.hooksPath hooks
git config --get core.hooksPath   # must print: hooks
```

With `core.hooksPath=hooks`, Git executes `hooks/pre-push`, which only runs the
local-focused sentinel. Do **not** rely on a global hooks directory that runs
`go test ./...`.

Legacy per-clone install (only if you cannot set `core.hooksPath`):

```bash
ln -sf ../../hooks/pre-push .git/hooks/pre-push
# or copy: cp hooks/pre-push .git/hooks/pre-push && chmod +x .git/hooks/pre-push
```

Run the same checks without installing a hook:

```bash
bash scripts/pre-push-sentinel.sh
```

### Stabilization additions

- Implementation PRs that close or implement `status:planned` issues without
  explicit authorization fail the `verify` job
  (`scripts/check-implementation-authorization.sh`).
- Every push to `pre-prod` re-runs `verify` / `test` / `race` / `security` via
  `.github/workflows/pre-prod-integration.yml` (`evidence_tier=merge-sha`).
- Before the next feature item: `bash scripts/assert-pre-prod-green.sh`.
- See [`v090-stabilization-gate.md`](v090-stabilization-gate.md).

### What developers run locally

| Situation | Local command | Wait for remotely |
| --- | --- | --- |
| Before push | `bash scripts/pre-push-sentinel.sh` (via `core.hooksPath=hooks`) | Nothing yet |
| After opening a PR | Optional focused package tests only when debugging | Required PR jobs `verify`, `test`, `race`, `security` plus authorization |
| Before merge | Do not re-run repository-wide suites locally | Green required PR checks on the exact head SHA |
| After merge to `pre-prod` | Do not start the next feature yet | Green `pre-prod-integration` jobs on the exact merge SHA (`assert-pre-prod-green.sh`) |
| Before release | Do not run full race or packaging locally as a substitute | Release workflow evidence for the tag SHA and archive digest |

If remote CI is red or unavailable, keep the PR unmergeable. Do **not** move
heavy gates back into pre-push.

## Optional review bots

Greptile Review and similar bots may add useful comments. They are optional
evidence:

- their absence does not block merge readiness;
- a red optional bot comment is human signal, not a required context unless
  repository protection and `.delivery.yml` explicitly list that exact check
  name; and
- automation must not hard-code waits for optional bot names.

## Non-goals

- Changing product runtime resource admission.
- Weakening required GitHub or release checks.
- Making Greptile Review required.
- Implementing release packaging or consumer canaries in this document.

## Related documents

- [`releasing.md`](releasing.md): release workflow and branch-protection contexts
- [`development-release-checklist.md`](development-release-checklist.md): end-to-end GO/NO-GO evidence
- [`../roadmaps/v0.9.0/README.md`](../roadmaps/v0.9.0/README.md): ordinary-development verification ownership
- [`.delivery.yml`](../../.delivery.yml): `ci.checks` required PR contexts
