// Package pushstage implements the idempotent remote branch push stage
// (V090-097 / #1133).
//
// One accepted commit is published to one intended remote branch with read-back
// and a stage receipt. Timeouts reconcile remote state; they never authorize
// worker replay, a second commit, or force-push. No PR, merge, ref deletion,
// credential storage, or history rewrite.
package pushstage
