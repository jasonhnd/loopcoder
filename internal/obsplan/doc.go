// Package obsplan implements ordered observation-source plans and provenance
// snapshots (V090-038 / #1141).
//
// Provider discovery is modeled as a deterministic ordered source plan with
// per-step bounds and stop/fallback rules. Each run records selected source,
// attempted/skipped sources, typed diagnostics, and capture time. Identical
// observations deduplicate; changed facts produce a new immutable snapshot
// digest suitable for project route events. Never parses credentials or stores
// auth tokens.
package obsplan
