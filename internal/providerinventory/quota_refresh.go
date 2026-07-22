package providerinventory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	QuotaRefreshStatusSchema = "loopcoder.provider_quota_refresh_status.v1"

	DefaultQuotaRefreshCadence = 5 * time.Minute
)

type RefreshTrigger string

const (
	RefreshTriggerExplicit         RefreshTrigger = "explicit"
	RefreshTriggerStartup          RefreshTrigger = "startup"
	RefreshTriggerStaleTTL         RefreshTrigger = "stale-ttl"
	RefreshTriggerApproachingReset RefreshTrigger = "approaching-reset"
	RefreshTriggerProviderError    RefreshTrigger = "provider-error"
	RefreshTriggerPeriodic         RefreshTrigger = "periodic"
)

type RefreshPolicy struct {
	PeriodicCadence        time.Duration
	MinPeriodicCadence     time.Duration
	MaxPeriodicCadence     time.Duration
	MaxJitter              time.Duration
	StaleTTL               time.Duration
	ApproachingResetWithin time.Duration
	ProviderTimeout        time.Duration
	GlobalDeadline         time.Duration
	MaxParallelism         int
	JitterSeed             string
}

type RefreshRequest struct {
	RepoPath                string
	Config                  config.Config
	RuntimeContract         runtimecap.Contract
	NetworkGrants           []NetworkGrant
	Providers               []string
	Trigger                 RefreshTrigger
	Policy                  RefreshPolicy
	Now                     func() time.Time
	AfterFreshCapacityEvent func(context.Context, RefreshResult) error
}

type RefreshResult struct {
	Report    Report                  `json:"report"`
	Status    QuotaRefreshStatus      `json:"status"`
	Providers []ProviderRefreshResult `json:"providers"`
}

type ProviderRefreshResult struct {
	AdapterID        string         `json:"adapter_id"`
	Trigger          RefreshTrigger `json:"trigger"`
	Refreshed        bool           `json:"refreshed"`
	Coalesced        bool           `json:"coalesced,omitempty"`
	ErrorCode        string         `json:"error_code,omitempty"`
	StartedAt        string         `json:"started_at,omitempty"`
	CompletedAt      string         `json:"completed_at,omitempty"`
	NextRefreshAt    string         `json:"next_refresh_at,omitempty"`
	QuotaSnapshotIDs []string       `json:"quota_snapshot_ids"`
}

type QuotaRefreshStatus struct {
	SchemaVersion string                `json:"schema_version"`
	GeneratedAt   string                `json:"generated_at"`
	Providers     []ProviderQuotaStatus `json:"providers"`
	GapReasons    []string              `json:"gap_reasons"`
}

type ProviderQuotaStatus struct {
	AdapterID         string          `json:"adapter_id"`
	AgeMS             *int64          `json:"age_ms,omitempty"`
	SourceKind        QuotaSourceKind `json:"source_kind,omitempty"`
	Confidence        Confidence      `json:"confidence"`
	FreshnessState    FreshnessState  `json:"freshness_state"`
	TerminalErrorCode string          `json:"terminal_error_code,omitempty"`
	InFlight          bool            `json:"in_flight"`
	NextRefreshAt     string          `json:"next_refresh_at,omitempty"`
	CapturedAt        string          `json:"captured_at,omitempty"`
	ResetAt           string          `json:"reset_at,omitempty"`
	QuotaSnapshotIDs  []string        `json:"quota_snapshot_ids"`
	GapReasons        []string        `json:"gap_reasons"`
}

type QuotaCollector func(context.Context, Options, Deps) (Report, error)

type RefreshManager struct {
	Store     storage.Store
	Deps      Deps
	Collector QuotaCollector

	mu                sync.Mutex
	inFlight          map[string]*refreshCall
	active            map[string]int
	refreshGoroutines int
	idle              *sync.Cond

	afterPublish func()
}

type refreshCall struct {
	waiters int
	done    chan struct{}
	result  RefreshResult
	err     error
}

type providerCollectResult struct {
	provider string
	report   Report
	err      error
}

func NewRefreshManager(store storage.Store, deps Deps) *RefreshManager {
	manager := &RefreshManager{Store: store, Deps: normalizeDeps(deps), Collector: Discover}
	manager.idle = sync.NewCond(&manager.mu)
	return manager
}

