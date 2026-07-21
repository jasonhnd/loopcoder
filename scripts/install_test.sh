#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
installer="${repo_root}/scripts/install.sh"

first_line="LoopCoder v0.8.0 supports macOS Apple Silicon only (darwin/arm64)."
guidance="LoopCoder v0.7.0 is the final legacy multi-platform release for Windows, Linux, WSL, containers, and Intel macOS."

tmp_root="$(mktemp -d)"
trap 'rm -rf "${tmp_root}"' EXIT

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

assert_empty_file() {
  local file="$1"
  if [[ -s "${file}" ]]; then
    echo "expected ${file} to be empty; actual:" >&2
    cat "${file}" >&2
    exit 1
  fi
}

write_fail_spies() {
  local bin_dir="$1"
  mkdir -p "${bin_dir}"
  for cmd in curl mktemp mkdir touch cp chmod mv rm tar awk sed cosign sha256sum shasum grep; do
    cat >"${bin_dir}/${cmd}" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s %s\n' "$(basename "$0")" "$*" >>"${INSTALL_SPY_LOG}"
touch "${INSTALL_SPY_SENTINEL}"
exit 97
STUB
    chmod +x "${bin_dir}/${cmd}"
  done
}

run_unsupported_case() {
  local name="$1"
  local goos="$2"
  local goarch="$3"
  local case_dir="${tmp_root}/${name}"
  local spy_dir="${case_dir}/bin"
  mkdir -p "${case_dir}/tmp"
  : >"${case_dir}/spy.log"
  write_fail_spies "${spy_dir}"

  set +e
  (
    export LOOPCODER_INSTALL_OS="${goos}"
    export LOOPCODER_INSTALL_ARCH="${goarch}"
    export LOOPCODER_INSTALL_DIR="${case_dir}/install"
    export HOME="${case_dir}/home"
    export TMPDIR="${case_dir}/tmp"
    export PATH="${spy_dir}:/usr/bin:/bin"
    export SHELL="/bin/sh"
    export INSTALL_SPY_LOG="${case_dir}/spy.log"
    export INSTALL_SPY_SENTINEL="${case_dir}/side-effect"
    /bin/sh "${installer}"
  ) >"${case_dir}/stdout.txt" 2>"${case_dir}/stderr.txt"
  local status="$?"
  set -e

  if [[ "${status}" -ne 78 ]]; then
    echo "${name}: expected exit 78, got ${status}" >&2
    cat "${case_dir}/stdout.txt" >&2
    cat "${case_dir}/stderr.txt" >&2
    exit 1
  fi

  local actual_first_line
  actual_first_line="$(sed -n '1p' "${case_dir}/stderr.txt")"
  if [[ "${actual_first_line}" != "${first_line}" ]]; then
    echo "${name}: first diagnostic line = ${actual_first_line}; want ${first_line}" >&2
    exit 1
  fi
  assert_contains "${case_dir}/stderr.txt" "Actual platform: ${goos}/${goarch}."
  assert_contains "${case_dir}/stderr.txt" "Supported platform: darwin/arm64."
  assert_contains "${case_dir}/stderr.txt" "${guidance}"
  assert_empty_file "${case_dir}/stdout.txt"
  assert_empty_file "${case_dir}/spy.log"

  if [[ -e "${case_dir}/side-effect" || -e "${case_dir}/install" || -e "${case_dir}/home" ]]; then
    echo "${name}: unsupported gate performed a filesystem side effect" >&2
    find "${case_dir}" -maxdepth 2 -print >&2
    exit 1
  fi
  if find "${case_dir}/tmp" -mindepth 1 -print -quit | grep -q .; then
    echo "${name}: unsupported gate created temporary files" >&2
    find "${case_dir}/tmp" -mindepth 1 -print >&2
    exit 1
  fi
}

