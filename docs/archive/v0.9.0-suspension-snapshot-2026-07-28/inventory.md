# Suspension Snapshot Inventory

This inventory records the exact preservation boundary used before deleting
the local v0.9 development estate. It should be read with
[`README.md`](README.md), [`asset-manifest.json`](asset-manifest.json), and the
authoritative
[`development suspension report`](../../v0.9.0-development-suspension-report-2026-07-26.md).

## 1. Inventory Rules

Every local item was classified as one of:

- **durable public source**: already reachable from `jasonhnd/loopcoder`;
- **unique source delta**: not reachable or patch-equivalent to `origin/main`;
- **decision evidence**: needed to explain `NO_GO`, the final candidate, or the
  cleanup;
- **reproducible output**: rebuildable from source and not independently useful;
- **perishable runtime state**: account-, process-, machine-, or time-bound;
- **private material**: unsafe to retain publicly without transformation;
- **empty or redundant**: no information beyond a retained item.

Only the first three classes were retained. Private material within decision
evidence was transformed into a public-safe representation before upload.

## 2. Local Development Root

The local development root contained six LoopCoder top-level paths totaling
585,072 KiB.

| Sanitized path | KiB | Classification | Disposition |
| --- | ---: | --- | --- |
| `${HOME}/AgenticCoder/loopcoder` | 54,520 | Central bare controller plus local refs and unreachable objects | Preserve unique patches and ref inventory; delete controller |
| `${HOME}/AgenticCoder/loopcoder-worktrees` | 447,156 | 22 registered worktrees | Delete after clean-status proof |
| `${HOME}/AgenticCoder/loopcoder-v090` | 23,112 | Clean independent clone at `dcdf608...` | Delete; ancestor of `main` |
| `${HOME}/AgenticCoder/loopcoder-v090-visibility` | 12,528 | Clean independent clone at `8ae110f...` | Delete; ancestor of `main` |
| `${HOME}/AgenticCoder/loopcoder-v090-gateA` | 23,672 | Dirty historical Gate A clone at `7a2117c...` | Preserve patch; delete clone |
| `${HOME}/AgenticCoder/loopcoder-v090-gateA-r02` | 24,084 | Dirty historical Gate A retry clone at `ea834f7...` | Preserve patch and six run records; delete clone |

The controller was configured as a bare repository even though its directory
also contained historical checkout files. It was the Git common directory for
the 22 registered worktrees and therefore had to be removed last.

## 3. Registered Worktrees

Git reported 23 worktree records:

- one central controller record;
- 22 linked development worktrees.

The 22 linked worktrees had:

- zero tracked modifications;
- zero meaningful untracked files;
- zero meaningful ignored files;
- 19 `.DS_Store` files in total.

The branches represented issue work from #1397 through #1447 plus the
suspension-report and stopped-main-promotion branches. Their durable commits
were already present in the remote repository. The worktree directories
therefore contained no unique source state.

The sanitized machine record is
`evidence/metadata/registered-worktree-status-before.txt` inside the evidence
asset.

## 4. Local Git Refs

The central controller had:

- 148 local branch refs;
- 176 remote-tracking refs;
- 26 tags;
- 247 commits not directly reachable from `origin/*` before patch-equivalence
  analysis.

Commit reachability alone overstated unique work because many local commits
had been rebased, squash-merged, or otherwise represented by equivalent
patches on `origin/main`.

### 4.1 Patch-equivalent branch tips

Nine branch tips had no unique patch relative to `origin/main`:

- `codex/issue-1058-depowershell`;
- `codex/v090-roadmap`;
- `ordinary/issue-1334`;
- `ordinary/issue-1335`;
- `ordinary/issue-1336`;
- `ordinary/issue-1340`;
- `ordinary/issue-1341`;
- `ordinary/issue-1342`;
- `ordinary/issue-1343`.

Their local refs were not retained separately.

### 4.2 Non-equivalent commits

Seven commits on three branches were not patch-equivalent to `origin/main`:

