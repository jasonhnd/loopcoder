// Package claudequota normalizes Claude Code quota-window observations (V090-043 / #1148).
//
// Supported five-hour, weekly, credit, and related windows are parsed from
// approved structured fixture/local surfaces with ordered provenance. Missing,
// unlimited, unknown, stale, malformed, and unavailable stay distinct from
// numeric zero. No model routing decisions and no credentials.
package claudequota
