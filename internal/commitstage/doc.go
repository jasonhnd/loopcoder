// Package commitstage implements the idempotent local commit stage
// (V090-032 / #1131).
//
// One verified worktree becomes one inspectable commit with an immutable intent
// and read-back receipt. No push, PR, hooks, provider replay, or branch
// deletion. Identical retries adopt a matching commit after timeout inspection.
package commitstage
