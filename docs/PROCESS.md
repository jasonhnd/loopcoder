# loopcoder Process

This is the canonical mandatory development workflow for loopcoder. It applies
to every unit of work.

The workflow is strict, ordered, non-skippable, and never bundled.

## 1. Document first

Start with a documentation issue. A worker writes the design or spec document
under `docs/specs/<NNNN>-<kebab-slug>.md`, then the document is reviewed and
merged.

`<NNNN>` is the originating GitHub issue number, zero-padded to four digits.
Specs that predate the visible doc-first issue history use `0000`. The slug is
a short kebab-case topic name without `-design` or `-spec`.

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
belong in frontmatter, not filenames.

The merged document is the contract. It is a hard prerequisite gate for any
code.

## 2. Code from the doc

Only after the design document is merged, open a separate code issue. The code
issue must reference the merged document, for example:

```text
implement per docs/specs/<NNNN>-<kebab-slug>.md
```

A worker then writes code strictly following that merged document. The code
change is reviewed and merged separately.

## 3. Verify last

After code exists, verify the implementation against the document. Verification
checks both that the code matches the spec and that it works.

## 4. Apply the self-hosting gate

When loopcoder develops or releases itself, the mandatory controls in
[`self-hosting-playbook.md`](self-hosting-playbook.md) apply in addition to the
doc-first sequence.

Before dispatch:

- keep the minor release within the documented version and dependency budget;
- split each implementation issue to one primary behavior and a bounded Worker
  attempt;
- allow only one local provider at a time and disable provider-native
  sub-agents by default;
- keep full repository, full race, security, signing, packaging, migration, and
  release smoke on remote runners; and
- establish a user-visible five-minute progress destination and a hard local
  timeout.

After a failure:

- classify it as implementation, test, provider, delivery, infrastructure,
  waiting, or human-decision failure before retrying;
- resume only the failed stage;
- never repeat a successful provider call for a push, PR, report, or CI-wait
  failure; and
- stop after one automatic retry or when external side effects are ambiguous.

During release-candidate freeze, accept only P0, P1, and release-contract
corrections. Stop for human GO/NO-GO after two failed candidates. Use
[`reference/development-release-checklist.md`](reference/development-release-checklist.md)
as the evidence checklist from intake through post-publication cleanup.

## 5. Close the operational loop

Before declaring a unit complete, confirm that provider, test, and watcher
processes have exited; worktree and branch ownership is explicit; delivery
evidence is durable; local-only data remains private; and the final status is
visible to the user.

Code-complete, candidate-complete, and release-complete are different states.
Name the state and its evidence rather than reporting an unqualified “done.”

## Hard rules

- Never open a code issue before its design document is merged.
- Never put documentation changes and code changes in the same issue or PR.
- Keep one concern per PR.
- Never hand-write code outside this loop.
- Never create a new spec with a `YYYY-MM-DD-` filename prefix.
- Never run unapproved local full-suite or full-race work during self-hosting.
- Never use a provider for deterministic waiting, heartbeat, progress, polling,
  or delivery-only recovery.
- Never restart implementation when a valid commit already exists and only a
  delivery stage failed.
- Never merge post-RC documentation or enhancements into a frozen candidate.
