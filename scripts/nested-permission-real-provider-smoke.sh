#!/usr/bin/env bash
set -euo pipefail

binary=""
provider=""
permission=""
model=""
effort=""
artifact_dir=""
timeout_seconds=180

usage() {
  echo "usage: nested-permission-real-provider-smoke.sh --binary <path> --provider codex|claude|grok --permission read-only|write [--model <id>] [--effort <level>] [--artifact-dir <path>] [--timeout-seconds <n>]" >&2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary) binary="${2:-}"; shift 2 ;;
    --provider) provider="${2:-}"; shift 2 ;;
    --permission) permission="${2:-}"; shift 2 ;;
    --model) model="${2:-}"; shift 2 ;;
    --effort) effort="${2:-}"; shift 2 ;;
    --artifact-dir) artifact_dir="${2:-}"; shift 2 ;;
    --timeout-seconds) timeout_seconds="${2:-}"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

if [[ "${LOOPCODER_REAL_PROVIDER_SMOKE-}" != "1" ]]; then
  echo "real-provider nested smoke is disabled; set LOOPCODER_REAL_PROVIDER_SMOKE=1 after protected operator approval" >&2
  exit 2
fi
case "${GITHUB_EVENT_NAME-}" in
  pull_request|pull_request_target)
    echo "real-provider nested smoke is never permitted on pull-request events" >&2
    exit 2
    ;;
esac
if [[ -z "$binary" || -z "$provider" || -z "$permission" ]]; then
  usage
  exit 2
fi
case "$provider" in
  codex|claude|grok) ;;
  *) usage; exit 2 ;;
esac
case "$permission" in
  read-only) ;;
  write)
    case "$provider" in
      codex|grok) ;;
      *) echo "provider $provider has no registered bounded-write nested executor" >&2; exit 2 ;;
    esac
    ;;
  *) usage; exit 2 ;;
esac
case "$timeout_seconds" in
  ''|*[!0-9]*) usage; exit 2 ;;
esac
if [[ "$timeout_seconds" -lt 30 || "$timeout_seconds" -gt 300 ]]; then
  usage
  exit 2
fi
for command_name in git python3 shasum; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "real-provider nested smoke requires $command_name" >&2
    exit 2
  fi
done
if [[ ! -f "$binary" || ! -x "$binary" ]]; then
  echo "real-provider nested smoke requires an executable candidate binary" >&2
  exit 2
fi

binary="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$binary")"
candidate_hash="$(shasum -a 256 "$binary" | awk '{print $1}')"
if ! version_text="$("$binary" version 2>/dev/null)"; then
  echo "real-provider nested smoke could not identify the candidate" >&2
  exit 2
fi
case "$version_text" in
  *platform=darwin/arm64*) ;;
  *) echo "real-provider nested smoke requires a packaged darwin/arm64 binary" >&2; exit 2 ;;
esac

root="$(mktemp -d "${TMPDIR:-/tmp}/loopcoder-real-provider-smoke.XXXXXX")"
repo="$root/repo"
loopcoder_home="$root/home"
plan="$root/plan.json"
result="$root/result.json"
old_loopcoder_home="${LOOPCODER_HOME-}"
had_loopcoder_home=0
if [[ -n "${LOOPCODER_HOME+x}" ]]; then
  had_loopcoder_home=1
fi
worktree=""

cleanup() {
  if [[ -n "$worktree" && -d "$repo" ]]; then
    git -C "$repo" worktree remove --force "$worktree" >/dev/null 2>&1 || true
  fi
  if [[ "$had_loopcoder_home" -eq 1 ]]; then
    export LOOPCODER_HOME="$old_loopcoder_home"
  else
    unset LOOPCODER_HOME
  fi
  rm -rf "$root"
}
trap cleanup EXIT

mkdir -p "$repo" "$loopcoder_home"
export LOOPCODER_HOME="$loopcoder_home"
git -C "$root" init -b main "$repo" >/dev/null 2>&1
git -C "$repo" config user.email real-provider-canary@example.invalid
git -C "$repo" config user.name "Real Provider Canary"
printf '# Real-provider canary\n' > "$repo/README.md"
printf 'allowed baseline\n' > "$repo/allowed.txt"
git -C "$repo" add README.md allowed.txt
git -C "$repo" commit -m "Initialize real-provider canary" >/dev/null
git -C "$repo" update-ref refs/remotes/origin/main HEAD
"$binary" projects register --repo "$repo" --format json >"$root/register.json" 2>"$root/register.stderr"

