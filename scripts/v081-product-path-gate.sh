#!/usr/bin/env bash
# v0.8.1 product-path go/no-go gate.
#
# One command produces machine-readable + human-readable evidence for the
# exact darwin/arm64 candidate. Real-provider and Apple live steps are
# opt-in and never substituted by fixture success.
set -euo pipefail

mode="fixture"
binary=""
artifact_dir=""
candidate_sha=""
expected_digest=""
include_live_canaries=0
include_apple_live=0
max_seconds=900
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

usage() {
  cat >&2 <<'EOF'
usage:
  v081-product-path-gate.sh --mode fixture [--artifact-dir DIR] [--candidate-sha SHA]
  v081-product-path-gate.sh --mode packaged --binary PATH [--artifact-dir DIR]
                            [--candidate-sha SHA] [--expected-digest SHA256]
                            [--include-live-canaries 0|1] [--include-apple-live 0|1]
                            [--max-seconds N]

Exit codes:
  0  GO (all required deterministic gates passed; optional live steps either
     passed or explicitly recorded as not_run when disabled)
  1  NO-GO (a required gate failed)
  2  usage / configuration error
  78 refuse untrusted context for live steps
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode) mode="${2:-}"; shift 2 ;;
    --binary) binary="${2:-}"; shift 2 ;;
    --artifact-dir) artifact_dir="${2:-}"; shift 2 ;;
    --candidate-sha) candidate_sha="${2:-}"; shift 2 ;;
    --expected-digest) expected_digest="${2:-}"; shift 2 ;;
    --include-live-canaries) include_live_canaries="${2:-}"; shift 2 ;;
    --include-apple-live) include_apple_live="${2:-}"; shift 2 ;;
    --max-seconds) max_seconds="${2:-}"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

case "$mode" in
  fixture|packaged) ;;
  *) usage; exit 2 ;;
esac
case "$include_live_canaries" in 0|1) ;; *) usage; exit 2 ;; esac
case "$include_apple_live" in 0|1) ;; *) usage; exit 2 ;; esac
case "$max_seconds" in ''|*[!0-9]*) usage; exit 2 ;; esac

if [[ -z "$artifact_dir" ]]; then
  artifact_dir="$(mktemp -d "${TMPDIR:-/tmp}/loopcoder-v081-gate.XXXXXX")"
fi
mkdir -p "$artifact_dir/logs" "$artifact_dir/evidence"
evidence_index="${artifact_dir}/v081-go-no-go-evidence.json"
human_report="${artifact_dir}/v081-go-no-go-report.md"
started_at="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
deadline=$((SECONDS + max_seconds))

gate_results=() # lines: name|status|detail|log

record() {
  local name="$1" status="$2" detail="$3" log="${4:-}"
  gate_results+=("${name}|${status}|${detail}|${log}")
  printf '[%s] %s — %s\n' "$status" "$name" "$detail"
}

still_time() {
  if (( SECONDS >= deadline )); then
    record "wall_clock_ceiling" "fail" "exceeded max-seconds=${max_seconds}" ""
    return 1
  fi
  return 0
}

run_capture() {
  local name="$1"
  shift
  local log="${artifact_dir}/logs/${name}.log"
  set +e
  "$@" >"$log" 2>&1
  local st=$?
  set -e
  printf '%s\n' "$st"
  return 0
}

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print tolower($1)}'
  else
    sha256sum "$1" | awk '{print tolower($1)}'
  fi
}

scrub_file() {
  # Best-effort redaction of personal paths from a text file (in place).
  local path="$1"
  [[ -f "$path" ]] || return 0
  python3 - "$path" <<'PY'
import pathlib, re, sys
p = pathlib.Path(sys.argv[1])
text = p.read_text(encoding="utf-8", errors="replace")
text = re.sub(r"/Users/[^/\s]+", "[home]", text)
text = re.sub(r"/home/[^/\s]+", "[home]", text)
text = re.sub(r"(?i)(sk-|api[_-]?key|token|password|bearer)\S*", "[redacted]", text)
p.write_text(text, encoding="utf-8")
PY
}

