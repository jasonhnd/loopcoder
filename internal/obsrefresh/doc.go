// Package obsrefresh implements adaptive refresh, health, and cooldown state
// for provider observations (V090-039 / #1142).
//
// Fresh evidence is reused; concurrent stale demand triggers at most one
// machine-scoped probe per source. Success/failure backoff with bounded jitter
// survives restart. Unknown, stale, and unavailable stay distinct from healthy
// or zero-capacity. Manual refresh cannot silently bypass safety cooldowns.
// Waiting uses zero coding-model calls.
package obsrefresh