run_unsupported_no_home_case() {
  local name="$1"
  local goos="$2"
  local goarch="$3"
  local case_dir="${tmp_root}/${name}"
  local spy_dir="${case_dir}/bin"
  mkdir -p "${case_dir}/tmp"
  : >"${case_dir}/spy.log"
  write_fail_spies "${spy_dir}"

  set +e
  env -u HOME \
    LOOPCODER_INSTALL_OS="${goos}" \
    LOOPCODER_INSTALL_ARCH="${goarch}" \
    LOOPCODER_INSTALL_DIR="${case_dir}/install" \
    TMPDIR="${case_dir}/tmp" \
    PATH="${spy_dir}:/usr/bin:/bin" \
    SHELL="/bin/sh" \
    INSTALL_SPY_LOG="${case_dir}/spy.log" \
    INSTALL_SPY_SENTINEL="${case_dir}/side-effect" \
    /bin/sh "${installer}" >"${case_dir}/stdout.txt" 2>"${case_dir}/stderr.txt"
  local status="$?"
  set -e

  if [[ "${status}" -ne 78 ]]; then
    echo "${name}: expected exit 78, got ${status}" >&2
    cat "${case_dir}/stdout.txt" >&2
    cat "${case_dir}/stderr.txt" >&2
    exit 1
  fi

  local actual_first_line
  actual_first_line="$(sed -n '1p' "${case_dir}/stderr.txt")"
  if [[ "${actual_first_line}" != "${first_line}" ]]; then
    echo "${name}: first diagnostic line = ${actual_first_line}; want ${first_line}" >&2
    exit 1
  fi
  assert_contains "${case_dir}/stderr.txt" "Actual platform: ${goos}/${goarch}."
  assert_contains "${case_dir}/stderr.txt" "Supported platform: darwin/arm64."
  assert_contains "${case_dir}/stderr.txt" "${guidance}"
  assert_empty_file "${case_dir}/stdout.txt"
  assert_empty_file "${case_dir}/spy.log"

  if [[ -e "${case_dir}/side-effect" || -e "${case_dir}/install" ]]; then
    echo "${name}: unsupported gate performed a filesystem side effect" >&2
    find "${case_dir}" -maxdepth 2 -print >&2
    exit 1
  fi
  if find "${case_dir}/tmp" -mindepth 1 -print -quit | grep -q .; then
    echo "${name}: unsupported gate created temporary files" >&2
    find "${case_dir}/tmp" -mindepth 1 -print >&2
    exit 1
  fi
}

make_archive_fixture() {
  local case_dir="$1"
  mkdir -p "${case_dir}/package"
  cat >"${case_dir}/package/loopcoder" <<'BIN'
#!/bin/sh
printf '%s\n' 'loopcoder test binary'
BIN
  chmod +x "${case_dir}/package/loopcoder"
  tar -C "${case_dir}/package" -czf "${case_dir}/loopcoder_0.8.0_darwin_arm64.tar.gz" loopcoder
  shasum -a 256 "${case_dir}/loopcoder_0.8.0_darwin_arm64.tar.gz" | awk '{print $1}' >"${case_dir}/archive.sha256"
}

write_supported_stubs() {
  local bin_dir="$1"
  mkdir -p "${bin_dir}"
  cat >"${bin_dir}/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
out=""
url=""
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    -o)
      out="$2"
      shift 2
      ;;
    -H|--header)
      shift 2
      ;;
    -*)
      shift
      ;;
    *)
      url="$1"
      shift
      ;;
  esac
done
printf '%s\n' "${url}" >>"${INSTALL_CURL_LOG}"
if [[ "${INSTALL_CURL_FAIL:-}" == "1" ]]; then
  exit 22
