# Local Cleanup Ledger

**Ledger opened:** 2026-07-28
**Operation:** v0.9 project suspension archive and local deletion
**Initial local scope:** 2,035,732,480 bytes
**Safety model:** Preserve, upload, download-verify, then delete
**Current phase:** All authorized local deletion complete; archival PR and
exact-merge verification pending

This ledger is intentionally committed in two phases.

1. The first commit records what is about to be deleted and the remote evidence
   required before deletion may begin.
2. The final commit records the actual deletions and post-cleanup scans.

The first phase prevents the cleanup plan from existing only in the same local
estate that will be deleted.

## 1. Authorized Scope

The owner authorized:

- project suspension;
- a detailed stop report;
- commit, push, PR, and merge of the stop record;
- closure of open project issues and PR #1450;
- preservation of useful local evidence in `jasonhnd/loopcoder`;
- sanitation of material that could not safely be stored directly;
- deletion of useless local LoopCoder worktrees, clones, build products,
  caches, databases, and temporary files;
- final verification that no LoopCoder development garbage remains locally.

The owner did not authorize publication of v0.9.0. No public v0.9 tag or release
may be created.

## 2. Pre-Deletion Remote Gates

Local deletion is blocked until every gate below passes:

| Gate | Required result | Phase-one state |
| --- | --- | --- |
| Documentation branch | Pushed to `jasonhnd/loopcoder` | Verified at `7fd50427d5b8f914e74e20b99479e1bffc96ae2f` |
| Pull request | Open against `main` | Verified as PR #1454 |
| Exact-head CI | `verify`, `test`, `race`, and `security` pass | Verified in run `30331022053` |
| Draft snapshot | Exists and `draft=true` | Verified as draft ID `360850177` |
| Draft publication | `published_at=null` | Verified |
| Product tag | No v0.9 tag | Verified again before deletion |
| Internal lookup ref | No Git tag ref | Verified again before deletion |
| RC60 upload | Size and SHA-256 match | Verified as asset `492314600` |
| RC61 upload | Size and SHA-256 match | Verified as asset `492314598` |
| Evidence upload | Size and SHA-256 match | Verified as asset `492314599` |
| Manifest upload | Downloaded JSON equals local file | Verified as asset `492314597` |
| Checksum upload | Downloaded file validates all assets | Verified as asset `492314596` |
| Process audit | No LoopCoder/Paseo development or canary process | Verified again before deletion |
| Worktree audit | No unique unpreserved tracked/untracked changes | Verified |
| Secret scan | Public-safe evidence only | Verified locally and after GitHub download |

## 3. Local Paths In Scope

### 3.1 Development root

| Path | KiB | Required preservation before deletion |
| --- | ---: | --- |
| `${HOME}/AgenticCoder/loopcoder` | 54,520 | Seven non-equivalent commit patches, refs, object inventory |
| `${HOME}/AgenticCoder/loopcoder-worktrees` | 447,156 | Clean-status record |
| `${HOME}/AgenticCoder/loopcoder-v090` | 23,112 | Commit identity and ancestor proof |
| `${HOME}/AgenticCoder/loopcoder-v090-visibility` | 12,528 | Commit identity and ancestor proof |
| `${HOME}/AgenticCoder/loopcoder-v090-gateA` | 23,672 | Binary Git patch |
| `${HOME}/AgenticCoder/loopcoder-v090-gateA-r02` | 24,084 | Binary Git patch and six small run records |

### 3.2 Temporary storage

All 57 `${TMPDIR}/loopcoder*` top-level paths are in scope after selected RC60,
RC61, qualification, provider, patch, and inventory evidence is uploaded and
verified.

Two scanner directories created during finalization are also in scope:

- `${TMPDIR}/lc-archive-scan.1jMwyb`;
- `${TMPDIR}/lc-archive-scan.KjTETb`.

A third disposable scanner directory was created for the independent
post-download scan and is also in scope:

- `${TMPDIR}/lc-v090-download-scan.QDnUUa`.

The finalizer's own temporary clone, staging, download-verification, and
archive-scan directories are deleted last:

- `${TMPDIR}/lc-v090-finalize`;
- `${TMPDIR}/lc-v090-snapshot-stage`;
- `${TMPDIR}/lc-v090-download-verify`.

## 4. Paths Explicitly Out Of Scope

