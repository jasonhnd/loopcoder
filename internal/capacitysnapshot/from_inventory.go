package capacitysnapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// capabilityIsDynamicExactFresh reports a production-routable catalog row.
// Fail closed: requires explicit source_kind=provider-machine-readable AND
// confidence=exact AND freshness=fresh. Empty, unknown, estimated, stale, or
// adapter-declared rows are never routable (no backward-compat empty fields).
func capabilityIsDynamicExactFresh(m providerinventory.ModelCapability) bool {
	// EntrySources is the authoritative production-route gate when present.
	for _, s := range m.EntrySources {
		if s.SourceKind != providerinventory.CatalogSourceProviderMachineReadable {
			continue
		}
		if s.Confidence != providerinventory.ConfidenceExact {
			continue
		}
		if s.FreshnessState != providerinventory.FreshnessFresh {
			continue
		}
		return true
	}
	// Partial rows with empty EntrySources: only when top-level Source.Kind,
	// Confidence, and FreshnessState are all explicit machine-readable/exact/fresh.
	if len(m.EntrySources) == 0 &&
		(m.Source.Kind == string(providerinventory.CatalogSourceProviderMachineReadable) ||
			m.Source.Kind == "provider-machine-readable") &&
		m.Confidence == providerinventory.ConfidenceExact &&
		m.FreshnessState == providerinventory.FreshnessFresh {
		return true
	}
	return false
}

// opaqueAccountRef returns the exact opaque account binding for a full non-secret
// account profile id. Never truncates. Empty → "" (unknown / non-routable).
// Full "acct-"+64hex is preserved; otherwise SHA-256 of the full material.
func opaqueAccountRef(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "acct-") && len(raw) == 5+64 && isHexLowerOrUpper(raw[5:]) {
		return strings.ToLower(raw)
	}
	sum := sha256.Sum256([]byte("acct|" + raw))
	return "acct-" + hex.EncodeToString(sum[:])
}

func isHexLowerOrUpper(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return len(s) > 0
}

// exactInstallRef returns the full ProviderInstallationID with no truncation.
func exactInstallRef(id string) string {
	return strings.TrimSpace(id)
}

// accountSegmentFromScope extracts the exact "account:" segment from a ScopeKey
// such as "provider:grok/account:X/detail:credits_usage". Never hashes the whole scope.
// Sentinel tokens (unknown / empty / root) are rejected so they cannot invent a
// fake AccountRef that splits capacity from auth (RC36 codex account:unknown).
func accountSegmentFromScope(scopeKey string) string {
	scopeKey = strings.TrimSpace(scopeKey)
	if scopeKey == "" {
		return ""
	}
	for _, part := range strings.Split(scopeKey, "/") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "account:") {
			acc := strings.TrimSpace(strings.TrimPrefix(part, "account:"))
			if acc == "" || isSentinelAccountToken(acc) {
				return ""
			}
			return opaqueAccountRef(acc)
		}
	}
	return ""
}

func isSentinelAccountToken(acc string) bool {
	switch strings.ToLower(strings.TrimSpace(acc)) {
	case "unknown", "root", "account", "none", "null", "nil":
		return true
	default:
		return false
	}
}

