#!/usr/bin/env bash
# Developer ID sign / notarize / verify for the darwin/arm64 loopcoder binary.
#
# Modes:
#   dry-run  – structure-only (no Apple credentials, no network)
#   live     – codesign + notarytool + optional staple + Gatekeeper assess
#
# Never prints certificate material, passwords, or API keys.
set -euo pipefail

mode=""
binary=""
archive=""
artifact_dir=""
identity="${APPLE_CODESIGN_IDENTITY:-${LOOPCODER_CODESIGN_IDENTITY:-}}"
team_id="${APPLE_TEAM_ID:-${LOOPCODER_APPLE_TEAM_ID:-}}"
bundle_id="${APPLE_BUNDLE_ID:-${LOOPCODER_APPLE_BUNDLE_ID:-com.jasonhnd.loopcoder}}"
keychain_profile="${APPLE_NOTARY_KEYCHAIN_PROFILE:-${LOOPCODER_NOTARY_KEYCHAIN_PROFILE:-}}"
staple="${APPLE_STAPLE:-${LOOPCODER_STAPLE:-auto}}" # auto|yes|no
skip_spctl="${APPLE_SKIP_SPCTL:-${LOOPCODER_SKIP_SPCTL:-0}}"

usage() {
  cat >&2 <<'EOF'
usage:
  macos-codesign-notarize.sh --mode dry-run --binary PATH [--archive PATH] [--artifact-dir DIR]
  macos-codesign-notarize.sh --mode live --binary PATH [--archive PATH] [--artifact-dir DIR]
                             [--identity "Developer ID Application: ..."]
                             [--team-id TEAMID] [--bundle-id ID]
                             [--keychain-profile PROFILE]

Environment (live):
  APPLE_CODESIGN_IDENTITY       Developer ID Application identity
  APPLE_TEAM_ID                 10-character Team ID
  APPLE_BUNDLE_ID               Bundle identifier (default com.jasonhnd.loopcoder)
  APPLE_NOTARY_KEYCHAIN_PROFILE notarytool keychain profile name
  APPLE_STAPLE                  auto|yes|no (default auto)
  APPLE_SKIP_SPCTL              1 to skip spctl (CI runners may lack full Gatekeeper)
  APPLE_SIGN=1                  required opt-in for live mode
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode) mode="${2:-}"; shift 2 ;;
    --binary) binary="${2:-}"; shift 2 ;;
    --archive) archive="${2:-}"; shift 2 ;;
    --artifact-dir) artifact_dir="${2:-}"; shift 2 ;;
    --identity) identity="${2:-}"; shift 2 ;;
    --team-id) team_id="${2:-}"; shift 2 ;;
    --bundle-id) bundle_id="${2:-}"; shift 2 ;;
    --keychain-profile) keychain_profile="${2:-}"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

case "$mode" in
  dry-run|live) ;;
  *) usage; exit 2 ;;
esac
if [[ -z "$binary" || ! -f "$binary" ]]; then
  echo "macos-codesign-notarize: --binary must be an existing file" >&2
  exit 2
fi
if [[ ! -x "$binary" ]]; then
  chmod +x "$binary" 2>/dev/null || true
fi

reject_untrusted_context() {
  case "${GITHUB_EVENT_NAME-}" in
    pull_request|pull_request_target)
      echo "macos-codesign-notarize refuses pull_request events" >&2
      exit 78
      ;;
  esac
  if [[ -n "${GITHUB_EVENT_PATH-}" && -f "${GITHUB_EVENT_PATH}" ]]; then
    if ! python3 - "${GITHUB_EVENT_PATH}" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as handle:
    event = json.load(handle)
if event.get("pull_request") is not None:
    sys.exit(1)
if (event.get("repository") or {}).get("fork") is True:
    sys.exit(1)
sys.exit(0)
PY
    then
      echo "macos-codesign-notarize refuses forked or pull_request contexts" >&2
      exit 78
    fi
  fi
}

iso_now() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print tolower($1)}'
  else
    sha256sum "$1" | awk '{print tolower($1)}'
  fi
}

