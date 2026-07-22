package providerinventory

import (
	"sort"
	"strings"
	"time"
)

// RehydrateForAutoRoute merges a live Discover report with durable inventory
// previously written by providers refresh / quota telemetry.
//
// Production auto-route must not require a hand-passed capacity snapshot.
// Discover without an explicit quota network grant emits honest
// not-collected / unavailable quota rows; after a successful
// `providers refresh --grant-quota-telemetry`, durable store holds exact or
// estimated windows with freshness/reset. This merge rehydrates those durable
// windows into the live report without inventing capacity.
//
// Policy:
//   - Keep live installations, auth, models, and probes as current host truth.
//   - For each adapter, if live has no trustworthy fresh quota window, overlay
//     durable trustworthy non-expired snapshots (and their telemetry sources).
//   - Never promote unavailable/unknown/stale durable rows into usable capacity.
//   - Never copy credential material (payloads are already redacted records).
func RehydrateForAutoRoute(live, durable Report, now time.Time) Report {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	out := live
	if len(durable.QuotaSnapshots) == 0 && len(durable.AuthReadiness) == 0 && len(durable.ModelCapabilities) == 0 {
		return out
	}

	// Freshness marking mirrors Load() so stale-after is honored.
	durableSnaps := markQuotaFreshness(LinkQuotaConflicts(append([]QuotaSnapshot(nil), durable.QuotaSnapshots...)), now)

	liveTrust := trustworthyQuotaByProvider(out.QuotaSnapshots)
	durableTrust := trustworthyQuotaByProvider(durableSnaps)

	var mergedSnaps []QuotaSnapshot
	var mergedSources []QuotaTelemetrySource
	sourceSeen := map[string]bool{}
	for _, s := range out.QuotaTelemetrySources {
		if s.QuotaSourceID == "" || sourceSeen[s.QuotaSourceID] {
			continue
		}
		sourceSeen[s.QuotaSourceID] = true
		mergedSources = append(mergedSources, s)
	}

	// Start from live snapshots, then replace per-provider when durable is better.
	providers := map[string]bool{}
	for _, s := range out.QuotaSnapshots {
		providers[s.AdapterID] = true
	}
	for p := range durableTrust {
		providers[p] = true
	}
	providerList := make([]string, 0, len(providers))
	for p := range providers {
		providerList = append(providerList, p)
	}
	sort.Strings(providerList)

	for _, provider := range providerList {
		liveOK := len(liveTrust[provider]) > 0
		durableOK := len(durableTrust[provider]) > 0
		if !liveOK && durableOK {
			// Drop live unusable rows for this provider; use durable trustworthy set.
			mergedSnaps = append(mergedSnaps, durableTrust[provider]...)
			for _, src := range durable.QuotaTelemetrySources {
				if src.AdapterID != provider {
					continue
				}
				if src.QuotaSourceID == "" || sourceSeen[src.QuotaSourceID] {
					continue
				}
				// Only attach sources referenced by selected snapshots.
				if !sourceReferenced(durableTrust[provider], src.QuotaSourceID) {
					continue
				}
				sourceSeen[src.QuotaSourceID] = true
				mergedSources = append(mergedSources, src)
			}
			continue
		}
		// Keep live snapshots for this provider (including honest unavailable).
		for _, s := range out.QuotaSnapshots {
			if s.AdapterID == provider {
				mergedSnaps = append(mergedSnaps, s)
			}
		}
	}

	out.QuotaSnapshots = LinkQuotaConflicts(mergedSnaps)
	out.QuotaTelemetrySources = mergedSources

	// Auth: if live is not ready for an adapter but durable still has ready+fresh,
	// rehydrate readiness (restart recovery after prior refresh).
	out.AuthReadiness = rehydrateAuth(out.AuthReadiness, durable.AuthReadiness, now)
	// Models: fill missing live catalog entries from durable when still fresh.
	out.ModelCapabilities = rehydrateModels(out.ModelCapabilities, durable.ModelCapabilities)

	// Drop grant-required gaps when we successfully rehydrated durable quota.
	out.GapReasons = filterRehydratedGaps(out.GapReasons, out.QuotaSnapshots)

	if fp, err := fingerprint(out); err == nil {
		out.InventoryFingerprint = fp
	}
	return out
}

func trustworthyQuotaByProvider(snapshots []QuotaSnapshot) map[string][]QuotaSnapshot {
	out := map[string][]QuotaSnapshot{}
	for _, provider := range adaptersInSnapshots(snapshots) {
		selected := latestTrustworthyQuotaSnapshotsByScope(Report{QuotaSnapshots: snapshots}, provider)
		// latestTrustworthy already filters unavailable/unknown/expired/terminal.
		// Additionally require freshness=fresh for auto-route rehydrate (stale
		// last-known-good must not silently satisfy unattended routing).
		var fresh []QuotaSnapshot
		for _, s := range selected {
			if s.FreshnessState == FreshnessFresh &&
				(s.Confidence == ConfidenceExact || s.Confidence == ConfidenceEstimated) {
				fresh = append(fresh, s)
			}
		}
		if len(fresh) > 0 {
			out[provider] = fresh
		}
	}
	return out
}

func adaptersInSnapshots(snapshots []QuotaSnapshot) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range snapshots {
		p := strings.TrimSpace(s.AdapterID)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func sourceReferenced(snaps []QuotaSnapshot, sourceID string) bool {
	for _, s := range snaps {
		if s.QuotaSourceID == sourceID {
			return true
		}
	}
	return false
}

func rehydrateAuth(live, durable []AuthReadiness, now time.Time) []AuthReadiness {
	_ = now
	if len(durable) == 0 {
		return live
	}
	liveBy := map[string]AuthReadiness{}
	for _, a := range live {
		liveBy[a.AdapterID] = a
	}
	out := append([]AuthReadiness(nil), live...)
	for _, d := range durable {
		if d.ReadinessState != ReadinessReady {
			continue
		}
		if d.FreshnessState == FreshnessStale || d.FreshnessState == FreshnessExpired {
			continue
		}
		cur, ok := liveBy[d.AdapterID]
		if ok && (cur.ReadinessState == ReadinessReady) {
			continue
		}
		// Replace non-ready live row or append.
		replaced := false
		for i := range out {
			if out[i].AdapterID == d.AdapterID {
				out[i] = d
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, d)
		}
	}
	return out
}

func rehydrateModels(live, durable []ModelCapability) []ModelCapability {
	if len(durable) == 0 {
		return live
	}
	hasLive := map[string]bool{}
	for _, m := range live {
		if strings.TrimSpace(m.CanonicalModelID) != "" {
			hasLive[m.AdapterID] = true
		}
	}
	out := append([]ModelCapability(nil), live...)
	for _, m := range durable {
		if hasLive[m.AdapterID] {
			continue
		}
		if m.FreshnessState == FreshnessStale || m.FreshnessState == FreshnessExpired {
			continue
		}
		if m.AvailabilityState != AvailabilityAvailable {
			continue
		}
		if m.LifecycleState == LifecycleRemoved || m.LifecycleState == LifecycleDeprecated {
			continue
		}
		out = append(out, m)
	}
	return out
}

func filterRehydratedGaps(gaps []string, snaps []QuotaSnapshot) []string {
	hasTrust := map[string]bool{}
	for p, list := range trustworthyQuotaByProvider(snaps) {
		if len(list) > 0 {
			hasTrust[p] = true
		}
	}
	var out []string
	for _, g := range gaps {
		drop := false
		for p := range hasTrust {
			if g == "provider-"+p+"-quota-unsupported" {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, g)
		}
	}
	return out
}
