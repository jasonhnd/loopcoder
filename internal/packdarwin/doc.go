// Package packdarwin models Darwin arm64 release packaging: single archive,
// checksums, SBOM, provenance/signature binding, and draft update metadata
// (V090-081 / #1196).
//
// Build once from an exact protected commit. No Windows/Linux product claims.
// Draft/build is separate from publication approval; local developer artifacts
// cannot be promoted.
package packdarwin
