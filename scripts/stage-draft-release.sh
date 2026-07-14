#!/usr/bin/env bash
set -euo pipefail

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "${name} is required" >&2
    exit 1
  fi
}

plain_release_version() {
  require_env TAG_NAME
  local version="${TAG_NAME#v}"
  if [[ -z "${version}" || "${version}" == "${TAG_NAME}" && "${TAG_NAME}" == v ]]; then
    echo "TAG_NAME resolved to an empty release version" >&2
    exit 1
  fi
  printf '%s\n' "${version}"
}

expected_release_archive() {
  local version
  version="$(plain_release_version)"
  printf 'loopcoder_%s_darwin_arm64.tar.gz\n' "${version}"
}

sha256_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${path}" | awk '{print tolower($1)}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${path}" | awk '{print tolower($1)}'
    return
  fi
  echo "sha256sum or shasum is required to validate release checksums" >&2
  exit 1
}

validate_checksum_inventory() {
  local expected_archive="$1"
  local expected_hash
  expected_hash="$(sha256_file "dist/${expected_archive}")"

  python3 -c '
import sys

expected_archive = sys.argv[1]
expected_hash = sys.argv[2]
matches = 0
archive_refs = []

for raw in sys.stdin:
    line = raw.strip()
    if not line:
        continue
    parts = line.split()
    if len(parts) < 2:
        print(f"malformed SHA256SUMS line: {line}", file=sys.stderr)
        sys.exit(1)
    digest = parts[0].lower()
    name = parts[1].lstrip("*")
    if name.startswith("loopcoder_") or name.endswith((".tar.gz", ".zip")):
        archive_refs.append(name)
        if name != expected_archive:
            print(f"SHA256SUMS references unsupported release archive {name}", file=sys.stderr)
            sys.exit(1)
        if digest != expected_hash:
            print(f"SHA256SUMS digest for {name} does not match staged archive", file=sys.stderr)
            sys.exit(1)
        matches += 1

if matches != 1:
    print(f"SHA256SUMS must contain exactly one entry for {expected_archive}; found {matches}", file=sys.stderr)
    sys.exit(1)
if len(archive_refs) != 1:
    print(f"SHA256SUMS must reference exactly one release archive; found {len(archive_refs)}", file=sys.stderr)
    sys.exit(1)
' "${expected_archive}" "${expected_hash}" < dist/SHA256SUMS
}

validate_dist_release_assets() {
  local mode="${1:-candidate}"
  local expected_archive
  expected_archive="$(expected_release_archive)"

  if [[ "${mode}" != "archives" && "${mode}" != "candidate" ]]; then
    echo "unknown release asset validation mode: ${mode}" >&2
    exit 2
  fi
  if [[ ! -d dist ]]; then
    echo "dist directory is required" >&2
    exit 1
  fi

  local required=("dist/${expected_archive}")
  if [[ "${mode}" == "candidate" ]]; then
    required+=("dist/SHA256SUMS" "dist/SHA256SUMS.sigstore")
  fi

  local required_path
  for required_path in "${required[@]}"; do
    if [[ ! -e "${required_path}" ]]; then
      echo "missing required release asset ${required_path#dist/}" >&2
      exit 1
    fi
    if [[ -L "${required_path}" ]]; then
      echo "release asset ${required_path#dist/} must not be a symlink" >&2
      exit 1
    fi
    if [[ ! -f "${required_path}" ]]; then
      echo "release asset ${required_path#dist/} must be a regular file" >&2
      exit 1
    fi
  done

  local entry base
  shopt -s nullglob dotglob
  for entry in dist/*; do
    base="${entry##*/}"
    if [[ -L "${entry}" ]]; then
      echo "release asset ${base} must not be a symlink" >&2
      exit 1
    fi
    if [[ ! -f "${entry}" ]]; then
      echo "release inventory contains non-file entry ${base}" >&2
      exit 1
    fi
    case "${base}" in
      "${expected_archive}")
        ;;
      SHA256SUMS|SHA256SUMS.sigstore)
        if [[ "${mode}" != "candidate" ]]; then
          echo "archive-only release inventory contains integrity asset ${base}" >&2
          exit 1
        fi
        ;;
      loopcoder_*|*.tar.gz|*.zip)
        echo "unsupported release archive ${base}; expected exactly ${expected_archive}" >&2
        exit 1
        ;;
      *)
        echo "unexpected release asset ${base}" >&2
        exit 1
        ;;
    esac
  done
  shopt -u nullglob dotglob

  if [[ "${mode}" == "candidate" ]]; then
    validate_checksum_inventory "${expected_archive}"
  fi
}

write_release_notes() {
  local notes_file=".github/release-notes/${TAG_NAME}.md"
  if [[ -f "${notes_file}" ]]; then
    cp "${notes_file}" release-notes.md
    return
  fi

  local identity="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/.github/workflows/release.yml@refs/tags/${TAG_NAME}"
  local issuer="https://token.actions.githubusercontent.com"
  printf 'Automated release for %s. SHA256SUMS is signed with cosign keyless identity %s and OIDC issuer %s.\n' "${TAG_NAME}" "${identity}" "${issuer}" > release-notes.md
}

