// Package effectivepolicy implements the v0.9.0 configuration authority
// resolver (V090-085 / #1094).
//
// It freezes one redacted, digests, provenance-bearing effective-policy
// snapshot before side effects. Explicit CLI / approved-run pins cannot be
// overridden by environment variables, host detection, automatic routing, or
// compiled defaults. Credentials and provider auth material are out of scope.
package effectivepolicy
