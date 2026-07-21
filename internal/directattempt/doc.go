// Package directattempt implements the direct-path worker attempt lifecycle
// (V090-030 / #1129).
//
// One accepted attempt generation launches the explicitly pinned provider once
// in a claimed worktree, only after a required UI client proves start:rendered.
// Terminal cleanup requires flush, join, and reservation release — provider
// exit alone is not completion. No commit/push/PR/verify/merge/retry/route
// choice and no v0.8 autonomous orchestration.
package directattempt
