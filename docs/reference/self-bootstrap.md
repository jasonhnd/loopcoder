# Self-Bootstrap Acceptance

This checklist defines the v0.7.0 self-bootstrap acceptance scenario: loopcoder
uses the loopcoder repository, the v0.7.0 milestone issue set, local runtime
state, nested child runs, and release checks to prove that the project can
develop and review itself without hiding evidence.

The scenario is acceptance evidence, not an automatic production merge. Human
review remains the final gate, and the run is only accepted when a human can
audit the path from issue to PR to local report/status output.

## Scripted Smoke

Run the deterministic local smoke from a loopcoder source checkout:

```text
pwsh scripts/self-bootstrap-smoke.ps1
```

The script builds the local `loopcoder` binary unless `-Binary <path>` is
provided. It sets `LOOPCODER_HOME` to a temporary directory, registers the
loopcoder checkout in the v0.7.0 project registry, creates a deterministic
parent/child run fixture under gitignored `.loopcoder/`, and verifies the JSON
surfaces exposed by `doctor`, `status`, and `report`.

The smoke proves:

- `loopcoder projects register --repo .` resolves the loopcoder checkout and
  writes the registry database under `$LOOPCODER_HOME/data/loopcoder.db`.
- `$LOOPCODER_HOME/data/loopcoder.db` exists outside the repository.
- `doctor --repo . --format json` exposes storage health, project registry
  health, provider compatibility, and nested-run health.
- `status --repo . --run <child> --format json` exposes a parent/child run
  tree with issue and worker report metadata.
- `report --repo . --run <child> --format json` includes both the child worker
  report record and the same run tree.

The smoke does not require provider authentication, paid services, GitHub
mutation, dispatch, review, merge, tags, or release assets. If `doctor` exits
non-zero only because local `gh` or provider CLIs are missing or unauthenticated,
the smoke still fails only when the v0.7.0 runtime assertions above are missing.
Use `-KeepArtifacts` to retain the temporary `LOOPCODER_HOME` and JSON outputs.

## Manual Acceptance Checklist

Use the v0.7.0 milestone and the issue chain named by issue #654:

- #632: v0.7.0 roadmap for global local runtime, multi-project state,
  provider compatibility, and nested sub-agent orchestration.
- #638: docs, migration, and v0.7.0 release readiness.
- #648: nested fan-out/fan-in scheduler.
- #650: resume and recovery for interrupted parent and child runs.
- #651: run tree observability in status and report.
- #652: v0.7.0 doctor checks and safe fix guidance.
- #653: v0.6.x to v0.7.0 migration command and docs.

Acceptance requires all of the following evidence:

1. Self-bootstrap issue evidence: implementation issues in the milestone have
   PR links, and each PR references the issue or merged design/spec it
   implements.
2. Nested run evidence: at least one parent run launched or recorded a child
   run, and both parent and child run IDs are visible in local `status` or
   `report` JSON.
3. Recovery evidence: an interrupted, failed, or fixture child run is visible
   enough for `resume`, `status`, or `doctor` to classify the next action
   without duplicate dispatch.
4. Observability evidence: `loopcoder status --repo . --format json` and
   `loopcoder report --repo . --run <run-id> --format json` expose the run
   tree; local report records stay out of PR bodies, issue comments, commits,
   merge artifacts, docs, examples, and fixtures.
5. Registry evidence: `loopcoder projects show --repo . --format json` resolves
   the loopcoder repository and reports a stable `project_id` and
   `identity_source`.
6. Storage evidence: `loopcoder doctor --repo . --format json` reports the
   database at `$LOOPCODER_HOME/data/loopcoder.db`, and that path is outside
   the repository checkout.
7. Provider compatibility evidence: doctor JSON includes the
   `provider_compatibility[]` matrix and selected Worker/Verifier compatibility
   checks. Unsupported read-only provider combinations must fail closed or warn
   explicitly.
8. Doctor readiness evidence: `loopcoder doctor --repo .` is clean, or every
   warning is documented as optional and non-blocking for the acceptance run.
   Hard failures require fixing or a human `needs-human` decision.
9. Release readiness evidence: the release smoke passes for the candidate
   artifact when assets exist:

   ```text
   pwsh scripts/release-smoke.ps1 -Version 0.7.0
   ```

10. Human audit evidence: a reviewer can follow every accepted item from
    GitHub issue, to PR, to checks/verifier result, to local report/status run
    evidence.
11. Go/no-go evidence: the final release readiness PR or issue includes the
    completed [`v0.7.0-go-no-go.md`](v0.7.0-go-no-go.md) report, with unsigned
    or missing assets recorded as NO-GO rather than inferred success.

## Non-Acceptance Cases

Do not count the self-bootstrap as accepted when any of these are true:

- An implementation issue has no PR evidence.
- A PR claims a run/report result that is not visible in local loopcoder output.
- The local database is inside the repository or under tracked source paths.
- Parent/child run relationships are only described in prose and not visible in
  `status` or `report` JSON.
- Doctor hides provider compatibility or reports unsupported provider behavior
  as success.
- Release readiness depends on a paid service or an unavailable provider rather
  than deterministic local or GitHub release evidence.
- The path to acceptance requires an automatic production merge.

## Evidence Template

Record the final acceptance evidence in the release checklist or PR summary:

```text
self-bootstrap smoke: pwsh scripts/self-bootstrap-smoke.ps1
registry project_id: <project_id>
database path: <LOOPCODER_HOME>/data/loopcoder.db outside repo: yes
parent run: <run-id>
child run: <run-id>
status JSON: <local path or command output location>
report JSON: <local path or command output location>
doctor JSON: <local path or command output location>
implementation issues: #<n> -> PR #<n>, ...
release smoke: pwsh scripts/release-smoke.ps1 -Version 0.7.0
human audit decision: pass | fail | needs-human
```