fi
case "${url}" in
  */releases/latest)
    printf '{"tag_name":"v0.8.0"}' >"${out}"
    ;;
  */SHA256SUMS)
    hash="${INSTALL_CHECKSUM_OVERRIDE:-$(cat "${INSTALL_ARCHIVE_SHA}")}"
    printf '%s  loopcoder_0.8.0_darwin_arm64.tar.gz\n' "${hash}" >"${out}"
    ;;
  */SHA256SUMS.sigstore)
    printf '%s\n' "sigstore bundle" >"${out}"
    ;;
  */loopcoder_0.8.0_darwin_arm64.tar.gz)
    cp "${INSTALL_ARCHIVE_FIXTURE}" "${out}"
    ;;
  *)
    echo "unexpected curl URL: ${url}" >&2
    exit 3
    ;;
esac
STUB
  chmod +x "${bin_dir}/curl"

  cat >"${bin_dir}/cosign" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${INSTALL_COSIGN_LOG}"
if [[ "${INSTALL_COSIGN_FAIL:-}" == "1" ]]; then
  exit 1
fi
STUB
  chmod +x "${bin_dir}/cosign"
}

run_supported_case() {
  local name="$1"
  local expected_status="$2"
  local case_dir="${tmp_root}/${name}"
  local bin_dir="${case_dir}/bin"
  local install_dir="${case_dir}/install"
  mkdir -p "${case_dir}/tmp"
  make_archive_fixture "${case_dir}"
  write_supported_stubs "${bin_dir}"
  : >"${case_dir}/curl.log"
  : >"${case_dir}/cosign.log"

  set +e
  (
    export LOOPCODER_INSTALL_OS="darwin"
    export LOOPCODER_INSTALL_ARCH="arm64"
    export LOOPCODER_VERSION="0.8.0"
    export LOOPCODER_INSTALL_DIR="${install_dir}"
    export HOME="${case_dir}/home"
    export TMPDIR="${case_dir}/tmp"
    export PATH="${bin_dir}:${install_dir}:${PATH}"
    export SHELL="/bin/sh"
    export INSTALL_ARCHIVE_FIXTURE="${case_dir}/loopcoder_0.8.0_darwin_arm64.tar.gz"
    export INSTALL_ARCHIVE_SHA="${case_dir}/archive.sha256"
    export INSTALL_CURL_LOG="${case_dir}/curl.log"
    export INSTALL_COSIGN_LOG="${case_dir}/cosign.log"
    /bin/sh "${installer}"
  ) >"${case_dir}/stdout.txt" 2>"${case_dir}/stderr.txt"
  local status="$?"
  set -e

  if [[ "${status}" -ne "${expected_status}" ]]; then
    echo "${name}: expected exit ${expected_status}, got ${status}" >&2
    cat "${case_dir}/stdout.txt" >&2
    cat "${case_dir}/stderr.txt" >&2
    exit 1
  fi
}