# Redact free-form diagnostic snippets before writing evidence.
scrub() {
  python3 -c 'import re,sys; t=sys.stdin.read();
t=re.sub(r"(?i)(password|secret|api[_-]?key|bearer|private[_-]?key)\S*","[redacted]",t);
t=re.sub(r"/Users/[^/\s]+","[home]",t);
print(t[:400])'
}

write_evidence() {
  local status="$1"
  local detail="$2"
  local signed="${3:-false}"
  local notarized="${4:-false}"
  local stapled="${5:-false}"
  local gatekeeper="${6:-not_run}"
  local team="${7:-}"
  local authority="${8:-}"
  local digest_before="${9:-}"
  local digest_after="${10:-}"

  if [[ -z "$artifact_dir" ]]; then
    return 0
  fi
  mkdir -p "$artifact_dir"
  local out="${artifact_dir}/macos-codesign-evidence.json"
  STATUS="$status" DETAIL="$detail" SIGNED="$signed" NOTARIZED="$notarized" \
    STAPLED="$stapled" GATEKEEPER="$gatekeeper" TEAM="$team" AUTH="$authority" \
    BEFORE="$digest_before" AFTER="$digest_after" MODE="$mode" \
    BINARY_NAME="$(basename "$binary")" STARTED="$started_at" ENDED="$(iso_now)" \
    OUT="$out" python3 <<'PY'
import json, os, re

def scrub(v: str) -> str:
    text = v or ""
    text = re.sub(r"(?i)(password|secret|api[_-]?key|bearer|private[_-]?key)\S*", "[redacted]", text)
    text = re.sub(r"/Users/[^/\s]+", "[home]", text)
    return text[:240]

payload = {
    "schema_version": "loopcoder.macos_codesign.v1",
    "mode": os.environ["MODE"],
    "status": os.environ["STATUS"],
    "detail_code": scrub(os.environ["DETAIL"]),
    "binary_basename": os.environ["BINARY_NAME"],
    "signed": os.environ["SIGNED"] == "true",
    "notarized": os.environ["NOTARIZED"] == "true",
    "stapled": os.environ["STAPLED"] == "true",
    "gatekeeper": os.environ["GATEKEEPER"],
    "team_id": scrub(os.environ.get("TEAM", "")) or None,
    "authority": scrub(os.environ.get("AUTH", "")) or None,
    "digests": {
        "before_sha256": os.environ.get("BEFORE") or None,
        "after_sha256": os.environ.get("AFTER") or None,
        "stable": (
            bool(os.environ.get("BEFORE") and os.environ.get("AFTER"))
            and os.environ.get("BEFORE") == os.environ.get("AFTER")
        ),
    },
    "timestamps": {
        "started_at": os.environ["STARTED"],
        "ended_at": os.environ["ENDED"],
    },
    "redaction": {
        "credentials_included": False,
        "certificate_material_included": False,
        "personal_paths_included": False,
    },
}
with open(os.environ["OUT"], "w", encoding="utf-8") as handle:
    json.dump(payload, handle, indent=2, sort_keys=True)
    handle.write("\n")
print(os.environ["OUT"])
PY
}

started_at="$(iso_now)"
binary="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$binary")"
digest_before="$(sha256_of "$binary")"

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "macos-codesign-notarize: required command missing: $1" >&2
    exit 1
  }
}

run_dry_run() {
  # Validate inputs and emit evidence without Apple tools mutating the binary.
  if [[ ! -f "$binary" ]]; then
    write_evidence "failed" "binary_missing" false false false "not_run" "" "" "$digest_before" ""
    exit 1
  fi
  if [[ -n "$archive" && ! -f "$archive" ]]; then
    write_evidence "failed" "archive_missing" false false false "not_run" "" "" "$digest_before" ""
    exit 1
  fi
  # Simulate digest stability for dry-run (no mutation).
  local digest_after="$digest_before"
  write_evidence "passed" "dry_run_ok" false false false "skipped" "DRYRUN0" "Developer ID Application: dry-run" "$digest_before" "$digest_after"
  echo "macos-codesign-notarize dry-run ok binary=$(basename "$binary") digest=$digest_before"
}

