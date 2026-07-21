#!/usr/bin/env bash
# Fail-closed implementation authorization for ordinary-development PRs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

OWNER_LOGIN="${IMPLEMENTATION_AUTH_OWNER:-jasonhnd}"

if [[ "${IMPLEMENTATION_AUTH_OFFLINE:-}" == "1" ]]; then
  go test ./internal/evidence -count=1 -timeout=60s -run 'TestDocsOnly|TestPolicy|TestUnlinked|TestMultiple|TestNonPlanned|TestBodyOnly|TestPlannedWith|TestClosed|TestAuthLabel|TestLatest|TestBootstrap|TestBaseSHA'
  echo "implementation-authorization: offline adversarial fixtures ok"
  exit 0
fi

fail() { echo "implementation-authorization: FAIL: $*" >&2; exit 1; }

command -v gh >/dev/null 2>&1 || fail "gh CLI required (not available)"

REPO="${REPO:-}"
if [[ -z "${REPO}" ]]; then
  REPO="$(NO_COLOR=1 GH_FORCE_TTY=0 gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || true)"
fi
[[ -n "${REPO}" ]] || fail "unable to resolve repository"

PR_NUMBER="${PR_NUMBER:-}"
if [[ -z "${PR_NUMBER}" && -n "${GITHUB_EVENT_PATH:-}" && -f "${GITHUB_EVENT_PATH}" ]]; then
  PR_NUMBER="$(python3 -c 'import json,sys; e=json.load(open(sys.argv[1])); print((e.get("pull_request") or {}).get("number") or "")' "${GITHUB_EVENT_PATH}" 2>/dev/null || true)"
fi
if [[ -z "${PR_NUMBER}" && -n "${GITHUB_REF:-}" && "${GITHUB_REF}" =~ refs/pull/([0-9]+)/ ]]; then
  PR_NUMBER="${BASH_REMATCH[1]}"
fi
[[ -n "${PR_NUMBER}" ]] || fail "PR_NUMBER required (no PR context)"

echo "implementation-authorization: PR #${PR_NUMBER} repo=${REPO} owner=${OWNER_LOGIN}"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

export REPO OWNER_LOGIN PR_NUMBER TMPDIR_AUTH="${TMP}"
export NO_COLOR=1 CLICOLOR=0 GH_FORCE_TTY=0

gh pr view "${PR_NUMBER}" --repo "${REPO}" --json files,labels,headRefName,baseRefName,baseRefOid \
  >"${TMP}/pr_view.json" || fail "gh pr view failed"

python3 -c '
import json, os
pr=json.load(open(os.environ["TMPDIR_AUTH"]+"/pr_view.json"))
t=os.environ["TMPDIR_AUTH"]
open(t+"/files.txt","w").write("\n".join(f.get("path","") for f in (pr.get("files") or []) if f.get("path"))+"\n")
open(t+"/pr_labels.txt","w").write("\n".join(l.get("name","") for l in (pr.get("labels") or []) if l.get("name"))+"\n")
open(t+"/head_branch.txt","w").write(pr.get("headRefName") or "")
open(t+"/base_branch.txt","w").write(pr.get("baseRefName") or "")
open(t+"/base_sha.txt","w").write(pr.get("baseRefOid") or "")
print("files=%d head=%s base=%s sha=%s"%(len(pr.get("files") or []), pr.get("headRefName"), pr.get("baseRefName"), (pr.get("baseRefOid") or "")[:12]))
'

[[ -s "${TMP}/base_sha.txt" ]] || fail "missing base SHA"

OWNER="${REPO%%/*}"
NAME="${REPO#*/}"
cat >"${TMP}/query.graphql" <<'GQL'
query($owner:String!, $name:String!, $number:Int!) {
  repository(owner:$owner, name:$name) {
    defaultBranchRef { name }
    pullRequest(number:$number) {
      baseRefName
      baseRefOid
      headRefName
      closingIssuesReferences(first: 20) {
        nodes { number }
      }
    }
  }
}
GQL
gh api graphql \
  -F "query=@${TMP}/query.graphql" \
  -f owner="${OWNER}" \
  -f name="${NAME}" \
  -F number="${PR_NUMBER}" \
  --jq '.' \
  >"${TMP}/pr.json" || fail "graphql query failed"
