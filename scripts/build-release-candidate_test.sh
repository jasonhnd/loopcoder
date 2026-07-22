#!/usr/bin/env bash
# Unit checks for build-release-candidate.sh argument/env gates (no full go build).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
fail() { echo "FAIL: $*" >&2; exit 1; }

# missing VERSION
if OUT_DIR=/tmp/rc-test-$$ VERSION= COMMIT_SHA=abc1234 bash "$ROOT/scripts/build-release-candidate.sh" 2>/dev/null; then
  fail "empty VERSION should fail"
fi

# version=dev
if OUT_DIR=/tmp/rc-test-$$ VERSION=dev COMMIT_SHA=abc1234 bash "$ROOT/scripts/build-release-candidate.sh" 2>/dev/null; then
  fail "VERSION=dev should fail"
fi

# short sha
if OUT_DIR=/tmp/rc-test-$$ VERSION=0.9.0-rc.1 COMMIT_SHA=ab bash "$ROOT/scripts/build-release-candidate.sh" 2>/dev/null; then
  fail "short COMMIT_SHA should fail"
fi

# wrong platform
if GOOS=linux GOARCH=amd64 OUT_DIR=/tmp/rc-test-$$ VERSION=0.9.0-rc.1 COMMIT_SHA=deadbeef bash "$ROOT/scripts/build-release-candidate.sh" 2>/dev/null; then
  fail "non-darwin/arm64 should fail"
fi

echo "build-release-candidate_test: ok"
