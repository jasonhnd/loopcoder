# Local Cleanup Ledger

**Ledger opened:** 2026-07-28
**Operation:** v0.9 project suspension archive and local deletion
**Initial local scope:** 2,035,732,480 bytes
**Safety model:** Preserve, upload, download-verify, then delete
**Current phase:** Preservation prepared; deletion not yet started

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
| Documentation branch | Pushed to `jasonhnd/loopcoder` | Pending |
| Pull request | Open against `main` | Pending |
| Draft snapshot | Exists and `draft=true` | Pending |
| Draft publication | `published_at=null` | Pending |
| Product tag | No v0.9 tag | Verified before branch creation |
| Internal lookup ref | No Git tag ref | Verified before branch creation |
| RC60 upload | Size and SHA-256 match | Pending |
| RC61 upload | Size and SHA-256 match | Pending |
| Evidence upload | Size and SHA-256 match | Pending |
| Manifest upload | Downloaded JSON equals local file | Pending |
| Checksum upload | Downloaded file validates all assets | Pending |
| Process audit | No LoopCoder/Paseo development or canary process | Verified before asset preparation; must recheck |
| Worktree audit | No unique unpreserved tracked/untracked changes | Verified |
| Secret scan | Public-safe evidence only | Verified locally; must recheck downloaded assets |

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

Pending remote upload and independent download verification.

No local deletion is authorized by this ledger until the gates in section 2
are updated with confirmed results.