func (m *RefreshManager) Refresh(ctx context.Context, req RefreshRequest) (RefreshResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil || m.Store == nil {
		return RefreshResult{}, errors.New("quota refresh: storage store is required")
	}
	m.ensureDefaults()
	now := normalizeNow(req.Now)().UTC()
	policy := normalizeRefreshPolicy(req.Policy)
	trigger := normalizeRefreshTrigger(req.Trigger)
	providers, inactiveProviders := selectActiveRefreshProviders(req.Config, req.Providers, req.NetworkGrants)
	cached, err := Load(ctx, m.Store)
	if err != nil {
		return RefreshResult{}, err
	}
	if trigger == "" {
		results := invalidTriggerRefreshResults(append(append([]string(nil), providers...), inactiveProviders...), req.Trigger, now)
		status := m.statusFromReport(cached, providers, policy, now)
		status.Providers = append(status.Providers, inactiveProviderQuotaStatuses(inactiveProviders, now)...)
		sort.Slice(status.Providers, func(i, j int) bool { return status.Providers[i].AdapterID < status.Providers[j].AdapterID })
		status.GapReasons = dedupeStrings(append(status.GapReasons, "invalid-trigger"))
		return RefreshResult{Report: cached, Status: status, Providers: results}, nil
	}
	inactiveResults := inactiveProviderRefreshResults(inactiveProviders, trigger, now)
	refreshProviders := providersForTrigger(cached, providers, trigger, policy, now)
	key := m.refreshRequestKey(req, trigger, refreshProviders, policy, now)
	if len(refreshProviders) == 0 {
		status := m.statusFromReport(cached, providers, policy, now)
		return RefreshResult{Report: cached, Status: status, Providers: inactiveResults}, nil
	}

	m.mu.Lock()
	if existing := m.inFlight[key]; existing != nil {
		existing.waiters++
		m.mu.Unlock()
		select {
		case <-existing.done:
			result := existing.result
			for i := range result.Providers {
				if result.Providers[i].ErrorCode == "" {
					result.Providers[i].Coalesced = true
				}
			}
			return result, existing.err
		case <-ctx.Done():
			return RefreshResult{}, ctx.Err()
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	m.inFlight[key] = call
	m.markActiveLocked(refreshProviders, 1)
	m.refreshGoroutines++
	m.mu.Unlock()

	go func(ctx context.Context) {
		result, err := m.runSharedRefresh(req, cached, refreshProviders, inactiveResults, policy, now)
		if err == nil && req.AfterFreshCapacityEvent != nil && refreshResultPublishedFreshCapacity(result) {
			err = req.AfterFreshCapacityEvent(ctx, result)
		}
		m.mu.Lock()
		call.result = result
		call.err = err
		m.markActiveLocked(refreshProviders, -1)
		close(call.done)
		afterPublish := m.afterPublish
		m.mu.Unlock()
		if afterPublish != nil {
			afterPublish()
		}
		m.mu.Lock()
		delete(m.inFlight, key)
		m.refreshGoroutines--
		if m.refreshGoroutines == 0 {
			m.idle.Broadcast()
		}
		m.mu.Unlock()
	}(ctx)

	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
		return RefreshResult{}, ctx.Err()
	}
}

func refreshResultPublishedFreshCapacity(result RefreshResult) bool {
	for _, provider := range result.Providers {
		if provider.Refreshed && len(provider.QuotaSnapshotIDs) > 0 && provider.ErrorCode == "" {
			return true
		}
	}
	return false
}

func (m *RefreshManager) Status(ctx context.Context, req RefreshRequest) (QuotaRefreshStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil || m.Store == nil {
		return QuotaRefreshStatus{}, errors.New("quota refresh status: storage store is required")
	}
	m.ensureDefaults()
	now := normalizeNow(req.Now)().UTC()
	report, err := Load(ctx, m.Store)
	if err != nil {
		return QuotaRefreshStatus{}, err
	}
	providers, inactiveProviders := selectActiveRefreshProviders(req.Config, req.Providers, req.NetworkGrants)
	status := m.statusFromReport(report, providers, normalizeRefreshPolicy(req.Policy), now)
	status.Providers = append(status.Providers, inactiveProviderQuotaStatuses(inactiveProviders, now)...)
	sort.Slice(status.Providers, func(i, j int) bool { return status.Providers[i].AdapterID < status.Providers[j].AdapterID })
	return status, nil
}

// Wait blocks until refresh publication goroutines started by this manager have
// fully released their in-flight bookkeeping.
func (m *RefreshManager) Wait() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.idle == nil {
		m.idle = sync.NewCond(&m.mu)
	}
	for m.refreshGoroutines > 0 {
		m.idle.Wait()
	}
	m.mu.Unlock()
}

