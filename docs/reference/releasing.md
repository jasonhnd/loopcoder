# Releasing

This reference records release-documentation rules that apply to every loopcoder version bump.

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
Release, and runs native smoke jobs on Ubuntu, macOS, and Windows before the
release is public. The final publication job does not rebuild or re-upload
archives; it only promotes the already-smoked draft release after the protected
`release-publication` environment grants approval.

A failing smoke job must leave the release as a draft. The workflow appends the
failed run URL to the draft notes so the candidate assets and diagnostic
evidence remain available without presenting the release as final.

The native smoke jobs run:

```powershell
pwsh scripts/release-smoke.ps1 -Version 0.7.0
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
`-PreviousVersion` is set. It is verification-only and must not create tags,
publish releases, or upload assets.

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

Configure `main` to require pull requests and the documented checks. Adjust the
check list to the exact check names shown by GitHub for the current workflow
matrix:

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
      "go",
      "staticcheck",
      "govulncheck",
      "audit",
      "native (ubuntu-latest)",
      "native (macos-latest)",
      "native (windows-latest)"
    ]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": {
    "required_approving_review_count": 1
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

## v0.7.0 Self-Bootstrap Acceptance

Before tagging v0.7.0, run the self-bootstrap acceptance path in
[`self-bootstrap.md`](self-bootstrap.md). At minimum, the release record must
include:

- the scripted smoke result from `pwsh scripts/self-bootstrap-smoke.ps1`;
- project registry evidence for the loopcoder checkout;
- proof that `$LOOPCODER_HOME/data/loopcoder.db`,
  `$LOOPCODER_HOME/projects/<project_id>/`, `$LOOPCODER_HOME/logs/`, and
  `$LOOPCODER_HOME/tmp/` exist outside the repository;
- doctor JSON showing storage, project registry, provider compatibility, and
  nested-run health;
- status and report JSON showing at least one parent/child run tree;
- issue-to-PR evidence for the v0.7.0 implementation issues;
- the normal consumer artifact smoke,
  `pwsh scripts/release-smoke.ps1 -Version 0.7.0`, after release assets exist.
- the completed go/no-go report from
  [`v0.7.0-go-no-go.md`](v0.7.0-go-no-go.md), attached to the release
  readiness PR or issue.

This acceptance path is verification-only. It must not force production
auto-merge, fake success without PR evidence, or depend on paid provider
services that are not available to the operator.

Historical changelog entries and accepted specs are release history. Do not terminology-sweep old release entries or shipped specs merely because current naming changed.
