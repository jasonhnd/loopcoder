package uisub

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/uireport"
)

const SchemaVersion = "loopcoder.ui_sub.v1"

// AckStage is monotonic delivery evidence.
type AckStage string

const (
	StagePersisted AckStage = "persisted"
	StageStreamed  AckStage = "streamed"
	StageAccepted  AckStage = "accepted"
	StageRendered  AckStage = "rendered"
	StageSeen      AckStage = "seen"
)

var stageOrder = map[AckStage]int{
	StagePersisted: 1,
	StageStreamed:  2,
	StageAccepted:  3,
	StageRendered:  4,
	StageSeen:      5,
}

var (
	ErrUnknownClient    = errors.New("uisub: unknown client")
	ErrStaleCursor      = errors.New("uisub: stale or regressive cursor")
	ErrWrongDigest      = errors.New("uisub: digest mismatch")
	ErrWrongProject     = errors.New("uisub: cross-project")
	ErrUnsupportedStage = errors.New("uisub: unsupported stage")
	ErrQueueOverflow    = errors.New("uisub: queue overflow")
	ErrInvalidAck       = errors.New("uisub: invalid acknowledgement")
)

// ClientIdentity is a registered UI client/session.
type ClientIdentity struct {
	ClientID       string
	SessionID      string
	AdapterVersion string
	ProjectID      string
	// Required means the client must receive all report kinds.
	Required bool
}

// Ack is acknowledgement evidence.
type Ack struct {
	ClientID  string
	SessionID string
	EventID   string
	Sequence  int64
	Digest    string
	Stage     AckStage
	At        time.Time
}

// ClientState is durable per-client delivery state.
type ClientState struct {
	Identity       ClientIdentity
	LastAccepted   int64
	LastStreamed   int64
	Queue          []uireport.Envelope
	OverflowCursor int64
	Closed         bool
}

// Ledger is the durable subscription and ack store (in-process for tests/core).
type Ledger struct {
	mu       sync.Mutex
	clients  map[string]*ClientState
	reports  []uireport.Envelope // project-scoped ordered reports
	project  string
	acks     map[string]Ack // key client|event|stage
	maxQueue int
	now      func() time.Time
}

// NewLedger creates an empty ledger for one project.
func NewLedger(projectID string, maxQueue int, now func() time.Time) *Ledger {
	if maxQueue <= 0 {
		maxQueue = 64
	}
	if now == nil {
		now = time.Now
	}
	return &Ledger{
		clients:  map[string]*ClientState{},
		project:  projectID,
		acks:     map[string]Ack{},
		maxQueue: maxQueue,
		now:      now,
	}
}

// RegisterClient registers a client for a project.
func (l *Ledger) RegisterClient(id ClientIdentity) error {
	if id.ClientID == "" || id.SessionID == "" || id.ProjectID == "" {
		return fmt.Errorf("uisub: incomplete client identity")
	}
	if id.ProjectID != l.project {
		return ErrWrongProject
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.clients[id.ClientID]; ok {
		return nil
	}
	l.clients[id.ClientID] = &ClientState{Identity: id, Queue: nil}
	return nil
}

// Publish appends a report to the project log. It does not block on clients.
func (l *Ledger) Publish(env uireport.Envelope) error {
	if env.ProjectID != l.project {
		return ErrWrongProject
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reports = append(l.reports, env)
	return nil
}

// Replay returns reports after cursor for a client and marks streamed.
// If the backlog exceeds maxQueue, the client is closed with a resume cursor
// (slow-client isolation) and no partial slice is returned.
func (l *Ledger) Replay(clientID string, afterSeq int64) ([]uireport.Envelope, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.clients[clientID]
	if !ok {
		return nil, ErrUnknownClient
	}
	if c.Closed {
		return nil, fmt.Errorf("%w: resume at cursor %d", ErrQueueOverflow, c.OverflowCursor)
	}
	var out []uireport.Envelope
	for _, r := range l.reports {
		if r.Sequence > afterSeq {
			out = append(out, r)
		}
	}
	// Bound page size; callers page with last cursor.
	// Unread backlog strictly larger than maxQueue closes the slow client.
	if len(out) > l.maxQueue {
		c.OverflowCursor = out[l.maxQueue-1].Sequence
		c.Closed = true
		return nil, fmt.Errorf("%w: resume at cursor %d", ErrQueueOverflow, c.OverflowCursor)
	}
	for _, r := range out {
		key := ackKey(clientID, r.EventID, StageStreamed)
		l.acks[key] = Ack{
			ClientID: clientID, SessionID: c.Identity.SessionID,
			EventID: r.EventID, Sequence: r.Sequence, Digest: r.ContentDigest,
			Stage: StageStreamed, At: l.now().UTC(),
		}
		if r.Sequence > c.LastStreamed {
			c.LastStreamed = r.Sequence
		}
	}
	return out, nil
}

// Acknowledge records a monotonic stage advance.
func (l *Ledger) Acknowledge(a Ack) error {
	if a.ClientID == "" || a.EventID == "" || a.Digest == "" || a.Stage == "" {
		return ErrInvalidAck
	}
	ord, ok := stageOrder[a.Stage]
	if !ok {
		return ErrUnsupportedStage
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.clients[a.ClientID]
	if !ok {
		return ErrUnknownClient
	}
	if c.Identity.ProjectID != l.project {
		return ErrWrongProject
	}
	// Find report
	var found *uireport.Envelope
	for i := range l.reports {
		if l.reports[i].EventID == a.EventID {
			found = &l.reports[i]
			break
		}
	}
	if found == nil {
		return ErrInvalidAck
	}
	if found.ProjectID != l.project {
		return ErrWrongProject
	}
	if a.Digest != found.ContentDigest {
		return ErrWrongDigest
	}
	if a.Sequence != 0 && a.Sequence != found.Sequence {
		return ErrInvalidAck
	}
	// Reject regressive stage
	for stage, o := range stageOrder {
		if o >= ord {
			continue
		}
		// ensure lower stages exist? Monotonic from current max for this event
		_ = stage
	}
	maxOrd := 0
	for stage, o := range stageOrder {
		key := ackKey(a.ClientID, a.EventID, stage)
		if _, ok := l.acks[key]; ok && o > maxOrd {
			maxOrd = o
		}
	}
	if ord < maxOrd {
		return ErrStaleCursor
	}
	if ord == maxOrd {
		// duplicate same stage ok (idempotent)
		return nil
	}
	if a.At.IsZero() {
		a.At = l.now().UTC()
	}
	a.SessionID = c.Identity.SessionID
	a.Sequence = found.Sequence
	l.acks[ackKey(a.ClientID, a.EventID, a.Stage)] = a
	if a.Stage == StageAccepted || a.Stage == StageRendered || a.Stage == StageSeen {
		if found.Sequence > c.LastAccepted {
			c.LastAccepted = found.Sequence
		}
	}
	return nil
}

// LastAcceptedCursor returns the resume cursor for reconnect.
func (l *Ledger) LastAcceptedCursor(clientID string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.clients[clientID]
	if !ok {
		return 0, ErrUnknownClient
	}
	return c.LastAccepted, nil
}

// AckEvidence returns stored ack if present.
func (l *Ledger) AckEvidence(clientID, eventID string, stage AckStage) (Ack, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.acks[ackKey(clientID, eventID, stage)]
	return a, ok
}

func ackKey(clientID, eventID string, stage AckStage) string {
	return clientID + "|" + eventID + "|" + string(stage)
}
