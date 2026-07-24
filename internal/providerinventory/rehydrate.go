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
// `providers refresh --grant-quota-telemetry`, exact/estimated windows
// with freshness/reset persist in the local store and are rehydrated here.
//
// Policy:
//   - Keep live installations, auth, models, and probes as current host truth.
//   - For each adapter, if live has no trustworthy fresh quota window, overlay
//     durable trustworthy non-expired snapshots (and their telemetry sources).
//   - Never promote unavailable/unknown/stale durable rows into usable capacity.
//   - Never promote durable installations as live-installed host truth.
//   - When durable quota/catalog references a pinst known only in durable exact/
//     fresh installation metadata, translate that pinst to the live PATH-primary
//     install (lowest DiscoveryOrder) sharing adapter + exact ResolvedPathHash
//     (RC36 path-alias rehydrate). Zero live targets ⇒ leave id unchanged.
//     Multiple live path aliases of one binary coalesce onto PATH primary.
//   - Durable AuthReadiness is historical: Load does not recompute FreshnessState.
//     Recovery uses a 30m CapturedAt horizon, latest state across ALL readiness
//     states per identity, and live exact+fresh observations dominate durable.
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

	// RC36: durable quota may reference path-alias pinst B while live Discover only
	// has alias A. Build durable→live install translation from exact+fresh resolved
	// identities on BOTH sides before overlaying durable quota.
	installAlias := durableToLiveInstallAliasMap(out.Installations, durable.Installations)
	durableSnaps = rewriteQuotaInstallIDs(durableSnaps, installAlias)
	durableAuth := rewriteAuthInstallIDs(append([]AuthReadiness(nil), durable.AuthReadiness...), installAlias)
	durableModels := append([]ModelCapability(nil), durable.ModelCapabilities...)
	// Catalog snapshots may carry ProviderInstallationID used for model attach.
	durableCatalogs := rewriteCatalogInstallIDs(append([]ModelCatalogSnapshot(nil), durable.ModelCatalogSnapshots...), installAlias)

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
			// Drop live unusable rows for this provider; use durable trustworthy set
			// (install IDs already translated to live aliases when unambiguous).
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

	// Auth: recover sole recent exact+fresh durable Ready only when live lacks an
	// exact+fresh observation on that install and durable latest truth allows it.
	out.AuthReadiness = rehydrateAuth(out.AuthReadiness, durableAuth, now)
	// Models: fill missing live catalog entries from durable when still fresh.
	out.ModelCapabilities = rehydrateModels(out.ModelCapabilities, durableModels)
	// Catalog snapshots: fill when live has no exact MR catalog for adapter.
	out.ModelCatalogSnapshots = rehydrateCatalogSnapshots(out.ModelCatalogSnapshots, durableCatalogs)

	// Live Installations remain sole host truth for Installed/Usable. Do not
	// append durable installs as live-installed (RC36 review).

	// Drop grant-required gaps when we successfully rehydrated durable quota.
	out.GapReasons = filterRehydratedGaps(out.GapReasons, out.QuotaSnapshots)

	if fp, err := fingerprint(out); err == nil {
		out.InventoryFingerprint = fp
	}
	return out
}

// exactFreshInstallIdentity reports whether an installation row may participate
// in path-alias resolution. Requires exact confidence, fresh (non-stale) state,
// and a nonempty ResolvedPathHash. Estimated/stale/empty-hash never alias-fuse.
func exactFreshInstallIdentity(inst ProviderInstallation) bool {
	if strings.TrimSpace(inst.ProviderInstallationID) == "" {
		return false
	}
	if strings.TrimSpace(inst.AdapterID) == "" {
		return false
	}
	if inst.Confidence != ConfidenceExact {
		return false
	}
	if inst.FreshnessState != FreshnessFresh {
		return false
	}
	if strings.TrimSpace(inst.ExecutableIdentity.ResolvedPathHash) == "" {
		return false
	}
	return true
}