[[ -s "${TMP}/pr.json" ]] || fail "empty graphql response"

python3 <<'PY'
import json, re, subprocess, os, datetime, sys

repo_name = os.environ["REPO"]
owner_login = os.environ["OWNER_LOGIN"]
pr_number = int(os.environ["PR_NUMBER"])
tmp = os.environ["TMPDIR_AUTH"]
env = dict(os.environ)
env["NO_COLOR"] = "1"
env["CLICOLOR"] = "0"
env["GH_FORCE_TTY"] = "0"

def gh_json(args):
    raw = subprocess.check_output(["gh"] + args, text=True, env=env)
    if not raw.strip():
        raise SystemExit("empty gh output: %s" % args)
    return json.loads(raw)

def parse_events_payload(raw):
    events, dec, idx = [], json.JSONDecoder(), 0
    text = raw.strip()
    while idx < len(text):
        while idx < len(text) and text[idx].isspace():
            idx += 1
        if idx >= len(text):
            break
        obj, end = dec.raw_decode(text[idx:])
        idx += end
        if isinstance(obj, list):
            events.extend(obj)
        elif isinstance(obj, dict):
            events.append(obj)
    return events

def label_events(issue_num):
    raw = subprocess.check_output(
        ["gh", "api", f"repos/{repo_name}/issues/{issue_num}/events", "--paginate"],
        text=True, env=env,
    )
    if not raw.strip():
        raise SystemExit("empty label events for issue %s" % issue_num)
    out = []
    for e in parse_events_payload(raw):
        ev = (e.get("event") or "").lower()
        if ev not in ("labeled", "unlabeled"):
            continue
        lab = ((e.get("label") or {}).get("name") or "")
        actor = ((e.get("actor") or {}).get("login") or "")
        created = e.get("created_at") or ""
        try:
            ts = datetime.datetime.fromisoformat(created.replace("Z", "+00:00"))
        except Exception:
            ts = datetime.datetime.min.replace(tzinfo=datetime.timezone.utc)
        out.append({"action": ev, "label": lab, "actor": actor, "created_at": ts})
    return out

def latest_apply_actor(events, label):
    filtered = [e for e in events if e["label"].lower() == label.lower()]
    if not filtered:
        return "", False
    filtered.sort(key=lambda e: e["created_at"], reverse=True)
    top = filtered[0]
    if top["action"] != "labeled":
        return "", False
    actor = (top["actor"] or "").strip()
    return actor, bool(actor)

def enrich_issue(num):
    iss = gh_json(["issue", "view", str(num), "--repo", repo_name, "--json", "number,title,body,labels,state"])
    labels = [x["name"] for x in (iss.get("labels") or [])]
    state = (iss.get("state") or "").upper()
    events = label_events(num)
    actor, ok_hist = latest_apply_actor(events, "implementation-authorized")
    live = any(l.lower() == "implementation-authorized" for l in labels)
    auth_ok = bool(live and ok_hist and actor.lower() == owner_login.lower())
    return {
        "number": iss["number"],
        "title": iss.get("title") or "",
        "body": iss.get("body") or "",
        "state": state,
        "labels": labels,
        "AuthLabelActor": actor,
        "AuthLabelOK": auth_ok,
    }

pr_wrapper = json.load(open(os.path.join(tmp, "pr.json")))
if not pr_wrapper.get("data"):
    raise SystemExit("missing graphql data")
