// Package wtclaim implements idempotent worktree and branch claims
// (V090-029 / #1128).
//
// One accepted run gets one isolated worktree and branch at a frozen base SHA.
// Identical retries reuse the claim; conflicts fail typed without clobbering
// user files. Never operates on the customer primary checkout. No force-push,
// no remote PR, no provider.
package wtclaim
