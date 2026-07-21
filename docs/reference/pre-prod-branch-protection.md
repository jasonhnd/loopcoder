# Proposed `pre-prod` Branch Protection (Owner-Applied)

This document is the **proposed** GitHub branch-protection configuration for
the ordinary-development integration branch `pre-prod` during the v0.9.0
stabilization gate.

**Repository automation must not silently change these settings.** An owner
with admin rights applies them in GitHub Settings (or via an explicit admin
`gh api` session).

## Required settings

| Setting | Required value |
| --- | --- |
| Branch | `pre-prod` |
| Require a pull request before merging | **Yes** |
| Required approving reviews | **1** (independent human reviewer; not the PR author) |
| Dismiss stale reviews when new commits are pushed | Recommended: **Yes** |
| Require review from Code Owners | Optional |
| Require status checks to pass | **Yes** |
| Require branches to be up to date before merging | **Yes** (strict) |
| Required status check contexts | Exactly: `verify`, `test`, `race`, `security` |
| Require conversation resolution | Recommended: **Yes** |
| Do not allow bypassing the above settings | **Yes** for administrators during the gate (or document any break-glass) |
| Restrict who can push | No direct pushes to `pre-prod` except break-glass |

## Required check names

These names must match both:

1. job `name:` fields in `.github/workflows/ci.yml` / resulting check runs, and
2. `.delivery.yml`:

```yaml
ci:
  checks: [verify, test, race, security]
```

Optional bots (for example Greptile Review) must **not** be listed as required
contexts unless the owner deliberately adds them to both protection and
`.delivery.yml`.

## Integrated SHA re-validation

After a PR merges to `pre-prod`, workflow
`.github/workflows/pre-prod-integration.yml` re-runs the same four check names
on the **exact merge commit**. Feature work for the next catalog item must not
start until those checks are green for that SHA
(`bash scripts/assert-pre-prod-green.sh`).

## Example admin apply (manual; not run by ordinary agents)

```bash
# Illustrative only — owner executes after reviewing this document.
gh api -X PUT "repos/jasonhnd/loopcoder/branches/pre-prod/protection" \
  -H "Accept: application/vnd.github+json" \
  --input - <<'JSON'
{
  "required_status_checks": {
    "strict": true,
    "contexts": ["verify", "test", "race", "security"]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": {
    "required_approving_review_count": 1,
    "dismiss_stale_reviews": true
  },
  "restrictions": null,
  "required_conversation_resolution": true,
  "allow_force_pushes": false,
  "allow_deletions": false
}
JSON
```

## Readback verification

```bash
gh api "repos/jasonhnd/loopcoder/branches/pre-prod/protection" --jq '{
  contexts: .required_status_checks.contexts,
  strict: .required_status_checks.strict,
  approvals: .required_pull_request_reviews.required_approving_review_count,
  enforce_admins: .enforce_admins.enabled
}'
```

Expected readback: four contexts, `strict=true`, approvals `>= 1`.