// FromProviderInventoryReport maps a live providerinventory.Report into AccountObservations.
//
// Capacity honesty rules (must never be weakened):
//   - missing/unsupported quota stays unknown — never invent 0, full, or unlimited
//   - auth-ready and live model catalog are not capacity evidence
//   - static model seed is estimated catalog only and does not create unattended eligibility
//   - unattended eligibility still requires a real fresh usable capacity window
//   - NEVER invent "acct-"+provider / "install-"+provider / "account-unknown"
//   - group by provider × account × install (not collapse all accounts of one provider)
//   - Join only through explicit provider/account/install IDs; ScopeKey only as
//     validated fallback extracting the exact account: segment (never whole scope)
//   - No truncation of AccountRef / InstallRef / ProviderInstallationID
//   - Deterministic joins (sorted keys); unknown joins stay unknown/non-routable
//   - After grouping, associateIdentityEvidence coalesces path aliases (same
//     adapter + ResolvedPathHash) and rebinds empty-account capacity to the sole
//     account on that install; ambiguous multi-account stays fail-closed.
func FromProviderInventoryReport(rep providerinventory.Report, now time.Time) []AccountObservation {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	type builder struct {
		in AccountInput
	}
	by := map[string]*builder{}
	groupKey := func(provider, account, install string) string {
		return strings.ToLower(strings.TrimSpace(provider)) + "|" +
			strings.TrimSpace(account) + "|" + strings.TrimSpace(install)
	}
	ensure := func(provider, account, install string) *builder {
		provider = strings.ToLower(strings.TrimSpace(provider))
		account = strings.TrimSpace(account)
		install = strings.TrimSpace(install)
		if provider == "" {
			provider = "unknown"
		}
		k := groupKey(provider, account, install)
		if b, ok := by[k]; ok {
			return b
		}
		b := &builder{in: AccountInput{
			Provider:         provider,
			AccountRef:       account,
			InstallRef:       install,
			HealthConfidence: ConfidenceUnknown,
			HealthFreshness:  FreshnessUnknown,
			Source:           "providerinventory",
			CapturedAt:       now,
			Provenance:       "inventory_fingerprint=" + rep.InventoryFingerprint,
		}}
		by[k] = b
		return b
	}
	// Full install identity — never shortRef / never invent install-+provider.
	installRefOf := func(inst providerinventory.ProviderInstallation) string {
		return exactInstallRef(inst.ProviderInstallationID)
	}
	// Explicit install-id → sorted list of group keys for deterministic rebind.
	installKeys := map[string][]string{} // installRef -> group keys
	// installInstalled carries InstallationInstalled presence by full install id
	// so every account bound to that install inherits Installed (multi-account
	// same PATH-primary CLI). Does not invent installs or relax path-plan gates.
	installInstalled := map[string]bool{}

	// Authoritative PATH LookPath plan: only true primary (and its PATH aliases
	// for identity repair) may complete unattended production eligibility.
	// Explicit-config and failed LookPath-first later PATH rows stay observable
	// but non-production-routable.
	pathPlan := computePathInstallPlan(rep.Installations)

	// Sort installations for deterministic processing.
	installs := append([]providerinventory.ProviderInstallation(nil), rep.Installations...)
	sort.SliceStable(installs, func(i, j int) bool {
		return installs[i].ProviderInstallationID < installs[j].ProviderInstallationID
	})
	for _, inst := range installs {
		iref := installRefOf(inst)
		b := ensure(inst.AdapterID, "", iref)
		prod := productionInstallEligible(pathPlan, iref)
		if inst.InstallationState == providerinventory.InstallationInstalled {
			// Observable install presence for all sources; production routing
			// still requires Authenticated+Healthy which are path-plan gated.
			b.in.Installed = true
			if iref != "" {
				installInstalled[iref] = true
			}
		}
		if inst.FreshnessState == providerinventory.FreshnessFresh {
			b.in.HealthFreshness = FreshnessFresh
		} else if inst.FreshnessState == providerinventory.FreshnessStale {
			b.in.HealthFreshness = FreshnessStale
		}
		if conf := mapPIConfidence(inst.Confidence); conf != ConfidenceUnknown {
			b.in.HealthConfidence = conf
		}
		if prod && (inst.UsableForInvocation == "yes" || inst.UsableForInvocation == "true") {
			b.in.Healthy = true
		} else if inst.UsableForInvocation == "yes" || inst.UsableForInvocation == "true" {
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; install_usable_non_production_routable source=" + string(inst.DiscoverySource))
		}
		if iref != "" {
			b.in.InstallRef = iref
			k := groupKey(b.in.Provider, b.in.AccountRef, iref)
			installKeys[iref] = append(installKeys[iref], k)
		}
		if !prod && inst.DiscoverySource == providerinventory.DiscoveryExplicitConfig {
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; discovery_source=explicit-config non_production_routable")
		}
	}

	// Auth readiness: bind by explicit AccountProfileID only (full opaque, no truncate).
	// Link to install only when Auth has explicit ProviderInstallationID.
	auths := append([]providerinventory.AuthReadiness(nil), rep.AuthReadiness...)
	sort.SliceStable(auths, func(i, j int) bool {
		ai, aj := "", ""
		if auths[i].AccountProfileID != nil {
			ai = *auths[i].AccountProfileID
		}
		if auths[j].AccountProfileID != nil {
			aj = *auths[j].AccountProfileID
		}
		if ai != aj {
			return ai < aj
		}
		return auths[i].AdapterID < auths[j].AdapterID
	})
	// account → install explicit links from auth when present.
	accountToInstalls := map[string][]string{} // accountRef -> installRefs
	for _, auth := range auths {
		acc := ""
		if auth.AccountProfileID != nil {
			acc = opaqueAccountRef(*auth.AccountProfileID)
		}
		iref := ""
		if auth.ProviderInstallationID != nil {
			iref = exactInstallRef(*auth.ProviderInstallationID)
		}
		// Only join through explicit install ID. Never first-map-iteration merge.
		b := ensure(auth.AdapterID, acc, iref)
		if acc != "" && iref != "" {
			// Re-key install-only row into account+install if present.
			emptyKey := groupKey(strings.ToLower(strings.TrimSpace(auth.AdapterID)), "", iref)
			if existing, ok := by[emptyKey]; ok && emptyKey != groupKey(b.in.Provider, acc, iref) {
				existing.in.AccountRef = acc
				delete(by, emptyKey)
				nk := groupKey(existing.in.Provider, acc, iref)
				by[nk] = existing
				b = existing
			}
			accountToInstalls[acc] = appendUniqueSorted(accountToInstalls[acc], iref)
		}
		// Every account bound to an installed install inherits Installed presence
		// (same PATH-primary CLI, distinct AccountRefs). No empty-install invent.
		if iref != "" && installInstalled[iref] {
			b.in.Installed = true
		}
		if productionRoutableAuth(auth) {
			// Auth may complete production eligibility only for LookPath-primary
			// (or PATH aliases that fuse onto it). Explicit-config never donates.
			if productionInstallEligible(pathPlan, iref) {
				b.in.Authenticated = true
				b.in.Healthy = true
				b.in.HealthFreshness = FreshnessFresh
				b.in.HealthConfidence = ConfidenceExact
			} else {
				b.in.Provenance = strings.TrimSpace(b.in.Provenance +
					"; auth_exact_ready_non_production_install install=" + iref)
			}
		} else if auth.ReadinessState == providerinventory.ReadinessReady {
			// Non-production Ready (stale/estimated/unknown): keep evidence but
			// never mark Authenticated for unattended routing.
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; auth_ready_not_production_routable confidence=" + string(auth.Confidence) +
				" readiness_confidence=" + string(auth.ReadinessConfidence) +
				" freshness=" + string(auth.FreshnessState))
		}
		if acc != "" {
			b.in.AccountRef = acc
		}
		if iref != "" {
			b.in.InstallRef = iref
		}
	}

	// Dynamic catalog: bind by installation/account scope when ModelCapability
	// carries explicit IDs; otherwise replicate only to explicitly linked accounts.
	snapshotByID := make(map[string]providerinventory.ModelCatalogSnapshot, len(rep.ModelCatalogSnapshots))
	for _, snapshot := range rep.ModelCatalogSnapshots {
		snapshotByID[snapshot.ModelCatalogSnapshotID] = snapshot
	}
	dynamicCapability := func(m providerinventory.ModelCapability) bool {
		if !capabilityIsDynamicExactFresh(m) {
			return false
		}
		if !strings.EqualFold(strings.TrimSpace(m.AdapterID), "claude") {
			return true
		}
		snapshot, ok := snapshotByID[m.ModelCatalogSnapshotID]
		return ok && providerinventory.ValidClaudeVerifiedCapability(snapshot, m, now)
	}
	liveModels := map[string]bool{}
	dynamicExactAdapters := map[string]bool{}
	for _, m := range rep.ModelCapabilities {
		if dynamicCapability(m) {
			dynamicExactAdapters[strings.ToLower(strings.TrimSpace(m.AdapterID))] = true
		}
	}
	for _, snap := range rep.ModelCatalogSnapshots {
		if snap.CatalogSourceKind == providerinventory.CatalogSourceProviderMachineReadable &&
			snap.Confidence == providerinventory.ConfidenceExact &&
			snap.FreshnessState == providerinventory.FreshnessFresh &&
			(!strings.EqualFold(strings.TrimSpace(snap.AdapterID), "claude") ||
				providerinventory.ValidClaudeVerifiedSnapshot(snap, now)) {
			dynamicExactAdapters[strings.ToLower(strings.TrimSpace(snap.AdapterID))] = true
		}
	}
	// modelIdx is per-builder (provider|account|install|model), not provider-global.
	modelIdx := map[string]int{}
	caps := append([]providerinventory.ModelCapability(nil), rep.ModelCapabilities...)
	sort.SliceStable(caps, func(i, j int) bool {
		di, dj := dynamicCapability(caps[i]), dynamicCapability(caps[j])
		if di != dj {
			return di && !dj
		}
		if caps[i].AdapterID != caps[j].AdapterID {
			return caps[i].AdapterID < caps[j].AdapterID
		}
		return caps[i].CanonicalModelID < caps[j].CanonicalModelID
	})
	// Collect builder keys sorted for deterministic replication targets.
	builderKeysForProvider := func(adapter string) []string {
		adapter = strings.ToLower(strings.TrimSpace(adapter))
		var keys []string
		for k, b := range by {
			if strings.EqualFold(b.in.Provider, adapter) && strings.TrimSpace(b.in.AccountRef) != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		return keys
	}
	attachModel := func(b *builder, m providerinventory.ModelCapability, dynamic, hintOnly bool, spec modelDepthSpec) {
		if b == nil {
			return
		}
		key := groupKey(b.in.Provider, b.in.AccountRef, b.in.InstallRef) + "|" + strings.ToLower(spec.ModelID)
		if idx, ok := modelIdx[key]; ok && idx >= 0 && idx < len(b.in.Models) {
			mergeModelSpec(&b.in.Models[idx], spec)
			if hintOnly {
				b.in.Models[idx].CatalogHintOnly = true
			}
		} else {
			modelIdx[key] = len(b.in.Models)
			b.in.Models = append(b.in.Models, ModelSpec{
				ModelID: spec.ModelID, Present: true,
				SupportedDepths: append([]string(nil), spec.SupportedDepths...),
				DefaultDepth:    spec.DefaultDepth,
				CatalogHintOnly: hintOnly,
			})
		}
		if !hintOnly {
			liveKey := strings.ToLower(m.AdapterID) + "|" + strings.ToLower(spec.ModelID)
			liveModels[liveKey] = true
			liveModels[strings.ToLower(m.AdapterID)+"|"+strings.ToLower(m.CanonicalModelID)] = true
			if slug := slugifyModelName(spec.ModelID); slug != "" {
				liveModels[strings.ToLower(m.AdapterID)+"|"+slug] = true
			}
		}
	}
	for _, m := range caps {
		adapterKey := strings.ToLower(strings.TrimSpace(m.AdapterID))
		dynamic := dynamicCapability(m)
		if dynamicExactAdapters[adapterKey] && !dynamic {
			// Suppress static for adapters with dynamic catalog (record on any linked row).
			for _, k := range builderKeysForProvider(m.AdapterID) {
				if b := by[k]; b != nil {
					b.in.Provenance = strings.TrimSpace(b.in.Provenance +
						"; adapter_declared_static_suppressed=dynamic_machine_readable_present")
				}
			}
			continue
		}
		present := m.CanonicalModelID != "" &&
			m.AvailabilityState == providerinventory.AvailabilityAvailable &&
			m.LifecycleState != providerinventory.LifecycleRemoved &&
			m.LifecycleState != providerinventory.LifecycleDeprecated
		if m.FreshnessState == providerinventory.FreshnessExpired ||
			m.FreshnessState == providerinventory.FreshnessStale {
			present = false
		}
		hintOnly := !dynamic
		spec := modelSpecFromCapability(m.AdapterID, m, dynamic)
		if present && strings.EqualFold(m.AdapterID, "codex") &&
			(strings.EqualFold(m.CanonicalModelID, "gpt-5.3-codex") || strings.EqualFold(spec.ModelID, "gpt-5.3-codex")) {
			present = false
			// Provenance recorded on all builders for this adapter after loop via
			// targets; mark here on a temporary provider row so tests observe it.
			tmp := ensure(m.AdapterID, "", "")
			tmp.in.Provenance = strings.TrimSpace(tmp.in.Provenance +
				"; model_excluded=gpt-5.3-codex reason=chatgpt_account_incompatible")
		}
		if !present || strings.TrimSpace(spec.ModelID) == "" {
			continue
		}
		// ModelCapability has no install/account fields; bind via catalog snapshot
		// when ModelCatalogSnapshotID is known, else replicate only to explicitly
		// linked accounts for this adapter (deterministic sorted keys).
		var targets []*builder
		var snapInstall, snapAccount string
		for _, snap := range rep.ModelCatalogSnapshots {
			if snap.ModelCatalogSnapshotID != m.ModelCatalogSnapshotID {
				continue
			}
			if snap.ProviderInstallationID != nil {
				snapInstall = exactInstallRef(*snap.ProviderInstallationID)
			}
			if snap.AccountProfileID != nil {
				snapAccount = opaqueAccountRef(*snap.AccountProfileID)
			}
			break
		}
		if snapInstall != "" || snapAccount != "" {
			if snapAccount != "" && snapInstall != "" {
				targets = append(targets, ensure(m.AdapterID, snapAccount, snapInstall))
			} else if snapAccount != "" {
				if irefs := accountToInstalls[snapAccount]; len(irefs) > 0 {
					for _, ir := range irefs {
						targets = append(targets, ensure(m.AdapterID, snapAccount, ir))
					}
				} else {
					targets = append(targets, ensure(m.AdapterID, snapAccount, ""))
				}
			} else {
				// Install-scoped: only accounts explicitly linked to this install.
				for accRef, irefs := range accountToInstalls {
					for _, ir := range irefs {
						if ir == snapInstall {
							targets = append(targets, ensure(m.AdapterID, accRef, snapInstall))
						}
					}
				}
				if len(targets) == 0 {
					targets = append(targets, ensure(m.AdapterID, "", snapInstall))
				}
			}
		} else {
			for _, k := range builderKeysForProvider(m.AdapterID) {
				targets = append(targets, by[k])
			}
			if len(targets) == 0 {
				targets = append(targets, ensure(m.AdapterID, "", ""))
			}
		}
		for _, b := range targets {
			if b == nil {
				continue
			}
			if hintOnly {
				b.in.Provenance = strings.TrimSpace(b.in.Provenance +
					"; catalog_hint_only=non_machine_readable model=" + spec.ModelID)
			}
			attachModel(b, m, dynamic, hintOnly, spec)
		}
	}

	// Quota: join via AccountProfileID + ProviderInstallationID only.
	// ScopeKey fallback extracts exact account: segment — never whole-scope hash.
	quotas := append([]providerinventory.QuotaSnapshot(nil), rep.QuotaSnapshots...)
	sort.SliceStable(quotas, func(i, j int) bool {
		return quotas[i].QuotaSnapshotID < quotas[j].QuotaSnapshotID
	})
	for _, q := range quotas {
		provider := strings.ToLower(strings.TrimSpace(q.AdapterID))
		acc := ""
		if q.AccountProfileID != nil {
			acc = opaqueAccountRef(*q.AccountProfileID)
		}
		if acc == "" {
			acc = accountSegmentFromScope(q.ScopeKey)
		}
		iref := ""
		if q.ProviderInstallationID != nil {
			iref = exactInstallRef(*q.ProviderInstallationID)
		}
		// Unknown join stays unknown/non-routable — do not first-map-pick another account.
		b := ensure(provider, acc, iref)
		if q.Confidence == providerinventory.ConfidenceUnavailable ||
			q.FreshnessState == providerinventory.FreshnessNotApplicable {
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; quota_observation=unsupported_or_unavailable")
			continue
		}
		if q.RemainingValue == nil && q.UsedValue == nil && q.LimitValue == nil {
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; quota_observation=no_numeric_fields")
			continue
		}
		unit := mapUnit(q.Unit)
		if unit == UnitUnknown {
			unit = unitFromQuantityKind(string(q.QuantityKind))
		}
		// Apply ValueScale before typed quantities (e.g. Grok scale=2 stores
		// hundredths of a percent: 6900 → 69.00, never 6900%).
		scale := q.ValueScale
		if scale < 0 {
			scale = 0
		}
		w := Window{
			Kind:       string(q.WindowKind),
			Unit:       unit,
			Used:       qtyFromPtrScaled(q.UsedValue, scale, unit),
			Remaining:  qtyFromPtrScaled(q.RemainingValue, scale, unit),
			Limit:      qtyFromPtrScaled(q.LimitValue, scale, unit),
			Confidence: mapPIConfidence(q.Confidence),
			Freshness:  mapPIFreshness(q.FreshnessState),
			Source:     string(q.SourceKind),
			CapturedAt: parseTimeOr(q.CapturedAt, now),
		}
		if q.ResetAt != "" {
			if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil {
				tt := t.UTC()
				w.ResetAt = &tt
			}
		}
		// StaleAfter must not outlive an earlier ResetAt: if provider reports
		// StaleAfter after ResetAt, clamp freshness via reset boundary.
		if q.StaleAfter != "" && w.ResetAt != nil {
			if st, err := time.Parse(time.RFC3339, q.StaleAfter); err == nil {
				st = st.UTC()
				if st.After(*w.ResetAt) {
					// Observation cannot remain "fresh" past reset.
					if w.Freshness == FreshnessFresh && now.After(*w.ResetAt) {
						w.Freshness = FreshnessStale
					}
					// Provenance note only — Window has no StaleAfter field.
					b.in.Provenance = strings.TrimSpace(b.in.Provenance +
						"; stale_after_clamped_to_reset_at")
				}
			}
		}
		b.in.Windows = append(b.in.Windows, w)
	}

	// Deterministic output order: sort keys.
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]AccountObservation, 0, len(keys))
	for _, k := range keys {
		b := by[k]
		if b.in.Installed && b.in.Authenticated && b.in.Healthy && b.in.HealthFreshness == FreshnessUnknown {
			b.in.HealthFreshness = FreshnessFresh
			if b.in.HealthConfidence == ConfidenceUnknown {
				b.in.HealthConfidence = ConfidenceEstimated
			}
		}
		if !hasProductionRoutableModel(b.in.Models) {
			seedStaticModelsEstimated(&b.in, liveModels)
		}
		if b.in.Provider == "codex" && len(b.in.Models) > 1 {
			preferCodexDefaultModel(&b.in)
		}
		out = append(out, FromAccountInput(b.in))
	}
	// Path-alias + empty-account reassociation (fail closed on ambiguity).
	return associateIdentityEvidence(out, rep.Installations)
}

