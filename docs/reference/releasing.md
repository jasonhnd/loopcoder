# Releasing

This reference records release-documentation rules that apply to every loopcoder version bump.

For the current production-closure candidate, follow the concrete operator
checklist in [`v0.8.1-release-runbook.md`](v0.8.1-release-runbook.md) (gate,
canaries, Apple trust, tag, publish).

## Required Release Documentation

Every version bump must rewrite all three release-facing surfaces:

- [`CHANGELOG.md`](../../CHANGELOG.md)
- the GitHub Release Note source/body
- [`README.md`](../../README.md) release-facing sections

A version bump is not release-ready until all three are current, internally consistent, and describe how to use the shipped behavior.

## Completeness Rule

The rewrite must be complete and detailed for the release being shipped. It must cover the features, behavior changes, breaking changes, compatibility windows, upgrade steps, operator commands, configuration changes, known transition aliases, and verification expectations that matter to a user installing or upgrading the release.

Do not leave a release note as a generic "automated release" body when the release changes operator behavior. Do not update only the changelog when README examples, status text, setup instructions, or command lists have changed. Do not update only README when the GitHub Release Note is the artifact users will see from the tagged release.

## Consistency Checklist

Before tagging a release:

1. Update the changelog entry for the exact version and date.
2. Update or create the GitHub Release Note source/body used by the release workflow.
3. Update README release-facing sections, including badges, current status, setup, upgrade, command examples, and feature summaries where applicable.
4. Confirm the three surfaces agree on command names, config keys, compatibility aliases, breaking-change wording, and upgrade steps.
5. Run the release's required local verification, including markdown well-formedness checks when available and `go build ./...` for loopcoder source releases.

## Staged GitHub Release Flow

The release workflow builds each advertised archive exactly once, generates and
signs one `SHA256SUMS` manifest, uploads those artifacts to a draft GitHub
Release, and runs the release's native smoke jobs before the release is public.
For v0.8, [`../specs/0884-macos-arm64-only.md`](../specs/0884-macos-arm64-only.md)
is binding: native implementation and release proof target `darwin/arm64`
only. Unsupported OS/arch diagnostics may use credential-free fixtures, but
they are not advertised as native support or required native smoke. Windows,
Linux/Ubuntu, WSL, containers used as a LoopCoder runtime, Intel
macOS, and Rosetta/amd64 macOS are unsupported in v0.8.0; users who need those
hosts remain on v0.7.0 or contribute to a later approved platform roadmap. The
final publication job does not rebuild or re-upload archives; it only promotes the
already-smoked draft release after the protected `release-publication`
environment grants approval.

A failing smoke job must leave the release as a draft. The workflow appends the
failed run URL to the draft notes so the candidate assets and diagnostic
evidence remain available without presenting the release as final.

The v0.8 native smoke job runs on the supported `darwin/arm64` host:

```powershell
pwsh scripts/release-smoke.ps1 -Version 0.8.0 -PreviousVersion 0.7.0
```

The smoke script targets the staged draft release. It downloads the current
platform archive through `gh release download`, verifies `SHA256SUMS` with
cosign, checks the archive checksum, runs
`loopcoder version`, confirms the source checkout has no tracked `.loopcoder/`
files, exercises a temporary-repository `init` / `skill install` / project
registry / `doctor --format json` / `migrate local-state --dry-run` /
`report --format json` path, invokes the self-bootstrap acceptance smoke for
nested run-tree observability, confirms the selected binary recognizes itself
as already latest, and verifies upgrade from the previous release when
`-PreviousVersion` is set. For a v0.7.0 predecessor it also creates a real
schema-9 database with that published binary, proves `migrate storage` planning
is read-only, applies schema 9 through 30, checks idempotent replay and the
verified owner-only backup, then proves the restored backup opens with v0.7.0.
It is verification-only and must not create tags, publish releases, or upload
assets.

Provider live smoke, including Grok, is not part of required CI or default
release smoke. It is an explicit operator diagnostic only, must be enabled by
its environment/flag gate, and must not be used as evidence that provider
credentials, subscription quota, browser/session state, or private
configuration are required for release acceptance.

## Required GitHub Repository Settings

These settings require repository administration rights and are intentionally
not mutated by workers. Apply them before tagging a public release and record
the evidence in the go/no-go report.

Create a protected publication environment with required reviewers:

```bash
REVIEWER_ID=123456
gh api \
  --method PUT \
  "repos/OWNER/REPO/environments/release-publication" \
  --input - <<JSON
{
  "wait_timer": 0,
  "reviewers": [
    {
      "type": "User",
      "id": ${REVIEWER_ID}
    }
  ]
}
JSON
```

Configure `main` to require pull requests and the documented checks. For
v0.8.0, pull-request CI emits exactly four stable contexts: `verify`, `test`,
`race`, and `security`, all running on pinned `macos-15` with a fail-fast
`darwin/arm64` Go tuple assertion. The pull-request `race` context runs the
race detector for Go packages changed by that PR, or a small concurrent sentinel
set when no Go package changed. The release `build` job reruns the complete
race suite with `bash scripts/ci-full-race.sh` before it packages the tagged
artifact. The helper derives the exact package inventory from `go list ./...`
and fails closed if that inventory cannot be built, contains duplicates, or
does not contain the required isolated packages. It runs every package exactly
once with `-race -count=1 -timeout=20m`: ordinary packages run in one bounded
group, then `internal/storage`, `internal/routing`, and
`internal/supervisedexec` run in isolated invocations without ordinary-package
contention. This keeps ordinary PR feedback bounded without weakening the final
release race gate.

