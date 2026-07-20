# Development and Release Checklist

This living checklist applies the mandatory workflow in
[`../PROCESS.md`](../PROCESS.md) and the stricter self-hosting controls in
[`../self-hosting-playbook.md`](../self-hosting-playbook.md).

Use one copy per version. Store final evidence in that version's GO/NO-GO
record; do not paste machine-local reports, credentials, absolute paths, or raw
provider transcripts into GitHub.

## State Definitions

| State | Required evidence |
| --- | --- |
| Planned | Scope, non-goals, accepted specs, dependency graph, issue and resource budgets. |
| Code-complete | Every planned implementation PR merged; implementation checks pass; no planned code issue remains open. |
| Candidate-complete | One protected `main` SHA passes integrated checks and is frozen for release. |
| Staged | Exact candidate archive, checksum, and signature exist in a diagnosable draft release. |
| Smoke-complete | Exact staged artifact passes install, upgrade, migration, rollback, self-bootstrap, and version checks. |
| Release-complete | Human publication gate approved; release public; post-publication verification passes; evidence and milestone closed. |

Do not use “done” without naming which state is meant.

## A. Version Intake

- [ ] Write one literal user outcome for the version.
- [ ] List explicit non-goals.
- [ ] Limit the minor release to 8–12 implementation issues.
- [ ] Limit dependency depth to 3.
- [ ] Confirm each issue has one primary behavior and no more than five
      independent acceptance criteria.
- [ ] Identify storage, migration, security, provider, host, and release blast
      radius.
- [ ] Assign every consequential decision to human, deterministic policy,
      planner, router, Worker, Verifier, or release operator.
- [ ] Identify the public evidence required for every acceptance criterion.
- [ ] Define P0, P1, and release-contract blockers before implementation.
- [ ] Define the RC freeze date or condition.
- [ ] Define the maximum of two failed release candidates.
- [ ] Record deferred work in the next milestone instead of silently retaining
      it in the current version.

### Intake evidence

```text
version:
literal outcome:
implementation issue count:
maximum dependency depth:
accepted non-goals:
release blocker definition:
RC entry condition:
next milestone for deferrals:
```

## B. Design

- [ ] Open a documentation issue before a code issue.
- [ ] Write the accepted spec under `docs/specs/<NNNN>-<slug>.md`.
- [ ] Include goals and non-goals.
- [ ] Include typed lifecycle and error semantics.
- [ ] Include identity, idempotency, replay, crash, and migration behavior.
- [ ] Include authority, side effect, approval, and privacy boundaries.
- [ ] Include human and JSON output contracts.
- [ ] Include failure honesty for unknown, stale, unavailable, and ambiguous
      evidence.
- [ ] Include rollback or compatibility behavior.
- [ ] Include acceptance mapping.
- [ ] Review and merge the spec before opening implementation work.

## C. Implementation Issue Readiness

- [ ] Reference the exact accepted spec.
- [ ] State one primary implementation outcome.
- [ ] List no more than five acceptance criteria.
- [ ] List files or ownership boundaries likely to change.
- [ ] Identify focused tests.
- [ ] Identify migration and rollback tests when applicable.
- [ ] Identify public documentation updates.
- [ ] State what is explicitly out of scope.
- [ ] Estimate one Worker attempt at 30 minutes or less.
- [ ] Define the commit/PR delivery checkpoint.
- [ ] Define how a delivery-only failure resumes without another provider call.

## D. Local Launch Gate

- [ ] No other Worker or Verifier provider is active.
- [ ] No local full-suite or full-race command is active.
- [ ] Local CPU, memory, swap, and thermal pressure are acceptable.
- [ ] The planned process tree fits the default limit of eight children.
- [ ] The task has a 10-minute soft timeout and 15-minute hard timeout.
- [ ] Provider-native sub-agents are disabled unless separately approved.
- [ ] One writer owns the worktree.
- [ ] Base branch and candidate ancestor are current and explicit.
- [ ] Pending relay/report obligations are visible before launch.
- [ ] The five-minute progress destination is known.

## E. Worker Evidence