# Install with install_dir intentionally off PATH so ensure_path mutates profiles.
# Args: name shell_path install_dir_rel [extra env assignments...]
run_path_case() {
  local name="$1"
  local shell_path="$2"
  local install_rel="$3"
  shift 3
  local case_dir="${tmp_root}/${name}"
  local bin_dir="${case_dir}/bin"
  local home_dir="${case_dir}/home"
  local install_dir="${case_dir}/${install_rel}"
  mkdir -p "${case_dir}/tmp" "${home_dir}"
  make_archive_fixture "${case_dir}"
  write_supported_stubs "${bin_dir}"
  : >"${case_dir}/curl.log"
  : >"${case_dir}/cosign.log"

  # Pre-seed profile once (do not clobber on re-runs — keeps idempotency tests honest).
  local seed_marker="${case_dir}/profile.seed"
  if [[ -f "${seed_marker}" ]]; then
    local shell_name profile_path
    shell_name="$(basename "${shell_path}")"
    case "${shell_name}" in
      zsh) profile_path="${home_dir}/.zshrc" ;;
      bash) profile_path="${home_dir}/.bashrc" ;;
      *) profile_path="${home_dir}/.profile" ;;
    esac
    if [[ ! -f "${profile_path}" ]]; then
      cp "${seed_marker}" "${profile_path}"
    fi
  fi

  set +e
  (
    export LOOPCODER_INSTALL_OS="darwin"
    export LOOPCODER_INSTALL_ARCH="arm64"
    export LOOPCODER_VERSION="0.8.0"
    export LOOPCODER_INSTALL_DIR="${install_dir}"
    export HOME="${home_dir}"
    export TMPDIR="${case_dir}/tmp"
    # Keep stubs and host tools, but do NOT put install_dir on PATH.
    export PATH="${bin_dir}:/usr/bin:/bin"
    export SHELL="${shell_path}"
    export INSTALL_ARCHIVE_FIXTURE="${case_dir}/loopcoder_0.8.0_darwin_arm64.tar.gz"
    export INSTALL_ARCHIVE_SHA="${case_dir}/archive.sha256"
    export INSTALL_CURL_LOG="${case_dir}/curl.log"
    export INSTALL_COSIGN_LOG="${case_dir}/cosign.log"
    # Optional extra env (e.g. LOOPCODER_NO_MODIFY_PATH=1)
    while [[ "$#" -gt 0 ]]; do
      export "$1"
      shift
    done
    /bin/sh "${installer}"
  ) >"${case_dir}/stdout.txt" 2>"${case_dir}/stderr.txt"
  local status="$?"
  set -e

  if [[ "${status}" -ne 0 ]]; then
    echo "${name}: expected exit 0, got ${status}" >&2
    cat "${case_dir}/stdout.txt" >&2
    cat "${case_dir}/stderr.txt" >&2
    exit 1
  fi
  test -x "${install_dir}/loopcoder" || {
    echo "${name}: binary missing at ${install_dir}/loopcoder" >&2
    exit 1
  }
  # Expose resolved paths for callers.
  printf '%s\n' "${install_dir}" >"${case_dir}/resolved_install_dir"
}

if [[ "${LOOPCODER_INSTALL_TEST_ONLY:-all}" != "interruption" ]]; then
run_unsupported_case "darwin_amd64" "darwin" "amd64"
run_unsupported_case "linux_amd64" "linux" "amd64"
run_unsupported_case "linux_arm64" "linux" "arm64"
run_unsupported_case "windows_like" "windows" "amd64"
run_unsupported_case "unknown_tuple" "plan9" "riscv64"
run_unsupported_no_home_case "unsupported_without_home" "linux" "amd64"

run_supported_case "darwin_arm64_happy" 0
assert_contains "${tmp_root}/darwin_arm64_happy/stdout.txt" "Installing loopcoder 0.8.0 for darwin/arm64"
assert_contains "${tmp_root}/darwin_arm64_happy/stdout.txt" "Installed loopcoder 0.8.0"
assert_contains "${tmp_root}/darwin_arm64_happy/curl.log" "/loopcoder_0.8.0_darwin_arm64.tar.gz"
assert_not_contains "${tmp_root}/darwin_arm64_happy/curl.log" "linux"
assert_not_contains "${tmp_root}/darwin_arm64_happy/curl.log" "windows"
assert_not_contains "${tmp_root}/darwin_arm64_happy/curl.log" "darwin_amd64"
test -x "${tmp_root}/darwin_arm64_happy/install/loopcoder"

run_supported_case "idempotent_reinstall" 0
run_supported_case "idempotent_reinstall" 0
test -x "${tmp_root}/idempotent_reinstall/install/loopcoder"

(
  export INSTALL_CURL_FAIL=1
  run_supported_case "download_failure" 1
)
assert_contains "${tmp_root}/download_failure/stderr.txt" "failed to download SHA256SUMS"
if [[ -e "${tmp_root}/download_failure/install/loopcoder" ]]; then
  echo "download failure installed a binary" >&2
  exit 1
fi

