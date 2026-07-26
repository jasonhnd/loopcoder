package grokquota

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	SchemaWindow   = "loopcoder.grok.quota.window.v1"
	SchemaSnapshot = "loopcoder.grok.quota.snapshot.v1"
	AdapterID      = "grok"
	MaxRawFixtureB = 32 << 10
)

// WindowKind classifies a quota window.
type WindowKind string

const (
	WindowFiveHour WindowKind = "five_hour"
	WindowWeekly   WindowKind = "weekly"
	WindowCredit   WindowKind = "credit"
	WindowOther    WindowKind = "other"
)

// QuantityClass distinguishes real numbers from non-numeric states.
type QuantityClass string

const (
	QtyFinite    QuantityClass = "finite"
	QtyMissing   QuantityClass = "missing"
	QtyUnlimited QuantityClass = "unlimited"
	QtyUnknown   QuantityClass = "unknown"
	QtyZero      QuantityClass = "zero" // explicit numeric zero only
)

// Quantity is a typed amount that cannot silently become zero when missing.
type Quantity struct {
	Class QuantityClass `json:"class"`
	// Value is valid only for finite/zero.
	Value float64 `json:"value,omitempty"`
	Unit  string  `json:"unit,omitempty"` // tokens|credits|percent|requests
}

// Window is one normalized quota window.
type Window struct {
	Schema     string     `json:"schema"`
	Kind       WindowKind `json:"kind"`
	Scope      string     `json:"scope"`                 // account|model|workspace
	AccountRef string     `json:"account_ref,omitempty"` // redacted
	ModelScope string     `json:"model_scope,omitempty"`
	Used       Quantity   `json:"used"`
	Remaining  Quantity   `json:"remaining"`
	Limit      Quantity   `json:"limit"`
	ResetAt    *time.Time `json:"reset_at,omitempty"`
	CapturedAt time.Time  `json:"captured_at"`
	Confidence string     `json:"confidence"`
	Freshness  string     `json:"freshness"`
	Source     string     `json:"source"`
	Diagnostic string     `json:"diagnostic,omitempty"`
}

// Snapshot aggregates windows for one observation.
type Snapshot struct {
	Schema      string    `json:"schema"`
	AdapterID   string    `json:"adapter_id"`
	Windows     []Window  `json:"windows"`
	Status      string    `json:"status"` // ok|partial|stale|malformed|unavailable|unknown
	Diagnostics []string  `json:"diagnostics,omitempty"`
	CapturedAt  time.Time `json:"captured_at"`
	Source      string    `json:"source"`
	// No model decision fields.
}

// RawWindow is a fixture input line/object (approved structured surface).
type RawWindow struct {
	Kind       string `json:"kind"`
	Scope      string `json:"scope"`
	AccountRef string `json:"account_ref,omitempty"`
	ModelScope string `json:"model_scope,omitempty"`
	// Optional numeric pointers: nil means missing (not zero).
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
	Limit     *float64 `json:"limit"`
	// LimitClass/RemainingClass override: unlimited|unknown
	LimitClass     string `json:"limit_class,omitempty"`
	RemainingClass string `json:"remaining_class,omitempty"`
	UsedClass      string `json:"used_class,omitempty"`
	Unit           string `json:"unit,omitempty"`
	// ResetRFC3339 is timezone-aware reset time.
	ResetRFC3339 string `json:"reset_at,omitempty"`
	Source       string `json:"source,omitempty"`
}

// ParseOptions configures normalization.
type ParseOptions struct {
	Now        time.Time
	Source     string
	StaleAfter time.Duration
	// ForceStatus for fixture fault injection.
	ForceStatus string // malformed|unavailable|stale|""
}

var (
	ErrMalformed   = errors.New("grokquota: malformed")
	ErrUnavailable = errors.New("grokquota: unavailable")
	ErrTooLarge    = errors.New("grokquota: fixture too large")
)

