#!/usr/bin/env bash
# local-focused evidence tier for ordinary development (V090-002 / #1092).
# Budget: under 60 seconds on a reference Apple Silicon Mac.
# Never runs repository-wide unit suites or a full race suite.
# Compatible with macOS Bash 3.2 (no mapfile/readarray).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

START_EPOCH="$(date +%s)"
BUDGET_SECONDS="${LOOPCODER_PRE_PUSH_BUDGET_SECONDS:-60}"

fail() {
  printf 'pre-push-sentinel: %s\n' "$*" >&2
  exit 1
}

elapsed() {
  now="$(date +%s)"
  echo $((now - START_EPOCH))
}

assert_budget() {
  spent="$(elapsed)"
  if [ "${spent}" -gt "${BUDGET_SECONDS}" ]; then
    fail "exceeded ${BUDGET_SECONDS}s budget after ${spent}s"
  fi
}

# Identity of the commit under evaluation (local-focused tier).
if tested_sha="$(git rev-parse HEAD 2>/dev/null)"; then
  printf 'evidence_tier=local-focused\n'
  printf 'tested_commit_sha=%s\n' "${tested_sha}"
else
  fail "unable to resolve HEAD commit SHA"
fi

# Formatting: only files touched by this branch or working tree.
# Pre-existing debt on the base branch is remote CI / follow-up work, not a
# local push blocker (keeps no-op and docs-only pushes inside the budget).
base_ref="${LOOPCODER_PRE_PUSH_BASE:-}"
if [ -z "${base_ref}" ]; then
  if git rev-parse --verify origin/pre-prod >/dev/null 2>&1; then
    base_ref="origin/pre-prod"
  elif git rev-parse --verify '@{upstream}' >/dev/null 2>&1; then
    base_ref="@{upstream}"
  else
    base_ref="HEAD"
  fi
fi

go_list_file="$(mktemp)"
trap 'rm -f "${go_list_file}"' EXIT
{
  if [ "${base_ref}" != "HEAD" ]; then
    git diff --name-only --diff-filter=ACMR "${base_ref}...HEAD" -- '*.go' || true
  fi
  git diff --name-only --diff-filter=ACMR HEAD -- '*.go' || true
  git diff --name-only --cached --diff-filter=ACMR -- '*.go' || true
  git ls-files --others --exclude-standard -- '*.go' || true
} | sort -u >"${go_list_file}"

if [ -s "${go_list_file}" ]; then
  unformatted="$(tr '\n' '\0' <"${go_list_file}" | xargs -0 gofmt -l)"
  if [ -n "${unformatted}" ]; then
    printf '%s\n' "${unformatted}" >&2
    fail "gofmt reported unformatted changed Go files"
  fi
fi
assert_budget

# Conflict markers / whitespace errors on the working and index trees.
git diff --check
git diff --cached --check
assert_budget

# Generated / embedded playbook consistency plus evidence-policy sentinel tests.
# Keep the -run filter narrow so this stays inside the local budget.
go test . -count=1 -timeout=45s -run 'TestEmbeddedPlaybookFilesAreNonEmptyAndMatchRootFiles|TestEvidenceSentinel'
assert_budget

# Required-check discovery unit tests (fixtures only; no network).
go test ./internal/evidence -count=1 -timeout=30s
assert_budget

spent="$(elapsed)"
printf 'pre-push-sentinel: ok elapsed_seconds=%s budget_seconds=%s\n' "${spent}" "${BUDGET_SECONDS}"
if [ "${spent}" -gt "${BUDGET_SECONDS}" ]; then
  fail "exceeded ${BUDGET_SECONDS}s budget after ${spent}s"
fi
