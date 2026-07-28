# LoopCoder v0.9.0 Suspension Snapshot

**Snapshot date:** 2026-07-28
**Repository:** `jasonhnd/loopcoder`
**Development state:** Suspended
**Product decision:** `NO_GO` / `NOT_RELEASED`
**Latest public release:** `v0.8.1`
**Publication authority:** None
**Snapshot form:** Unpublished GitHub draft with authenticated assets

> This snapshot is a preservation and cleanup record. It is not a v0.9.0
> release, release candidate approval, installation recommendation, or
> authorization to resume development.

## 1. Why This Snapshot Exists

The
[`v0.9.0 development suspension report`](../../v0.9.0-development-suspension-report-2026-07-26.md)
is the authoritative explanation of what the project attempted, what was
built, why the complete product objective was not achieved, what the failed
acceptance evidence proved, and what must change before any future attempt.

This directory answers a different operational question:

> What must be retained on GitHub so the local v0.9 development estate can be
> removed without losing useful, unique, or decision-relevant evidence?

The initial targeted local estate had grown to 2,035,732,480 bytes across:

- one central Git controller;
- 22 registered development worktrees;
- four independent historical clones;
- 57 top-level temporary paths;
- repeated RC archives and extracted binaries;
- audit and smoke-test builds;
- runtime databases, provider-home fixtures, caches, logs, and zero-byte
  placeholders.

Most of that footprint was not unique product state. It was the accumulated
cost of repeated build, extract, smoke, audit, canary, and qualification
cycles. Keeping all of it would make future recovery less trustworthy, not
more trustworthy: a future operator would have to distinguish dozens of stale
and partially overlapping local states before reaching the few records that
actually explain the stop.

After the primary cleanup, a broader home-directory scan found another
252,332 KiB of historical checkouts, hidden runtime homes, caches, and host
metadata outside the initial scope. That second sweep is documented in
[`residual-local-state-inventory.md`](residual-local-state-inventory.md).
A final full-name scan then found another 1,200,932 KiB, dominated by raw Grok
session and terminal history, plus Claude/Gemini histories, local binary
installations, caches, closed Paseo records, and one recurring automation.
That third sweep is documented in the same inventory.

The snapshot therefore follows a strict rule:

1. preserve unique source differences;
2. preserve the last artifact and the nearest failed acceptance artifact;
3. preserve raw decision evidence in a public-safe form;
4. preserve exact digests and remote identities;
5. discard reproducible, duplicate, stale, private, or structurally
   untrustworthy local state.

## 2. Authority and Source Bindings

The snapshot distinguishes four identities that must not be conflated:

| Identity | Exact value | Meaning |
| --- | --- | --- |
| Stopped product `pre-prod` | `30cabdaf77d749c8305349f7f6a87189014b8af8` | Final source candidate that produced RC61 |
| RC60 candidate | `4361b7bad4cbba897cd3069a81692aec6bad5f0c` | Nearest candidate to complete live acceptance; qualifier returned `NO_GO` |
| Documentation `pre-prod` before snapshot | `017e450e3a503ab55c5bd3d6b21268d167368e51` | Stop-report state after product development had ended |
| `main` before snapshot PR | `40bc76eaa13d7cdc113c0f17950239819141e411` | Public repository state containing the suspension report |

RC61's GitHub workflow metadata used workflow-definition head
`9646de33ed38189c74a13e8609d5811d83b58bad`. The archive manifest, binary build
metadata, and digest bind the candidate itself to `30cabdaf...`. The workflow
definition head is not a substitute for the candidate source identity.

## 3. Durable Draft Assets

The assets are attached to the unpublished draft named
`Internal suspension snapshot 2026-07-28 (not a release)`, database ID
`360850177`. After the draft target was rebound to the archival merge commit,
GitHub represented its draft-only tag label as
`untagged-c2dee6e6a6773d7d9781`.

That opaque label is not a Git ref or product version and must not be converted
into a Git tag. The draft must remain unpublished unless the owner separately
authorizes a different archival action. In particular, it must never be
published as a v0.9 release.

