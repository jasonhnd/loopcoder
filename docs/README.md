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
- [`learnings.md`](learnings.md): append-only operational learnings.
- [`reference/architecture.md`](reference/architecture.md): current system map.
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
