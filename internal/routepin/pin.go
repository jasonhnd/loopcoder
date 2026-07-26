package routepin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerexec"
)

const SchemaPin = "loopcoder.route.pin.v1"

var (
	ErrInvalid     = errors.New("routepin: invalid")
	ErrUnavailable = errors.New("routepin: unavailable")
	ErrMismatch    = errors.New("routepin: actual route mismatch")
	ErrNotReady    = errors.New("routepin: not ready for launch")
	ErrImmutable   = errors.New("routepin: active pin immutable")
	ErrNotFound    = errors.New("routepin: not found")
)

// SubagentPolicy is whether provider-native sub-agents are allowed.
type SubagentPolicy string

const (
	SubagentForbidden SubagentPolicy = "forbidden"
	SubagentAllowed   SubagentPolicy = "allowed"
)

// Fields are the explicit pin components including account/install/window binding.
type Fields struct {
	Provider         string         `json:"provider"`
	Model            string         `json:"model"`
	Effort           string         `json:"effort,omitempty"`
	Permission       string         `json:"permission,omitempty"`
	AccountRef       string         `json:"account_ref,omitempty"`
	InstallRef       string         `json:"install_ref,omitempty"`
	WindowKind       string         `json:"window_kind,omitempty"`
	ReservationID    string         `json:"reservation_id,omitempty"`
	RouteReason      string         `json:"route_reason,omitempty"`
	SubagentPolicy   SubagentPolicy `json:"subagent_policy"`
	NativeDelegation bool           `json:"native_delegation"`
}

// Normalize applies canonical form (trim, lower provider, reject empty).
func (f Fields) Normalize() (Fields, error) {
	out := Fields{
		Provider:         strings.ToLower(strings.TrimSpace(f.Provider)),
		Model:            strings.TrimSpace(f.Model),
		Effort:           strings.TrimSpace(f.Effort),
		Permission:       strings.TrimSpace(f.Permission),
		AccountRef:       strings.TrimSpace(f.AccountRef),
		InstallRef:       strings.TrimSpace(f.InstallRef),
		WindowKind:       strings.TrimSpace(f.WindowKind),
		ReservationID:    strings.TrimSpace(f.ReservationID),
		RouteReason:      strings.TrimSpace(f.RouteReason),
		SubagentPolicy:   f.SubagentPolicy,
		NativeDelegation: f.NativeDelegation,
	}
	if out.Provider == "" || out.Model == "" {
		return Fields{}, fmt.Errorf("%w: provider and model required", ErrInvalid)
	}
	if out.SubagentPolicy == "" {
		out.SubagentPolicy = SubagentForbidden
	}
	if out.SubagentPolicy != SubagentForbidden && out.SubagentPolicy != SubagentAllowed {
		return Fields{}, fmt.Errorf("%w: subagent policy", ErrInvalid)
	}
	if out.NativeDelegation && out.SubagentPolicy == SubagentForbidden {
		return Fields{}, fmt.Errorf("%w: native delegation forbidden by policy", ErrInvalid)
	}
	// no silent alias substitution — reject whitespace models
	if strings.Contains(out.Model, " ") {
		return Fields{}, fmt.Errorf("%w: model alias not allowed", ErrInvalid)
	}
	if out.Permission == "" {
		out.Permission = "default"
	}
	return out, nil
}

// Digest is the immutable route digest including account/install/window.
func (f Fields) Digest() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%v",
		f.Provider, f.Model, f.Effort, f.Permission,
		f.AccountRef, f.InstallRef, f.WindowKind, f.ReservationID, f.RouteReason,
		f.SubagentPolicy, f.NativeDelegation)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:24]
}

// ToExecRoute maps to providerexec.Route for launch.
func (f Fields) ToExecRoute() providerexec.Route {
	return providerexec.Route{
		Provider: f.Provider, Model: f.Model, Effort: f.Effort,
		Permission: f.Permission, AccountRef: f.AccountRef, InstallRef: f.InstallRef,
		WindowKind: f.WindowKind, ReservationID: f.ReservationID, RouteReason: f.RouteReason,
		NativeDelegation: f.NativeDelegation,
	}
}

// Pin is a persisted route pin for an attempt.
type Pin struct {
	Schema       string    `json:"schema"`
	PinID        string    `json:"pin_id"`
	AttemptID    string    `json:"attempt_id"`
	ProjectID    string    `json:"project_id"`
	Requested    Fields    `json:"requested"`
	Normalized   Fields    `json:"normalized"`
	Resolved     Fields    `json:"resolved"` // same as normalized when explicit; no auto-choice
	Digest       string    `json:"digest"`
	Acknowledged bool      `json:"acknowledged"`
	Provenance   string    `json:"provenance"` // owner_explicit
	CreatedAt    time.Time `json:"created_at"`
	SuccessorOf  string    `json:"successor_of,omitempty"`
	SupersededBy string    `json:"superseded_by,omitempty"`
	Active       bool      `json:"active"`
}

// LaunchEvidence binds launch/terminal records to the pin digest.
type LaunchEvidence struct {
	AttemptID    string `json:"attempt_id"`
	PinDigest    string `json:"pin_digest"`
	ActualDigest string `json:"actual_digest"`
	Match        bool   `json:"match"`
}

// Store holds pins.
type Store struct {
	mu     sync.Mutex
	byID   map[string]*Pin
	active map[string]string // attempt -> pin_id
	seq    int64
	now    func() time.Time
	// Available reports whether provider/model is known to the named adapter (no fallback).
	Available func(provider, model string) bool
}

