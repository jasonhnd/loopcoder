#!/usr/bin/env bash
# Assert the latest origin/pre-prod integration SHA has green required checks.
# Used before starting the next v0.9.0 feature item. Does not modify settings.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

if ! command -v gh >/dev/null 2>&1; then
  echo "assert-pre-prod-green: gh not installed; cannot query integration status" >&2
  exit 2
fi

git fetch origin pre-prod --quiet
SHA="$(git rev-parse origin/pre-prod)"
echo "evidence_tier=merge-sha"
echo "tested_commit_sha=${SHA}"
echo "assert-pre-prod-green: evaluating origin/pre-prod@${SHA}"

# Required names from .delivery.yml ci.checks (simple parse; no PyYAML).
REQUIRED="$(awk '
  $0 ~ /^ci:/ { in_ci=1; next }
  in_ci && $0 ~ /^[^[:space:]#]/ { in_ci=0 }
  in_ci && $0 ~ /checks:/ {
    line=$0
    sub(/.*\[/, "", line)
    sub(/\].*/, "", line)
    gsub(/[ ",]/, " ", line)
    print line
  }
' .delivery.yml)"
if [[ -z "${REQUIRED}" ]]; then
  REQUIRED="verify test race security"
fi

python3 - "${SHA}" ${REQUIRED} <<'PY'
import json, subprocess, sys
sha = sys.argv[1]
required = sys.argv[2:]
raw = subprocess.check_output([
    "gh", "api", f"repos/{{owner}}/{{repo}}/commits/{sha}/check-runs",
    "--paginate",
    "--jq", "[.check_runs[] | {name, status, conclusion}]",
], text=True)
# paginate may return multiple JSON arrays; flatten
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
    if isinstance(obj, list):
        runs.extend(obj)
    idx += end
by_name = {}
for r in runs:
    name = r.get("name") or ""
    by_name[name] = r
missing, failed, pending = [], [], []
for name in required:
    r = by_name.get(name)
    if r is None:
        for k, v in by_name.items():
            if name == k or k.endswith("/" + name) or k.endswith(" " + name):
                r = v
                break
    if r is None:
        missing.append(name)
        continue
    st = (r.get("status") or "").lower()
    conc = (r.get("conclusion") or "").lower()
    if st != "completed":
        pending.append(name)
    elif conc != "success":
        failed.append(f"{name}:{conc or st}")
if missing or failed or pending:
    print("assert-pre-prod-green: NOT GREEN", file=sys.stderr)
    if missing:
        print("missing:", ", ".join(missing), file=sys.stderr)
    if pending:
        print("pending:", ", ".join(pending), file=sys.stderr)
    if failed:
        print("failed:", ", ".join(failed), file=sys.stderr)
    print("Do not start the next v0.9.0 feature item until pre-prod integration is green.", file=sys.stderr)
    sys.exit(1)
print("assert-pre-prod-green: ok required checks green on", sha)
PY