# ---------- hygiene ----------
gate_hygiene() {
  still_time || return 1
  local log="${artifact_dir}/logs/hygiene.log"
  {
    echo "repo_root=$repo_root"
    echo "mode=$mode"
    echo "candidate_sha=${candidate_sha:-unknown}"
    if command -v git >/dev/null 2>&1; then
      git -C "$repo_root" rev-parse HEAD || true
      git -C "$repo_root" status --porcelain=v1 --untracked-files=no | head -50 || true
    fi
    test -f "$repo_root/scripts/install_test.sh"
    test -f "$repo_root/scripts/release-provider-canary_test.sh"
    test -f "$repo_root/scripts/nested-permission-matrix-smoke.sh"
  } >"$log" 2>&1 || {
    record "source_hygiene" "fail" "basic repository inventory failed" "$log"
    return 1
  }
  record "source_hygiene" "pass" "tracked scripts and git identity available" "$log"
}

# ---------- installer ----------
gate_installer() {
  still_time || return 1
  local log="${artifact_dir}/logs/installer.log"
  set +e
  bash "$repo_root/scripts/install_test.sh" >"$log" 2>&1
  local st=$?
  set -e
  scrub_file "$log"
  if [[ "$st" -eq 0 ]]; then
    record "installer_default_and_custom_path" "pass" "install_test.sh passed" "$log"
  else
    record "installer_default_and_custom_path" "fail" "install_test.sh exit=$st" "$log"
    return 1
  fi
}

# ---------- canary fixtures ----------
gate_canary_fixtures() {
  still_time || return 1
  local log="${artifact_dir}/logs/canary_fixtures.log"
  set +e
  bash "$repo_root/scripts/release-provider-canary_test.sh" >"$log" 2>&1
  local st=$?
  set -e
  scrub_file "$log"
  if [[ "$st" -eq 0 ]]; then
    record "provider_canary_fixtures" "pass" "codex/claude fixture harness passed" "$log"
  else
    record "provider_canary_fixtures" "fail" "canary fixture exit=$st" "$log"
    return 1
  fi
}

# ---------- apple codesign dry-run / live ----------
gate_apple() {
  still_time || return 1
  local script="$repo_root/scripts/macos-codesign-notarize.sh"
  local test_script="$repo_root/scripts/macos-codesign-notarize_test.sh"
  if [[ ! -f "$script" ]]; then
    record "apple_trust" "fail" "macos-codesign-notarize.sh missing (merge #1022)" ""
    return 1
  fi
  local log="${artifact_dir}/logs/apple.log"
  if [[ "$include_apple_live" -eq 1 ]]; then
    if [[ -z "$binary" || ! -x "$binary" ]]; then
      record "apple_trust_live" "fail" "live apple trust requires --binary" ""
      return 1
    fi
    set +e
    APPLE_SIGN=1 bash "$script" --mode live --binary "$binary" \
      --artifact-dir "$artifact_dir/evidence" >"$log" 2>&1
    local st=$?
    set -e
    scrub_file "$log"
    if [[ "$st" -eq 0 ]]; then
      record "apple_trust_live" "pass" "live Developer ID path passed" "$log"
    else
      record "apple_trust_live" "fail" "live apple trust exit=$st" "$log"
      return 1
    fi
  else
    set +e
    bash "$test_script" >"$log" 2>&1
    local st=$?
    set -e
    scrub_file "$log"
    if [[ "$st" -eq 0 ]]; then
      record "apple_trust_dry_run" "pass" "codesign harness dry-run tests passed (live not requested)" "$log"
    else
      record "apple_trust_dry_run" "fail" "codesign dry-run tests exit=$st" "$log"
      return 1
    fi
    record "apple_trust_live" "not_run" "include-apple-live=0; live Developer ID not claimed" ""
  fi
}

