package obsrefresh

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

const (
	SchemaState    = "loopcoder.obs.refresh.state.v1"
	SchemaHealth   = "loopcoder.obs.health.v1"
	SchemaCooldown = "loopcoder.obs.cooldown.v1"
	SchemaResult   = "loopcoder.obs.refresh.result.v1"
)

// HealthClass is distinct from capacity facts.
type HealthClass string

const (
	HealthHealthy     HealthClass = "healthy"
	HealthUnknown     HealthClass = "unknown"
	HealthStale       HealthClass = "stale"
	HealthUnavailable HealthClass = "unavailable"
	HealthCooldown    HealthClass = "cooldown"
	HealthDegraded    HealthClass = "degraded"
)

// CooldownScope is the safety boundary for a cooldown.
type CooldownScope string

const (
	ScopeSource  CooldownScope = "source"
	ScopeAccount CooldownScope = "account"
	ScopeAdapter CooldownScope = "adapter"
)

var (
	ErrInvalid    = errors.New("obsrefresh: invalid")
	ErrInFlight   = errors.New("obsrefresh: in flight")
	ErrCooldowned = errors.New("obsrefresh: cooldown active")
	ErrNoOverride = errors.New("obsrefresh: cooldown requires explicit override")
)

// Config bounds adaptive refresh.
type Config struct {
	TTL               time.Duration
	SuccessBackoff    time.Duration
	FailureBackoff    time.Duration
	MaxFailureBackoff time.Duration
	// JitterFrac is [0,1); applied as +/- fraction of backoff.
	JitterFrac float64
	// MinInterval is the floor between probes for a source.
	MinInterval time.Duration
}

// DefaultConfig returns conservative defaults.
func DefaultConfig() Config {
	return Config{
		TTL: 5 * time.Minute, SuccessBackoff: 30 * time.Second,
		FailureBackoff: 15 * time.Second, MaxFailureBackoff: 10 * time.Minute,
		JitterFrac: 0.1, MinInterval: 5 * time.Second,
	}
}

// Cooldown is a safety hold.
type Cooldown struct {
	Schema    string        `json:"schema"`
	Scope     CooldownScope `json:"scope"`
	Reason    string        `json:"reason"`
	ExpiresAt time.Time     `json:"expires_at"`
	// ProtectsAccount is true when bypass needs explicit override.
	ProtectsAccount bool `json:"protects_account"`
}

// SourceState is durable per-source refresh state (machine-scoped).
type SourceState struct {
	Schema        string      `json:"schema"`
	SourceID      string      `json:"source_id"`
	AdapterID     string      `json:"adapter_id"`
	Health        HealthClass `json:"health"`
	LastSuccessAt time.Time   `json:"last_success_at,omitempty"`
	LastAttemptAt time.Time   `json:"last_attempt_at,omitempty"`
	NextRefreshAt time.Time   `json:"next_refresh_at,omitempty"`
	FailureStreak int         `json:"failure_streak"`
	InFlight      bool        `json:"in_flight"`
	// LastObservationDigest is preserved on failure (never deleted).
	LastObservationDigest string    `json:"last_observation_digest,omitempty"`
	LastFacts             []string  `json:"last_facts,omitempty"` // redacted keys only
	Cooldown              *Cooldown `json:"cooldown,omitempty"`
	// InstallationKnown survives failures.
	InstallationKnown bool `json:"installation_known"`
}

// RefreshResult is one demand-refresh outcome.
type RefreshResult struct {
	Schema      string      `json:"schema"`
	SourceID    string      `json:"source_id"`
	Probed      bool        `json:"probed"`
	Reused      bool        `json:"reused"`
	Health      HealthClass `json:"health"`
	Message     string      `json:"message,omitempty"`
	Observation string      `json:"observation_digest,omitempty"`
}

// Probe is injected observation work (no provider model runner).
// Returns ok, observation digest, fact keys, installKnown, typed failure reason.
type Probe func(sourceID string) (ok bool, digest string, factKeys []string, installKnown bool, failReason string)

// Clock is injectable time.
type Clock interface {
	Now() time.Time
}

// MemoryClock for tests.
type MemoryClock struct{ T time.Time }

func (c *MemoryClock) Now() time.Time          { return c.T.UTC() }
func (c *MemoryClock) Advance(d time.Duration) { c.T = c.T.Add(d) }

// Jitter derives deterministic jitter from a counter (no math/rand race).
func deterministicJitter(base time.Duration, frac float64, n int) time.Duration {
	if frac <= 0 || base <= 0 {
		return base
	}
	// alternate +/- using n
	mag := time.Duration(float64(base) * frac)
	if n%2 == 0 {
		return base + mag
	}
	if base > mag {
		return base - mag
	}
	return base
}

