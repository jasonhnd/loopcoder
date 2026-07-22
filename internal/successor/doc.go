// Package successor defines when a failed attempt may create a new attempt with
// another eligible route (V090-054).
//
// Route identity never changes inside an active attempt. Ambiguous launch never
// auto-falls back. Explicit pins have no cross-route fallback unless the owner
// pre-authorized a named ordered policy. Prior attempt evidence remains linked
// and immutable.
package successor