- [ ] Worker used the approved provider/model/permission.
- [ ] Worker changed only the issue scope.
- [ ] Changed files are formatted.
- [ ] Directly affected packages compile.
- [ ] Focused tests pass.
- [ ] No full local repository or race suite ran without explicit approval.
- [ ] A commit exists with the expected parent.
- [ ] Provider report is durable.
- [ ] Token usage is exact, unknown, or unavailable; never fabricated as zero.
- [ ] No credentials, absolute paths, or private transcripts entered the diff.

## F. Delivery Recovery

When push, PR creation, or report publication fails:

- [ ] Classify the failure as `delivery-failure`.
- [ ] Preserve the commit and worktree.
- [ ] Verify whether the remote branch already contains the commit.
- [ ] Verify whether a PR already exists.
- [ ] Retry only the missing delivery step.
- [ ] Do not invoke the Worker again.
- [ ] Retry automatically at most once.
- [ ] Return `needs-human` when remote side effects remain ambiguous.

## G. Pull Request

- [ ] PR references the issue and accepted spec.
- [ ] PR changes one concern.
- [ ] Diff contains no unrelated generated or metadata churn.
- [ ] Remote `verify` passes.
- [ ] Remote `test` passes.
- [ ] Remote `race` passes for the configured tier.
- [ ] Remote `security` passes.
- [ ] Independent verifier returns `pass`, or a human records why a
      zero-blocker `needs-human` result is acceptable.
- [ ] The Conductor reads failed job evidence before requesting a fix.
- [ ] No local full suite duplicates a pending or completed remote check.
- [ ] User-visible progress was emitted at least every five minutes while work
      was active.
- [ ] Merge target is correct.

## H. Integrated Branch

- [ ] Merge implementation to `pre-prod` through the configured gate.
- [ ] Record the merge SHA.
- [ ] Confirm required checks run on the integrated SHA.
- [ ] Revert or stop when an integrated required check fails.
- [ ] Create one protected promotion PR from `pre-prod` to `main`.
- [ ] Confirm `main` protection requires the documented contexts.
- [ ] Merge only after every required context succeeds.
- [ ] Record the final protected `main` SHA.
- [ ] Confirm no unrelated commit entered the candidate.

## I. Code-Complete Gate

- [ ] Every planned implementation issue is closed.
- [ ] Every implementation has a merged PR.
- [ ] Every merged PR is an ancestor of the candidate.
- [ ] No P0, P1, security, migration, or release-contract issue is open.
- [ ] User-facing and reference documentation describe current behavior.
- [ ] Changelog and release-note source are current.
- [ ] Known limitations are explicit.
- [ ] Deferred improvements have a future milestone or backlog entry.

## J. RC Freeze

- [ ] Record candidate SHA and freeze time in the release-readiness issue.
- [ ] Reject new feature work from the candidate.
- [ ] Permit only P0, P1, and release-contract corrections.
- [ ] Require every correction to name the failed evidence.
- [ ] Keep every correction narrowly scoped.
- [ ] Re-run only the evidence invalidated by the correction until the final
      release gate requires the complete matrix.
- [ ] Count failed candidates.
- [ ] Stop for human GO/NO-GO after the second failed candidate.

## K. Candidate and Tag

- [ ] Candidate is the protected `main` SHA.
- [ ] Integrated CI passes on that exact SHA.
- [ ] Tag is annotated and resolves to that SHA.
- [ ] No public release exists for a different SHA under the same tag.
- [ ] Any replaced unpublished candidate is documented in the release-readiness
      issue before moving the tag.
- [ ] Release workflow starts from the tag.

## L. Build and Supply Chain

- [ ] Build runs on the supported native platform.
- [ ] Full release race gate passes once before packaging.
- [ ] Archive is built exactly once.
- [ ] Archive contains only the supported platform binary and intended files.
- [ ] Version, commit, and date are stamped.
- [ ] `SHA256SUMS` contains the exact archive hash.
- [ ] Sigstore bundle signs the checksum manifest.
- [ ] Signature identity and issuer match the release workflow policy.
- [ ] Draft release contains exactly the advertised assets.
- [ ] Failed smoke leaves the draft and diagnostics available.

## M. Exact-Artifact Smoke

