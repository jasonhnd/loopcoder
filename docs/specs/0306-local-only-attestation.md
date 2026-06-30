---
id: 306
title: Local-Only Attestation With Zero Repo And GitHub Footprint
status: draft
date: 2026-07-01
issue: 306
pr: null
supersedes:
  - docs/specs/0146-attestation.md#decision-6
  - docs/specs/0146-attestation.md#schema-rendering-pr-bodies
  - docs/specs/0218-surface-worker-attestation.md#pr-body-durable-home
  - docs/specs/0214-human-readable-attestation.md#pr-body-and-merge-artifact-rows
  - docs/specs/0282-default-pretty-attestation.md#durable-pr-and-merge-targets
superseded_by: []
---

# Local-Only Attestation With Zero Repo And GitHub Footprint

This is a design-only spec. This PR must add only this document: no Go code, no
test, no `.delivery.yml` change, no command behavior change, and no edit to any
other spec body. Implementation and documentation alignment belong in separate
issues after this spec is reviewed and merged per [`docs/PROCESS.md`](../PROCESS.md).

## Goal

Per-invocation attestation for Worker, Verifier, and Conductor runs is a local
operational and audit signal only.

loopcoder must keep attestation visible and recoverable for the local operator,
but must never persist attestation into repository-visible or GitHub-hosted
surfaces. Attestation is not project content, PR content, issue content, commit
content, or durable public delivery record content.

## Local-Only Invariant

The hard invariant is:

> Attestation must be surfaced locally and must not be persisted into any
> repository-visible or GitHub-hosted surface.

Allowed local surfaces are exactly:

- the stderr pretty block for Worker, Verifier, and Conductor invocations;
- the `dispatch` and `loopreview` result JSON `attestation` object;
- gitignored `.loopcoder/` run records.

Forbidden zero-footprint surfaces are exactly:

- PR body;
- PR comments;
- issue body;
- issue comments;
- commit message;
- merge commit body;
- merge comments;
- docs;
- any version-controlled file.

No `[attestation]` header and no attestation JSON may appear in any forbidden
surface. This includes generated PR text, issue text, comments, commits,
merge artifacts, repository docs, examples, fixtures, snapshots, or any other
committed file produced by loopcoder.

## Decisions

1. **Attestation is local-only.** Worker, Verifier, and Conductor attestation
   remains mandatory operational evidence, but it is local evidence. It is not
   copied into GitHub or repository-visible artifacts.
2. **Local surfaces stay intact.** The stderr pretty block, `dispatch` result
   JSON `attestation` object, `loopreview` result JSON `attestation` object,
   and gitignored `.loopcoder/` records remain valid attestation surfaces.
3. **Repo and GitHub surfaces get zero attestation footprint.** PR bodies,
   PR/issue comments, issue bodies, commit messages, merge commit bodies, docs,
   and committed files must not contain an attestation header or attestation
   JSON.
4. **The source of truth is local state.** Attestation validation and recovery
   read from the in-memory record, command result, and `.loopcoder/` run
   records, not from PR bodies or merge artifacts.
5. **Existing attestation semantics are unchanged.** This spec does not change
   `AttestationRecord`, `Header()`, `CanonicalJSON()`, `verified`,
   `model_source`, token usage rules, validation strictness, model/effort
   inheritance, the human merge gate, or reviewer-not-worker guidance.
6. **The dispatch stdout contract remains local.** The 0218 dispatch stdout
   three-record contract remains exactly as specified: header line, canonical
   JSON line, then result JSON. Stdout is a local command surface. The same
   records must not be copied from stdout into a PR body, issue body, comment,
   commit, merge body, doc, or committed file.

## Amendments To Existing Specs

This spec supersedes only the PR-body, merge-artifact, and repo-persisted
attestation decisions named below. All other decisions in the referenced specs
remain unchanged.

### 0146 Attestation

[`0146-attestation.md`](0146-attestation.md) Decision 6 said Conductor
attestation survives context loss by being stamped into commit, PR, or merge
artifacts. That persistence target is superseded. Conductor attestation must
now survive context loss through gitignored `.loopcoder/` local run records
instead.

The 0146 schema-rendering note that describes the one-line header as targeting
PR bodies is superseded only for the PR body target. The `AttestationRecord`
schema, `Header()`, `CanonicalJSON()`, trust marker, model source values, token
usage requirements, and fail-closed behavior remain unchanged.

### 0218 Surface Worker Attestation