// Manager is the machine-scoped refresh coordinator.
type Manager struct {
	mu     sync.Mutex
	cfg    Config
	clock  Clock
	probe  Probe
	states map[string]*SourceState
	// waiters coalesce concurrent demand for same source
	waiters map[string]int
	seq     int
}

// NewManager builds a manager. probe required for actual probes.
func NewManager(cfg Config, clock Clock, probe Probe) *Manager {
	if cfg.TTL <= 0 {
		cfg = DefaultConfig()
	}
	if clock == nil {
		clock = &MemoryClock{T: time.Now().UTC()}
	}
	return &Manager{
		cfg: cfg, clock: clock, probe: probe,
		states: map[string]*SourceState{}, waiters: map[string]int{},
	}
}

// Checkpoint returns a copy of all source states for restart.
func (m *Manager) Checkpoint() []SourceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SourceState, 0, len(m.states))
	for _, st := range m.states {
		cp := *st
		if st.Cooldown != nil {
			c := *st.Cooldown
			cp.Cooldown = &c
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceID < out[j].SourceID })
	return out
}

// Restore loads checkpoint after restart (clears in-flight).
func (m *Manager) Restore(states []SourceState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states = map[string]*SourceState{}
	for _, st := range states {
		cp := st
		cp.InFlight = false
		cp.Schema = SchemaState
		m.states[st.SourceID] = &cp
	}
}

// DemandRefresh reuses fresh evidence or probes once per source scope.
// projectID is accepted for coalesce accounting only (machine-scoped probe).
func (m *Manager) DemandRefresh(projectID, adapterID, sourceID string) (RefreshResult, error) {
	if sourceID == "" || adapterID == "" {
		return RefreshResult{}, ErrInvalid
	}
	now := m.clock.Now()

	m.mu.Lock()
	st := m.ensureLocked(adapterID, sourceID)
	// active cooldown with account protection
	if st.Cooldown != nil && now.Before(st.Cooldown.ExpiresAt) {
		if st.Cooldown.ProtectsAccount {
			res := RefreshResult{Schema: SchemaResult, SourceID: sourceID, Probed: false, Reused: true, Health: HealthCooldown, Message: "cooldown_active", Observation: st.LastObservationDigest}
			m.mu.Unlock()
			return res, ErrCooldowned
		}
	} else if st.Cooldown != nil && !now.Before(st.Cooldown.ExpiresAt) {
		st.Cooldown = nil
		if st.Health == HealthCooldown {
			st.Health = HealthStale
		}
	}

	// Fresh?
	if !st.LastSuccessAt.IsZero() && now.Sub(st.LastSuccessAt) < m.cfg.TTL && st.Health == HealthHealthy {
		res := RefreshResult{Schema: SchemaResult, SourceID: sourceID, Probed: false, Reused: true, Health: HealthHealthy, Observation: st.LastObservationDigest, Message: "fresh"}
		m.mu.Unlock()
		return res, nil
	}

	// Next refresh not due (backoff)
	if !st.NextRefreshAt.IsZero() && now.Before(st.NextRefreshAt) && st.Health != HealthHealthy {
		// still reuse last with stale
		h := st.Health
		if h == HealthHealthy {
			h = HealthStale
		}
		res := RefreshResult{Schema: SchemaResult, SourceID: sourceID, Probed: false, Reused: true, Health: h, Observation: st.LastObservationDigest, Message: "backoff_wait"}
		m.mu.Unlock()
		return res, nil
	}

	// In-flight dedup
	if st.InFlight {
		m.waiters[sourceID]++
		res := RefreshResult{Schema: SchemaResult, SourceID: sourceID, Probed: false, Reused: true, Health: st.Health, Observation: st.LastObservationDigest, Message: "coalesced_in_flight"}
		m.mu.Unlock()
		return res, nil
	}

	st.InFlight = true
	st.LastAttemptAt = now
	m.seq++
	seq := m.seq
	m.mu.Unlock()

	// Probe outside lock (still zero model — probe is injected)
	if m.probe == nil {
		m.mu.Lock()
		st = m.states[sourceID]
		st.InFlight = false
		m.mu.Unlock()
		return RefreshResult{}, fmt.Errorf("%w: probe required", ErrInvalid)
	}
	ok, digest, facts, installKnown, failReason := m.probe(sourceID)

	m.mu.Lock()
	defer m.mu.Unlock()
	st = m.states[sourceID]
	st.InFlight = false
	delete(m.waiters, sourceID)

	if ok {
		st.LastSuccessAt = now
		st.FailureStreak = 0
		st.Health = HealthHealthy
		st.LastObservationDigest = digest
		st.LastFacts = append([]string(nil), facts...)
		if installKnown {
			st.InstallationKnown = true
		}
		backoff := deterministicJitter(m.cfg.SuccessBackoff, m.cfg.JitterFrac, seq)
		if backoff < m.cfg.MinInterval {
			backoff = m.cfg.MinInterval
		}
		st.NextRefreshAt = now.Add(maxDuration(m.cfg.TTL, backoff))
		st.Cooldown = nil
		return RefreshResult{Schema: SchemaResult, SourceID: sourceID, Probed: true, Reused: false, Health: HealthHealthy, Observation: digest, Message: "probed_ok"}, nil
	}

	// Failure: preserve last observation, mark stale/unavailable — never delete install
	st.FailureStreak++
	if installKnown {
		st.InstallationKnown = true
	}
	// Do not clear LastObservationDigest or InstallationKnown
	prevDigest := st.LastObservationDigest
	st.Health = HealthStale
	if failReason == "unavailable" {
		st.Health = HealthUnavailable
	} else if failReason == "unknown" {
		st.Health = HealthUnknown
	}

	// failure backoff exponential-ish with cap + jitter
	base := m.cfg.FailureBackoff * time.Duration(int(math.Pow(2, float64(min(st.FailureStreak-1, 6)))))
	if base > m.cfg.MaxFailureBackoff {
		base = m.cfg.MaxFailureBackoff
	}
	base = deterministicJitter(base, m.cfg.JitterFrac, seq)
	if base < m.cfg.MinInterval {
		base = m.cfg.MinInterval
	}
	st.NextRefreshAt = now.Add(base)

	// Rate-limit style failures enter account-protecting cooldown
	if failReason == "rate_limit" {
		st.Cooldown = &Cooldown{
			Schema: SchemaCooldown, Scope: ScopeAccount, Reason: "rate_limit",
			ExpiresAt: now.Add(base), ProtectsAccount: true,
		}
		st.Health = HealthCooldown
	}

	return RefreshResult{
		Schema: SchemaResult, SourceID: sourceID, Probed: true, Reused: false,
		Health: st.Health, Observation: prevDigest, Message: "probed_fail:" + failReason,
	}, nil
}

