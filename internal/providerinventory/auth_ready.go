package providerinventory

// ExactFreshReadyAuth is the shared production gate for Ready auth that may
// mark an installation usable for invocation, recover via rehydrate, or set
// capacity Authenticated for unattended routes.
//
// Requires all of:
//   - ReadinessState == Ready
//   - FreshnessState == Fresh (never stale/expired/unknown)
//   - Confidence == Exact
//   - ReadinessConfidence == Exact
//
// Stale, estimated, or unknown Ready must not claim live invocation usability
// and must not become unattended-eligible when combined with exact install/quota.
func ExactFreshReadyAuth(a AuthReadiness) bool {
	if a.ReadinessState != ReadinessReady {
		return false
	}
	if a.FreshnessState != FreshnessFresh {
		return false
	}
	if a.Confidence != ConfidenceExact {
		return false
	}
	if a.ReadinessConfidence != ConfidenceExact {
		return false
	}
	return true
}
