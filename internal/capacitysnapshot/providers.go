package capacitysnapshot

import (
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/antigravityquota"
	"github.com/jasonhnd/loopcoder/internal/claudequota"
	"github.com/jasonhnd/loopcoder/internal/codexquota"
	"github.com/jasonhnd/loopcoder/internal/grokquota"
)

// ObservationPlan documents the official bounded observation surface for a provider.
// It does not scrape credentials; callers use installed CLI/API status only.
type ObservationPlan struct {
	Provider        string   `json:"provider"`
	SourceKinds     []string `json:"source_kinds"`
	SupportedUnits  []string `json:"supported_units"`
	CLIStatusHints  []string `json:"cli_status_hints,omitempty"`
	Notes           string   `json:"notes,omitempty"`
	RequiresNetwork bool     `json:"requires_network"`
}

// OfficialObservationPlans returns the reviewed plans for Codex/Claude/Grok/Antigravity.
func OfficialObservationPlans() []ObservationPlan {
	return []ObservationPlan{
		{
			Provider: "codex",
			SourceKinds: []string{
				"official-cli-machine-readable-command",
				"official-cli-status-or-error-class",
				"fixture",
			},
			SupportedUnits:  []string{string(UnitPercentage), string(UnitTokens), string(UnitCredits), string(UnitUnknown)},
			CLIStatusHints:  []string{"codex", "usage/rate-limit surfaces when exposed"},
			Notes:           "Normalize via codexquota; never invent remaining when missing.",
			RequiresNetwork: false,
		},
		{
			Provider: "claude",
			SourceKinds: []string{
				"official-cli-machine-readable-command",
				"official-cli-status-or-error-class",
				"fixture",
			},
			SupportedUnits:  []string{string(UnitPercentage), string(UnitTokens), string(UnitUnknown)},
			CLIStatusHints:  []string{"claude", "/usage or status when exposed"},
			Notes:           "Normalize via claudequota; percentage/window retained as estimated when not exact tokens.",
			RequiresNetwork: false,
		},
		{
			Provider: "grok",
			SourceKinds: []string{
				"official-cli-machine-readable-command",
				"provider-machine-readable",
				"fixture",
			},
			SupportedUnits:  []string{string(UnitPercentage), string(UnitTokens), string(UnitUnknown)},
			CLIStatusHints:  []string{"grok", "dynamic catalog required for model presence"},
			Notes:           "Grok requires dynamic inventory; static model lists alone are insufficient.",
			RequiresNetwork: false,
		},
		{
			Provider: "antigravity",
			SourceKinds: []string{
				"official-cli-status-or-error-class",
				"fixture",
			},
			SupportedUnits:  []string{string(UnitPercentage), string(UnitWindow), string(UnitUnknown)},
			CLIStatusHints:  []string{"antigravity/gemini CLI status"},
			Notes:           "Windows may be unknown; unknown must remain unknown (not fabricated).",
			RequiresNetwork: false,
		},
	}
}

// ModelSpec is a catalog model observation input.
type ModelSpec struct {
	ModelID         string
	SupportedDepths []string
	DefaultDepth    string
	ClassHint       string
	Present         bool
	// CatalogHintOnly: static/adapter-declared/source-less catalog hint.
	// Present may still be true for display; production auto-route must skip.
	CatalogHintOnly bool
}

// AccountInput is a provider-neutral observation used when not mapping from *quota packages.
type AccountInput struct {
	Provider         string
	AccountRef       string
	InstallRef       string
	Installed        bool
	Authenticated    bool
	Healthy          bool
	RateLimited      bool
	CooldownActive   bool
	HealthConfidence Confidence
	HealthFreshness  Freshness
	Windows          []Window
	Models           []ModelSpec
	Source           string
	CapturedAt       time.Time
	Provenance       string
}