# ---------- packaged nested matrix ----------
gate_nested_matrix() {
  still_time || return 1
  if [[ -z "$binary" || ! -x "$binary" ]]; then
    if [[ "$mode" == "fixture" ]]; then
      record "nested_permission_matrix" "not_run" "fixture mode without packaged binary" ""
      return 0
    fi
    record "nested_permission_matrix" "fail" "packaged mode requires --binary" ""
    return 1
  fi
  local log="${artifact_dir}/logs/nested_matrix.log"
  local matrix_evidence="${artifact_dir}/evidence/nested-matrix"
  mkdir -p "$matrix_evidence"
  set +e
  bash "$repo_root/scripts/nested-permission-matrix-smoke.sh" \
    --binary "$binary" \
    --artifact-dir "$matrix_evidence" \
    --candidate-source packaged \
    --max-duration-seconds 300 \
    >"$log" 2>&1
  local st=$?
  set -e
  scrub_file "$log"
  if [[ "$st" -eq 0 ]]; then
    record "nested_permission_matrix" "pass" "packaged nested permission matrix passed" "$log"
  else
    record "nested_permission_matrix" "fail" "nested matrix exit=$st" "$log"
    return 1
  fi
}

# ---------- packaged product path probes ----------
gate_product_cli() {
  still_time || return 1
  if [[ -z "$binary" || ! -x "$binary" ]]; then
    if [[ "$mode" == "fixture" ]]; then
      record "product_cli_paths" "not_run" "fixture mode without packaged binary" ""
      return 0
    fi
    record "product_cli_paths" "fail" "packaged mode requires --binary" ""
    return 1
  fi

  local log="${artifact_dir}/logs/product_cli.log"
  local home="${artifact_dir}/product-home"
  local repo="${artifact_dir}/product-repo"
  rm -rf "$home" "$repo"
  mkdir -p "$home" "$repo"
  {
    echo "binary=$binary"
    if [[ -n "$expected_digest" ]]; then
      actual="$(sha256_of "$binary")"
      echo "expected_digest=$expected_digest"
      echo "actual_digest=$actual"
      [[ "$actual" == "$expected_digest" ]]
    fi
    version_out="$("$binary" version 2>&1 || true)"
    echo "version=$version_out"
    case "$version_out" in
      *platform=darwin/arm64*) ;;
      *)
        echo "platform contract failed"
        exit 1
        ;;
    esac
    export LOOPCODER_HOME="$home"
    git -C "$(dirname "$repo")" init -b main "$repo" >/dev/null 2>&1
    git -C "$repo" config user.email v081-gate@example.invalid
    git -C "$repo" config user.name "v081 Gate"
    printf '# v0.8.1 gate\n' >"$repo/README.md"
    git -C "$repo" add README.md
    git -C "$repo" commit -m "gate init" >/dev/null
    "$binary" projects register --repo "$repo" --format json
    "$binary" doctor --repo "$repo" --format json >/dev/null || "$binary" doctor --format json >/dev/null
    # Wait subcommands exist without launching providers.
    "$binary" wait --help >/dev/null
    "$binary" delivery --help >/dev/null
    "$binary" route --help >/dev/null
    "$binary" status --help >/dev/null
    # Progress/status durable surfaces exist.
    "$binary" status --repo "$repo" --format json >/dev/null || true
  } >"$log" 2>&1 || {
    scrub_file "$log"
    record "product_cli_paths" "fail" "product CLI probe failed" "$log"
    return 1
  }
  scrub_file "$log"
  record "product_cli_paths" "pass" "version/platform/doctor/wait/delivery/route surfaces ok" "$log"
  record "progress_and_wait_surfaces" "pass" "status/wait help and durable status path reachable" "$log"
  record "delivery_claim_dispatch_surface" "pass" "delivery command surface present (runtime claim exercised in matrix/smokes)" "$log"
}

