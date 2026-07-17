#!/usr/bin/env bash
set -euo pipefail
set -E

case_timeout_seconds=20
max_diagnostic_bytes=65536
max_duration_seconds=300
binary=""
artifact_dir=""
diagnostics_dir=""
candidate_source=""

usage() {
  echo "usage: nested-permission-matrix-smoke.sh --binary <path> --artifact-dir <path> --candidate-source packaged|development [--diagnostics-dir <path>] [--max-duration-seconds <n>]" >&2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary) binary="${2:-}"; shift 2 ;;
    --artifact-dir) artifact_dir="${2:-}"; shift 2 ;;
    --diagnostics-dir) diagnostics_dir="${2:-}"; shift 2 ;;
    --candidate-source) candidate_source="${2:-}"; shift 2 ;;
    --max-duration-seconds) max_duration_seconds="${2:-}"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

if [[ -z "$binary" || -z "$artifact_dir" || -z "$candidate_source" ]]; then
  usage
  exit 2
fi
case "$candidate_source" in
  packaged|development) ;;
  *) usage; exit 2 ;;
esac
case "$max_duration_seconds" in
  ''|*[!0-9]*) usage; exit 2 ;;
esac
if [[ "$max_duration_seconds" -lt 60 || "$max_duration_seconds" -gt 600 ]]; then
  usage
  exit 2
fi
for command_name in git python3 shasum; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "nested permission matrix requires $command_name" >&2
    exit 2
  fi
done
if [[ ! -f "$binary" || ! -x "$binary" ]]; then
  echo "nested permission matrix requires an executable candidate binary" >&2
  exit 2
fi

binary="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$binary")"
artifact_dir="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$artifact_dir")"
mkdir -p "$artifact_dir"
if [[ -z "$diagnostics_dir" ]]; then
  diagnostics_dir="${RUNNER_TEMP:-$artifact_dir/diagnostics}/loopcoder-permission-matrix-diagnostics"
fi
diagnostics_dir="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$diagnostics_dir")"
rm -f "$artifact_dir/permission-matrix-diagnostics.json" "$diagnostics_dir/permission-matrix-diagnostics.json"

matrix_root="$(mktemp -d "${TMPDIR:-/tmp}/loopcoder-permission-matrix.XXXXXX")"
repo="$matrix_root/repo"
loopcoder_home="$matrix_root/home"
plans_dir="$matrix_root/plans"
results_dir="$matrix_root/results"
completed_file="$matrix_root/completed.jsonl"
worktrees_file="$matrix_root/worktrees.txt"
active_case="setup"
failure_code="matrix_setup_failed"
started_epoch="$(date +%s)"
old_loopcoder_home="${LOOPCODER_HOME-}"
had_loopcoder_home=0
if [[ -n "${LOOPCODER_HOME+x}" ]]; then
  had_loopcoder_home=1
fi

cleanup() {
  if [[ -f "$worktrees_file" && -d "$repo" ]]; then
    while IFS= read -r worktree; do
      [[ -n "$worktree" ]] || continue
      git -C "$repo" worktree remove --force "$worktree" >/dev/null 2>&1 || true
    done < "$worktrees_file"
  fi
  if [[ "$had_loopcoder_home" -eq 1 ]]; then
    export LOOPCODER_HOME="$old_loopcoder_home"
  else
    unset LOOPCODER_HOME
  fi
  rm -rf "$matrix_root"
}
trap cleanup EXIT

write_diagnostic() {
  local destination="$1"
  FAILURE_CODE="$failure_code" ACTIVE_CASE="$active_case" MAX_BYTES="$max_diagnostic_bytes" \
    python3 - "$completed_file" "$destination" <<'PY'
import json, os, sys

completed_path, destination = sys.argv[1:]
completed = []
if os.path.exists(completed_path):
    with open(completed_path, encoding="utf-8") as handle:
        completed = [json.loads(line) for line in handle if line.strip()]
payload = {
    "schema_version": "loopcoder.nested_permission_matrix_diagnostic.v1",
    "status": "failed",
    "failed_case": os.environ["ACTIVE_CASE"],
    "failure_code": os.environ["FAILURE_CODE"],
    "completed_cases": completed,
    "redaction": {
        "paths_included": False,
        "prompts_included": False,
        "credentials_included": False,
        "raw_output_included": False,
    },
}
encoded = json.dumps(payload, indent=2, sort_keys=True).encode()
if len(encoded) + 1 > int(os.environ["MAX_BYTES"]):
    payload["completed_cases"] = []
    encoded = json.dumps(payload, indent=2, sort_keys=True).encode()
if len(encoded) + 1 > int(os.environ["MAX_BYTES"]):
    raise RuntimeError("sanitized diagnostic exceeds size ceiling")
os.makedirs(os.path.dirname(destination), exist_ok=True)
with open(destination, "wb") as handle:
    handle.write(encoded + b"\n")
PY
}

