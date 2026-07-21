package statusproj

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/evidencecollect"
)

const (
	SchemaVersion     = "loopcoder.status_proj.v1"
	MaxSnapshotBytes  = 16 << 10
	MaxEventJSONBytes = 4 << 10
)

// Cursor is an opaque resume position (monotonic sequence).
type Cursor int64

// Status is the compact current projection.
type Status struct {
	SchemaVersion    string    `json:"schema_version"`
	ProjectID        string    `json:"project_id"`
	AttemptID        string    `json:"attempt_id"`
	Stage            string    `json:"stage"`
	Liveness         string    `json:"liveness"` // alive|exited|unknown|not_started
	Heartbeat        bool      `json:"heartbeat"`
	ConcreteProgress bool      `json:"concrete_progress"`
	ResourceState    string    `json:"resource_state"` // ok|stale|unknown|breach
	DeliveryGate     string    `json:"delivery_gate"`
	Blocker          string    `json:"blocker,omitempty"`
	NextAction       string    `json:"next_action"`
	NextTime         time.Time `json:"next_time,omitempty"`
	FinalMile        string    `json:"final_mile"`
	Cursor           Cursor    `json:"cursor"`
	Digest           string    `json:"digest"`
	// Unknown flags must stay explicit.
	ProcessUnknown  bool `json:"process_unknown"`
	ResourceUnknown bool `json:"resource_unknown"`
}

// Stream is an append-only event log with projection.
type Stream struct {
	mu     sync.Mutex
	events []evidencecollect.Event
	status Status
	seq    Cursor
}

// NewStream starts empty for a project/attempt.
func NewStream(projectID, attemptID string) *Stream {
	return &Stream{
		status: Status{
			SchemaVersion:   SchemaVersion,
			ProjectID:       projectID,
			AttemptID:       attemptID,
			Stage:           "not_started",
			Liveness:        "not_started",
			ResourceState:   "unknown",
			DeliveryGate:    "unknown",
			FinalMile:       "unknown",
			NextAction:      "wait",
			ProcessUnknown:  false,
			ResourceUnknown: true,
		},
	}
}

// Append accepts an evidence event and updates projection.
func (s *Stream) Append(ev evidencecollect.Event) (Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Size bound
	b, _ := json.Marshal(ev)
	if len(b) > MaxEventJSONBytes {
		return 0, fmt.Errorf("statusproj: event too large")
	}
	s.seq++
	s.events = append(s.events, ev)
	s.reduce(ev)
	s.status.Cursor = s.seq
	s.status.Digest = s.digestLocked()
	return s.seq, nil
}

func (s *Stream) reduce(ev evidencecollect.Event) {
	st := &s.status
	switch ev.Type {
	case evidencecollect.TypeProcessState:
		if ev.IsHeartbeat {
			st.Heartbeat = true
		}
		if state, ok := ev.Fields["state"]; ok {
			st.Liveness = state
			st.ProcessUnknown = state == "unknown"
		}
		if ev.IsProgress {
			st.ConcreteProgress = true
			if state, ok := ev.Fields["state"]; ok && state != "" {
				st.Stage = state
			}
		}
	case evidencecollect.TypeOutputMovement, evidencecollect.TypeGitCommit, evidencecollect.TypeOperatorAction:
		if ev.IsProgress {
			st.ConcreteProgress = true
		}
	case evidencecollect.TypeResourceSample:
		if ev.IsHeartbeat {
			st.Heartbeat = true
		}
		if ev.IsProgress || ev.Significance == evidencecollect.SigTransition {
			st.ResourceState = "ok"
			st.ResourceUnknown = false
			if _, ok := ev.Fields["breach"]; ok {
				st.ResourceState = "breach"
			}
		} else if st.ResourceState == "unknown" {
			st.ResourceState = "ok"
			st.ResourceUnknown = false
		}
	case evidencecollect.TypeGitHubCheck:
		if conc, ok := ev.Fields["conclusion"]; ok {
			st.DeliveryGate = conc
			if conc == "failure" {
				st.Blocker = "check_" + ev.Fields["check"]
				st.NextAction = "fix"
			}
		}
	case evidencecollect.TypeProviderProse:
		// Must not alter lifecycle/stage/liveness/delivery.
		return
	}
	// Never promote unknown to success by omission.
	if st.Liveness == "" {
		st.Liveness = "unknown"
		st.ProcessUnknown = true
	}
	if st.ResourceState == "" {
		st.ResourceState = "unknown"
		st.ResourceUnknown = true
	}
}

// Snapshot returns current status (size-bounded).
func (s *Stream) Snapshot() (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.Digest = s.digestLocked()
	b, _ := json.Marshal(st)
	if len(b) > MaxSnapshotBytes {
		return Status{}, fmt.Errorf("statusproj: snapshot too large")
	}
	return st, nil
}

// Rebuild recomputes status from the full event log; digest must match Snapshot.
func (s *Stream) Rebuild() (Status, error) {
	s.mu.Lock()
	events := append([]evidencecollect.Event(nil), s.events...)
	projectID, attemptID := s.status.ProjectID, s.status.AttemptID
	s.mu.Unlock()
	n := NewStream(projectID, attemptID)
	for _, ev := range events {
		if _, err := n.Append(ev); err != nil {
			return Status{}, err
		}
	}
	return n.Snapshot()
}

// Follow returns events strictly after cursor, once each, in order.
func (s *Stream) Follow(after Cursor) []evidencecollect.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if after < 0 {
		after = 0
	}
	if int(after) >= len(s.events) {
		return nil
	}
	out := make([]evidencecollect.Event, len(s.events)-int(after))
	copy(out, s.events[after:])
	return out
}

// Cursor returns the latest sequence.
func (s *Stream) Cursor() Cursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

func (s *Stream) digestLocked() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%v|%v|%s|%s|%d",
		s.status.Stage, s.status.Liveness, s.status.ResourceState, s.status.DeliveryGate,
		s.status.Blocker, s.status.ConcreteProgress, s.status.Heartbeat,
		s.status.FinalMile, s.status.NextAction, s.seq)
	for _, ev := range s.events {
		fmt.Fprintf(h, "|%s", ev.Digest)
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}
