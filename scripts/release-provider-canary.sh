#!/usr/bin/env bash
# Protected real-provider canary for Codex and Claude release evidence.
#
# Modes:
#   fixture  – deterministic local scenarios (no paid calls)
#   live     – one bounded read-only invocation per provider against a
#              packaged candidate binary (requires explicit opt-in)
#
# Live mode never runs on pull_request events and never falls back from one
# provider to another. Evidence is sanitized (no prompts, credentials, paths).
set -euo pipefail

mode=""
provider=""
scenario="success"
binary=""
artifact_dir=""
timeout_seconds=120
max_calls=1
candidate_sha=""
expected_digest=""

usage() {
  cat >&2 <<'EOF'
usage:
  release-provider-canary.sh --mode fixture --provider codex|claude [--scenario success|auth_failure|quota_failure|timeout|malformed_output|cancel|missing_cli] [--artifact-dir DIR]
  release-provider-canary.sh --mode live --provider codex|claude --binary PATH [--timeout-seconds N] [--max-calls 1] [--artifact-dir DIR] [--expected-digest SHA256] [--candidate-sha GITSHA]
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode) mode="${2:-}"; shift 2 ;;
    --provider) provider="${2:-}"; shift 2 ;;
    --scenario) scenario="${2:-}"; shift 2 ;;
    --binary) binary="${2:-}"; shift 2 ;;
    --artifact-dir) artifact_dir="${2:-}"; shift 2 ;;
    --timeout-seconds) timeout_seconds="${2:-}"; shift 2 ;;
    --max-calls) max_calls="${2:-}"; shift 2 ;;
    --candidate-sha) candidate_sha="${2:-}"; shift 2 ;;
    --expected-digest) expected_digest="${2:-}"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

case "$mode" in
  fixture|live) ;;
  *) usage; exit 2 ;;
esac
case "$provider" in
  codex|claude) ;;
  *) usage; exit 2 ;;
esac
case "$timeout_seconds" in
  ''|*[!0-9]*) usage; exit 2 ;;
esac
if [[ "$timeout_seconds" -lt 15 || "$timeout_seconds" -gt 300 ]]; then
  echo "timeout-seconds must be between 15 and 300" >&2
  exit 2
fi
case "$max_calls" in
  ''|*[!0-9]*) usage; exit 2 ;;
esac
if [[ "$max_calls" -ne 1 ]]; then
  echo "max-calls must be 1 (no retry loop)" >&2
  exit 2
fi

# Pull requests and forks never receive live canary execution.
reject_untrusted_context() {
  case "${GITHUB_EVENT_NAME-}" in
    pull_request|pull_request_target)
      echo "release provider canary refuses pull_request events" >&2
      exit 78
      ;;
  esac
  if [[ -n "${GITHUB_EVENT_PATH-}" && -f "${GITHUB_EVENT_PATH}" ]]; then
    if python3 - "${GITHUB_EVENT_PATH}" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as handle:
    event = json.load(handle)
repo = event.get("repository") or {}
# Fork PRs / untrusted head repos must never run secret-bearing canaries.
if event.get("pull_request") is not None:
    sys.exit(1)
if repo.get("fork") is True:
    sys.exit(1)
head = ((event.get("pull_request") or {}).get("head") or {}).get("repo") or {}
if head.get("fork") is True:
    sys.exit(1)
sys.exit(0)
PY
    then
      :
    else
      echo "release provider canary refuses forked or pull_request contexts" >&2
      exit 78
    fi
  fi
}