on_error() {
  local status=$?
  trap - ERR
  failure_code="matrix_unhandled_error"
  write_diagnostic "$artifact_dir/permission-matrix-diagnostics.json" || true
  write_diagnostic "$diagnostics_dir/permission-matrix-diagnostics.json" || true
  echo "nested permission matrix failed: case=$active_case code=$failure_code" >&2
  exit "$status"
}
trap on_error ERR

fail() {
  failure_code="$1"
  write_diagnostic "$artifact_dir/permission-matrix-diagnostics.json" || true
  write_diagnostic "$diagnostics_dir/permission-matrix-diagnostics.json" || true
  echo "nested permission matrix failed: case=$active_case code=$failure_code" >&2
  exit 1
}

json_get() {
  python3 - "$1" "$2" <<'PY'
import json, sys

with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle)
for part in sys.argv[2].split("."):
    if isinstance(value, list):
        value = value[int(part)]
    elif isinstance(value, dict):
        value = value.get(part)
    else:
        value = None
        break
if value is None:
    print("")
elif isinstance(value, bool):
    print("true" if value else "false")
elif isinstance(value, (dict, list)):
    print(json.dumps(value, separators=(",", ":"), sort_keys=True))
else:
    print(value)
PY
}

line_count() {
  if [[ -f "$1" ]]; then
    awk 'END { print NR + 0 }' "$1"
  else
    echo 0
  fi
}

outside_repo() {
  local candidate canonical_repo canonical_candidate
  candidate="$1"
  canonical_repo="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$repo")"
  canonical_candidate="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$candidate")"
  [[ "$canonical_candidate" != "$canonical_repo" && "$canonical_candidate" != "$canonical_repo/"* ]]
}

assert_parent_unchanged() {
  local head status
  head="$(git -C "$repo" rev-parse HEAD)" || fail parent_head_query_failed
  status="$(git -C "$repo" status --porcelain=v1 --untracked-files=all)" || fail parent_status_query_failed
  [[ "$head" == "$expected_head" ]] || fail parent_head_changed
  [[ "$status" == "$expected_status" ]] || fail parent_tree_changed
}

run_nested() {
  local plan="$1" result="$2" stderr_file="$3"
  set +e
  "$binary" nested run \
    --repo "$repo" \
    --plan "$plan" \
    --provider test-subprocess \
    --base-branch main \
    --timeout "${case_timeout_seconds}s" \
    --format json >"$result" 2>"$stderr_file"
  nested_exit=$?
  set -e
}

validate_first_report() {
  REPORT="$1" EXPECTED_EXIT="$2" ACTUAL_EXIT="$3" EXPECTED_STATUS="$4" \
    EXPECTED_OUTCOME="$5" REASON_CODE="$6" EXECUTED="$7" CASE_KIND="$8" \
    python3 <<'PY' 2>/dev/null
import json, os

with open(os.environ["REPORT"], encoding="utf-8") as handle:
    report = json.load(handle)
assert int(os.environ["ACTUAL_EXIT"]) == int(os.environ["EXPECTED_EXIT"])
status = os.environ["EXPECTED_STATUS"]
outcome = os.environ["EXPECTED_OUTCOME"]
assert report.get("status") == status
assert (report.get("outcome") or "") == outcome
children = report.get("children") or []
assert len(children) == 1
child = children[0]
assert child.get("status") == status
summary = report.get("summary") or {}
if status == "succeeded":
    assert summary.get("succeeded_count") == 1 and summary.get("needs_human_count") == 0
else:
    assert summary.get("succeeded_count") == 0 and summary.get("needs_human_count") == 1
executed = os.environ["EXECUTED"] == "true"
if executed:
    assert child.get("started_at") and child.get("finished_at")
    assert int(child.get("claim_generation") or 0) >= 1
    assert child.get("claim_outcome") == "claimed"
    assert child.get("attempt_path")
else:
    for field in ("started_at", "finished_at", "claim_outcome", "claim_owner", "claim_generation", "claim_phase", "attempt_path", "worktree_path"):
        assert child.get(field) in (None, "", 0)
    refusals = report.get("refusals") or []
    assert len(refusals) == 1
    assert refusals[0].get("reason_code") == os.environ["REASON_CODE"]
    assert (refusals[0].get("delegation_capability") or {}).get("reason_code") == os.environ["REASON_CODE"]
kind = os.environ["CASE_KIND"]
if kind == "read-only-ok":
    assert (child.get("read_only_enforcement") or {}).get("verification") == "passed"
elif kind == "read-only-mutation":
    assert child.get("outcome") == "read_only_policy_violation"
    codes = [item.get("code") for item in (child.get("read_only_enforcement") or {}).get("violations", [])]
    assert "untracked_file_created" in codes
elif kind == "write-in-scope":
    manifest = child.get("mutation_manifest") or {}
    assert manifest.get("verification") == "passed"
    assert any(item.get("path") == "allowed.txt" for item in manifest.get("changes", []))
    assert child.get("worktree_path")
elif kind == "write-outside-scope":
    assert child.get("outcome") == "write_scope_policy_violation"
    codes = [item.get("code") for item in (child.get("mutation_manifest") or {}).get("violations", [])]
    assert "out_of_scope_mutation" in codes
    assert child.get("worktree_path")
PY
}

