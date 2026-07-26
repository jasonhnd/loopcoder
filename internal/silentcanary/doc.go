// Package silentcanary implements the twelve-minute silent-worker multi-UI
// visibility and cleanup canary (V090-024 / #1139).
//
// A deterministic silent provider fixture runs under an injected clock so start,
// five-minute, ten-minute, and terminal (or blocker) reports fire without
// wall-clock correctness or provider/model polling. Terminal, generic UI bridge,
// and an independent black-box conformance client consume the same report
// digests. Completion and cancellation join owned children, flush evidence, and
// release reservations; manifests stay redacted and free of machine-identifying
// data.
package silentcanary