// ParseJSONFixture normalizes a JSON array of RawWindow.
func ParseJSONFixture(raw []byte, opts ParseOptions) (Snapshot, error) {
	if len(raw) > MaxRawFixtureB {
		return Snapshot{}, ErrTooLarge
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.Source == "" {
		opts.Source = "fixture"
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 15 * time.Minute
	}
	if opts.ForceStatus == "unavailable" {
		return Snapshot{
			Schema: SchemaSnapshot, AdapterID: AdapterID, Status: "unavailable",
			CapturedAt: opts.Now, Source: opts.Source,
			Diagnostics: []string{"source_unavailable"},
		}, nil
	}
	if opts.ForceStatus == "malformed" || !json.Valid(raw) {
		return Snapshot{
			Schema: SchemaSnapshot, AdapterID: AdapterID, Status: "malformed",
			CapturedAt: opts.Now, Source: opts.Source,
			Diagnostics: []string{"malformed_json"},
		}, ErrMalformed
	}
	var raws []RawWindow
	if err := json.Unmarshal(raw, &raws); err != nil {
		return Snapshot{
			Schema: SchemaSnapshot, AdapterID: AdapterID, Status: "malformed",
			CapturedAt: opts.Now, Source: opts.Source,
			Diagnostics: []string{"unmarshal:" + err.Error()},
		}, ErrMalformed
	}

	var windows []Window
	var diags []string
	for i, r := range raws {
		w, d, err := normalizeWindow(r, opts)
		if err != nil {
			diags = append(diags, fmt.Sprintf("window[%d]:%v", i, err))
			continue
		}
		if d != "" {
			diags = append(diags, d)
		}
		windows = append(windows, w)
	}
	sort.SliceStable(windows, func(i, j int) bool {
		if windows[i].Kind != windows[j].Kind {
			return windows[i].Kind < windows[j].Kind
		}
		return windows[i].Scope < windows[j].Scope
	})

	status := "ok"
	if len(windows) == 0 && len(diags) > 0 {
		status = "malformed"
	} else if len(windows) > 0 && len(diags) > 0 {
		status = "partial"
	} else if len(windows) == 0 {
		status = "unknown"
	}
	if opts.ForceStatus == "stale" {
		status = "stale"
		for i := range windows {
			windows[i].Freshness = "stale"
			windows[i].Confidence = "low"
		}
	}

	// Redact: never keep raw that looks like tokens
	for i := range windows {
		if looksSecret(windows[i].AccountRef) {
			windows[i].AccountRef = "redacted"
			diags = append(diags, "account_ref_redacted")
		}
	}

	return Snapshot{
		Schema: SchemaSnapshot, AdapterID: AdapterID, Windows: windows,
		Status: status, Diagnostics: diags, CapturedAt: opts.Now, Source: opts.Source,
	}, nil
}

func normalizeWindow(r RawWindow, opts ParseOptions) (Window, string, error) {
	kind := mapKind(r.Kind)
	if kind == "" {
		return Window{}, "", fmt.Errorf("unknown kind %q", r.Kind)
	}
	scope := r.Scope
	if scope == "" {
		scope = "account"
	}
	src := r.Source
	if src == "" {
		src = opts.Source
	}
	w := Window{
		Schema: SchemaWindow, Kind: kind, Scope: scope,
		AccountRef: redactAccount(r.AccountRef), ModelScope: r.ModelScope,
		Used:       qtyFrom(r.Used, r.UsedClass, r.Unit),
		Remaining:  qtyFrom(r.Remaining, r.RemainingClass, r.Unit),
		Limit:      qtyFrom(r.Limit, r.LimitClass, r.Unit),
		CapturedAt: opts.Now.UTC(), Confidence: "high", Freshness: "fresh", Source: src,
	}
	// Never fabricate missing limit from used+remaining
	if w.Limit.Class == QtyMissing && w.Used.Class == QtyFinite && w.Remaining.Class == QtyFinite {
		// leave limit missing — do not set used+remaining
		w.Diagnostic = "limit_missing_not_fabricated"
	}
	// Explicit zero is QtyZero only when value is 0 and class finite input
	if r.Remaining != nil && *r.Remaining == 0 && r.RemainingClass == "" {
		w.Remaining = Quantity{Class: QtyZero, Value: 0, Unit: r.Unit}
	}
	if r.Used != nil && *r.Used == 0 && r.UsedClass == "" {
		w.Used = Quantity{Class: QtyZero, Value: 0, Unit: r.Unit}
	}

	if r.ResetRFC3339 != "" {
		t, err := parseReset(r.ResetRFC3339, opts.Now)
		if err != nil {
			return Window{}, "reset_parse_error", err
		}
		w.ResetAt = &t
	}
	return w, w.Diagnostic, nil
}

func qtyFrom(v *float64, class, unit string) Quantity {
	switch strings.ToLower(class) {
	case "unlimited":
		return Quantity{Class: QtyUnlimited, Unit: unit}
	case "unknown":
		return Quantity{Class: QtyUnknown, Unit: unit}
	case "missing":
		return Quantity{Class: QtyMissing, Unit: unit}
	}
	if v == nil {
		return Quantity{Class: QtyMissing, Unit: unit}
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) {
		return Quantity{Class: QtyUnknown, Unit: unit}
	}
	if *v == 0 {
		return Quantity{Class: QtyZero, Value: 0, Unit: unit}
	}
	return Quantity{Class: QtyFinite, Value: *v, Unit: unit}
}

func mapKind(k string) WindowKind {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "five_hour", "5h", "five-hour", "primary":
		return WindowFiveHour
	case "weekly", "week", "7d":
		return WindowWeekly
	case "credit", "credits":
		return WindowCredit
	case "other", "secondary":
		return WindowOther
	default:
		return ""
	}
}

// parseReset accepts RFC3339 / RFC3339Nano; rejects bare numbers as epoch without zone.
func parseReset(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	// numeric epoch seconds only if clearly unix and reasonable
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		// require 10+ digit unix or reject ambiguous
		if n > 1_000_000_000 {
			t := time.Unix(n, 0).UTC()
			// clock skew guard: more than 10y past/future is diagnostic only still accepted
			_ = now
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("timezone-unsafe reset %q", s)
}

func redactAccount(s string) string {
	if looksSecret(s) {
		return "redacted"
	}
	// strip email-like to local handle mark
	if strings.Contains(s, "@") {
		return "account_redacted"
	}
	return s
}

func looksSecret(s string) bool {
	ls := strings.ToLower(s)
	return strings.HasPrefix(ls, "ghp_") || strings.HasPrefix(ls, "sk-") || strings.Contains(ls, "token=")
}

// IsNumericZero reports only explicit zero quantities.
func IsNumericZero(q Quantity) bool {
	return q.Class == QtyZero || (q.Class == QtyFinite && q.Value == 0)
}

// IsAbsent reports missing/unknown/unlimited (not usable as zero remaining).
func IsAbsent(q Quantity) bool {
	return q.Class == QtyMissing || q.Class == QtyUnknown || q.Class == QtyUnlimited
}