func (m *RefreshManager) runSharedRefresh(req RefreshRequest, cached Report, providers []string, inactiveResults []ProviderRefreshResult, policy RefreshPolicy, now time.Time) (RefreshResult, error) {
	globalCtx := context.Background()
	var cancel context.CancelFunc
	if policy.GlobalDeadline > 0 {
		globalCtx, cancel = context.WithTimeout(globalCtx, policy.GlobalDeadline)
	} else {
		globalCtx, cancel = context.WithCancel(globalCtx)
	}
	defer cancel()

	started := formatTime(now)
	jobs := make(chan string)
	results := make(chan ProviderRefreshResult, len(providers))
	providerResults := make(map[string]ProviderRefreshResult, len(providers))
	for _, provider := range providers {
		providerResults[provider] = ProviderRefreshResult{
			AdapterID:     provider,
			Trigger:       normalizeRefreshTrigger(req.Trigger),
			Refreshed:     false,
			ErrorCode:     "",
			StartedAt:     "",
			CompletedAt:   "",
			NextRefreshAt: nextRefreshForReport(cached, provider, policy, now),
		}
	}
	parallelism := policy.MaxParallelism
	if parallelism > len(providers) {
		parallelism = len(providers)
	}
	if parallelism < 1 {
		parallelism = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for provider := range jobs {
				collect := m.collectProvider(globalCtx, req, provider, policy, now)
				result := ProviderRefreshResult{
					AdapterID:     provider,
					Trigger:       normalizeRefreshTrigger(req.Trigger),
					StartedAt:     started,
					CompletedAt:   formatTime(normalizeNow(req.Now)().UTC()),
					NextRefreshAt: nextRefreshForReport(cached, provider, policy, now),
				}
				if collect.err != nil {
					result.ErrorCode = quotaErrorCode(collect.err)
					stale := staleQuotaErrorReport(req.Config, cached, provider, result.ErrorCode, now, result.NextRefreshAt)
					if len(stale.QuotaSnapshots) > 0 {
						if err := Refresh(globalCtx, m.Store, stale, now); err != nil {
							result.ErrorCode = quotaErrorCode(err)
						} else {
							result.QuotaSnapshotIDs = quotaSnapshotIDs(stale.QuotaSnapshots)
						}
					}
					results <- result
					continue
				}
				if err := Refresh(globalCtx, m.Store, collect.report, now); err != nil {
					result.ErrorCode = quotaErrorCode(err)
					results <- result
					continue
				}
				result.Refreshed = true
				result.QuotaSnapshotIDs = quotaSnapshotIDs(collect.report.QuotaSnapshots)
				results <- result
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, provider := range providers {
			if globalCtx.Err() != nil {
				return
			}
			select {
			case jobs <- provider:
			case <-globalCtx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(results)

	for result := range results {
		providerResults[result.AdapterID] = result
	}
	completedAt := formatTime(normalizeNow(req.Now)().UTC())
	for _, provider := range providers {
		result := providerResults[provider]
		if result.ErrorCode == "" && !result.Refreshed {
			result.CompletedAt = completedAt
			result.ErrorCode = quotaErrorCode(globalCtx.Err())
			if result.ErrorCode == "" {
				result.ErrorCode = "ErrQuotaRefreshDeadlineExceeded"
			}
			providerResults[provider] = result
		}
	}
	orderedResults := make([]ProviderRefreshResult, 0, len(providers)+len(inactiveResults))
	for _, provider := range providers {
		orderedResults = append(orderedResults, providerResults[provider])
	}
	orderedResults = append(orderedResults, inactiveResults...)
	sort.Slice(orderedResults, func(i, j int) bool {
		return orderedResults[i].AdapterID < orderedResults[j].AdapterID
	})
	report, err := Load(context.Background(), m.Store)
	if err != nil {
		return RefreshResult{}, err
	}
	activeProviders, inactiveProviders := selectActiveRefreshProviders(req.Config, req.Providers, req.NetworkGrants)
	status := m.statusFromReport(report, activeProviders, policy, normalizeNow(req.Now)().UTC())
	status.Providers = append(status.Providers, inactiveProviderQuotaStatuses(inactiveProviders, now)...)
	sort.Slice(status.Providers, func(i, j int) bool { return status.Providers[i].AdapterID < status.Providers[j].AdapterID })
	return RefreshResult{Report: report, Status: status, Providers: orderedResults}, nil
}

func (m *RefreshManager) collectProvider(parent context.Context, req RefreshRequest, provider string, policy RefreshPolicy, now time.Time) providerCollectResult {
	providerCtx := parent
	var cancel context.CancelFunc
	if policy.ProviderTimeout > 0 {
		providerCtx, cancel = context.WithTimeout(parent, policy.ProviderTimeout)
	} else {
		providerCtx, cancel = context.WithCancel(parent)
	}
	defer cancel()
	out := make(chan providerCollectResult, 1)
	go func() {
		opts := Options{
			RepoPath:        req.RepoPath,
			Config:          req.Config,
			RuntimeContract: req.RuntimeContract,
			Now:             func() time.Time { return now },
			NetworkGrants:   req.NetworkGrants,
			ActiveProviders: []string{provider},
		}
		report, err := m.Collector(providerCtx, opts, m.Deps)
		out <- providerCollectResult{provider: provider, report: report, err: err}
	}()
	select {
	case result := <-out:
		return result
	case <-providerCtx.Done():
		return providerCollectResult{provider: provider, err: providerCtx.Err()}
	}
}

func (m *RefreshManager) statusFromReport(report Report, providers []string, policy RefreshPolicy, now time.Time) QuotaRefreshStatus {
	statuses := make([]ProviderQuotaStatus, 0, len(providers))
	for _, provider := range providers {
		snapshot, ok := latestQuotaSnapshot(report, provider)
		item := ProviderQuotaStatus{
			AdapterID:      provider,
			Confidence:     ConfidenceUnknown,
			FreshnessState: FreshnessStale,
			InFlight:       m.isActive(provider),
			NextRefreshAt:  formatTime(now),
			GapReasons:     []string{"quota-cache-missing"},
		}
		if ok {
			item.SourceKind = snapshot.SourceKind
			item.Confidence = snapshot.Confidence
			item.FreshnessState = snapshot.FreshnessState
			item.TerminalErrorCode = snapshot.TerminalErrorCode
			item.CapturedAt = snapshot.CapturedAt
			item.ResetAt = snapshot.ResetAt
			item.QuotaSnapshotIDs = []string{snapshot.QuotaSnapshotID}
			item.GapReasons = append([]string(nil), snapshot.GapReasons...)
			item.NextRefreshAt = nextRefreshForProvider(snapshot, provider, policy, now)
			if captured, err := time.Parse(time.RFC3339Nano, snapshot.CapturedAt); err == nil {
				age := now.Sub(captured.UTC()).Milliseconds()
				if age < 0 {
					age = 0
					item.GapReasons = dedupeStrings(append(item.GapReasons, "clock-skew"))
				}
				item.AgeMS = &age
			} else {
				item.GapReasons = dedupeStrings(append(item.GapReasons, "captured-at-unparseable"))
			}
		}
		statuses = append(statuses, item)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].AdapterID < statuses[j].AdapterID })
	return QuotaRefreshStatus{
		SchemaVersion: QuotaRefreshStatusSchema,
		GeneratedAt:   formatTime(now),
		Providers:     statuses,
		GapReasons:    []string{},
	}
}

func (m *RefreshManager) ensureDefaults() {
	if m.Deps.RunProbe == nil {
		m.Deps = normalizeDeps(m.Deps)
	}
	if m.Collector == nil {
		m.Collector = Discover
	}
	if m.inFlight == nil {
		m.inFlight = map[string]*refreshCall{}
	}
	if m.active == nil {
		m.active = map[string]int{}
	}
	if m.idle == nil {
		m.idle = sync.NewCond(&m.mu)
	}
}

func (m *RefreshManager) markActiveLocked(providers []string, delta int) {
	for _, provider := range providers {
		next := m.active[provider] + delta
		if next <= 0 {
			delete(m.active, provider)
		} else {
			m.active[provider] = next
		}
	}
}

func (m *RefreshManager) isActive(provider string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[provider] > 0
}

func selectActiveRefreshProviders(cfg config.Config, requested []string, grants []NetworkGrant) ([]string, []string) {
	configured := configuredProviderNames(cfg)
	// Grants expand the active set so --grant-quota-telemetry all can refresh
	// codex+claude+grok even when delivery only configures worker/verifier.
	active := map[string]bool{}
	for _, provider := range configured {
		if provider != "" {
			active[provider] = true
		}
	}
	for _, g := range grants {
		p := strings.TrimSpace(g.ProviderID)
		if p != "" && refreshableProvider(cfg, p) {
			active[p] = true
		}
	}
	if len(requested) == 0 {
		// No explicit list: refresh configured + granted adapters.
		out := make([]string, 0, len(active))
		for p := range active {
			out = append(out, p)
		}
		sort.Strings(out)
		return out, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(requested))
	inactive := make([]string, 0)
	for _, provider := range requested {
		provider = strings.TrimSpace(provider)
		if provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		if active[provider] {
			out = append(out, provider)
		} else {
			inactive = append(inactive, provider)
		}
	}
	sort.Strings(out)
	sort.Strings(inactive)
	return out, inactive
}

func inactiveProviderRefreshResults(providers []string, trigger RefreshTrigger, now time.Time) []ProviderRefreshResult {
	results := make([]ProviderRefreshResult, 0, len(providers))
	for _, provider := range providers {
		results = append(results, ProviderRefreshResult{
			AdapterID:     provider,
			Trigger:       trigger,
			Refreshed:     false,
			ErrorCode:     "ErrQuotaRefreshProviderInactive",
			StartedAt:     "",
			CompletedAt:   formatTime(now),
			NextRefreshAt: formatTime(now),
		})
	}
	return results
}

func invalidTriggerRefreshResults(providers []string, trigger RefreshTrigger, now time.Time) []ProviderRefreshResult {
	providers = canonicalProviderList(providers)
	results := make([]ProviderRefreshResult, 0, len(providers))
	for _, provider := range providers {
		results = append(results, ProviderRefreshResult{
			AdapterID:     provider,
			Trigger:       trigger,
			Refreshed:     false,
			ErrorCode:     "ErrQuotaRefreshInvalidTrigger",
			CompletedAt:   formatTime(now),
			NextRefreshAt: formatTime(now),
		})
	}
	return results
}

func inactiveProviderQuotaStatuses(providers []string, now time.Time) []ProviderQuotaStatus {
	statuses := make([]ProviderQuotaStatus, 0, len(providers))
	for _, provider := range providers {
		statuses = append(statuses, ProviderQuotaStatus{
			AdapterID:         provider,
			Confidence:        ConfidenceUnavailable,
			FreshnessState:    FreshnessNotApplicable,
			TerminalErrorCode: "ErrQuotaRefreshProviderInactive",
			InFlight:          false,
			NextRefreshAt:     formatTime(now),
			GapReasons:        []string{"provider-not-configured-active"},
		})
	}
	return statuses
}

func providersForTrigger(report Report, providers []string, trigger RefreshTrigger, policy RefreshPolicy, now time.Time) []string {
	trigger = normalizeRefreshTrigger(trigger)
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		snapshot, ok := latestQuotaSnapshot(report, provider)
		if shouldRefreshProvider(snapshot, ok, provider, trigger, policy, now) {
			out = append(out, provider)
		}
	}
	sort.Strings(out)
	return out
}

func shouldRefreshProvider(snapshot QuotaSnapshot, ok bool, provider string, trigger RefreshTrigger, policy RefreshPolicy, now time.Time) bool {
	if trigger == RefreshTriggerExplicit {
		return true
	}
	if !ok {
		return trigger == RefreshTriggerStartup || trigger == RefreshTriggerStaleTTL || trigger == RefreshTriggerProviderError || trigger == RefreshTriggerPeriodic
	}
	switch trigger {
	case RefreshTriggerStartup:
		return false
	case RefreshTriggerStaleTTL:
		if snapshot.FreshnessState == FreshnessStale || snapshot.FreshnessState == FreshnessExpired {
			return true
		}
		captured, err := time.Parse(time.RFC3339Nano, snapshot.CapturedAt)
		if err != nil {
			return true
		}
		return now.Sub(captured.UTC()) >= policy.StaleTTL
	case RefreshTriggerApproachingReset:
		resetAt, err := time.Parse(time.RFC3339Nano, snapshot.ResetAt)
		if err != nil {
			return false
		}
		until := resetAt.UTC().Sub(now)
		return until >= 0 && until <= policy.ApproachingResetWithin
	case RefreshTriggerProviderError:
		return strings.TrimSpace(snapshot.TerminalErrorCode) != ""
	case RefreshTriggerPeriodic:
		next := nextRefreshForProvider(snapshot, provider, policy, now)
		if next == "" {
			return true
		}
		nextAt, err := time.Parse(time.RFC3339Nano, next)
		return err != nil || !now.Before(nextAt.UTC())
	default:
		return false
	}
}

func normalizeRefreshTrigger(trigger RefreshTrigger) RefreshTrigger {
	switch trigger {
	case RefreshTriggerExplicit, RefreshTriggerStartup, RefreshTriggerStaleTTL, RefreshTriggerApproachingReset, RefreshTriggerProviderError, RefreshTriggerPeriodic:
		return trigger
	default:
		return RefreshTrigger("")
	}
}

func normalizeRefreshPolicy(policy RefreshPolicy) RefreshPolicy {
	if policy.MinPeriodicCadence <= 0 {
		policy.MinPeriodicCadence = time.Minute
	}
	if policy.MaxPeriodicCadence <= 0 {
		policy.MaxPeriodicCadence = time.Hour
	}
	if policy.PeriodicCadence <= 0 {
		policy.PeriodicCadence = DefaultQuotaRefreshCadence
	}
	if policy.PeriodicCadence < policy.MinPeriodicCadence {
		policy.PeriodicCadence = policy.MinPeriodicCadence
	}
	if policy.PeriodicCadence > policy.MaxPeriodicCadence {
		policy.PeriodicCadence = policy.MaxPeriodicCadence
	}
	if policy.MaxJitter <= 0 {
		policy.MaxJitter = 30 * time.Second
	}
	maxAllowedJitter := policy.PeriodicCadence / 10
	if maxAllowedJitter < time.Second {
		maxAllowedJitter = time.Second
	}
	if policy.MaxJitter > maxAllowedJitter {
		policy.MaxJitter = maxAllowedJitter
	}
	if policy.StaleTTL <= 0 {
		policy.StaleTTL = 15 * time.Minute
	}
	if policy.ApproachingResetWithin <= 0 {
		policy.ApproachingResetWithin = 2 * time.Minute
	}
	if policy.ProviderTimeout <= 0 {
		policy.ProviderTimeout = 20 * time.Second
	}
	if policy.GlobalDeadline <= 0 {
		policy.GlobalDeadline = 45 * time.Second
	}
	if policy.MaxParallelism <= 0 {
		policy.MaxParallelism = 2
	}
	if policy.MaxParallelism > 4 {
		policy.MaxParallelism = 4
	}
	if strings.TrimSpace(policy.JitterSeed) == "" {
		policy.JitterSeed = "loopcoder-quota-refresh-v1"
	}
	return policy
}

func deterministicJitter(provider string, policy RefreshPolicy) time.Duration {
	if policy.MaxJitter <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(policy.JitterSeed + "\x00" + provider))
	value := int64(binary.BigEndian.Uint32(sum[:4]))
	limit := int64(policy.MaxJitter) + 1
	return time.Duration(value % limit)
}

