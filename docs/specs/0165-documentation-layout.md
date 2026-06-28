---
id: 165
title: Documentation Layout - Spec Naming Convention and Reference Split
status: accepted
date: 2026-06-28
issue: 165
pr: null
supersedes: []
superseded_by: []
---

# Documentation Layout - Spec Naming Convention and Reference Split

This is a design-only change. This PR must add only this document: no file
moves, no renames, no link edits, and no code changes. The moves, link rewrites,
reference reconciliation, process updates, and tests described here belong in a
separate follow-up code issue per `docs/PROCESS.md`.

## 1. Governing Principle

One document, one nature.

Every document is exactly one of:

- A frozen design record in `docs/specs/`, immutable after merge. A later change
  supersedes it with a new spec; it never edits the accepted record in place.
- A living reference in `docs/reference/`, kept current with built behavior.
- Process meta in the `docs/` root.

This prevents a document from being both current reference and historical
rationale. It also gives readers a reliable way to decide whether a document is
the current operating contract or a point-in-time decision record.

## 2. Spec Naming Convention

Specs use an ADR/RFC-style identifier instead of a date prefix:

```text
docs/specs/<NNNN>-<kebab-slug>.md
```

`<NNNN>` is the originating GitHub issue number, zero-padded to four digits.
For example, issue #131 becomes `docs/specs/0131-multi-provider-roles.md`.

Rationale:

- The number is citable as a stable id, such as "spec 131".
- The id is race-free under parallel worker dispatch because GitHub assigns the
  issue number before the worker writes the spec.
- The id is 1:1 traceable to the doc-first issue and its PR.
- Sorting by id groups specs by project history without pretending that the date
  is the identifier.

The slug is a short kebab-case topic name. It must not collide with living
reference terms such as `architecture`, `worker`, or `usage`. Do not add
`-design` or `-spec`; those suffixes are redundant inside `docs/specs/`.

A spec that predates the doc-first process and has no originating issue uses
`0000` as the genesis id.

All secondary or mutable metadata goes in YAML frontmatter, not the filename:

```yaml
---
id: 131
title: <human title>
status: accepted        # draft | accepted | superseded
date: 2026-06-27        # informational merge date
issue: 131
pr: 132
supersedes: []
superseded_by: []
---
```

## 3. Target Tree

The target `docs/` tree is:

```text
docs/
  reference/   architecture.md, worker.md, usage.md   (no numbers, no dates; living)
  specs/       <NNNN>-<slug>.md ...                    (numbered, frontmatter, frozen)
  PROCESS.md  BACKLOG.md  learnings.md                 (process meta, docs root)
  README.md                                             (new index and doc-type legend)
```

`docs/README.md` is a new index. It must define the
one-document-one-nature legend, explain where each document type belongs, and
explain how to add a new doc.

The repository root remains unchanged:

- `README.md`
- `DESIGN.md`, the living north-star vision, distinct from any frozen spec
- `CHANGELOG.md`
- `ROADMAP.md`, a functional compiler input that must remain in place; its
  header should make clear that it is a template
- `SKILL.md`, `AGENTS.md`, and `GEMINI.md`, the host entrypoints

## 4. Rename And Move Map

The follow-up issue must apply this full map. The ids below were checked against
git history: commits for #28, #39, #40, #41, #81, #89, and #131 include
`closes #...`; the original v1 spec predates the visible doc-first issue history
and therefore uses the genesis id.

| current | target | id |
|---|---|---|
| `docs/specs/2026-06-27-multi-provider-roles-design.md` | `docs/specs/0131-multi-provider-roles.md` | #131 |
| `docs/specs/2026-06-26-loopcoder-v1-design.md` | `docs/specs/0000-loopcoder-v1.md` | genesis (no issue) |
| `docs/verification.md` | `docs/specs/0039-verification.md` | #39 |
| `docs/self-improvement.md` | `docs/specs/0040-self-improvement.md` | #40 |
| `docs/resilience.md` | `docs/specs/0041-resilience.md` | #41 |
| `docs/orchestration.md` | `docs/specs/0081-orchestration.md` | #81 |
| `docs/scheduling.md` | `docs/specs/0028-scheduling.md` | #28 |
| `docs/go-migration.md` | `docs/specs/0089-go-migration.md` | #89 |
| `docs/architecture.md` | `docs/reference/architecture.md` | -- |
| `docs/worker.md` | `docs/reference/worker.md` | -- |
| `docs/usage.md` | `docs/reference/usage.md` | -- |

## 5. Reference Reconciliation

