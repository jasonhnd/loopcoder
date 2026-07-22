// Package installsmoke defines the exact-artifact install, migration, and
// cleanup smoke harness for the V090-081 Darwin arm64 archive (V090-082 / #1197).
//
// Smoke runs only the exact draft archive digest — never rebuilds during smoke.
// Covers clean install, v0.8 export/import fixtures, no-repo-state, redaction,
// cleanup, and DB integrity after interruption.
package installsmoke
