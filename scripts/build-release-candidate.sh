#!/usr/bin/env bash
# Build one Darwin arm64 v0.9 draft archive with SBOM + SHA256SUMS (V090-RB08 / #1319).
# Required env: VERSION, COMMIT_SHA. Optional: OUT_DIR (default dist).
set -euo pipefail

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "${name} is required" >&2
    exit 1
  fi
}

require_env VERSION
require_env COMMIT_SHA

if [[ "${VERSION}" == "dev" || -z "${VERSION}" ]]; then
  echo "VERSION must not be empty or dev" >&2
  exit 1
fi
if [[ ${#COMMIT_SHA} -lt 7 ]]; then
  echo "COMMIT_SHA too short" >&2
  exit 1
fi

OUT_DIR="${OUT_DIR:-dist}"
GOOS_V="${GOOS:-darwin}"
GOARCH_V="${GOARCH:-arm64}"

if [[ "${GOOS_V}" != "darwin" || "${GOARCH_V}" != "arm64" ]]; then
  echo "only darwin/arm64 draft archives are supported" >&2
  exit 1
fi

if command -v go >/dev/null 2>&1; then
  actual_goos="$(go env GOOS)"
  actual_goarch="$(go env GOARCH)"
  if [[ "${actual_goos}" != "darwin" || "${actual_goarch}" != "arm64" ]]; then
    echo "host Go tuple must be darwin/arm64; got ${actual_goos}/${actual_goarch}" >&2
    exit 1
  fi
fi

build_date="$(git show -s --format=%cI "${COMMIT_SHA}" 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)"
archive="loopcoder_${VERSION}_darwin_arm64.tar.gz"
binary="loopcoder"

rm -rf "${OUT_DIR}"
mkdir -p "${OUT_DIR}/package" "${OUT_DIR}/evidence"

CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build \
  -trimpath \
  -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT_SHA} -X main.date=${build_date} -X main.buildSource=release-candidate" \
  -o "${OUT_DIR}/package/${binary}" \
  ./cmd/loopcoder

version_output="$("${OUT_DIR}/package/${binary}" --version 2>/dev/null || true)"
echo "${version_output}"
if [[ "${version_output}" == *"version=dev"* ]]; then
  echo "binary still reports version=dev" >&2
  exit 1
fi
if [[ "${version_output}" == *"commit=unknown"* || "${version_output}" == *"date=unknown"* ]]; then
  echo "binary still reports unknown commit/date" >&2
  exit 1
fi

cp LICENSE "${OUT_DIR}/package/LICENSE"
if [[ -f README.md ]]; then
  cp README.md "${OUT_DIR}/package/README.md"
fi

tar -C "${OUT_DIR}/package" -czf "${OUT_DIR}/${archive}" .
rm -rf "${OUT_DIR}/package"

# SHA256SUMS
(
  cd "${OUT_DIR}"
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${archive}" > SHA256SUMS
  else
    sha256sum "${archive}" > SHA256SUMS
  fi
)

# Minimal SPDX SBOM (CycloneDX-compatible JSON summary)
archive_digest="$(awk '{print tolower($1)}' "${OUT_DIR}/SHA256SUMS" | head -1)"
cat > "${OUT_DIR}/sbom.spdx.json" <<EOF
{
  "spdxVersion": "SPDX-2.3",
  "dataLicense": "CC0-1.0",
  "SPDXID": "SPDXRef-DOCUMENT",
  "name": "loopcoder-${VERSION}",
  "documentNamespace": "https://github.com/jasonhnd/loopcoder/spdx/${VERSION}/${COMMIT_SHA}",
  "creationInfo": {
    "created": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
    "creators": ["Tool: loopcoder-build-release-candidate"]
  },
  "packages": [
    {
      "SPDXID": "SPDXRef-Package-loopcoder",
      "name": "loopcoder",
      "versionInfo": "${VERSION}",
      "downloadLocation": "NOASSERTION",
      "filesAnalyzed": false,
      "checksums": [
        {"algorithm": "SHA256", "checksumValue": "${archive_digest}"}
      ],
      "externalRefs": [
        {
          "referenceCategory": "OTHER",
          "referenceType": "commit",
          "referenceLocator": "${COMMIT_SHA}"
        }
      ]
    }
  ]
}
EOF

# Provenance / evidence record (machine-readable; not a public release)
cat > "${OUT_DIR}/evidence/rc-manifest.json" <<EOF
{
  "schema": "loopcoder.release.candidate.v1",
  "version": "${VERSION}",
  "commit_sha": "${COMMIT_SHA}",
  "build_date": "${build_date}",
  "platform": "darwin/arm64",
  "build_source": "release-candidate",
  "archive": "${archive}",
  "archive_digest_sha256": "${archive_digest}",
  "sbom": "sbom.spdx.json",
  "checksums": "SHA256SUMS",
  "public_release": false,
  "draft_only": true,
  "signing": {
    "status": "optional_environment",
    "notes": "Apple signing/notarization and Sigstore attach when protected secrets available"
  }
}
EOF

# Bind check: all digests match
python3 - <<'PY'
import json, hashlib, pathlib, sys
root = pathlib.Path("dist") if pathlib.Path("dist").exists() else pathlib.Path(".")
# locate OUT_DIR via env not available — use default dist
import os
out = pathlib.Path(os.environ.get("OUT_DIR", "dist"))
man = json.loads((out / "evidence" / "rc-manifest.json").read_text())
arch = out / man["archive"]
h = hashlib.sha256(arch.read_bytes()).hexdigest()
if h != man["archive_digest_sha256"]:
    print("archive digest mismatch", h, man["archive_digest_sha256"], file=sys.stderr)
    sys.exit(1)
sums = (out / "SHA256SUMS").read_text().strip().split()
if sums[0].lower() != h:
    print("SHA256SUMS mismatch", file=sys.stderr)
    sys.exit(1)
print("rc_ok=true digest=%s version=%s sha=%s" % (h, man["version"], man["commit_sha"]))
PY

printf 'evidence_tier=release-candidate\n'
printf 'tested_commit_sha=%s\n' "${COMMIT_SHA}"
printf 'archive_digest_sha256=%s\n' "${archive_digest}"
printf 'archive_name=%s\n' "${archive}"
printf 'version=%s\n' "${VERSION}"
printf 'draft_only=true\n'
