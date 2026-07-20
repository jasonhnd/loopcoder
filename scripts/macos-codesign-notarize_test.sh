#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/scripts/macos-codesign-notarize.sh"
chmod +x "$script"

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

# Create a fake Mach-O-like executable payload (any executable file is fine for dry-run).
fake_bin="${tmp_root}/loopcoder"
printf '#!/bin/sh\necho loopcoder-fake\n' >"$fake_bin"
chmod +x "$fake_bin"
digest_before="$(shasum -a 256 "$fake_bin" | awk '{print $1}')"

# dry-run success
dry_dir="${tmp_root}/dry"
mkdir -p "$dry_dir"
bash "$script" --mode dry-run --binary "$fake_bin" --artifact-dir "$dry_dir" >"${dry_dir}/out.txt"
test -f "${dry_dir}/macos-codesign-evidence.json"
assert_file_contains "${dry_dir}/macos-codesign-evidence.json" '"schema_version": "loopcoder.macos_codesign.v1"'
assert_file_contains "${dry_dir}/macos-codesign-evidence.json" '"status": "passed"'
assert_file_contains "${dry_dir}/macos-codesign-evidence.json" '"mode": "dry-run"'
assert_file_contains "${dry_dir}/macos-codesign-evidence.json" "\"before_sha256\": \"${digest_before}\""
assert_file_contains "${dry_dir}/macos-codesign-evidence.json" '"credentials_included": false'
assert_file_contains "${dry_dir}/macos-codesign-evidence.json" '"certificate_material_included": false'
assert_file_not_contains "${dry_dir}/macos-codesign-evidence.json" "BEGIN CERTIFICATE"
assert_file_not_contains "${dry_dir}/macos-codesign-evidence.json" "password"

# dry-run missing binary
set +e
bash "$script" --mode dry-run --binary "${tmp_root}/missing" --artifact-dir "${tmp_root}/miss" >"${tmp_root}/miss.out" 2>"${tmp_root}/miss.err"
miss_status=$?
set -e
assert_eq "$miss_status" 2 "missing binary"

# live refuses without opt-in
set +e
(
  unset APPLE_SIGN || true
  unset GITHUB_EVENT_NAME || true
  unset GITHUB_EVENT_PATH || true
  bash "$script" --mode live --binary "$fake_bin"
) >"${tmp_root}/live_no.out" 2>"${tmp_root}/live_no.err"
live_no=$?
set -e
assert_eq "$live_no" 2 "live without opt-in"
assert_file_contains "${tmp_root}/live_no.err" "APPLE_SIGN=1"

# live refuses pull_request even with opt-in
set +e
(
  export APPLE_SIGN=1
  export GITHUB_EVENT_NAME=pull_request
  unset GITHUB_EVENT_PATH || true
  bash "$script" --mode live --binary "$fake_bin" --identity "Developer ID Application: Test" --team-id ABCDE12345 --keychain-profile test
) >"${tmp_root}/live_pr.out" 2>"${tmp_root}/live_pr.err"
live_pr=$?
set -e
assert_eq "$live_pr" 78 "live on pull_request"

# live fails closed on missing identity (with opt-in, trusted event)
set +e
(
  export APPLE_SIGN=1
  export GITHUB_EVENT_NAME=workflow_dispatch
  unset GITHUB_EVENT_PATH || true
  unset APPLE_CODESIGN_IDENTITY || true
  bash "$script" --mode live --binary "$fake_bin" --artifact-dir "${tmp_root}/live_id" --team-id ABCDE12345 --keychain-profile test
) >"${tmp_root}/live_id.out" 2>"${tmp_root}/live_id.err"
live_id=$?
set -e
assert_eq "$live_id" 1 "live missing identity"
assert_file_contains "${tmp_root}/live_id/macos-codesign-evidence.json" '"detail_code": "missing_identity"'

# fork event refuse
fork_event="${tmp_root}/fork.json"
printf '%s\n' '{"repository":{"fork":true}}' >"$fork_event"
set +e
(
  export APPLE_SIGN=1
  export GITHUB_EVENT_NAME=workflow_dispatch
  export GITHUB_EVENT_PATH="$fork_event"
  bash "$script" --mode live --binary "$fake_bin" --identity "Developer ID Application: Test" --team-id ABCDE12345 --keychain-profile test
) >"${tmp_root}/fork.out" 2>"${tmp_root}/fork.err"
fork_status=$?
set -e
assert_eq "$fork_status" 78 "fork refused"

# Binary digest unchanged by dry-run
digest_after="$(shasum -a 256 "$fake_bin" | awk '{print $1}')"
assert_eq "$digest_after" "$digest_before" "dry-run must not mutate binary"

echo "macos-codesign-notarize tests passed"