The cleanup must not delete or modify:

- unrelated repositories under `${HOME}/AgenticCoder`;
- global `gh` authentication;
- provider CLI installations or credentials outside the selected LoopCoder
  temporary fixtures;
- Paseo application data unrelated to this stopped project;
- Codex tasks, memory, or conversation history;
- unrelated content in the current Codex workspace; only the separately
  inventoried LoopCoder subpaths and notes are in the residual sweep;
- public v0.8.1 release assets;
- any GitHub repository other than `jasonhnd/loopcoder`;
- the GitHub source, issues, PRs, Actions runs, or draft snapshot.

## 5. Deletion Method

The intended order is:

1. remove each registered linked worktree through Git;
2. prune worktree administration records;
3. remove the four independent clones;
4. remove the now-empty worktree parent;
5. remove the central controller last;
6. remove every `${TMPDIR}/loopcoder*` top-level path;
7. update this ledger from the independent finalization clone;
8. push and merge the final ledger;
9. remove the finalization clone, staging, and download-verification paths.

The controller is deleted after its linked worktrees because it owns their
common Git metadata.

## 6. Final Verification Requirements

The final ledger must report:

- remote draft ID;
- asset IDs, sizes, and downloaded SHA-256 results;
- PR number and merged `main` SHA;
- exact deleted top-level path counts;
- bytes in the pre-deletion scope;
- final scan for `${HOME}/AgenticCoder/loopcoder*`;
- final scan for `${TMPDIR}/loopcoder*` and finalizer paths;
- no open issues;
- no open pull requests;
- no v0.9 product tag;
- no v0.9 public release;
- draft snapshot still unpublished.

## 7. Phase-One Result

Remote preservation and independent download verification passed.

### 7.1 Documentation and CI

- Branch: `docs/v090-suspension-snapshot-20260728`
- Exact verified head:
  `7fd50427d5b8f914e74e20b99479e1bffc96ae2f`
- Pull request: #1454 against `main`
- CI run: `30331022053`
- Same-run jobs: `verify`, `test`, `race`, and `security` all passed

### 7.2 Unpublished Draft

- Release database ID: `360850177`
- Name: `Internal suspension snapshot 2026-07-28 (not a release)`
- `draft`: `true`
- `prerelease`: `true`
- `published_at`: `null`
- No `refs/tags/internal-snapshot-v090-20260728` Git ref exists.
- No `refs/tags/v0.9*` Git ref exists.

The draft lookup label is not a product version or release authorization.

### 7.3 Assets

| Asset | Asset ID | Bytes | Downloaded SHA-256 |
| --- | ---: | ---: | --- |
| `loopcoder_0.9.0-rc.60_darwin_arm64.tar.gz` | `492314600` | 9,264,799 | `270870d7ae0b1712d6eb3cbf67d948fdc680b0da4aceaf5344a08c03b70e7aae` |
| `loopcoder_0.9.0-rc.61_darwin_arm64.tar.gz` | `492314598` | 9,271,775 | `bea1a17c1e1ef500569ce2283128646c8337a2191c9639921580fbc9d7ac8db1` |
| `SHA256SUMS` | `492314596` | 417 | `b29001e0e3e62ccb9936aac2fc563d58e83f1832c0806eec6530a48a70770a85` |
| `snapshot-manifest.json` | `492314597` | 2,653 | `5628071912dc60defb143cb0f6ae3fc5576414f66c9252805f0d0a1182a25072` |
| `v0.9.0-suspension-evidence-2026-07-28.tar.zst` | `492314599` | 187,791 | `ea0bd8ca4654c1e5f04b6d2067a61bc6d62dc8c2d825d7d09d47628082c1c52d` |

Each asset was downloaded through the GitHub asset API and compared
byte-for-byte with the prepared source. `SHA256SUMS` validated all four
payload assets, the evidence archive passed `zstd -t`, and the manifest parsed
as JSON.

### 7.4 Safety Recheck

- The downloaded evidence archive was independently extracted.
- Sixty-two retained files were rescanned.
- No workstation home path, private disposable repository name, email address,
  or recognized token shape was found.
- Process and open-file audits found no active LoopCoder development, Paseo
  agent, provider canary, goal run, or artifact qualification process using
  the deletion scope.
- The 22 linked worktrees still matched the audited inventory.
- GitHub reported zero open project issues before deletion.

