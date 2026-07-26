// Package supportbundle builds bounded, redacted diagnostic support bundles
// with no-telemetry defaults (V090-101 / #1195).
//
// Bundles never include source, issue/PR bodies, prompts, auth, env, absolute
// home paths, raw logs, tokens, or provider responses by default. No network
// upload, analytics, crash reporting, or phone-home behavior.
package supportbundle
