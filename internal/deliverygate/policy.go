package deliverygate

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attention"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

const (
	SchemaSnapshot = "loopcoder.delivery.policy.v1"
	SchemaDecision = "loopcoder.delivery.decision.v1"
)

// MissedPolicy is the frozen two-miss outcome.
type MissedPolicy string

const (
	MissedStop   MissedPolicy = "stop"
	MissedDetach MissedPolicy = "detach"
)

// DeliveryState is the enforcement phase.
type DeliveryState string

const (
	StatePendingLaunch DeliveryState = "pending_launch"
	StateLive          DeliveryState = "live"
	StateDegraded      DeliveryState = "delivery_degraded"
	StateBlocked       DeliveryState = "blocked_pre_launch"
	StateStopped       DeliveryState = "stopped_by_policy"
	StateDetached      DeliveryState = "detached_by_policy"
)

var (
	ErrInvalidSnapshot = errors.New("deliverygate: invalid snapshot")
	ErrNotLaunched     = errors.New("deliverygate: provider not launchable")
	ErrNoFallback      = errors.New("deliverygate: fallback not connected")
	ErrUnknownClient   = errors.New("deliverygate: unknown client")
)

// ClientSpec freezes a UI client expectation in the run snapshot.
type ClientSpec struct {
	ClientID string
	Required bool
	// Mode is a named UI mode (terminal, bridge, …) — not host detection.
	Mode string
}

// Snapshot freezes delivery policy for a run.
type Snapshot struct {
	Schema             string        `json:"schema"`
	ProjectID          string        `json:"project_id"`
	RunID              string        `json:"run_id"`
	AttemptID          string        `json:"attempt_id"`
	RequiredClients    []ClientSpec  `json:"required_clients"`
	OptionalClients    []ClientSpec  `json:"optional_clients"`
	AllowedFallbacks   []string      `json:"allowed_fallbacks"` // client IDs that may substitute
	AckDeadline        time.Duration `json:"ack_deadline"`
	MissedReportPolicy MissedPolicy  `json:"missed_report_policy"`
	ReportInterval     time.Duration `json:"report_interval"`
}

// Decision is a typed pre-launch or enforcement outcome.
type Decision struct {
	Schema       string        `json:"schema"`
	AllowLaunch  bool          `json:"allow_launch"`
	State        DeliveryState `json:"state"`
	Reason       string        `json:"reason"`
	Remediation  string        `json:"remediation,omitempty"`
	ActiveClient string        `json:"active_client,omitempty"`
	FallbackUsed string        `json:"fallback_used,omitempty"`
	At           time.Time     `json:"at"`
}

// Gate enforces delivery policy against a uisub ledger (and optional attention store).
type Gate struct {
	mu           sync.Mutex
	snap         Snapshot
	ledger       *uisub.Ledger
	attention    *attention.Store
	now          func() time.Time
	state        DeliveryState
	missed       int
	activeClient string
	fallbackUsed string
	// startEventID is the event that must be rendered before launch.
	startEventID string
	startDigest  string
	// last mandatory report tracking
	lastMandatorySeq int64
	lastAckedSeq     int64
	// report generation remains active regardless of UI
	reportsGenerated int
	// cleanup independent flags
	cleanupDone bool
}

// New builds a gate for a frozen snapshot.
func New(snap Snapshot, ledger *uisub.Ledger, att *attention.Store, now func() time.Time) (*Gate, error) {
	if snap.ProjectID == "" || snap.AttemptID == "" {
		return nil, fmt.Errorf("%w: missing project/attempt", ErrInvalidSnapshot)
	}
	if len(snap.RequiredClients) == 0 {
		return nil, fmt.Errorf("%w: at least one required client", ErrInvalidSnapshot)
	}
	if snap.AckDeadline <= 0 {
		snap.AckDeadline = 30 * time.Second
	}
	if snap.ReportInterval <= 0 {
		snap.ReportInterval = 5 * time.Minute
	}
	if snap.MissedReportPolicy == "" {
		snap.MissedReportPolicy = MissedStop
	}
	if snap.MissedReportPolicy != MissedStop && snap.MissedReportPolicy != MissedDetach {
		return nil, fmt.Errorf("%w: missed policy", ErrInvalidSnapshot)
	}
	if snap.Schema == "" {
		snap.Schema = SchemaSnapshot
	}
	if now == nil {
		now = time.Now
	}
	if ledger == nil {
		return nil, fmt.Errorf("%w: ledger required", ErrInvalidSnapshot)
	}
	return &Gate{
		snap:      snap,
		ledger:    ledger,
		attention: att,
		now:       now,
		state:     StatePendingLaunch,
	}, nil
}

