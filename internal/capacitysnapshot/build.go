package capacitysnapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrInvalidSnapshot   = errors.New("capacitysnapshot: invalid")
	ErrCredentialLeak    = errors.New("capacitysnapshot: credential material forbidden")
	ErrNoEligibleAccount = errors.New("capacitysnapshot: no unattended-eligible account")
)

// Build validates and freezes observations into an immutable Snapshot with digest.
func Build(accounts []AccountObservation, now time.Time) (Snapshot, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	if len(accounts) == 0 {
		return Snapshot{}, fmt.Errorf("%w: empty accounts", ErrInvalidSnapshot)
	}
	out := make([]AccountObservation, 0, len(accounts))
	var reasons []string
	eligible := 0
	for i, a := range accounts {
		norm, err := normalizeAccount(a, now)
		if err != nil {
			return Snapshot{}, err
		}
		ok, why := unattendedEligible(norm)
		if ok {
			eligible++
		} else if why != "" {
			reasons = append(reasons, fmt.Sprintf("%s/%s: %s", norm.Provider, norm.AccountRef, why))
		}
		out = append(out, norm)
		_ = i
	}
	// Stable order for digest.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].AccountRef < out[j].AccountRef
	})
	snap := Snapshot{
		Schema:       SchemaSnapshot,
		Accounts:     out,
		CapturedAt:   now,
		UnattendedOK: eligible > 0,
		Reasons:      reasons,
	}
	if !snap.UnattendedOK && len(reasons) == 0 {
		snap.Reasons = []string{"no eligible provider/account for unattended routing"}
	}
	d, err := digestOf(snap)
	if err != nil {
		return Snapshot{}, err
	}
	snap.Digest = d
	return snap, nil
}

func normalizeAccount(a AccountObservation, now time.Time) (AccountObservation, error) {
	a.Provider = strings.ToLower(strings.TrimSpace(a.Provider))
	a.AccountRef = strings.TrimSpace(a.AccountRef)
	a.InstallRef = strings.TrimSpace(a.InstallRef)
	a.Source = strings.TrimSpace(a.Source)
	a.Schema = SchemaAccount
	if a.Provider == "" {
		return AccountObservation{}, fmt.Errorf("%w: provider required", ErrInvalidSnapshot)
	}
	// Empty AccountRef stays empty (unknown / non-routable). Never invent
	// "account-unknown" — that becomes hash-shaped and can be routed as exact.
	if err := rejectCredentialMaterial(a.AccountRef, a.InstallRef, a.Source, a.Provenance); err != nil {
		return AccountObservation{}, err
	}
	if a.CapturedAt.IsZero() {
		a.CapturedAt = now
	}
	a.CapturedAt = a.CapturedAt.UTC()
	if a.HealthConfidence == "" {
		a.HealthConfidence = ConfidenceUnknown
	}
	if a.HealthFreshness == "" {
		a.HealthFreshness = FreshnessUnknown
	}
	// Normalize windows
	windows := make([]Window, 0, len(a.Windows))
	for _, w := range a.Windows {
		nw, err := normalizeWindow(w, now)
		if err != nil {
			return AccountObservation{}, err
		}
		windows = append(windows, nw)
	}
	a.Windows = windows
	// Models: only keep present-in-catalog true entries; absent models are dropped
	// so routing cannot select them.
	models := make([]ModelEntry, 0, len(a.Models))
	for _, m := range a.Models {
		m.ModelID = strings.TrimSpace(m.ModelID)
		if m.ModelID == "" {
			continue
		}
		if err := rejectCredentialMaterial(m.ModelID); err != nil {
			return AccountObservation{}, err
		}
		m.Schema = SchemaModel
		if !m.PresentInCatalog {
			// Explicitly absent — not selectable; skip.
			continue
		}
		// Normalize depths
		depths := make([]string, 0, len(m.SupportedDepths))
		seen := map[string]bool{}
		for _, d := range m.SupportedDepths {
			d = strings.ToLower(strings.TrimSpace(d))
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			depths = append(depths, d)
		}
		sort.Strings(depths)
		m.SupportedDepths = depths
		m.DefaultDepth = strings.ToLower(strings.TrimSpace(m.DefaultDepth))
		models = append(models, m)
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].ModelID < models[j].ModelID })
	a.Models = models
	return a, nil
}

