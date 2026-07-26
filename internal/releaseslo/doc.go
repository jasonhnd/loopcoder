// Package releaseslo compiles release SLO scorecards and GO/NO-GO evidence from
// manifests tied to a candidate SHA/archive digest (V090-102 / #1198).
//
// Missing metrics never default to pass. Waivers require owner, rationale, scope,
// expiry, and documented risk.
package releaseslo
