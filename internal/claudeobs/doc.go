// Package claudeobs consolidates Claude Code discovery and model-catalog
// observation (V090-042 / #1146).
//
// Bounded local probes report install/version, auth known/unknown, account
// profile markers, and a normalized model catalog with reversible aliases.
// Never launches Claude for work, reads credentials, chooses a route, or writes
// customer repo-local runtime state.
package claudeobs
