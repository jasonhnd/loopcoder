// Package termui is the terminal reference UI client for loopcoder.ui.v1
// reports (V090-088 / #1116).
//
// Human and JSONL modes share the same report sequence and digests. Machine
// command stdout is never mixed with human reports. Rendered acknowledgements
// are submitted only after a full bounded write succeeds.
package termui