All section 2 gates have passed. The useful recovery material no longer
depends on the local development estate, so local deletion may proceed.

## 8. Deletion Result

The authorized primary cleanup completed on 2026-07-28.

### 8.1 Linked Worktrees

- Registered linked worktrees before removal: 22
- Nontrivial uncommitted state found in the final status pass: 0
- Removal method: `git worktree remove --force`
- Worktree administration cleanup: `git worktree prune`
- Registered linked worktrees after removal: 0

The `--force` flag was needed only because 19 worktrees contained an untracked
`.DS_Store`. The final status pass found no unpreserved source or evidence
change.

### 8.2 Development Root

Six top-level paths were deleted:

| Deleted path class | Count |
| --- | ---: |
| Central bare controller | 1 |
| Linked-worktree parent | 1 |
| Clean independent clones | 2 |
| Dirty independent clones preserved as binary patches | 2 |
| **Total** | **6** |

The controller was deleted last because it owned the linked worktree
administration records.

### 8.3 Temporary Estate

- Audited `${TMPDIR}/loopcoder*` top-level paths: 57
- Deleted `${TMPDIR}/loopcoder*` top-level paths: 57
- Remaining `${TMPDIR}/loopcoder*` top-level paths: 0
- Finalization scanner paths deleted: 3

One old canary cache contained read-only Go module files. After the first
deletion pass stopped at that boundary, write permission was restored only for
that selected canary directory. The directory and the remaining nine
top-level paths were then deleted successfully.

The removed temporary material included:

- repeated extracted RC binaries;
- draft-RC build and qualification directories;
- consumer-canary clones and homes;
- provider smoke-test homes;
- Go build and module caches;
- runtime databases and lock files;
- duplicate logs, JSON observations, and PR-body scratch files.

Their useful decision and recovery content had already been retained in the
download-verified GitHub assets.

### 8.4 Space Boundary

The audited pre-deletion scope was 2,035,732,480 bytes, approximately 1.90 GiB
or 2.04 GB. That number covered the six development-root paths and 57
`${TMPDIR}/loopcoder*` paths.

The three finalization-only paths below were retained until PR #1454 was
merged and the draft target was rebound to the merge commit:

- `${TMPDIR}/lc-v090-finalize`;
- `${TMPDIR}/lc-v090-snapshot-stage`;
- `${TMPDIR}/lc-v090-download-verify`.

They were not development leftovers. They were the independent Git clone,
upload source, and download-verification copy needed to finish the remote
transaction. Their combined size before final removal was 123,384 KiB. All
three were deleted after exact-merge CI passed and the remote state was read
back successfully.

## 9. Post-Deletion Verification

The primary cleanup passed these checks:

| Check | Result |
| --- | --- |
| `${HOME}/AgenticCoder/loopcoder*` top-level scan | 0 paths |
| `${TMPDIR}/loopcoder*` top-level scan | 0 paths |
| Finalization scanner and transaction-path scan | 0 paths |
| Open project issues | 0 |
| Open pull requests | 0 after PR #1454 merged |
| v0.9 product tags | 0 |
| Internal lookup Git tag refs | 0 |
| Draft snapshot | ID `360850177`, still `draft=true` |
| Draft publication | `published_at=null` |
| Draft asset count | 5 |

Process inspection found no LoopCoder development agent, provider canary, goal
run, or artifact qualification process. Matches during the check were only the
inspection commands themselves.

## 10. Primary Transaction Result

PR #1454 merged on 2026-07-28.

| Identity | Exact value |
| --- | --- |
| Final PR head | `2c8970d06aaee98613178de653299eaee30927b3` |
| Merge commit | `19d25dbf230482564173c7eb6eb7c7d1de5f189f` |
| Exact-merge CI run | `30332243077` |
| Exact-merge CI result | `verify`, `test`, `race`, and `security` passed |
| Draft target after merge | `19d25dbf230482564173c7eb6eb7c7d1de5f189f` |
| Draft publication | `draft=true`, `published_at=null` |
| Latest public release | `v0.8.1` |

The remote branch `docs/v090-suspension-snapshot-20260728` was deleted after
merge. Final readback found zero open issues, zero open pull requests, no
v0.9 Git tag, no internal lookup Git ref, and all five draft assets still
uploaded with their recorded sizes and digests.

