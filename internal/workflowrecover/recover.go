package workflowrecover

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

const (
	SchemaCancel     = "loopcoder.workflow.cancel.v1"
	SchemaRestart    = "loopcoder.workflow.restart.v1"
	SchemaProjection = "loopcoder.workflow.terminal_projection.v1"
	SchemaEvent      = "loopcoder.workflow.lifecycle_event.v1"
	PolicyVersion    = "workflow-recover-v1"
)

// ChildState tracks a workflow-owned child for recovery.
type ChildState struct {
	ChildID    string
	WorkItemID string
	Required   bool
	// Running, UnstartedClaim, Terminal, Ambiguous
	Kind string
	// ClaimStarted false means unstarted claim may be released on cancel.
	ClaimStarted bool
	Terminal     workgraph.TerminalState
	// PersistError simulates durable write failure.
	PersistError bool
}

// Workflow is the parent recovery unit.
type Workflow struct {
	WorkflowID string
	// Cancelling/Running/Terminal
	Phase string
	// ParentTerminal only after all required children durable.
	ParentTerminal workgraph.TerminalState
	Children       map[string]*ChildState
	// LiveChildren adopted on restart.
	Events []Event
	// WaveSeq last persisted wave.
	WaveSeq int
	// IntegrationSeq last integration progress.
	IntegrationSeq int
}

