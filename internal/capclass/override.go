package capclass

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// OverrideStore is an in-memory append-only store for owner overrides.
// Once an attempt is marked active, no override may mutate its route class.
type OverrideStore struct {
	mu        sync.Mutex
	byID      map[string]Override
	active    map[string]struct{} // attempt IDs with frozen route
	byAttempt map[string][]string
}

// NewOverrideStore returns an empty store.
func NewOverrideStore() *OverrideStore {
	return &OverrideStore{
		byID:      make(map[string]Override),
		active:    make(map[string]struct{}),
		byAttempt: make(map[string][]string),
	}
}

// Put appends an override. If AttemptID is set and that attempt is already
// active, Put fails with ErrImmutable (acceptance #4).
func (s *OverrideStore) Put(ov Override) (Override, error) {
	if s == nil {
		return Override{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	actor := strings.TrimSpace(ov.Actor)
	reason := strings.TrimSpace(ov.Reason)
	if actor == "" || reason == "" {
		return Override{}, fmt.Errorf("%w: actor and reason required", ErrInvalid)
	}
	if !ov.TargetClass.Valid() {
		return Override{}, fmt.Errorf("%w: target class", ErrInvalid)
	}
	if ov.Direction != OverrideRaise && ov.Direction != OverrideLower {
		return Override{}, fmt.Errorf("%w: direction", ErrInvalid)
	}
	attempt := strings.TrimSpace(ov.AttemptID)

	s.mu.Lock()
	defer s.mu.Unlock()

	if attempt != "" {
		if _, frozen := s.active[attempt]; frozen {
			return Override{}, fmt.Errorf("%w: attempt %s", ErrImmutable, attempt)
		}
	}

	out := Override{
		Schema:      SchemaOverride,
		Actor:       actor,
		Reason:      reason,
		Direction:   ov.Direction,
		TargetClass: ov.TargetClass,
		AttemptID:   attempt,
		CreatedAt:   ov.CreatedAt.UTC(),
	}
	if out.CreatedAt.IsZero() {
		out.CreatedAt = time.Now().UTC()
	}
	if id := strings.TrimSpace(ov.ID); id != "" {
		if _, exists := s.byID[id]; exists {
			return Override{}, fmt.Errorf("%w: override id already exists", ErrInvalid)
		}
		out.ID = id
	} else {
		out.ID = overrideID(out)
	}
	if _, exists := s.byID[out.ID]; exists {
		return Override{}, fmt.Errorf("%w: override id collision", ErrInvalid)
	}
	s.byID[out.ID] = out
	if attempt != "" {
		s.byAttempt[attempt] = append(s.byAttempt[attempt], out.ID)
	}
	return out, nil
}

// MarkActive freezes an attempt so subsequent overrides cannot mutate its route.
func (s *OverrideStore) MarkActive(attemptID string) error {
	if s == nil {
		return fmt.Errorf("%w: nil store", ErrInvalid)
	}
	attempt := strings.TrimSpace(attemptID)
	if attempt == "" {
		return fmt.Errorf("%w: attempt id required", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[attempt] = struct{}{}
	return nil
}

// IsActive reports whether an attempt route is frozen.
func (s *OverrideStore) IsActive(attemptID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.active[strings.TrimSpace(attemptID)]
	return ok
}

// Get returns an override by ID.
func (s *OverrideStore) Get(id string) (Override, error) {
	if s == nil {
		return Override{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ov, ok := s.byID[strings.TrimSpace(id)]
	if !ok {
		return Override{}, ErrNotFound
	}
	return ov, nil
}

// ForAttempt returns override IDs recorded for an attempt (append order).
func (s *OverrideStore) ForAttempt(attemptID string) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.byAttempt[strings.TrimSpace(attemptID)]
	out := make([]string, len(src))
	copy(out, src)
	return out
}

func overrideID(ov Override) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%d",
		ov.Actor, ov.Reason, ov.Direction, ov.TargetClass, ov.AttemptID, ov.CreatedAt.UnixNano())
	return "cov_" + hex.EncodeToString(h.Sum(nil))[:16]
}
