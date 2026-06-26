# loopcoder Verification And Quality Gates

Status: DESIGN. This document describes the target verification and
quality-gate layer for loopcoder. It is not implemented yet.

Relationship to current v1: [`architecture.md`](architecture.md) describes the
built system, where the Verifier is Opus review in the conductor session plus a
read of `gh pr checks`. This document specifies the next design target: an
evidence-backed verifier and gate that decide whether a pull request is
merge-eligible.

## Problem

The current loop can dispatch work, open pull requests, and ask Opus to review
the resulting diff. That is useful, but it is not a quality gate.

Today, "verify" means the Verifier reads the diff, compares it with the issue
body, optionally checks `gh pr checks`, and reports concerns in chat. There is
no enforced rule that a pull request must have green automated checks before it
is presented as merge-eligible. There is also no structured proof that the code
implements the merged design document that authorized the code issue.

That gap is fundamental. A delivery loop is only as trustworthy as its ability
to check its own work. Autonomous loops that are stronger than a chat relay make
their feedback loop executable: required type checks, tests, builds, and hosted
CI must be green before code is considered ready. Ralph from `snarktank/ralph`
is an explicit reference point: it runs quality checks before committing and
uses browser verification for UI behavior. loopcoder should adopt the same
evidence-before-ready discipline while adding its doc-first advantage.

loopcoder's unique advantage is the doc-first contract in
[`PROCESS.md`](PROCESS.md). Every feature starts with a merged design or spec
document under `docs/`. The code issue then implements that merged document in a
separate pull request. That means verification can be stricter than generic
"does the diff look reasonable?" review: the verifier can check the code
against a living artifact that constrains the output.

## Goals

- Make automated quality gates first-class: checks declared in `.delivery.yml`
  are run or read for every pull request, and required checks must be green
  before the PR is merge-eligible.
- Make spec-driven verification first-class: a code PR is checked against the
  merged design document that authorized it, not only against the issue body.
- Add agent-driven verification for behavior that unit tests and static checks
  miss, especially browser-visible UI flows.
- Produce explicit verdicts: `pass`, `fail`, or `needs-human`.
- Keep the human as the merge authority for v1. A passing gate means
  merge-eligible, not auto-merged.
- Keep the design compatible with the existing ports in
  [`architecture.md`](architecture.md): VcsHost supplies PRs and checks,
  Verifier produces evidence, Gate decides merge eligibility, and Reporter
  surfaces state in chat.

## Non-Goals

- No auto-merge in v1. `adapters.gate: human-merge` remains the default and the
  user still names PRs to merge.
- No replacement for product judgment. Ambiguous requirements, subjective visual
  decisions, and risk acceptance still route to the human.
- No claim that tests prove correctness. Automated checks are necessary
  evidence, not sufficient evidence for every change.
- No new long-running daemon in this design. The conductor is still the Opus
  session described in [`architecture.md`](architecture.md), and the target
  verifier runs inside that session unless a later design introduces a separate
  runtime.

## Design Principles

### Evidence Before Merge Eligibility

The verifier must produce evidence, not only prose. Evidence includes remote
check status, local command output, spec-conformance findings, browser
observations, screenshots when useful, changed file lists, and links to check
runs.

### The Design Doc Is The Contract

For code work, the merged design or spec document is the primary contract. The
issue body scopes the task, but the design document defines the behavior,
interfaces, constraints, acceptance criteria, and non-goals the implementation
must satisfy.

### Human Approval Is A Separate Gate

Verification answers "is this PR eligible to merge?" The human answers "should
we merge it now?" Those are deliberately separate decisions. A `pass` verdict
allows the conductor to present the PR as merge-eligible; it does not authorize
an unattended merge in v1.

### Failures Feed Fix Passes

Objective failures should not become chat-only warnings. A failed gate routes
back into a fix pass with the failure evidence attached. The loop should
continue until the PR passes, exhausts a configured bound, or needs human input.

## Target Loop Placement

The target loop is:

```text
dispatch
  -> worker opens PR
  -> review diff
  -> verify automated checks + spec conformance + agent behavior
  -> gate merge eligibility
  -> human names PRs to merge
  -> merge ordering and gh pr merge
```

This refines the current v1 loop without changing the scheduling model in
[`scheduling.md`](scheduling.md). Ready issues still dispatch according to the
dependency DAG. File overlap is still observed from actual PR diffs at merge
time. The new layer sits between worker-created PRs and human-directed merge.

## Model

### Inputs

The verifier receives:

- The GitHub issue number, title, body, labels, and dependency state.
- The pull request number, branch, base branch, diff, changed files, and hosted
  check runs.
- The repository `.delivery.yml`.
- The merged design document referenced by the code issue.
- The worker summary and any worker-reported command output.

Worker-reported output is useful context, but it is not authoritative gate
evidence. The verifier must independently read required hosted checks and rerun
or validate required local commands according to `.delivery.yml`.

### Outputs

The verifier produces a structured verification record:

```yaml
pr: 123
issue: 39
design_doc: docs/example.md
verdict: pass | fail | needs-human
merge_eligible: true | false
automated_checks:
  remote:
    - name: test
      required: true
      status: pass | fail | pending | missing
      url: https://github.com/owner/repo/actions/runs/...
  local:
    - group: typecheck
      command: bunx tsc --noEmit -p tsconfig.json
      required: true
      status: pass | fail | skipped
spec_conformance:
  - criterion: "The worker branches from main."
    status: pass | fail | needs-human
    evidence: "scripts/dispatch-worker.ps1 creates a worktree from origin/main."
agent_checks:
  - name: browser-flow
    status: pass | fail | needs-human | not-required
    evidence: "Visited /settings, changed theme, confirmed persistence."
risks:
  - "No explicit acceptance criteria section in design doc; inferred from Goals."
next_action: merge-eligible | fix-pass | escalate
```

The exact storage format can be JSON, YAML, or a markdown report in chat. The
important contract is the field model: verdict, merge eligibility, required
checks, spec criteria, agent observations, risks, and next action.

## `.delivery.yml` Configuration

The current `.delivery.yml` already declares adapters and contains
`ci.checks: []`. The target verification layer extends the `ci` section while
remaining backward-compatible with an empty configuration:

```yaml
ci:
  checks:
    - verify
    - Vercel
  tests:
    - bun run test
  typecheck:
    - bunx tsc --noEmit -p tsconfig.json
  build:
    - bun run build
verification:
  spec_required: true
  max_fix_passes: 3
  browser:
    enabled: auto
    globs:
      - web/**
      - app/**
      - "**/*.css"
      - "**/*.tsx"
```

The meanings are:

| Field | Meaning |
| --- | --- |
| `ci.checks` | Required hosted check names that must be green in `gh pr checks <pr>`. |
| `ci.tests` | Test commands the verifier can run in a PR checkout when the repo wants local verification. |
| `ci.typecheck` | Typecheck commands required for code PRs when present. |
| `ci.build` | Build commands required for code PRs when present. |
| `verification.spec_required` | Whether code PRs must reference a merged design/spec document. Defaults to `true` for loopcoder work. |
| `verification.max_fix_passes` | Bound on automatic fix loops before escalation. Defaults to `3` if absent. |
| `verification.browser.enabled` | `auto`, `always`, or `never`. `auto` runs browser verification when the spec, issue, or changed files indicate UI behavior. |
| `verification.browser.globs` | Changed-path patterns that trigger browser verification when browser mode is `auto`. |

All configured checks are required unless a later schema adds an explicit
`required: false` flag. Empty arrays mean no checks in that group. For code PRs,
an empty `ci` configuration is a configuration gap: the verifier may still
review the diff and spec, but it should report `needs-human` rather than
pretending the project has an automated gate.

Documentation-only PRs are different. A documentation PR can pass without test,
typecheck, or build commands when `.delivery.yml` has no documentation-specific
check. It still must satisfy the issue scope and avoid unrelated changes.

## Automated Quality Gates

### Hosted Checks

