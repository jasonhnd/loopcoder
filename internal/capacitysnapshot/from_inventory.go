package capacitysnapshot

import (
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

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

	// Live catalog capabilities first (preferred for final acceptance routing).
	liveModels := map[string]bool{}
	for _, m := range rep.ModelCapabilities {
		b := ensure(m.AdapterID)
		present := m.CanonicalModelID != "" &&
			m.AvailabilityState == providerinventory.AvailabilityAvailable &&
			m.LifecycleState != providerinventory.LifecycleRemoved &&
			m.LifecycleState != providerinventory.LifecycleDeprecated
		// Only mark present when catalog freshness is not expired.
		if m.FreshnessState == providerinventory.FreshnessExpired ||
			m.FreshnessState == providerinventory.FreshnessStale {
			present = false
		}
		// Account/model capability exclusions (not a silent global delete of the
		// static registry): ChatGPT Codex accounts reject gpt-5.3-codex.
		// Record as not-present with provenance; do not invent a replacement window.
		if present && strings.EqualFold(m.AdapterID, "codex") &&
			strings.EqualFold(m.CanonicalModelID, "gpt-5.3-codex") {
			present = false
			b.in.Provenance = strings.TrimSpace(b.in.Provenance +
				"; model_excluded=gpt-5.3-codex reason=chatgpt_account_incompatible")
		}
		depths := []string{"low", "medium", "high"}
		b.in.Models = append(b.in.Models, ModelSpec{
			ModelID: m.CanonicalModelID, Present: present,
			SupportedDepths: depths, DefaultDepth: "medium",
		})
		if present {
			liveModels[strings.ToLower(m.AdapterID)+"|"+strings.ToLower(m.CanonicalModelID)] = true
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
		// Static model seed: estimated catalog only when live catalog has zero
		// present models. Never creates capacity windows and never bypasses the
		// requirement for fresh live catalog on final multi-provider acceptance
		// (callers must still require live capability rows for RC acceptance).
		if !hasPresentModel(b.in.Models) {
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
// hints only. Does not invent capacity. Does not claim live freshness.
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
			depths = append(depths, d.Token)
		}
		if len(depths) == 0 {
			depths = []string{"low", "medium", "high"}
		}
		def := m.DefaultDepth
		if def == "" {
			def = p.DefaultDepth
		}
		if def == "" {
			def = "medium"
		}
		in.Models = append(in.Models, ModelSpec{
			ModelID: m.Name, Present: true,
			SupportedDepths: depths, DefaultDepth: def,
		})
	}
	in.Provenance = strings.TrimSpace(in.Provenance + "; models_source=static_registry_estimated")
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
