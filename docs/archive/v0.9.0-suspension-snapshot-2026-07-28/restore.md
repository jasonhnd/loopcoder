# Future Inspection and Recovery Guide

This guide explains how an explicitly authorized future operator can inspect
the suspension snapshot. It is not authorization to resume development or
publish a release.

## 1. Required Decision Before Recovery

Do not begin by running RC61.

First record a new owner decision that states:

- the new product objective;
- whether the goal is audit, extraction, redesign, or resumed development;
- the time and provider-cost budget;
- the acceptance contract;
- the stop-loss checkpoints;
- whether the starting source is v0.8.1, the stopped v0.9 source, or a selected
  patch subset.

No public tag or release may be created without a separate publication GO
after fresh acceptance.

## 2. Establish Current Remote State

The values in this snapshot are historical. Re-query current state:

```bash
gh repo view jasonhnd/loopcoder \
  --json defaultBranchRef,isArchived,visibility,url

git ls-remote --heads --tags \
  https://github.com/jasonhnd/loopcoder.git

gh issue list --repo jasonhnd/loopcoder --state all --limit 200
gh pr list --repo jasonhnd/loopcoder --state all --limit 200
gh release list --repo jasonhnd/loopcoder --limit 100
```

Compare the result with [`asset-manifest.json`](asset-manifest.json). Do not
assume branches, issues, workflow artifacts, provider catalogs, or draft
release behavior remained unchanged.

## 3. Locate The Unpublished Draft

The snapshot uses the internal lookup label
`internal-snapshot-v090-20260728`. It is an unpublished draft, not a Git tag.
Authenticated repository administration access may be required to see it.

```bash
gh api repos/jasonhnd/loopcoder/releases \
  --jq '.[] |
    select(.tag_name == "internal-snapshot-v090-20260728") |
    {
      id,
      name,
      tag_name,
      draft,
      prerelease,
      published_at,
      assets: [.assets[] | {id,name,size,digest}]
    }'
```

Expected safety state:

- `draft: true`;
- `prerelease: true`;
- `published_at: null`;
- no corresponding `refs/tags/internal-snapshot-v090-20260728`;
- no v0.9 product tag;
- no v0.9 public release.

If the draft was published or a tag ref exists, stop and investigate before
using any asset.

## 4. Download Assets By Asset ID

The API path is preferable to a browser download because it preserves an
authenticated, scriptable boundary:

```bash
mkdir -p snapshot-download

release_id="$(
  gh api repos/jasonhnd/loopcoder/releases \
    --jq '.[] |
      select(.tag_name == "internal-snapshot-v090-20260728") |
      .id'
)"

gh api "repos/jasonhnd/loopcoder/releases/${release_id}" \
  --jq '.assets[] | [.id,.name] | @tsv'
```

For each asset:

```bash
gh api \
  -H "Accept: application/octet-stream" \
  "repos/jasonhnd/loopcoder/releases/assets/ASSET_ID" \
  > "snapshot-download/ASSET_NAME"
```

Do not use signed browser URLs as durable evidence. They expire.

## 5. Verify Downloaded Bytes

Use the tracked checksum record, not a checksum copied from a chat or old
terminal:

```bash
cp docs/archive/v0.9.0-suspension-snapshot-2026-07-28/SHA256SUMS \
  snapshot-download/

cd snapshot-download
shasum -a 256 -c SHA256SUMS
```

Expected digests:

| Asset | SHA-256 |
| --- | --- |
| RC60 | `270870d7ae0b1712d6eb3cbf67d948fdc680b0da4aceaf5344a08c03b70e7aae` |
| RC61 | `bea1a17c1e1ef500569ce2283128646c8337a2191c9639921580fbc9d7ac8db1` |
| Evidence | `ea0bd8ca4654c1e5f04b6d2067a61bc6d62dc8c2d825d7d09d47628082c1c52d` |

Stop on any mismatch. Do not rebuild an archive and assign it the historical
identity.

## 6. Inspect Without Executing

List RC contents:

```bash
tar -tzf loopcoder_0.9.0-rc.60_darwin_arm64.tar.gz
tar -tzf loopcoder_0.9.0-rc.61_darwin_arm64.tar.gz
```

Inspect Go build metadata:

```bash
mkdir rc61
tar -xzf loopcoder_0.9.0-rc.61_darwin_arm64.tar.gz -C rc61
go version -m rc61/loopcoder
codesign -dvv rc61/loopcoder
```

Expected RC61 candidate revision:

```text
30cabdaf77d749c8305349f7f6a87189014b8af8
```

This proves archive identity, not product acceptance.

## 7. Inspect The Evidence Archive

```bash
zstd -t v0.9.0-suspension-evidence-2026-07-28.tar.zst
zstd -dc v0.9.0-suspension-evidence-2026-07-28.tar.zst |
  tar -tf -
```

Extract into a disposable directory:

```bash
mkdir evidence
zstd -dc v0.9.0-suspension-evidence-2026-07-28.tar.zst |
  tar -xf - -C evidence
```

Read in this order:

1. `evidence/metadata/redaction-report.json`;
2. `evidence/evidence/rc60/qualification/qualification-evidence.json`;
3. `evidence/evidence/rc60/canary-evidence-acceptance2.json`;
4. `evidence/evidence/rc60/formal-acceptance2-attempt1.json`;
5. `evidence/evidence/rc60/formal-acceptance2-resume.json`;
6. `evidence/evidence/rc60/qualification/capacity-ledger.json`;
7. `evidence/evidence/rc61/provider-refresh.json`;
8. `evidence/evidence/metadata/unmerged-commit-metadata.tsv`;
9. the local and dirty-clone patches.

Do not reverse the `NO_GO` decision by selecting only passing fields. The
qualifier's failed and `not_run` metrics are the binding interpretation of the
RC60 evidence.

## 8. Recover Source For Audit

The stopped source remains in normal Git history:

```bash
git clone https://github.com/jasonhnd/loopcoder.git
cd loopcoder
git switch --detach 30cabdaf77d749c8305349f7f6a87189014b8af8
```

Verify ancestry and objects before analysis:

```bash
git cat-file -t 30cabdaf77d749c8305349f7f6a87189014b8af8
git show --stat --oneline \
  30cabdaf77d749c8305349f7f6a87189014b8af8
```

The documentation commits after `30cabdaf...` are not product implementation
changes. They record suspension and archival state.

## 9. Review Preserved Patches

Never apply all patches as a batch merely because they were preserved.

For each branch:

1. read the mail patches in listed order;
2. compare each patch with current source;
3. identify whether equivalent behavior was merged by another route;
4. assess coupling to the stopped architecture;
5. create a disposable review branch;
6. apply with three-way conflict detection;
7. run fresh tests and product-level acceptance;
8. open a normal issue and PR if the change is still wanted.

Example inspection:

```bash
git apply --stat issue-1337-d036a1f.patch
git apply --check issue-1337-d036a1f.patch
```

Example isolated recovery, only after review:

```bash
git switch -c audit/recover-issue-1337
git am --3way issue-1337-d036a1f.patch
git am --3way issue-1337-89d15db.patch
```

The dirty-clone patches do not carry approved commit history. Treat them as
forensic diffs.

## 10. Data That Cannot Be Restored

The cleanup intentionally does not preserve:

- process IDs or live process groups;
- machine-local SQLite runtime state;
- active provider sessions;
- OAuth credentials;
- provider-home contents;
- historical quota windows as usable current inventory;
- private disposable repository checkout;
- stale worktree registrations;
- dependency caches;
- old extracted executables;
- unreachable Git objects without an approved ref.

This data was unsafe, perishable, redundant, or not authoritative. Its absence
is part of the cleanup design.

## 11. Fresh Evidence Required For Any Resume

A future resume must freshly establish:

- provider CLI versions;
- authentication readiness;
- account identity binding;
- current model catalogs and depth support;
- current quota windows and reset times;
- current GitHub permissions and protected environments;
- current branch protection and required checks;
- exact qualifier schema;
- one new disposable consumer repository;
- one new exact artifact;
- one complete live canary;
- `not_run=0`;
- explicit owner GO.

Historical RC60 or RC61 evidence may inform design. It may not satisfy a future
acceptance gate.

## 12. Safe End State

After an audit, remove the disposable download and checkout unless a new owner
decision makes them part of an approved workstream. The durable copy remains
the authenticated GitHub draft plus the tracked manifests in this directory.