| Asset | Bytes | SHA-256 | Why retained |
| --- | ---: | --- | --- |
| `loopcoder_0.9.0-rc.60_darwin_arm64.tar.gz` | 9,264,799 | `270870d7ae0b1712d6eb3cbf67d948fdc680b0da4aceaf5344a08c03b70e7aae` | Exact archive used by the nearest completed live acceptance attempt |
| `loopcoder_0.9.0-rc.61_darwin_arm64.tar.gz` | 9,271,775 | `bea1a17c1e1ef500569ce2283128646c8337a2191c9639921580fbc9d7ac8db1` | Final built archive; canary and qualifier were never run |
| `v0.9.0-suspension-evidence-2026-07-28.tar.zst` | 187,791 | `ea0bd8ca4654c1e5f04b6d2067a61bc6d62dc8c2d825d7d09d47628082c1c52d` | Sanitized acceptance, source-delta, inventory, and cleanup evidence |
| `snapshot-manifest.json` | See draft asset | Bound by the draft `SHA256SUMS` asset | Machine-readable source, artifact, redaction, and cleanup scope |
| `SHA256SUMS` | See draft asset | Self-excluded checksum list | Offline verification for the other four assets |

The tracked [`asset-manifest.json`](asset-manifest.json) records the same
artifact names, sizes, and digests. The exact `SHA256SUMS` file remains a draft
asset and is independently bound by GitHub's asset digest.

## 4. Evidence Archive Contents

The evidence archive contains 61 sanitized text files and no runtime database,
credential store, provider session, extracted executable, or private Git
repository.

### 4.1 RC60 evidence

RC60 is retained because it is the nearest attempt that exercised the complete
shape of the intended acceptance flow. The archive includes:

- candidate manifest, checksum record, and SPDX SBOM;
- GitHub workflow and artifact metadata;
- fresh provider inventory and account-bound observation output;
- Claude paid model-probe receipt and stderr;
- first-run and resume output for the second acceptance attempt;
- pre-resume event counts and event-log digest;
- final canary evidence envelope;
- exact qualifier output and stderr;
- release qualification evidence and scorecard;
- capacity ledger plus capacity and exhausted snapshots;
- export/import evidence;
- UI latency report and stage receipt.

The retained evidence does not turn RC60 into an accepted artifact. Its
qualifier decision is `NO_GO`. Six required metrics were `not_run`, and raw
capacity, unavailable-route, and forced-restart evidence did not satisfy the
qualifier.

### 4.2 RC61 evidence

RC61 is retained because it was the last exact archive built before the owner
stopped development. The archive includes:

- candidate manifest, checksum record, and SPDX SBOM;
- GitHub workflow and artifact metadata;
- the final provider refresh report and stderr.

There is intentionally no RC61 canary or qualifier evidence. Those stages
never began. Absence is the truthful state.

### 4.3 Unique local source differences

The local Git controller contained 148 branch refs. Patch-equivalence analysis
found that nine branch tips were already represented by `origin/main`. Three
branches contained seven commits that were not patch-equivalent to
`origin/main`:

| Branch | Preserved commits |
| --- | --- |
| `ordinary/issue-1337` | `d036a1f...`, `89d15db...` |
| `ordinary/issue-1338` | `7d3d69c...`, `d3420c7...`, `9657a7a...` |
| `ordinary/issue-1339` | `d38a9d3...`, `18310ec...` |

Each commit is preserved as an ordered mail patch with commit metadata. The
patches are evidence and optional recovery material; they are not approved
changes and must not be applied automatically during a future resume.

### 4.4 Dirty independent clones

Two independent clones contained tracked modifications:

- `gateA`: one `.delivery.yml` difference, preserved as a 5,320-byte binary
  patch;
- `gateA-r02`: ten tracked-file differences, preserved as a 29,849-byte binary
  patch, plus six small run/recovery evidence files.

Two other independent clones were clean historical ancestors of `main` and
needed no source-delta preservation.

### 4.5 Local inventory

The archive also contains sanitized inventories for:

- local branch refs;
- registered worktrees;
- independent clone status;
- remote refs and release state before the snapshot;
- top-level local and temporary path sizes;
- temporary files larger than 8 MiB;
- Git unreachable-object counts;
- the exact cleanup scope.

These inventories replace local absolute paths with `${HOME}` and `${TMPDIR}`.

