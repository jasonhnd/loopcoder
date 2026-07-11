#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${repo_root}/scripts/stage-draft-release.sh"

tmp_root="$(mktemp -d)"
trap 'rm -rf "${tmp_root}"' EXIT

stub_dir="${tmp_root}/bin"
mkdir -p "${stub_dir}"

cat >"${stub_dir}/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail

printf '%s' "$1" >>"${GH_STUB_LOG}"
shift
for arg in "$@"; do
  printf ' %s' "$arg" >>"${GH_STUB_LOG}"
done
printf '\n' >>"${GH_STUB_LOG}"

case "${1:-}" in
  "repos/${GH_REPO}/releases")
    cat "${GH_STUB_RELEASES_FILE}"
    ;;
  "repos/${GH_REPO}/releases/tags/${TAG_NAME}")
    printf '{"message":"Not Found","documentation_url":"https://docs.github.com/rest/releases/releases#get-a-release-by-tag"}'
    exit 1
    ;;
  create|edit|upload)
    ;;
  *)
    echo "unexpected gh invocation: gh $*" >&2
    exit 2
    ;;
esac
STUB
chmod +x "${stub_dir}/gh"

assert_contains() {
  local file="$1"
  local expected="$2"
  if ! grep -Fq -- "${expected}" "${file}"; then
    echo "expected ${file} to contain: ${expected}" >&2
    echo "actual:" >&2
    cat "${file}" >&2
    exit 1
  fi
}

assert_not_contains() {
  local file="$1"
  local unexpected="$2"
  if grep -Fq -- "${unexpected}" "${file}"; then
    echo "expected ${file} not to contain: ${unexpected}" >&2
    echo "actual:" >&2
    cat "${file}" >&2
    exit 1
  fi
}

run_case() {
  local name="$1"
  local releases_json="$2"
  local expected_status="$3"
  local case_dir

  case_dir="${tmp_root}/${name}"
  mkdir -p "${case_dir}/dist"
  printf '%s\n' "binary" >"${case_dir}/dist/loopcoder_0.7.0_linux_amd64.tar.gz"
  printf '%s\n' "checksums" >"${case_dir}/dist/SHA256SUMS"
  printf '%s\n' "signature" >"${case_dir}/dist/SHA256SUMS.sigstore"
  printf '%s\n' "${releases_json}" >"${case_dir}/releases.json"
  : >"${case_dir}/gh.log"

  (
    cd "${case_dir}"
    export PATH="${stub_dir}:${PATH}"
    export GH_REPO="owner/repo"
    export TAG_NAME="v0.7.0"
    export GITHUB_REPOSITORY="owner/repo"
    export GITHUB_SERVER_URL="https://github.com"
    export GH_STUB_LOG="${case_dir}/gh.log"
    export GH_STUB_RELEASES_FILE="${case_dir}/releases.json"

    set +e
    (set -e; stage_draft_release) >"${case_dir}/stdout.txt" 2>"${case_dir}/stderr.txt"
    status="$?"
    set -e

    if [[ "${status}" -ne "${expected_status}" ]]; then
      echo "${name}: expected exit ${expected_status}, got ${status}" >&2
      echo "stdout:" >&2
      cat "${case_dir}/stdout.txt" >&2
      echo "stderr:" >&2
      cat "${case_dir}/stderr.txt" >&2
      exit 1
    fi
  )
}

run_case "create_none" '[[]]' 0
assert_contains "${tmp_root}/create_none/gh.log" "api repos/owner/repo/releases --paginate --slurp"
assert_contains "${tmp_root}/create_none/gh.log" "release create v0.7.0"
assert_contains "${tmp_root}/create_none/gh.log" "--draft"
assert_contains "${tmp_root}/create_none/gh.log" "--prerelease"
assert_contains "${tmp_root}/create_none/gh.log" "--verify-tag"

run_case "update_draft" '[[{"id":1,"tag_name":"v0.7.0","draft":true}]]' 0
assert_contains "${tmp_root}/update_draft/gh.log" "release edit v0.7.0 --repo owner/repo --prerelease --notes-file release-notes.md"
assert_contains "${tmp_root}/update_draft/gh.log" "release upload v0.7.0 --repo owner/repo"
assert_contains "${tmp_root}/update_draft/gh.log" "--clobber"
assert_not_contains "${tmp_root}/update_draft/gh.log" "release create"

run_case "refuse_public" '[[{"id":2,"tag_name":"v0.7.0","draft":false}]]' 1
assert_contains "${tmp_root}/refuse_public/stderr.txt" "release v0.7.0 already exists and is public; refusing to overwrite final release"
assert_not_contains "${tmp_root}/refuse_public/gh.log" "release create"
assert_not_contains "${tmp_root}/refuse_public/gh.log" "release edit"
assert_not_contains "${tmp_root}/refuse_public/gh.log" "release upload"

run_case "refuse_duplicate_drafts" '[[{"id":3,"tag_name":"v0.7.0","draft":true},{"id":4,"tag_name":"v0.7.0","draft":true}]]' 1
assert_contains "${tmp_root}/refuse_duplicate_drafts/stderr.txt" "found 2 draft releases for v0.7.0; refusing to choose one"
assert_not_contains "${tmp_root}/refuse_duplicate_drafts/gh.log" "release create"
assert_not_contains "${tmp_root}/refuse_duplicate_drafts/gh.log" "release edit"
assert_not_contains "${tmp_root}/refuse_duplicate_drafts/gh.log" "release upload"

# Regression guard: an old releases/tags/:tag lookup would see this 404 stdout
# body and misclassify it as an existing public release. This script lists all
# releases instead, so a new tag still takes the create path.
legacy_dir="${tmp_root}/legacy_tag_404_mock"
mkdir -p "${legacy_dir}"
: >"${legacy_dir}/gh.log"
(
  export GH_REPO="owner/repo"
  export TAG_NAME="v0.7.0"
  export GH_STUB_LOG="${legacy_dir}/gh.log"

  set +e
  "${stub_dir}/gh" api "repos/${GH_REPO}/releases/tags/${TAG_NAME}" >"${legacy_dir}/stdout.txt" 2>"${legacy_dir}/stderr.txt"
  status="$?"
  set -e

  if [[ "${status}" -ne 1 ]]; then
    echo "legacy tag lookup mock: expected exit 1, got ${status}" >&2
    exit 1
  fi
)
assert_contains "${legacy_dir}/stdout.txt" '{"message":"Not Found"'

run_case "tag_404_body_regression" '[[]]' 0
assert_contains "${tmp_root}/tag_404_body_regression/gh.log" "release create v0.7.0"
assert_not_contains "${tmp_root}/tag_404_body_regression/gh.log" "api repos/owner/repo/releases/tags/v0.7.0"

echo "stage-draft-release tests passed"
