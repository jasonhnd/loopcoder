// Package uisub implements durable UI subscription, cursor, and acknowledgement
// ledger over UI-neutral reports (V090-023 / #1115).
//
// Only identified client acknowledgements advance delivery stages. Transport
// handoff or DB writes alone never count as operator-visible. Slow clients are
// isolated with bounded queues and resume from the last accepted cursor.
package uisub
