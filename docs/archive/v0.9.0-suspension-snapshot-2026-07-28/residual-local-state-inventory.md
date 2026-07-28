# Residual Local State Inventory

**Audit date:** 2026-07-28
**Trigger:** Broad post-cleanup home-directory scans
**Disposition:** Preserve bounded public-safe summaries and unique source;
delete raw local state

## 1. Why Additional Sweeps Were Required

The primary cleanup was deliberately scoped to the active development estate
under `${HOME}/AgenticCoder/loopcoder*` and `${TMPDIR}/loopcoder*`. After that
scope was removed, a broader name-based scan found older LoopCoder material in
three other classes:

1. historical Codex work areas under `${HOME}/Documents/Codex`;
2. hidden `$LOOPCODER_HOME` variants directly under `${HOME}`;
3. Go module cache and macOS crash-report metadata.

These paths were not part of the original 2,035,732,480-byte audit boundary.
The second and third sweeps account for them explicitly instead of silently
expanding the first inventory after deletion.

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

## 5. Third Sweep: Provider Histories And Installed Copies

A final full-name scan found another 1,200,928 KiB of measured
project-specific paths. One 4 KiB Codex heartbeat definition had already been
removed through the Codex automation API, so the complete third-sweep boundary
was 1,200,932 KiB, approximately 1.15 GiB.

This was not another source checkout. Most of it was provider transcript and
terminal history.

### 5.1 Provider And Orchestrator State

| Sanitized class | KiB | Entries | Observed file-time range | Disposition |
| --- | ---: | ---: | --- | --- |
| Grok session and terminal history | 1,001,828 | 6,251 files | 2026-07-12 through 2026-07-28 | Summarize; delete raw |
| Claude project histories | 34,428 | 195 project directories, 223 files | 2026-07-03 through 2026-07-23 | Summarize; delete raw |
| Gemini history and temporary records | 16 | 4 files | 2026-07-10 through 2026-07-22 | Summarize; delete raw |
| Paseo agent index for this repository | 36 | 9 agent records | 2026-07-04 through 2026-07-28 | Verify closed and archived; hard-delete |
| Claude project caches | 520 | 4 directories | Host-specific | Delete |
| Claude temporary project placeholders | 0 | 7 empty directories | Host-specific | Delete |

The Grok tree contained 5,903 `.log`, 174 `.json`, 66 `.jsonl`, 51 `.md`,
34 `.lock`, and 23 `.txt` files. Its logical file size was 1,009,206,975
bytes. The Claude project histories had a logical file size of 34,417,626
bytes. These format counts were collected without copying transcript content
into the repository.

All nine Paseo agents were inspected through the Paseo CLI before deletion.
Every record reported `status=closed` and `archived=true`; the provider split
was four Grok, three Codex, and two Claude agents. This included the final
`0900` Codex agent and the earlier closed Grok agent. No agent was resumed or
woken during inspection.

The recurring Codex heartbeat named `Keep LoopCoder v0.9 core routing moving`
ran every 15 minutes. It was deleted through the Codex automation API before
the filesystem cleanup so it could not reopen or continue the stopped
development effort.

Raw provider and orchestrator state was not uploaded because it can contain
prompts, terminal output, account-bound metadata, local paths, private
repository context, and provider-controlled credential shapes. A compressed
raw upload would be unsafe in a public repository and would not be reliable
product evidence.

### 5.2 Installed And Reproducible Copies

| Sanitized class | KiB | Contents | Disposition |
| --- | ---: | --- | --- |
| `${HOME}/.local/bin/loopcoder*` | 83,920 | Four binaries plus one symlink; 85,927,560 logical bytes | Delete |
| `${HOME}/.local/opt/loopcoder` | 80,144 | Six v0.8.1 archive, checksum, signature, and test-binary files | Delete |
| Global Claude `loopcoder` skill | 36 | `SKILL.md` and `AGENTS.md` | Preserve one bounded diff; delete installed copy |

The public v0.8.1 archive and release metadata remain on GitHub. The other
installed binaries were local test, hotfix, backup, or RC copies and were
reproducible or superseded by retained source and draft assets. They were not
uploaded again.

The installed skill's `AGENTS.md` was byte-identical to the tracked repository
file. Its `SKILL.md` differed from the tracked file by 15 changed lines around
foreground and detached dispatch behavior. That stale local delta is retained
as a path-sanitized text diff under
[`historical/local-tooling`](historical/local-tooling); the installed skill is
not an authoritative runtime or resume instruction.

### 5.3 Unrelated Repository State Left Intact

The broad scan also matched `.loopcoder` directories and `refs/loopcoder`
namespaces inside other active repositories. Those paths belong to those
repositories, not to the deleted LoopCoder source-development estate. They
were deliberately left intact to avoid corrupting unrelated run history or
Git state.

Codex task history, Codex memory, and provider memory belonging to other
repositories were also left intact. A name match alone is not sufficient
authority to delete another project's state.

## 6. Recovery Boundary

The deleted runtime homes are intentionally not recoverable byte-for-byte.
Future work can recover:

- source and tracked history from GitHub;
- RC60 and RC61 exact archives from the unpublished draft;
- sanitized decision evidence from the draft evidence asset;
- the historical notes and two local-only code diffs in this directory.

Future work cannot recover:

- the deleted provider sessions;
- the deleted Claude, Gemini, Grok, and Paseo project histories;
- mutable SQLite runtime state;
- old temporary worktrees;
- cache contents;
- local process and lock identity.

That loss is intentional. None of those classes is accepted product evidence
or safe routing authority.
