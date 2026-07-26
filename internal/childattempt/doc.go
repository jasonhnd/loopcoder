// Package childattempt isolates cross-provider WorkItem executions as their own
// Attempts (V090-063). Each child has independent claim generation, route digest,
// worktree, credentials scope, and terminal evidence. Parent workflow aggregates
// status without rewriting child terminals or sharing writable checkouts.
package childattempt