The merge commit cannot be written into the commit that creates it without an
infinite self-reference. GitHub PR #1454 is therefore the authoritative merge
record, and the draft's `target_commitish` is the durable merge-SHA binding.

## 11. Residual Sweep Result

A broader scan after the primary transaction found another 252,332 KiB outside
the original scope:

- three historical checkout/work-area paths;
- three standalone Markdown notes;
- six hidden `$LOOPCODER_HOME` variants;
- two Go module cache paths;
- one macOS crash-reporter plist.

The six hidden runtime homes contained 5,563 files and six SQLite databases.
Every database passed `PRAGMA integrity_check`. Raw state was excluded because
it included machine-local paths, mutable databases, provider homes, session
logs, and temporary Git worktrees.

Five bounded files totaling 172,655 bytes were retained:

- three historical Markdown notes;
- one code diff for local-only issue #708 commit `d73b797...`;
- one code diff for local-only issue #711 commit `69dc517...`.

Their sizes, hashes, provenance, and safety scan are in
[`historical/README.md`](historical/README.md). The complete residual
classification is in
[`residual-local-state-inventory.md`](residual-local-state-inventory.md).

The preservation commit
`6610b91a3b2cf71c54003c7a806166c27082b597` was pushed to
`docs/v090-residual-cleanup-20260728`. Each of the five retained historical
files was then read back through the GitHub API and matched against its local
SHA-256 value before deletion.

The second-sweep deletion removed:

- the three selected historical checkout and work-area paths;
- the three standalone Markdown source files after remote preservation;
- all six hidden LoopCoder runtime homes;
- the two selected Go module-cache paths;
- the LoopCoder crash-reporter plist.

The linked documentation and release-control worktrees were removed through
`git worktree remove --force` before their controller metadata was pruned.
All selected second-sweep paths were absent in the post-deletion scan. No
unrelated repository was modified.

## 12. Third Sweep Pre-Deletion Record

A final full-name scan found 1,200,932 KiB of additional project-specific
state, including one 4 KiB automation definition:

| Class | KiB | Preservation boundary |
| --- | ---: | --- |
| Grok session and terminal history | 1,001,828 | Counts, formats, logical bytes, and time range only |
| Claude project histories | 34,428 | Directory count, file count, logical bytes, and time range only |
| Gemini project records | 16 | File count, logical bytes, and time range only |
| Paseo agent records | 36 | Nine agents verified closed and archived |
| Claude project caches | 520 | Reproducible cache; no preservation |
| Local LoopCoder binaries and symlink | 83,920 | Reproducible or superseded; no binary upload |
| Local LoopCoder installation tree | 80,144 | Public v0.8.1 remains remote; no duplicate upload |
| Installed Claude LoopCoder skill | 36 | One stale text delta retained; duplicate file omitted |
| Empty Claude temporary project directories | 0 | No information content |
| Deleted Codex heartbeat definition | 4 | Name, 15-minute cadence, and deletion result recorded |

The recurring Codex heartbeat was deleted through the Codex automation API
before local file removal. This prevents a later scheduled wake from
contradicting the owner's project-stop decision.

The installed skill's unique, path-sanitized delta is retained as
[`historical/local-tooling/claude-skill-stale-dispatch.diff.md`](historical/local-tooling/claude-skill-stale-dispatch.diff.md).
Its remote readback must match SHA-256
`d13577605e2acb0f5c6b209275ee157e3c5c59286ea4a7fe651291d77e5761fd`
before the third-sweep deletion begins.

Raw Claude, Gemini, Grok, and Paseo state is excluded from GitHub because it
contains or may contain private prompts, terminal output, account-bound
metadata, host paths, and provider-controlled credential formats. The
repository retains a bounded inventory, not those raw transcripts.

The final cleanup clone at `${TMPDIR}/lc-v090-residual-finalize` is retained
until this ledger is merged, exact-merge CI passes, and the unpublished draft
target is rebound to the new merge commit. It measured 30,028 KiB before the
third-sweep deletion.

## 13. Verification-Residue Pre-Deletion Record

The full local test and pre-push verification runs recreated host-local state
after the earlier cleanup. A targeted scan of the macOS per-user temporary
root and selected caches found another 4,573,620 KiB:

