#!/usr/bin/env bash
# Assert that an exact pre-prod (or other) SHA has green integration checks.
# Distinct check names: integration-verify, integration-canary.
# Selects the newest completed check run per name; validates github-actions app.
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
      echo "Usage: $0 [--sha <full-or-prefix>] [--require integration-verify|integration-canary] [--quiet]"
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

# Expand short SHA if needed.
if [[ ${#SHA} -lt 40 ]]; then
  SHA="$(git rev-parse "${SHA}" 2>/dev/null || echo "${SHA}")"
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

# Fetch check runs for this commit; pick newest completed per name.
# Validate app/slug is github-actions (or Actions).
python3 - "${SHA}" "${REQUIRED[@]}" <<'PY'
import json, subprocess, sys
from datetime import datetime

sha = sys.argv[1]
required = sys.argv[2:]

raw = subprocess.check_output([
    "gh", "api",
    f"repos/{{owner}}/{{repo}}/commits/{sha}/check-runs",
    "--paginate",
    "-H", "Accept: application/vnd.github+json",
], text=True)

# Paginated API may return concatenated JSON objects; parse robustly.
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

# Group by name, keep newest.
by_name = {}
for r in runs:
    name = r.get("name") or ""
    if name not in by_name or parse_ts(r) > parse_ts(by_name[name]):
        by_name[name] = r

missing, failed, pending, bad_app = [], [], [], []
for name in required:
    r = by_name.get(name)
    if r is None:
        missing.append(name)
        continue
    app = ((r.get("app") or {}).get("slug") or (r.get("app") or {}).get("name") or "").lower()
    # Accept GitHub Actions app identities only.
    if app and app not in ("github-actions", "github actions"):
        # Some payloads use name "GitHub Actions"
        if "github" not in app or "action" not in app.replace(" ", ""):
            bad_app.append(f"{name}:app={app}")
            continue
    st = (r.get("status") or "").lower()
    conc = (r.get("conclusion") or "").lower()
    if st != "completed":
        pending.append(name)
    elif conc != "success":
        failed.append(f"{name}:{conc or st}")

if missing or failed or pending or bad_app:
    print("assert-pre-prod-green: NOT GREEN", file=sys.stderr)
    if missing:
        print("missing:", ", ".join(missing), file=sys.stderr)
    if pending:
        print("pending:", ", ".join(pending), file=sys.stderr)
    if failed:
        print("failed:", ", ".join(failed), file=sys.stderr)
    if bad_app:
        print("bad_app:", ", ".join(bad_app), file=sys.stderr)
    print("Required distinct checks: integration-verify, integration-canary", file=sys.stderr)
    sys.exit(1)
print("assert-pre-prod-green: ok", sha, "checks=", ",".join(required))
PY