Hosted checks are authoritative for merge eligibility when they are declared in
`ci.checks`.

For each PR, the verifier runs:

```text
gh pr checks <pr>
```

The implementation should use the structured form where available:

```text
gh pr checks <pr> --json name,state,conclusion,link,startedAt,completedAt
```

Gate rules:

- A configured check name must be present.
- A configured check must be completed.
- A configured check must have a successful conclusion.
- A missing, failed, cancelled, timed-out, skipped, or still-pending required
  check makes the automated gate not pass.
- If GitHub cannot return checks because of permissions or outage, the verdict
  is `needs-human`, not `pass`.

The gate must match configured check names exactly unless a later schema
introduces explicit patterns. Exact names avoid accidental passes from unrelated
checks.

### Local Verification Commands

The verifier may also run local commands declared in `ci.tests`,
`ci.typecheck`, and `ci.build`. These commands run in a fresh checkout of the PR
head or in an isolated worktree created for verification. They do not run in the
worker's now-cleaned worktree.

Local command rules:

- Commands run from the repository root unless a later schema adds `cwd`.
- Commands run after checking out the PR head against the intended base branch.
- The verifier captures the command, exit code, and a bounded log excerpt.
- Exit code `0` is pass. Any non-zero exit is fail.
- Missing tools, dependency install failures, or environment setup failures are
  `needs-human` when the failure is environmental rather than caused by the PR.
- Commands must be deterministic enough to gate. Known flaky commands should not
  be listed as required gates until they are stabilized.

If a hosted check already runs the same command, the repo can choose to list
only the hosted check in `ci.checks`. The design does not require duplicate
execution.

### Enforcement

The Gate consumes the verification record. A PR is merge-eligible only when:

1. Every required hosted check is green.
2. Every required local command group passes.
3. Spec-driven verification passes.
4. Required agent-driven verification passes or is not required.
5. The verifier has no unresolved `needs-human` finding.

If any required automated check fails, the verdict is `fail` and the next action
is a fix pass. If checks are missing, pending beyond a configured wait, or
unreadable, the verdict is `needs-human`.

The conductor must report merge eligibility separately from the human merge
decision:

```text
PR #123: pass, merge-eligible. Waiting for you to name it for merge.
PR #124: fail, not merge-eligible. Re-dispatching fix pass with failing test output.
PR #125: needs-human, not merge-eligible. Ambiguous spec criterion: ...
```

## Spec-Driven Verification

### Document Discovery

The doc-first workflow requires code issues to reference the merged document
they implement, for example:

```text
implement per docs/verification.md
```

The verifier finds the design document by scanning, in order:

1. Explicit `docs/...md` paths in the issue body.
2. Explicit `implements`, `implement per`, `per`, or `according to` references
   in the issue body.
3. Pull request body references copied from the issue.
4. If no document is referenced, the verifier marks the PR `fail` for violating
   the doc-first contract.

The verifier must read the merged version of the document from the base branch,
not a modified copy in the PR branch. The target command shape is:

```text
git fetch origin <base-branch>
git show origin/<base-branch>:docs/<document>.md
```

This matters because the document is the contract already accepted by the
project. A worker cannot change the contract in the same PR and then claim
compliance with its own changes.

### Acceptance Criteria Extraction

Design documents should contain explicit acceptance criteria when they authorize
code work. Until every older document has that section, the verifier builds a
conformance checklist from these sections, in order:

1. `Acceptance Criteria`
2. `Goals`
3. `Interfaces`, `Ports`, `Configuration`, or similar contract sections
4. `Non-Goals`
5. `Failure Handling`, `Risks`, and `Open Questions`
6. Any imperative requirements using words such as `must`, `required`, `never`,
   or `only`

Each extracted item becomes one criterion in the verification record. The
verifier should keep criteria concrete. "Improve UX" is not directly
verifiable; "The settings page preserves the selected theme after reload" is.
When the document is too ambiguous to produce concrete criteria, the verifier
returns `needs-human`.

### Conformance Judgment

For each criterion, the verifier asks:

- Does the implementation provide the behavior, interface, configuration, or
  constraint required by the design?
