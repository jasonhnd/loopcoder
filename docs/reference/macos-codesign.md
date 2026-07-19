# macOS Developer ID signing and notarization

v0.8.1 public distribution requires Apple trust in addition to Sigstore archive
checksum provenance. This document describes the local harness and release
integration for issue [#1022](https://github.com/jasonhnd/loopcoder/issues/1022).

## Harness

| Item | Path |
| --- | --- |
| Runner | [`scripts/macos-codesign-notarize.sh`](../../scripts/macos-codesign-notarize.sh) |
| Tests | [`scripts/macos-codesign-notarize_test.sh`](../../scripts/macos-codesign-notarize_test.sh) |

## Modes

### dry-run

Structure-only. Validates inputs and writes
`loopcoder.macos_codesign.v1` evidence without calling Apple services or
mutating the binary. Safe for CI.

```bash
bash scripts/macos-codesign-notarize_test.sh
bash scripts/macos-codesign-notarize.sh --mode dry-run --binary ./loopcoder --artifact-dir ./evidence
```

### live

Requires:

1. `APPLE_SIGN=1`
2. Non-PR, non-fork context
3. Developer ID Application identity (`APPLE_CODESIGN_IDENTITY`)
4. Team ID (`APPLE_TEAM_ID`)
5. `notarytool` keychain profile (`APPLE_NOTARY_KEYCHAIN_PROFILE`)
6. macOS host with `codesign`, `ditto`, `xcrun`, and (optionally) `spctl`

Steps performed:

1. `codesign --options runtime --timestamp` (hardened runtime)
2. `codesign --verify --deep --strict`
3. Authority/Team ID checks (must be Developer ID Application)
4. Zip the signed binary and `notarytool submit --wait`
5. Optional staple (`APPLE_STAPLE=auto|yes|no`)
6. `spctl --assess` unless `APPLE_SKIP_SPCTL=1`
7. Optional archive re-pack when `--archive` is set

After live signing, regenerate `SHA256SUMS` and Sigstore over the **final**
bytes. Digest stability is required from post-sign through canary and publish.

## Secrets

Store certificates and notary credentials in a required-review GitHub
environment (for example `release-publication` or `release-apple-signing`).
Never print or upload:

- P12/P8 material
- passwords or API keys
- keychain unlock secrets

Evidence JSON redacts credentials and personal paths.

## Release workflow integration

The release workflow should:

1. Build the darwin/arm64 binary once
2. Run live codesign/notarize on that binary under a protected environment
3. Re-pack `loopcoder_<version>_darwin_arm64.tar.gz`
4. Generate SHA256SUMS + Sigstore on the final archive
5. Smoke/canary against those exact digests
6. Publish only if digests still match

Dry-run remains available when Apple credentials are not configured; live mode
must fail closed rather than publish an ad-hoc-signed Mach-O.

Release repository configuration:

| Kind | Name |
| --- | --- |
| Variable | `APPLE_SIGN=1` to enable live signing on tag releases |
| Secret | `APPLE_CODESIGN_IDENTITY` |
| Secret | `APPLE_TEAM_ID` |
| Secret | `APPLE_NOTARY_KEYCHAIN_PROFILE` |

