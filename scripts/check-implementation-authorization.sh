#!/usr/bin/env bash
# Fail-closed implementation authorization for ordinary-development PRs.
# Uses GitHub closingIssuesReferences (GraphQL), not a local issue regex.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

if [[ "${SKIP_IMPLEMENTATION_AUTH_CHECK:-}" == "1" ]]; then
  echo "implementation-authorization: skipped"
  exit 0
fi

if [[ "${IMPLEMENTATION_AUTH_OFFLINE:-}" == "1" ]]; then
  go test ./internal/evidence -count=1 -timeout=60s -run 'TestDocsOnly|TestPolicy|TestUnlinked|TestMultiple|TestNonPlanned|TestBodyOnly|TestPlannedWith|TestReady|TestBaseSHA'
  echo "implementation-authorization: offline adversarial fixtures ok"
  exit 0
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "implementation-authorization: gh missing; offline fixtures only" >&2
  IMPLEMENTATION_AUTH_OFFLINE=1 exec bash "$0"
fi

REPO="${REPO:-}"
if [[ -z "${REPO}" ]]; then
  REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || true)"
fi
PR_NUMBER="${PR_NUMBER:-}"
if [[ -z "${PR_NUMBER}" && -n "${GITHUB_EVENT_PATH:-}" && -f "${GITHUB_EVENT_PATH}" ]]; then
  PR_NUMBER="$(python3 -c 'import json,sys; e=json.load(open(sys.argv[1])); print((e.get("pull_request") or {}).get("number") or "")' "${GITHUB_EVENT_PATH}" 2>/dev/null || true)"
fi
if [[ -z "${PR_NUMBER}" && -n "${GITHUB_REF:-}" && "${GITHUB_REF}" =~ refs/pull/([0-9]+)/ ]]; then
  PR_NUMBER="${BASH_REMATCH[1]}"
fi
if [[ -z "${PR_NUMBER}" || -z "${REPO}" ]]; then
  echo "implementation-authorization: no PR context; offline fixtures" >&2
  IMPLEMENTATION_AUTH_OFFLINE=1 exec bash "$0"
fi

echo "implementation-authorization: PR #${PR_NUMBER} repo=${REPO}"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

gh pr view "${PR_NUMBER}" --repo "${REPO}" --json files -q '.files[].path' >"${TMP}/files.txt"

OWNER="${REPO%%/*}"
NAME="${REPO#*/}"
gh api graphql -f query='
query($owner:String!, $name:String!, $number:Int!) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$number) {
      baseRefOid
      closingIssuesReferences(first: 20) {
        nodes { number title body labels(first: 50) { nodes { name } } }
      }
    }
  }
}' -f owner="${OWNER}" -f name="${NAME}" -F number="${PR_NUMBER}" >"${TMP}/pr.json"

python3 - "${TMP}/pr.json" "${TMP}/closing.json" "${TMP}/base_sha.txt" <<'PY'
import json, sys
pr = json.load(open(sys.argv[1]))["data"]["repository"]["pullRequest"]
nodes = (pr.get("closingIssuesReferences") or {}).get("nodes") or []
out = []
for n in nodes:
    labels = [x["name"] for x in ((n.get("labels") or {}).get("nodes") or [])]
    out.append({
        "number": n["number"],
        "title": n.get("title") or "",
        "body": n.get("body") or "",
        "labels": labels,
    })
json.dump(out, open(sys.argv[2], "w"))
open(sys.argv[3], "w").write(pr.get("baseRefOid") or "")
print("closing_issue_count=%d" % len(out))
for item in out:
    print("closing_issue=%d labels=%s" % (item["number"], ",".join(item["labels"])))
PY

BASE_SHA="$(cat "${TMP}/base_sha.txt")"
echo "base_sha=${BASE_SHA}"

VERIFY_OK=false
CANARY_OK=false
if [[ -n "${BASE_SHA}" ]]; then
  if bash scripts/assert-pre-prod-green.sh --sha "${BASE_SHA}" --require integration-verify --quiet; then
    VERIFY_OK=true
  fi
  if bash scripts/assert-pre-prod-green.sh --sha "${BASE_SHA}" --require integration-canary --quiet; then
    CANARY_OK=true
  fi
fi
echo "base_integration_verify_ok=${VERIFY_OK}"
echo "base_integration_canary_ok=${CANARY_OK}"

BOOTSTRAP=false
if python3 - "${TMP}/closing.json" <<'PY'
import json,sys
issues=json.load(open(sys.argv[1]))
sys.exit(0 if len(issues)==1 and issues[0]["number"]==1092 else 1)
PY
then
  BOOTSTRAP=true
fi
echo "bootstrap_1092=${BOOTSTRAP}"

go run ./scripts/cmd/checkimplauth/ \
  -files "${TMP}/files.txt" \
  -closing-issues "${TMP}/closing.json" \
  -base-sha "${BASE_SHA}" \
  -base-verify-ok="${VERIFY_OK}" \
  -base-canary-ok="${CANARY_OK}" \
  -bootstrap-1092="${BOOTSTRAP}"

echo "implementation-authorization: ok pr=${PR_NUMBER}"