// RecordStartReport binds the start report that must be rendered pre-launch.
func (g *Gate) RecordStartReport(eventID, digest string, seq int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.startEventID = eventID
	g.startDigest = digest
	g.lastMandatorySeq = seq
	g.reportsGenerated++
}

// NoteReportGenerated tracks that report generation continued (outage-safe).
func (g *Gate) NoteReportGenerated(seq int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reportsGenerated++
	g.lastMandatorySeq = seq
}

// PreLaunch evaluates whether provider launch is allowed.
func (g *Gate) PreLaunch() Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now().UTC()
	if g.startEventID == "" || g.startDigest == "" {
		g.state = StateBlocked
		return Decision{
			Schema: SchemaDecision, AllowLaunch: false, State: StateBlocked,
			Reason: "start_report_missing", Remediation: "emit start report before launch gate",
			At: now,
		}
	}
	// Required clients must be registered (connected) — no host invention.
	var connectedRequired []string
	for _, c := range g.snap.RequiredClients {
		// Probe via Replay which requires registration.
		if _, err := g.ledger.Replay(c.ClientID, 0); err != nil {
			if errors.Is(err, uisub.ErrUnknownClient) {
				continue
			}
			// overflow still means client exists
			if errors.Is(err, uisub.ErrQueueOverflow) {
				connectedRequired = append(connectedRequired, c.ClientID)
			}
			continue
		}
		connectedRequired = append(connectedRequired, c.ClientID)
	}
	if len(connectedRequired) == 0 {
		// try allowed fallbacks that are connected
		fb, ok := g.firstConnectedFallbackLocked()
		if !ok {
			g.state = StateBlocked
			return Decision{
				Schema: SchemaDecision, AllowLaunch: false, State: StateBlocked,
				Reason: "no_required_ui_connected", Remediation: "connect required UI or named fallback client",
				At: now,
			}
		}
		// fallback must still have start:rendered
		if !g.hasRenderedLocked(fb, g.startEventID, g.startDigest) {
			g.state = StateBlocked
			return Decision{
				Schema: SchemaDecision, AllowLaunch: false, State: StateBlocked,
				Reason: "fallback_start_not_rendered", Remediation: "fallback must ack start:rendered",
				ActiveClient: fb, FallbackUsed: fb, At: now,
			}
		}
		g.state = StateLive
		g.activeClient = fb
		g.fallbackUsed = fb
		g.lastAckedSeq = g.lastMandatorySeq
		return Decision{
			Schema: SchemaDecision, AllowLaunch: true, State: StateLive,
			Reason: "fallback_start_rendered", ActiveClient: fb, FallbackUsed: fb, At: now,
		}
	}
	// Any required with start:rendered satisfies gate.
	for _, id := range connectedRequired {
		if g.hasRenderedLocked(id, g.startEventID, g.startDigest) {
			g.state = StateLive
			g.activeClient = id
			g.lastAckedSeq = g.lastMandatorySeq
			return Decision{
				Schema: SchemaDecision, AllowLaunch: true, State: StateLive,
				Reason: "required_start_rendered", ActiveClient: id, At: now,
			}
		}
	}
	g.state = StateBlocked
	return Decision{
		Schema: SchemaDecision, AllowLaunch: false, State: StateBlocked,
		Reason: "start_not_rendered", Remediation: "required client must acknowledge start:rendered",
		At: now,
	}
}