[`0218-surface-worker-attestation.md`](0218-surface-worker-attestation.md)
requires `dispatch` to surface Worker attestation locally, and that requirement
remains. Its statements that the Worker record is stamped into the PR body,
appears in the PR body, or uses the PR body as the durable home for the Worker
record are superseded.

The 0218 dispatch stdout contract is unchanged because stdout is local:

- the Worker attestation header line remains first;
- the Worker attestation canonical JSON line remains second;
- the final dispatch result JSON with its `attestation` object remains third.

The final result JSON `attestation` object is still the structured local result
surface. It must not be copied into GitHub or committed repository content.

### 0214 Human-Readable Attestation

[`0214-human-readable-attestation.md`](0214-human-readable-attestation.md)
defined rendering target rows for PR bodies created by `dispatch` and for merge
commit bodies or merge comments. Those rows are superseded only for those
targets. PR bodies, merge commit bodies, and merge comments are now forbidden
surfaces for attestation.

The 0214 pretty renderer, stable header renderer, canonical JSON renderer,
field formatting rules, non-parse-target rule for pretty output, cross-platform
requirements, and fail-closed semantics remain unchanged.

### 0282 Default Pretty Attestation

[`0282-default-pretty-attestation.md`](0282-default-pretty-attestation.md)
refers to PR bodies and merge commits as durable attestation targets. Those
durable target references are superseded. The default stderr pretty behavior,
Conductor relay of command stderr pretty blocks, stdout compatibility rules,
suppression and force precedence, and separation of Worker and Verifier records
remain unchanged.

## Unchanged Contracts

This spec intentionally leaves these contracts unchanged:

- the 0146 `AttestationRecord` schema;
- `Header()`;
- `CanonicalJSON()`;
- `verified`;
- `model_source`;
- token usage parsing and validation rules;
- fail-closed strictness;
- Worker behavior that opens no PR when attestation is missing or invalid;
- Verifier behavior that returns `needs-human` when attestation is missing or
  invalid;
- the 0218 dispatch stdout three-record contract;
- stderr pretty rendering and Conductor relay of the pretty block;
- model and effort inheritance;
- the human merge gate;
- reviewer-not-worker guidance.

Only the persistence targets change. Local command output and local run records
continue to carry attestation. Repository-visible and GitHub-hosted surfaces do
not.

## Local Recovery

Amending 0146 Decision 6 removes merge artifacts as the Conductor recovery
source. Recovery must now use `.loopcoder/` local run records.

Each Worker, Verifier, and Conductor invocation must persist the full
`AttestationRecord` into the relevant gitignored `.loopcoder/` run record. For
Worker attempts, the attempt record must store the full record, not only token
usage. For Verifier runs, the review run record must store the full record. For
Conductor attestations, the run record must store the self-reported
Conductor record with `verified: false` and `model_source: self-reported`.

A later Conductor after compaction, interruption, or same-host session transfer
recovers prior attestation locally by reading `.loopcoder/` records and command
result JSON. It must not read PR bodies, issue bodies, comments, commit
messages, merge bodies, docs, or committed files to recover attestation.

If `.loopcoder/` records are absent, unavailable, corrupt, or incomplete, the
Conductor must report the missing local evidence honestly. It must not treat a
GitHub artifact or committed file as a fallback attestation source.

## Fail-Closed Validation Source

Fail-closed validation is sourced from the local attestation record, not from a
PR body.

For Worker delivery, the implementation must validate the in-memory
`AttestationRecord` and the local dispatch result before PR creation. If the
Worker attestation is missing, incomplete, unparseable, or invalid, delivery
stays blocked and the Worker opens no PR.

For Verifier delivery, `loopreview` must validate the local Verifier record
before returning a normal pass/fail result. If the Verifier attestation is
missing, incomplete, unparseable, or invalid, the Verifier returns
`needs-human`.

For recovery and resume, `.loopcoder/` records are the persisted local source
of truth for attestation. A PR body without attestation is expected and must
not be treated as missing evidence. A PR body, issue, comment, commit, merge
body, doc, or committed file with attestation is a zero-footprint violation.

## Regression Guard

The follow-on implementation must add a regression guard that fails if any
loopcoder-produced forbidden surface contains an attestation header or
attestation JSON.

The guard must cover at least:

- PR bodies produced by `dispatch`;
- issue bodies produced by loopcoder;
- PR and issue comments produced by loopcoder;
- commit messages produced by loopcoder;
- merge commit bodies produced by loopcoder;
- committed files produced by loopcoder, including docs, examples, fixtures,
  snapshots, and generated artifacts.

