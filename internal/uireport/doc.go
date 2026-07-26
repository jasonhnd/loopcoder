// Package uireport implements the UI-neutral loopcoder.ui.v1 report envelope and
// compact human view model (V090-022 / #1114).
//
// Reports project accepted events into bounded envelopes. Pretty text is never
// authority. Credentials, prompts, issue bodies, and absolute paths are excluded
// from default content.
package uireport
