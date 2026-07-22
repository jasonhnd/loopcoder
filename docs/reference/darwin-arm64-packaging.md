# Darwin arm64 packaging, signing, and update metadata (V090-081)

Package: [`internal/packdarwin`](../../internal/packdarwin)  
Issue: [#1196](https://github.com/jasonhnd/loopcoder/issues/1196)

## Purpose

One reproducible **darwin/arm64** archive from an exact protected commit in a
clean hosted environment, with checksums, SBOM, provenance/signature binding,
and **draft** release metadata. No Windows/Linux product claims.

## Archive members

Required: binary `loopcoder`, `LICENSE`, `README.md`, v0.9 quickstart, `VERSION`,
`COMMIT`. Forbidden: windows/linux/exe assets.

## Flow

1. `ValidateBuildIdentity` — full SHA, version, `CleanHosted=true`
2. `NewArtifact` — SHA-256 of archive bytes
3. `Checksums` / `BuildSBOM` / `BindProvenance`
4. `NewDraftRelease` — draft only; `LocalDev` cannot promote
5. `ApprovePublication` — separate human/publication approval

## Verification

```bash
go test ./internal/packdarwin/
```
