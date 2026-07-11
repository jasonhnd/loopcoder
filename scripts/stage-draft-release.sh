#!/usr/bin/env bash
set -euo pipefail

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "${name} is required" >&2
    exit 1
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

stage_draft_release() {
  require_env GH_REPO
  require_env TAG_NAME
  require_env GITHUB_REPOSITORY
  require_env GITHUB_SERVER_URL

  local assets=(dist/loopcoder_* dist/SHA256SUMS dist/SHA256SUMS.sigstore)
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
  stage_draft_release "$@"
fi
