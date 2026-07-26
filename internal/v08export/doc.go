// Package v08export extracts supported facts from v0.8 global/repo-local schema
// and payloads into a versioned neutral export (V090-069 / #1183).
//
// Read-only: never write, repair, upgrade, or execute through the old store.
// Source files remain byte-for-byte unchanged. Credentials, live leases, and PID
// authority never enter the export. Output is produced outside the customer repo.
package v08export