- Is there evidence: code references, tests, build output, browser observation,
  or check status?
- Did the implementation avoid the document's non-goals?
- Did it introduce unrelated changes outside the issue scope?
- Did it preserve existing process constraints, especially one concern per PR
  and no bundled doc/code changes?

The result is not a general code review. It is a contract check:

```text
criterion -> evidence -> status -> impact
```

Examples:

| Criterion | Evidence | Status |
| --- | --- | --- |
| `ci.checks` gates merge eligibility. | Gate code reads `gh pr checks` and fails missing required checks. | pass |
| The human remains the merge authority. | Gate reports `merge_eligible` but does not call `gh pr merge`. | pass |
| Browser verification runs for UI globs. | No implementation or tests cover UI changed paths. | fail |
| The design forbids auto-merge. | PR adds auto-merge on pass. | fail |

Spec conformance fails when a required criterion lacks evidence or is
contradicted by the diff. It becomes `needs-human` when the criterion cannot be
interpreted safely without product judgment.

### Why This Is Loopcoder's Edge

Generic autonomous loops can check whether code compiles and tests pass. They
cannot reliably know whether the code implements the intended product decision
unless the intent is captured somewhere stable. loopcoder's doc-first contract
creates that stable artifact. The verifier can read the merged design document,
extract the promises it makes, and reject code that passes tests but violates
the spec.

That is the difference between "the code works somehow" and "the code
implements the agreed design."

## Agent-Driven Verification

Automated tests and static checks miss important behavior:

- UI rendering, navigation, and interaction flows.
- Browser console errors.
- Responsive layout regressions.
- End-to-end behavior across routes or screens.
- User-visible copy or state transitions that are difficult to assert in unit
  tests.

For those cases, the verifier can launch an agent-driven check. In v1 target
terms, this is still the Verifier role, but it uses tools such as a headless
browser to inspect the running PR.

### Triggers

Agent-driven browser verification runs when any of these are true:

- `verification.browser.enabled: always`.
- `verification.browser.enabled: auto` and changed files match
  `verification.browser.globs`.
- The design document or issue acceptance criteria mention UI, browser,
  navigation, forms, visual state, screenshots, accessibility, or manual
  verification.
- Automated checks pass, but spec conformance depends on behavior that cannot
  be judged from code and tests alone.

### Procedure

The target browser verification procedure is:

1. Check out the PR head in an isolated verification workspace.
2. Install dependencies if the repo's setup requires it.
3. Start the app or preview command declared by the repo.
4. Navigate to the routes or screens named by the design document.
5. Perform the user actions required by the acceptance criteria.
6. Check DOM state, visible text, URL transitions, persistence, console errors,
   network failures, and screenshots where relevant.
7. Record evidence and stop the server.

The verifier should prefer deterministic assertions over visual opinion. For
example, "the Save button remains disabled until required fields are filled" is
deterministic. "The page looks nicer" is not and should route to `needs-human`
if it matters.

### Results

Agent-driven verification can produce:

- `pass`: the agent confirmed the behavior required by the spec.
- `fail`: the behavior is objectively missing or broken.
- `needs-human`: the behavior is subjective, the environment cannot be started,
  authentication is unavailable, or the agent cannot safely determine the
  intended result.
- `not-required`: no agent-driven check was triggered.

Browser verification does not replace tests. It covers the behavior tests often
miss.

## Verdict Model

### `pass`

`pass` means all required evidence is green:

- Required hosted checks are present and successful.
- Required local commands pass.
- The implementation conforms to the merged design document.
- Required agent-driven checks pass or are not required.
- No unresolved ambiguity or human risk decision remains.

Routing: mark the PR merge-eligible, report the evidence in chat, and wait for
the user to name the PR for merge.

### `fail`

`fail` means there is an objective problem the loop can attempt to fix:

- A required check failed.
- A required local command failed.
- The diff violates the design document.
- A required acceptance criterion is not implemented.
- Browser verification found a reproducible broken behavior.
- The PR includes unrelated changes that should be removed.