func latestQuotaSnapshot(report Report, provider string) (QuotaSnapshot, bool) {
	var best QuotaSnapshot
	var bestAt time.Time
	found := false
	for _, snapshot := range report.QuotaSnapshots {
		if snapshot.AdapterID != provider {
			continue
		}
		captured, err := time.Parse(time.RFC3339Nano, snapshot.CapturedAt)
		if err != nil {
			captured = time.Time{}
		}
		if !found || captured.After(bestAt) || (captured.Equal(bestAt) && snapshot.QuotaSnapshotID > best.QuotaSnapshotID) {
			best = snapshot
			bestAt = captured
			found = true
		}
	}
	return best, found
}

func nextRefreshForProvider(snapshot QuotaSnapshot, provider string, policy RefreshPolicy, now time.Time) string {
	if strings.TrimSpace(snapshot.CapturedAt) == "" {
		return formatTime(now)
	}
	captured, err := time.Parse(time.RFC3339Nano, snapshot.CapturedAt)
	if err != nil {
		return formatTime(now)
	}
	next := captured.UTC().Add(policy.PeriodicCadence).Add(deterministicJitter(provider, policy))
	if resetAt, err := time.Parse(time.RFC3339Nano, snapshot.ResetAt); err == nil {
		resetRefresh := resetAt.UTC().Add(-policy.ApproachingResetWithin)
		if resetRefresh.Before(next) {
			next = resetRefresh
		}
	}
	if snapshot.FreshnessState == FreshnessStale || snapshot.FreshnessState == FreshnessExpired || strings.TrimSpace(snapshot.TerminalErrorCode) != "" {
		if now.Before(next) {
			next = now
		}
	}
	return formatTime(next)
}

