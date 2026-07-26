// Package ciwatch implements the zero-model CI and approval watcher
// (V090-033 / #1135).
//
// Required PR checks and approvals are watched as a deterministic state machine
// from GitHub evidence and timers only — no coding-model runner dependency.
// Optional bots (e.g. Greptile) are evidence unless policy explicitly requires
// them. Rate limits use bounded backoff; never busy-poll.
package ciwatch