validate_replay_report() {
  REPORT="$1" EXPECTED_EXIT="$2" ACTUAL_EXIT="$3" EXPECTED_STATUS="$4" \
    EXPECTED_OUTCOME="$5" REASON_CODE="$6" EXECUTED="$7" \
    python3 <<'PY' 2>/dev/null
import json, os

with open(os.environ["REPORT"], encoding="utf-8") as handle:
    report = json.load(handle)
assert int(os.environ["ACTUAL_EXIT"]) == int(os.environ["EXPECTED_EXIT"])
status = os.environ["EXPECTED_STATUS"]
assert report.get("status") == status
children = report.get("children") or []
assert len(children) == 1
child = children[0]
summary = report.get("summary") or {}
if status == "succeeded":
    assert summary.get("succeeded_count") == 1 and summary.get("needs_human_count") == 0
else:
    assert summary.get("succeeded_count") == 0 and summary.get("needs_human_count") == 1
if os.environ["EXECUTED"] == "true":
    expected_action = "blocked" if status == "needs-human" else "reused"
    assert child.get("replay_action") == expected_action
else:
    assert (report.get("outcome") or "") == os.environ["EXPECTED_OUTCOME"]
    refusals = report.get("refusals") or []
    assert len(refusals) == 1 and refusals[0].get("reason_code") == os.environ["REASON_CODE"]
    for field in ("started_at", "finished_at", "claim_outcome", "claim_owner", "claim_generation", "claim_phase", "attempt_path", "worktree_path"):
        assert child.get(field) in (None, "", 0)
PY
}

validate_progress() {
  PROGRESS="$1" EXECUTED="$2" TERMINAL_STATUS="$3" python3 <<'PY' 2>/dev/null
import json, os

with open(os.environ["PROGRESS"], encoding="utf-8") as handle:
    batch = json.load(handle)
views = batch.get("receipts") or []
if os.environ["EXECUTED"] != "true":
    assert not views
else:
    phases = [(item.get("receipt") or {}).get("phase") for item in views]
    for phase in ("nested.child.queued", "nested.child.running", "nested.child.finished"):
        assert phase in phases
    assert any((item.get("receipt") or {}).get("phase") == "nested.child.finished" and
               (item.get("receipt") or {}).get("status") == os.environ["TERMINAL_STATUS"]
               for item in views)
PY
}

mkdir -p "$repo" "$loopcoder_home" "$plans_dir" "$results_dir"
: > "$completed_file"
: > "$worktrees_file"
export LOOPCODER_HOME="$loopcoder_home"

git -C "$matrix_root" init -b main "$repo" >/dev/null 2>&1 || fail repo_init_failed
git -C "$repo" config user.email permission-matrix@example.invalid || fail repo_config_failed
git -C "$repo" config user.name "Permission Matrix" || fail repo_config_failed
printf '# Permission matrix fixture\n' > "$repo/README.md"
printf 'allowed baseline\n' > "$repo/allowed.txt"
printf 'outside baseline\n' > "$repo/outside.txt"
git -C "$repo" add README.md allowed.txt outside.txt || fail repo_stage_failed
git -C "$repo" commit -m "Initialize permission matrix fixture" >/dev/null || fail repo_commit_failed
git -C "$repo" update-ref refs/remotes/origin/main HEAD || fail origin_main_failed

