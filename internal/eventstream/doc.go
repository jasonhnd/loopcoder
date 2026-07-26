// Package eventstream is the production wiring for loopcoder.ui.v1 report
// follow, acknowledgement, and the run-bounded loopback HTTP/SSE bridge
// (V090-RB01 / #1312).
//
// It persists ordered report envelopes under the project payload, drives
// uisub.Ledger + termui clients, and optionally owns a uibridge.Bridge.
// Lifecycle/store/provider packages remain UI-neutral; Paseo is never imported.
package eventstream