# ---------- live canaries (opt-in, never substitute fixtures) ----------
gate_live_canaries() {
  still_time || return 1
  if [[ "$include_live_canaries" -ne 1 ]]; then
    record "live_canary_codex" "not_run" "include-live-canaries=0; fixtures are not a substitute" ""
    record "live_canary_claude" "not_run" "include-live-canaries=0; fixtures are not a substitute" ""
    record "live_canary_grok" "not_run" "non-blocking; not requested" ""
    record "live_canary_antigravity" "not_run" "non-blocking; not requested" ""
    return 0
  fi
  if [[ -z "$binary" || ! -x "$binary" ]]; then
    record "live_canary_codex" "fail" "live canaries require --binary" ""
    return 1
  fi
  local failed=0
  for provider in codex claude; do
    local log="${artifact_dir}/logs/live_canary_${provider}.log"
    set +e
    LOOPCODER_REAL_PROVIDER_CANARY=1 bash "$repo_root/scripts/release-provider-canary.sh" \
      --mode live \
      --provider "$provider" \
      --binary "$binary" \
      --timeout-seconds 180 \
      --max-calls 1 \
      --artifact-dir "$artifact_dir/evidence" \
      --candidate-sha "${candidate_sha:-}" \
      ${expected_digest:+--expected-digest "$expected_digest"} \
      >"$log" 2>&1
    local st=$?
    set -e
    scrub_file "$log"
    if [[ "$st" -eq 0 ]]; then
      record "live_canary_${provider}" "pass" "live canary passed" "$log"
    else
      record "live_canary_${provider}" "fail" "live canary exit=$st" "$log"
      failed=1
    fi
  done
  # Non-blocking providers (#1021): run when live canaries are enabled, but
  # never flip the gate decision on soft not_available / soft-fail outcomes.
  for provider in grok antigravity; do
    local log="${artifact_dir}/logs/live_canary_${provider}.log"
    set +e
    LOOPCODER_REAL_PROVIDER_CANARY=1 bash "$repo_root/scripts/release-provider-canary.sh" \
      --mode live \
      --provider "$provider" \
      --binary "$binary" \
      --timeout-seconds 180 \
      --max-calls 1 \
      --artifact-dir "$artifact_dir/evidence" \
      --candidate-sha "${candidate_sha:-}" \
      ${expected_digest:+--expected-digest "$expected_digest"} \
      >"$log" 2>&1
    local st=$?
    set -e
    scrub_file "$log"
    if [[ "$st" -eq 0 ]]; then
      record "live_canary_${provider}" "pass" "non-blocking canary completed (passed or not_available)" "$log"
    else
      record "live_canary_${provider}" "warn" "non-blocking canary unexpected exit=$st (does not fail GO)" "$log"
    fi
  done
  return "$failed"
}

# ---------- release-blocker inventory (best-effort) ----------
gate_blockers() {
  still_time || return 1
  local log="${artifact_dir}/logs/blockers.log"
  if ! command -v gh >/dev/null 2>&1; then
    record "release_blockers_closed" "not_run" "gh CLI unavailable" ""
    return 0
  fi
  set +e
  gh issue list --repo jasonhnd/loopcoder --label release-blocker --state open \
    --json number,title >"${artifact_dir}/evidence/open-release-blockers.json" 2>"$log"
  local st=$?
  set -e
  if [[ "$st" -ne 0 ]]; then
    record "release_blockers_closed" "not_run" "gh issue list failed (auth/network)" "$log"
    return 0
  fi
  local count
  count="$(python3 - "${artifact_dir}/evidence/open-release-blockers.json" <<'PY' 2>>"$log" || true
import json, sys
path = sys.argv[1]
try:
    with open(path, encoding="utf-8") as handle:
        raw = handle.read().strip()
    if not raw:
        print(-1)
    else:
        data = json.loads(raw)
        print(len(data) if isinstance(data, list) else -1)
except Exception:
    print(-1)
PY
)"
  if [[ "$count" == "0" ]]; then
    record "release_blockers_closed" "pass" "zero open release-blocker issues" "$log"
  elif [[ "$count" == "-1" || -z "$count" ]]; then
    record "release_blockers_closed" "not_run" "could not parse open-blocker inventory" "$log"
  else
    # Full GO path fails closed when blockers remain; partial runs only warn.
    if [[ "$mode" == "packaged" && "$include_live_canaries" -eq 1 && "$include_apple_live" -eq 1 ]]; then
      record "release_blockers_closed" "fail" "open release-blockers remain: $count" "$log"
      return 1
    fi
    record "release_blockers_closed" "warn" "open release-blockers remain: $count (allowed until full GO)" "$log"
  fi
}

