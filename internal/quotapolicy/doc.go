// Package quotapolicy normalizes heterogeneous quota windows into burn-urgency,
// reserve, and reliability soft-ranking features for hard-eligible routes
// (V090-052).
//
// Windows are never compared as fake absolute tokens across providers. Unknown
// and stale evidence follow explicit uncertainty policy (not numeric zero). Soft
// ranking never mutates immutable pins or hard eligibility decisions.
package quotapolicy