func normalizeWindow(w Window, now time.Time) (Window, error) {
	w.Schema = SchemaWindow
	w.Kind = strings.TrimSpace(w.Kind)
	if w.Kind == "" {
		w.Kind = "unknown"
	}
	if w.Unit == "" {
		w.Unit = UnitUnknown
	}
	w.Used = normalizeQty(w.Used, w.Unit)
	w.Remaining = normalizeQty(w.Remaining, w.Unit)
	w.Limit = normalizeQty(w.Limit, w.Unit)
	if w.Confidence == "" {
		w.Confidence = ConfidenceUnknown
	}
	if w.Freshness == "" {
		w.Freshness = FreshnessUnknown
	}
	if w.CapturedAt.IsZero() {
		w.CapturedAt = now
	}
	w.CapturedAt = w.CapturedAt.UTC()
	if w.ResetAt != nil {
		t := w.ResetAt.UTC()
		w.ResetAt = &t
	}
	if err := rejectCredentialMaterial(w.Source); err != nil {
		return Window{}, err
	}
	// Honesty: never invent remaining as full when unknown/missing.
	if w.Remaining.Class == QtyMissing {
		w.Remaining.Class = QtyUnknown
		w.Remaining.Value = 0
	}
	return w, nil
}

func normalizeQty(q Quantity, unit CapacityUnit) Quantity {
	switch q.Class {
	case QtyFinite, QtyZero, QtyMissing, QtyUnknown, QtyUnlimited:
	case "":
		q.Class = QtyUnknown
	default:
		q.Class = QtyUnknown
	}
	if q.Unit == "" {
		q.Unit = unit
	}
	if q.Class == QtyUnknown || q.Class == QtyMissing || q.Class == QtyUnlimited {
		// Do not leave a misleading numeric payload.
		q.Value = 0
	}
	if q.Class == QtyZero {
		q.Value = 0
	}
	return q
}

// unattendedEligible requires install+auth+health, fresh health, at least one
// fresh non-unknown capacity signal or unlimited window, and ≥1 catalog model.
func unattendedEligible(a AccountObservation) (bool, string) {
	if !a.Installed {
		return false, "not installed"
	}
	if !a.Authenticated {
		return false, "not authenticated"
	}
	if a.RateLimited {
		return false, "rate limited"
	}
	if a.CooldownActive {
		return false, "cooldown active"
	}
	if !a.Healthy {
		return false, "unhealthy"
	}
	if a.HealthFreshness != FreshnessFresh {
		return false, "health not fresh"
	}
	if a.HealthConfidence == ConfidenceUnknown {
		return false, "health confidence unknown"
	}
	if len(a.Models) == 0 {
		return false, "no catalog models"
	}
	hasUsableWindow := false
	for _, w := range a.Windows {
		if w.Freshness != FreshnessFresh {
			continue
		}
		if w.Confidence == ConfidenceUnknown {
			continue
		}
		switch w.Remaining.Class {
		case QtyFinite, QtyZero, QtyUnlimited:
			hasUsableWindow = true
		}
	}
	if !hasUsableWindow {
		// Partial: allow estimated percentage if finite.
		for _, w := range a.Windows {
			if w.Freshness == FreshnessFresh && w.Remaining.Class == QtyFinite &&
				(w.Confidence == ConfidenceExact || w.Confidence == ConfidenceEstimated) {
				hasUsableWindow = true
				break
			}
		}
	}
	if !hasUsableWindow {
		return false, "no fresh usable capacity window (unknown/stale only)"
	}
	return true, ""
}

func digestOf(s Snapshot) (string, error) {
	// Digest excludes the Digest field itself.
	clone := s
	clone.Digest = ""
	b, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func rejectCredentialMaterial(vals ...string) error {
	needles := []string{
		"sk-", "api_key", "apikey", "authorization:", "bearer ",
		"-----begin", "x-api-key", "password=", "secret=",
	}
	for _, v := range vals {
		low := strings.ToLower(v)
		for _, n := range needles {
			if strings.Contains(low, n) {
				return fmt.Errorf("%w: %q", ErrCredentialLeak, n)
			}
		}
	}
	return nil
}

// RemainingFraction returns remaining/limit when both finite, else nil.
// Unknown never becomes 0 or 1.
func RemainingFraction(w Window) *float64 {
	if w.Remaining.Class == QtyUnlimited {
		v := 1.0
		return &v
	}
	if w.Remaining.Class != QtyFinite && w.Remaining.Class != QtyZero {
		return nil
	}
	if w.Limit.Class != QtyFinite || w.Limit.Value <= 0 {
		// percentage unit may store remaining as 0..1 or 0..100 without limit
		if w.Unit == UnitPercentage && (w.Remaining.Class == QtyFinite || w.Remaining.Class == QtyZero) {
			v := w.Remaining.Value
			if v > 1 {
				v = v / 100
			}
			if v < 0 {
				return nil
			}
			if v > 1 {
				v = 1
			}
			return &v
		}
		return nil
	}
	v := w.Remaining.Value / w.Limit.Value
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return &v
}