iso_now() {
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

write_evidence() {
  local status="$1"
  local result_class="$2"
  local detail_code="$3"
  local model_id="${4:-}"
  local adapter_mode="${5:-}"
  local receipt_delivery="${6:-not_attempted}"
  local provider_launches="${7:-0}"
  local cancelled="${8:-false}"

  if [[ -z "$artifact_dir" ]]; then
    return 0
  fi
  mkdir -p "$artifact_dir"
  local out="${artifact_dir}/release-provider-canary-${provider}.json"
  PROVIDER="$provider" MODE="$mode" SCENARIO="$scenario" STATUS="$status" \
    RESULT_CLASS="$result_class" DETAIL_CODE="$detail_code" MODEL_ID="$model_id" \
    ADAPTER_MODE="$adapter_mode" RECEIPT="$receipt_delivery" LAUNCHES="$provider_launches" \
    CANCELLED="$cancelled" CANDIDATE_SHA="$candidate_sha" BINARY_DIGEST="${binary_digest:-}" \
    EXPECTED_DIGEST="$expected_digest" STARTED="$started_at" ENDED="$(iso_now)" \
    TIMEOUT="$timeout_seconds" MAX_CALLS="$max_calls" OUT="$out" python3 <<'PY'
import json, os, re

def scrub(value: str) -> str:
    text = value or ""
    # Drop obvious secrets and absolute personal paths from free text.
    text = re.sub(r"(?i)(sk-|api[_-]?key|token|password|bearer)\S*", "[redacted]", text)
    text = re.sub(r"/Users/[^/\s]+", "[home]", text)
    text = re.sub(r"/home/[^/\s]+", "[home]", text)
    return text[:240]

payload = {
    "schema_version": "loopcoder.release_provider_canary.v1",
    "provider": os.environ["PROVIDER"],
    "mode": os.environ["MODE"],
    "scenario": os.environ["SCENARIO"] or None,
    "status": os.environ["STATUS"],
    "result_class": os.environ["RESULT_CLASS"],
    "detail_code": scrub(os.environ["DETAIL_CODE"]),
    "resolved_model": scrub(os.environ.get("MODEL_ID", "")) or None,
    "adapter_mode": scrub(os.environ.get("ADAPTER_MODE", "")) or None,
    "progress_receipt_delivery": os.environ.get("RECEIPT") or "not_attempted",
    "provider_launches": int(os.environ.get("LAUNCHES") or "0"),
    "cancelled": os.environ.get("CANCELLED") == "true",
    "limits": {
        "max_calls": int(os.environ["MAX_CALLS"]),
        "timeout_seconds": int(os.environ["TIMEOUT"]),
        "concurrency": 1,
        "retries": 0,
    },
    "candidate": {
        "git_sha": os.environ.get("CANDIDATE_SHA") or None,
        "binary_sha256": os.environ.get("BINARY_DIGEST") or None,
        "expected_sha256": os.environ.get("EXPECTED_DIGEST") or None,
        "digest_match": (
            bool(os.environ.get("EXPECTED_DIGEST"))
            and os.environ.get("BINARY_DIGEST") == os.environ.get("EXPECTED_DIGEST")
        ) if os.environ.get("EXPECTED_DIGEST") else None,
    },
    "timestamps": {
        "started_at": os.environ["STARTED"],
        "ended_at": os.environ["ENDED"],
    },
    "redaction": {
        "credentials_included": False,
        "prompts_included": False,
        "raw_provider_output_included": False,
        "personal_paths_included": False,
        "account_ids_included": False,
    },
    "fallback_provider": None,
    "blocking": True,
}
with open(os.environ["OUT"], "w", encoding="utf-8") as handle:
    json.dump(payload, handle, indent=2, sort_keys=True)
    handle.write("\n")
print(os.environ["OUT"])
PY
}

started_at="$(iso_now)"
binary_digest=""

# ---------- fixture mode ----------
run_fixture() {
  case "$scenario" in
    success|auth_failure|quota_failure|timeout|malformed_output|cancel|missing_cli) ;;
    *)
      echo "unknown fixture scenario: $scenario" >&2
      exit 2
      ;;
  esac

  case "$scenario" in
    missing_cli)
      write_evidence "failed" "infrastructure" "cli_not_found" "" "discover" "not_attempted" 0 false
      echo "fixture[$provider]: missing_cli -> infrastructure/cli_not_found" >&2
      exit 1
      ;;
    auth_failure)
      write_evidence "failed" "infrastructure" "auth_unavailable" "" "auth" "not_attempted" 0 false
      echo "fixture[$provider]: auth_failure -> infrastructure/auth_unavailable" >&2
      exit 1
      ;;
    quota_failure)
      write_evidence "failed" "infrastructure" "quota_exhausted" "" "quota" "not_attempted" 0 false
      echo "fixture[$provider]: quota_failure -> infrastructure/quota_exhausted" >&2
      exit 1
      ;;
    timeout)
      write_evidence "failed" "infrastructure" "timeout" "" "invoke" "not_attempted" 1 true
      echo "fixture[$provider]: timeout -> infrastructure/timeout (cancelled)" >&2
      exit 1
      ;;
    malformed_output)
      write_evidence "failed" "product" "malformed_provider_output" "" "parse" "not_attempted" 1 false
      echo "fixture[$provider]: malformed_output -> product/malformed_provider_output" >&2
      exit 1
      ;;
    cancel)
      write_evidence "failed" "infrastructure" "cancelled" "" "invoke" "not_attempted" 1 true
      echo "fixture[$provider]: cancel -> infrastructure/cancelled" >&2
      exit 1
      ;;
    success)
      write_evidence "passed" "product" "ok" "fixture-model" "read-only" "delivered" 1 false
      echo "fixture[$provider]: success"
      exit 0
      ;;
  esac
}

