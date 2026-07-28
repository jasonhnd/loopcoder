# Local Cleanup Ledger

**Ledger opened:** 2026-07-28
**Operation:** v0.9 project suspension archive and local deletion
**Initial local scope:** 2,035,732,480 bytes
**Safety model:** Preserve, upload, download-verify, then delete
**Current phase:** Remote preservation verified; local deletion authorized

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
- the current unrelated Codex workspace;
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
