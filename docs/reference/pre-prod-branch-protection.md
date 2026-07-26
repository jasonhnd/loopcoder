# `pre-prod` Branch Protection — Bootstrap vs Target

Automation in this repository **must not** modify GitHub administration
settings. An owner applies protection manually.

## Hard prerequisite before “one approval” is meaningful

Today the repository effectively has a **single human collaborator**
(`@jasonhnd`). If branch protection requires one approving review while the
same account authors and must approve, the workflow **deadlocks**.

Therefore:

1. **Development agents must not be able to apply authorization labels** on the
   upstream repository. Prefer either:
   - a **fork identity** without upstream triage/write; or
   - a **fine-grained GitHub App/token without Issues label-write**.
   Ordinary write/triage on the upstream repo can apply labels and is therefore
   insufficient as a trust boundary for `implementation-authorized`.
2. Agents may open PRs with write access but **no admin and no bypass** of
   branch protection.
3. **`@jasonhnd` remains owner/reviewer**.
4. Only after that identity split may protection require **one owner approval**.

CODEOWNERS for the authorization control plane is owned by `@jasonhnd` so that,
after identity separation, the bot cannot merge control-plane changes without
owner review.

## Bootstrap mode (current single-account reality)

Documented **honest** bootstrap settings for `pre-prod` while only one human
can approve:

| Setting | Bootstrap value |
| --- | --- |
| Require a pull request before merging | **Yes** |
| Required approving review count | **0** (avoid single-account deadlock) |
| Require status checks to pass | **Yes** |
| Require branches to be up to date (strict) | **Yes** |
| Required status check contexts (PR merge) | `verify`, `test`, `race`, `security` |
| Enforce admins | Prefer **Yes** once bot has no admin |
| Allow force pushes / deletions | **No** |

Post-merge integration checks `integration-verify` and `integration-canary`
are **not** PR merge required contexts; they run on the integrated SHA after
merge and gate **starting the next feature** via authorization base-SHA
evaluation.

## Target mode (after separate agent identity)

| Setting | Target value |
| --- | --- |
| Require a pull request before merging | **Yes** |
| Required approving reviews | **1** from `@jasonhnd` (or CODEOWNERS) |
| Dismiss stale reviews on new commits | **Yes** |
| Require review from Code Owners | **Yes** for control-plane paths |
| Require status checks (strict) | **Yes** |
| Required PR contexts | `verify`, `test`, `race`, `security` |
| Restrict push / no admin bypass for bot | **Yes** |

## Required PR check names

Must match `.delivery.yml`:

```yaml
ci:
  checks: [verify, test, race, security]
```

Optional bots (for example Greptile) must not be required unless deliberately
added to both protection and `.delivery.yml`.

## Readback (owner)

```bash
gh api "repos/jasonhnd/loopcoder/branches/pre-prod/protection" --jq '{
  contexts: .required_status_checks.contexts,
  strict: .required_status_checks.strict,
  approvals: .required_pull_request_reviews.required_approving_review_count,
  enforce_admins: .enforce_admins.enabled
}'
```