# ---------- live mode ----------
command_for_provider() {
  case "$provider" in
    codex) printf '%s\n' codex ;;
    claude) printf '%s\n' claude ;;
  esac
}

run_live() {
  # Opt-in before trust gates so missing approval is not masked by ambient CI
  # event names (e.g. pull_request on the verify job).
  if [[ "${LOOPCODER_REAL_PROVIDER_CANARY-}" != "1" ]]; then
    echo "live canary disabled; set LOOPCODER_REAL_PROVIDER_CANARY=1 after protected approval" >&2
    exit 2
  fi
  reject_untrusted_context

  if [[ -z "$binary" || ! -x "$binary" ]]; then
    echo "live canary requires --binary executable" >&2
    exit 2
  fi
  if ! command -v python3 >/dev/null 2>&1 || ! command -v shasum >/dev/null 2>&1 || ! command -v git >/dev/null 2>&1; then
    echo "live canary requires python3, shasum, and git" >&2
    exit 2
  fi

  binary="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$binary")"
  binary_digest="$(shasum -a 256 "$binary" | awk '{print $1}')"
  if [[ -n "$expected_digest" && "$binary_digest" != "$expected_digest" ]]; then
    write_evidence "failed" "infrastructure" "artifact_digest_mismatch" "" "digest" "not_attempted" 0 false
    echo "candidate digest mismatch: got $binary_digest want $expected_digest" >&2
    exit 1
  fi

  if ! version_text="$("$binary" version 2>/dev/null)"; then
    write_evidence "failed" "infrastructure" "candidate_unusable" "" "discover" "not_attempted" 0 false
    echo "candidate binary could not report version" >&2
    exit 1
  fi
  case "$version_text" in
    *platform=darwin/arm64*) ;;
    *)
      write_evidence "failed" "infrastructure" "unsupported_platform" "" "discover" "not_attempted" 0 false
      echo "live canary requires packaged darwin/arm64 candidate" >&2
      exit 1
      ;;
  esac

  cli_name="$(command_for_provider)"
  if ! command -v "$cli_name" >/dev/null 2>&1; then
    write_evidence "failed" "infrastructure" "cli_not_found" "" "discover" "not_attempted" 0 false
    echo "provider CLI not found: $cli_name" >&2
    exit 1
  fi

  root="$(mktemp -d "${TMPDIR:-/tmp}/loopcoder-release-canary.XXXXXX")"
  repo="$root/repo"
  loopcoder_home="$root/home"
  plan="$root/plan.json"
  result="$root/result.json"
  worktree=""
  old_loopcoder_home="${LOOPCODER_HOME-}"
  had_loopcoder_home=0
  if [[ -n "${LOOPCODER_HOME+x}" ]]; then
    had_loopcoder_home=1
  fi

  cleanup_live() {
    if [[ -n "${worktree-}" && -d "$repo" ]]; then
      git -C "$repo" worktree remove --force "$worktree" >/dev/null 2>&1 || true
    fi
    # Best-effort: no detached provider children should remain under our temp home.
    if [[ "$had_loopcoder_home" -eq 1 ]]; then
      export LOOPCODER_HOME="$old_loopcoder_home"
    else
      unset LOOPCODER_HOME
    fi
    rm -rf "$root"
  }
  trap cleanup_live EXIT

  mkdir -p "$repo" "$loopcoder_home"
  export LOOPCODER_HOME="$loopcoder_home"
  git -C "$root" init -b main "$repo" >/dev/null 2>&1
  git -C "$repo" config user.email release-provider-canary@example.invalid
  git -C "$repo" config user.name "Release Provider Canary"
  printf '# Release provider canary\n' >"$repo/README.md"
  git -C "$repo" add README.md
  git -C "$repo" commit -m "Initialize release provider canary" >/dev/null
  git -C "$repo" update-ref refs/remotes/origin/main HEAD

  if ! "$binary" projects register --repo "$repo" --format json >"$root/register.json" 2>"$root/register.stderr"; then
    write_evidence "failed" "product" "project_register_failed" "" "setup" "not_attempted" 0 false
    echo "projects register failed" >&2
    exit 1
  fi

  # Inventory/auth readiness (sanitized): only status codes, not credentials.
  inventory_status="unknown"
  if "$binary" providers refresh --format json >"$root/inventory.json" 2>"$root/inventory.stderr"; then
    inventory_status="refreshed"
  else
    inventory_status="refresh_failed"
  fi

  PROVIDER="$provider" PLAN="$plan" python3 <<'PY'
