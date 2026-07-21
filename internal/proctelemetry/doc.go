// Package proctelemetry samples aggregate CPU, RSS, and process-count for an
// owned process tree (V090-015). Metrics are host evidence over known owned
// PIDs only — never a full system scan when identities are known, and never
// provider self-report. Partial/unavailable quality is explicit (never fake zeros).
package proctelemetry
