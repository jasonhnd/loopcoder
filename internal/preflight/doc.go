// Package preflight implements first-run doctor and direct-run readiness
// probes (V090-026 / #1125).
//
// Before any model is invoked, preflight produces a deterministic evidence
// snapshot: platform, home layout, repo/git, explicit provider capability,
// optional UI/quota gaps, and resource budget. Product prerequisites fail
// closed; optional capabilities warn. Probes are bounded, redacted, and
// read-only unless EnsureLayout is explicitly requested for validated global
// paths outside the repository.
package preflight