func appendUniqueSorted(ss []string, v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ss
	}
	for _, s := range ss {
		if s == v {
			return ss
		}
	}
	ss = append(ss, v)
	sort.Strings(ss)
	return ss
}

// hasProductionRoutableModel reports a present, non-hint catalog model.
func hasProductionRoutableModel(models []ModelSpec) bool {
	for _, m := range models {
		if m.Present && !m.CatalogHintOnly && strings.TrimSpace(m.ModelID) != "" {
			return true
		}
	}
	return false
}

// preferCodexDefaultModel puts present gpt-5.5 first when available.
func preferCodexDefaultModel(in *AccountInput) {
	if in == nil || len(in.Models) < 2 {
		return
	}
	want := "gpt-5.5"
	idx := -1
	for i, m := range in.Models {
		if strings.EqualFold(m.ModelID, want) && m.Present {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return
	}
	m := in.Models[idx]
	copy(in.Models[1:idx+1], in.Models[0:idx])
	in.Models[0] = m
}

// seedStaticModelsEstimated adds static registry models as estimated catalog
// hints only (CatalogHintOnly=true). Does not invent capacity, does not claim
// live freshness, and must never enter production auto-route — even when the
// account has real quota windows.
func seedStaticModelsEstimated(in *AccountInput, live map[string]bool) {
	if in == nil {
		return
	}
	p, ok := models.LookupProvider(in.Provider)
	if !ok || len(p.Models) == 0 {
		return
	}
	for _, m := range p.Models {
		key := strings.ToLower(in.Provider) + "|" + strings.ToLower(m.Name)
		if live[key] {
			continue
		}
		// Skip ChatGPT-incompatible codex model from estimated seed as well.
		if strings.EqualFold(in.Provider, "codex") && strings.EqualFold(m.Name, "gpt-5.3-codex") {
			in.Provenance = strings.TrimSpace(in.Provenance +
				"; static_seed_skipped=gpt-5.3-codex reason=chatgpt_account_incompatible")
			continue
		}
		depths := make([]string, 0, len(m.Depths))
		for _, d := range m.Depths {
			if t := strings.TrimSpace(d.Token); t != "" {
				depths = append(depths, t)
			}
		}
		// Curated static only — never invent a full ladder when empty.
		if len(depths) == 0 {
			depths = []string{"medium"}
		}
		def := m.DefaultDepth
		if def == "" {
			def = p.DefaultDepth
		}
		if def == "" {
			def = depths[0]
		}
		// Also skip when live already registered a slug/base variant of this model.
		slugKey := strings.ToLower(in.Provider) + "|" + slugifyModelName(m.Name)
		if live[slugKey] {
			continue
		}
		in.Models = append(in.Models, ModelSpec{
			ModelID: m.Name, Present: true,
			SupportedDepths: depths, DefaultDepth: def,
			CatalogHintOnly: true,
		})
	}
	in.Provenance = strings.TrimSpace(in.Provenance +
		"; models_source=static_registry_estimated; catalog_hint_only=static_seed_non_routable")
}

// productionRoutableAuth delegates to the shared providerinventory gate so
// capacity Authenticated matches promoteUsableInstallations and rehydrateAuth.
func productionRoutableAuth(auth providerinventory.AuthReadiness) bool {
	return providerinventory.ExactFreshReadyAuth(auth)
}

func mapPIConfidence(c providerinventory.Confidence) Confidence {
	switch c {
	case providerinventory.ConfidenceExact:
		return ConfidenceExact
	case providerinventory.ConfidenceEstimated:
		return ConfidenceEstimated
	default:
		return ConfidenceUnknown
	}
}

func mapPIFreshness(f providerinventory.FreshnessState) Freshness {
	switch f {
	case providerinventory.FreshnessFresh:
		return FreshnessFresh
	case providerinventory.FreshnessStale:
		return FreshnessStale
	case providerinventory.FreshnessExpired:
		return FreshnessExpired
	default:
		return FreshnessUnknown
	}
}

// qtyFromPtrScaled converts a scaled integer quantity into a typed Quantity.
// scale N means the integer stores units of 10^-N (ValueScale=2 → divide by 100).
// Never rounds to a fixed two-decimal presentation; the float64 holds the exact
// decimal that the scaled integer represents (exact for scales used by providers).
func qtyFromPtrScaled(v *int64, scale int, unit CapacityUnit) Quantity {
	if v == nil {
		return Quantity{Class: QtyUnknown, Unit: unit}
	}
	if *v == 0 {
		return Quantity{Class: QtyZero, Value: 0, Unit: unit}
	}
	val := float64(*v)
	if scale > 0 {
		denom := 1.0
		for i := 0; i < scale; i++ {
			denom *= 10
		}
		val = val / denom
	}
	return Quantity{Class: QtyFinite, Value: val, Unit: unit}
}

func unitFromQuantityKind(k string) CapacityUnit {
	switch {
	case strings.Contains(k, "token"):
		return UnitTokens
	case strings.Contains(k, "request") || strings.Contains(k, "message"):
		return UnitMessages
	default:
		return UnitUnknown
	}
}

func parseTimeOr(s string, fallback time.Time) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return fallback
}
