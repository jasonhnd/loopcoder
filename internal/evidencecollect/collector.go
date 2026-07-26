package evidencecollect

import (
	"fmt"
	"sync"
	"time"
)

// Store holds accepted events with digest dedup.
type Store struct {
	mu     sync.Mutex
	events []Event
	last   map[string]string // subject+type -> digest
	now    func() time.Time
}

// NewStore creates an empty evidence store.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{last: map[string]string{}, now: now}
}

// Accept validates, redacts, deduplicates, and appends. Returns (event, accepted, err).
// Unchanged digests return accepted=false without growth.
func (s *Store) Accept(o Observation) (Event, bool, error) {
	if o.Type == "" || o.Source == "" || o.Subject == "" || o.Confidence == "" || o.ObservedAt.IsZero() {
		return Event{}, false, ErrMissingFields
	}
	if o.Significance == "" {
		return Event{}, false, ErrMissingFields
	}
	if o.Privacy == "" {
		o.Privacy = PrivacyInternal
	}
	// Provider prose cannot set lifecycle authority.
	if o.Source == SourceProvider || o.Type == TypeProviderProse {
		if o.LifecycleAuthority {
			return Event{}, false, ErrProviderLifecycle
		}
		// Also reject if significance claims terminal/process authority via fields.
		if o.Fields != nil {
			for _, k := range []string{"process_state", "delivery_state", "verification_state", "terminal_state"} {
				if _, ok := o.Fields[k]; ok {
					return Event{}, false, ErrProviderLifecycle
				}
			}
		}
	}
	if o.Privacy == PrivacySecret {
		return Event{}, false, ErrSecretNotPersistable
	}
	if o.Excerpt != "" {
		o.Excerpt = RedactExcerpt(o.Excerpt, 256)
	}
	// Copy fields redacted keys only
	fields := map[string]string{}
	for k, v := range o.Fields {
		if containsSecret(k) || containsSecret(v) {
			continue
		}
		fields[k] = v
	}
	o.Fields = fields
	dig := DigestOf(o)
	key := fmt.Sprintf("%s|%s", o.Subject, o.Type)

	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.last[key]; ok && prev == dig && o.Significance == SigHeartbeat {
		// Heartbeat unchanged: no growth.
		return Event{}, false, nil
	}
	if prev, ok := s.last[key]; ok && prev == dig && o.Significance != SigTransition {
		// Unchanged non-transition: skip.
		return Event{}, false, nil
	}

	ev := Event{
		SchemaVersion:  SchemaVersion,
		Type:           o.Type,
		Source:         o.Source,
		Subject:        o.Subject,
		Confidence:     o.Confidence,
		ObservedAt:     o.ObservedAt.UTC(),
		RecordedAt:     s.now().UTC(),
		Digest:         dig,
		CausalIdentity: o.CausalIdentity,
		Privacy:        o.Privacy,
		Significance:   o.Significance,
		IsProgress:     o.Significance == SigProgress || o.Significance == SigTransition,
		IsHeartbeat:    o.Significance == SigHeartbeat,
		Fields:         fields,
		Excerpt:        o.Excerpt,
	}
	// Heartbeat cannot be labeled progress.
	if ev.IsHeartbeat {
		ev.IsProgress = false
	}
	s.events = append(s.events, ev)
	s.last[key] = dig
	return ev, true, nil
}

// Events returns a copy of the accepted sequence.
func (s *Store) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

func containsSecret(s string) bool {
	lower := s
	for i := 0; i < len(lower); i++ {
		// cheap lower for ASCII
	}
	// use strings
	for _, bad := range []string{"sk-", "ghp_", "password", "api_key", "secret", "token="} {
		if len(s) >= len(bad) {
			// case-insensitive contains
			for i := 0; i+len(bad) <= len(s); i++ {
				match := true
				for j := 0; j < len(bad); j++ {
					c := s[i+j]
					if c >= 'A' && c <= 'Z' {
						c += 'a' - 'A'
					}
					if c != bad[j] {
						match = false
						break
					}
				}
				if match {
					return true
				}
			}
		}
	}
	return false
}

