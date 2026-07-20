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
assert_file_contains "${tmp_root}/codex_success/release-provider-canary-codex.json" '"blocking": true'
assert_file_contains "${tmp_root}/claude_success/release-provider-canary-claude.json" '"blocking": true'

# Failure scenarios (typed infrastructure vs product) — blocking hard-fail.
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

# Non-blocking Grok / Antigravity: success + not_available paths (exit 0).
run_fixture grok success 0
assert_file_contains "${tmp_root}/grok_success/release-provider-canary-grok.json" '"blocking": false'
assert_file_contains "${tmp_root}/grok_success/release-provider-canary-grok.json" '"status": "passed"'
assert_file_contains "${tmp_root}/grok_success/release-provider-canary-grok.json" '"provider": "grok"'

run_fixture antigravity success 0
assert_file_contains "${tmp_root}/antigravity_success/release-provider-canary-antigravity.json" '"blocking": false'
assert_file_contains "${tmp_root}/antigravity_success/release-provider-canary-antigravity.json" '"provider": "antigravity"'

run_fixture grok missing_cli 0
assert_file_contains "${tmp_root}/grok_missing_cli/release-provider-canary-grok.json" '"status": "not_available"'
assert_file_contains "${tmp_root}/grok_missing_cli/release-provider-canary-grok.json" '"detail_code": "cli_not_found"'

run_fixture antigravity not_available 0
assert_file_contains "${tmp_root}/antigravity_not_available/release-provider-canary-antigravity.json" '"status": "not_available"'

run_fixture grok model_unavailable 0
assert_file_contains "${tmp_root}/grok_model_unavailable/release-provider-canary-grok.json" '"detail_code": "model_unavailable"'

run_fixture antigravity auth_failure 0
assert_file_contains "${tmp_root}/antigravity_auth_failure/release-provider-canary-antigravity.json" '"status": "not_available"'
assert_file_contains "${tmp_root}/antigravity_auth_failure/release-provider-canary-antigravity.json" '"detail_code": "auth_unavailable"'

# Non-blocking quota gaps are unknown, never fabricated zero quota.
run_fixture grok quota_failure 0
assert_file_contains "${tmp_root}/grok_quota_failure/release-provider-canary-grok.json" '"detail_code": "quota_unknown"'
assert_file_not_contains "${tmp_root}/grok_quota_failure/release-provider-canary-grok.json" 'quota_exhausted'

# No silent cross-provider fallback: running codex must not write claude/grok evidence.
codex_only="${tmp_root}/codex_only"
mkdir -p "$codex_only"
bash "$canary" --mode fixture --provider codex --scenario success --artifact-dir "$codex_only" >/dev/null
test -f "${codex_only}/release-provider-canary-codex.json"
if [[ -e "${codex_only}/release-provider-canary-claude.json" || -e "${codex_only}/release-provider-canary-grok.json" ]]; then
  echo "FAIL codex canary wrote another provider evidence" >&2
  exit 1
fi
# Grok must not write antigravity evidence either.
grok_only="${tmp_root}/grok_only"
mkdir -p "$grok_only"
bash "$canary" --mode fixture --provider grok --scenario success --artifact-dir "$grok_only" >/dev/null
test -f "${grok_only}/release-provider-canary-grok.json"
if [[ -e "${grok_only}/release-provider-canary-antigravity.json" || -e "${grok_only}/release-provider-canary-codex.json" ]]; then
  echo "FAIL grok canary wrote another provider evidence" >&2
  exit 1
fi

# Live mode refused without opt-in (clear ambient CI event vars).
set +e
(
  unset LOOPCODER_REAL_PROVIDER_CANARY || true
  unset GITHUB_EVENT_NAME || true
  unset GITHUB_EVENT_PATH || true
  bash "$canary" --mode live --provider codex --binary /bin/sh
) >"${tmp_root}/live_no_opt.out" 2>"${tmp_root}/live_no_opt.err"
live_status=$?
set -e
assert_eq "$live_status" 2 "live without opt-in"
assert_file_contains "${tmp_root}/live_no_opt.err" "LOOPCODER_REAL_PROVIDER_CANARY=1"

# Live mode refused on pull_request events even with opt-in.
set +e
(
  export LOOPCODER_REAL_PROVIDER_CANARY=1
  export GITHUB_EVENT_NAME=pull_request
  unset GITHUB_EVENT_PATH || true
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

# Unknown provider rejected.
set +e
bash "$canary" --mode fixture --provider nope --scenario success >"${tmp_root}/nope.out" 2>"${tmp_root}/nope.err"
nope_status=$?
set -e
assert_eq "$nope_status" 2 "unknown provider rejected"

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