if ! "$binary" projects register --repo "$repo" --format json >"$results_dir/register.json" 2>"$results_dir/register.stderr"; then
  fail project_registration_failed
fi
if ! REGISTER="$results_dir/register.json" python3 <<'PY' 2>/dev/null
import json, os
with open(os.environ["REGISTER"], encoding="utf-8") as handle:
    payload = json.load(handle)
assert (payload.get("project") or {}).get("project_id")
PY
then
  fail project_registration_invalid
fi
[[ -f "$loopcoder_home/data/loopcoder.db" ]] || fail matrix_home_missing

candidate_hash="$(shasum -a 256 "$binary" | awk '{print $1}')"
if ! version_text="$("$binary" version 2>/dev/null)"; then
  fail candidate_version_failed
fi
case "$version_text" in
  *platform=darwin/arm64*) ;;
  *) fail candidate_platform_invalid ;;
esac

expected_head="$(git -C "$repo" rev-parse HEAD)" || fail parent_head_query_failed
expected_status="$(git -C "$repo" status --porcelain=v1 --untracked-files=all)" || fail parent_status_query_failed
[[ -z "$expected_status" ]] || fail fixture_repo_dirty
initial_worktree_count="$(git -C "$repo" worktree list --porcelain | awk '$1 == "worktree" { count++ } END { print count + 0 }')"

ordinal=0
while IFS='|' read -r slug name permission kind scoped_path expected_exit expected_status_case expected_outcome reason_code executed native_requested; do
  [[ -n "$slug" ]] || continue
  ordinal=$((ordinal + 1))
  active_case="$slug"
  failure_code=case_assertion_failed
  launch_marker="$matrix_root/launch-$slug.txt"
  launch_command="printf 'launch\\n' >> '$launch_marker'"
  case "$kind" in
    read-only-ok) case_command="$launch_command; git status --short" ;;
    read-only-mutation) case_command="$launch_command; printf 'mutation\\n' > read-only-violation.txt" ;;
    write-in-scope) case_command="$launch_command; printf 'matrix write\\n' >> allowed.txt" ;;
    write-outside-scope) case_command="$launch_command; printf 'matrix escape\\n' >> outside.txt" ;;
    refusal) case_command="$launch_command" ;;
    *) fail unknown_case_kind ;;
  esac
  second="$(printf '%02d' "$ordinal")"
  parent_run="run-20260717T0600${second}Z-wave-$slug"
  child_run="run-20260717T0601${second}Z-child-0-$slug"
  plan="$plans_dir/$slug.json"
  CASE_SLUG="$slug" CASE_NAME="$name" PERMISSION="$permission" SCOPED_PATH="$scoped_path" \
    CASE_COMMAND="$case_command" PARENT_RUN="$parent_run" CHILD_RUN="$child_run" \
    ORDINAL="$ordinal" NATIVE_REQUESTED="$native_requested" python3 - "$plan" <<'PY'
import json, os, sys

item = {
    "child_key": os.environ["CASE_SLUG"],
    "title": os.environ["CASE_NAME"],
    "role": "worker",
    "run_id": os.environ["CHILD_RUN"],
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
}
if os.environ["NATIVE_REQUESTED"] == "true":
    item["metadata"] = {"provider_native_subagent": True}