(
  export INSTALL_CHECKSUM_OVERRIDE="0000000000000000000000000000000000000000000000000000000000000000"
  run_supported_case "checksum_failure" 1
)
assert_contains "${tmp_root}/checksum_failure/stderr.txt" "checksum mismatch for loopcoder_0.8.0_darwin_arm64.tar.gz"
if [[ -e "${tmp_root}/checksum_failure/install/loopcoder" ]]; then
  echo "checksum failure installed a binary" >&2
  exit 1
fi

(
  export INSTALL_COSIGN_FAIL=1
  run_supported_case "signature_failure" 1
)
assert_contains "${tmp_root}/signature_failure/stderr.txt" "failed to verify SHA256SUMS signature"
assert_not_contains "${tmp_root}/signature_failure/curl.log" "/loopcoder_0.8.0_darwin_arm64.tar.gz"
if [[ -e "${tmp_root}/signature_failure/install/loopcoder" ]]; then
  echo "signature failure installed a binary" >&2
  exit 1
fi

missing_dir="${tmp_root}/missing_tools"
mkdir -p "${missing_dir}/bin" "${missing_dir}/tmp"
for cmd in uname curl tar awk sed sha256sum; do
  cat >"${missing_dir}/bin/${cmd}" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
  chmod +x "${missing_dir}/bin/${cmd}"
done
set +e
(
  export LOOPCODER_INSTALL_OS="darwin"
  export LOOPCODER_INSTALL_ARCH="arm64"
  export LOOPCODER_VERSION="0.8.0"
  export LOOPCODER_INSTALL_DIR="${missing_dir}/install"
  export HOME="${missing_dir}/home"
  export TMPDIR="${missing_dir}/tmp"
  export PATH="${missing_dir}/bin"
  /bin/sh "${installer}"
) >"${missing_dir}/stdout.txt" 2>"${missing_dir}/stderr.txt"
missing_status="$?"
set -e
if [[ "${missing_status}" -ne 1 ]]; then
  echo "missing_tools: expected exit 1, got ${missing_status}" >&2
  exit 1
fi
assert_contains "${missing_dir}/stderr.txt" "cosign is required"
if [[ -e "${missing_dir}/install" ]] || find "${missing_dir}/tmp" -mindepth 1 -print -quit | grep -q .; then
  echo "missing tools case performed install/temp filesystem side effects" >&2
  exit 1
fi

# --- PATH / profile coverage for custom and default install directories ---

# Custom absolute dir with zsh: writes that exact dir into ~/.zshrc.
run_path_case "path_custom_zsh" "/bin/zsh" "opt/loopcoder/bin"
custom_install="$(cat "${tmp_root}/path_custom_zsh/resolved_install_dir")"
assert_contains "${tmp_root}/path_custom_zsh/home/.zshrc" "export PATH='${custom_install}':\$PATH"
assert_contains "${tmp_root}/path_custom_zsh/stdout.txt" "Added ${custom_install} to PATH"
assert_not_contains "${tmp_root}/path_custom_zsh/home/.zshrc" '$HOME/.loopcoder/bin'
assert_not_contains "${tmp_root}/path_custom_zsh/stdout.txt" 'export PATH="$HOME/.loopcoder/bin'