The move must not just relocate files. The current design records for
orchestration, verification, resilience, self-improvement, and scheduling also
serve as the de-facto reference for built behavior. The existing
`docs/architecture.md` is only 91 lines and does not cover that operational
depth.

The follow-up code issue must enrich `docs/reference/architecture.md` into the
single living system map. It must add one short subsystem section each for:

- orchestration
- verification
- resilience
- self-improvement
- scheduling

Each section must state current behavior and link to the frozen spec for
rationale. After that reconciliation, specs are purely historical records and no
current-behavior knowledge is lost.

## 6. Link-Integrity Inventory

The follow-up issue must rewrite every current reference to a moved doc except
for `CHANGELOG.md`. Changelog entries are historical records of what was true at
each release and must remain unchanged.

Update these non-doc or entrypoint sites:

| file | sites to rewrite |
|---|---|
| `README.md` | Cross-platform badge target `docs/go-migration.md`; docs index entries for `docs/architecture.md`, `docs/scheduling.md`, `docs/verification.md`, `docs/self-improvement.md`, `docs/resilience.md`, `docs/orchestration.md`, `docs/go-migration.md`, and `docs/usage.md`. |
| `SKILL.md` | The v1 spec link, two `docs/self-improvement.md` links, three `docs/scheduling.md` links, two `docs/verification.md` links, and three `docs/resilience.md` links. |
| `DESIGN.md` | References to `docs/architecture.md` and `docs/worker.md`. |
| `internal/gitutil/doc.go` | Package comment referencing `docs/go-migration.md`. |
| `internal/worker/doc.go` | Package comment referencing `docs/go-migration.md`. |
| `internal/cli/cli.go` | User-facing error string `see docs/go-migration.md`. |

Update these cross-links inside moved design records:

| file | current references to rewrite |
|---|---|
| `docs/orchestration.md` | `architecture.md`, `scheduling.md`, `resilience.md`, and `verification.md` links and inline `docs/...` mentions throughout the file. |
| `docs/resilience.md` | Inline references to `docs/architecture.md`, `docs/scheduling.md`, `docs/worker.md`, and `docs/resilience.md` in examples. |
| `docs/scheduling.md` | `worker.md` and `architecture.md` links. |
| `docs/verification.md` | `architecture.md`, `scheduling.md`, `worker.md`, and `docs/verification.md` references. |
| `docs/self-improvement.md` | `architecture.md`, `scheduling.md`, and `docs/worker.md` references. |
| `docs/go-migration.md` | `architecture.md`, `orchestration.md`, `verification.md`, `resilience.md`, `scheduling.md`, and `SKILL.md` links. |
| `docs/architecture.md` | Existing v1 spec link to `docs/specs/2026-06-26-loopcoder-v1-design.md`. |
| `docs/worker.md` | Existing multi-provider spec link to `docs/specs/2026-06-27-multi-provider-roles-design.md`. |
| `docs/usage.md` | Existing `architecture.md` link. |
| `docs/specs/2026-06-27-multi-provider-roles-design.md` | Existing `../worker.md` reference and any related doc references that change under the new split. |

Leave these historical references unchanged:

| file | historical references |
|---|---|
| `CHANGELOG.md` | Entries for `docs/resilience.md`, `docs/verification.md`, `docs/self-improvement.md`, `docs/scheduling.md`, `docs/go-migration.md`, and `docs/specs/2026-06-26-loopcoder-v1-design.md`. |

## 7. loopreview Compatibility

`internal/loopreview` already discovers referenced docs with a
`docs/[A-Za-z0-9._/-]+\.md` match and then prefers candidates under
`docs/specs/`. Moving frozen specs into `docs/specs/` is aligned with that
behavior. Moving living reference docs under `docs/reference/` still leaves them
matching the regex, but specs keep priority when both are present.

No loopreview production code change is required for the layout move. The
follow-up code issue must add a loopreview test asserting spec discovery on the
new layout, including a case where text contains both a `docs/reference/...`
path and a `docs/specs/<NNNN>-<slug>.md` path and discovery selects the spec.

## 8. Codify The Convention

The follow-up issue must codify this convention in both:

- `docs/PROCESS.md`, so doc-first work requires
  `docs/specs/<NNNN>-<kebab-slug>.md` with YAML frontmatter and rejects the old
  date-prefix convention.
- `docs/README.md`, so readers and future workers can find the document-type
  legend, the spec naming rule, the frontmatter contract, and the instructions
  for adding a new document.

That documentation is what prevents the `YYYY-MM-DD-...-design.md` convention
from returning.