matching_releases_for_tag() {
  local releases_json
  if ! releases_json="$(gh api "repos/${GH_REPO}/releases" --paginate --slurp)"; then
    echo "failed to list releases while looking up ${TAG_NAME}" >&2
    exit 1
  fi

  # Supported release and local verification environments provide python3.
  TAG_NAME="${TAG_NAME}" python3 -c '
import json
import os
import sys

tag = os.environ["TAG_NAME"]
pages = json.load(sys.stdin)
matches = []
for page in pages:
    if not isinstance(page, list):
        print("release lookup returned a non-list page", file=sys.stderr)
        sys.exit(1)
    for release in page:
        if isinstance(release, dict) and release.get("tag_name") == tag:
            matches.append(release)

print(json.dumps(matches, separators=(",", ":")))
' <<<"${releases_json}"
}

resolve_release_json_for_tag() {
  require_env GH_REPO
  require_env TAG_NAME

  local matches_json
  matches_json="$(matching_releases_for_tag)"

  TAG_NAME="${TAG_NAME}" python3 -c '
import json
import os
import sys

tag = os.environ["TAG_NAME"]
matches = json.load(sys.stdin)
if len(matches) == 0:
    print(f"release {tag} was not found", file=sys.stderr)
    sys.exit(1)
if len(matches) > 1:
    print(f"found {len(matches)} releases for {tag}; refusing to choose one; delete duplicate releases manually and rerun", file=sys.stderr)
    sys.exit(1)

release = matches[0]
if not isinstance(release, dict):
    print(f"release lookup for {tag} returned a non-object record", file=sys.stderr)
    sys.exit(1)
if "id" not in release:
    print(f"release lookup for {tag} returned a record without an id field", file=sys.stderr)
    sys.exit(1)

print(json.dumps(release, separators=(",", ":")))
' <<<"${matches_json}"
}

resolve_release_id_for_tag() {
  resolve_release_json_for_tag | python3 -c '
import json
import sys

release = json.load(sys.stdin)
print(release["id"])
'
}

stage_draft_release() {
  require_env GH_REPO
  require_env TAG_NAME
  require_env GITHUB_REPOSITORY
  require_env GITHUB_SERVER_URL

  validate_dist_release_assets candidate

  local expected_archive
  expected_archive="$(expected_release_archive)"
  local assets=("dist/${expected_archive}" dist/SHA256SUMS dist/SHA256SUMS.sigstore)
  write_release_notes

  local matches_json
  matches_json="$(matching_releases_for_tag)"

  local action
  action="$(TAG_NAME="${TAG_NAME}" python3 -c '
import json
import os
import sys

tag = os.environ["TAG_NAME"]
matches = json.load(sys.stdin)
invalid = [release for release in matches if not isinstance(release.get("draft"), bool)]
if invalid:
    print(f"release lookup for {tag} returned records without a boolean draft field", file=sys.stderr)
    sys.exit(1)

public = [release for release in matches if release["draft"] is False]
if public:
    print(f"release {tag} already exists and is public; refusing to overwrite final release", file=sys.stderr)
    sys.exit(1)

drafts = [release for release in matches if release["draft"] is True]
if len(drafts) > 1:
    print(f"found {len(drafts)} draft releases for {tag}; refusing to choose one; delete duplicate drafts manually and rerun", file=sys.stderr)
    sys.exit(1)

print("update" if drafts else "create")
' <<<"${matches_json}")"

  if [[ "${action}" == "update" ]]; then
    gh release edit "${TAG_NAME}" --repo "${GH_REPO}" --prerelease --notes-file release-notes.md
    gh release upload "${TAG_NAME}" --repo "${GH_REPO}" "${assets[@]}" --clobber
    return
  fi

  if [[ "${action}" != "create" ]]; then
    echo "unexpected release action: ${action}" >&2
    exit 1
  fi

  gh release create "${TAG_NAME}" "${assets[@]}" \
    --repo "${GH_REPO}" \
    --verify-tag \
    --draft \
    --prerelease \
    --title "${TAG_NAME}" \
    --notes-file release-notes.md
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  mode="${1:-stage}"
  case "${mode}" in
    stage)
      shift || true
      stage_draft_release "$@"
      ;;
    resolve-json)
      shift || true
      resolve_release_json_for_tag "$@"
      ;;
    resolve-id)
      shift || true
      resolve_release_id_for_tag "$@"
      ;;
    validate-archives)
      shift || true
      validate_dist_release_assets archives "$@"
      ;;
    validate-candidate)
      shift || true
      validate_dist_release_assets candidate "$@"
      ;;
    *)
      echo "usage: $0 [stage|resolve-json|resolve-id|validate-archives|validate-candidate]" >&2
      exit 2
      ;;
  esac
fi
