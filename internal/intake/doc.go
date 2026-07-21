// Package intake implements GitHub issue intake and immutable policy snapshots
// (V090-027 / #1126).
//
// One issue is fetched, validated, and frozen into a work-request snapshot with
// source revision and policy digest. Later source edits produce drift events and
// never silently overwrite the active snapshot. No provider, worktree, comment,
// label, branch, or PR side effects.
package intake
