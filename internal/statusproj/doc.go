// Package statusproj builds a compact current-status projection and cursor-based
// event follow stream (V090-021 / #1113).
//
// Projections rebuild from events with versioned checkpoints. Heartbeat,
// concrete progress, delivery gate, and final-mile stage are distinct fields.
// Unknown/stale evidence is never rendered as healthy success by omission.
package statusproj
