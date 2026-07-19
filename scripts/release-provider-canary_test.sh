#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
canary="${repo_root}/scripts/release-provider-canary.sh"
chmod +x "$canary"

tmp_root="$(mktemp -d)"
trap 'rm -rf "${tmp_root}"' EXIT

assert_eq() {
  local got="$1" want="$2" label="$3"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL $label: got=$got want=$want" >&2
    exit 1
  fi
}

assert_file_contains() {
  local file="$1" needle="$2"
  if ! grep -Fq -- "$needle" "$file"; then
    echo "FAIL expected $file to contain: $needle" >&2
    cat "$file" >&2
    exit 1
  fi
}

assert_file_not_contains() {
  local file="$1" needle="$2"
  if grep -Fq -- "$needle" "$file"; then
    echo "FAIL expected $file NOT to contain: $needle" >&2
    cat "$file" >&2
    exit 1
  fi
}

run_fixture() {
  local provider="$1" scenario="$2" expect_status="$3"
  local dir="${tmp_root}/${provider}_${scenario}"
  mkdir -p "$dir"
  set +e
  bash "$canary" \
    --mode fixture \
    --provider "$provider" \
    --scenario "$scenario" \
    --artifact-dir "$dir" \
    >"${dir}/stdout.txt" 2>"${dir}/stderr.txt"
  local status=$?
  set -e
  assert_eq "$status" "$expect_status" "${provider}/${scenario} exit"
  local evidence="${dir}/release-provider-canary-${provider}.json"
  test -f "$evidence"
  assert_file_contains "$evidence" '"schema_version": "loopcoder.release_provider_canary.v1"'
  assert_file_contains "$evidence" "\"provider\": \"${provider}\""
  assert_file_contains "$evidence" '"credentials_included": false'
  assert_file_contains "$evidence" '"prompts_included": false'
  assert_file_contains "$evidence" '"fallback_provider": null'
  assert_file_contains "$evidence" '"retries": 0'
  assert_file_contains "$evidence" '"concurrency": 1'
  # Secret canary must never appear if somehow injected via env into detail paths.
  assert_file_not_contains "$evidence" "sk-secret"
  assert_file_not_contains "$evidence" "/Users/alice"
}

# Success for both blocking providers.
run_fixture codex success 0
run_fixture claude success 0
assert_file_contains "${tmp_root}/codex_success/release-provider-canary-codex.json" '"status": "passed"'
assert_file_contains "${tmp_root}/claude_success/release-provider-canary-claude.json" '"status": "passed"'

# Failure scenarios (typed infrastructure vs product).
run_fixture codex auth_failure 1
assert_file_contains "${tmp_root}/codex_auth_failure/release-provider-canary-codex.json" '"result_class": "infrastructure"'
assert_file_contains "${tmp_root}/codex_auth_failure/release-provider-canary-codex.json" '"detail_code": "auth_unavailable"'

run_fixture claude quota_failure 1
assert_file_contains "${tmp_root}/claude_quota_failure/release-provider-canary-claude.json" '"detail_code": "quota_exhausted"'

run_fixture codex timeout 1
assert_file_contains "${tmp_root}/codex_timeout/release-provider-canary-codex.json" '"cancelled": true'

run_fixture claude malformed_output 1
assert_file_contains "${tmp_root}/claude_malformed_output/release-provider-canary-claude.json" '"result_class": "product"'

run_fixture codex cancel 1
assert_file_contains "${tmp_root}/codex_cancel/release-provider-canary-codex.json" '"detail_code": "cancelled"'

run_fixture claude missing_cli 1
assert_file_contains "${tmp_root}/claude_missing_cli/release-provider-canary-claude.json" '"detail_code": "cli_not_found"'

# No silent cross-provider fallback: running codex must not write claude evidence.
codex_only="${tmp_root}/codex_only"
mkdir -p "$codex_only"
bash "$canary" --mode fixture --provider codex --scenario success --artifact-dir "$codex_only" >/dev/null
test -f "${codex_only}/release-provider-canary-codex.json"
if [[ -e "${codex_only}/release-provider-canary-claude.json" ]]; then
  echo "FAIL codex canary wrote claude evidence" >&2
  exit 1
fi

# Live mode refused without opt-in.
set +e
bash "$canary" --mode live --provider codex --binary /bin/sh >"${tmp_root}/live_no_opt.out" 2>"${tmp_root}/live_no_opt.err"
live_status=$?
set -e
assert_eq "$live_status" 2 "live without opt-in"
assert_file_contains "${tmp_root}/live_no_opt.err" "LOOPCODER_REAL_PROVIDER_CANARY=1"

# Live mode refused on pull_request events even with opt-in.
set +e
(
  export LOOPCODER_REAL_PROVIDER_CANARY=1
  export GITHUB_EVENT_NAME=pull_request
  bash "$canary" --mode live --provider claude --binary /bin/sh
) >"${tmp_root}/live_pr.out" 2>"${tmp_root}/live_pr.err"
pr_status=$?
set -e
assert_eq "$pr_status" 78 "live on pull_request"
assert_file_contains "${tmp_root}/live_pr.err" "pull_request"

# max-calls must be 1.
set +e
bash "$canary" --mode fixture --provider codex --scenario success --max-calls 3 >"${tmp_root}/mc.out" 2>"${tmp_root}/mc.err"
mc_status=$?
set -e
assert_eq "$mc_status" 2 "max-calls>1 rejected"

# Unsupported provider rejected.
set +e
bash "$canary" --mode fixture --provider grok --scenario success >"${tmp_root}/grok.out" 2>"${tmp_root}/grok.err"
g_status=$?
set -e
assert_eq "$g_status" 2 "grok rejected for blocking canary script"

# Fork context refuse (synthetic event payload).
fork_event="${tmp_root}/fork_event.json"
printf '%s\n' '{"repository":{"fork":true}}' >"$fork_event"
set +e
(
  export LOOPCODER_REAL_PROVIDER_CANARY=1
  export GITHUB_EVENT_NAME=workflow_dispatch
  export GITHUB_EVENT_PATH="$fork_event"
  bash "$canary" --mode live --provider codex --binary /bin/sh
) >"${tmp_root}/fork.out" 2>"${tmp_root}/fork.err"
fork_status=$?
set -e
assert_eq "$fork_status" 78 "fork refused"

echo "release-provider-canary tests passed"