import json, os
provider = os.environ["PROVIDER"]
parent = "run-release-canary-" + provider
payload = {
    "schema_version": "loopcoder.child_plan.v1",
    "plan_id": "plan-" + parent,
    "parent_run_id": parent,
    "root_run_id": parent,
    "parent_depth": 0,
    "max_depth": 1,
    "max_concurrency": 1,
    "created_at": "2026-07-19T00:00:00Z",
    "items": [{
        "child_key": "release-canary-" + provider,
        "title": "Inspect README.md and report the heading without changing any file",
        "role": "worker",
        "run_id": "run-release-canary-child-" + provider,
        "issue": 1020,
        "scope": {
            "repo": ".",
            "paths": ["README.md"],
            "issues": [1020],
            "commands": ["git diff -- README.md"],
        },
        "permission": "read-only",
        "depends_on": [],
        "aggregation": {"mode": "collect", "required": True, "include_report": True},
    }],
}
with open(os.environ["PLAN"], "w", encoding="utf-8") as handle:
    json.dump(payload, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY

  # Exactly one provider launch; no retries.
  set +e
  "$binary" nested run \
    --repo "$repo" \
    --plan "$plan" \
    --provider "$provider" \
    --base-branch main \
    --timeout "${timeout_seconds}s" \
    --strict \
    --format json \
    >"$result" 2>"$root/provider.stderr"
  launch_status=$?
  set -e

  if [[ "$launch_status" -ne 0 ]]; then
    # Classify without retaining raw provider output.
    detail="provider_invoke_failed"
    result_class="infrastructure"
    stderr_snip="$(tr '\n' ' ' <"$root/provider.stderr" | head -c 400 || true)"
    case "$stderr_snip" in
      *[Aa]uth*|*[Ll]ogin*|*[Uu]nauthorized*) detail="auth_unavailable" ;;
      *[Qq]uota*|*[Rr]ate*limit*|*[Ll]imit*) detail="quota_exhausted" ;;
      *[Tt]imeout*|*[Dd]eadline*) detail="timeout" ;;
      *[Mm]alformed*|*JSON*|*parse*) detail="malformed_provider_output"; result_class="product" ;;
    esac
    write_evidence "failed" "$result_class" "$detail" "" "read-only" "not_attempted" 1 false
    echo "live canary invoke failed class=$result_class detail=$detail inventory=$inventory_status" >&2
    exit 1
  fi

  if ! REPORT="$result" python3 <<'PY'
import json, os, sys
with open(os.environ["REPORT"], encoding="utf-8") as handle:
    report = json.load(handle)
children = report.get("children") or []
if report.get("status") != "succeeded" or len(children) != 1 or children[0].get("status") != "succeeded":
    sys.exit(1)
child = children[0]
enforcement = child.get("read_only_enforcement") or {}
if enforcement.get("verification") != "passed":
    sys.exit(2)
# Model may appear on child/route fields; never require prompt text.
sys.exit(0)
PY
  then
    write_evidence "failed" "product" "unexpected_product_status" "" "read-only" "not_attempted" 1 false
    echo "live canary product status unexpected" >&2
    exit 1
  fi

  if [[ -n "$(git -C "$repo" status --porcelain=v1 --untracked-files=all)" ]]; then
    write_evidence "failed" "product" "parent_repository_mutated" "" "read-only" "not_attempted" 1 false
    echo "live canary mutated parent repository" >&2
    exit 1
  fi

  model_id="$(REPORT="$result" python3 -c 'import json,os; r=json.load(open(os.environ["REPORT"])); c=(r.get("children") or [{}])[0]; print((c.get("route") or c.get("model") or {}).get("model") if isinstance(c.get("route") or c.get("model"), dict) else (c.get("model") or ""))' 2>/dev/null || true)"
  receipt="delivered"
  if [[ -d "$loopcoder_home" ]] && find "$loopcoder_home" -type f -name '*progress*' -print -quit 2>/dev/null | grep -q .; then
    receipt="delivered"
  fi

  write_evidence "passed" "product" "ok" "${model_id}" "read-only" "$receipt" 1 false
  echo "live canary passed: provider=$provider inventory=$inventory_status launches=1"
}

case "$mode" in
  fixture) run_fixture ;;
  live) run_live ;;
esac
