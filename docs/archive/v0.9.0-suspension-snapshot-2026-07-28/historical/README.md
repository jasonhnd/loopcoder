# Historical Residual Material

This directory preserves useful material found by the final broad local sweep
after the primary v0.9 suspension archive had already been merged.

The contents are historical evidence only. They do not reopen development,
change the `NO_GO` decision, approve either unmerged change, or define the next
roadmap.

## Source Notes

The three Markdown payloads were copied byte-for-byte from an older Codex work
area before that local area was deleted. Each payload is stored in a valid JSON
archive envelope with its original filename, media type, byte count, SHA-256,
and Base64 content. This keeps the historical bytes exactly recoverable while
preventing repository-wide live-document terminology gates from interpreting
frozen prose as current product documentation.

| File | Bytes | SHA-256 | Interpretation |
| --- | ---: | --- | --- |
| [`source-notes/loopcoder-product-notes-2026-07.snapshot.json`](source-notes/loopcoder-product-notes-2026-07.snapshot.json) | 25,270 | `b300b8b0377ba330b11f456ebc0e39015bffaae7f776a9278c000b26bbf7ed9f` | Product analysis spanning the post-0.6 period and a v0.8 review |
| [`source-notes/loopcoder-0.6.1-customer-ready-roadmap.snapshot.json`](source-notes/loopcoder-0.6.1-customer-ready-roadmap.snapshot.json) | 29,461 | `4f651c01de6a2d81eb50d0854eb8c6ecff79127cec18b4ca7f1429a953852fcc` | Superseded 0.6.1 customer-readiness plan |
| [`source-notes/loopcoder-v0.8.0-postmortem-draft.zh-CN.snapshot.json`](source-notes/loopcoder-v0.8.0-postmortem-draft.zh-CN.snapshot.json) | 15,855 | `38e99bec910885bba156644dd1c98a48a881e31b3d601a9d0b52d5d59ee1f17f` | Earlier Chinese postmortem draft; not identical to the tracked v0.8 retrospective |

These notes predate the authoritative
[`v0.9.0 development suspension report`](../../../v0.9.0-development-suspension-report-2026-07-26.md).
Where they disagree with current tracked documentation, the later tracked
documentation and suspension report control.

## Unmerged Code Diffs

Two clean local branches contained commits that were neither reachable nor
patch-equivalent to `origin/main`. GitHub had no copy of the first commit and
no associated pull request for either commit. Their code changes are preserved
as plain binary-capable Git diffs without author name, author email, commit
message headers, or local path metadata. Each unchanged raw diff is stored in
the same JSON archive-envelope format used for the source notes.

| File | Local commit | Parent | Payload bytes | Payload SHA-256 |
| --- | --- | --- | ---: | --- |
| [`unmerged-diffs/issue-708-d73b797.diff.json`](unmerged-diffs/issue-708-d73b797.diff.json) | `d73b797800ee2bc04152fbef9f99a0bb1cd61395` | `cd83d73e9b6b144095f9259a19ee2872a15e00a2` | 50,406 | `264adb9151cdc2e53e91b7709ffe78b2a05c1afa63d1b5527abd637a9eb4035c` |
| [`unmerged-diffs/issue-711-69dc517.diff.json`](unmerged-diffs/issue-711-69dc517.diff.json) | `69dc51719875aa3c07b993bd5851ba6a2be01427` | `cd83d73e9b6b144095f9259a19ee2872a15e00a2` | 51,663 | `31b5586ac2f1bc5b957f15a1e33471792cb82ff2ec25bf31049c17732abe9199` |

Issue #708 and issue #711 were closed as completed on 2026-07-10, but closure
does not prove that these exact local commits were reviewed, merged, or
accepted. A future operator may inspect the diffs, but must not apply them
automatically. At minimum, recovery requires a fresh design decision, review
against the then-current tree, `git apply --check`, focused tests, full CI, and
ordinary PR review.

## Local Tooling Delta

The global Claude `loopcoder` skill contained two files. Its `AGENTS.md` was
byte-identical to the tracked repository file and was not duplicated. Its
`SKILL.md` was a stale local copy with a small foreground-versus-detached
dispatch difference. The installed file was not authoritative, but its unique
text delta is retained so the deletion is auditable.

| File | Payload bytes | Payload SHA-256 | Interpretation |
| --- | ---: | --- | --- |
| [`local-tooling/claude-skill-stale-dispatch.diff.json`](local-tooling/claude-skill-stale-dispatch.diff.json) | 3,369 | `d13577605e2acb0f5c6b209275ee157e3c5c59286ea4a7fe651291d77e5761fd` | Path-sanitized diff from the current tracked `SKILL.md` to the stale installed copy |

This diff is historical evidence, not installation guidance. Current tracked
documentation controls. A future operator must not use the diff to downgrade
the repository's dispatch behavior.

## Payload Recovery

Every JSON envelope uses schema
`loopcoder.suspension_archive_payload.v1`. Recover and verify a payload with:

```sh
jq -r '.payload_base64' <archive.json> | base64 --decode > <restored-file>
shasum -a 256 <restored-file>
```

The result must match both `payload_bytes` and `payload_sha256` in the
envelope. A mismatch is a failed recovery; do not use the result.

## Public-Safety Check

All six payloads were scanned after copying or export for:

- workstation home and temporary paths;
- email addresses;
- GitHub, OpenAI, Anthropic, xAI, bearer, and common API-key shapes;
- credential assignments;
- private repository names.

No actual workstation path, email address, private repository identity, or
recognized credential value was found. References such as
`$LOOPCODER_HOME/tmp/` in the code diff are product documentation variables,
not local host paths.
