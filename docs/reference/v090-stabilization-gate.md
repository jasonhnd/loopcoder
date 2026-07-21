# v0.9.0 Stabilization Control Gate

Status: **active control gate** before further v0.9.0 feature development

Freeze point (owner-recorded):

| Branch | SHA |
| --- | --- |
| `main` | `2d5faf8d9aaa6d9dcb723537e0c87aaef9032d87` |
| `pre-prod` | `1a6fd6bd6a87232b23db2f6fa06de299604cf57e` |

Related: GitHub issue `#1092` (V090-002 evidence tiers + stabilization).

## Trust boundary (honest)

PR CI **executes code from the pull request branch**, including this
authorization checker. A PR can attempt to weaken its own gate. Therefore:

- This is **not** tamper-proof enforcement.
- Real enforcement requires **CODEOWNERS** review of the control plane,
  a **separate agent identity**, and branch protection the agent cannot bypass.
- Bots with ordinary write/triage can often **apply labels**. Target agents must
  use either:
  - a **fork identity** without upstream triage/write; or
  - a **fine-grained GitHub App/token without Issues label-write**.
- Until that separation exists, treat authorization as **best-effort CI policy**
  plus human review.

See also: [`pre-prod-branch-protection.md`](pre-prod-branch-protection.md).

## Fail-closed implementation authorization

For every PR that is **not pure documentation** (including Go, workflows,
hooks, scripts, configuration, and policy code):

1. Resolve **exactly one** issue pointer:
   - preferred: GitHub GraphQL `closingIssuesReferences`;
   - if empty and the PR base is **not** the repository default branch: exactly
     one structured PR label `closes:<number>` (for example `closes:1092`).
2. That issue is an **untrusted pointer** until verified:
   - issue state is **OPEN**;
   - issue currently carries label **`implementation-authorized`**;
   - GitHub label events show the **latest apply** of that label was by the
     repository owner (`jasonhnd`);
   - if `closes:<N>` is used, its latest apply actor on the PR must also be the
     owner.
3. The following **never** authorize: `status:ready`, absence of
   `status:planned`, free-text body phrases.
4. Pure documentation-only path changes remain exempt.

Free-text body parsing is not used for authorization.

## Required CI on pull requests

Authoritative product quality gates remain the PR jobs:

- `verify` (includes authorization)
- `test`
- `race`
- `security`

Outside `IMPLEMENTATION_AUTH_OFFLINE=1` (fixture tests only), missing `gh`,
repository, PR context, API data, issue state, or label-event evidence fails
closed (nonzero exit). There is **no** skip env override for production checks.

## Integrated pre-prod SHA (bounded)

Workflow `.github/workflows/pre-prod-integration.yml` runs **only on push to
`pre-prod`** (no `workflow_dispatch`) with two distinct check names:

| Check | Purpose | Ceiling |
| --- | --- | --- |
| `integration-verify` | YAML + policy + authorization fixtures | 5 minutes |
| `integration-canary` | Isolated `LOOPCODER_HOME`, register, doctor/report JSON, worktree unchanged | 5 minutes wall clock |

It does **not** re-run `go test ./...`, full race, or PR security suites.

`bash scripts/assert-pre-prod-green.sh --sha <full>` requires, for each check:

- non-empty GitHub Actions app identity;
- evidence the run is from `.github/workflows/pre-prod-integration.yml` /
  push / `pre-prod` / exact head SHA;
- newest completed **success** only.

Same-name jobs from other workflows are rejected.

Future non-documentation PRs fail when their exact pre-prod base SHA lacks
these checks, except the one-time bootstrap identity below.

### One-time bootstrap exception (all fields required)

Only when **all** match:

| Field | Value |
| --- | --- |
| PR number | `1218` |
| Head branch | `ordinary/v090-stabilization-gate` |
| Base branch | `pre-prod` |
| Base SHA | `1a6fd6bd6a87232b23db2f6fa06de299604cf57e` |
| Closing issue | `1092` |

Any mismatched field disables the exception.

## Local hooks

```bash
git config core.hooksPath hooks
git config --get core.hooksPath   # must print: hooks
```

## Out of scope

- Implementing `#1108` or product features
- LoopCoder self-bootstrap / compile / dispatch / tick
- Silent GitHub admin or branch-protection API changes
