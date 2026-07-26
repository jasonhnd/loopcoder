// Package authoritystore is the only permitted v0.9.0 entry point for opening
// machine-scoped and project-scoped compact stores (V090-004 / #1096).
//
// Callers must open an explicit role. One on-disk file cannot serve both machine
// and project authority. Domain tables are not added here; later issues extend
// schemas through migrations on these role-specific format identities.
//
// Legacy v0.8 storage is available only through a read-only compatibility port.
package authoritystore