- [ ] Download the staged archive rather than rebuilding locally.
- [ ] Verify the archive checksum.
- [ ] Verify the Sigstore bundle.
- [ ] Extract and run `loopcoder version`.
- [ ] Confirm version, commit, platform, and date.
- [ ] Run install into a temporary location.
- [ ] Run upgrade and already-latest behavior.
- [ ] Run deterministic self-bootstrap with zero paid provider calls.
- [ ] Run the seven-case packaged nested-permission matrix and all seven
      terminal replays against the same installed candidate binary.
- [ ] Confirm executed cases launch once, replays launch zero times, and
      unsupported modes create no provider, lifecycle, claim, or progress
      state.
- [ ] Confirm read-only and write violations cannot aggregate to parent
      success, and their stable audit/reason codes are present.
- [ ] Confirm the matrix stays within 14 invocations, concurrency one, depth
      two, 20 seconds per invocation, and five minutes overall.
- [ ] Confirm any retained permission-matrix diagnostic is at most 64 KiB and
      contains no raw output, prompt, credential, or machine path.
- [ ] Confirm runtime state remains outside the registered repository.
- [ ] Confirm human and JSON output describe the same run identities and
      outcomes.
- [ ] Create predecessor-version storage with the published prior binary.
- [ ] Confirm migration planning is read-only.
- [ ] Apply migration and verify the owner-only backup.
- [ ] Confirm repeated migration is idempotent.
- [ ] Restore a copied backup and open it with the predecessor binary.
- [ ] Confirm unsupported platforms fail before side effects.

## N. Human Publication Gate

- [ ] Draft assets are the exact assets that passed smoke.
- [ ] Every pre-publication workflow job is green.
- [ ] Branch-protection readback matches policy.
- [ ] Publication environment readback names the required reviewer.
- [ ] Release GO/NO-GO record contains candidate and artifact evidence.
- [ ] Required human approves the protected publication environment.
- [ ] Publication job promotes the existing draft without rebuilding.

## O. Post-Publication Verification

- [ ] Release is public (`draft=false`).
- [ ] Release is the intended latest stable release.
- [ ] Public tag resolves to the candidate SHA.
- [ ] Public asset names and count are correct.
- [ ] Download the public archive again.
- [ ] Verify public checksum and signature.
- [ ] Run public binary version and upgrade checks.
- [ ] Confirm install documentation uses the public URL and supported platform.
- [ ] Update the GO/NO-GO record from provisional to final GO.
- [ ] Close release blockers and tracking issues only after evidence exists.
- [ ] Close the milestone.
- [ ] Record deferred operational work in the next patch milestone.

## P. Cleanup

- [ ] No Worker provider remains active.
- [ ] No Verifier provider remains active.
- [ ] No local test or watcher remains active.
- [ ] No verified orphan process group remains.
- [ ] Temporary worktrees are removed or have an explicit retained owner.
- [ ] Stale worktree registrations are reconciled.
- [ ] Temporary release evidence contains no credential or private transcript.
- [ ] Local draft branches have a documented disposition.
- [ ] Final report is user-visible.

## GO/NO-GO Evidence Matrix

Copy this table into the version's final release record.

| Requirement | Expected evidence | Result | Public reference |
| --- | --- | --- | --- |
| Scope frozen | Roadmap and RC decision | pending | |
| Code complete | Closed issues and merged PRs | pending | |
| Protected candidate | `main` SHA and integrated CI | pending | |
| Platform | Native supported tuple | pending | |
| Build | Release workflow build job | pending | |
| Full race | Release race job | pending | |
| Archive | Asset name, size, digest | pending | |
| Checksum | `SHA256SUMS` verification | pending | |
| Signature | Sigstore identity/issuer verification | pending | |
| Version stamp | Extracted binary output | pending | |
| Install/upgrade | Exact-artifact smoke | pending | |
| Self-bootstrap | Deterministic zero-paid-provider smoke | pending | |
| Migration | Prior schema to current schema | pending | |
| Backup/rollback | Verified backup opened by prior binary | pending | |
| Repository settings | Branch protection and environment readback | pending | |
| Human approval | Publication environment deployment | pending | |
| Public release | Public URL and tag SHA | pending | |
| Post-publication | Re-downloaded artifact verification | pending | |
| Open blockers | Query result equals zero | pending | |

Final decision:

```text
GO | NO-GO | NEEDS-HUMAN

reason:
accepted residual risk:
deferred work:
decision owner:
decision time:
```