// OnMandatoryInterval evaluates missed acknowledgement for a mandatory report tick.
// Call once per report interval after NoteReportGenerated for the mandatory report.
func (g *Gate) OnMandatoryInterval() Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now().UTC()
	if g.state == StatePendingLaunch || g.state == StateBlocked {
		return Decision{Schema: SchemaDecision, AllowLaunch: false, State: g.state, Reason: "not_live", At: now}
	}
	if g.state == StateStopped || g.state == StateDetached {
		return Decision{Schema: SchemaDecision, AllowLaunch: false, State: g.state, Reason: "already_terminal_policy", At: now}
	}
	acked := false
	if g.activeClient != "" {
		// Check last accepted cursor advanced for mandatory seq, or rendered ack on last event.
		cur, err := g.ledger.LastAcceptedCursor(g.activeClient)
		if err == nil && cur >= g.lastMandatorySeq {
			acked = true
		}
	}
	if acked {
		g.missed = 0
		if g.state == StateDegraded {
			g.state = StateLive
		}
		g.lastAckedSeq = g.lastMandatorySeq
		return Decision{
			Schema: SchemaDecision, AllowLaunch: true, State: g.state,
			Reason: "mandatory_acked", ActiveClient: g.activeClient, FallbackUsed: g.fallbackUsed, At: now,
		}
	}
	g.missed++
	if g.missed == 1 {
		g.state = StateDegraded
		// surface attention if store available
		if g.attention != nil {
			_, _ = g.attention.Open(attention.OpenInput{
				ProjectID: g.snap.ProjectID, RunID: g.snap.RunID, AttemptID: g.snap.AttemptID,
				RunRevision: 1, Kind: attention.KindDeliveryBlock, Severity: attention.SeverityWarn,
				Reason: "missed_mandatory_report_ack",
				Evidence: map[string]string{
					"missed": "1", "active_client": g.activeClient,
				},
			})
		}
		return Decision{
			Schema: SchemaDecision, AllowLaunch: true, State: StateDegraded,
			Reason: "delivery_degraded", Remediation: "reconnect UI and ack rendered",
			ActiveClient: g.activeClient, At: now,
		}
	}
	// two consecutive misses
	switch g.snap.MissedReportPolicy {
	case MissedDetach:
		g.state = StateDetached
		return Decision{
			Schema: SchemaDecision, AllowLaunch: false, State: StateDetached,
			Reason: "two_missed_detach", Remediation: "operator detached by frozen policy",
			ActiveClient: g.activeClient, At: now,
		}
	default:
		g.state = StateStopped
		return Decision{
			Schema: SchemaDecision, AllowLaunch: false, State: StateStopped,
			Reason: "two_missed_stop", Remediation: "attempt stopped by frozen policy",
			ActiveClient: g.activeClient, At: now,
		}
	}
}

// TryFallback switches to a named connected fallback after its own rendered ack of start.
func (g *Gate) TryFallback(startEventID, digest string) (Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now().UTC()
	fb, ok := g.firstConnectedFallbackLocked()
	if !ok {
		return Decision{}, ErrNoFallback
	}
	if !g.hasRenderedLocked(fb, startEventID, digest) {
		return Decision{
			Schema: SchemaDecision, AllowLaunch: false, State: g.state,
			Reason: "fallback_not_rendered", ActiveClient: fb, At: now,
		}, fmt.Errorf("%w: rendered required", ErrNoFallback)
	}
	g.activeClient = fb
	g.fallbackUsed = fb
	g.missed = 0
	g.state = StateLive
	return Decision{
		Schema: SchemaDecision, AllowLaunch: true, State: StateLive,
		Reason: "fallback_active", ActiveClient: fb, FallbackUsed: fb, At: now,
	}, nil
}

// MarkCleanupDone records process cleanup independent of UI.
func (g *Gate) MarkCleanupDone() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cleanupDone = true
}

// CleanupDone reports whether cleanup completed (UI-independent).
func (g *Gate) CleanupDone() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cleanupDone
}

// ReportsGenerated counts reports even during outage.
func (g *Gate) ReportsGenerated() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reportsGenerated
}

// State returns current delivery state.
func (g *Gate) State() DeliveryState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// ActiveClient returns the client currently satisfying policy.
func (g *Gate) ActiveClient() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.activeClient
}

// Snapshot returns the frozen policy.
func (g *Gate) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snap
}

// Reconnect clears degradation after matching rendered evidence on active or required client.
func (g *Gate) Reconnect(clientID, eventID, digest string) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now().UTC()
	if !g.hasRenderedLocked(clientID, eventID, digest) {
		return Decision{
			Schema: SchemaDecision, AllowLaunch: g.state == StateLive || g.state == StateDegraded,
			State: g.state, Reason: "reconnect_without_matching_ack", At: now,
		}
	}
	g.activeClient = clientID
	g.missed = 0
	if g.state == StateDegraded {
		g.state = StateLive
	}
	return Decision{
		Schema: SchemaDecision, AllowLaunch: true, State: g.state,
		Reason: "reconnect_ack_cleared", ActiveClient: clientID, At: now,
	}
}

func (g *Gate) hasRenderedLocked(clientID, eventID, digest string) bool {
	if eventID == "" || digest == "" {
		return false
	}
	_, ok := g.ledger.AckEvidence(clientID, eventID, uisub.StageRendered)
	if !ok {
		return false
	}
	// digest match: AckEvidence stores digest on ack
	a, ok := g.ledger.AckEvidence(clientID, eventID, uisub.StageRendered)
	if !ok {
		return false
	}
	return a.Digest == digest
}

func (g *Gate) firstConnectedFallbackLocked() (string, bool) {
	for _, id := range g.snap.AllowedFallbacks {
		if _, err := g.ledger.Replay(id, 0); err != nil {
			if errors.Is(err, uisub.ErrUnknownClient) {
				continue
			}
			// overflow still connected
			if errors.Is(err, uisub.ErrQueueOverflow) {
				return id, true
			}
			continue
		}
		return id, true
	}
	return "", false
}
