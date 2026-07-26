// Package rehydrate reconstructs project/work/delivery status on a fresh machine
// from GitHub remote evidence after a terminal handoff (V090-068 / #1180).
//
// Mac B never reads or copies Mac A SQLite, local leases, process identity, or
// in-flight scheduler state. Rehydration creates or reuses a stable project
// identity and appends a local rehydration event that references remote evidence
// only. In-flight or ambiguous remote state cannot become a live local attempt.
//
// No database sync, Dolt, distributed lease, or simultaneous same-attempt work.
// PR CI uses isolated home fixtures and fake GitHub histories; no physical
// second Mac is required.
package rehydrate
