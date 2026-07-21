#!/usr/bin/env bash
# Deterministic offline tests for scripts/assert-pre-prod-green.sh provenance.
# No network: installs a fake `gh` on PATH that serves fixture JSON.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/scripts/assert-pre-prod-green.sh"
chmod +x "$script"

tmp_root="$(mktemp -d)"
trap 'rm -rf "${tmp_root}"' EXIT

SHA="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
SHA_OTHER="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
RUN_NEW="900001"
RUN_OLD="800001"
RUN_OTHER_WF="700001"
SUITE_NEW=5001
SUITE_OLD=5000
SUITE_OTHER=4999

bin="${tmp_root}/bin"
fix="${tmp_root}/fixtures"
mkdir -p "$bin" "$fix"

pass=0
fail=0

assert_exit() {
  local label="$1" expect="$2"
  shift 2
  local out="${tmp_root}/${label}.out" err="${tmp_root}/${label}.err"
  set +e
  env PATH="${bin}:${PATH}" NO_COLOR=1 CLICOLOR=0 GH_FORCE_TTY=0 \
    bash "$script" "$@" >"$out" 2>"$err"
  local status=$?
  set -e
  if [[ "$status" -ne "$expect" ]]; then
    echo "FAIL $label: exit=$status want=$expect" >&2
    echo "--- stdout ---" >&2
    cat "$out" >&2 || true
    echo "--- stderr ---" >&2
    cat "$err" >&2 || true
    fail=$((fail + 1))
    return 0
  fi
  echo "ok $label (exit $expect)"
  pass=$((pass + 1))
}

write_check_runs() {
  # $1 = fixture file content (full JSON body for check-runs endpoint)
  printf '%s\n' "$1" >"${fix}/check_runs.json"
}

write_run() {
  local id="$1"
  printf '%s\n' "$2" >"${fix}/run_${id}.json"
}

# Fake gh: only supports the two API shapes assert-pre-prod-green uses.
cat >"${bin}/gh" <<'EOS'
#!/usr/bin/env bash
set -euo pipefail
# args: api <path> [--paginate] -H ...
if [[ "${1:-}" != "api" ]]; then
  echo "fake-gh: unexpected: $*" >&2
  exit 99
fi
path="${2:-}"
fixture_dir="${ASSERT_PRE_PROD_FIXTURE_DIR:?}"
case "$path" in
  repos/{owner}/{repo}/commits/*/check-runs|repos/*/commits/*/check-runs)
    cat "${fixture_dir}/check_runs.json"
    ;;
  repos/{owner}/{repo}/actions/runs/*|repos/*/actions/runs/*)
    rid="${path##*/actions/runs/}"
    rid="${rid%%/*}"
    f="${fixture_dir}/run_${rid}.json"
    if [[ ! -f "$f" ]]; then
      echo "fake-gh: missing run fixture for $rid" >&2
      exit 1
    fi
    # Optional forced API failure
    if [[ -f "${fixture_dir}/fail_runs" ]]; then
      echo "fake-gh: forced api failure" >&2
      exit 1
    fi
    cat "$f"
    ;;
  *)
    echo "fake-gh: unhandled path: $path" >&2
    exit 1
    ;;
esac
EOS
chmod +x "${bin}/gh"
export ASSERT_PRE_PROD_FIXTURE_DIR="$fix"

make_check() {
  # name run_id suite_id status conclusion [details_url_override] [app_json]
  local name="$1" run_id="$2" suite_id="$3" status="$4" conclusion="$5"
  local details="${6:-https://github.com/jasonhnd/loopcoder/actions/runs/${run_id}/job/1}"
  local app_json="${7-}"
  if [[ -z "$app_json" ]]; then
    app_json='{"id": 15368, "slug": "github-actions", "name": "GitHub Actions"}'
  fi
  local conc_json
  if [[ -z "$conclusion" || "$conclusion" == "null" ]]; then
    conc_json="null"
  else
    conc_json="\"${conclusion}\""
  fi
  cat <<EOF
{
  "id": ${run_id}01,
  "name": "${name}",
  "status": "${status}",
  "conclusion": ${conc_json},
  "details_url": "${details}",
  "html_url": "https://github.com/jasonhnd/loopcoder/actions/runs/${run_id}/job/1",
  "started_at": "2026-07-21T12:00:00Z",
  "completed_at": "2026-07-21T12:05:00Z",
  "app": ${app_json},
  "check_suite": {"id": ${suite_id}}
}
EOF
}

make_workflow_run() {
  local id="$1" path="$2" event="$3" branch="$4" head_sha="$5" status="$6" conclusion="$7" suite_id="$8" created="$9"
  local conc_json
  if [[ -z "$conclusion" || "$conclusion" == "null" ]]; then
    conc_json="null"
  else
    conc_json="\"${conclusion}\""
  fi
  cat <<EOF
{
  "id": ${id},
  "path": "${path}",
  "event": "${event}",
  "head_branch": "${branch}",
  "head_sha": "${head_sha}",
  "status": "${status}",
  "conclusion": ${conc_json},
  "check_suite_id": ${suite_id},
  "created_at": "${created}",
  "run_started_at": "${created}",
  "run_attempt": 1
}
EOF
}

wrap_checks() {
  # joins check JSON objects as array
  local first=1
  printf '{"total_count":99,"check_runs":['
  for item in "$@"; do
    [[ $first -eq 1 ]] || printf ','
    first=0
    printf '%s' "$item"
  done
  printf ']}\n'
}

GOOD_PATH=".github/workflows/pre-prod-integration.yml"
OTHER_PATH=".github/workflows/ci.yml"