// FromAccountInput converts a neutral input into AccountObservation.
func FromAccountInput(in AccountInput) AccountObservation {
	models := make([]ModelEntry, 0, len(in.Models))
	for _, m := range in.Models {
		models = append(models, ModelEntry{
			ModelID:          m.ModelID,
			SupportedDepths:  append([]string(nil), m.SupportedDepths...),
			DefaultDepth:     m.DefaultDepth,
			ClassHint:        m.ClassHint,
			PresentInCatalog: m.Present,
			CatalogHintOnly:  m.CatalogHintOnly,
		})
	}
	return AccountObservation{
		Provider:         in.Provider,
		AccountRef:       in.AccountRef,
		InstallRef:       in.InstallRef,
		Installed:        in.Installed,
		Authenticated:    in.Authenticated,
		Healthy:          in.Healthy,
		RateLimited:      in.RateLimited,
		CooldownActive:   in.CooldownActive,
		HealthConfidence: in.HealthConfidence,
		HealthFreshness:  in.HealthFreshness,
		Windows:          append([]Window(nil), in.Windows...),
		Models:           models,
		Source:           in.Source,
		CapturedAt:       in.CapturedAt,
		Provenance:       in.Provenance,
	}
}

// --- Provider *quota adapters -------------------------------------------------

func mapQtyClass(class string) QuantityClass {
	switch QuantityClass(class) {
	case QtyFinite, QtyZero, QtyMissing, QtyUnknown, QtyUnlimited:
		return QuantityClass(class)
	default:
		return QtyUnknown
	}
}

func mapUnit(u string) CapacityUnit {
	switch strings.ToLower(strings.TrimSpace(u)) {
	case "tokens", "token":
		return UnitTokens
	case "credits", "credit":
		return UnitCredits
	case "percent", "percentage", "%":
		return UnitPercentage
	case "requests", "request", "messages", "message":
		return UnitMessages
	case "window":
		return UnitWindow
	default:
		if u == "" {
			return UnitUnknown
		}
		return CapacityUnit(strings.ToLower(u))
	}
}

func mapConfidence(c string) Confidence {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "exact", "high":
		// provider packages sometimes use high/low; map high→estimated unless exact.
		if strings.EqualFold(c, "exact") {
			return ConfidenceExact
		}
		return ConfidenceEstimated
	case "estimated", "medium":
		return ConfidenceEstimated
	case "low", "unknown", "":
		return ConfidenceUnknown
	default:
		return ConfidenceUnknown
	}
}

func mapFreshness(f string) Freshness {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "fresh":
		return FreshnessFresh
	case "stale":
		return FreshnessStale
	case "expired":
		return FreshnessExpired
	default:
		return FreshnessUnknown
	}
}

type commonQty struct {
	Class string
	Value float64
	Unit  string
}

type commonWindow struct {
	Kind       string
	AccountRef string
	Used       commonQty
	Remaining  commonQty
	Limit      commonQty
	ResetAt    *time.Time
	CapturedAt time.Time
	Confidence string
	Freshness  string
	Source     string
}

func windowsFromCommon(ws []commonWindow) []Window {
	out := make([]Window, 0, len(ws))
	for _, w := range ws {
		unit := mapUnit(w.Remaining.Unit)
		if unit == UnitUnknown {
			unit = mapUnit(w.Limit.Unit)
		}
		if unit == UnitUnknown {
			unit = mapUnit(w.Used.Unit)
		}
		out = append(out, Window{
			Kind: w.Kind,
			Unit: unit,
			Used: Quantity{Class: mapQtyClass(w.Used.Class), Value: w.Used.Value, Unit: mapUnit(w.Used.Unit)},
			Remaining: Quantity{
				Class: mapQtyClass(w.Remaining.Class), Value: w.Remaining.Value, Unit: mapUnit(w.Remaining.Unit),
			},
			Limit: Quantity{
				Class: mapQtyClass(w.Limit.Class), Value: w.Limit.Value, Unit: mapUnit(w.Limit.Unit),
			},
			ResetAt:    w.ResetAt,
			CapturedAt: w.CapturedAt,
			Confidence: mapConfidence(w.Confidence),
			Freshness:  mapFreshness(w.Freshness),
			Source:     w.Source,
		})
	}
	return out
}

// FromCodexQuota maps a codexquota.Snapshot into an AccountObservation.
func FromCodexQuota(s codexquota.Snapshot, account AccountInput) AccountObservation {
	ws := make([]commonWindow, 0, len(s.Windows))
	for _, w := range s.Windows {
		ws = append(ws, commonWindow{
			Kind: string(w.Kind), AccountRef: w.AccountRef,
			Used:      commonQty{Class: string(w.Used.Class), Value: w.Used.Value, Unit: w.Used.Unit},
			Remaining: commonQty{Class: string(w.Remaining.Class), Value: w.Remaining.Value, Unit: w.Remaining.Unit},
			Limit:     commonQty{Class: string(w.Limit.Class), Value: w.Limit.Value, Unit: w.Limit.Unit},
			ResetAt:   w.ResetAt, CapturedAt: w.CapturedAt,
			Confidence: w.Confidence, Freshness: w.Freshness, Source: w.Source,
		})
	}
	account.Provider = "codex"
	if account.Source == "" {
		account.Source = s.Source
	}
	if account.CapturedAt.IsZero() {
		account.CapturedAt = s.CapturedAt
	}
	account.Windows = windowsFromCommon(ws)
	return FromAccountInput(account)
}