// Event is append-only audit truth (never deleted here).
type Event struct {
	Schema    string    `json:"schema"`
	EventID   string    `json:"event_id"`
	Type      string    `json:"type"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// CancelReport is the cancellation outcome.
type CancelReport struct {
	Schema            string   `json:"schema"`
	WorkflowID        string   `json:"workflow_id"`
	JoinedChildren    []string `json:"joined_children"`
	ReleasedClaims    []string `json:"released_claims"`
	Ambiguous         []string `json:"ambiguous"`
	IntegrationCancel bool     `json:"integration_cancel"`
}

// RestartReport is the restart reconciliation outcome.
type RestartReport struct {
	Schema           string   `json:"schema"`
	WorkflowID       string   `json:"workflow_id"`
	AdoptedLive      []string `json:"adopted_live"`
	ResumedWaveSeq   int      `json:"resumed_wave_seq"`
	ResumedIntegSeq  int      `json:"resumed_integration_seq"`
	DuplicateBlocked bool     `json:"duplicate_blocked"`
}

// Projection is compact terminal status with audit range digest.
type Projection struct {
	Schema         string            `json:"schema"`
	WorkflowID     string            `json:"workflow_id"`
	ParentTerminal string            `json:"parent_terminal"`
	Children       map[string]string `json:"children"`
	// EventRange first..last event ids
	EventRangeFrom string `json:"event_range_from"`
	EventRangeTo   string `json:"event_range_to"`
	AuditDigest    string `json:"audit_digest"`
	// SourceEventsIntact always true — projection never mutates/deletes events.
	SourceEventsIntact bool `json:"source_events_intact"`
}

// Store holds workflows for recovery tests.
type Store struct {
	mu  sync.Mutex
	wfs map[string]*Workflow
	seq int64
	now func() time.Time
}

// NewStore creates a recovery store.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{wfs: map[string]*Workflow{}, now: now}
}

var (
	ErrInvalid = errors.New("workflowrecover: invalid")
	ErrState   = errors.New("workflowrecover: state")
)

// Create registers a running workflow with children.
func (s *Store) Create(wfID string, children []ChildState, waveSeq, integSeq int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.wfs[wfID]; ok {
		return fmt.Errorf("%w: exists", ErrInvalid)
	}
	w := &Workflow{
		WorkflowID: wfID, Phase: "running",
		Children: map[string]*ChildState{}, WaveSeq: waveSeq, IntegrationSeq: integSeq,
	}
	for i := range children {
		c := children[i]
		w.Children[c.ChildID] = &c
	}
	s.appendLocked(w, "created", "")
	s.wfs[wfID] = w
	return nil
}

// Cancel propagates stop/join, releases unstarted claims, records ambiguous.
func (s *Store) Cancel(wfID string, cancelIntegration bool) (CancelReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.wfs[wfID]
	if !ok {
		return CancelReport{}, fmt.Errorf("%w: not found", ErrInvalid)
	}
	rep := CancelReport{Schema: SchemaCancel, WorkflowID: wfID, IntegrationCancel: cancelIntegration}
	w.Phase = "cancelling"
	for id, c := range w.Children {
		switch {
		case c.Kind == "Ambiguous" || (c.Kind == "Running" && !c.ClaimStarted):
			// unknown ownership
			rep.Ambiguous = append(rep.Ambiguous, id)
			c.Kind = "Ambiguous"
			s.appendLocked(w, "ambiguous", id)
		case c.Kind == "UnstartedClaim" || (c.Kind == "Running" && !c.ClaimStarted):
			// proven unstarted
			rep.ReleasedClaims = append(rep.ReleasedClaims, id)
			c.Kind = "Terminal"
			c.Terminal = workgraph.TermCancelled
			s.appendLocked(w, "release_unstarted", id)
		case c.Kind == "Running" && c.ClaimStarted:
			// join running
			rep.JoinedChildren = append(rep.JoinedChildren, id)
			c.Kind = "Terminal"
			c.Terminal = workgraph.TermCancelled
			s.appendLocked(w, "join_cancel", id)
		case c.Kind == "Terminal":
			// already done
		default:
			if c.Terminal == workgraph.TermNone && c.ClaimStarted {
				rep.JoinedChildren = append(rep.JoinedChildren, id)
				c.Kind = "Terminal"
				c.Terminal = workgraph.TermCancelled
				s.appendLocked(w, "join_cancel", id)
			} else if c.Terminal == workgraph.TermNone && !c.ClaimStarted {
				rep.ReleasedClaims = append(rep.ReleasedClaims, id)
				c.Kind = "Terminal"
				c.Terminal = workgraph.TermCancelled
				s.appendLocked(w, "release_unstarted", id)
			}
		}
	}
	sort.Strings(rep.JoinedChildren)
	sort.Strings(rep.ReleasedClaims)
	sort.Strings(rep.Ambiguous)
	if cancelIntegration {
		s.appendLocked(w, "integration_cancel", "")
	}
	// try parent terminal after cancel
	s.tryParentTerminalLocked(w)
	return rep, nil
}

// Restart reconciles live children and resumes wave/integration seq without duplicates.
func (s *Store) Restart(wfID string, liveChildIDs []string) (RestartReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.wfs[wfID]
	if !ok {
		return RestartReport{}, fmt.Errorf("%w: not found", ErrInvalid)
	}
	rep := RestartReport{
		Schema: SchemaRestart, WorkflowID: wfID,
		ResumedWaveSeq: w.WaveSeq, ResumedIntegSeq: w.IntegrationSeq,
		DuplicateBlocked: true,
	}
	for _, id := range liveChildIDs {
		if c, ok := w.Children[id]; ok {
			if c.Kind == "Terminal" {
				// adopt without re-launch
				rep.AdoptedLive = append(rep.AdoptedLive, id)
				continue
			}
			c.Kind = "Running"
			c.ClaimStarted = true
			rep.AdoptedLive = append(rep.AdoptedLive, id)
			s.appendLocked(w, "adopt_live", id)
		} else {
			// unknown live process — ambiguous
			w.Children[id] = &ChildState{ChildID: id, Kind: "Ambiguous", ClaimStarted: true}
			s.appendLocked(w, "ambiguous_live", id)
			rep.AdoptedLive = append(rep.AdoptedLive, id)
		}
	}
	if w.Phase == "cancelling" {
		// stay cancelling until resolved
	} else if w.ParentTerminal == workgraph.TermNone {
		w.Phase = "running"
	}
	sort.Strings(rep.AdoptedLive)
	s.appendLocked(w, "restart", fmt.Sprintf("wave=%d integ=%d", w.WaveSeq, w.IntegrationSeq))
	return rep, nil
}

// AcceptChildTerminal durably accepts a child terminal (may fail persistence).
func (s *Store) AcceptChildTerminal(wfID, childID string, term workgraph.TerminalState, persistOK bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.wfs[wfID]
	if !ok {
		return fmt.Errorf("%w: not found", ErrInvalid)
	}
	c, ok := w.Children[childID]
	if !ok {
		return fmt.Errorf("%w: child", ErrInvalid)
	}
	if !persistOK {
		c.PersistError = true
		s.appendLocked(w, "child_persist_error", childID)
		// suppress parent success
		return fmt.Errorf("%w: child terminal persistence failed", ErrState)
	}
	c.Kind = "Terminal"
	c.Terminal = term
	c.PersistError = false
	s.appendLocked(w, "child_terminal", childID+":"+string(term))
	s.tryParentTerminalLocked(w)
	return nil
}

// tryParentTerminalLocked sets parent terminal only when all required children
// are durably terminal without persist errors.
func (s *Store) tryParentTerminalLocked(w *Workflow) {
	if w.ParentTerminal != workgraph.TermNone {
		return
	}
	anyRequiredFail := false
	anyRunning := false
	anyPersistErr := false
	anyAmbiguous := false
	hasRequired := false
	for _, c := range w.Children {
		if c.PersistError {
			anyPersistErr = true
		}
		if c.Kind == "Ambiguous" {
			anyAmbiguous = true
		}
		if c.Required {
			hasRequired = true
			if c.Kind != "Terminal" || c.Terminal == workgraph.TermNone {
				anyRunning = true
			} else if c.Terminal == workgraph.TermFailed || c.Terminal == workgraph.TermCancelled {
				anyRequiredFail = true
			}
		} else if c.Kind != "Terminal" && c.Kind != "Ambiguous" {
			// optional still running — parent may wait or ignore; wait for all for simplicity
			if c.Kind == "Running" || c.Kind == "UnstartedClaim" {
				anyRunning = true
			}
		}
	}
	if anyPersistErr || anyAmbiguous || anyRunning {
		return // no parent terminal yet
	}
	if !hasRequired {
		// no required children — cancel path may set cancelled
		if w.Phase == "cancelling" {
			w.ParentTerminal = workgraph.TermCancelled
			w.Phase = "terminal"
			s.appendLocked(w, "parent_terminal", string(w.ParentTerminal))
		}
		return
	}
	if anyRequiredFail {
		w.ParentTerminal = workgraph.TermFailed
	} else {
		w.ParentTerminal = workgraph.TermSucceeded
	}
	w.Phase = "terminal"
	s.appendLocked(w, "parent_terminal", string(w.ParentTerminal))
}

// Project builds compact terminal projection without mutating events.
func (s *Store) Project(wfID string) (Projection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.wfs[wfID]
	if !ok {
		return Projection{}, fmt.Errorf("%w: not found", ErrInvalid)
	}
	p := Projection{
		Schema: SchemaProjection, WorkflowID: wfID,
		ParentTerminal: string(w.ParentTerminal),
		Children:       map[string]string{}, SourceEventsIntact: true,
	}
	for id, c := range w.Children {
		if c.Terminal != workgraph.TermNone {
			p.Children[id] = string(c.Terminal)
		} else {
			p.Children[id] = c.Kind
		}
	}
	if len(w.Events) > 0 {
		p.EventRangeFrom = w.Events[0].EventID
		p.EventRangeTo = w.Events[len(w.Events)-1].EventID
	}
	p.AuditDigest = auditDigest(w.Events, p.ParentTerminal, p.Children)
	return p, nil
}

// Events returns a copy of source events (retention is separate).
func (s *Store) Events(wfID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.wfs[wfID]
	if !ok {
		return nil, fmt.Errorf("%w: not found", ErrInvalid)
	}
	out := append([]Event{}, w.Events...)
	return out, nil
}

func (s *Store) appendLocked(w *Workflow, typ, detail string) {
	s.seq++
	w.Events = append(w.Events, Event{
		Schema: SchemaEvent, EventID: fmt.Sprintf("wle_%d", s.seq),
		Type: typ, Detail: detail, CreatedAt: s.now().UTC(),
	})
}

func auditDigest(events []Event, parent string, children map[string]string) string {
	type wire struct {
		Parent   string
		Children map[string]string
		EventIDs []string
	}
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.EventID
	}
	// stable children
	keys := make([]string, 0, len(children))
	for k := range children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cm := map[string]string{}
	for _, k := range keys {
		cm[k] = children[k]
	}
	b, _ := json.Marshal(wire{Parent: parent, Children: cm, EventIDs: ids})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

// Silence unused import
var _ = strings.TrimSpace
