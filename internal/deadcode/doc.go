// Package deadcode provides the final dependency/schema/dead-code disposition
// inventory after parity deletion groups (V090-079 / #1193).
//
// Sweep is inventory + unreachable residual policy only — no new behavior.
// Old user files untouched; schema-code deletion is not a destructive DB migration.
// Migration fixture readers and license notices are preserved.
package deadcode
