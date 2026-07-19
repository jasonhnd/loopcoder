#!/usr/bin/env bash
set -euo pipefail

timeout="${LOOPCODER_RACE_TIMEOUT:-20m}"
ordinary_parallelism="${LOOPCODER_RACE_ORDINARY_PARALLELISM:-4}"
isolated_packages=(
  "internal/storage"
  "internal/routing"
  "internal/supervisedexec"
)

inventory_file="$(mktemp)"
ordinary_file="$(mktemp)"
isolated_file="$(mktemp)"
trap 'rm -f "${inventory_file}" "${ordinary_file}" "${isolated_file}"' EXIT

if ! go list ./... >"${inventory_file}"; then
  echo "go list ./... failed; refusing to run a partial race suite." >&2
  exit 1
fi

if [[ ! -s "${inventory_file}" ]]; then
  echo "go list ./... returned no packages; refusing to run an empty race suite." >&2
  exit 1
fi

duplicate_package="$(sort "${inventory_file}" | uniq -d | head -n 1 || true)"
if [[ -n "${duplicate_package}" ]]; then
  printf 'go list ./... returned duplicate package %s; refusing ambiguous race coverage.\n' "${duplicate_package}" >&2
  exit 1
fi

: >"${ordinary_file}"
: >"${isolated_file}"
while IFS= read -r package; do
  if [[ -z "${package}" ]]; then
    echo "go list ./... returned an empty package path; refusing ambiguous race coverage." >&2
    exit 1
  fi

  isolated=false
  for isolated_package in "${isolated_packages[@]}"; do
    case "${package}" in
      */"${isolated_package}"|"${isolated_package}")
        printf '%s\n' "${package}" >>"${isolated_file}"
        isolated=true
        break
        ;;
    esac
  done
  if [[ "${isolated}" = false ]]; then
    printf '%s\n' "${package}" >>"${ordinary_file}"
  fi
done <"${inventory_file}"

for isolated_package in "${isolated_packages[@]}"; do
  count="$(grep -Ec "(^|/)${isolated_package}$" "${isolated_file}" || true)"
  if [[ "${count}" != "1" ]]; then
    printf 'missing required isolated race package %s; matched %s package(s).\n' "${isolated_package}" "${count}" >&2
    exit 1
  fi
done

ordinary_packages=()
while IFS= read -r package; do
  ordinary_packages+=("${package}")
done <"${ordinary_file}"

printf 'Full race inventory: %d ordinary package(s), %d isolated package(s).\n' "${#ordinary_packages[@]}" "${#isolated_packages[@]}"

if [[ "${#ordinary_packages[@]}" -gt 0 ]]; then
  printf 'Race ordinary packages (-p=%s, timeout=%s):\n' "${ordinary_parallelism}" "${timeout}"
  printf '  %s\n' "${ordinary_packages[@]}"
  go test -race -count=1 -timeout="${timeout}" -p="${ordinary_parallelism}" "${ordinary_packages[@]}"
else
  echo "Race ordinary packages: none."
fi

for isolated_package in "${isolated_packages[@]}"; do
  package="$(grep -E "(^|/)${isolated_package}$" "${isolated_file}")"
  printf 'Race isolated package (-p=1, timeout=%s):\n  %s\n' "${timeout}" "${package}"
  go test -race -count=1 -timeout="${timeout}" -p=1 "${package}"
done