Routing: create a fix-pass prompt containing the failed criteria, command
output, check links, changed files, and relevant spec excerpts. Re-dispatch the
worker against the same issue and PR branch when supported, or open a follow-up
fix PR if the adapter cannot amend the existing PR. Stop and escalate after
`verification.max_fix_passes`.

### `needs-human`

`needs-human` means the verifier cannot safely decide:

- The spec is ambiguous or internally inconsistent.
- The PR needs product judgment or visual taste judgment.
- Required checks are unavailable because of infrastructure, permissions, or
  missing secrets.
- The verifier cannot run the app because setup is undocumented.
- The change touches high-risk areas and the configured policy requires human
  review.
- The design document is missing acceptance criteria and inference would be
  unsafe.

Routing: report a concise question to the human, include the evidence already
collected, and do not mark the PR merge-eligible. Dependents remain blocked
until the human resolves the question or approves a narrowed fix pass.

## Gate Behavior

The Gate is a policy function over the verification record:

```text
Gate.evaluate(record, .delivery.yml) -> {
  verdict,
  merge_eligible,
  next_action,
  human_message,
  fix_context?
}
```

Gate rules are intentionally simple for v1:

- `pass` -> `merge_eligible: true`, `next_action: wait-for-human-merge`.
- `fail` -> `merge_eligible: false`, `next_action: fix-pass`.
- `needs-human` -> `merge_eligible: false`, `next_action: escalate`.

The Gate never calls `gh pr merge` on its own while `.delivery.yml` uses
`adapters.gate: human-merge`. Merge still happens only when the user names one
or more PRs. At that point, the conductor follows [`scheduling.md`](scheduling.md):
it reads real changed files, groups overlapping PRs, rebases where needed, and
runs `gh pr merge` only for named PRs that remain merge-eligible.

## Interfaces

### Verifier Port

Target contract:

```text
Verifier.verify(change, item, config) -> VerificationRecord
```

Inputs:

- `change`: PR number, branch, base, diff, changed files, check runs.
- `item`: issue number, body, labels, dependency state, referenced design doc.
- `config`: parsed `.delivery.yml`.

Responsibilities:

- Read the PR diff and changed files.
- Read required hosted checks through VcsHost.
- Run configured local verification commands when required.
- Resolve and read the merged design document.
- Build and evaluate the spec-conformance checklist.
- Run agent-driven verification when triggered.
- Return `pass`, `fail`, or `needs-human` with evidence.

### VcsHost Port

Existing VcsHost responsibilities expand from "read checks" to "provide
structured check evidence":

```text
VcsHost.checks(pr) -> CheckRun[]
VcsHost.diff(pr) -> Diff
VcsHost.changedFiles(pr) -> string[]
VcsHost.checkout(pr, targetPath) -> Workspace
```

The v1 GitHub adapter can implement these with:

```text
gh pr checks <pr> --json name,state,conclusion,link,startedAt,completedAt
gh pr diff <pr>
gh pr diff <pr> --name-only
gh pr checkout <pr>
```

### Gate Port

Target contract:

```text
Gate.decide(change, verificationRecord, config) -> GateDecision
```

Responsibilities:

- Convert verification evidence into merge eligibility.
- Route failed PRs to fix pass.
- Route ambiguous PRs to the human.
- Preserve the v1 human-merge rule.

### Reporter Port

Reporter output must separate status from evidence:

```text
PR #123 - pass, merge-eligible
Required checks: verify pass, Vercel pass
Spec: 7/7 criteria pass against docs/example.md
Agent: browser-flow not required
Next: waiting for user-named merge
```

The chat report should be compact, but the conductor should keep enough detail
to build a fix-pass prompt without redoing the whole review.

## Failure Handling

### Red Checks

Red required checks produce `fail`. The fix pass receives the check name, link,
conclusion, and relevant log excerpt when available.

### Pending Checks

Pending required checks are not pass. The conductor waits up to a configured or
reasonable timeout, then reports `needs-human` if checks remain pending. It
should not merge while required checks are pending.

### Missing Checks