// --- Collectors (pure builders of Observation) ---

// ProcessState builds a process liveness/heartbeat or transition observation.
func ProcessState(subject, state string, progress bool, at time.Time) Observation {
	sig := SigHeartbeat
	if progress {
		sig = SigTransition
	}
	return Observation{
		Type:               TypeProcessState,
		Source:             SourceRuntime,
		Subject:            subject,
		Confidence:         ConfidenceFull,
		ObservedAt:         at,
		Privacy:            PrivacyInternal,
		Significance:       sig,
		LifecycleAuthority: true,
		Fields:             map[string]string{"state": state},
	}
}

// OutputMovement records bounded output advance as concrete progress.
func OutputMovement(subject string, bytesDelta int, at time.Time) Observation {
	return Observation{
		Type:               TypeOutputMovement,
		Source:             SourceRuntime,
		Subject:            subject,
		Confidence:         ConfidenceFull,
		ObservedAt:         at,
		Privacy:            PrivacyInternal,
		Significance:       SigProgress,
		LifecycleAuthority: false,
		Fields:             map[string]string{"bytes_delta": fmt.Sprintf("%d", bytesDelta)},
	}
}

// ResourceSample is a resource heartbeat (or transition when threshold crossed).
func ResourceSample(subject string, cpu float64, rss int64, procs int, transition bool, at time.Time) Observation {
	sig := SigHeartbeat
	if transition {
		sig = SigTransition
	}
	return Observation{
		Type:               TypeResourceSample,
		Source:             SourceResource,
		Subject:            subject,
		Confidence:         ConfidenceFull,
		ObservedAt:         at,
		Privacy:            PrivacyInternal,
		Significance:       sig,
		LifecycleAuthority: false,
		Fields: map[string]string{
			"cpu_rate":      fmt.Sprintf("%.4f", cpu),
			"rss_bytes":     fmt.Sprintf("%d", rss),
			"process_count": fmt.Sprintf("%d", procs),
		},
	}
}

// GitCommitObserved records a worktree commit as progress.
func GitCommitObserved(subject, commitSHA string, at time.Time) Observation {
	return Observation{
		Type:         TypeGitCommit,
		Source:       SourceGit,
		Subject:      subject,
		Confidence:   ConfidenceFull,
		ObservedAt:   at,
		Privacy:      PrivacyPublic,
		Significance: SigProgress,
		Fields:       map[string]string{"commit": commitSHA},
	}
}

// GitHubCheckChanged records a required check status change as progress.
func GitHubCheckChanged(subject, check, conclusion string, at time.Time) Observation {
	return Observation{
		Type:               TypeGitHubCheck,
		Source:             SourceGitHub,
		Subject:            subject,
		Confidence:         ConfidenceFull,
		ObservedAt:         at,
		Privacy:            PrivacyPublic,
		Significance:       SigTransition,
		LifecycleAuthority: true,
		Fields:             map[string]string{"check": check, "conclusion": conclusion},
	}
}

// OperatorAction records an explicit operator action.
func OperatorAction(subject, action string, at time.Time) Observation {
	return Observation{
		Type:               TypeOperatorAction,
		Source:             SourceOperator,
		Subject:            subject,
		Confidence:         ConfidenceFull,
		ObservedAt:         at,
		Privacy:            PrivacyInternal,
		Significance:       SigProgress,
		LifecycleAuthority: true,
		Fields:             map[string]string{"action": action},
	}
}

// ProviderProse retains bounded content without lifecycle authority.
func ProviderProse(subject, excerpt string, at time.Time) Observation {
	return Observation{
		Type:               TypeProviderProse,
		Source:             SourceProvider,
		Subject:            subject,
		Confidence:         ConfidencePartial,
		ObservedAt:         at,
		Privacy:            PrivacyInternal,
		Significance:       SigProgress,
		LifecycleAuthority: false,
		Excerpt:            excerpt,
	}
}
