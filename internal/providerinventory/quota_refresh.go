package providerinventory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
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
	RepoPath        string
	Config          config.Config
	RuntimeContract runtimecap.Contract
	NetworkGrants   []NetworkGrant
	Providers       []string
	Trigger         RefreshTrigger
	Policy          RefreshPolicy
	Now             func() time.Time
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

	mu       sync.Mutex
	inFlight map[string]*refreshCall
	active   map[string]int
}

type refreshCall struct {
	done   chan struct{}
	result RefreshResult
	err    error
}

type providerCollectResult struct {
	provider string
	report   Report
	err      error
}

func NewRefreshManager(store storage.Store, deps Deps) *RefreshManager {
	return &RefreshManager{Store: store, Deps: normalizeDeps(deps), Collector: Discover}
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
	providers := activeRefreshProviders(req.Config, req.Providers)
	cached, err := Load(ctx, m.Store)
	if err != nil {
		return RefreshResult{}, err
	}
	refreshProviders := providersForTrigger(cached, providers, req.Trigger, policy, now)
	key := refreshRequestKey(req.Trigger, refreshProviders, policy)
	if len(refreshProviders) == 0 {
		status := m.statusFromReport(cached, providers, policy, now)
		return RefreshResult{Report: cached, Status: status, Providers: []ProviderRefreshResult{}}, nil
	}

	m.mu.Lock()
	if existing := m.inFlight[key]; existing != nil {
		m.mu.Unlock()
		select {
		case <-existing.done:
			result := existing.result
			for i := range result.Providers {
				result.Providers[i].Coalesced = true
			}
			return result, existing.err
		case <-ctx.Done():
			return RefreshResult{}, ctx.Err()
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	m.inFlight[key] = call
	m.markActiveLocked(refreshProviders, 1)
	m.mu.Unlock()

	go func() {
		call.result, call.err = m.runSharedRefresh(req, cached, refreshProviders, policy, now)
		m.mu.Lock()
		m.markActiveLocked(refreshProviders, -1)
		delete(m.inFlight, key)
		m.mu.Unlock()
		close(call.done)
	}()

	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
		return RefreshResult{}, ctx.Err()
	}
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
	return m.statusFromReport(report, activeRefreshProviders(req.Config, req.Providers), normalizeRefreshPolicy(req.Policy), now), nil
}

func (m *RefreshManager) runSharedRefresh(req RefreshRequest, cached Report, providers []string, policy RefreshPolicy, now time.Time) (RefreshResult, error) {
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
			select {
			case jobs <- provider:
			case <-globalCtx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(results)

	providerResults := make([]ProviderRefreshResult, 0, len(providers))
	for result := range results {
		providerResults = append(providerResults, result)
	}
	sort.Slice(providerResults, func(i, j int) bool {
		return providerResults[i].AdapterID < providerResults[j].AdapterID
	})
	report, err := Load(context.Background(), m.Store)
	if err != nil {
		return RefreshResult{}, err
	}
	status := m.statusFromReport(report, activeRefreshProviders(req.Config, req.Providers), policy, normalizeNow(req.Now)().UTC())
	return RefreshResult{Report: report, Status: status, Providers: providerResults}, nil
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

func activeRefreshProviders(cfg config.Config, requested []string) []string {
	if len(requested) == 0 {
		return configuredProviderNames(cfg)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(requested))
	for _, provider := range requested {
		provider = strings.TrimSpace(provider)
		if provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		out = append(out, provider)
	}
	sort.Strings(out)
	return out
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
		return RefreshTriggerExplicit
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
	includedSources := map[string]bool{}
	for _, snapshot := range cached.QuotaSnapshots {
		if snapshot.AdapterID != provider {
			continue
		}
		source, ok := sourceByID[snapshot.QuotaSourceID]
		if !ok {
			continue
		}
		if !includedSources[source.QuotaSourceID] {
			sources = append(sources, source)
			includedSources[source.QuotaSourceID] = true
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

func quotaSnapshotIDs(snapshots []QuotaSnapshot) []string {
	out := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		out = append(out, snapshot.QuotaSnapshotID)
	}
	sort.Strings(out)
	return out
}

func refreshRequestKey(trigger RefreshTrigger, providers []string, policy RefreshPolicy) string {
	parts := append([]string{string(normalizeRefreshTrigger(trigger)), policy.PeriodicCadence.String(), policy.ProviderTimeout.String(), policy.GlobalDeadline.String()}, providers...)
	return strings.Join(parts, "\x00")
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
