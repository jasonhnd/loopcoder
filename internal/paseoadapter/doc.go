// Package paseoadapter is the LoopCoder-owned Paseo reference UI adapter
// (V090-093 / #1121).
//
// It consumes only the public loopcoder.ui.v1 protocol (terminal JSONL and/or
// HTTP/SSE bridge). No Paseo source, schema, test, or prose is copied or
// linked. Capability claims are limited to stages proven by real
// acknowledgements. If a truthful rendered acknowledgement cannot be proven
// against a live Paseo surface, the adapter records a bounded interface-gap
// rather than weakening rendered semantics.
//
// Real Paseo process smoke is opt-in (PASEO_ADAPTER_SMOKE=1) and uses only
// synthetic project/report fixtures.
package paseoadapter