# Default install dir form: unset LOOPCODER_INSTALL_DIR via empty install using HOME default.
# Reuse path harness with install under home/.loopcoder/bin by not setting INSTALL_DIR —
# run a dedicated case.
default_case="${tmp_root}/path_default_zsh"
mkdir -p "${default_case}/tmp" "${default_case}/home" "${default_case}/bin"
make_archive_fixture "${default_case}"
write_supported_stubs "${default_case}/bin"
: >"${default_case}/curl.log"
: >"${default_case}/cosign.log"
set +e
(
  export LOOPCODER_INSTALL_OS="darwin"
  export LOOPCODER_INSTALL_ARCH="arm64"
  export LOOPCODER_VERSION="0.8.0"
  unset LOOPCODER_INSTALL_DIR || true
  export HOME="${default_case}/home"
  export TMPDIR="${default_case}/tmp"
  export PATH="${default_case}/bin:/usr/bin:/bin"
  export SHELL="/bin/zsh"
  export INSTALL_ARCHIVE_FIXTURE="${default_case}/loopcoder_0.8.0_darwin_arm64.tar.gz"
  export INSTALL_ARCHIVE_SHA="${default_case}/archive.sha256"
  export INSTALL_CURL_LOG="${default_case}/curl.log"
  export INSTALL_COSIGN_LOG="${default_case}/cosign.log"
  /bin/sh "${installer}"
) >"${default_case}/stdout.txt" 2>"${default_case}/stderr.txt"
default_status="$?"
set -e
if [[ "${default_status}" -ne 0 ]]; then
  echo "path_default_zsh: expected exit 0, got ${default_status}" >&2
  cat "${default_case}/stdout.txt" >&2
  cat "${default_case}/stderr.txt" >&2
  exit 1
fi
default_bin="${default_case}/home/.loopcoder/bin"
test -x "${default_bin}/loopcoder"
assert_contains "${default_case}/home/.zshrc" "export PATH='${default_bin}':\$PATH"

# Paths containing spaces.
run_path_case "path_spaces_bash" "/bin/bash" "My Tools/loopcoder bin"
spaces_install="$(cat "${tmp_root}/path_spaces_bash/resolved_install_dir")"
assert_contains "${tmp_root}/path_spaces_bash/home/.bashrc" "export PATH='${spaces_install}':\$PATH"
test -x "${spaces_install}/loopcoder"

# Idempotent re-run does not duplicate profile lines; preserves unrelated text.
seed_profile="${tmp_root}/path_idempotent_zsh"
mkdir -p "${seed_profile}"
printf '%s\n' '# user marker keep-me' 'export FOO=bar' >"${seed_profile}/profile.seed"
run_path_case "path_idempotent_zsh" "/bin/zsh" "custom/bin"
run_path_case "path_idempotent_zsh" "/bin/zsh" "custom/bin"
idem_install="$(cat "${tmp_root}/path_idempotent_zsh/resolved_install_dir")"
idem_profile="${tmp_root}/path_idempotent_zsh/home/.zshrc"
assert_contains "${idem_profile}" "# user marker keep-me"
assert_contains "${idem_profile}" "export FOO=bar"
export_count="$(grep -cF "export PATH='${idem_install}':\$PATH" "${idem_profile}" || true)"
if [[ "${export_count}" -ne 1 ]]; then
  echo "path_idempotent_zsh: expected exactly one PATH export, got ${export_count}" >&2
  cat "${idem_profile}" >&2
  exit 1
fi
assert_contains "${tmp_root}/path_idempotent_zsh/stdout.txt" "A loopcoder PATH entry already exists"

# Already on PATH: no profile mutation.
already_case="${tmp_root}/path_already_on_path"
mkdir -p "${already_case}/tmp" "${already_case}/home" "${already_case}/bin" "${already_case}/install"
printf '%s\n' 'preexisting' >"${already_case}/home/.profile"
make_archive_fixture "${already_case}"
write_supported_stubs "${already_case}/bin"
: >"${already_case}/curl.log"
: >"${already_case}/cosign.log"
set +e
(
  export LOOPCODER_INSTALL_OS="darwin"
  export LOOPCODER_INSTALL_ARCH="arm64"
  export LOOPCODER_VERSION="0.8.0"
  export LOOPCODER_INSTALL_DIR="${already_case}/install"
  export HOME="${already_case}/home"
  export TMPDIR="${already_case}/tmp"
  export PATH="${already_case}/bin:${already_case}/install:/usr/bin:/bin"
  export SHELL="/bin/sh"
  export INSTALL_ARCHIVE_FIXTURE="${already_case}/loopcoder_0.8.0_darwin_arm64.tar.gz"
  export INSTALL_ARCHIVE_SHA="${already_case}/archive.sha256"
  export INSTALL_CURL_LOG="${already_case}/curl.log"
  export INSTALL_COSIGN_LOG="${already_case}/cosign.log"
  /bin/sh "${installer}"
) >"${already_case}/stdout.txt" 2>"${already_case}/stderr.txt"
already_status="$?"
set -e
if [[ "${already_status}" -ne 0 ]]; then
  echo "path_already_on_path: expected exit 0, got ${already_status}" >&2
  exit 1