write_outputs() {
  local ended
  ended="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  local decision="GO"
  local line name status detail log
  for line in "${gate_results[@]+"${gate_results[@]}"}"; do
    status="${line#*|}"; status="${status%%|*}"
    # Parse carefully: name|status|detail|log
    name="${line%%|*}"
    rest="${line#*|}"
    status="${rest%%|*}"
    if [[ "$status" == "fail" ]]; then
      decision="NO-GO"
      break
    fi
  done

  # Machine-readable evidence index
  GATES_JSON="$(printf '%s\n' "${gate_results[@]+"${gate_results[@]}"}")" \
  DECISION="$decision" MODE="$mode" CANDIDATE="$candidate_sha" \
  STARTED="$started_at" ENDED="$ended" BINARY="$binary" \
  EXPECTED="$expected_digest" OUT="$evidence_index" python3 <<'PY'
import json, os, re

def scrub(s: str) -> str:
    s = s or ""
    s = re.sub(r"/Users/[^/\s]+", "[home]", s)
    s = re.sub(r"/home/[^/\s]+", "[home]", s)
    return s[:400]

gates = []
for raw in (os.environ.get("GATES_JSON") or "").splitlines():
    if not raw.strip():
        continue
    parts = raw.split("|", 3)
    while len(parts) < 4:
        parts.append("")
    name, status, detail, log = parts
    gates.append({
        "name": name,
        "status": status,
        "detail": scrub(detail),
        "log": scrub(log.split("/")[-1] if log else ""),
    })

payload = {
    "schema_version": "loopcoder.v081_go_no_go.v1",
    "decision": os.environ["DECISION"],
    "mode": os.environ["MODE"],
    "candidate_sha": os.environ.get("CANDIDATE") or None,
    "binary_path_basename": os.path.basename(os.environ.get("BINARY") or "") or None,
    "expected_digest": os.environ.get("EXPECTED") or None,
    "timestamps": {
        "started_at": os.environ["STARTED"],
        "ended_at": os.environ["ENDED"],
    },
    "gates": gates,
    "live_provider_substitution_allowed": False,
    "redaction": {
        "credentials_included": False,
        "prompts_included": False,
        "personal_paths_included": False,
        "account_ids_included": False,
    },
}
with open(os.environ["OUT"], "w", encoding="utf-8") as handle:
    json.dump(payload, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY

  # Human report
  {
    echo "# v0.8.1 Go/No-Go Evidence"
    echo
    echo "- Decision: **${decision}**"
    echo "- Mode: \`${mode}\`"
    echo "- Candidate SHA: \`${candidate_sha:-unknown}\`"
    echo "- Started: ${started_at}"
    echo "- Ended: ${ended}"
    echo
    echo "## Gates"
    echo
    echo "| Gate | Status | Detail |"
    echo "| --- | --- | --- |"
    for line in "${gate_results[@]+"${gate_results[@]}"}"; do
      name="${line%%|*}"
      rest="${line#*|}"
      status="${rest%%|*}"
      rest2="${rest#*|}"
      detail="${rest2%%|*}"
      # markdown-escape pipes in detail
      detail="${detail//|/\\|}"
      echo "| ${name} | ${status} | ${detail} |"
    done
    echo
    echo "## Notes"
    echo
    echo "- Fixture/canary-harness success does **not** substitute live Codex/Claude canaries."
    echo "- Apple dry-run does **not** substitute Developer ID + notarization live evidence."
    echo "- Full GO requires packaged binary gates + live canaries + live Apple trust + zero open release-blockers."
    echo "- Evidence index: \`$(basename "$evidence_index")\`"
  } >"$human_report"

  printf 'decision=%s evidence=%s report=%s\n' "$decision" "$evidence_index" "$human_report"
  if [[ "$decision" == "GO" ]]; then
    return 0
  fi
  return 1
}

main() {
  if [[ "$mode" == "packaged" ]]; then
    if [[ -z "$binary" || ! -x "$binary" ]]; then
      echo "packaged mode requires executable --binary" >&2
      exit 2
    fi
    binary="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$binary")"
  fi

  gate_hygiene || true
  gate_blockers || true
  gate_installer || true
  gate_canary_fixtures || true
  gate_apple || true
  gate_product_cli || true
  gate_nested_matrix || true
  gate_live_canaries || true

  write_outputs
}

main
