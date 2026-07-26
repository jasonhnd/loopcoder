// Package localverify implements focused local verification plan selection
// (V090-031 / #1130).
//
// Deterministic policy maps changed files to a bounded command plan. Default
// plans deny full-repo tests, full race, security/release/provider probes, and
// packaging. No model chooses or reinterprets results.
package localverify