# ---------- 1) success: correct workflow/push/pre-prod/exact-SHA, same run ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_NEW" "2026-07-21T12:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit success_same_run 0 --sha "$SHA" --quiet

# ---------- 2) same job names from another workflow ----------
write_run "$RUN_OTHER_WF" "$(make_workflow_run "$RUN_OTHER_WF" "$OTHER_PATH" push pre-prod "$SHA" completed success "$SUITE_OTHER" "2026-07-21T13:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_OTHER_WF" "$SUITE_OTHER" completed success)" \
  "$(make_check integration-canary "$RUN_OTHER_WF" "$SUITE_OTHER" completed success)" \
)"
assert_exit other_workflow 1 --sha "$SHA" --quiet

# ---------- 3a) pull_request event ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" pull_request pre-prod "$SHA" completed success "$SUITE_NEW" "2026-07-21T12:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit event_pull_request 1 --sha "$SHA" --quiet

# ---------- 3b) workflow_dispatch event ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" workflow_dispatch pre-prod "$SHA" completed success "$SUITE_NEW" "2026-07-21T12:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit event_workflow_dispatch 1 --sha "$SHA" --quiet

# ---------- 4a) wrong branch ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push main "$SHA" completed success "$SUITE_NEW" "2026-07-21T12:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit wrong_branch 1 --sha "$SHA" --quiet

# ---------- 4b) wrong SHA (run head_sha differs from requested) ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA_OTHER" completed success "$SUITE_NEW" "2026-07-21T12:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit wrong_sha 1 --sha "$SHA" --quiet

# ---------- 5) mixed jobs from different runs ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_NEW" "2026-07-21T13:00:00Z")"
write_run "$RUN_OLD" "$(make_workflow_run "$RUN_OLD" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_OLD" "2026-07-21T11:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success)" \
  "$(make_check integration-canary "$RUN_OLD" "$SUITE_OLD" completed success)" \
)"
assert_exit mixed_runs 1 --sha "$SHA" --quiet

# ---------- 6a) newer failed run with older successful run ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA" completed failure "$SUITE_NEW" "2026-07-21T14:00:00Z")"
write_run "$RUN_OLD" "$(make_workflow_run "$RUN_OLD" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_OLD" "2026-07-21T10:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed failure)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed failure)" \
  "$(make_check integration-verify "$RUN_OLD" "$SUITE_OLD" completed success)" \
  "$(make_check integration-canary "$RUN_OLD" "$SUITE_OLD" completed success)" \
)"
assert_exit newer_failed 1 --sha "$SHA" --quiet

# ---------- 6b) newer pending run with older successful run ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA" in_progress null "$SUITE_NEW" "2026-07-21T15:00:00Z")"
write_run "$RUN_OLD" "$(make_workflow_run "$RUN_OLD" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_OLD" "2026-07-21T10:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" in_progress null)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" queued null)" \
  "$(make_check integration-verify "$RUN_OLD" "$SUITE_OLD" completed success)" \
  "$(make_check integration-canary "$RUN_OLD" "$SUITE_OLD" completed success)" \
)"
assert_exit newer_pending 1 --sha "$SHA" --quiet

# ---------- 7a) missing app identity ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_NEW" "2026-07-21T12:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success "https://github.com/jasonhnd/loopcoder/actions/runs/${RUN_NEW}/job/1" '{}')" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit missing_app 1 --sha "$SHA" --quiet

# ---------- 7b) malformed details_url (no run id) ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_NEW" "2026-07-21T12:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success 'https://example.com/not-a-run')" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit malformed_url 1 --sha "$SHA" --quiet

# ---------- 7c) missing check_suite_id on workflow run ----------
write_run "$RUN_NEW" "$(cat <<EOF
{
  "id": ${RUN_NEW},
  "path": "${GOOD_PATH}",
  "event": "push",
  "head_branch": "pre-prod",
  "head_sha": "${SHA}",
  "status": "completed",
  "conclusion": "success",
  "created_at": "2026-07-21T12:00:00Z"
}
EOF
)"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit missing_run_suite 1 --sha "$SHA" --quiet

# ---------- 7d) check_suite_id mismatch ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_NEW" "2026-07-21T12:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" 9999 completed success)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit suite_mismatch 1 --sha "$SHA" --quiet

# ---------- 7e) API failure ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_NEW" "2026-07-21T12:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
touch "${fix}/fail_runs"
assert_exit api_failure 1 --sha "$SHA" --quiet
rm -f "${fix}/fail_runs"

# ---------- 7f) malformed API JSON ----------
printf '{not-json\n' >"${fix}/check_runs.json"
assert_exit malformed_json 1 --sha "$SHA" --quiet

# ---------- 7g) empty app slug+name with present object ----------
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_NEW" "2026-07-21T12:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success "https://github.com/jasonhnd/loopcoder/actions/runs/${RUN_NEW}/job/1" '{"id":1,"slug":"","name":""}')" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit empty_app_identity 1 --sha "$SHA" --quiet

# Re-confirm success still works after failure cases (fixture isolation).
write_run "$RUN_NEW" "$(make_workflow_run "$RUN_NEW" "$GOOD_PATH" push pre-prod "$SHA" completed success "$SUITE_NEW" "2026-07-21T16:00:00Z")"
write_check_runs "$(wrap_checks \
  "$(make_check integration-verify "$RUN_NEW" "$SUITE_NEW" completed success)" \
  "$(make_check integration-canary "$RUN_NEW" "$SUITE_NEW" completed success)" \
)"
assert_exit success_again 0 --sha "$SHA" --quiet

echo "assert-pre-prod-green_test: pass=${pass} fail=${fail}"
if [[ "$fail" -ne 0 ]]; then
  exit 1
fi