### 4.6 Broad residual sweep

The final broad scan found three historical product notes and two code commits
that existed only in an older local checkout. They are retained in
[`historical/`](historical/), with exact sizes and SHA-256 values.

The same scan found six hidden runtime homes containing 5,563 files and six
valid SQLite databases. Raw databases, provider sessions, logs, and temporary
worktrees were excluded for privacy and trust reasons. Their bounded
statistics, Git disposition, and deletion rationale are retained in
[`residual-local-state-inventory.md`](residual-local-state-inventory.md).

The final scan found 1,001,828 KiB of Grok session and terminal history, 195
Claude project-history directories, four small Gemini records, nine closed and
archived Paseo agent records, local LoopCoder binaries and install copies, and
the installed Claude LoopCoder skill. Raw histories and binaries were excluded.
One 3,369-byte path-sanitized skill diff was the only unique text delta and is
retained under [`historical/local-tooling`](historical/local-tooling).

## 5. What Was Deliberately Excluded

The following content was reviewed and deliberately excluded:

| Excluded class | Reason |
| --- | --- |
| 22 worktree directories | No tracked changes and no meaningful untracked files; 19 contained only `.DS_Store` |
| Nine patch-equivalent branch tips | Source already represented by `origin/main` |
| 510 unreachable commits, 409 blobs, 1,206 trees, four tags | Unreachable Git garbage, not an approved recovery boundary |
| Repeated extracted RC binaries | Reproducible from retained archives and already digest-bound |
| Older RC archives | Superseded debugging artifacts; the decision-relevant RC60 and final RC61 are retained |
| Audit and smoke-test binaries | Reproducible build products, not source or release evidence |
| Runtime SQLite databases | Machine-local mutable state with privacy and consistency risk |
| Provider homes and sessions | Account-bound, perishable, and not safe public evidence |
| Claude, Gemini, and Grok transcript histories | May contain prompts, terminal output, account context, local paths, and provider-controlled secrets |
| Closed Paseo agent index | Host-local orchestration metadata; all nine records were verified closed and archived before deletion |
| Dependency and tool caches | Reproducible and stale by definition |
| Temporary Git repositories | Fixtures or disposable consumer repositories |
| Zero-byte placeholders | No information content |
| Private repository names and URLs | Removed by the public-safety policy |
| Provider credentials, OAuth data, emails, machine IDs | Removed or pseudonymized; never stored as raw durable evidence |
| Hidden `$LOOPCODER_HOME` variants | Summarized by count, size, date range, and integrity result; raw cross-project and provider-session state excluded |

This is a semantic retention decision, not a byte-for-byte backup. The project
can be audited and selectively resumed from GitHub, but the deleted local
runtime cannot and should not be reconstructed exactly.

## 6. Why The Local Footprint Reached 1.9 GiB

The footprint was not 1.9 GiB of unique source code.

- 585,072 KiB was under the local development root: the Git controller,
  registered worktrees, and independent clones.
- 1,402,948 KiB was under temporary storage.
- 61 temporary files larger than 8 MiB accounted for 1,249,121,008 bytes.
- 38 files at least 20 MB accounted for 1,037,544,796 bytes.

The dominant class was repeated Mach-O executables: release archives, extracted
archives, audit builds, smoke builds, and copied canary binaries. Git
worktrees and clones added another large layer because the same repository
history and checkout contents were present in many paths.

This explains why exact-byte deduplication alone would not have solved the
problem. Many binaries differed because each RC was built from a different
commit. They were different bytes but served the same obsolete debugging role.
The correct cleanup unit was semantic usefulness, not hash equality.

The later broad sweep added approximately 246.4 MiB. Its largest classes were
hidden bootstrap/runtime homes and two old checkouts, not additional release
artifacts. The three unique Markdown notes and two local-only code diffs were
less than 173 KB in total and are now tracked under `historical/`.

The final full-name sweep added approximately 1.15 GiB. About 978 MiB of that
was Grok session and terminal history; approximately 164 MiB was installed
LoopCoder binaries and v0.8.1/test copies; about 34 MiB was Claude project
history. These were accumulated operational copies, not unique source. The
only additional unique public-safe text was the small installed-skill diff.

