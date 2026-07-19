#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
gate="${repo_root}/scripts/v081-product-path-gate.sh"
chmod +x "$gate"

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

# Fixture mode should be runnable without a packaged binary.
# Apple harness may be missing on this branch until #1022 merges — gate then fails
# apple_trust. For unit tests of the orchestrator, we only require the script to
# emit evidence and treat missing apple as fail in full sense; here we check
# structure on a synthetic partial path by running with a stub.

# 1) usage error
set +e
bash "$gate" --mode weird >"${tmp_root}/usage.out" 2>"${tmp_root}/usage.err"
st=$?
set -e
assert_eq "$st" 2 "bad mode"

# 2) packaged without binary
set +e
bash "$gate" --mode packaged --artifact-dir "${tmp_root}/nobin" >"${tmp_root}/nobin.out" 2>"${tmp_root}/nobin.err"
st=$?
set -e
assert_eq "$st" 2 "packaged without binary"

# 3) full fixture run when harness scripts exist
art="${tmp_root}/fixture"
set +e
bash "$gate" --mode fixture --artifact-dir "$art" --candidate-sha deadbeef --max-seconds 600 \
  >"${tmp_root}/fixture.out" 2>"${tmp_root}/fixture.err"
st=$?
set -e

test -f "${art}/v081-go-no-go-evidence.json"
test -f "${art}/v081-go-no-go-report.md"
assert_file_contains "${art}/v081-go-no-go-evidence.json" '"schema_version": "loopcoder.v081_go_no_go.v1"'
assert_file_contains "${art}/v081-go-no-go-evidence.json" '"live_provider_substitution_allowed": false'
assert_file_contains "${art}/v081-go-no-go-evidence.json" '"credentials_included": false'
assert_file_contains "${art}/v081-go-no-go-report.md" "v0.8.1 Go/No-Go Evidence"
assert_file_not_contains "${art}/v081-go-no-go-evidence.json" "sk-secret"
assert_file_not_contains "${art}/v081-go-no-go-report.md" "/Users/"

# Evidence must record that live canaries were not substituted.
assert_file_contains "${art}/v081-go-no-go-evidence.json" '"name": "live_canary_codex"'
assert_file_contains "${art}/v081-go-no-go-evidence.json" '"status": "not_run"'

# Installer + canary fixtures should pass if present on pre-prod.
assert_file_contains "${art}/v081-go-no-go-evidence.json" '"name": "installer_default_and_custom_path"'
assert_file_contains "${art}/v081-go-no-go-evidence.json" '"name": "provider_canary_fixtures"'

# Decision depends on whether apple harness is merged yet.
if [[ -f "${repo_root}/scripts/macos-codesign-notarize_test.sh" ]]; then
  assert_eq "$st" 0 "fixture GO when apple harness present"
  assert_file_contains "${art}/v081-go-no-go-evidence.json" '"decision": "GO"'
else
  assert_eq "$st" 1 "fixture NO-GO when apple harness missing"
  assert_file_contains "${art}/v081-go-no-go-evidence.json" '"decision": "NO-GO"'
  assert_file_contains "${art}/v081-go-no-go-evidence.json" 'macos-codesign-notarize.sh missing'
fi

echo "v081-product-path-gate tests passed"
