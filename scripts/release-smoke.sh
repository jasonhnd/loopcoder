#!/usr/bin/env bash
# Thin driver for Go release smoke (internal/releasesmoke).
# See docs/specs/1058-depowershell-release-smoke.md.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION=""
PREVIOUS="0.7.0"
GITHUB_REPO="jasonhnd/loopcoder"
KEEP=0

usage() {
  cat >&2 <<'EOF'
usage: release-smoke.sh --version VER [--previous VER] [--repo owner/name] [--keep-artifacts]
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version|-Version) VERSION="${2:-}"; shift 2 ;;
    --previous|-PreviousVersion) PREVIOUS="${2:-}"; shift 2 ;;
    --repo|-Repo) GITHUB_REPO="${2:-}"; shift 2 ;;
    --keep-artifacts|-KeepArtifacts) KEEP=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

if [[ -z "$VERSION" ]]; then
  usage
  exit 2
fi

export LOOPCODER_SMOKE_MODE=release
export LOOPCODER_SMOKE_VERSION="$VERSION"
export LOOPCODER_SMOKE_PREVIOUS_VERSION="$PREVIOUS"
export LOOPCODER_SMOKE_GITHUB_REPO="$GITHUB_REPO"
export LOOPCODER_SMOKE_REPO="$ROOT"
if [[ "$KEEP" -eq 1 ]]; then
  export LOOPCODER_SMOKE_KEEP_ARTIFACTS=1
fi

cd "$ROOT"
exec go test ./internal/releasesmoke -run '^TestReleaseSmoke$' -count=1 -timeout=45m -v
