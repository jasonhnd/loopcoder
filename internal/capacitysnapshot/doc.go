// Package capacitysnapshot builds an immutable multi-provider capacity truth
// snapshot for production routing (V090-CRO-003 / #1336).
//
// Design rules:
//   - Unknown capacity is never coerced to zero or full.
//   - Stale or unknown-only observations do not satisfy unattended routing.
//   - No credential material may appear in snapshot fields.
//   - Models absent from a fresh account catalog are not selectable.
//
// Provider-specific *quota packages remain the observation parsers. This package
// unifies their outputs into one digestable snapshot consumed by autoroute.
package capacitysnapshot
