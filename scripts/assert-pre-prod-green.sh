#!/usr/bin/env bash
# Assert an exact SHA has green integration checks from pre-prod-integration only.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

SHA=""
QUIET=0
REQUIRE_ONLY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --sha) SHA="${2:-}"; shift 2 ;;
    --quiet) QUIET=1; shift ;;
    --require) REQUIRE_ONLY="${2:-}"; shift 2 ;;
    -h|--help)
      echo "Usage: $0 --sha <full-sha> [--require integration-verify|integration-canary] [--quiet]"
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if ! command -v gh >/dev/null 2>&1; then
  echo "assert-pre-prod-green: gh not installed" >&2
  exit 2
fi

if [[ -z "${SHA}" ]]; then
  git fetch origin pre-prod --quiet
  SHA="$(git rev-parse origin/pre-prod)"
fi
if [[ ${#SHA} -lt 40 ]]; then
  SHA="$(git rev-parse "${SHA}" 2>/dev/null || echo "${SHA}")"
fi
if [[ ${#SHA} -lt 40 ]]; then
  echo "assert-pre-prod-green: need full commit SHA, got ${SHA}" >&2
  exit 2
fi

[[ ${QUIET} -eq 1 ]] || {
  echo "evidence_tier=merge-sha"
  echo "tested_commit_sha=${SHA}"
  echo "assert-pre-prod-green: evaluating ${SHA}"
}

REQUIRED=(integration-verify integration-canary)
if [[ -n "${REQUIRE_ONLY}" ]]; then
  REQUIRED=("${REQUIRE_ONLY}")
fi

export NO_COLOR=1 CLICOLOR=0 GH_FORCE_TTY=0
python3 - "${SHA}" "${REQUIRED[@]}" <<'PY'
import json, subprocess, sys, os
from datetime import datetime

sha = sys.argv[1]
required = sys.argv[2:]
env = dict(os.environ)
env["NO_COLOR"] = "1"
env["CLICOLOR"] = "0"
env["GH_FORCE_TTY"] = "0"

raw = subprocess.check_output([
    "gh", "api",
    f"repos/{{owner}}/{{repo}}/commits/{sha}/check-runs",
    "--paginate",
    "-H", "Accept: application/vnd.github+json",
], text=True, env=env)
if not raw.strip():
    print("assert-pre-prod-green: empty check-runs response", file=sys.stderr)
    sys.exit(1)

runs = []
dec = json.JSONDecoder()
idx = 0
text = raw.strip()
while idx < len(text):
    while idx < len(text) and text[idx].isspace():
        idx += 1
    if idx >= len(text):
        break
    obj, end = dec.raw_decode(text[idx:])
    idx += end
    if isinstance(obj, dict) and "check_runs" in obj:
        runs.extend(obj["check_runs"])
    elif isinstance(obj, list):
        runs.extend(obj)

def parse_ts(r):
    for k in ("completed_at", "started_at"):
        v = r.get(k)
        if v:
            try:
                return datetime.fromisoformat(v.replace("Z", "+00:00"))
            except Exception:
                pass
    return datetime.min

def is_github_actions(r):
    app = r.get("app") or {}
    slug = (app.get("slug") or "").lower().strip()
    name = (app.get("name") or "").lower().strip()
    if not slug and not name:
        return False  # empty identity rejected
    if slug == "github-actions":
        return True
    if "github" in name and "action" in name.replace(" ", ""):
        return True
    return False

def workflow_path(r):
    # Prefer check_suite / workflow name fields when present
    suite = r.get("check_suite") or {}
    # Some payloads include details_url like .../actions/runs/ID/job/ID
    # HTML URL sometimes embeds workflow
    # GitHub REST check-run includes:
    #   check_suite.app, name
    # Workflow path often in: r.get("html_url") linking to actions
    # Use external_id / name only as last resort
    # REST: GET check-run does not always include workflow path; use check_suite.id then API
    return (r.get("workflow_path") or
            (r.get("check_suite") or {}).get("workflow_path") or
            "")

# Keep candidates that match identity constraints, then pick newest success per name.
candidates = {name: [] for name in required}
rejected = []
for r in runs:
    name = r.get("name") or ""
    if name not in candidates:
        continue
    if not is_github_actions(r):
        rejected.append("%s:empty_or_non_actions_app" % name)
        continue
    # Fetch workflow path via check suite / run if needed
    path = workflow_path(r)
    details = r.get("details_url") or r.get("html_url") or ""
    # Require pre-prod-integration workflow markers in details or explicit path
    if path and path != ".github/workflows/pre-prod-integration.yml":
        rejected.append("%s:workflow_path=%s" % (name, path))
        continue
    if not path:
        # details_url for GHA jobs usually contains /actions/runs/
        if "/actions/runs/" not in details:
            rejected.append("%s:missing_actions_run_identity" % name)
            continue
        # Further constrain: check-suite head_branch when present
        suite = r.get("check_suite") or {}
        head_branch = (suite.get("head_branch") or r.get("head_branch") or "")
        # head_sha must match
        head_sha = (suite.get("head_sha") or r.get("head_sha") or "")
        if head_sha and not (head_sha == sha or sha.startswith(head_sha) or head_sha.startswith(sha)):
            rejected.append("%s:head_sha_mismatch" % name)
            continue
        if head_branch and head_branch != "pre-prod":
            rejected.append("%s:head_branch=%s" % (name, head_branch))
            continue
    candidates[name].append(r)

missing, failed, pending, bad = [], [], [], []
for name in required:
    lst = candidates.get(name) or []
    if not lst:
        missing.append(name)
        continue
    # Newest by completed_at
    lst.sort(key=parse_ts, reverse=True)
    r = lst[0]
    st = (r.get("status") or "").lower()
    conc = (r.get("conclusion") or "").lower()
    if st != "completed":
        pending.append(name)
    elif conc != "success":
        failed.append("%s:%s" % (name, conc or st))

if missing or failed or pending:
    print("assert-pre-prod-green: NOT GREEN", file=sys.stderr)
    if missing:
        print("missing:", ", ".join(missing), file=sys.stderr)
    if pending:
        print("pending:", ", ".join(pending), file=sys.stderr)
    if failed:
        print("failed:", ", ".join(failed), file=sys.stderr)
    if rejected:
        print("rejected_candidates:", "; ".join(rejected[:20]), file=sys.stderr)
    print("Required: integration-verify + integration-canary from .github/workflows/pre-prod-integration.yml on push/pre-prod", file=sys.stderr)
    sys.exit(1)
print("assert-pre-prod-green: ok", sha, "checks=", ",".join(required))
PY
