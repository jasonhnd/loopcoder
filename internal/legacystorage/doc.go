// Package legacystorage retires v0.8 storage mutation/write/migration entry
// points from v0.9 command reachability while retaining the smallest audited
// immutable reader for one-release migration export (V090-073 / #1187).
//
// Never mutates or deletes a user's existing DB. Old tables are removed from
// code paths only. Read-only exporter access uses immutable open options.
package legacystorage