// NewStore creates a pin store.
func NewStore(now func() time.Time, available func(provider, model string) bool) *Store {
	if now == nil {
		now = time.Now
	}
	if available == nil {
		available = func(string, string) bool { return true }
	}
	return &Store{byID: map[string]*Pin{}, active: map[string]string{}, now: now, Available: available}
}

// Persist creates an active pin for an attempt. Fails if incomplete or unavailable.
func (s *Store) Persist(projectID, attemptID string, requested Fields) (Pin, error) {
	norm, err := requested.Normalize()
	if err != nil {
		return Pin{}, err
	}
	if !s.Available(norm.Provider, norm.Model) {
		return Pin{}, fmt.Errorf("%w: %s/%s", ErrUnavailable, norm.Provider, norm.Model)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.active[attemptID]; ok {
		if p := s.byID[id]; p != nil && p.Active {
			return Pin{}, fmt.Errorf("%w: attempt already has active pin %s", ErrImmutable, id)
		}
	}
	s.seq++
	id := fmt.Sprintf("pin_%d", s.seq)
	p := &Pin{
		Schema: SchemaPin, PinID: id, AttemptID: attemptID, ProjectID: projectID,
		Requested: requested, Normalized: norm, Resolved: norm,
		Digest: norm.Digest(), Acknowledged: false, Provenance: "owner_explicit",
		CreatedAt: s.now().UTC(), Active: true,
	}
	s.byID[id] = p
	s.active[attemptID] = id
	return *p, nil
}

// Acknowledge marks owner acknowledgment required before launch.
func (s *Store) Acknowledge(pinID string) (Pin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[pinID]
	if !ok {
		return Pin{}, ErrNotFound
	}
	p.Acknowledged = true
	return *p, nil
}

// ReadyForLaunch is true when pin is active, acknowledged, and complete.
func (p Pin) ReadyForLaunch() bool {
	return p.Active && p.Acknowledged && p.Digest != "" && p.Normalized.Provider != "" && p.Normalized.Model != ""
}

// GetActive returns the active pin for an attempt.
func (s *Store) GetActive(attemptID string) (Pin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.active[attemptID]
	if !ok {
		return Pin{}, ErrNotFound
	}
	p := s.byID[id]
	if p == nil || !p.Active {
		return Pin{}, ErrNotFound
	}
	return *p, nil
}

// VerifyActual checks actual fields match the pin digest (integrity).
func (s *Store) VerifyActual(attemptID string, actual Fields) (LaunchEvidence, error) {
	p, err := s.GetActive(attemptID)
	if err != nil {
		return LaunchEvidence{}, err
	}
	if !p.ReadyForLaunch() {
		return LaunchEvidence{}, ErrNotReady
	}
	act, err := actual.Normalize()
	if err != nil {
		return LaunchEvidence{}, err
	}
	ad := act.Digest()
	ev := LaunchEvidence{AttemptID: attemptID, PinDigest: p.Digest, ActualDigest: ad, Match: ad == p.Digest}
	if !ev.Match {
		return ev, ErrMismatch
	}
	return ev, nil
}

// Successor creates a new attempt pin superseding the old (route change requires new approval).
func (s *Store) Successor(oldPinID, newAttemptID string, requested Fields) (Pin, error) {
	s.mu.Lock()
	old, ok := s.byID[oldPinID]
	if !ok {
		s.mu.Unlock()
		return Pin{}, ErrNotFound
	}
	if !old.Active {
		s.mu.Unlock()
		return Pin{}, ErrNotFound
	}
	oldAttempt := old.AttemptID
	old.Active = false
	s.mu.Unlock()

	p, err := s.Persist(old.ProjectID, newAttemptID, requested)
	if err != nil {
		// restore active on failure
		s.mu.Lock()
		old.Active = true
		s.active[oldAttempt] = oldPinID
		s.mu.Unlock()
		return Pin{}, err
	}
	s.mu.Lock()
	old.SupersededBy = p.PinID
	p2 := s.byID[p.PinID]
	p2.SuccessorOf = oldPinID
	// clear old attempt active mapping
	if s.active[oldAttempt] == oldPinID {
		delete(s.active, oldAttempt)
	}
	out := *p2
	s.mu.Unlock()
	return out, nil
}

// MutateActive is rejected — documents immutability.
func (s *Store) MutateActive(attemptID string, _ Fields) error {
	if _, err := s.GetActive(attemptID); err != nil {
		return err
	}
	return ErrImmutable
}

// StatusView is redacted status evidence (no auth).
type StatusView struct {
	AttemptID  string `json:"attempt_id"`
	Requested  string `json:"requested_route"`
	Actual     string `json:"actual_route,omitempty"`
	Digest     string `json:"digest"`
	Ready      bool   `json:"ready_for_launch"`
	Provenance string `json:"provenance"`
}

// Status builds public route evidence.
func Status(p Pin, actual *Fields) StatusView {
	v := StatusView{
		AttemptID: p.AttemptID,
		Requested: fmt.Sprintf("%s/%s effort=%s perm=%s subagent=%s",
			p.Normalized.Provider, p.Normalized.Model, p.Normalized.Effort, p.Normalized.Permission, p.Normalized.SubagentPolicy),
		Digest: p.Digest, Ready: p.ReadyForLaunch(), Provenance: p.Provenance,
	}
	if actual != nil {
		a, err := actual.Normalize()
		if err == nil {
			v.Actual = fmt.Sprintf("%s/%s effort=%s perm=%s subagent=%s",
				a.Provider, a.Model, a.Effort, a.Permission, a.SubagentPolicy)
		}
	}
	return v
}
