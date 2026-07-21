// Package termination implements the idempotent stop/join/escalation lifecycle
// for owned process trees (V090-017 / #1109).
//
// Runtime success, failure, and cancel all end through a single join path that
// flushes output, joins observable owned descendants, and releases the machine
// reservation. Escaped/unobservable children become attention-required rather
// than falsely free ownership. Caller context cancellation cannot skip bounded
// cleanup.
//
// No adoption, report rendering, automatic retry, or secret/argv persistence.
package termination