plan = {
    "schema_version": "loopcoder.child_plan.v1",
    "plan_id": "plan-" + os.environ["PARENT_RUN"],
    "parent_run_id": os.environ["PARENT_RUN"],
    "root_run_id": os.environ["PARENT_RUN"],
    "parent_depth": 0,
    "max_depth": 2,
    "max_concurrency": 1,
    "created_at": "2026-07-17T06:00:%02dZ" % int(os.environ["ORDINAL"]),
    "items": [item],
}
with open(sys.argv[1], "w", encoding="utf-8") as handle:
    json.dump(plan, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY

  first_result="$results_dir/$slug-first.json"
  run_nested "$plan" "$first_result" "$results_dir/$slug-first.stderr"
  if ! validate_first_report "$first_result" "$expected_exit" "$nested_exit" "$expected_status_case" "$expected_outcome" "$reason_code" "$executed" "$kind"; then
    fail first_result_invalid
  fi

  launches_before_replay="$(line_count "$launch_marker")"
  if [[ "$executed" == "true" ]]; then
    [[ "$launches_before_replay" -eq 1 ]] || fail provider_launch_count_invalid
    attempt_path="$(json_get "$first_result" children.0.attempt_path)"
    [[ -f "$attempt_path" ]] || fail attempt_not_durable
    outside_repo "$attempt_path" || fail attempt_inside_repo
  else
    [[ "$launches_before_replay" -eq 0 ]] || fail refusal_provider_started
  fi

  worktree_path=""
  case "$kind" in
    read-only-mutation)
      [[ -f "$repo/read-only-violation.txt" ]] || fail read_only_evidence_missing
      rm -f "$repo/read-only-violation.txt"
      ;;
    write-in-scope|write-outside-scope)
      worktree_path="$(json_get "$first_result" children.0.worktree_path)"
      [[ -d "$worktree_path" ]] || fail write_worktree_not_durable
      outside_repo "$worktree_path" || fail write_worktree_inside_repo
      printf '%s\n' "$worktree_path" >> "$worktrees_file"
      ;;
  esac

  progress_result="$results_dir/$slug-progress.json"
  if ! "$binary" status --repo "$repo" --run "$parent_run" --receipts --format json >"$progress_result" 2>"$results_dir/$slug-progress.stderr"; then
    fail progress_query_failed
  fi
  if ! validate_progress "$progress_result" "$executed" "$expected_status_case"; then
    fail progress_evidence_invalid
  fi
  assert_parent_unchanged

  replay_result="$results_dir/$slug-replay.json"
  run_nested "$plan" "$replay_result" "$results_dir/$slug-replay.stderr"
  if ! validate_replay_report "$replay_result" "$expected_exit" "$nested_exit" "$expected_status_case" "$expected_outcome" "$reason_code" "$executed"; then
    fail replay_result_invalid
  fi
  launches_after_replay="$(line_count "$launch_marker")"
  [[ "$launches_after_replay" -eq "$launches_before_replay" ]] || fail replay_provider_started
  assert_parent_unchanged

  replay_action="$(json_get "$replay_result" children.0.replay_action)"
  claim_generation="$(json_get "$first_result" children.0.claim_generation)"
  [[ -n "$claim_generation" ]] || claim_generation=0
  progress_count="$(PROGRESS="$progress_result" python3 -c 'import json,os; print(len(json.load(open(os.environ["PROGRESS"])).get("receipts") or []))')"
  progress_phases="$(PROGRESS="$progress_result" python3 -c 'import json,os; p=json.load(open(os.environ["PROGRESS"])); print(",".join(sorted(set((x.get("receipt") or {}).get("phase", "") for x in p.get("receipts") or [] if (x.get("receipt") or {}).get("phase")))))')"
  audit_codes="$(REPORT="$first_result" CASE_KIND="$kind" python3 <<'PY'
import json, os
with open(os.environ["REPORT"], encoding="utf-8") as handle:
    child = (json.load(handle).get("children") or [{}])[0]
if os.environ["CASE_KIND"] == "read-only-mutation":
    values = (child.get("read_only_enforcement") or {}).get("violations", [])
elif os.environ["CASE_KIND"] == "write-outside-scope":
    values = (child.get("mutation_manifest") or {}).get("violations", [])
else:
    values = []
print(",".join(sorted(set(item.get("code", "") for item in values if item.get("code")))))
PY
)"
  CASE_SLUG="$slug" PERMISSION="$permission" FIRST_STATUS="$expected_status_case" FIRST_OUTCOME="$expected_outcome" \
    REPLAY_ACTION="$replay_action" LAUNCHES_FIRST="$launches_before_replay" CLAIM_GENERATION="$claim_generation" \
    PROGRESS_COUNT="$progress_count" PROGRESS_PHASES="$progress_phases" REASON_CODE="$reason_code" \
    AUDIT_CODES="$audit_codes" EXECUTED="$executed" CASE_KIND="$kind" python3 >> "$completed_file" <<'PY'
import json, os
print(json.dumps({
    "case": os.environ["CASE_SLUG"],
    "permission": os.environ["PERMISSION"],
    "first_status": os.environ["FIRST_STATUS"],
    "first_outcome": os.environ["FIRST_OUTCOME"],
    "replay_action": os.environ["REPLAY_ACTION"],
    "provider_launches_first": int(os.environ["LAUNCHES_FIRST"]),
    "provider_launches_replay": 0,
    "lifecycle_created": os.environ["EXECUTED"] == "true",
    "claim_generation": int(os.environ["CLAIM_GENERATION"]),
    "progress_receipts": int(os.environ["PROGRESS_COUNT"]),
    "progress_phases": [x for x in os.environ["PROGRESS_PHASES"].split(",") if x],
    "reason_code": os.environ["REASON_CODE"],
    "audit_codes": [x for x in os.environ["AUDIT_CODES"].split(",") if x],
    "parent_unchanged": True,
    "worktree_isolated": os.environ["CASE_KIND"].startswith("write-"),
}, sort_keys=True))
PY

  if [[ -n "$worktree_path" ]]; then
    git -C "$repo" worktree remove --force "$worktree_path" >/dev/null 2>&1 || fail worktree_cleanup_failed
  fi