This repository currently has one collaborator. GitHub does not allow a pull
request author to approve their own pull request, so a positive branch-review
count would make protected promotion impossible without adding a second trusted
maintainer. Until that happens, `main` still requires a pull request and all
four checks, but `required_approving_review_count` is `0`. The non-waivable
human release approval is the separate `release-publication` environment gate,
whose required reviewer is the repository owner. Do not disable that
environment gate to compensate for the single-maintainer branch topology.

Branch-protection changes are a human-controlled promotion boundary. Do not
remove old required contexts from live protection until a fresh pull request has
shown the four new contexts. Use this order so `main` is never left requiring
permanently un-emitted contexts:

1. Confirm a fresh pull request emits green `verify`, `test`, `race`, and
   `security` contexts.
2. Update `main` branch protection to require exactly those four contexts.
3. Read back the effective GitHub branch-protection configuration and confirm
   no legacy `go`, `staticcheck`, `govulncheck`, `audit`, native Ubuntu,
   native Windows, or `macos-latest` contexts remain required.
4. Only after that confirmation, continue with the human-controlled promotion
   decision.

The branch-protection update payload is:

```bash
gh api \
  --method PUT \
  "repos/OWNER/REPO/branches/main/protection" \
  -H "Accept: application/vnd.github+json" \
  --input - <<'JSON'
{
  "required_status_checks": {
    "strict": true,
    "contexts": [
      "verify",
      "test",
      "race",
      "security"
    ]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": {
    "dismiss_stale_reviews": false,
    "require_code_owner_reviews": false,
    "require_last_push_approval": false,
    "required_approving_review_count": 0
  },
  "restrictions": null
}
JSON
```

Verify the effective settings before GO:

```bash
gh api "repos/OWNER/REPO/branches/main/protection"
gh api "repos/OWNER/REPO/environments/release-publication"
```

The branch-protection readback must show strict `verify`, `test`, `race`, and
`security` checks, pull-request review protection enabled with approval count
`0`, and admin enforcement enabled. The environment readback must independently
show the repository owner as a required reviewer. If a second trusted
maintainer is added later, raise the branch approval count in a separate,
reviewed repository-policy change.

## v0.8.0 Self-Bootstrap Acceptance

Before tagging v0.8.0, run the self-bootstrap acceptance path in
[`self-bootstrap.md`](self-bootstrap.md). At minimum, the release record must
include:

- the scripted smoke result from `pwsh scripts/self-bootstrap-smoke.ps1
  -Version 0.8.0 -Binary <staged-binary>`;
- the staged candidate binary path, SHA-256, version stamp, and proof that the
  same archive is later eligible for publication;
- native `darwin/arm64` host evidence;
- project registry evidence for the loopcoder checkout;
- proof that `$LOOPCODER_HOME/data/loopcoder.db`,
  `$LOOPCODER_HOME/projects/<project_id>/`, `$LOOPCODER_HOME/logs/`, and
  `$LOOPCODER_HOME/tmp/` exist outside the repository;
- doctor JSON showing storage, project registry, provider compatibility, and
  nested-run health;
- status and report human/JSON output showing the same parent/child run tree;
- zero paid provider calls and no private credential dependency;
- issue-to-PR-to-candidate-SHA evidence for the v0.8.0 implementation issues;
- the normal consumer artifact smoke,
  `pwsh scripts/release-smoke.ps1 -Version 0.8.0 -PreviousVersion 0.7.0`;
- schema-9 planning, owner-only backup, atomic schema-30 application,
  idempotent replay, and a copied backup opened by v0.7.0;
- the completed go/no-go report from
  [`v0.8.0-go-no-go.md`](v0.8.0-go-no-go.md), attached to the release
  readiness issue.

This acceptance path is verification-only. It must not force production
auto-merge, fake success without PR evidence, or depend on paid provider
services that are not available to the operator.

## v0.8.1 Packaged Nested Permission Matrix

The v0.8.1 release record must include the packaged nested-permission matrix
result for all seven permission outcomes and their replays, including
zero-launch refusal evidence, progress receipts, deterministic audit/reason
codes, parent/worktree isolation, and a matching candidate SHA-256.

The packaged permission matrix is bounded to seven cases, fourteen invocations,
one concurrent child, depth two, 20 seconds per invocation, and five minutes
overall. Its blocking provider is always `test-subprocess` with zero paid calls
and no network. On failure, the release workflow uploads the sanitized
`loopcoder.nested_permission_matrix_diagnostic.v1` bundle for seven days, with
a 64 KiB size ceiling; raw outputs, prompts, credentials, and machine paths are
excluded.

The successful sanitized evidence JSON is retained as a private GitHub Actions
artifact for 90 days. It is release evidence, not a public release asset, so
the published release remains limited to the macOS arm64 archive, checksums,
and Sigstore bundle.

Protected real-provider canaries are opt-in only through
`scripts/nested-permission-real-provider-smoke.sh` and
`LOOPCODER_REAL_PROVIDER_SMOKE=1`. The script refuses pull-request events and
supports only registered contracts: Codex, Claude, and Grok for read-only;
Codex and Grok for bounded write. These canaries are non-blocking and never
receive fork PR secrets.

Historical changelog entries, release notes, go/no-go records, and accepted
specs are release history. Do not terminology-sweep old release evidence merely
because the current platform or naming changed.