The guard should distinguish references to this policy from actual emitted
attestation records. It must fail on a real attestation header line or canonical
attestation JSON object in any forbidden surface, while allowing local stderr,
stdout result JSON, and gitignored `.loopcoder/` run records to continue
carrying attestation.

## Follow-Up Issues

After this spec merges, implementation and doc alignment should be split into
separate issues:

1. **Implementation:** remove attestation stamping from PR bodies, issue
   bodies, comments, commit messages, merge bodies, docs, and committed files;
   persist the full `AttestationRecord` in `.loopcoder/` local run records; and
   add the zero-footprint regression guard.
2. **Documentation alignment:** update `0146-attestation.md`,
   `0218-surface-worker-attestation.md`,
   `0214-human-readable-attestation.md`, and
   `0282-default-pretty-attestation.md` frontmatter with `superseded_by`
   pointers and align operational docs such as `SKILL.md`, `AGENTS.md`, and
   reference docs to this accepted policy.

## Acceptance Criteria For Follow-On Code

- Worker, Verifier, and Conductor attestation remains surfaced locally.
- `dispatch` and `loopreview` result JSON retain their `attestation` objects.
- stderr pretty blocks remain available for local human reporting.
- `.loopcoder/` records store the full `AttestationRecord`, not token usage
  only.
- PR bodies, issue bodies, PR/issue comments, commit messages, merge commit
  bodies, docs, and committed files contain no attestation header or
  attestation JSON.
- Worker fail-closed validation uses the local record and still opens no PR
  when attestation is missing or invalid.
- Verifier fail-closed validation uses the local record and still returns
  `needs-human` when attestation is missing or invalid.
- Regression tests fail when loopcoder emits attestation into any forbidden
  repo-visible or GitHub-hosted surface.

## Acceptance Criteria For This PR

- This PR adds only `docs/specs/0306-local-only-attestation.md`.
- The spec has `status: draft`, `date: 2026-07-01`, `id: 306`, `issue: 306`,
  and a `supersedes` list naming the superseded PR-body, merge-artifact, and
  repo-persisted decisions.
- The spec states the local-only invariant with explicit allowed and forbidden
  surface lists.
- The spec names and amends the conflicting PR-body, merge-artifact, and
  repo-persisted decisions in 0146, 0218, 0214, and 0282.
- The spec states that all non-persistence contracts in those specs remain
  unchanged.
- The spec defines recovery through gitignored `.loopcoder/` local records,
  including persistence of the full `AttestationRecord`.
- The spec re-sources fail-closed validation from the local record and
  preserves the delivery-blocking guarantee.
- The spec requires the zero-footprint regression guard.
- No Go code, test, `.delivery.yml`, accepted spec body, command behavior, or
  runtime dependency change is included.

## Non-Goals

- No Go implementation in this design PR.
- No test change in this design PR.
- No `.delivery.yml` change in this design PR.
- No command behavior change in this design PR.
- No edit to other specs' bodies in this design PR.
- No frontmatter pointer edits to other specs in this design PR.
- No change to `AttestationRecord`.
- No change to `Header()`.
- No change to `CanonicalJSON()`.
- No change to token usage rules.
- No change to `verified` or `model_source` semantics.
- No weakening of fail-closed strictness.
- No change to the dispatch stdout three-record contract.
- No change to model or effort inheritance.
- No change to the human merge gate.
- No change to reviewer-not-worker guidance.

## Relationship To Existing Specs

- [`0146-attestation.md`](0146-attestation.md) defines the shared
  attestation schema, renderers, trust semantics, and fail-closed behavior.
  This spec supersedes only its repository and GitHub persistence targets.
- [`0218-surface-worker-attestation.md`](0218-surface-worker-attestation.md)
  defines the local dispatch result and stdout surfaces for Worker
  attestation. This spec preserves those local surfaces and supersedes only
  the PR-body persistence target.
- [`0214-human-readable-attestation.md`](0214-human-readable-attestation.md)
  defines pretty rendering and usage split rules. This spec preserves those
  renderers and supersedes only the PR-body and merge-artifact target rows.
- [`0282-default-pretty-attestation.md`](0282-default-pretty-attestation.md)
  defines default stderr pretty output and Conductor relay. This spec preserves
  those local behaviors and supersedes only references to PR bodies and merge
  commits as durable attestation targets.
- [`0041-resilience.md`](0041-resilience.md) defines `.loopcoder/` run records
  as local recovery state. This spec extends that local state by requiring the
  full attestation record to be stored there for Worker, Verifier, and
  Conductor recovery.
