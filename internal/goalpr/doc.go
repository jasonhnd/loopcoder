// Package goalpr opens a real GitHub PR from a LoopCoder-owned goal run and
// always stops at the human merge gate (never auto-merges).
//
// Product path for #1343 body item 7 / scorecard metric real_pr_human_gate:
// branch → commit → push → gh pr create → collect required checks + independent
// verifier evidence. Evidence fields are filled from the live create path, not
// hand-edited canary manifest placeholders.
package goalpr
