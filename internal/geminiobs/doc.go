// Package geminiobs consolidates Gemini CLI discovery and model-catalog
// observation (V090-044 / #1149).
//
// Bounded local probes report install/version, auth known/unknown, account
// profile markers, and a normalized model catalog with reversible aliases.
// Never launches Gemini for work, reads credentials, chooses a route, or writes
// customer repo-local runtime state.
package geminiobs
