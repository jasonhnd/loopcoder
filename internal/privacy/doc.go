// Package privacy provides private-repository redaction policy, synthetic
// canary markers, leak scanning, and fail-closed GitHub visibility checks
// (V090-067 / #1179).
//
// Scope is ordinary development of policy + fixtures only:
//   - data classes and allowed destinations
//   - centralized redaction for each destination surface
//   - synthetic secret markers that must never appear in global/host/CI output
//   - least-permission GitHub model and fail-closed on visibility ambiguity
//
// No encryption-at-rest claim, no credential manager, and no real private
// content is required for CI. Real private-repo canaries are owner-only at the
// release gate.
package privacy
