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
  a **separate non-admin agent identity**, and branch protection that agents
  cannot bypass.
- Until the agent uses a bot account without admin/bypass rights, treat
  authorization as **best-effort CI policy** plus human review.

See also: [`pre-prod-branch-protection.md`](pre-prod-branch-protection.md).

## Fail-closed implementation authorization

For every PR that is **not pure documentation** (including Go, workflows,
hooks, scripts, configuration, and policy code):

1. GitHub must report **exactly one** `closingIssuesReferences` entry.
2. That issue must carry the owner-applied label **`implementation-authorized`**.
3. The following **do not** authorize implementation:
   - `status:ready`
   - absence of `status:planned`
   - any issue/PR body text (including former `Implementation authorization: granted`)
4. A `status:planned` issue may proceed **only if** it also has
   `implementation-authorized`.
5. Pure documentation-only path changes remain exempt (no closing issue required).

Issue links are taken only from GitHub **`closingIssuesReferences`** (GraphQL),
not from a local regex parser over PR bodies.

## Required CI on pull requests

Authoritative product quality gates remain the PR jobs named:

- `verify`
- `test`
- `race`
- `security`

`verify` also runs implementation-authorization evaluation.

## Integrated pre-prod SHA (bounded, not a full re-suite)

Workflow `.github/workflows/pre-prod-integration.yml` runs on every `pre-prod`
push with **two distinct** check names only:

| Check | Purpose | Ceiling |
| --- | --- | --- |
| `integration-verify` | YAML validity, stabilization policy tests, authorization fixtures | 5 minutes |
| `integration-canary` | Provider-free build + evidence tests + pre-push sentinel | 5 minutes wall clock |

It does **not** re-run `go test ./...`, the full race suite, or PR security
suites. Those remain PR-authoritative.

Before starting the next non-documentation feature item, the **exact** pre-prod
base SHA must show green `integration-verify` and `integration-canary`
(newest check run per name, GitHub Actions app). Helper:

```bash
bash scripts/assert-pre-prod-green.sh --sha <full-sha>
```

This helper is a **developer convenience and CI input**, not a
repository-enforced merge hold by itself. Enforcement of “base SHA green”
for future implementation PRs is evaluated inside the authorization check
on those PRs.

### One-time bootstrap exception

The stabilization PR that **closes only #1092** may land against a pre-prod
base SHA that has not yet produced integration checks (bootstrap). That
exception is code-gated to closing issue number `1092` only and must not be
reused for `#1108` or other catalog items.

## Local hooks

```bash
git config core.hooksPath hooks
git config --get core.hooksPath   # must print: hooks
```

`hooks/pre-push` runs only `scripts/pre-push-sentinel.sh` (local-focused,
under 60s). It never runs `go test ./...`.

## Out of scope

- Implementing `#1108` or product features
- LoopCoder self-bootstrap / compile / dispatch / tick
- Silent GitHub admin or branch-protection API changes