run_live() {
  if [[ "${APPLE_SIGN-}" != "1" ]]; then
    echo "live Apple sign disabled; set APPLE_SIGN=1 after protected approval" >&2
    exit 2
  fi
  reject_untrusted_context

  require_cmd codesign
  require_cmd ditto
  require_cmd xcrun
  require_cmd python3

  if [[ -z "$identity" ]]; then
    write_evidence "failed" "missing_identity" false false false "not_run" "" "" "$digest_before" ""
    echo "APPLE_CODESIGN_IDENTITY / --identity is required" >&2
    exit 1
  fi
  if [[ -z "$team_id" ]]; then
    write_evidence "failed" "missing_team_id" false false false "not_run" "" "" "$digest_before" ""
    echo "APPLE_TEAM_ID / --team-id is required" >&2
    exit 1
  fi
  if [[ -z "$keychain_profile" ]]; then
    write_evidence "failed" "missing_notary_profile" false false false "not_run" "" "" "$digest_before" ""
    echo "APPLE_NOTARY_KEYCHAIN_PROFILE / --keychain-profile is required" >&2
    exit 1
  fi

  # Never echo secrets; identity/team are non-secret metadata.
  local work
  work="$(mktemp -d "${TMPDIR:-/tmp}/loopcoder-codesign.XXXXXX")"
  cleanup() { rm -rf "$work"; }
  trap cleanup EXIT

  # 1) Sign with hardened runtime + secure timestamp.
  if ! codesign \
    --force \
    --options runtime \
    --timestamp \
    --sign "$identity" \
    --identifier "$bundle_id" \
    "$binary" 2>"$work/codesign.err"; then
    write_evidence "failed" "codesign_failed" false false false "not_run" "$team_id" "" "$digest_before" ""
    echo "codesign failed (details redacted)" >&2
    exit 1
  fi

  if ! codesign --verify --deep --strict --verbose=2 "$binary" >"$work/verify.out" 2>&1; then
    write_evidence "failed" "codesign_verify_failed" true false false "not_run" "$team_id" "" "$digest_before" "$(sha256_of "$binary")"
    echo "codesign --verify failed" >&2
    exit 1
  fi

  local authority
  authority="$(codesign -dv --verbose=2 "$binary" 2>&1 | sed -n 's/^Authority=\(.*\)$/\1/p' | head -n1 || true)"
  local recorded_team
  recorded_team="$(codesign -dv --verbose=2 "$binary" 2>&1 | sed -n 's/^TeamIdentifier=\(.*\)$/\1/p' | head -n1 || true)"
  if [[ -n "$recorded_team" && "$recorded_team" != "$team_id" ]]; then
    write_evidence "failed" "team_id_mismatch" true false false "not_run" "$recorded_team" "$authority" "$digest_before" "$(sha256_of "$binary")"
    echo "TeamIdentifier mismatch" >&2
    exit 1
  fi
  case "$authority" in
    *"Developer ID Application"*) ;;
    *)
      write_evidence "failed" "not_developer_id" true false false "not_run" "${recorded_team:-$team_id}" "$authority" "$digest_before" "$(sha256_of "$binary")"
      echo "signature is not Developer ID Application" >&2
      exit 1
      ;;
  esac

  # 2) Notarize a zip of the signed binary (Apple accepts zip for CLI tools).
  local zip_path="$work/loopcoder-notarize.zip"
  ditto -c -k --keepParent "$binary" "$zip_path"
  if ! xcrun notarytool submit "$zip_path" \
    --keychain-profile "$keychain_profile" \
    --wait \
    --timeout 30m \
    >"$work/notary.out" 2>"$work/notary.err"; then
    write_evidence "failed" "notarize_failed" true false false "not_run" "${recorded_team:-$team_id}" "$authority" "$digest_before" "$(sha256_of "$binary")"
    echo "notarytool submit failed (details redacted)" >&2
    exit 1
  fi
  if ! grep -Eqi 'status:[[:space:]]*Accepted|Accepted' "$work/notary.out" "$work/notary.err" 2>/dev/null; then
    # Some notarytool versions print JSON; accept "status": "Accepted".
    if ! grep -Eqi '"status"[[:space:]]*:[[:space:]]*"Accepted"|status[[:space:]]*=[[:space:]]*Accepted' "$work/notary.out" "$work/notary.err" 2>/dev/null; then
      write_evidence "failed" "notarize_not_accepted" true false false "not_run" "${recorded_team:-$team_id}" "$authority" "$digest_before" "$(sha256_of "$binary")"
      echo "notarization did not report Accepted" >&2
      exit 1
    fi
  fi

  # 3) Staple when possible (raw Mach-O often cannot be stapled; document auto skip).
  local stapled=false
  case "$staple" in
    yes)
      if xcrun stapler staple "$binary" >"$work/staple.out" 2>"$work/staple.err"; then
        stapled=true
      else
        write_evidence "failed" "staple_failed" true true false "not_run" "${recorded_team:-$team_id}" "$authority" "$digest_before" "$(sha256_of "$binary")"
        echo "stapler staple failed" >&2
        exit 1
      fi
      ;;
    no)
      stapled=false
      ;;
    auto|*)
      if xcrun stapler staple "$binary" >"$work/staple.out" 2>"$work/staple.err"; then
        stapled=true
      else
        stapled=false
        echo "stapler staple skipped (unsupported for this artifact form)" >&2
      fi
      ;;
  esac

  # 4) Gatekeeper assessment on a clean copy path.
  local gatekeeper="not_run"
  if [[ "$skip_spctl" != "1" ]]; then
    if spctl --assess --type execute --verbose=4 "$binary" >"$work/spctl.out" 2>&1; then
      gatekeeper="accepted"
    else
      # Online-notarized CLI tools sometimes need --ignore-cache; try once.
      if spctl --assess --type execute --ignore-cache --verbose=4 "$binary" >"$work/spctl2.out" 2>&1; then
        gatekeeper="accepted"
      else
        write_evidence "failed" "gatekeeper_rejected" true true "$stapled" "rejected" "${recorded_team:-$team_id}" "$authority" "$digest_before" "$(sha256_of "$binary")"
        echo "spctl --assess rejected the signed binary" >&2
        exit 1
      fi
    fi
  else
    gatekeeper="skipped"
  fi

  # 5) If an archive path is provided, re-pack the signed binary into it and
  #    record digests. Caller is responsible for regenerating SHA256SUMS/Sigstore
  #    over the final bytes.
  local digest_after
  digest_after="$(sha256_of "$binary")"
  if [[ -n "$archive" ]]; then
    local arch_dir="$work/repack"
    mkdir -p "$arch_dir"
    cp "$binary" "$arch_dir/loopcoder"
    chmod 755 "$arch_dir/loopcoder"
    # Prefer LICENSE next to the binary (extract layout) or CWD.
    if [[ -f "$(dirname "$binary")/LICENSE" ]]; then
      cp "$(dirname "$binary")/LICENSE" "$arch_dir/LICENSE"
    elif [[ -f LICENSE ]]; then
      cp LICENSE "$arch_dir/LICENSE"
    fi
    if [[ -f "$arch_dir/LICENSE" ]]; then
      tar -C "$arch_dir" -czf "$archive" loopcoder LICENSE
    else
      tar -C "$arch_dir" -czf "$archive" loopcoder
    fi
  fi

  write_evidence "passed" "ok" true true "$stapled" "$gatekeeper" "${recorded_team:-$team_id}" "$authority" "$digest_before" "$digest_after"
  echo "macos-codesign-notarize live ok team=${recorded_team:-$team_id} stapled=$stapled gatekeeper=$gatekeeper"
  # Note: digest_before != digest_after is expected (codesign mutates the Mach-O).
  # Stability is required across notarize→publish, which the caller enforces by
  # hashing after this script returns.
}

case "$mode" in
  dry-run) run_dry_run ;;
  live) run_live ;;
esac
