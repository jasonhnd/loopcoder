# Residual Local State Inventory

**Audit date:** 2026-07-28
**Trigger:** Broad post-cleanup home-directory scan
**Disposition:** Preserve bounded public-safe summaries and unique source;
delete raw local state

## 1. Why A Second Sweep Was Required

The primary cleanup was deliberately scoped to the active development estate
under `${HOME}/AgenticCoder/loopcoder*` and `${TMPDIR}/loopcoder*`. After that
scope was removed, a broader name-based scan found older LoopCoder material in
three other classes:

1. historical Codex work areas under `${HOME}/Documents/Codex`;
2. hidden `$LOOPCODER_HOME` variants directly under `${HOME}`;
3. Go module cache and macOS crash-report metadata.

These paths were not part of the original 2,035,732,480-byte audit boundary.
The second sweep accounts for them explicitly instead of silently expanding
the first inventory after deletion.

## 2. Size Boundary

The second sweep identified 252,332 KiB of allocated local storage,
approximately 246.4 MiB.

### 2.1 Historical Checkouts And Notes

| Sanitized path class | KiB | State before deletion | Preservation decision |
| --- | ---: | --- | --- |
| Older standalone clone | 4,880 | Clean `main` at `e7c0385...`; exact ancestor of current `main` | Delete |
| Older release-control checkout | 30,996 | Clean branch at `91e0e89...`; exact ancestor of current `main` | Preserve two unique local branch diffs; delete |
| Linked documentation worktree | 11,256 | Clean at `cf2e357...`; merged by PR #1001 and ancestor of current `main` | Delete |
| Three standalone Markdown notes | 76 | 70,586 logical bytes | Preserve byte-for-byte under [`historical/source-notes`](historical/source-notes) |

The release-control checkout also referenced historical issue #993 and #997
worktrees. Issue #993's commit remains durable in merged PR #994 and its remote
branch; issue #997's local patch is equivalent to current `main` and is
durable in merged PR #998. No additional local preservation was required for
those worktrees.

Two other local branches, issue #708 and issue #711, were different:

- neither exact commit was reachable or patch-equivalent to current `main`;
- neither exact commit had an associated GitHub pull request;
- the source worktrees were clean, so each commit diff was a complete bounded
  unit.

Their 102,069 bytes of code diff are preserved under
[`historical/unmerged-diffs`](historical/unmerged-diffs).

### 2.2 Hidden Runtime Homes

Six hidden runtime homes accounted for 204,996 KiB and 5,563 files.

| Sanitized runtime home | KiB | Files | Observed file-time range | Classification |
| --- | ---: | ---: | --- | --- |
| `${HOME}/.loopcoder` | 70,180 | 502 | 2026-07-12 through 2026-07-28 | Global self-development and test runtime |
| `${HOME}/.loopcoder-v090-bootstrap` | 75,376 | 3,275 | 2026-07-20 | v0.9 Gate A bootstrap attempts |
| `${HOME}/.loopcoder-v090-bootstrap-r13-grok` | 40,156 | 1,203 | 2026-07-20 through 2026-07-21 | Grok-isolated Gate A attempts |
| `${HOME}/.loopcoder-v080-release` | 14,340 | 571 | 2026-07-16 | v0.8 release-blocker runs |
| `${HOME}/.loopcoder-v080-release-fix1` | 2,648 | 11 | 2026-07-16 | v0.8 issue #993 retry |
| `${HOME}/.loopcoder-v080-final` | 2,296 | 1 | 2026-07-16 | Final v0.8 runtime database |

The file-time range records filesystem state, not an acceptance timeline.

Each runtime home contained one valid SQLite database. All six returned `ok`
from `PRAGMA integrity_check` before deletion. The directories also contained
some combination of:

- project registrations and run records;
- lifecycle and event JSON Lines;
- provider execution logs and isolated provider homes;
- recovery, relay, audit, and quota records;
- temporary Git worktrees and compiled artifacts;
- machine-local paths and mutable lock/process state.

The raw databases and logs were not uploaded. They cross project, account,
machine, and provider-session boundaries and cannot be made public-safe merely
by renaming files. The useful v0.9 decision evidence had already been
structurally sanitized and retained in the draft evidence asset; the v0.8 code
results were already represented by GitHub commits and merged PRs.

### 2.3 Reproducible Caches And Host Metadata

| Sanitized class | KiB | Disposition |
| --- | ---: | --- |
| Go module download cache for `jasonhnd/loopcoder` | 120 | Delete; reproducible |
| Unrelated empty/stub `github.com/loopcoder` module-cache path | 4 | Delete; no unique source |
| macOS LoopCoder crash-reporter plist | 4 | Delete; host-specific metadata |

## 3. Raw-State Exclusion Rationale

The repository is public. Raw runtime homes were excluded because they may
contain:

- absolute workstation paths;
- account and provider installation identifiers;
- session histories and prompts;
- private repository remnants;
- mutable process, lock, and quota observations;
- credentials or signed values in provider-controlled formats;
- stale runtime state whose internal relationships are no longer trustworthy.

Uploading a compressed copy would preserve more bytes but less reliable
knowledge, while increasing privacy risk. The safe preservation boundary is:

- exact historical source notes;
- exact code diffs for unique local commits;
- counts, sizes, date ranges, integrity results, and Git disposition;
- the already-sanitized v0.9 decision evidence asset.

## 4. Deletion Preconditions

Deletion of the second-sweep paths is allowed only after:

1. the five historical files are committed to a remote branch;
2. their remote bytes and SHA-256 values match the local sources;
3. the residual inventory is present on the same remote branch;
4. no LoopCoder development, canary, agent, or qualification process is
   running;
5. all affected Git checkouts are clean or their unique diff is preserved;
6. the unpublished suspension draft remains `draft=true`;
7. no v0.9 Git tag or public release exists.

The final cleanup result is recorded in
[`cleanup-ledger.md`](cleanup-ledger.md).

## 5. Recovery Boundary

The deleted runtime homes are intentionally not recoverable byte-for-byte.
Future work can recover:

- source and tracked history from GitHub;
- RC60 and RC61 exact archives from the unpublished draft;
- sanitized decision evidence from the draft evidence asset;
- the historical notes and two local-only code diffs in this directory.

Future work cannot recover:

- the deleted provider sessions;
- mutable SQLite runtime state;
- old temporary worktrees;
- cache contents;
- local process and lock identity.

That loss is intentional. None of those classes is accepted product evidence
or safe routing authority.

