# v0.9.0 Stabilization Control Gate

Status: **active control gate** before further v0.9.0 feature development  
Freeze point (owner-recorded):

| Branch | SHA |
| --- | --- |
| `main` | `2d5faf8d9aaa6d9dcb723537e0c87aaef9032d87` |
| `pre-prod` | `1a6fd6bd6a87232b23db2f6fa06de299604cf57e` |

Related: GitHub issue `#1092` (V090-002 evidence tiers + stabilization extension).

## Purpose

Ordinary v0.9.0 development advanced through P0–P2 items quickly. This gate
stops feature expansion until repository controls prevent:

1. implementing catalog items still labeled `status:planned` without explicit
   authorization;
2. treating catalog publication as implementation permission;
3. skipping required PR checks (`verify`, `test`, `race`, `security`);
4. advancing to the next work item before the **integrated** `pre-prod` SHA is
   re-validated; and
5. relying on a global full-repository pre-push hook instead of the repository
   local-focused sentinel.

## Required controls (repository-enforced)

| Control | Mechanism |
| --- | --- |
| Reject `status:planned` implementation | `scripts/check-implementation-authorization.sh` in PR `verify` + pure policy in `internal/evidence` |
| Explicit implementation authorization | Label `status:authorized` / `implementation-authorized`, or body text `Implementation authorization: granted` |
| PR required checks | `.delivery.yml` `ci.checks: [verify, test, race, security]` + CI jobs of the same names |
| Integrated SHA re-check | `.github/workflows/pre-prod-integration.yml` on every `pre-prod` push |
| Next-item hold | Developers run `bash scripts/assert-pre-prod-green.sh` before starting a new feature; documented hold in ROADMAP |
| Local pre-push budget | `core.hooksPath=hooks` → `hooks/pre-push` → `scripts/pre-push-sentinel.sh` (never `go test ./...`) |

## Explicit authorization (English contract)

Catalog publication **never** authorizes implementation. An issue remains blocked
when it carries `status:planned` unless **one** of the following is true:

1. Labels include `status:authorized` or `implementation-authorized`; or
2. Issue body contains the exact grant phrase (case-insensitive):

   ```text
   Implementation authorization: granted
   ```

`status:ready` alone authorizes only when `status:planned` is **absent**.

## Local Git hooks configuration

In each developer clone of this repository:

```bash
git config core.hooksPath hooks
git config --get core.hooksPath   # must print: hooks
```

With `core.hooksPath=hooks`, Git runs `hooks/pre-push` and does **not** use a
global hooks directory that may run full-repository tests.

Manual equivalent without install:

```bash
bash scripts/pre-push-sentinel.sh
```

## Before starting the next feature item

1. Confirm owner freeze / gate status.
2. `git fetch origin pre-prod && bash scripts/assert-pre-prod-green.sh`
3. Confirm the issue is not `status:planned` without authorization.
4. Open ordinary branch / PR against `pre-prod` only after the above pass.

## Branch protection (owner-applied; not auto-modified)

See [`pre-prod-branch-protection.md`](pre-prod-branch-protection.md). Automation
in this repository **documents** the required settings and does **not** call
GitHub administration APIs to change protection.

## Rollback

- Revert the stabilization PR to remove authorization CI step and pre-prod
  integration workflow.
- Treat such a revert as a temporary safety regression; do not resume feature
  work while `status:planned` rejection is disabled.