fi
assert_contains "${already_case}/stdout.txt" "is already on PATH"
if ! grep -qx 'preexisting' "${already_case}/home/.profile"; then
  echo "path_already_on_path: profile was mutated" >&2
  cat "${already_case}/home/.profile" >&2
  exit 1
fi

# LOOPCODER_NO_MODIFY_PATH=1 prints instructions and leaves profiles untouched.
run_path_case "path_no_modify" "/bin/zsh" "nomod/bin" "LOOPCODER_NO_MODIFY_PATH=1"
nomod_install="$(cat "${tmp_root}/path_no_modify/resolved_install_dir")"
assert_contains "${tmp_root}/path_no_modify/stdout.txt" "${nomod_install} is not on PATH"
assert_contains "${tmp_root}/path_no_modify/stdout.txt" "export PATH='${nomod_install}':\$PATH"
if [[ -e "${tmp_root}/path_no_modify/home/.zshrc" ]]; then
  echo "path_no_modify: unexpected profile write" >&2
  cat "${tmp_root}/path_no_modify/home/.zshrc" >&2
  exit 1
fi

# Unsupported shell: print manual instructions for the resolved BIN_DIR.
run_path_case "path_unsupported_shell" "/usr/local/bin/fish" "fish/bin"
fish_install="$(cat "${tmp_root}/path_unsupported_shell/resolved_install_dir")"
assert_contains "${tmp_root}/path_unsupported_shell/stdout.txt" "export PATH='${fish_install}':\$PATH"
assert_not_contains "${tmp_root}/path_unsupported_shell/stdout.txt" '$HOME/.loopcoder/bin'
if [[ -e "${tmp_root}/path_unsupported_shell/home/.zshrc" || -e "${tmp_root}/path_unsupported_shell/home/.profile" ]]; then
  echo "path_unsupported_shell: unexpected profile write" >&2
  exit 1
fi

# Relative LOOPCODER_INSTALL_DIR is rejected.
rel_case="${tmp_root}/path_relative_rejected"
mkdir -p "${rel_case}/tmp" "${rel_case}/home" "${rel_case}/bin"
make_archive_fixture "${rel_case}"
write_supported_stubs "${rel_case}/bin"
set +e
(
  export LOOPCODER_INSTALL_OS="darwin"
  export LOOPCODER_INSTALL_ARCH="arm64"
  export LOOPCODER_VERSION="0.8.0"
  export LOOPCODER_INSTALL_DIR="relative/bin"
  export HOME="${rel_case}/home"
  export TMPDIR="${rel_case}/tmp"
  export PATH="${rel_case}/bin:/usr/bin:/bin"
  export SHELL="/bin/sh"
  export INSTALL_ARCHIVE_FIXTURE="${rel_case}/loopcoder_0.8.0_darwin_arm64.tar.gz"
  export INSTALL_ARCHIVE_SHA="${rel_case}/archive.sha256"
  export INSTALL_CURL_LOG="${rel_case}/curl.log"
  export INSTALL_COSIGN_LOG="${rel_case}/cosign.log"
  /bin/sh "${installer}"
) >"${rel_case}/stdout.txt" 2>"${rel_case}/stderr.txt"
rel_status="$?"
set -e
if [[ "${rel_status}" -ne 1 ]]; then
  echo "path_relative_rejected: expected exit 1, got ${rel_status}" >&2
  exit 1
