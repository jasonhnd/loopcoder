#!/usr/bin/env bash
# Fail-closed implementation authorization for ordinary-development PRs.
#
# Closing issue sources (in order):
# 1) GitHub GraphQL closingIssuesReferences (preferred; works when GitHub links).
# 2) If empty and PR base != default branch: exactly one structured PR label
#    closes:<digits> (owner-applied). GitHub does not populate
#    closingIssuesReferences from keywords for non-default base branches.
# Body free-text is never used for authorization.
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
export REPO

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

gh pr view "${PR_NUMBER}" --repo "${REPO}" --json files -q '.files[].path' >"${TMP}/files.txt"
gh pr view "${PR_NUMBER}" --repo "${REPO}" --json labels -q '.labels[].name' >"${TMP}/pr_labels.txt"

OWNER="${REPO%%/*}"
NAME="${REPO#*/}"
cat >"${TMP}/query.graphql" <<'GQL'
query($owner:String!, $name:String!, $number:Int!) {
  repository(owner:$owner, name:$name) {
    defaultBranchRef { name }
    pullRequest(number:$number) {
      baseRefName
      baseRefOid
      closingIssuesReferences(first: 20) {
        nodes { number title body labels(first: 50) { nodes { name } } }
      }
    }
  }
}
GQL
# Force non-TTY JSON (gh may inject ANSI color codes when stdout is redirected poorly).
NO_COLOR=1 CLICOLOR=0 GH_FORCE_TTY=0 gh api graphql \
  -F "query=@${TMP}/query.graphql" \
  -f owner="${OWNER}" \
  -f name="${NAME}" \
  -F number="${PR_NUMBER}" \
  --jq '.' \
  >"${TMP}/pr.json"


python3 - "${TMP}/pr.json" "${TMP}/closing.json" "${TMP}/base_sha.txt" "${TMP}/pr_labels.txt" <<'PY'
import json, re, sys, subprocess, os

repo_name = os.environ["REPO"]
pr_wrapper = json.load(open(sys.argv[1]))
repo = pr_wrapper["data"]["repository"]
pr = repo["pullRequest"]
default_branch = (repo.get("defaultBranchRef") or {}).get("name") or "main"
base_name = pr.get("baseRefName") or ""
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
source = "closingIssuesReferences"

if not out and base_name and base_name != default_branch:
    labels = [ln.strip() for ln in open(sys.argv[4]) if ln.strip()]
    closes = []
    for lab in labels:
        m = re.fullmatch(r"closes:(\d+)", lab, flags=re.I)
        if m:
            closes.append(int(m.group(1)))
    if len(closes) == 1:
        num = closes[0]
        env = dict(os.environ)
        env["NO_COLOR"] = "1"
        env["CLICOLOR"] = "0"
        env["GH_FORCE_TTY"] = "0"
        raw = subprocess.check_output(
            ["gh", "issue", "view", str(num), "--repo", repo_name, "--json", "number,title,body,labels"],
            text=True,
            env=env,
        )
        iss = json.loads(raw)
        out = [{
            "number": iss["number"],
            "title": iss.get("title") or "",
            "body": iss.get("body") or "",
            "labels": [x["name"] for x in (iss.get("labels") or [])],
        }]
        source = "pr_label_closes"
    elif len(closes) > 1:
        out = [{"number": n, "title": "", "body": "", "labels": []} for n in closes]
        source = "pr_label_closes_multiple"

json.dump(out, open(sys.argv[2], "w"))
open(sys.argv[3], "w").write(pr.get("baseRefOid") or "")
print("base_branch=%s" % base_name)
print("default_branch=%s" % default_branch)
print("closing_source=%s" % source)
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
import json, sys
issues = json.load(open(sys.argv[1]))
sys.exit(0 if len(issues) == 1 and issues[0]["number"] == 1092 else 1)
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
