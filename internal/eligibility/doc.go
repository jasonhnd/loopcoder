// Package eligibility implements hard route eligibility and immutable explicit-pin
// precedence before any soft quota scoring (V090-051).
//
// Evaluation is pure and deterministic from a captured snapshot: unknown hard
// prerequisites never assume true; high quota never makes an incompatible route
// eligible; an ineligible explicit pin fails closed with no automatic fallback.
package eligibility