func nextRefreshForReport(report Report, provider string, policy RefreshPolicy, now time.Time) string {
	snapshot, ok := latestQuotaSnapshot(report, provider)
	if !ok {
		return formatTime(now)
	}
	return nextRefreshForProvider(snapshot, provider, policy, now)
}

func staleQuotaErrorReport(cfg config.Config, cached Report, provider, code string, now time.Time, nextRefreshAt string) Report {
	sourceByID := map[string]QuotaTelemetrySource{}
	for _, source := range cached.QuotaTelemetrySources {
		if source.AdapterID == provider {
			sourceByID[source.QuotaSourceID] = source
		}
	}
	var sources []QuotaTelemetrySource
	var snapshots []QuotaSnapshot
	sourceSeen := map[string]bool{}
	for _, snapshot := range latestTrustworthyQuotaSnapshotsByScope(cached, provider) {
		source, ok := sourceByID[snapshot.QuotaSourceID]
		if ok && !sourceSeen[source.QuotaSourceID] {
			sourceSeen[source.QuotaSourceID] = true
			sources = append(sources, source)
		}
		next := snapshot
		next.QuotaSnapshotID = quotaSnapshotID(provider, snapshot.QuotaSnapshotID, code, formatTime(now))
		next.Confidence = ConfidenceStale
		next.FreshnessState = FreshnessStale
		next.TerminalErrorCode = code
		next.StaleAfter = formatTime(now)
		next.CapturedAt = formatTime(now)
		next.CreatedAt = formatTime(now)
		next.UpdatedAt = formatTime(now)
		next.RedactedDiagnostics = boundedDiagnostic(fmt.Sprintf("quota refresh failed for %s with %s; last-known-good marked stale; next_refresh_at=%s", provider, code, nextRefreshAt))
		next.GapReasons = dedupeStrings(append(next.GapReasons, "provider-error", "last-known-good-stale", "next-refresh-scheduled"))
		snapshots = append(snapshots, normalizeQuotaSnapshot(next))
	}
	if len(snapshots) == 0 {
		source, snapshot := quotaTelemetryFallbackForAdapter(AdapterDeclaration{AdapterID: provider}, now, "provider-error", code)
		snapshot.RedactedDiagnostics = boundedDiagnostic(fmt.Sprintf("quota refresh failed for %s with %s; no cached capacity was available; next_refresh_at=%s", provider, code, nextRefreshAt))
		snapshot.GapReasons = dedupeStrings(append(snapshot.GapReasons, "provider-error", "no-last-known-good", "next-refresh-scheduled"))
		sources = append(sources, source)
		snapshots = append(snapshots, snapshot)
	}
	report := Report{
		SchemaVersion:         ProviderInventoryJSONSchema,
		GeneratedAt:           formatTime(now),
		Confidence:            ConfidenceStale,
		Installations:         []ProviderInstallation{},
		ProbeResults:          []ProbeResult{},
		AccountProfiles:       []AccountProfile{},
		AuthReadiness:         []AuthReadiness{},
		ModelCatalogSnapshots: []ModelCatalogSnapshot{},
		ModelCapabilities:     []ModelCapability{},
		QuotaTelemetrySources: sources,
		QuotaSnapshots:        snapshots,
		GapReasons:            []string{"provider-" + provider + "-quota-refresh-error"},
	}
	fingerprint, err := fingerprint(report)
	if err == nil {
		report.InventoryFingerprint = fingerprint
	}
	_ = cfg
	return report
}

