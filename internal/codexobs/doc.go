// Package codexobs consolidates Codex discovery and model-catalog observation
// behind the provider observation contract (V090-040 / #1143).
//
// Bounded local probes report install/version, auth known/unknown, account
// profile markers, and a normalized model catalog with reversible aliases.
// This package never launches Codex for work, reads credentials, chooses a
// route, or writes customer repo-local runtime state. Invocation is V090-103.
package codexobs
