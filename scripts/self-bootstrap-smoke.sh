#!/usr/bin/env bash
# Thin driver for Go self-bootstrap smoke (internal/releasesmoke).
# See docs/specs/1058-depowershell-release-smoke.md.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO="${ROOT}"
BINARY=""
VERSION="0.8.1"
KEEP=0

usage() {
  cat >&2 <<'EOF'
usage: self-bootstrap-smoke.sh [--repo PATH] [--binary PATH] [--version VER] [--keep-artifacts]
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) REPO="${2:-}"; shift 2 ;;
    --binary) BINARY="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --keep-artifacts) KEEP=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *)
      # PowerShell-compatible legacy flags: -Repo -Binary -Version -KeepArtifacts
      case "$1" in
        -Repo) REPO="${2:-}"; shift 2 ;;
        -Binary) BINARY="${2:-}"; shift 2 ;;
        -Version) VERSION="${2:-}"; shift 2 ;;
        -KeepArtifacts) KEEP=1; shift ;;
        *) usage; exit 2 ;;
      esac
      ;;
  esac
done

export LOOPCODER_SMOKE_MODE=self-bootstrap
export LOOPCODER_SMOKE_REPO="$(cd "$REPO" && pwd)"
export LOOPCODER_SMOKE_VERSION="${VERSION}"
if [[ -n "$BINARY" ]]; then
  export LOOPCODER_SMOKE_BINARY="$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")"
fi
if [[ "$KEEP" -eq 1 ]]; then
  export LOOPCODER_SMOKE_KEEP_ARTIFACTS=1
fi

cd "$ROOT"
exec go test ./internal/releasesmoke -run '^TestSelfBootstrapSmoke$' -count=1 -timeout=45m -v
