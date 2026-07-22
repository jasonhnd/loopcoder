package capacitysnapshot

import "time"

const (
	SchemaSnapshot = "loopcoder.capacity_snapshot.v1"
	SchemaAccount  = "loopcoder.capacity_account.v1"
	SchemaModel    = "loopcoder.capacity_model.v1"
	SchemaWindow   = "loopcoder.capacity_window.v1"
)

// Confidence is observation confidence. Unknown must never be coerced to exact.
type Confidence string

const (
	ConfidenceExact     Confidence = "exact"
	ConfidenceEstimated Confidence = "estimated"
	ConfidenceUnknown   Confidence = "unknown"
)

// Freshness is observation freshness for routing eligibility.
type Freshness string

const (
	FreshnessFresh   Freshness = "fresh"
	FreshnessStale   Freshness = "stale"
	FreshnessExpired Freshness = "expired"
	FreshnessUnknown Freshness = "unknown"
)

// CapacityUnit names how remaining/used/limit are measured.
type CapacityUnit string

const (
	UnitTokens     CapacityUnit = "tokens"
	UnitMessages   CapacityUnit = "messages"
	UnitPercentage CapacityUnit = "percentage"
	UnitWindow     CapacityUnit = "window"
	UnitCredits    CapacityUnit = "credits"
	UnitRequests   CapacityUnit = "requests"
	UnitUnknown    CapacityUnit = "unknown"
)

// QuantityClass prevents silent zero for missing values.
type QuantityClass string

const (
	QtyFinite    QuantityClass = "finite"
	QtyZero      QuantityClass = "zero"
	QtyMissing   QuantityClass = "missing"
	QtyUnknown   QuantityClass = "unknown"
	QtyUnlimited QuantityClass = "unlimited"
)

// Quantity is a typed amount. Missing/unknown never serialize as numeric 0.
type Quantity struct {
	Class QuantityClass `json:"class"`
	Value float64       `json:"value,omitempty"`
	Unit  CapacityUnit  `json:"unit,omitempty"`
}

// Window is one capacity window for a provider/account/install scope.
type Window struct {
	Schema     string       `json:"schema"`
	Kind       string       `json:"kind"`
	Unit       CapacityUnit `json:"unit"`
	Used       Quantity     `json:"used"`
	Remaining  Quantity     `json:"remaining"`
	Limit      Quantity     `json:"limit"`
	ResetAt    *time.Time   `json:"reset_at,omitempty"`
	Confidence Confidence   `json:"confidence"`
	Freshness  Freshness    `json:"freshness"`
	Source     string       `json:"source"`
	CapturedAt time.Time    `json:"captured_at"`
}

// ModelEntry is a model present in a fresh account/install catalog.
type ModelEntry struct {
	Schema           string   `json:"schema"`
	ModelID          string   `json:"model_id"`
	SupportedDepths  []string `json:"supported_depths,omitempty"`
	DefaultDepth     string   `json:"default_depth,omitempty"`
	ClassHint        string   `json:"class_hint,omitempty"` // optional capclass hint
	PresentInCatalog bool     `json:"present_in_catalog"`
}

// AccountObservation is one provider company × account/install profile.
type AccountObservation struct {
	Schema           string       `json:"schema"`
	Provider         string       `json:"provider"`
	AccountRef       string       `json:"account_ref"` // redacted stable id; never a secret
	InstallRef       string       `json:"install_ref,omitempty"`
	Installed        bool         `json:"installed"`
	Authenticated    bool         `json:"authenticated"`
	Healthy          bool         `json:"healthy"`
	RateLimited      bool         `json:"rate_limited"`
	CooldownActive   bool         `json:"cooldown_active"`
	HealthConfidence Confidence   `json:"health_confidence"`
	HealthFreshness  Freshness    `json:"health_freshness"`
	Windows          []Window     `json:"windows,omitempty"`
	Models           []ModelEntry `json:"models,omitempty"`
	Source           string       `json:"source"`
	CapturedAt       time.Time    `json:"captured_at"`
	Provenance       string       `json:"provenance,omitempty"`
}

// Snapshot is the immutable multi-provider capacity truth for routing.
type Snapshot struct {
	Schema     string               `json:"schema"`
	Accounts   []AccountObservation `json:"accounts"`
	CapturedAt time.Time            `json:"captured_at"`
	Digest     string               `json:"digest"`
	// UnattendedOK is true only when at least one account can feed auto-route.
	UnattendedOK bool     `json:"unattended_ok"`
	Reasons      []string `json:"reasons,omitempty"`
}
