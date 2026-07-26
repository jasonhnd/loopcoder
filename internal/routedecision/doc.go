// Package routedecision persists deterministic route decisions composed from
// hard eligibility, soft quota policy, and optional mode adjustments (V090-053).
//
// Same task + policy + evidence snapshot always yields the same ordered
// candidates, winner, reasons, and digest. Explain surfaces pin precedence,
// hard exclusions, soft score components, and tie-breaks without credentials
// or raw quota payloads.
package routedecision
