package capacitysnapshot

import (
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

// FromProviderInventoryReport maps a live providerinventory.Report into AccountObservations.
//
// Capacity honesty rules (must never be weakened):
//   - missing/unsupported quota stays unknown — never invent 0, full, or unlimited
//   - auth-ready and live model catalog are not capacity evidence
//   - static model seed is estimated catalog only and does not create unattended eligibility
//   - unattended eligibility still requires a real fresh usable capacity window
func FromProviderInventoryReport(rep providerinventory.Report, now time.Time) []AccountObservation {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// Group by adapter_id
	type builder struct {
		in AccountInput
	}
	by := map[string]*builder{}

	ensure := func(provider string) *builder {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			provider = "unknown"
		}
		if b, ok := by[provider]; ok {
			return b
		}
		b := &builder{in: AccountInput{
			Provider:         provider,
			AccountRef:       "acct-" + provider,
			InstallRef:       "install-" + provider,
			HealthConfidence: ConfidenceUnknown,
			HealthFreshness:  FreshnessUnknown,
			Source:           "providerinventory",
			CapturedAt:       now,
			Provenance:       "inventory_fingerprint=" + rep.InventoryFingerprint,
		}}
		by[provider] = b
		return b
	}

	for _, inst := range rep.Installations {
		b := ensure(inst.AdapterID)
		if inst.InstallationState == providerinventory.InstallationInstalled {
			b.in.Installed = true
		}
		if inst.FreshnessState == providerinventory.FreshnessFresh {
			b.in.HealthFreshness = FreshnessFresh
		} else if inst.FreshnessState == providerinventory.FreshnessStale {
			b.in.HealthFreshness = FreshnessStale
		}
		if conf := mapPIConfidence(inst.Confidence); conf != ConfidenceUnknown {
			b.in.HealthConfidence = conf
		}
		if inst.UsableForInvocation == "yes" || inst.UsableForInvocation == "true" {
			b.in.Healthy = true
		}
	}

	for _, auth := range rep.AuthReadiness {
		b := ensure(auth.AdapterID)
		if auth.ReadinessState == providerinventory.ReadinessReady {
			b.in.Authenticated = true
			b.in.Healthy = true
			if b.in.HealthFreshness == FreshnessUnknown {
				b.in.HealthFreshness = FreshnessFresh
			}
			if b.in.HealthConfidence == ConfidenceUnknown {
				b.in.HealthConfidence = ConfidenceEstimated
			}
		}
		if auth.AccountProfileID != nil && strings.TrimSpace(*auth.AccountProfileID) != "" {
			// Redacted account profile id only (no secrets).
			b.in.AccountRef = "acct-" + shortRef(*auth.AccountProfileID)
		}
	}

	// Production routes prefer provider-machine-readable exact+fresh catalog
	// rows (EntrySources / Source.Kind). Adapter-declared static catalogs also
	// carry ConfidenceExact+FreshnessFresh in production — Confidence alone is
	// not enough to distinguish them.
	liveModels := map[string]bool{}
	// Adapters that have at least one machine-readable exact+fresh capability.
	dynamicExactAdapters := map[string]bool{}
	for _, m := range rep.ModelCapabilities {
		if capabilityIsDynamicExactFresh(m) {
			dynamicExactAdapters[strings.ToLower(strings.TrimSpace(m.AdapterID))] = true
		}
	}
	// Also mark from ModelCatalogSnapshots when present (stronger source of truth).
	for _, snap := range rep.ModelCatalogSnapshots {
		if snap.CatalogSourceKind == providerinventory.CatalogSourceProviderMachineReadable &&
			snap.Confidence == providerinventory.ConfidenceExact &&
			snap.FreshnessState == providerinventory.FreshnessFresh {
			dynamicExactAdapters[strings.ToLower(strings.TrimSpace(snap.AdapterID))] = true
		}
	}
	modelIdx := map[string]int{}
	// Prefer dynamic machine-readable rows first, then static-only adapters.
	caps := append([]providerinventory.ModelCapability(nil), rep.ModelCapabilities...)
	sort.SliceStable(caps, func(i, j int) bool {
		di, dj := capabilityIsDynamicExactFresh(caps[i]), capabilityIsDynamicExactFresh(caps[j])
		if di != dj {
			return di && !dj
		}
		return false
	})
	for _, m := range caps {
		b := ensure(m.AdapterID)
		adapterKey := strings.ToLower(strings.TrimSpace(m.AdapterID))
		dynamic := capabilityIsDynamicExactFresh(m)
		// When this adapter has dynamic exact catalog, drop adapter-declared static
		// rows entirely from the model list (display comes from dynamic rows).
		if dynamicExactAdapters[adapterKey] && !dynamic {
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; adapter_declared_static_suppressed=dynamic_machine_readable_present")
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
		spec := modelSpecFromCapability(m.AdapterID, m)
		if present && strings.EqualFold(m.AdapterID, "codex") &&
			(strings.EqualFold(m.CanonicalModelID, "gpt-5.3-codex") || strings.EqualFold(spec.ModelID, "gpt-5.3-codex")) {
			present = false
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; model_excluded=gpt-5.3-codex reason=chatgpt_account_incompatible")
		}
		if !present || strings.TrimSpace(spec.ModelID) == "" {
			continue
		}
		// Fail closed: only fresh machine-readable is production-routable.
		// Adapter-declared static and source-less rows stay as catalog hints.
		hintOnly := !dynamic
		if hintOnly {
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; catalog_hint_only=non_machine_readable model=" + spec.ModelID)
		}
		key := strings.ToLower(m.AdapterID) + "|" + strings.ToLower(spec.ModelID)
		if idx, ok := modelIdx[key]; ok && idx >= 0 && idx < len(b.in.Models) {
			mergeModelSpec(&b.in.Models[idx], spec)
			// Never promote a hint to routable via merge; demote if either was hint.
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
		// liveModels keys only for production-routable rows so static seed does
		// not treat a hint as "live catalog present".
		if !hintOnly {
			liveModels[key] = true
			liveModels[strings.ToLower(m.AdapterID)+"|"+strings.ToLower(m.CanonicalModelID)] = true
			if slug := slugifyModelName(spec.ModelID); slug != "" {
				liveModels[strings.ToLower(m.AdapterID)+"|"+slug] = true
			}
			if peel := peelBaseName(spec.ModelID); peel != "" {
				liveModels[strings.ToLower(m.AdapterID)+"|"+strings.ToLower(peel)] = true
				liveModels[strings.ToLower(m.AdapterID)+"|"+slugifyModelName(peel)] = true
			}
		}
	}

	// Quota windows — only real observations with known confidence/freshness.
	// Unavailable/unsupported snapshots must not become finite/unlimited windows.
	for _, q := range rep.QuotaSnapshots {
		provider := strings.ToLower(strings.TrimSpace(q.AdapterID))
		if provider == "" {
			// try scope key
			provider = strings.ToLower(strings.TrimSpace(q.ScopeKey))
		}
		b := ensure(provider)
		// Explicitly refuse unavailable/not-applicable snapshots as usable capacity.
		if q.Confidence == providerinventory.ConfidenceUnavailable ||
			q.FreshnessState == providerinventory.FreshnessNotApplicable {
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; quota_observation=unsupported_or_unavailable")
			continue
		}
		// Require at least one of remaining/used/limit to be observed; pure empty
		// snapshots with unknown confidence are not capacity evidence.
		if q.RemainingValue == nil && q.UsedValue == nil && q.LimitValue == nil {
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; quota_observation=no_numeric_fields")
			continue
		}
		unit := mapUnit(q.Unit)
		if unit == UnitUnknown {
			unit = unitFromQuantityKind(string(q.QuantityKind))
		}
		w := Window{
			Kind:       string(q.WindowKind),
			Unit:       unit,
			Used:       qtyFromPtr(q.UsedValue, unit),
			Remaining:  qtyFromPtr(q.RemainingValue, unit),
			Limit:      qtyFromPtr(q.LimitValue, unit),
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
		b.in.Windows = append(b.in.Windows, w)
	}

	out := make([]AccountObservation, 0, len(by))
	for _, b := range by {
		// Health freshness fill is not capacity.
		if b.in.Installed && b.in.Authenticated && b.in.Healthy && b.in.HealthFreshness == FreshnessUnknown {
			b.in.HealthFreshness = FreshnessFresh
			if b.in.HealthConfidence == ConfidenceUnknown {
				b.in.HealthConfidence = ConfidenceEstimated
			}
		}
		// Static model seed: estimated catalog hints only when no production-
		// routable (machine-readable) model is present. Never creates capacity
		// windows and never enters production auto-route (CatalogHintOnly).
		if !hasProductionRoutableModel(b.in.Models) {
			seedStaticModelsEstimated(&b.in, liveModels)
		}
		// Prefer gpt-5.5 ordering among present models (capability preference).
		if b.in.Provider == "codex" && len(b.in.Models) > 1 {
			preferCodexDefaultModel(&b.in)
		}
		// CRITICAL: do NOT inject unlimited/full/zero remaining when windows empty.
		// Missing/unsupported quota ⇒ empty windows ⇒ unattended ineligible.
		out = append(out, FromAccountInput(b.in))
	}
	return out
}

func hasPresentModel(models []ModelSpec) bool {
	for _, m := range models {
		if m.Present && strings.TrimSpace(m.ModelID) != "" {
			return true
		}
	}
	return false
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

func qtyFromPtr(v *int64, unit CapacityUnit) Quantity {
	if v == nil {
		return Quantity{Class: QtyUnknown, Unit: unit}
	}
	if *v == 0 {
		return Quantity{Class: QtyZero, Value: 0, Unit: unit}
	}
	return Quantity{Class: QtyFinite, Value: float64(*v), Unit: unit}
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

func shortRef(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