slug="$provider-${permission//-/}"
if [[ "$permission" == "read-only" ]]; then
  title="Inspect README.md and report the heading without changing any file"
  scoped_path=README.md
  case_command="git diff -- README.md"
else
  title="Append one line containing real provider canary to allowed.txt and change no other file"
  scoped_path=allowed.txt
  case_command="printf 'real provider canary\\n' >> allowed.txt"
fi
SLUG="$slug" TITLE="$title" PERMISSION="$permission" SCOPED_PATH="$scoped_path" CASE_COMMAND="$case_command" \
  python3 - "$plan" <<'PY'
import json, os, sys

slug = os.environ["SLUG"]
parent = "run-20260717T070000Z-wave-real-provider-" + slug
payload = {
    "schema_version": "loopcoder.child_plan.v1",
    "plan_id": "plan-" + parent,
    "parent_run_id": parent,
    "root_run_id": parent,
    "parent_depth": 0,
    "max_depth": 2,
    "max_concurrency": 1,
    "created_at": "2026-07-17T07:00:00Z",
    "items": [{
        "child_key": "real-provider-" + slug,
        "title": os.environ["TITLE"],
        "role": "worker",
        "run_id": "run-20260717T070001Z-child-0-real-provider-" + slug,
        "issue": 1009,
        "scope": {
            "repo": ".",
            "paths": [os.environ["SCOPED_PATH"]],
            "issues": [1009],
            "commands": [os.environ["CASE_COMMAND"]],
        },
        "permission": os.environ["PERMISSION"],
        "depends_on": [],
        "aggregation": {"mode": "collect", "required": True, "include_report": True},
    }],
}
with open(sys.argv[1], "w", encoding="utf-8") as handle:
    json.dump(payload, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY

arguments=(nested run --repo "$repo" --plan "$plan" --provider "$provider" --base-branch main --timeout "${timeout_seconds}s" --strict --format json)
if [[ -n "$model" ]]; then
  arguments+=(--model "$model")
fi
if [[ -n "$effort" ]]; then
  arguments+=(--effort "$effort")
fi
if ! "$binary" "${arguments[@]}" >"$result" 2>"$root/provider.stderr"; then
  echo "real-provider nested canary failed without retaining raw provider output" >&2
  exit 1
fi
if ! REPORT="$result" PERMISSION="$permission" python3 <<'PY' 2>/dev/null
import json, os
with open(os.environ["REPORT"], encoding="utf-8") as handle:
    report = json.load(handle)
children = report.get("children") or []
assert report.get("status") == "succeeded" and len(children) == 1 and children[0].get("status") == "succeeded"
child = children[0]
if os.environ["PERMISSION"] == "read-only":
    assert (child.get("read_only_enforcement") or {}).get("verification") == "passed"
else:
    assert (child.get("mutation_manifest") or {}).get("verification") == "passed"
PY
then
  echo "real-provider nested canary returned an unexpected status" >&2
  exit 1
fi

if [[ "$permission" == "write" ]]; then
  worktree="$(REPORT="$result" python3 -c 'import json,os; print((json.load(open(os.environ["REPORT"])).get("children") or [{}])[0].get("worktree_path", ""))')"
fi
if [[ -n "$(git -C "$repo" status --porcelain=v1 --untracked-files=all)" ]]; then
  echo "real-provider nested canary changed the parent repository" >&2
  exit 1
fi

if [[ -n "$artifact_dir" ]]; then
  artifact_dir="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$artifact_dir")"
  mkdir -p "$artifact_dir"
  CANDIDATE_HASH="$candidate_hash" PROVIDER="$provider" PERMISSION="$permission" \
    python3 - "$artifact_dir/real-provider-canary-evidence.json" <<'PY'
import json, os, sys
payload = {
    "schema_version": "loopcoder.nested_real_provider_canary.v1",
    "status": "passed",
    "candidate_sha256": os.environ["CANDIDATE_HASH"],
    "provider": os.environ["PROVIDER"],
    "permission": os.environ["PERMISSION"],
    "provider_launches": 1,
    "parent_repository_unchanged": True,
    "audit_verification": "passed",
    "protected_opt_in": True,
    "pull_request_events_allowed": False,
    "redaction": {
        "paths_included": False,
        "prompts_included": False,
        "credentials_included": False,
        "raw_output_included": False,
    },
}
with open(sys.argv[1], "w", encoding="utf-8") as handle:
    json.dump(payload, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY
fi

echo "real-provider nested canary passed: provider=$provider permission=$permission"
