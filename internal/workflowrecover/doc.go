// Package workflowrecover implements deterministic workflow cancellation,
// restart reconciliation, and compact terminal projection (V090-064).
//
// Parent success is never optimistic: it requires durable accepted child
// terminals. Cancellation joins owned children, releases only proven unstarted
// claims, and records ambiguous ownership. Compact projections retain audit
// range digests without deleting source events.
package workflowrecover