fi
assert_contains "${rel_case}/stderr.txt" "LOOPCODER_INSTALL_DIR must be an absolute path"

fi

run_interruption_case() {
local interrupt_dir="${tmp_root}/interruption"
mkdir -p "${interrupt_dir}/tmp"
make_archive_fixture "${interrupt_dir}"
write_supported_stubs "${interrupt_dir}/bin"
cat >"${interrupt_dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${INSTALL_CURL_LOG}"
touch "${INSTALL_CURL_READY}"
deadline=$((SECONDS + 5))
while [[ "${SECONDS}" -lt "${deadline}" ]]; do
  sleep 0.1
done
exit 143
STUB
chmod +x "${interrupt_dir}/bin/curl"
: >"${interrupt_dir}/curl.log"
rm -f "${interrupt_dir}/curl.ready"
(
  export LOOPCODER_INSTALL_OS="darwin"
  export LOOPCODER_INSTALL_ARCH="arm64"
  export LOOPCODER_VERSION="0.8.0"
  export LOOPCODER_INSTALL_DIR="${interrupt_dir}/install"
  export HOME="${interrupt_dir}/home"
  export TMPDIR="${interrupt_dir}/tmp"
  export PATH="${interrupt_dir}/bin:${interrupt_dir}/install:${PATH}"
  export SHELL="/bin/sh"
  export INSTALL_ARCHIVE_FIXTURE="${interrupt_dir}/loopcoder_0.8.0_darwin_arm64.tar.gz"
  export INSTALL_ARCHIVE_SHA="${interrupt_dir}/archive.sha256"
  export INSTALL_CURL_LOG="${interrupt_dir}/curl.log"
  export INSTALL_CURL_READY="${interrupt_dir}/curl.ready"
  export INSTALL_COSIGN_LOG="${interrupt_dir}/cosign.log"
  exec /bin/sh "${installer}"
) >"${interrupt_dir}/stdout.txt" 2>"${interrupt_dir}/stderr.txt" &
interrupt_pid="$!"
if [[ ! "${interrupt_pid}" =~ ^[0-9]+$ ]]; then
  echo "interruption: installer pid was not numeric: ${interrupt_pid}" >&2
  exit 1
fi
installer_parent="$(ps -o ppid= -p "${interrupt_pid}")"
installer_parent="${installer_parent//[[:space:]]/}"
if [[ "${installer_parent}" != "$$" ]]; then
  echo "interruption: installer pid is not a child: ${interrupt_pid}" >&2
  exit 1
fi
ready_deadline=$((SECONDS + 5))
while [[ ! -e "${interrupt_dir}/curl.ready" && "${SECONDS}" -lt "${ready_deadline}" ]]; do
  if ! kill -0 "${interrupt_pid}" 2>/dev/null; then
    break
  fi
  sleep 0.1
done
if [[ ! -e "${interrupt_dir}/curl.ready" ]]; then
  echo "interruption: installer did not reach blocking download" >&2
  kill "${interrupt_pid}" 2>/dev/null || true
  wait "${interrupt_pid}" 2>/dev/null || true
  exit 1
fi
kill -TERM "${interrupt_pid}" 2>/dev/null || true
set +e
wait "${interrupt_pid}"
interrupt_status="$?"
set -e
if [[ "${interrupt_status}" -eq 0 ]]; then
  echo "interruption: expected non-zero status" >&2
  exit 1
fi
if find "${interrupt_dir}/tmp" -mindepth 1 -name 'loopcoder-install.*' -print -quit | grep -q .; then
  echo "interruption left temporary install directories" >&2
  find "${interrupt_dir}/tmp" -mindepth 1 -print >&2
  exit 1
fi
if [[ -e "${interrupt_dir}/install/loopcoder" ]]; then
  echo "interruption installed a binary" >&2
  exit 1
fi
}

run_interruption_case

echo "install tests passed"