func latestTrustworthyQuotaSnapshotsByScope(report Report, provider string) []QuotaSnapshot {
	type selected struct {
		snapshot   QuotaSnapshot
		capturedAt time.Time
	}
	bestByKey := map[string]selected{}
	for _, snapshot := range report.QuotaSnapshots {
		if snapshot.AdapterID != provider || snapshot.FreshnessState == FreshnessExpired || snapshot.Confidence == ConfidenceUnavailable || snapshot.Confidence == ConfidenceUnknown {
			continue
		}
		if strings.TrimSpace(snapshot.TerminalErrorCode) != "" {
			continue
		}
		captured, err := time.Parse(time.RFC3339Nano, snapshot.CapturedAt)
		if err != nil {
			continue
		}
		key := quotaWindowScopeKey(snapshot)
		current, ok := bestByKey[key]
		captured = captured.UTC()
		if !ok || captured.After(current.capturedAt) || (captured.Equal(current.capturedAt) && snapshot.QuotaSnapshotID > current.snapshot.QuotaSnapshotID) {
			bestByKey[key] = selected{snapshot: snapshot, capturedAt: captured}
		}
	}
	keys := make([]string, 0, len(bestByKey))
	for key := range bestByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]QuotaSnapshot, 0, len(keys))
	for _, key := range keys {
		out = append(out, bestByKey[key].snapshot)
	}
	return out
}

