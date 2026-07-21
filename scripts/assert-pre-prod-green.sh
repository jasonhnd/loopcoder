#!/usr/bin/env bash
# Assert an exact SHA has green integration checks from pre-prod-integration only.
#
# Provenance (fail-closed): for each candidate check-run, extract the Actions
# workflow run ID from details_url, then query:
#   repos/{owner}/{repo}/actions/runs/<run_id>
# and require ALL of:
#   - path == .github/workflows/pre-prod-integration.yml
#   - event == push
#   - head_branch == pre-prod
#   - head_sha exactly equals the requested full SHA
#   - GitHub Actions app identity on the check-run is non-empty
#   - check_suite_id matches the workflow run when available on the check-run
#
# Selects the newest provenance-valid workflow run for that SHA. Required jobs
# must be completed+success in that SAME run (never combined across runs or
# attempts). Missing fields, malformed URLs, API failures, pending/cancelled
# runs, and ambiguous results fail closed.
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
  case "${REQUIRE_ONLY}" in
    integration-verify|integration-canary) REQUIRED=("${REQUIRE_ONLY}") ;;
    *) echo "assert-pre-prod-green: unknown --require ${REQUIRE_ONLY}" >&2; exit 2 ;;
  esac
fi

export NO_COLOR=1 CLICOLOR=0 GH_FORCE_TTY=0
python3 - "${SHA}" "${REQUIRED[@]}" <<'PY'
import json
import os
import re
import subprocess
import sys
from datetime import datetime, timezone

sha = sys.argv[1]
required = sys.argv[2:]
discovery_names = ("integration-verify", "integration-canary")
workflow_path_required = ".github/workflows/pre-prod-integration.yml"
env = dict(os.environ)
env["NO_COLOR"] = "1"
env["CLICOLOR"] = "0"
env["GH_FORCE_TTY"] = "0"

RUN_ID_RE = re.compile(r"/actions/runs/(\d+)(?:/|$)")


def fail(msg, *extra):
    print("assert-pre-prod-green: NOT GREEN", file=sys.stderr)
    print(msg, file=sys.stderr)
    for line in extra:
        if line:
            print(line, file=sys.stderr)
    print(
        "Required: integration-verify + integration-canary from "
        ".github/workflows/pre-prod-integration.yml on push/pre-prod "
        "in the same newest workflow run (exact head_sha)",
        file=sys.stderr,
    )
    sys.exit(1)


def gh_api(path):
    try:
        raw = subprocess.check_output(
            [
                "gh",
                "api",
                path,
                "--paginate",
                "-H",
                "Accept: application/vnd.github+json",
            ],
            text=True,
            env=env,
            stderr=subprocess.STDOUT,
        )
    except subprocess.CalledProcessError as exc:
        out = (exc.output or "").strip()
        fail("api_failure path=%s exit=%s" % (path, exc.returncode), out[:500])
    except Exception as exc:  # noqa: BLE001 — fail closed on any transport error
        fail("api_failure path=%s error=%s" % (path, exc))
    if raw is None or not str(raw).strip():
        fail("empty_api_response path=%s" % path)
    return raw


def parse_json_stream(raw):
    """Parse one or more JSON values from gh --paginate output."""
    objects = []
    dec = json.JSONDecoder()
    idx = 0
    text = raw.strip()
    while idx < len(text):
        while idx < len(text) and text[idx].isspace():
            idx += 1
        if idx >= len(text):
            break
        try:
            obj, end = dec.raw_decode(text[idx:])
        except json.JSONDecodeError as exc:
            fail("malformed_api_json: %s" % exc)
        # end is relative to text[idx:]; advance absolute cursor (paginate stream).
        idx += end
        objects.append(obj)
    return objects


def parse_ts(value):
    if not value or not isinstance(value, str):
        return datetime.min.replace(tzinfo=timezone.utc)
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except Exception:
        return datetime.min.replace(tzinfo=timezone.utc)


def is_github_actions_app(check_run):
    app = check_run.get("app")
    if not isinstance(app, dict):
        return False
    slug = (app.get("slug") or "").strip()
    name = (app.get("name") or "").strip()
    if not slug and not name:
        return False
    if slug.lower() == "github-actions":
        return True
    compact = name.lower().replace(" ", "")
    if "github" in name.lower() and "action" in compact:
        return True
    return False


def extract_run_id(check_run):
    # Authoritative source is details_url only (fail closed if missing/malformed).
    url = check_run.get("details_url")
    if not isinstance(url, str) or not url.strip():
        return None
    m = RUN_ID_RE.search(url)
    if not m:
        return None
    return m.group(1)


def check_suite_id_of(check_run):
    suite = check_run.get("check_suite")
    if isinstance(suite, dict) and suite.get("id") is not None:
        try:
            return int(suite["id"])
        except (TypeError, ValueError):
            return "malformed"
    # Some payloads expose check_suite_id at top level.
    if check_run.get("check_suite_id") is not None:
        try:
            return int(check_run["check_suite_id"])
        except (TypeError, ValueError):
            return "malformed"
    return None


def load_check_runs(commit_sha):
    raw = gh_api("repos/{owner}/{repo}/commits/%s/check-runs" % commit_sha)
    runs = []
    for obj in parse_json_stream(raw):
        if isinstance(obj, dict) and "check_runs" in obj:
            if not isinstance(obj["check_runs"], list):
                fail("malformed_api_data: check_runs is not a list")
            runs.extend(obj["check_runs"])
        elif isinstance(obj, list):
            runs.extend(obj)
        else:
            fail("malformed_api_data: unexpected check-runs payload type")
    return runs