done <<'CASES'
read-only-ok|supported read-only|read-only|read-only-ok|README.md|0|succeeded|||true|false
read-only-mutation|read-only mutation|read-only|read-only-mutation|README.md|1|needs-human|read_only_policy_violation|untracked_file_created|true|false
write-in-scope|bounded write in scope|write|write-in-scope|allowed.txt|0|succeeded|||true|false
write-outside-scope|write outside scope|write|write-outside-scope|allowed.txt|1|needs-human|write_scope_policy_violation|out_of_scope_mutation|true|false
orchestrate-refusal|orchestrate refusal|orchestrate|refusal|README.md|1|needs-human|permission_not_enforceable|orchestrate_unsupported|false|false
provider-native-refusal|provider-native refusal|read-only|refusal|README.md|1|needs-human|permission_not_enforceable|provider_native_bridge_required|false|true
unknown-permission|unknown permission refusal|admin|refusal|README.md|1|needs-human|permission_not_enforceable|nested_permission_unknown|false|false
CASES

final_worktree_count="$(git -C "$repo" worktree list --porcelain | awk '$1 == "worktree" { count++ } END { print count + 0 }')"
[[ "$final_worktree_count" -eq "$initial_worktree_count" ]] || fail worktree_leak
[[ ! -e "$repo/.loopcoder" ]] || fail repo_local_payload_created
assert_parent_unchanged
elapsed_seconds=$(( $(date +%s) - started_epoch ))
[[ "$elapsed_seconds" -le "$max_duration_seconds" ]] || fail duration_ceiling_exceeded

CANDIDATE_SOURCE="$candidate_source" CANDIDATE_HASH="$candidate_hash" VERSION_TEXT="$version_text" \
  CASE_TIMEOUT="$case_timeout_seconds" MAX_DURATION="$max_duration_seconds" MAX_DIAGNOSTIC="$max_diagnostic_bytes" \
  ELAPSED_SECONDS="$elapsed_seconds" python3 - "$completed_file" "$artifact_dir/permission-matrix-evidence.json" <<'PY'
import json, os, sys

with open(sys.argv[1], encoding="utf-8") as handle:
    cases = [json.loads(line) for line in handle if line.strip()]
evidence = {
    "schema_version": "loopcoder.nested_permission_matrix_evidence.v1",
    "status": "passed",
    "candidate": {
        "source": os.environ["CANDIDATE_SOURCE"],
        "sha256": os.environ["CANDIDATE_HASH"],
        "version": os.environ["VERSION_TEXT"],
        "host_tuple": "darwin/arm64",
    },
    "fixture": {
        "provider": "test-subprocess",
        "deterministic": True,
        "paid_provider_calls": 0,
        "clean_temporary_home": True,
        "clean_temporary_repository": True,
        "repository_local_payload": False,
    },
    "resource_limits": {
        "cases": 7,
        "invocations": 14,
        "max_concurrency": 1,
        "max_depth": 2,
        "per_invocation_timeout_seconds": int(os.environ["CASE_TIMEOUT"]),
        "max_matrix_duration_seconds": int(os.environ["MAX_DURATION"]),
        "network_required": False,
    },
    "cases": cases,
    "real_provider_canaries": {
        "blocking": False,
        "opt_in_gate": "LOOPCODER_REAL_PROVIDER_SMOKE=1",
        "fork_pull_request_secrets_required": False,
        "read_only_providers": ["codex", "claude", "grok"],
        "bounded_write_providers": ["codex", "grok"],
    },
    "diagnostics": {
        "schema_version": "loopcoder.nested_permission_matrix_diagnostic.v1",
        "max_bytes": int(os.environ["MAX_DIAGNOSTIC"]),
        "paths_included": False,
        "prompts_included": False,
        "credentials_included": False,
        "raw_output_included": False,
    },
    "elapsed_seconds": int(os.environ["ELAPSED_SECONDS"]),
}
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(evidence, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY

echo "nested permission matrix passed: cases=7 invocations=14 provider=test-subprocess paid_calls=0"
