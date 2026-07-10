---
id: 639
title: v0.7.0 Global Data Layout And Project Identity Model
status: accepted
date: 2026-07-09
issue: 639
pr: 656
supersedes: []
superseded_by: []
---

# v0.7.0 Global Data Layout And Project Identity Model

This accepted spec defines the loopcoder v0.7.0 machine-local storage layout
and project identity model implemented by the storage, registry, migration,
doctor, credential-sanitization, and permission-hardening follow-up work. The
review for issue #694 confirmed the current implementation matches this
contract after PRs #657, #674, #676, #678, #679, #696, #698, #699, and #700.

## Goals

- Define the v0.7.0 machine-global data layout under the resolved loopcoder
  home directory.
- Keep `LOOPCODER_HOME` as the single override for the machine-level root.
- Define project identity fields that work across many local repositories and
  many checkouts of the same repository.
- Define collision behavior for duplicate names, duplicate remotes, path-only
  projects, and moved repositories.
- Define the compatibility relationship between v0.6.x repo-local
  `.loopcoder/` state and v0.7.0 global storage.
- Make clear which data is tracked in a project repository, which data is
  gitignored repo-local compatibility state, and which data is machine-local.

## Non-Goals

- No SQLite schema or DB implementation in this issue.
- No project registry CLI in this issue.
- No cloud identity, hosted account identity, or cross-machine sync.
- No automatic deletion of v0.6.x `.loopcoder/` directories.
- No change to `.delivery.yml` as the project-scoped tracked configuration
  file.

## Home Resolution

v0.7.0 must continue to use `internal/home` as the starting point for
machine-level paths:

1. If `LOOPCODER_HOME` is set to a non-empty value, that cleaned path is the
   loopcoder home.
2. Otherwise the loopcoder home is `~/.loopcoder`.

Existing home-store paths keep their meaning:

| Path | Meaning |
| --- | --- |
| `$LOOPCODER_HOME/bin/` | Stable selected binary path. |
| `$LOOPCODER_HOME/versions/` | Versioned binary installs. |
| `$LOOPCODER_HOME/skills/` | Stable bundled skill and playbook files. |

v0.7.0 adds machine-local runtime storage under the same home root:

| Path | Meaning |
| --- | --- |
| `$LOOPCODER_HOME/data/loopcoder.db` | Primary local database for registry, runs, reports, leases, and indexes. |
| `$LOOPCODER_HOME/projects/` | Per-project file payloads that should not live in the database, keyed by generated project ID. |
| `$LOOPCODER_HOME/logs/` | Machine-local command, worker, verifier, and diagnostic logs. |
| `$LOOPCODER_HOME/tmp/` | Temporary files, lock files, transient downloads, and scratch space. |

All four added directories are machine-local runtime data. They are never
tracked in consumer repositories, never copied into PR bodies, and never used as
repository-visible evidence unless a user explicitly exports a separate report.

## Project Layout

Each registered project has one generated project ID. File payloads for that
project live under:

```text
$LOOPCODER_HOME/projects/<project_id>/
```

The initial reserved subpaths are:

| Path | Meaning |
| --- | --- |
| `runs/` | Imported or newly written run records that need file storage. |
| `relay/` | Local relay records and pending relay obligations. |
| `recovery/` | Recovery briefs and bounded retry context. |
| `audit/` | Audit logs and audit output referenced by reports. |
| `worktrees/` | Optional loopcoder-managed worktree metadata, not the worktrees themselves unless a later spec says so. |

The database is the authoritative index for these payloads. File paths under
`projects/<project_id>/` are implementation storage details and must not become
public stable identifiers. User-facing commands should render project display
metadata, run IDs, issue numbers, and short relative paths when helpful.

## Project Identity Fields

A project record must have these fields:

| Field | Required | Meaning |
| --- | --- | --- |
| `project_id` | yes | Opaque generated ID used for storage keys and foreign keys. |
| `display_name` | yes | Human display label, usually the repository name. Not unique. |
| `local_path` | yes | Current absolute path to the checkout used by the command. |
| `local_path_canonical` | yes | Cleaned, absolute, case-normalized path used for local comparison. |
| `git_root` | when available | Absolute git worktree root when the path is inside a git repository. |
| `default_branch` | when available | Current default/base branch detected from config, remote metadata, or `.delivery.yml`. |
| `remote_url` | when available | Sanitized selected git remote URL for display and diagnostics. URL userinfo, credentials, query strings, and fragments are never persisted. |
| `remote_url_normalized` | when available | Normalized remote URL used for identity matching. |
| `github_owner` | when available | GitHub owner parsed from a GitHub remote. |
| `github_name` | when available | GitHub repository name parsed from a GitHub remote. |
| `identity_source` | yes | `github`, `git-remote`, or `local-path`. |
| `created_at` | yes | Time the project record was created. |
| `updated_at` | yes | Time identity metadata was last refreshed. |

