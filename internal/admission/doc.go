// Package admission implements machine-global resource admission and
// generation-fenced reservations in machine.db (V090-016 / #1108).
//
// Policy evaluation is separate from enforcement: threshold crossings produce
// enforcement requests with observed evidence; they never mark attempts failed.
// Unknown process liveness fails closed into attention_required rather than
// automatic reassignment.
//
// No provider quota, scheduler waves, or process killing.
package admission