If `.delivery.yml` declares a required check and `gh pr checks` does not return
it, the gate fails or escalates:

- `fail` when the check should exist for this PR and absence indicates CI did
  not run.
- `needs-human` when the repo or permissions make the absence unclear.

### Flaky Checks

The verifier should not hide flakes. If a command or hosted check is known
flaky, the project should either fix it or remove it from required gates. A
single rerun can be allowed by policy, but a pass after rerun should still note
the flake in the verification record.

### Missing Design Document

A code issue without a merged design document fails the doc-first contract. The
verifier returns `fail` when the reference is missing and `needs-human` when a
reference exists but cannot be resolved because of repository state or
permissions.

### Ambiguous Design

If the design document lacks concrete acceptance criteria, the verifier may
infer criteria from goals and normative language. If inference would determine
product behavior rather than verify it, the verdict is `needs-human`.

## Decisions

### Decision 1: Required Hosted Checks Are Read From `gh pr checks`

Rationale: GitHub is already the v1 VcsHost adapter, and branch protection and
CI status are visible through `gh pr checks`. Using the same source avoids a
parallel local-only gate that can disagree with GitHub.

Consequence: `.delivery.yml` must name the required hosted checks accurately.
Misnamed checks block merge eligibility instead of silently passing.

### Decision 2: Local Commands Are Declared By Category

Rationale: `tests`, `typecheck`, and `build` are different kinds of evidence.
Keeping them separate makes reports clearer and lets repos add only the gates
they can support.

Consequence: The verifier can say exactly what failed: test, typecheck, build,
or hosted CI.

### Decision 3: Spec Conformance Is Required For Code PRs

Rationale: The doc-first process is not just planning ceremony. Its value is
that a merged artifact constrains implementation. If verification ignores the
document, loopcoder loses its main advantage over generic autonomous workers.

Consequence: A code PR that passes tests but violates the design is not
merge-eligible.

### Decision 4: Browser Verification Is Agent-Driven And Triggered By Policy

Rationale: UI behavior often cannot be verified from static review or unit
tests. Ralph's browser verification points to the right pattern: the loop
should inspect the running behavior when the change is visual or interactive.

Consequence: Browser verification needs app startup commands and may sometimes
return `needs-human` because local secrets, auth, or subjective design judgment
are unavailable.

### Decision 5: `needs-human` Is Separate From `fail`

Rationale: Some problems are fixable by another worker pass; others require a
human decision. Conflating them causes either blind retries or premature
blocking.

Consequence: The conductor can keep momentum on objective failures while
escalating only the decisions automation should not make.

### Decision 6: Human-Merge Remains The v1 Gate Adapter

Rationale: Current loopcoder docs consistently state that the user names PRs to
merge. Verification should strengthen merge eligibility, not quietly change the
merge authority.

Consequence: A passing gate reports readiness. It does not merge.

## Relationship To Existing Docs

- [`PROCESS.md`](PROCESS.md) defines the doc-first contract this verifier
  enforces.
- [`architecture.md`](architecture.md) defines the v1 ports and current
  Verifier/Gate adapters.
- [`scheduling.md`](scheduling.md) defines dispatch, dependency, file-overlap,
  and merge-ordering behavior. Verification sits before merge ordering.
- [`worker.md`](worker.md) defines how a worker turns an issue into a PR. The
  verifier must not trust worker self-report as final gate evidence.
- [`../SKILL.md`](../SKILL.md) is the conductor playbook that should eventually
  call this target verification procedure.
- [`../.delivery.yml`](../.delivery.yml) is the per-repo configuration surface
  that declares checks and adapters.

## References

- `snarktank/ralph`: reference for autonomous loop discipline where typecheck,
  tests, CI, and browser verification are part of the work loop before code is
  considered ready: <https://github.com/snarktank/ralph>
- Spec-driven verification: loopcoder's merged design document is a living
  artifact that constrains output; the verifier checks compliance before merge
  eligibility.
- Doc-first contract: [`PROCESS.md`](PROCESS.md) requires design first, code
  from the merged document, and verification last.

