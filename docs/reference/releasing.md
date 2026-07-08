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

After publishing a release, run the consumer artifact smoke:

```powershell
pwsh scripts/release-smoke.ps1 -Version 0.6.1
```

The smoke script downloads the published archive for the current platform,
verifies `SHA256SUMS` with cosign, checks the archive checksum, runs
`loopcoder version`, exercises a temporary-repository `init` / `skill install`
/ `doctor --format json` / `report` path, and confirms
`loopcoder upgrade --version 0.6.1` recognizes the selected binary as already
latest. It is verification-only and must not create tags, publish releases, or
upload assets.

Historical changelog entries and accepted specs are release history. Do not terminology-sweep old release entries or shipped specs merely because current naming changed.