// durableToLiveInstallAliasMap maps durable ProviderInstallationIDs onto the
// live PATH-primary installation when adapter + exact ResolvedPathHash matches
// at least one live target. Primary = lowest DiscoveryOrder among live aliases
// (LookPath / first PATH hit); secondary live aliases are not independent
// translation targets. Distinct ResolvedPathHash values never map together.
// Does not promote durable installs as live-installed.
func durableToLiveInstallAliasMap(live, durable []ProviderInstallation) map[string]string {
	// resolvedKey → live installs eligible for alias (id + discovery order)
	type liveMeta struct {
		id    string
		order int
	}
	liveByKey := map[string][]liveMeta{}
	liveSeen := map[string]map[string]bool{}
	for _, inst := range live {
		if !exactFreshInstallIdentity(inst) {
			continue
		}
		adapter := strings.ToLower(strings.TrimSpace(inst.AdapterID))
		resolved := strings.TrimSpace(inst.ExecutableIdentity.ResolvedPathHash)
		key := adapter + "|" + resolved
		id := strings.TrimSpace(inst.ProviderInstallationID)
		if liveSeen[key] == nil {
			liveSeen[key] = map[string]bool{}
		}
		if liveSeen[key][id] {
			continue
		}
		liveSeen[key][id] = true
		liveByKey[key] = append(liveByKey[key], liveMeta{id: id, order: inst.DiscoveryOrder})
	}
	// PATH primary per key: earliest DiscoveryOrder, then stable id.
	primaryByKey := map[string]string{}
	for k, members := range liveByKey {
		sort.SliceStable(members, func(i, j int) bool {
			if members[i].order != members[j].order {
				return members[i].order < members[j].order
			}
			return members[i].id < members[j].id
		})
		primaryByKey[k] = members[0].id
	}

	out := map[string]string{}
	for _, inst := range durable {
		if !exactFreshInstallIdentity(inst) {
			continue
		}
		adapter := strings.ToLower(strings.TrimSpace(inst.AdapterID))
		resolved := strings.TrimSpace(inst.ExecutableIdentity.ResolvedPathHash)
		key := adapter + "|" + resolved
		durID := strings.TrimSpace(inst.ProviderInstallationID)
		primary, ok := primaryByKey[key]
		if !ok || primary == "" {
			// 0 live targets: no translation.
			continue
		}
		if durID == primary {
			continue
		}
		out[durID] = primary
	}
	return out
}

func rewriteQuotaInstallIDs(snaps []QuotaSnapshot, alias map[string]string) []QuotaSnapshot {
	if len(alias) == 0 {
		return snaps
	}
	out := make([]QuotaSnapshot, len(snaps))
	copy(out, snaps)
	for i := range out {
		if out[i].ProviderInstallationID == nil {
			continue
		}
		id := strings.TrimSpace(*out[i].ProviderInstallationID)
		if live, ok := alias[id]; ok && live != "" {
			v := live
			out[i].ProviderInstallationID = &v
		}
	}
	return out
}

func rewriteAuthInstallIDs(auths []AuthReadiness, alias map[string]string) []AuthReadiness {
	if len(alias) == 0 {
		return auths
	}
	out := make([]AuthReadiness, len(auths))
	copy(out, auths)
	for i := range out {
		if out[i].ProviderInstallationID == nil {
			continue
		}
		id := strings.TrimSpace(*out[i].ProviderInstallationID)
		if live, ok := alias[id]; ok && live != "" {
			v := live
			out[i].ProviderInstallationID = &v
		}
	}
	return out
}

func rewriteCatalogInstallIDs(snaps []ModelCatalogSnapshot, alias map[string]string) []ModelCatalogSnapshot {
	if len(alias) == 0 {
		return snaps
	}
	out := make([]ModelCatalogSnapshot, len(snaps))
	copy(out, snaps)
	for i := range out {
		if out[i].ProviderInstallationID == nil {
			continue
		}
		id := strings.TrimSpace(*out[i].ProviderInstallationID)
		if live, ok := alias[id]; ok && live != "" {
			v := live
			out[i].ProviderInstallationID = &v
		}
	}
	return out
}