| Branch | Commit | Subject | Archive file |
| --- | --- | --- | --- |
| `ordinary/issue-1337` | `d036a1f...` | `feat(run): wire auto-route to live capacitysnapshot inventory (#1337)` | `issue-1337-d036a1f.patch` |
| `ordinary/issue-1337` | `89d15db...` | `fix(capacitysnapshot): remove unused accKey type for staticcheck` | `issue-1337-89d15db.patch` |
| `ordinary/issue-1338` | `7d3d69c...` | `feat(routing): reconcile model maps and depth policy without universal high (#1338)` | `issue-1338-7d3d69c.patch` |
| `ordinary/issue-1338` | `d3420c7...` | `fix(capacitysnapshot): drop unused pickDefaultDepth after depthpolicy wire` | `issue-1338-d3420c7.patch` |
| `ordinary/issue-1338` | `9657a7a...` | `test: update agent/cli expectations for medium depth defaults` | `issue-1338-9657a7a.patch` |
| `ordinary/issue-1339` | `d38a9d3...` | `feat(run): classify task requirements into route class and depth (#1339)` | `issue-1339-d38a9d3.patch` |
| `ordinary/issue-1339` | `18310ec...` | `test(cli): allow preflight block after auto-route selection in hermetic env` | `issue-1339-18310ec.patch` |

The patches include parent identity and commit metadata. A future audit must
review them individually against current source; order within each branch
matters.

### 4.3 Unreachable object garbage

`git fsck --full --unreachable --no-reflogs` found:

| Object type | Count |
| --- | ---: |
| Commits | 510 |
| Blobs | 409 |
| Trees | 1,206 |
| Tags | 4 |

These objects had no approved ref, no identified recovery requirement, and no
safe semantic boundary. They were recorded by object ID in the sanitized
inventory and then discarded with the local controller.

## 5. Dirty Independent Clone Deltas

### 5.1 Gate A

Base commit: `7a2117c7f5656e48bbb4ab67978283cfa4f69d3e`

Tracked difference:

- `.delivery.yml`

Preserved form:

- `evidence/dirty-worktrees/gateA.patch`;
- 5,320 bytes after path and identity sanitation.

Ignored or untracked `.DS_Store` files were excluded.

### 5.2 Gate A retry 02

Base commit: `ea834f7ee9a5da49e836403b673f9bdc5483d814`

Tracked differences:

- `.delivery.yml`;
- `internal/agent/grok.go`;
- `internal/agent/grok_test.go`;
- `internal/availability/helpers.go`;
- `internal/availability/scoring.go`;
- `internal/cli/cli.go`;
- `internal/models/registry.go`;
- `internal/providerinventory/inventory.go`;
- `internal/routing/hard_eligibility.go`;
- `internal/taskrequirements/store.go`.

Preserved form:

- `evidence/dirty-worktrees/gateA-r02.patch`;
- 29,849 bytes after sanitation;
- six small `.loopcoder` lifecycle, event, recovery, worker, and relay-receipt
  records totaling less than 13 KiB.

The `.loopcoder` runtime database and any provider credentials were not
present in this preserved subset.

## 6. Temporary Storage

The audit found 57 top-level `${TMPDIR}/loopcoder*` paths totaling
1,402,948 KiB. They contained 3,805 files at the initial audit.

The largest consumers were repeated:

- extracted 20-23 MB Mach-O binaries;
- standalone audit and smoke binaries;
- RC archives;
- RC extraction directories;
- fixture and disposable Git repositories;
- dependency and module caches;
- runtime databases;
- provider refresh output;
- canary and qualifier evidence;
- empty stdout, stderr, lock, and placeholder files.

Sixty-one files larger than 8 MiB accounted for 1,249,121,008 bytes. Thirty-
eight files at least 20 MB accounted for 1,037,544,796 bytes. This is why the
temporary footprint dominated the total.

### 6.1 Retained temporary products

Only these semantic products were retained:

- RC60 archive;
- RC61 archive;
- selected RC60 decision evidence;
- selected RC61 final inventory evidence;
- sanitized top-level and large-file inventories.

### 6.2 Deleted temporary products

The cleanup removes:

- all older RC archives and extracts;
- duplicate RC60/RC61 extracts;
- test and audit executables;
- fixture repositories;
- private disposable consumer repository checkout;
- runtime SQLite databases;
- provider-home fixtures and session records;
- Grok local documentation cache;
- dependency and module caches;
- old logs not selected for the evidence archive;
- zero-byte placeholders.

## 7. RC60 Inventory

| Field | Value |
| --- | --- |
| Candidate SHA | `4361b7bad4cbba897cd3069a81692aec6bad5f0c` |
| Workflow run | `30190284334` |
| Actions artifact | `8628327068` |
| Archive digest | `270870d7ae0b1712d6eb3cbf67d948fdc680b0da4aceaf5344a08c03b70e7aae` |
| Install smoke | Pass |
| Live canaries reported | 9/9 |
| Qualifier | Fail |
| Decision | `NO_GO` |
| Required metrics `not_run` | 6 |