def load_workflow_run(run_id):
    raw = gh_api("repos/{owner}/{repo}/actions/runs/%s" % run_id)
    objs = parse_json_stream(raw)
    if len(objs) != 1 or not isinstance(objs[0], dict):
        fail("malformed_api_data: workflow run %s" % run_id)
    return objs[0]


def validate_workflow_run(run, run_id, check_suite_id):
    if not isinstance(run, dict):
        return "malformed_workflow_run"
    # Identity / required fields must be present and exact.
    path = run.get("path")
    event = run.get("event")
    head_branch = run.get("head_branch")
    head_sha = run.get("head_sha")
    if path is None or event is None or head_branch is None or head_sha is None:
        return "missing_workflow_run_fields"
    if path != workflow_path_required:
        return "workflow_path=%s" % path
    if event != "push":
        return "event=%s" % event
    if head_branch != "pre-prod":
        return "head_branch=%s" % head_branch
    if head_sha != sha:
        return "head_sha_mismatch got=%s want=%s" % (head_sha, sha)
    run_suite = run.get("check_suite_id")
    if run_suite is None:
        return "missing_check_suite_id"
    try:
        run_suite_i = int(run_suite)
    except (TypeError, ValueError):
        return "malformed_check_suite_id"
    if check_suite_id == "malformed":
        return "malformed_check_suite_on_check_run"
    if check_suite_id is not None and int(check_suite_id) != run_suite_i:
        return "check_suite_id_mismatch check=%s run=%s" % (check_suite_id, run_suite_i)
    # Prefer numeric id consistency when present.
    rid = run.get("id")
    if rid is not None:
        try:
            if str(int(rid)) != str(run_id):
                return "run_id_mismatch"
        except (TypeError, ValueError):
            return "malformed_run_id"
    return None


raw_checks = load_check_runs(sha)
workflow_cache = {}
# run_id -> {meta, jobs: {name: best check_run}}
by_run = {}
rejected = []

for r in raw_checks:
    if not isinstance(r, dict):
        rejected.append("non_object_check_run")
        continue
    name = r.get("name") or ""
    if name not in discovery_names:
        continue
    if not is_github_actions_app(r):
        rejected.append("%s:empty_or_non_actions_app" % name)
        continue
    run_id = extract_run_id(r)
    if not run_id:
        rejected.append("%s:malformed_or_missing_run_id_in_details_url" % name)
        continue
    suite_id = check_suite_id_of(r)
    if run_id not in workflow_cache:
        workflow_cache[run_id] = load_workflow_run(run_id)
    wr = workflow_cache[run_id]
    reason = validate_workflow_run(wr, run_id, suite_id)
    if reason:
        rejected.append("%s:run_%s:%s" % (name, run_id, reason))
        continue
    entry = by_run.setdefault(
        run_id,
        {
            "run": wr,
            "jobs": {},
        },
    )
    # Keep the newest attempt/check for this job name within the run.
    prev = entry["jobs"].get(name)
    if prev is None:
        entry["jobs"][name] = r
    else:
        # Prefer higher check-run id / later completed_at.
        def rank(cr):
            return (parse_ts(cr.get("completed_at") or cr.get("started_at")), int(cr.get("id") or 0))

        if rank(r) >= rank(prev):
            entry["jobs"][name] = r

if not by_run:
    fail(
        "no_provenance_valid_workflow_runs",
        "rejected_candidates: " + "; ".join(rejected[:30]) if rejected else "",
    )


def run_sort_key(run_id):
    wr = by_run[run_id]["run"]
    created = parse_ts(wr.get("created_at") or wr.get("run_started_at") or wr.get("updated_at"))
    try:
        rid = int(run_id)
    except ValueError:
        rid = 0
    return (created, rid)


newest_run_id = sorted(by_run.keys(), key=run_sort_key, reverse=True)[0]
chosen = by_run[newest_run_id]
wr = chosen["run"]
jobs = chosen["jobs"]

# Workflow-level status must be completed+success (fail closed otherwise).
wr_status = (wr.get("status") or "").lower()
wr_conclusion = (wr.get("conclusion") or "").lower() if wr.get("conclusion") is not None else ""
if wr_status != "completed":
    fail(
        "newest_run_pending run_id=%s status=%s" % (newest_run_id, wr_status),
        "rejected_candidates: " + "; ".join(rejected[:20]) if rejected else "",
    )
if wr_conclusion != "success":
    fail(
        "newest_run_not_success run_id=%s conclusion=%s" % (newest_run_id, wr_conclusion or "empty"),
        "rejected_candidates: " + "; ".join(rejected[:20]) if rejected else "",
    )

missing, failed, pending = [], [], []
for name in required:
    cr = jobs.get(name)
    if cr is None:
        missing.append(name)
        continue
    st = (cr.get("status") or "").lower()
    conc = (cr.get("conclusion") or "").lower() if cr.get("conclusion") is not None else ""
    if st != "completed":
        pending.append("%s:%s" % (name, st or "unknown"))
    elif conc != "success":
        failed.append("%s:%s" % (name, conc or st or "unknown"))

if missing or failed or pending:
    fail(
        "same_run_jobs_not_green run_id=%s" % newest_run_id,
        "missing: " + ", ".join(missing) if missing else "",
        "pending: " + ", ".join(pending) if pending else "",
        "failed: " + ", ".join(failed) if failed else "",
        "rejected_candidates: " + "; ".join(rejected[:20]) if rejected else "",
    )

# When requiring a subset, still bind to newest run; when requiring both,
# both already validated above. Extra safety: if both discovery jobs exist on
# the chosen run and we required only one, do not claim cross-run success.
print(
    "assert-pre-prod-green: ok",
    sha,
    "run_id=%s" % newest_run_id,
    "checks=" + ",".join(required),
)
PY
