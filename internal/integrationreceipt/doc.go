// Package integrationreceipt applies ordered integration of completion
// candidates into one designated integration worktree with durable receipts
// and explicit conflict stop (V090-100).
//
// Execution success ≠ integration success. No force operations, no auto model
// conflict resolution, no keeping workers alive during wait. WorkItems close
// only after read-back, verification, and receipt persistence.
package integrationreceipt
