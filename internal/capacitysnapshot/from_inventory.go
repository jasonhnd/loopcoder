package capacitysnapshot

import (
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// FromProviderInventoryReport maps a live providerinventory.Report into AccountObservations.
// It does not invent quota: missing remaining stays unknown.
func FromProviderInventoryReport(rep providerinventory.Report, now time.Time) []AccountObservation {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// Group by adapter_id
	type accKey struct {
		provider string
	}
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
		depths := []string{"low", "medium", "high"}
		b.in.Models = append(b.in.Models, ModelSpec{
			ModelID: m.CanonicalModelID, Present: present,
			SupportedDepths: depths, DefaultDepth: "medium",
		})
	}

	// Quota windows
	for _, q := range rep.QuotaSnapshots {
		provider := strings.ToLower(strings.TrimSpace(q.AdapterID))
		if provider == "" {
			// try scope key
			provider = strings.ToLower(strings.TrimSpace(q.ScopeKey))
		}
		b := ensure(provider)
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
		// If installed+auth but no windows, leave windows empty → not unattended
		// unless we have usable data — honesty.
		if b.in.Installed && b.in.Authenticated && b.in.Healthy && b.in.HealthFreshness == FreshnessUnknown {
			b.in.HealthFreshness = FreshnessFresh
			if b.in.HealthConfidence == ConfidenceUnknown {
				b.in.HealthConfidence = ConfidenceEstimated
			}
		}
		out = append(out, FromAccountInput(b.in))
	}
	return out
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