The six `not_run` metrics were:

- `capacity_after_runtime`;
- `forced_restart_ceilings`;
- `multi_depth_routing`;
- `multi_provider_execution`;
- `real_pr_human_gate`;
- `unavailable_route_exclude`.

The qualifier also rejected raw capacity and route evidence. Reported canary
activity and an opened PR were not enough because the exact raw evidence did
not satisfy the qualification contract.

## 8. RC61 Inventory

| Field | Value |
| --- | --- |
| Candidate SHA | `30cabdaf77d749c8305349f7f6a87189014b8af8` |
| Workflow definition head | `9646de33ed38189c74a13e8609d5811d83b58bad` |
| Workflow run | `30193014249` |
| Actions artifact | `8629212456` |
| Archive digest | `bea1a17c1e1ef500569ce2283128646c8337a2191c9639921580fbc9d7ac8db1` |
| Provider refresh | Retained, sanitized |
| Live consumer canary | Not run |
| Exact-artifact qualifier | Not run |
| Decision | Unaccepted / `NO_GO` project state |

RC61 must not inherit RC60's partial canary evidence. It is a different
artifact from a different source SHA.

## 9. Remote Repository Inventory

Before the snapshot PR:

| Surface | State |
| --- | --- |
| Repository visibility | Public |
| Default branch | `main` |
| `main` | `40bc76eaa13d7cdc113c0f17950239819141e411` |
| `pre-prod` | `017e450e3a503ab55c5bd3d6b21268d167368e51` |
| Open issues | 0 |
| Open pull requests | 0 |
| v0.9 tags | 0 |
| v0.9 public releases | 0 |
| Latest public release | `v0.8.1` |

The draft snapshot is intentionally not counted as a public release.

## 10. Asset Verification Inventory

Each RC archive contains exactly:

- `loopcoder`;
- `LICENSE`;
- `README.md`.

Both binaries are Darwin arm64 Mach-O executables built with:

- Go `1.26.5`;
- `CGO_ENABLED=0`;
- `-trimpath=true`;
- embedded Git revision;
- ad hoc linker signature;
- no Team ID.

The extracted binaries were not retained separately because they are exactly
recoverable from the retained archives.

## 11. Broad Residual Inventory

The primary inventory was intentionally bounded to the active development root
and temporary estate. A subsequent broad home-directory scan found 252,332 KiB
outside that boundary:

| Class | KiB | Durable result |
| --- | ---: | --- |
| Historical checkouts and linked worktree | 47,132 | Clean or remote-represented; two local-only commit diffs retained |
| Three standalone product/roadmap/postmortem notes | 76 | Exact files retained under `historical/source-notes/` |
| Six hidden runtime homes | 204,996 | Counts and integrity results retained; raw state excluded |
| Go module caches and crash metadata | 128 | Reproducible or host-specific; no raw retention |

The two local-only commit diffs are:

| Local branch | Commit | Diff bytes | SHA-256 |
| --- | --- | ---: | --- |
| `loop/issue-708` | `d73b797800ee2bc04152fbef9f99a0bb1cd61395` | 50,406 | `264adb9151cdc2e53e91b7709ffe78b2a05c1afa63d1b5527abd637a9eb4035c` |
| `loop/issue-711` | `69dc51719875aa3c07b993bd5851ba6a2be01427` | 51,663 | `31b5586ac2f1bc5b957f15a1e33471792cb82ff2ec25bf31049c17732abe9199` |

Neither exact commit was reachable or patch-equivalent to current `main`.
Neither had an associated GitHub pull request. The preserved form deliberately
omits author identity and commit headers.

See
[`residual-local-state-inventory.md`](residual-local-state-inventory.md) and
[`historical/README.md`](historical/README.md) for the complete classification.

## 12. Final Disposition

The retention result is intentionally small:

- approximately 18.7 MB of RC archives;
- approximately 188 KB of compressed decision evidence;
- tracked Markdown, JSON manifests, and redaction records, plus the draft
  `SHA256SUMS` asset;
- 172,655 bytes of historical notes and local-only code diffs discovered in
  the broad residual sweep.

The primary deleted local estate was approximately 2.04 GB. The broad sweep
identified another 252,332 KiB, and the primary transaction used and then
deleted 123,384 KiB of finalization-only storage. The difference between that
local footprint and the retained material is the space occupied by repetition,
mutable runtime state, stale process evidence, caches, and checkout mechanics
rather than unique durable engineering knowledge.