repo = pr_wrapper["data"]["repository"]
pr = repo["pullRequest"]
default_branch = (repo.get("defaultBranchRef") or {}).get("name") or "main"
base_name = pr.get("baseRefName") or open(os.path.join(tmp, "base_branch.txt")).read().strip()
head_name = pr.get("headRefName") or open(os.path.join(tmp, "head_branch.txt")).read().strip()
base_sha = pr.get("baseRefOid") or open(os.path.join(tmp, "base_sha.txt")).read().strip()
if not base_sha:
    raise SystemExit("missing base SHA")

nodes = (pr.get("closingIssuesReferences") or {}).get("nodes") or []
out = []
source = "closingIssuesReferences"
for n in nodes:
    out.append(enrich_issue(n["number"]))

if not out and base_name and base_name != default_branch:
    labels = [ln.strip() for ln in open(os.path.join(tmp, "pr_labels.txt")) if ln.strip()]
    closes = []
    for lab in labels:
        m = re.fullmatch(r"closes:(\d+)", lab, flags=re.I)
        if m:
            closes.append(int(m.group(1)))
    if len(closes) > 1:
        out = [{
            "number": n, "title": "", "body": "", "state": "OPEN",
            "labels": [], "AuthLabelActor": "", "AuthLabelOK": False,
        } for n in closes]
        source = "pr_label_closes_multiple"
    elif len(closes) == 1:
        lab_name = "closes:%d" % closes[0]
        pr_events = label_events(pr_number)
        actor, ok_hist = latest_apply_actor(pr_events, lab_name)
        if not ok_hist or actor.lower() != owner_login.lower():
            raise SystemExit(
                "closes label %s latest apply actor %r is not owner %r (or missing labeled event)"
                % (lab_name, actor, owner_login)
            )
        out = [enrich_issue(closes[0])]
        source = "pr_label_closes"
    else:
        source = "none"

json.dump(out, open(os.path.join(tmp, "closing.json"), "w"))
open(os.path.join(tmp, "base_sha.txt"), "w").write(base_sha)
open(os.path.join(tmp, "head_branch.txt"), "w").write(head_name)
open(os.path.join(tmp, "base_branch.txt"), "w").write(base_name)
print("closing_source=%s" % source)
print("closing_issue_count=%d" % len(out))
for item in out:
    print(
        "closing_issue=%d state=%s auth_ok=%s actor=%s labels=%s"
        % (
            item["number"], item.get("state"), item.get("AuthLabelOK"),
            item.get("AuthLabelActor"), ",".join(item.get("labels") or []),
        )
    )
PY

BASE_SHA="$(cat "${TMP}/base_sha.txt")"
HEAD_BRANCH="$(cat "${TMP}/head_branch.txt")"
BASE_BRANCH="$(cat "${TMP}/base_branch.txt")"
[[ -n "${BASE_SHA}" ]] || fail "empty base SHA"
echo "base_sha=${BASE_SHA}"
echo "head_branch=${HEAD_BRANCH}"
echo "base_branch=${BASE_BRANCH}"

VERIFY_OK=false
CANARY_OK=false
if bash scripts/assert-pre-prod-green.sh --sha "${BASE_SHA}" --require integration-verify --quiet; then
  VERIFY_OK=true
fi
if bash scripts/assert-pre-prod-green.sh --sha "${BASE_SHA}" --require integration-canary --quiet; then
  CANARY_OK=true
fi
echo "base_integration_verify_ok=${VERIFY_OK}"
echo "base_integration_canary_ok=${CANARY_OK}"

[[ -f "${TMP}/closing.json" ]] || fail "closing.json missing"

go run ./scripts/cmd/checkimplauth/ \
  -files "${TMP}/files.txt" \
  -closing-issues "${TMP}/closing.json" \
  -base-sha "${BASE_SHA}" \
  -base-verify-ok="${VERIFY_OK}" \
  -base-canary-ok="${CANARY_OK}" \
  -pr-number="${PR_NUMBER}" \
  -head-branch="${HEAD_BRANCH}" \
  -base-branch="${BASE_BRANCH}"

echo "implementation-authorization: ok pr=${PR_NUMBER}"