`display_name` is metadata only. It must never be the only identity key because
many unrelated repositories can share the same name.

## Remote Normalization

When a git remote is available, loopcoder should prefer the configured `origin`
remote. If `origin` is absent, later implementation may choose another remote
only with deterministic ordering and clear diagnostics.

Remote normalization must produce a comparison string with these rules:

- Trim whitespace.
- Convert SCP-like GitHub syntax such as `git@github.com:Owner/Repo.git` to a
  URL-shaped comparison form.
- Lowercase scheme and host.
- Remove userinfo, credentials, fragments, and query strings.
- Remove default ports.
- Remove one trailing `.git` suffix.
- Collapse duplicate slashes in the path.
- For GitHub remotes, parse `github_owner` and `github_name` from the normalized
  owner/name path and compare those fields case-insensitively.

The normalized remote is an identity input, not a display value. Commands must
not persist or print raw Git remote output. Display metadata must use a
sanitized URL, and ambiguous remotes must omit the display URL instead of
preserving potentially sensitive input.

If implementation introduces helper code for this normalization, that code must
have unit tests for HTTPS remotes, SSH remotes, SCP-like GitHub remotes,
trailing `.git`, credentials, case differences, and invalid or unsupported
remote strings.

## Generated Project ID

`project_id` must be stable after registration and opaque to users. The
recommended form is:

```text
proj_<short-base32-or-hex-hash>
```

The hash input should be derived from the strongest available identity at first
registration:

1. For GitHub repositories: `github:<owner>/<name>` using normalized,
   case-insensitive comparison values.
2. For other git remotes: `git-remote:<remote_url_normalized>`.
3. For projects without a usable remote: `local-path:<local_path_canonical>`.

Once created, a project keeps its `project_id` even when metadata changes.
Later code may store additional aliases for old remotes or old local paths, but
must not rewrite existing run records to a different project ID as part of an
ordinary metadata refresh.

## Collision Behavior

Name collisions are allowed. Two projects with the same `display_name` remain
separate unless their stronger identity fields match.

Remote collisions behave as follows:

- Same normalized GitHub owner/name means the same logical project, even when
  it is checked out at multiple local paths.
- Same normalized non-GitHub remote means the same logical project, even when
  it is checked out at multiple local paths.
- Same `display_name` with different normalized remotes means separate
  projects.
- Same local path with a changed remote is an identity change that must be
  diagnosed before merging history. Later code may require explicit user
  confirmation before attaching the path to a different existing project.

Path-only collisions behave as follows:

- Same `local_path_canonical` means the same path-only project.
- Different paths with no usable remote are separate projects by default, even
  if their directory names or `.delivery.yml` contents match.
- A later registry CLI may offer an explicit attach or merge command, but
  v0.7.0 must not silently merge two path-only projects.

If collision handling is ambiguous, commands must fail closed with a diagnostic
that names the conflicting identity fields and the next explicit action needed.
They must not pick a project based only on repository name.

## Path Moves

Path moves are safe to detect only when a stronger identity is available:

- If a checkout moves and the normalized remote identity still matches an
  existing project, loopcoder should update the current `local_path` and keep
  the existing `project_id`.
- If a GitHub repository is renamed but the local checkout still has a remote
  that resolves to the new owner/name, later implementation should record the
  new owner/name as current metadata and may retain the old normalized remote
  as an alias. The `project_id` remains stable.
- If a project has only path identity and the directory moves, loopcoder cannot
  prove it is the same project. The moved path is a new project unless the user
  explicitly attaches it to the old record through a later registry command.

Path history is machine-local metadata. It must not be written to
`.delivery.yml` and must not be committed to the project repository.

## Why Runtime State Moves Out Of Repositories

v0.6.x stores most runtime records under repo-local `.loopcoder/` directories.
That worked for a single local checkout, but it creates v0.7.0 problems:

- One machine can run loopcoder across many repositories, so status, reports,
  leases, logs, and recovery need one machine-local query surface.
- Multiple checkouts of the same repository should share one logical project
  history instead of fragmenting records by path.
- Runtime records are machine-local operational data, not source code or
  project configuration. Keeping them near the checkout makes accidental commits
  more likely.
- A global database can index projects, runs, reports, leases, and recovery
  records without walking every repository on disk.
