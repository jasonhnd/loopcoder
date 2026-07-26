// Package uiconform is the generic UI conformance runner and golden transcript
// harness for loopcoder.ui.v1 (V090-092 / #1120).
//
// Product claims require black-box protocol transcripts against published
// schemas and transports (terminal JSONL or HTTP/SSE). Lying adapters that
// ack without render, wrong digests, skips, or out-of-order stages fail with
// precise vectors. Manifests record proven profiles only and never claim
// real-host support from fixture-only evidence.
package uiconform