Across the original estate, cleanup transaction paths, second sweep, and third
sweep, the accounted local boundary is 3,650,220,032 bytes. The final
30,028-KiB residual cleanup clone is removed only after its documentation PR is
merged and exact-merge verification succeeds; it is cleanup transaction
overhead, not retained project state.

## 7. Redaction and Safety Boundary

The evidence archive was transformed before upload:

- secret-valued JSON fields were removed;
- account, machine, device, and user identifiers were replaced by stable
  SHA-256 pseudonyms;
- email addresses were removed;
- home and temporary paths were replaced by `${HOME}` and `${TMPDIR}`;
- private disposable repository names were removed;
- common GitHub, OpenAI, Anthropic, xAI, Slack, bearer, JWT, and signed-query
  token shapes were scanned.

All JSON documents parsed successfully after transformation. No parser
fallback was used. See [`redaction-report.json`](redaction-report.json) and
[`redaction.md`](redaction.md).

The RC binaries cannot be text-transformed without invalidating their exact
digests. They were therefore scanned separately. Their Go build metadata uses
`-trimpath=true`; no actual local username or recognized credential shape was
found. Both binaries contain the repository's intentional synthetic privacy
test marker `/Users/syn-private/v090067/SECRET_PATH_DDDD`. That string is
public source, not host data.

## 8. Repository State At Archival Time

At the snapshot audit:

- there were no open issues;
- there were no open pull requests;
- release blockers that were open at the original stop were subsequently
  closed as `not planned` or completed as part of the project suspension;
- PR #1450 was closed;
- no v0.9 tag existed;
- no v0.9 public release existed;
- `v0.8.1` remained the latest public release.

The archival PR #1454 subsequently merged to
`19d25dbf230482564173c7eb6eb7c7d1de5f189f`. Exact-SHA `main` CI run
`30332243077` passed `verify`, `test`, `race`, and `security`. The unpublished
draft target was rebound to that merge commit.

Closing an issue as part of project suspension does not prove its acceptance
criteria were met. The historical issue state and the product verdict must be
read separately.

## 9. Cleanup Order

The cleanup was designed to fail safely:

1. generate sanitized evidence;
2. calculate local digests;
3. commit the documentation and tracked manifests;
4. create an unpublished draft in `jasonhnd/loopcoder`;
5. upload all assets;
6. download every uploaded asset into a separate verification directory;
7. compare downloaded bytes and SHA-256 digests with the tracked manifest;
8. confirm the draft remains unpublished and creates no Git tag ref;
9. remove registered worktrees, independent clones, controller, and temporary
   paths;
10. record the actual deletion and post-cleanup scan in
    [`cleanup-ledger.md`](cleanup-ledger.md);
11. merge the documentation PR to `main`;
12. delete the temporary finalization clone and staging directory.
13. run a broader home-directory scan;
14. preserve newly discovered unique notes and source diffs;
15. delete the newly inventoried hidden runtime, checkout, cache, and
    host-metadata residuals.

Local deletion must not begin before step 8 succeeds.

## 10. Future Recovery

Recovery instructions are in [`restore.md`](restore.md). Any future operator
must begin with the suspension report, not by running RC61.

The retained archive supports:

- verifying exact historical artifact identity;
- reading the failed RC60 qualifier and raw evidence;
- inspecting RC61's final inventory state;
- reviewing source changes that existed only in local refs or dirty clones;
- deciding whether to discard, extract, or redesign the stopped architecture.

It does not support:

- treating historical provider inventory as current;
- restoring provider sessions or quota;
- recreating the deleted private consumer repository;
- claiming that RC60 or RC61 passed acceptance;
- publishing v0.9.0;
- resuming without a new owner decision, budget, frozen acceptance contract,
  and stop-loss rule.

## 11. Final Boundary

After this snapshot, the durable v0.9 record lives in
`jasonhnd/loopcoder`:

- source history;
- merged suspension report;
- this archival index;
- tracked manifests and the draft checksum asset;
- historical source notes and local-only code diffs found by the final sweep;
- an unpublished, authenticated draft containing the bounded binary and
  evidence assets.

No local LoopCoder development checkout, worktree, clone, RC directory, or
temporary evidence path is authoritative after cleanup.
