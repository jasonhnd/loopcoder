// Package routepin implements immutable explicit provider/model/effort pins
// (V090-028 / #1127).
//
// Direct runs cannot launch until provider, canonical model, effort, permission,
// and native-subagent policy are persisted with a route digest. Actual
// invocation must match the pin; mismatch fails closed. Route changes require a
// successor attempt — never silent mutation of the active pin. No credentials,
// no auto-fallback, no quota scoring.
package routepin