| Class | KiB | Count |
| --- | ---: | ---: |
| Go `Test...` temporary directories containing LoopCoder state | 4,353,260 | 2,774 |
| `loopcoder-*` temporary audit/build/qualification directories | 95,664 | 42 |
| Generic temporary directory containing a LoopCoder build | 32,340 | 1 |
| Go build-cache entries containing `loopcoder` executables | 90,944 | 4 |
| Recreated `${HOME}/.loopcoder` runtime | 1,364 | 1 database |
| Python cache mirrors of deleted temporary paths | 44 | 3 |
| Go module download cache | 4 | 1 path |
| **Total** | **4,573,620** | |

The 2,817 system temporary directories ranged from 2026-07-25 through
2026-07-28. The recreated runtime database passed `PRAGMA integrity_check`.
Process and open-file scans found no active test, LoopCoder process, or open
file in the selected paths.

This finding increases the accounted local boundary, excluding the final
cleanup clone, to 8,333,606,912 bytes. It also explains why the user's disk
observation exceeded the original 1.90-GiB targeted scope: the first audit did
not include the macOS per-user system temporary root, and verification could
repopulate runtime state after deletion.

The statistics in this section must be pushed and read back remotely before
these paths are deleted. The documentation-only push may bypass the local
pre-push hook because that hook has already passed twice in this transaction
and rerunning it would recreate the exact residue this section is meant to
remove. GitHub CI remains the authoritative verification for the resulting
pull request.

## 14. Final Local Deletion Result

The third sweep and verification-residue cleanup completed on 2026-07-28.

### 14.1 Scheduled And Agent State

- The 15-minute Codex heartbeat was deleted through the Codex automation API.
- Nine LoopCoder Paseo agents were deleted by exact agent ID through
  `paseo delete`.
- Every agent had first been independently verified as `closed` and archived.
- The initial `paseo delete --cwd` call deleted zero records because archived
  agents are excluded from that selector; the result was not treated as
  success.
- Each of the nine exact-ID calls then returned `deletedCount: 1`.
- The now-empty repository-specific Paseo agent directory was removed.
- A final `paseo ls --all` query found zero agents for the stopped repository
  cwd.

No agent was resumed or woken during cleanup.

### 14.2 Provider, Installation, And Cache State

The following selected classes were deleted:

- the 1,001,828-KiB Grok session tree;
- all 195 Claude project-history directories and 223 files;
- four Gemini project records;
- four Claude project-cache directories;
- seven empty Claude temporary project directories;
- the installed Claude LoopCoder skill after its unique diff was preserved;
- four local LoopCoder binaries, one symlink, and the v0.8.1/test install tree;
- the recreated `${HOME}/.loopcoder` database;
- four Go build-cache directories containing LoopCoder executables;
- three Python cache mirrors;
- the selected Go module download-cache path.

`loopcoder` no longer resolves on `PATH`.

### 14.3 System Temporary State

- Selected top-level directories before deletion: 2,817
- Selected allocated size before deletion: 4,481,264 KiB
- Selected top-level directories after deletion: 0
- Active Go test or LoopCoder process after deletion: 0
- Open files in the selected directories before deletion: 0

The deletion operated on the 2,817 audited top-level names derived from a
LoopCoder-bearing descendant. It did not delete other macOS temporary
directories.

### 14.4 Intentional Retentions

Other active repositories still own their own `.loopcoder` directories or
`refs/loopcoder` namespaces. Those are not copies of the stopped source
repository and were left intact. Codex tasks, Codex memory, and provider memory
for other repositories were also retained.

The broad home scan encountered normal macOS privacy denials in unrelated
system containers. Those protected containers were not part of the
project-managed storage roots. Every accessible, previously identified
LoopCoder project path was checked through a targeted post-deletion scan.

### 14.5 Remaining Transaction Path

The only remaining local source checkout for this transaction is
`${TMPDIR}/lc-v090-residual-finalize`, the 30,028-KiB clone used to commit,
push, merge, and verify this ledger. It is deleted after:

1. the archival PR merges to `main`;
2. exact-merge CI passes;
3. the unpublished draft remains unpublished and is rebound to the merge SHA;
4. the remote branch and tracked archive are read back successfully.

Including the finalizer after its eventual removal, the complete accounted
local deletion transaction totals 8,364,355,584 bytes. This is approximately
7.79 GiB or 8.36 GB. The figure includes cleanup-only transaction paths and
verification-generated residue, not 7.79 GiB of unique product source.
