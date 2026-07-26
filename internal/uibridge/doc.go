// Package uibridge implements the local HTTP/SSE UI bridge and capability
// handshake for loopcoder.ui.v1 (V090-089 / #1117).
//
// The bridge is an explicitly owned ephemeral loopback process, not a daemon,
// login item, LAN service, discovery broadcast, or cloud relay. Clients must
// present a short-lived scoped bearer capability. Provider credentials and raw
// stores are never exposed; only redacted uireport envelopes flow over SSE.
package uibridge
