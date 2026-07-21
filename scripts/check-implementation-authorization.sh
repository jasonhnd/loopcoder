#!/usr/bin/env bash
# Reject ordinary-development implementation PRs that close status:planned
# issues without explicit implementation authorization (v0.9.0 stabilization).
#
# Usage (CI on pull_request):
#   bash scripts/check-implementation-authorization.sh
#
# Optional env:
#   GITHUB_EVENT_PATH  — Actions event payload (preferred)
#   PR_NUMBER          — override PR number
#   REPO               — owner/name (default from gh)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

if [[ "${SKIP_IMPLEMENTATION_AUTH_CHECK:-}" == "1" ]]; then
  echo "implementation-authorization: skipped (SKIP_IMPLEMENTATION_AUTH_CHECK=1)"
  exit 0
fi

# Offline unit-test mode: pure Go fixtures only (no network).
if [[ "${IMPLEMENTATION_AUTH_OFFLINE:-}" == "1" ]]; then
  go test ./internal/evidence -count=1 -timeout=30s -run 'TestRejectStatusPlanned|TestAllowWhenExplicit|TestIsImplementation|TestParseClosing|TestUnlinked|TestNonImplementation|TestReady'
  echo "implementation-authorization: offline fixture tests ok"
  exit 0
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "implementation-authorization: gh not available; running offline fixtures only" >&2
  IMPLEMENTATION_AUTH_OFFLINE=1 exec bash "$0"
fi

REPO="${REPO:-}"
if [[ -z "${REPO}" ]]; then
  REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || true)"
fi
if [[ -z "${REPO}" ]]; then
  echo "implementation-authorization: unable to resolve REPO" >&2
  exit 1
fi

PR_NUMBER="${PR_NUMBER:-}"
if [[ -z "${PR_NUMBER}" && -n "${GITHUB_EVENT_PATH:-}" && -f "${GITHUB_EVENT_PATH}" ]]; then
  PR_NUMBER="$(python3 -c 'import json,sys; e=json.load(open(sys.argv[1])); print(e.get("pull_request",{}).get("number") or "")' "${GITHUB_EVENT_PATH}" 2>/dev/null || true)"
fi
if [[ -z "${PR_NUMBER}" && -n "${GITHUB_REF:-}" ]]; then
  # refs/pull/123/merge
  if [[ "${GITHUB_REF}" =~ refs/pull/([0-9]+)/ ]]; then
    PR_NUMBER="${BASH_REMATCH[1]}"
  fi
fi
if [[ -z "${PR_NUMBER}" ]]; then
  echo "implementation-authorization: PR_NUMBER not set; running offline fixtures" >&2
  IMPLEMENTATION_AUTH_OFFLINE=1 exec bash "$0"
fi

echo "implementation-authorization: evaluating PR #${PR_NUMBER} on ${REPO}"

BASE_REF="$(gh pr view "${PR_NUMBER}" --repo "${REPO}" --json baseRefName -q .baseRefName)"
BODY="$(gh pr view "${PR_NUMBER}" --repo "${REPO}" --json body -q .body)"
# Changed files vs base
FILES="$(gh pr view "${PR_NUMBER}" --repo "${REPO}" --json files -q '.files[].path' | tr '\n' ' ')"

# Pure classification via a tiny Go driver
TMPDIR_AUTH="$(mktemp -d)"
trap 'rm -rf "${TMPDIR_AUTH}"' EXIT
printf '%s\n' ${FILES} >"${TMPDIR_AUTH}/files.txt"
printf '%s' "${BODY}" >"${TMPDIR_AUTH}/body.txt"

# Collect linked issues from closing keywords
ISSUES_JSON='[]'
while read -r num; do
  [[ -z "${num}" ]] && continue
  issue_json="$(gh issue view "${num}" --repo "${REPO}" --json number,title,body,labels 2>/dev/null || echo '')"
  if [[ -z "${issue_json}" ]]; then
    echo "implementation-authorization: WARNING unable to load issue #${num}" >&2
    continue
  fi
  ISSUES_JSON="$(python3 -c '
import json,sys
acc=json.loads(sys.argv[1])
iss=json.loads(sys.argv[2])
labels=[l["name"] for l in iss.get("labels") or []]
acc.append({"number": iss["number"], "title": iss.get("title") or "", "body": iss.get("body") or "", "labels": labels})
print(json.dumps(acc))
' "${ISSUES_JSON}" "${issue_json}")"
done < <(python3 -c '
import re,sys
body=open(sys.argv[1]).read()
print("\n".join(sorted(set(re.findall(r"(?i)(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s*#(\d+)", body)), key=int)))
' "${TMPDIR_AUTH}/body.txt")

export IMPLEMENTATION_AUTH_FILES="${TMPDIR_AUTH}/files.txt"
export IMPLEMENTATION_AUTH_ISSUES="${TMPDIR_AUTH}/issues.json"
printf '%s' "${ISSUES_JSON}" >"${IMPLEMENTATION_AUTH_ISSUES}"

# Evaluate with go test helper / small program
go run ./scripts/cmd/checkimplauth/ \
  -files "${TMPDIR_AUTH}/files.txt" \
  -issues "${TMPDIR_AUTH}/issues.json" \
  -body "${TMPDIR_AUTH}/body.txt"

echo "implementation-authorization: ok base=${BASE_REF} pr=${PR_NUMBER}"