func rehydrateCatalogSnapshots(live, durable []ModelCatalogSnapshot) []ModelCatalogSnapshot {
	if len(durable) == 0 {
		return live
	}
	hasLiveAdapter := map[string]bool{}
	for _, s := range live {
		if s.CatalogSourceKind == CatalogSourceProviderMachineReadable &&
			s.Confidence == ConfidenceExact &&
			s.FreshnessState == FreshnessFresh &&
			s.EntryCount > 0 {
			hasLiveAdapter[strings.ToLower(strings.TrimSpace(s.AdapterID))] = true
		}
	}
	out := append([]ModelCatalogSnapshot(nil), live...)
	for _, d := range durable {
		if d.FreshnessState == FreshnessStale || d.FreshnessState == FreshnessExpired {
			continue
		}
		if d.CatalogSourceKind != CatalogSourceProviderMachineReadable {
			continue
		}
		if d.Confidence != ConfidenceExact {
			continue
		}
		ad := strings.ToLower(strings.TrimSpace(d.AdapterID))
		if hasLiveAdapter[ad] {
			continue
		}
		out = append(out, d)
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

// DurableAuthRecoveryHorizon bounds how long a stored AuthReadiness may be
// recovered. Aligned with provider auth probe StaleAfter (30m). Load does not
// recompute auth FreshnessState, so CapturedAt age is the recovery clock.
const DurableAuthRecoveryHorizon = 30 * time.Minute

// authCaptureFutureSkew tolerates small clock skew; CapturedAt beyond this
// ahead of now is treated as materially future and not recoverable.
const authCaptureFutureSkew = 2 * time.Minute

func authAccountInstallKey(a AuthReadiness) string {
	acc := ""
	if a.AccountProfileID != nil {
		acc = strings.TrimSpace(*a.AccountProfileID)
	}
	inst := ""
	if a.ProviderInstallationID != nil {
		inst = strings.TrimSpace(*a.ProviderInstallationID)
	}
	return strings.ToLower(strings.TrimSpace(a.AdapterID)) + "|" + acc + "|" + inst
}

func authAdapterInstallKey(a AuthReadiness) string {
	inst := ""
	if a.ProviderInstallationID != nil {
		inst = strings.TrimSpace(*a.ProviderInstallationID)
	}
	return strings.ToLower(strings.TrimSpace(a.AdapterID)) + "|" + inst
}

// exactFreshAuthObservation is live dominance: exact Confidence, exact
// ReadinessConfidence, and FreshnessFresh — for any ReadinessState (Ready,
// NotAuthenticated, Expired, unknown, …).
func exactFreshAuthObservation(a AuthReadiness) bool {
	return a.Confidence == ConfidenceExact &&
		a.ReadinessConfidence == ConfidenceExact &&
		a.FreshnessState == FreshnessFresh
}

func parseAuthCapturedAt(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func withinDurableAuthRecoveryHorizon(captured, now time.Time) bool {
	now = now.UTC()
	captured = captured.UTC()
	if captured.After(now.Add(authCaptureFutureSkew)) {
		return false
	}
	if now.Sub(captured) > DurableAuthRecoveryHorizon {
		return false
	}
	return true
}

// authTruthSignature compares equal-time durable rows for conflict. Differing
// readiness state, confidence, readiness confidence, or freshness ⇒ fail closed.
func authTruthSignature(a AuthReadiness) string {
	return string(a.ReadinessState) + "|" +
		string(a.Confidence) + "|" +
		string(a.ReadinessConfidence) + "|" +
		string(a.FreshnessState)
}

// rehydrateAuth recovers a sole recent durable exact+fresh Ready auth when live
// has no exact+fresh observation on that adapter+install.
//
// Algorithm (fail closed):
//  1. Live exact+fresh (both confidences) any state on adapter+install blocks
//     all durable recovery for that install.
//  2. Group ALL durable rows by adapter+account+translated-install (do not
//     filter to Ready first).
//  3. Per identity: any empty/unparseable CapturedAt ⇒ non-recoverable (do not
//     fall back to an older Ready). Among parseable rows, take max CapturedAt;
//     equal-time rows with differing state/confidence/freshness ⇒ block.
//  4. Only the latest row may then pass ExactFreshReadyAuth + recency
//     (CapturedAt age ≤ DurableAuthRecoveryHorizon, not materially future).
//  5. Build candidates independently of live row iteration (live may be empty).
//  6. Group recoverable Ready candidates by adapter+install; recover only when
//     exactly one distinct account (multi-account ⇒ recover none).
func rehydrateAuth(live, durable []AuthReadiness, now time.Time) []AuthReadiness {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	out := append([]AuthReadiness(nil), live...)
	if len(durable) == 0 {
		return out
	}

	// Live dominate: exact+fresh observation of any readiness state blocks
	// durable recovery on that adapter+install.
	liveBlockedAI := map[string]bool{}
	for _, a := range live {
		if exactFreshAuthObservation(a) {
			liveBlockedAI[authAdapterInstallKey(a)] = true
		}
	}

	// Group ALL durable rows by full identity (adapter|account|install).
	groups := map[string][]AuthReadiness{}
	for _, d := range durable {
		k := authAccountInstallKey(d)
		groups[k] = append(groups[k], d)
	}

	type cand struct {
		auth AuthReadiness
		acc  string
		ai   string
	}
	// Candidates built from durable resolution only; live emptiness is OK.
	var candidates []cand

	idKeys := make([]string, 0, len(groups))
	for k := range groups {
		idKeys = append(idKeys, k)
	}
	sort.Strings(idKeys)

	for _, idKey := range idKeys {
		rows := groups[idKey]
		type timed struct {
			auth AuthReadiness
			at   time.Time
		}
		timedRows := make([]timed, 0, len(rows))
		identityBlocked := false
		for _, r := range rows {
			at, ok := parseAuthCapturedAt(r.CapturedAt)
			if !ok {
				// Empty/invalid CapturedAt: identity non-recoverable. A newer
				// unparseable stamp must not silently expose an older Ready.
				identityBlocked = true
				break
			}
			timedRows = append(timedRows, timed{auth: r, at: at})
		}
		if identityBlocked || len(timedRows) == 0 {
			continue
		}

		maxAt := timedRows[0].at
		for _, tr := range timedRows[1:] {
			if tr.at.After(maxAt) {
				maxAt = tr.at
			}
		}
		var latest []AuthReadiness
		for _, tr := range timedRows {
			if tr.at.Equal(maxAt) {
				latest = append(latest, tr.auth)
			}
		}
		sig0 := authTruthSignature(latest[0])
		conflict := false
		for _, a := range latest[1:] {
			if authTruthSignature(a) != sig0 {
				conflict = true
				break
			}
		}
		if conflict {
			continue
		}
		chosen := latest[0]

		// Only after latest-state resolution: exact+fresh Ready + recency.
		if !ExactFreshReadyAuth(chosen) {
			continue
		}
		if !withinDurableAuthRecoveryHorizon(maxAt, now) {
			continue
		}
		acc := ""
		if chosen.AccountProfileID != nil {
			acc = strings.TrimSpace(*chosen.AccountProfileID)
		}
		if acc == "" {
			continue
		}
		ai := authAdapterInstallKey(chosen)
		if liveBlockedAI[ai] {
			continue
		}
		candidates = append(candidates, cand{auth: chosen, acc: acc, ai: ai})
	}

	// One distinct recoverable account per adapter+install.
	byAI := map[string][]cand{}
	for _, c := range candidates {
		byAI[c.ai] = append(byAI[c.ai], c)
	}
	aiKeys := make([]string, 0, len(byAI))
	for ai := range byAI {
		aiKeys = append(aiKeys, ai)
	}
	sort.Strings(aiKeys)
	for _, ai := range aiKeys {
		list := byAI[ai]
		byAcc := map[string]AuthReadiness{}
		for _, c := range list {
			if _, ok := byAcc[c.acc]; !ok {
				byAcc[c.acc] = c.auth
			}
		}
		if len(byAcc) != 1 {
			continue
		}
		accKeys := make([]string, 0, 1)
		for acc := range byAcc {
			accKeys = append(accKeys, acc)
		}
		sort.Strings(accKeys)
		out = append(out, byAcc[accKeys[0]])
	}
	return out
}

// machineReadableExactFreshSource mirrors capacitysnapshot.capabilityIsDynamicExactFresh
// for rehydrate gate decisions (keep source priority identical; do not fork).
//
// EntrySources is the authoritative production-route gate when present. A row
// with EntrySources=[adapter-declared] must NOT become MR truth even if
// top-level Source.Kind claims machine-readable. Source.Kind fallback applies
// only when len(EntrySources)==0 and Confidence/Freshness are exact+fresh.
func machineReadableExactFreshSource(m ModelCapability) bool {
	for _, s := range m.EntrySources {
		if s.SourceKind != CatalogSourceProviderMachineReadable {
			continue
		}
		if s.Confidence != ConfidenceExact {
			continue
		}
		if s.FreshnessState != FreshnessFresh {
			continue
		}
		return true
	}
	if len(m.EntrySources) == 0 &&
		(m.Source.Kind == string(CatalogSourceProviderMachineReadable) ||
			m.Source.Kind == "provider-machine-readable") &&
		m.Confidence == ConfidenceExact &&
		m.FreshnessState == FreshnessFresh {
		return true
	}
	return false
}

// productionRoutableMRModel is the full "may stand as production model truth"
// predicate used both for live has-MR skip decisions and durable overlay.
//
// Requires:
//  1. machineReadableExactFreshSource (EntrySources authority; Source.Kind
//     fallback only when EntrySources empty) — same as capabilityIsDynamicExactFresh
//  2. present semantics aligned with FromProviderInventoryReport: nonempty
//     CanonicalModelID, AvailabilityAvailable, not removed/deprecated, and
//     top-level FreshnessState neither stale nor expired.
//
// Live rows that are MR-sourced but not present (empty id, unavailable, removed,
// stale/expired) must NOT block durable exact+fresh MR overlay.
func productionRoutableMRModel(m ModelCapability) bool {
	if !machineReadableExactFreshSource(m) {
		return false
	}
	if strings.TrimSpace(m.CanonicalModelID) == "" {
		return false
	}
	if m.AvailabilityState != AvailabilityAvailable {
		return false
	}
	if m.LifecycleState == LifecycleRemoved || m.LifecycleState == LifecycleDeprecated {
		return false
	}
	if m.FreshnessState == FreshnessStale || m.FreshnessState == FreshnessExpired {
		return false
	}
	return true
}

func rehydrateModels(live, durable []ModelCapability) []ModelCapability {
	if len(durable) == 0 {
		return live
	}
	// Skip durable only when live already has a present production-routable MR
	// model for that adapter. Unroutable live rows (empty id, unavailable,
	// removed, stale) must not suppress durable exact+fresh MR.
	hasLiveProductionMR := map[string]bool{}
	for _, m := range live {
		if productionRoutableMRModel(m) {
			ad := strings.ToLower(strings.TrimSpace(m.AdapterID))
			hasLiveProductionMR[ad] = true
		}
	}
	out := append([]ModelCapability(nil), live...)
	for _, m := range durable {
		ad := strings.ToLower(strings.TrimSpace(m.AdapterID))
		if hasLiveProductionMR[ad] {
			continue
		}
		if !productionRoutableMRModel(m) {
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
