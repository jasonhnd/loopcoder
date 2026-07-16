#!/usr/bin/env bash
set -euo pipefail

base_sha="${1:-}"
zero_sha="0000000000000000000000000000000000000000"

if [[ -z "${base_sha}" || "${base_sha}" = "${zero_sha}" ]] || ! git cat-file -e "${base_sha}^{commit}" 2>/dev/null; then
  if git cat-file -e 'HEAD^' 2>/dev/null; then
    base_sha="HEAD^"
  else
    echo "No usable base commit; running the full race suite."
    bash scripts/ci-full-race.sh
    exit
  fi
fi

changed_file="$(mktemp)"
package_file="$(mktemp)"
trap 'rm -f "${changed_file}" "${package_file}"' EXIT

git diff --name-only "${base_sha}...HEAD" -- '*.go' go.mod go.sum >"${changed_file}"

if grep -Eq '^(go\.mod|go\.sum)$' "${changed_file}"; then
  echo "Go module metadata changed; running the full race suite."
  bash scripts/ci-full-race.sh
  exit
fi

while IFS= read -r source_path; do
  [[ "${source_path}" = *.go ]] || continue
  package_dir="$(dirname "${source_path}")"
  if [[ "${package_dir}" = "." ]]; then
    package_arg="."
  else
    package_arg="./${package_dir}"
  fi
  [[ -d "${package_dir}" ]] || continue
  if go list "${package_arg}" >/dev/null 2>&1; then
    printf '%s\n' "${package_arg}" >>"${package_file}"
  fi
done <"${changed_file}"

if [[ ! -s "${package_file}" ]]; then
  printf '%s\n' \
    './internal/orchestrationcost' \
    './internal/supervisedexec' \
    './internal/waitstate' >"${package_file}"
fi

sort -u "${package_file}" -o "${package_file}"
packages=()
while IFS= read -r package_arg; do
  packages+=("${package_arg}")
done <"${package_file}"

printf 'Race packages (%d):\n' "${#packages[@]}"
printf '  %s\n' "${packages[@]}"
go test -race -count=1 -timeout=20m "${packages[@]}"