// ManualRefresh attempts every source; cannot bypass protecting cooldown without override.
func (m *Manager) ManualRefresh(adapterID string, sourceIDs []string, overrideEvidence string) ([]RefreshResult, error) {
	var out []RefreshResult
	now := m.clock.Now()
	for _, sid := range sourceIDs {
		m.mu.Lock()
		st := m.ensureLocked(adapterID, sid)
		if st.Cooldown != nil && now.Before(st.Cooldown.ExpiresAt) && st.Cooldown.ProtectsAccount {
			if overrideEvidence == "" {
				out = append(out, RefreshResult{
					Schema: SchemaResult, SourceID: sid, Probed: false, Reused: true,
					Health: HealthCooldown, Message: "manual_blocked_cooldown", Observation: st.LastObservationDigest,
				})
				m.mu.Unlock()
				continue
			}
			// explicit override clears cooldown
			st.Cooldown = nil
			st.NextRefreshAt = time.Time{}
		}
		m.mu.Unlock()
		res, err := m.DemandRefresh("manual", adapterID, sid)
		if err != nil && !errors.Is(err, ErrCooldowned) {
			return out, err
		}
		if res.Message == "" && errors.Is(err, ErrCooldowned) {
			res.Message = "cooldown"
		}
		out = append(out, res)
	}
	return out, nil
}

// Get returns source state copy.
func (m *Manager) Get(sourceID string) (SourceState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[sourceID]
	if !ok {
		return SourceState{}, false
	}
	cp := *st
	return cp, true
}

// HasProviderRunner is always false.
func (m *Manager) HasProviderRunner() bool { return false }

func (m *Manager) ensureLocked(adapterID, sourceID string) *SourceState {
	st, ok := m.states[sourceID]
	if !ok {
		st = &SourceState{
			Schema: SchemaState, SourceID: sourceID, AdapterID: adapterID,
			Health: HealthUnknown,
		}
		m.states[sourceID] = st
	}
	return st
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RenderHealth never maps unknown/stale/unavailable to healthy or zero capacity.
func RenderHealth(h HealthClass) (label string, capacityClaim string) {
	switch h {
	case HealthHealthy:
		return "healthy", "use_last_facts"
	case HealthStale:
		return "stale", "unknown_capacity"
	case HealthUnknown:
		return "unknown", "unknown_capacity"
	case HealthUnavailable:
		return "unavailable", "unknown_capacity"
	case HealthCooldown:
		return "cooldown", "unknown_capacity"
	case HealthDegraded:
		return "degraded", "unknown_capacity"
	default:
		return "unknown", "unknown_capacity"
	}
}