- Path moves, renamed repositories, and deleted local checkouts can be tracked
  more reliably when the identity record is independent of the current
  directory.

The project repository still owns tracked project configuration such as
`.delivery.yml`, source code, documentation, and normal CI files. The global
store owns local loopcoder runtime state.

## Tracked, Gitignored, And Machine-Local Data

| Data | Location | Repository visibility |
| --- | --- | --- |
| Project config | `<repo>/.delivery.yml` | Tracked when the project chooses to track it. |
| Project docs and source | `<repo>/...` | Tracked normally. |
| v0.6.x compatibility state | `<repo>/.loopcoder/` | Gitignored repo-local machine state. |
| v0.7.0 database | `$LOOPCODER_HOME/data/loopcoder.db` | Machine-local, outside repos. |
| v0.7.0 project payloads | `$LOOPCODER_HOME/projects/<project_id>/` | Machine-local, outside repos. |
| v0.7.0 logs | `$LOOPCODER_HOME/logs/` | Machine-local, outside repos. |
| v0.7.0 temp files | `$LOOPCODER_HOME/tmp/` | Machine-local, outside repos. |

The repo-local `.loopcoder/` directory remains local-only compatibility state.
It must stay gitignored when present. v0.7.0 must not require tracked
`.gitignore` edits for its global store because the global store is outside the
repository.

## v0.6.x Compatibility And Migration

v0.7.0 must be able to coexist with v0.6.x repo-local state during the
transition:

- Existing v0.6.x `.loopcoder/` directories are legacy local state, not source
  files.
- v0.7.0 commands should write new runtime state to global storage by default
  after the project is registered.
- Read paths should fall back to repo-local `.loopcoder/` when global records
  for the selected project are absent, so existing status, report, relay, and
  recovery records remain discoverable during migration.
- Import or migration code must copy legacy records into global storage rather
  than moving them in place.
- Migration must not delete `.loopcoder/`, run `git rm`, edit tracked
  `.gitignore`, or mutate GitHub.
- After import, commands should prefer global records and may mark imported
  records with source metadata such as `legacy-repo-local`.
- The explicit `state push` boundary remains explicit. Importing repo-local
  state into the machine-global store does not publish that state to GitHub or
  any cloud service.

The compatibility window should be long enough for a v0.6.x user to upgrade,
run `doctor`, inspect discovered legacy state, and choose whether to import or
leave the old repo-local records in place. The exact commands and schema belong
to later implementation specs.

## Doctor And Diagnostics Expectations

Later implementation work should extend diagnostics so users can answer:

- Which `LOOPCODER_HOME` is active.
- Whether `$LOOPCODER_HOME/data/loopcoder.db` is reachable.
- Which project ID the current repository resolves to.
- Which identity source selected that project.
- Whether another project record conflicts with the current path or remote.
- Whether legacy repo-local `.loopcoder/` state exists and has or has not been
  imported.

Diagnostics must distinguish machine-local global state from repository-tracked
configuration. They should not describe global storage as cloud sync or as
repository state.

## Acceptance Criteria For Follow-On Implementation

- `internal/home` exposes or supports the added `data`, `projects`, `logs`, and
  `tmp` paths while preserving `LOOPCODER_HOME`.
- Remote normalization has unit tests for the cases named in this spec.
- Project registry records include the required identity fields from this spec.
- Project lookup never uses `display_name` as the sole identity key.
- Duplicate names, duplicate remotes, path-only projects, path moves, and
  ambiguous collisions follow this spec.
- New runtime records are written to global storage after registration.
- Existing v0.6.x repo-local `.loopcoder/` records remain readable during the
  compatibility window.
- Migration copies legacy state and does not delete repo-local `.loopcoder/` or
  mutate tracked repository files.

## Relationship To Existing Specs

- [`0603-customer-ready-bridge.md`](0603-customer-ready-bridge.md) keeps
  v0.6.1 repo-local and explicitly defers SQLite and global registry work to
  v0.7.0. This spec defines that deferred v0.7.0 storage direction.
- [`0583-upgrade-migration-doctor.md`](0583-upgrade-migration-doctor.md)
  defines v0.6 migration and stale-state cleanup behavior. v0.7.0 migration
  must preserve that safety posture and avoid deleting local state implicitly.
- [`0567-reporter.md`](0567-reporter.md) defines local-only report records.
  v0.7.0 changes where those local-only records are stored and indexed, not
  their repository visibility.
- [`0041-resilience.md`](0041-resilience.md) defines v0.6 run state, recovery,
  and liveness records under repo-local `.loopcoder/`. v0.7.0 must preserve the
  same operational concepts while moving the authoritative machine-local index
  to global storage.
