// Package codexbar implements an optional CodexBar-compatible observation
// bridge (V090-048 / #1158).
//
// When present, it may supplement official provider quota/catalog observations
// with provenance-tagged, lower-authority evidence. Absence is not an error.
// The bridge never owns credentials, never silently overrides fresher
// higher-authority official facts, and LoopCoder remains fully usable without it.
package codexbar
