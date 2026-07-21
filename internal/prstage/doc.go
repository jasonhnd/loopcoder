// Package prstage implements idempotent pull-request creation and
// reconciliation (V090-098 / #1134).
//
// One verified remote branch becomes one PR with an immutable receipt. Create
// is isolated from push so network timeouts cannot duplicate PRs or replay
// provider/commit/push work. Never merges, auto-merges, or closes unrelated PRs.
package prstage
