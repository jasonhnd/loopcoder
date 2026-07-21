// Package deliveryresume implements delivery-only resume without worker replay
// (V090-035 / #1137).
//
// After worker cleanup-terminal, crash/timeout/UI disconnect resumes the first
// incomplete delivery, watch, verifier, or gate stage. Completed worker launches
// are never repeated. Ambiguous Git/GitHub side effects are read back and adopted
// idempotently. Dry-run plans evidence and proposed actions without mutation.
package deliveryresume
