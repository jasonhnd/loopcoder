# Self-Bootstrap Acceptance

This checklist defines the historical v0.8.0 self-bootstrap acceptance
scenario. It proved that the staged release artifact could operate on the
LoopCoder repository, persist
durable machine-local state, execute a bounded child graph, render human and
JSON evidence, and recover or roll back without paid provider quota.

The scenario is release evidence, not publication authority. It cannot promote
`main`, create a tag, publish a release, waive a failed check, or replace the
required human approval in the `release-publication` environment.

The child graph uses the reserved `test-subprocess` fixture. This smoke is not
real-provider nested evidence and does not prove end-to-end production support.
See the binding
[`v0.8.0 capability and support matrix`](v0.8.0-capability-matrix.md).

## Platform Boundary

The v0.8.0 self-bootstrap path supports native macOS Apple Silicon
(`darwin/arm64`) only. Windows, Linux/Ubuntu, WSL, containers used as a
LoopCoder runtime, Intel macOS, and Rosetta/amd64 macOS are unsupported. Those
hosts must remain on the historical v0.7.0 release or wait for a separately
approved future platform roadmap.

The smoke must fail before creating temporary state, opening storage, invoking
a provider, contacting GitHub, or mutating a repository when the host tuple is
unsupported.

## Deterministic Release Smoke

Run from a LoopCoder source checkout on Darwin arm64:

```text
pwsh scripts/self-bootstrap-smoke.ps1 -Version 0.8.0
```

For release evidence, pass the binary extracted from the staged candidate:

```text
pwsh scripts/self-bootstrap-smoke.ps1 \
  -Version 0.8.0 \
  -Binary <staged-candidate>/loopcoder \
  -KeepArtifacts
```

The script may build a local development binary only when `-Binary` is absent.
It records the selected binary path, SHA-256, and `loopcoder version` output so
a release run can prove that self-bootstrap consumed the same candidate later
eligible for publication.

The smoke uses a temporary `LOOPCODER_HOME`, the reserved
`test-subprocess` provider, fixed child-plan input, and local git commands. It
must not read provider credentials, call a paid model, use provider-native
sub-agents, mutate GitHub, or require network access.

The smoke proves all of the following:

- the runtime and selected binary report native `darwin/arm64`;
- `projects register` resolves the LoopCoder checkout and creates
  `$LOOPCODER_HOME/data/loopcoder.db` outside the repository;
- registered run payloads are written below
  `$LOOPCODER_HOME/projects/<project_id>/`, not the repository;
- `migrate storage` planning is read-only and reports the current source and
  target schema without creating a backup for a fresh schema-30 database;
- a deterministic three-child graph executes with dependency-aware fan-out and
  fan-in, durable parent/child identity, and no remote provider call;
- `status` and `report` both produce representative human output and valid JSON
  for the same run tree;
- progress, report, and doctor artifacts are retained only under the temporary
  evidence directory when `-KeepArtifacts` is used;
- doctor reports database, registry, nested-run, provider compatibility, and
  host-profile evidence honestly, while optional missing provider login or
  GitHub readiness remains visible rather than fabricated as success;
- no new runtime payload appears under the registered repository's
  `.loopcoder/runs`, `.loopcoder/logs`, `.loopcoder/recovery`, or
  `.loopcoder/relay` paths.

## v0.7.0 Upgrade And Rollback Smoke

The staged release workflow must run the exact candidate artifact through:

```text
pwsh scripts/release-smoke.ps1 \
  -Version v0.8.0 \
  -PreviousVersion 0.7.0
```

That path must use the published v0.7.0 Darwin arm64 binary to create a real
schema-9 database, then prove that the v0.8.0 candidate:

1. renders a side-effect-free migration plan;
2. applies schema 9 through 30 only after `--apply`;
3. creates exactly one verified owner-only backup;
4. preserves project identity, run history, reports, and multi-project state;
5. treats repeated apply as `no-op`;
6. upgrades through the signed installer seam to the exact staged candidate;
7. copies, rather than moves or mutates, the backup for rollback; and
8. proves the restored backup still opens with the published v0.7.0 binary.

All LoopCoder processes must be stopped before migration or rollback. A v0.7.0
binary must never open the migrated schema-30 database. Rolling back to the
backup discards v0.8-only state created after migration. See
[`storage-migration.md`](storage-migration.md) for stable limitation codes and
the executable offline procedure.

## Required Release Evidence

This section records the pre-publication evidence map. The later product-path
audit narrowed what that evidence supports; it must be read together with the
capability matrix rather than as a claim that every listed internal component
was connected to a shipped production path.

The final v0.8.0 evidence record must connect these boundaries:

1. #959 and children #960-#968: durable provider ownership, detached
   supervision, five-minute progress, provider-free waits, and orchestration
   cost limits.
2. #867: schema planning, verified backup, atomic migration, idempotent replay,
   corrupt/disk-full handling, and rollback proof.
3. #953: current living documentation and the Darwin arm64-only policy test.
4. #868: deterministic fresh and v0.7-upgrade self-bootstrap evidence.
5. #869: final candidate SHA, signed artifact identities, staged smoke,
   environment approval, publication result, and post-publication verification.

Every implementation issue must identify its merged PR and candidate ancestor.
Local reporter blocks, raw run records, credentials, provider output, and
machine-local paths must not be copied into GitHub-visible artifacts. Release
evidence records commands, hashes, stable result summaries, and public workflow
URLs only.

## Non-Acceptance Cases

The release remains NO-GO when any of the following is true:

- self-bootstrap used a locally rebuilt binary when claiming staged-artifact
  evidence;
- the host was not native Darwin arm64;
- any default smoke path consumed paid quota or required private credentials;
- state or evidence was written inside the registered repository;
- human and JSON outputs describe different run identities or outcomes;
- migration planning changed a file, backup verification was skipped, or
  rollback was not opened by v0.7.0;
- an implementation issue lacks merged-PR and candidate-ancestor evidence;
- a failed or cancelled release run deleted its diagnosable draft or claimed
  publication success;
- the release-publication approval or post-publication public verification is
  missing.

## Evidence Template

```text
candidate SHA: <sha>
candidate binary: <absolute local path used by smoke>
candidate binary SHA-256: <sha256>
host tuple: darwin/arm64
self-bootstrap: pwsh scripts/self-bootstrap-smoke.ps1 -Version 0.8.0 -Binary <path> -KeepArtifacts
project_id: <project_id>
database outside repo: yes
registered payload outside repo: yes
parent run: <run-id>
child runs: <run-id>, ...
status human/JSON: <artifact paths>
report human/JSON: <artifact paths>
doctor JSON: <artifact path>
storage plan JSON: <artifact path>
upgrade smoke: pwsh scripts/release-smoke.ps1 -Version v0.8.0 -PreviousVersion 0.7.0
migration backup verified and opened by v0.7.0: yes
paid provider calls: 0
implementation evidence: #<issue> -> PR #<pr> -> <merge SHA>, ...
human audit decision: pass | fail | needs-human
```