func quotaWindowScopeKey(snapshot QuotaSnapshot) string {
	parts := []string{
		snapshot.ScopeKey,
		string(snapshot.QuantityKind),
		snapshot.ProviderQuantityName,
		snapshot.Unit,
		string(snapshot.WindowKind),
		snapshot.WindowStart,
		snapshot.WindowEnd,
		strconvFormatInt(snapshot.RollingDurationMS),
		snapshot.ResetAt,
		string(snapshot.ResetSemantics),
		ptrValue(snapshot.ProviderInstallationID),
		ptrValue(snapshot.AccountProfileID),
		ptrValue(snapshot.ModelCapabilityID),
	}
	return strings.Join(parts, "\x00")
}

func strconvFormatInt(value int64) string {
	return fmt.Sprintf("%d", value)
}

func quotaSnapshotIDs(snapshots []QuotaSnapshot) []string {
	out := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		out = append(out, snapshot.QuotaSnapshotID)
	}
	sort.Strings(out)
	return out
}

func (m *RefreshManager) refreshRequestKey(req RefreshRequest, trigger RefreshTrigger, providers []string, policy RefreshPolicy, now time.Time) string {
	type key struct {
		RepoPath        string              `json:"repo_path"`
		Config          config.Config       `json:"config"`
		RuntimeContract runtimecap.Contract `json:"runtime_contract"`
		NetworkGrants   []NetworkGrant      `json:"network_grants"`
		Requested       []string            `json:"requested"`
		Providers       []string            `json:"providers"`
		Trigger         RefreshTrigger      `json:"trigger"`
		Policy          refreshPolicyKey    `json:"policy"`
		Now             string              `json:"now"`
		Collector       string              `json:"collector"`
	}
	payload, err := json.Marshal(key{
		RepoPath:        strings.TrimSpace(req.RepoPath),
		Config:          req.Config,
		RuntimeContract: req.RuntimeContract,
		NetworkGrants:   canonicalNetworkGrants(req.NetworkGrants),
		Requested:       canonicalProviderList(req.Providers),
		Providers:       append([]string(nil), providers...),
		Trigger:         trigger,
		Policy:          refreshPolicyKeyFromPolicy(policy),
		Now:             formatTime(now),
		Collector:       m.collectorKey(),
	})
	if err != nil {
		return strings.Join(append([]string{string(trigger), formatTime(now)}, providers...), "\x00")
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + fmt.Sprintf("%x", sum[:])
}

func canonicalProviderList(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

type refreshPolicyKey struct {
	PeriodicCadenceMS        int64  `json:"periodic_cadence_ms"`
	MinPeriodicCadenceMS     int64  `json:"min_periodic_cadence_ms"`
	MaxPeriodicCadenceMS     int64  `json:"max_periodic_cadence_ms"`
	MaxJitterMS              int64  `json:"max_jitter_ms"`
	StaleTTLMS               int64  `json:"stale_ttl_ms"`
	ApproachingResetWithinMS int64  `json:"approaching_reset_within_ms"`
	ProviderTimeoutMS        int64  `json:"provider_timeout_ms"`
	GlobalDeadlineMS         int64  `json:"global_deadline_ms"`
	MaxParallelism           int    `json:"max_parallelism"`
	JitterSeed               string `json:"jitter_seed"`
}

func refreshPolicyKeyFromPolicy(policy RefreshPolicy) refreshPolicyKey {
	return refreshPolicyKey{
		PeriodicCadenceMS:        policy.PeriodicCadence.Milliseconds(),
		MinPeriodicCadenceMS:     policy.MinPeriodicCadence.Milliseconds(),
		MaxPeriodicCadenceMS:     policy.MaxPeriodicCadence.Milliseconds(),
		MaxJitterMS:              policy.MaxJitter.Milliseconds(),
		StaleTTLMS:               policy.StaleTTL.Milliseconds(),
		ApproachingResetWithinMS: policy.ApproachingResetWithin.Milliseconds(),
		ProviderTimeoutMS:        policy.ProviderTimeout.Milliseconds(),
		GlobalDeadlineMS:         policy.GlobalDeadline.Milliseconds(),
		MaxParallelism:           policy.MaxParallelism,
		JitterSeed:               strings.TrimSpace(policy.JitterSeed),
	}
}

func canonicalNetworkGrants(grants []NetworkGrant) []NetworkGrant {
	out := append([]NetworkGrant(nil), grants...)
	sort.Slice(out, func(i, j int) bool {
		left := out[i].ProviderID + "\x00" + string(out[i].Purpose) + "\x00" + string(out[i].Scope)
		right := out[j].ProviderID + "\x00" + string(out[j].Purpose) + "\x00" + string(out[j].Scope)
		return left < right
	})
	return out
}

func (m *RefreshManager) collectorKey() string {
	if m == nil || m.Collector == nil {
		return "nil"
	}
	return fmt.Sprintf("%#x", reflect.ValueOf(m.Collector).Pointer())
}

func quotaErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "ErrQuotaRefreshDeadlineExceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "ErrQuotaRefreshCanceled"
	}
	text := strings.TrimSpace(err.Error())
	if text == "" {
		return "ErrQuotaRefreshFailed"
	}
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ':' || r == ';' || r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		if strings.HasPrefix(field, "Err") && safeAdapterKey(field) {
			return field
		}
	}
	return "ErrQuotaRefreshFailed"
}

func boundedDiagnostic(value string) string {
	value = safeSummary(value)
	if len(value) > 512 {
		return value[:512] + "...[truncated]"
	}
	return value
}
