# loopcoder Documentation

One document, one nature.

Every document is exactly one of these types:

- Frozen specs in [`specs/`](specs/): accepted design records. After merge, a
  later change supersedes a spec with a new spec instead of editing the old
  rationale in place.
- Living reference in [`reference/`](reference/): current behavior and operating
  guidance, kept up to date as the implementation changes.
- Process meta in this directory: workflow, backlog, learnings, and this index.

## Index

- [`PROCESS.md`](PROCESS.md): mandatory doc-first workflow.
- [`BACKLOG.md`](BACKLOG.md): backlog and deferred work.
- [`domains.md`](domains.md): domain profile guidance and docs-domain example.
- [`learnings.md`](learnings.md): append-only operational learnings.
- [`product-evolution.md`](product-evolution.md): historical product decisions from v0.6 through v0.8 and links to their authoritative specs.
- [`self-hosting-playbook.md`](self-hosting-playbook.md): mandatory scope, resource, progress, retry, verification, and cleanup controls for LoopCoder developing itself.
- [`v0.8.0-retrospective.md`](v0.8.0-retrospective.md): sanitized engineering retrospective and corrective-action record for the v0.8.0 cycle.
- [`reference/audit.md`](reference/audit.md): read-only security audit command.
- [`reference/architecture.md`](reference/architecture.md): current system map.
- [`reference/development-release-checklist.md`](reference/development-release-checklist.md): reusable intake-to-publication evidence checklist and GO/NO-GO matrix.
- [`reference/evidence-tiers.md`](reference/evidence-tiers.md): local vs remote evidence ownership, pre-push budget, and required-check discovery.
- [`reference/store-platform-conformance.md`](reference/store-platform-conformance.md): v0.9 compact-store darwin/arm64 boundary and conformance inventory.
- [`architecture/v0.9.0-threat-model.md`](architecture/v0.9.0-threat-model.md): v0.9 threat model, data classes, and capability enforcement inventory.
- [`reference/effective-policy.md`](reference/effective-policy.md): v0.9 effective-policy precedence, freeze, provenance, and digest.
- [`reference/acceptance-harness.md`](reference/acceptance-harness.md): v0.9 deterministic acceptance fixtures and evidence manifests.
- [`reference/authority-store.md`](reference/authority-store.md): v0.9 machine/project store topology and entry points.
- [`reference/v09-home-layout.md`](reference/v09-home-layout.md): global `$LOOPCODER_HOME` layout and owner-only creation.
- [`reference/project-authority-store.md`](reference/project-authority-store.md): project.db domain tables and immutable events.
- [`reference/releasing.md`](reference/releasing.md): release documentation rules.
- [`reference/self-bootstrap.md`](reference/self-bootstrap.md): v0.8.0 self-bootstrap acceptance checklist.
- [`reference/stability-policy.md`](reference/stability-policy.md): 0.x stability policy.
- [`reference/storage-migration.md`](reference/storage-migration.md): v0.7-to-v0.8 storage planning, backup, and rollback contract.
- [`reference/task-requirement-classification.md`](reference/task-requirement-classification.md): v0.8 planner task requirement classification.
- [`reference/v0.8.0-capability-matrix.md`](reference/v0.8.0-capability-matrix.md): binding v0.8.0 capability, reachability, evidence, and production-support status.
- [`reference/v0.8.0-go-no-go.md`](reference/v0.8.0-go-no-go.md): current v0.8.0 release evidence and go/no-go record.
- [`reference/v0.7.0-go-no-go.md`](reference/v0.7.0-go-no-go.md): completed historical v0.7.0 release record.
- [`reference/worker.md`](reference/worker.md): current worker adapter behavior.
- [`reference/usage.md`](reference/usage.md): setup and end-to-end usage.
- [`specs/`](specs/): accepted specs and historical design records.

## Spec Convention

Specs use this path shape:

```text
docs/specs/<NNNN>-<kebab-slug>.md
```

`<NNNN>` is the originating GitHub issue number, zero-padded to four digits.
Specs that predate the visible doc-first issue history use `0000`. The slug is
a short kebab-case topic name and must not add redundant `-design` or `-spec`
suffixes.

Every spec must start with YAML frontmatter:

```yaml
---
id: 167
title: Human Title
status: draft
date: 2026-06-28
issue: 167
pr: null
supersedes: []
superseded_by: []
---
```

Use `issue: null` for the `0000` genesis spec. `pr` is informational and may be
`null` when the PR number is not worth recovering.

Spec status is a lifecycle marker:

- A new spec PR opens with `status: draft` while the document is under review.
- The merging conductor sets `status: accepted` when the spec PR merges.
- A later replacement changes the old spec to `status: superseded` and records
  the replacement in `superseded_by`; the replacement spec records the old spec
  in `supersedes`.

The `status` frontmatter field is the one permitted post-merge edit to an
otherwise frozen spec. When setting `status: superseded`, the same lifecycle
edit may also populate `superseded_by`; all other spec content remains
immutable.

The old `YYYY-MM-DD-...-design.md` date-prefix convention is retired. Dates
belong in frontmatter.

## Adding A Document

For a new design decision, open a documentation issue first, write the draft
record under `docs/specs/`, and merge it as `status: accepted` before any
implementation issue.

For current behavior, update the relevant living document under
`docs/reference/` and link to the frozen spec when readers need rationale.

For workflow or repository process, update the appropriate root document in
`docs/`, usually [`PROCESS.md`](PROCESS.md) or this index.