// FromClaudeQuota maps a claudequota.Snapshot into an AccountObservation.
func FromClaudeQuota(s claudequota.Snapshot, account AccountInput) AccountObservation {
	ws := make([]commonWindow, 0, len(s.Windows))
	for _, w := range s.Windows {
		ws = append(ws, commonWindow{
			Kind: string(w.Kind), AccountRef: w.AccountRef,
			Used:      commonQty{Class: string(w.Used.Class), Value: w.Used.Value, Unit: w.Used.Unit},
			Remaining: commonQty{Class: string(w.Remaining.Class), Value: w.Remaining.Value, Unit: w.Remaining.Unit},
			Limit:     commonQty{Class: string(w.Limit.Class), Value: w.Limit.Value, Unit: w.Limit.Unit},
			ResetAt:   w.ResetAt, CapturedAt: w.CapturedAt,
			Confidence: w.Confidence, Freshness: w.Freshness, Source: w.Source,
		})
	}
	account.Provider = "claude"
	if account.Source == "" {
		account.Source = s.Source
	}
	if account.CapturedAt.IsZero() {
		account.CapturedAt = s.CapturedAt
	}
	account.Windows = windowsFromCommon(ws)
	return FromAccountInput(account)
}

// FromGrokQuota maps a grokquota.Snapshot into an AccountObservation.
func FromGrokQuota(s grokquota.Snapshot, account AccountInput) AccountObservation {
	ws := make([]commonWindow, 0, len(s.Windows))
	for _, w := range s.Windows {
		ws = append(ws, commonWindow{
			Kind: string(w.Kind), AccountRef: w.AccountRef,
			Used:      commonQty{Class: string(w.Used.Class), Value: w.Used.Value, Unit: w.Used.Unit},
			Remaining: commonQty{Class: string(w.Remaining.Class), Value: w.Remaining.Value, Unit: w.Remaining.Unit},
			Limit:     commonQty{Class: string(w.Limit.Class), Value: w.Limit.Value, Unit: w.Limit.Unit},
			ResetAt:   w.ResetAt, CapturedAt: w.CapturedAt,
			Confidence: w.Confidence, Freshness: w.Freshness, Source: w.Source,
		})
	}
	account.Provider = "grok"
	if account.Source == "" {
		account.Source = s.Source
	}
	if account.CapturedAt.IsZero() {
		account.CapturedAt = s.CapturedAt
	}
	account.Windows = windowsFromCommon(ws)
	return FromAccountInput(account)
}

// FromAntigravityQuota maps an antigravityquota.Snapshot into an AccountObservation.
func FromAntigravityQuota(s antigravityquota.Snapshot, account AccountInput) AccountObservation {
	ws := make([]commonWindow, 0, len(s.Windows))
	for _, w := range s.Windows {
		ws = append(ws, commonWindow{
			Kind: string(w.Kind), AccountRef: w.AccountRef,
			Used:      commonQty{Class: string(w.Used.Class), Value: w.Used.Value, Unit: w.Used.Unit},
			Remaining: commonQty{Class: string(w.Remaining.Class), Value: w.Remaining.Value, Unit: w.Remaining.Unit},
			Limit:     commonQty{Class: string(w.Limit.Class), Value: w.Limit.Value, Unit: w.Limit.Unit},
			ResetAt:   w.ResetAt, CapturedAt: w.CapturedAt,
			Confidence: w.Confidence, Freshness: w.Freshness, Source: w.Source,
		})
	}
	account.Provider = "antigravity"
	if account.Source == "" {
		account.Source = s.Source
	}
	if account.CapturedAt.IsZero() {
		account.CapturedAt = s.CapturedAt
	}
	account.Windows = windowsFromCommon(ws)
	return FromAccountInput(account)
}
