package providerinventory

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestQuotaRefreshCoalescesEquivalentRequestsAndWaiterCancelDoesNotCancelWork(t *testing.T) {
	ctx := context.Background()
	store := quotaRefreshStore(t, fixedInventoryNow)
	defer store.Close()

	now := fixedInventoryNow()
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var once sync.Once
	calls := 0
	manager := NewRefreshManager(store, fakeDeps(t, nil))
	manager.Collector = func(ctx context.Context, opts Options, deps Deps) (Report, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		once.Do(func() { close(started) })
		<-release
		if ctx.Err() != nil {
			t.Fatalf("shared collector context was canceled by waiter cancellation: %v", ctx.Err())
		}
		return quotaRefreshReport("codex", now, 42, ""), nil
	}
	req := RefreshRequest{
		Config:  config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Trigger: RefreshTriggerExplicit,
		Now:     func() time.Time { return now },
	}

	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.Refresh(firstCtx, req)
		firstDone <- err
	}()
	<-started

	secondDone := make(chan RefreshResult, 1)
	secondErr := make(chan error, 1)
	go func() {
		result, err := manager.Refresh(ctx, req)
		if err != nil {
			secondErr <- err
			return
		}
		secondDone <- result
	}()
	policy := normalizeRefreshPolicy(req.Policy)
	key := manager.refreshRequestKey(req, normalizeRefreshTrigger(req.Trigger), []string{"codex"}, policy, now)
	waiterDeadline := time.After(2 * time.Second)
	for {
		manager.mu.Lock()
		call := manager.inFlight[key]
		joined := call != nil && call.waiters > 0
		manager.mu.Unlock()
		if joined {
			break
		}
		select {
		case <-waiterDeadline:
			t.Fatal("timed out waiting for second waiter to join shared refresh")
		default:
		}
	}
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter error = %v, want context.Canceled", err)
	}
	close(release)
	select {
	case err := <-secondErr:
		t.Fatalf("second waiter error = %v", err)
	case result := <-secondDone:
		if len(result.Providers) != 1 || !result.Providers[0].Refreshed || !result.Providers[0].Coalesced {
			t.Fatalf("second result = %#v, want coalesced refresh", result.Providers)
		}
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("collector calls = %d, want 1", gotCalls)
	}
}

func TestQuotaRefreshPublicationBoundarySharesCompletedResult(t *testing.T) {
	ctx := context.Background()
	store := quotaRefreshStore(t, fixedInventoryNow)
	defer store.Close()

	now := fixedInventoryNow()
	published := make(chan struct{})
	releaseCleanup := make(chan struct{})
	var publishOnce sync.Once
	var mu sync.Mutex
	calls := 0
	manager := NewRefreshManager(store, fakeDeps(t, nil))
	manager.afterPublish = func() {
		publishOnce.Do(func() { close(published) })
		<-releaseCleanup
	}
	manager.Collector = func(context.Context, Options, Deps) (Report, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return quotaRefreshReport("codex", now, 42, ""), nil
	}
	req := RefreshRequest{
		Config:  config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Trigger: RefreshTriggerExplicit,
		Now:     func() time.Time { return now },
	}
	firstDone := make(chan RefreshResult, 1)
	firstErr := make(chan error, 1)
	go func() {
		result, err := manager.Refresh(ctx, req)
		if err != nil {
			firstErr <- err
			return
		}
		firstDone <- result
	}()
	<-published

	second, err := manager.Refresh(ctx, req)
	if err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	close(releaseCleanup)
	select {
	case err := <-firstErr:
		t.Fatalf("first Refresh: %v", err)
	case first := <-firstDone:
		if !reflect.DeepEqual(first.Report.InventoryFingerprint, second.Report.InventoryFingerprint) {
			t.Fatalf("shared result fingerprints differ: first=%s second=%s", first.Report.InventoryFingerprint, second.Report.InventoryFingerprint)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first refresh did not finish")
	}
	if len(second.Providers) != 1 || !second.Providers[0].Coalesced || !second.Providers[0].Refreshed {
		t.Fatalf("second providers = %#v, want completed coalesced result", second.Providers)
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("collector calls = %d, want 1", gotCalls)
	}
}

func TestQuotaRefreshRejectsUnconfiguredRequestedProviderWithoutInvokingIt(t *testing.T) {
	store := quotaRefreshStore(t, fixedInventoryNow)
	defer store.Close()
	now := fixedInventoryNow()
	var called []string
	manager := NewRefreshManager(store, fakeDeps(t, nil))
	manager.Collector = func(_ context.Context, opts Options, _ Deps) (Report, error) {
		provider := opts.ActiveProviders[0]
		if provider == "grok" {
			t.Fatal("collector invoked unconfigured provider grok")
		}
		called = append(called, provider)
		return quotaRefreshReport(provider, now, 9, ""), nil
	}
	result, err := manager.Refresh(context.Background(), RefreshRequest{
		Config:    config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Providers: []string{"codex", "grok"},
		Trigger:   RefreshTriggerExplicit,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !reflect.DeepEqual(called, []string{"codex"}) {
		t.Fatalf("called providers = %#v, want only codex", called)
	}
	byProvider := map[string]ProviderRefreshResult{}
	for _, provider := range result.Providers {
		byProvider[provider.AdapterID] = provider
	}
	if byProvider["codex"].Refreshed != true || byProvider["grok"].ErrorCode != "ErrQuotaRefreshProviderInactive" || byProvider["grok"].Refreshed {
		t.Fatalf("provider results = %#v", result.Providers)
	}
}

func TestQuotaRefreshRequestKeyIncludesSemanticInputs(t *testing.T) {
	now := fixedInventoryNow()
	baseRequest := func() RefreshRequest {
		return RefreshRequest{
			RepoPath: "/repo/a",
			Config:   config.Config{Adapters: config.Adapters{Worker: "codex"}},
			RuntimeContract: runtimecap.Contract{Providers: []runtimecap.ProviderRuntime{{
				Name: "codex",
			}}},
			NetworkGrants: []NetworkGrant{{ProviderID: "codex", Purpose: NetworkPurposeQuotaTelemetry, Scope: NetworkScopeMachineInventory}},
			Trigger:       RefreshTriggerExplicit,
			Policy:        RefreshPolicy{MaxParallelism: 1, JitterSeed: "seed-a", StaleTTL: time.Minute, ApproachingResetWithin: time.Minute, PeriodicCadence: 5 * time.Minute, MaxJitter: time.Second, ProviderTimeout: time.Second, GlobalDeadline: time.Second},
			Now:           func() time.Time { return now },
		}
	}
	base := baseRequest()
	basePolicy := normalizeRefreshPolicy(base.Policy)
	baseManager := NewRefreshManager(nil, fakeDeps(t, nil))
	baseKey := baseManager.refreshRequestKey(base, normalizeRefreshTrigger(base.Trigger), []string{"codex"}, basePolicy, now)
	cases := map[string]func(RefreshRequest, *RefreshManager) (RefreshRequest, *RefreshManager){
		"repo": func(req RefreshRequest, manager *RefreshManager) (RefreshRequest, *RefreshManager) {
			req.RepoPath = "/repo/b"
			return req, manager
		},
		"config": func(req RefreshRequest, manager *RefreshManager) (RefreshRequest, *RefreshManager) {
			req.Config.Adapters.Verifier = "claude"
			return req, manager
		},
		"network grants": func(req RefreshRequest, manager *RefreshManager) (RefreshRequest, *RefreshManager) {
			req.NetworkGrants = nil
			return req, manager
		},
		"requested providers": func(req RefreshRequest, manager *RefreshManager) (RefreshRequest, *RefreshManager) {
			req.Providers = []string{"codex", "grok"}
			return req, manager
		},
		"runtime contract": func(req RefreshRequest, manager *RefreshManager) (RefreshRequest, *RefreshManager) {
			req.RuntimeContract.Providers[0].AdapterVersion = "changed"
			return req, manager
		},
		"max parallelism": func(req RefreshRequest, manager *RefreshManager) (RefreshRequest, *RefreshManager) {
			req.Policy.MaxParallelism = 2
			return req, manager
		},
		"jitter": func(req RefreshRequest, manager *RefreshManager) (RefreshRequest, *RefreshManager) {
			req.Policy.JitterSeed = "seed-b"
			return req, manager
		},
		"stale policy": func(req RefreshRequest, manager *RefreshManager) (RefreshRequest, *RefreshManager) {
			req.Policy.StaleTTL = 2 * time.Minute
			return req, manager
		},
		"reset policy": func(req RefreshRequest, manager *RefreshManager) (RefreshRequest, *RefreshManager) {
			req.Policy.ApproachingResetWithin = 2 * time.Minute
			return req, manager
		},
		"trigger": func(req RefreshRequest, manager *RefreshManager) (RefreshRequest, *RefreshManager) {
			req.Trigger = RefreshTriggerPeriodic
			return req, manager
		},
		"collector": func(req RefreshRequest, _ *RefreshManager) (RefreshRequest, *RefreshManager) {
			manager := NewRefreshManager(nil, fakeDeps(t, nil))
			manager.Collector = func(context.Context, Options, Deps) (Report, error) { return Report{}, nil }
			return req, manager
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req, manager := mutate(baseRequest(), baseManager)
			key := manager.refreshRequestKey(req, normalizeRefreshTrigger(req.Trigger), []string{"codex"}, normalizeRefreshPolicy(req.Policy), now)
			if key == baseKey {
				t.Fatalf("request key did not change for %s", name)
			}
		})
	}
	equivalent := baseRequest()
	equivalent.NetworkGrants = []NetworkGrant{{ProviderID: "codex", Purpose: NetworkPurposeQuotaTelemetry, Scope: NetworkScopeMachineInventory}}
	if got := baseManager.refreshRequestKey(equivalent, normalizeRefreshTrigger(equivalent.Trigger), []string{"codex"}, normalizeRefreshPolicy(equivalent.Policy), now); got != baseKey {
		t.Fatalf("equivalent request key = %s, want %s", got, baseKey)
	}
}

func TestQuotaRefreshRunsOnlyConfiguredProvidersWithBoundedParallelism(t *testing.T) {
	store := quotaRefreshStore(t, fixedInventoryNow)
	defer store.Close()

	now := fixedInventoryNow()
	var mu sync.Mutex
	active := 0
	maxActive := 0
	called := map[string]int{}
	release := map[string]chan struct{}{
		"codex":  make(chan struct{}),
		"claude": make(chan struct{}),
	}
	manager := NewRefreshManager(store, fakeDeps(t, nil))
	manager.Collector = func(ctx context.Context, opts Options, deps Deps) (Report, error) {
		if len(opts.ActiveProviders) != 1 {
			t.Fatalf("ActiveProviders = %#v, want one provider", opts.ActiveProviders)
		}
		provider := opts.ActiveProviders[0]
		mu.Lock()
		called[provider]++
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		if ch := release[provider]; ch != nil {
			<-ch
		}
		mu.Lock()
		active--
		mu.Unlock()
		return quotaRefreshReport(provider, now, 10, ""), nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := manager.Refresh(context.Background(), RefreshRequest{
			Config: config.Config{Adapters: config.Adapters{
				Worker:   "codex",
				Verifier: "claude",
			}},
			Providers: []string{"codex", "claude"},
			Trigger:   RefreshTriggerExplicit,
			Policy:    RefreshPolicy{MaxParallelism: 1},
			Now:       func() time.Time { return now },
		})
		done <- err
	}()
	var first string
	for {
		mu.Lock()
		startedCount := len(called)
		for provider := range called {
			first = provider
		}
		mu.Unlock()
		if startedCount == 1 {
			break
		}
	}
	close(release[first])
	var second string
	for {
		mu.Lock()
		startedCount := len(called)
		for provider := range called {
			if provider != first {
				second = provider
			}
		}
		mu.Unlock()
		if startedCount == 2 {
			break
		}
	}
	close(release[second])
	if err := <-done; err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("max active collectors = %d, want 1", maxActive)
	}
	if !reflect.DeepEqual(called, map[string]int{"codex": 1, "claude": 1}) {
		t.Fatalf("called providers = %#v, want only configured active providers", called)
	}
}

func TestQuotaRefreshProviderErrorPersistsLastKnownGoodAsStaleWithoutFabricatingCapacity(t *testing.T) {
	now := fixedInventoryNow()
	current := now
	store := quotaRefreshStore(t, func() time.Time { return current })
	defer store.Close()
	if err := Refresh(context.Background(), store, quotaRefreshReport("codex", now.Add(-time.Minute), 7, ""), now); err != nil {
		t.Fatalf("seed Refresh: %v", err)
	}

	manager := NewRefreshManager(store, fakeDeps(t, nil))
	manager.Collector = func(context.Context, Options, Deps) (Report, error) {
		return Report{}, errors.New("ErrProviderQuotaUnavailable: provider refused")
	}
	current = now.Add(time.Minute)
	result, err := manager.Refresh(context.Background(), RefreshRequest{
		Config:  config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Trigger: RefreshTriggerExplicit,
		Now:     func() time.Time { return current },
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(result.Providers) != 1 || result.Providers[0].ErrorCode != "ErrProviderQuotaUnavailable" {
		t.Fatalf("provider result = %#v, want typed provider error", result.Providers)
	}
	loaded, err := Load(context.Background(), store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	latest, ok := latestQuotaSnapshot(loaded, "codex")
	if !ok {
		t.Fatal("missing stale quota snapshot")
	}
	if latest.FreshnessState != FreshnessStale || latest.Confidence != ConfidenceStale || latest.TerminalErrorCode != "ErrProviderQuotaUnavailable" {
		t.Fatalf("latest = %#v, want stale typed error", latest)
	}
	if latest.RemainingValue == nil || *latest.RemainingValue != 7 {
		t.Fatalf("remaining value = %#v, want preserved last-known-good value 7", latest.RemainingValue)
	}
	if !containsString(latest.GapReasons, "last-known-good-stale") || secretLike(latest.RedactedDiagnostics) {
		t.Fatalf("diagnostics/gaps = %#v / %q", latest.GapReasons, latest.RedactedDiagnostics)
	}
}

func TestQuotaRefreshProviderErrorCarriesLatestTrustworthySnapshotPerScopeWindow(t *testing.T) {
	now := fixedInventoryNow()
	current := now
	store := quotaRefreshStore(t, func() time.Time { return current })
	defer store.Close()

	source := fixtureQuotaSource("codex", QuotaSourceFixture, false)
	windowAOlder := quotaRefreshSnapshot("codex", now.Add(-6*time.Minute), 99, "")
	windowAOlder.QuotaSourceID = source.QuotaSourceID
	windowAOlder.QuotaSnapshotID = "qsnap_codex_window_a_older"
	windowAOlder.ScopeKey = "provider:codex/window:a"
	windowAOlder.ResetAt = formatTime(now.Add(time.Hour))
	windowANewer := windowAOlder
	remainingA := int64(7)
	windowANewer.QuotaSnapshotID = "qsnap_codex_window_a_newer"
	windowANewer.CapturedAt = formatTime(now.Add(-2 * time.Minute))
	windowANewer.CreatedAt = windowANewer.CapturedAt
	windowANewer.UpdatedAt = windowANewer.CapturedAt
	windowANewer.RemainingValue = &remainingA
	windowBOlder := quotaRefreshSnapshot("codex", now.Add(-5*time.Minute), 88, "")
	windowBOlder.QuotaSourceID = source.QuotaSourceID
	windowBOlder.QuotaSnapshotID = "qsnap_codex_window_b_older"
	windowBOlder.ScopeKey = "provider:codex/window:b"
	windowBOlder.ResetAt = formatTime(now.Add(2 * time.Hour))
	windowBNewer := windowBOlder
	remainingB := int64(3)
	windowBNewer.QuotaSnapshotID = "qsnap_codex_window_b_newer"
	windowBNewer.CapturedAt = formatTime(now.Add(-time.Minute))
	windowBNewer.CreatedAt = windowBNewer.CapturedAt
	windowBNewer.UpdatedAt = windowBNewer.CapturedAt
	windowBNewer.RemainingValue = &remainingB

	seed := quotaRefreshReport("codex", now.Add(-6*time.Minute), 0, "")
	seed.QuotaTelemetrySources = []QuotaTelemetrySource{source}
	seed.QuotaSnapshots = []QuotaSnapshot{
		normalizeQuotaSnapshot(windowAOlder),
		normalizeQuotaSnapshot(windowANewer),
		normalizeQuotaSnapshot(windowBOlder),
		normalizeQuotaSnapshot(windowBNewer),
	}
	if err := Refresh(context.Background(), store, seed, now); err != nil {
		t.Fatalf("seed windows: %v", err)
	}
	manager := NewRefreshManager(store, fakeDeps(t, nil))
	manager.Collector = func(context.Context, Options, Deps) (Report, error) {
		return Report{}, errors.New("ErrProviderQuotaUnavailable")
	}
	current = now.Add(time.Minute)
	result, err := manager.Refresh(context.Background(), RefreshRequest{
		Config:  config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Trigger: RefreshTriggerExplicit,
		Now:     func() time.Time { return current },
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := result.Providers[0].QuotaSnapshotIDs; len(got) != 2 {
		t.Fatalf("stale result snapshot ids = %#v, want two scope/window carry-forwards", got)
	}
	loaded, err := Load(context.Background(), store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var stale []QuotaSnapshot
	for _, snapshot := range loaded.QuotaSnapshots {
		if snapshot.AdapterID == "codex" && snapshot.TerminalErrorCode == "ErrProviderQuotaUnavailable" {
			stale = append(stale, snapshot)
		}
	}
	if len(stale) != 2 {
		t.Fatalf("stale snapshots = %#v, want exactly two", stale)
	}
	byScope := map[string]QuotaSnapshot{}
	for _, snapshot := range stale {
		byScope[snapshot.ScopeKey] = snapshot
	}
	if byScope["provider:codex/window:a"].RemainingValue == nil || *byScope["provider:codex/window:a"].RemainingValue != 7 {
		t.Fatalf("window a stale snapshot = %#v, want latest trustworthy value 7", byScope["provider:codex/window:a"])
	}
	if byScope["provider:codex/window:b"].RemainingValue == nil || *byScope["provider:codex/window:b"].RemainingValue != 3 {
		t.Fatalf("window b stale snapshot = %#v, want latest trustworthy value 3", byScope["provider:codex/window:b"])
	}
}

func TestQuotaRefreshTriggersUseInjectedClockAndDeterministicJitter(t *testing.T) {
	now := fixedInventoryNow()
	snapshot := quotaRefreshSnapshot("codex", now.Add(-6*time.Minute), 1, "")
	policy := normalizeRefreshPolicy(RefreshPolicy{PeriodicCadence: DefaultQuotaRefreshCadence, MaxJitter: time.Nanosecond, JitterSeed: "seed"})
	if !shouldRefreshProvider(snapshot, true, "codex", RefreshTriggerPeriodic, policy, now) {
		t.Fatal("periodic trigger should refresh after default five minute cadence plus deterministic jitter")
	}
	if shouldRefreshProvider(snapshot, true, "codex", RefreshTriggerStartup, policy, now) {
		t.Fatal("startup trigger should not refresh when durable cache exists")
	}
	resetSoon := snapshot
	resetSoon.ResetAt = formatTime(now.Add(time.Minute))
	if !shouldRefreshProvider(resetSoon, true, "codex", RefreshTriggerApproachingReset, policy, now) {
		t.Fatal("approaching reset trigger should refresh inside reset window")
	}
	future := quotaRefreshSnapshot("codex", now.Add(time.Minute), 1, "")
	status := (&RefreshManager{}).statusFromReport(Report{QuotaSnapshots: []QuotaSnapshot{future}}, []string{"codex"}, policy, now)
	if len(status.Providers) != 1 || status.Providers[0].AgeMS == nil || *status.Providers[0].AgeMS != 0 || !containsString(status.Providers[0].GapReasons, "clock-skew") {
		t.Fatalf("clock skew status = %#v", status.Providers)
	}
}

func TestQuotaRefreshStatusReplaysDurableCacheAfterRestartAndOrdersDeterministically(t *testing.T) {
	now := fixedInventoryNow()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(context.Background(), storage.Options{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	if err := Refresh(context.Background(), store, quotaRefreshReport("claude", now.Add(-time.Minute), 2, ""), now); err != nil {
		t.Fatalf("seed claude: %v", err)
	}
	if err := Refresh(context.Background(), store, quotaRefreshReport("codex", now.Add(-2*time.Minute), 3, ""), now); err != nil {
		t.Fatalf("seed codex: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	reopened, err := storage.Open(context.Background(), storage.Options{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("storage.Open reopened: %v", err)
	}
	defer reopened.Close()
	manager := NewRefreshManager(reopened, fakeDeps(t, nil))
	manager.Collector = func(context.Context, Options, Deps) (Report, error) {
		t.Fatal("status must consume durable cache only")
		return Report{}, nil
	}
	status, err := manager.Status(context.Background(), RefreshRequest{
		Config: config.Config{Adapters: config.Adapters{Worker: "claude", Verifier: "codex"}},
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var got []string
	for _, provider := range status.Providers {
		got = append(got, provider.AdapterID)
		if provider.InFlight {
			t.Fatalf("provider unexpectedly in flight after restart: %#v", provider)
		}
	}
	if !reflect.DeepEqual(got, []string{"claude", "codex"}) {
		t.Fatalf("provider order = %#v, want configured deterministic order", got)
	}
}

func TestQuotaRefreshProviderTimeoutDoesNotBlockOtherProviderResult(t *testing.T) {
	store := quotaRefreshStore(t, fixedInventoryNow)
	defer store.Close()
	now := fixedInventoryNow()
	blocked := make(chan struct{})
	release := make(chan struct{})
	manager := NewRefreshManager(store, fakeDeps(t, nil))
	manager.Collector = func(ctx context.Context, opts Options, deps Deps) (Report, error) {
		provider := opts.ActiveProviders[0]
		if provider == "codex" {
			close(blocked)
			<-release
			return Report{}, ctx.Err()
		}
		return quotaRefreshReport(provider, now, 5, ""), nil
	}
	result, err := manager.Refresh(context.Background(), RefreshRequest{
		Config:    config.Config{Adapters: config.Adapters{Worker: "codex", Verifier: "claude"}},
		Providers: []string{"codex", "claude"},
		Trigger:   RefreshTriggerExplicit,
		Policy:    RefreshPolicy{MaxParallelism: 2, ProviderTimeout: 50 * time.Millisecond, GlobalDeadline: time.Second},
		Now:       func() time.Time { return now },
	})
	close(release)
	<-blocked
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	byProvider := map[string]ProviderRefreshResult{}
	for _, provider := range result.Providers {
		byProvider[provider.AdapterID] = provider
	}
	if !byProvider["claude"].Refreshed {
		t.Fatalf("claude result = %#v, want refreshed despite codex timeout", byProvider["claude"])
	}
	if byProvider["codex"].ErrorCode != "ErrQuotaRefreshDeadlineExceeded" {
		t.Fatalf("codex result = %#v, want deadline error", byProvider["codex"])
	}
}

func TestQuotaRefreshGlobalDeadlineReturnsEntryForEverySelectedProvider(t *testing.T) {
	store := quotaRefreshStore(t, fixedInventoryNow)
	defer store.Close()
	now := fixedInventoryNow()
	started := make(chan struct{})
	release := make(chan struct{})
	manager := NewRefreshManager(store, fakeDeps(t, nil))
	manager.Collector = func(ctx context.Context, opts Options, deps Deps) (Report, error) {
		close(started)
		<-release
		return Report{}, ctx.Err()
	}
	result, err := manager.Refresh(context.Background(), RefreshRequest{
		Config:    config.Config{Adapters: config.Adapters{Worker: "codex", Verifier: "claude"}},
		Providers: []string{"codex", "claude"},
		Trigger:   RefreshTriggerExplicit,
		Policy:    RefreshPolicy{MaxParallelism: 1, ProviderTimeout: time.Hour, GlobalDeadline: time.Nanosecond},
		Now:       func() time.Time { return now },
	})
	close(release)
	select {
	case <-started:
	default:
	}
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(result.Providers) != 2 {
		t.Fatalf("providers = %#v, want two entries", result.Providers)
	}
	if got := []string{result.Providers[0].AdapterID, result.Providers[1].AdapterID}; !reflect.DeepEqual(got, []string{"claude", "codex"}) {
		t.Fatalf("provider order = %#v", got)
	}
	for _, provider := range result.Providers {
		if provider.Refreshed || provider.ErrorCode != "ErrQuotaRefreshDeadlineExceeded" {
			t.Fatalf("provider result = %#v, want typed deadline gap", provider)
		}
	}
}

func TestQuotaRefreshInvalidTriggerFailsClosed(t *testing.T) {
	store := quotaRefreshStore(t, fixedInventoryNow)
	defer store.Close()
	now := fixedInventoryNow()
	manager := NewRefreshManager(store, fakeDeps(t, nil))
	manager.Collector = func(context.Context, Options, Deps) (Report, error) {
		t.Fatal("invalid trigger must not probe providers")
		return Report{}, nil
	}
	result, err := manager.Refresh(context.Background(), RefreshRequest{
		Config:    config.Config{Adapters: config.Adapters{Worker: "codex", Verifier: "claude"}},
		Providers: []string{"claude", "codex"},
		Trigger:   RefreshTrigger("surprise"),
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(result.Providers) != 2 {
		t.Fatalf("providers = %#v, want deterministic provider evidence for invalid trigger", result.Providers)
	}
	if got := []string{result.Providers[0].AdapterID, result.Providers[1].AdapterID}; !reflect.DeepEqual(got, []string{"claude", "codex"}) {
		t.Fatalf("provider order = %#v", got)
	}
	for _, provider := range result.Providers {
		if provider.Refreshed || provider.ErrorCode != "ErrQuotaRefreshInvalidTrigger" || provider.Trigger != RefreshTrigger("surprise") {
			t.Fatalf("provider result = %#v, want invalid-trigger typed gap", provider)
		}
	}
}

func quotaRefreshStore(t *testing.T, now func() time.Time) storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: now})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	return store
}

func quotaRefreshReport(provider string, capturedAt time.Time, remaining int64, terminal string) Report {
	source := fixtureQuotaSource(provider, QuotaSourceFixture, false)
	snapshot := quotaRefreshSnapshot(provider, capturedAt, remaining, terminal)
	snapshot.QuotaSourceID = source.QuotaSourceID
	report := Report{
		SchemaVersion:         ProviderInventoryJSONSchema,
		GeneratedAt:           formatTime(capturedAt),
		Confidence:            snapshot.Confidence,
		Installations:         []ProviderInstallation{},
		ProbeResults:          []ProbeResult{},
		AccountProfiles:       []AccountProfile{},
		AuthReadiness:         []AuthReadiness{},
		ModelCatalogSnapshots: []ModelCatalogSnapshot{},
		ModelCapabilities:     []ModelCapability{},
		QuotaTelemetrySources: []QuotaTelemetrySource{source},
		QuotaSnapshots:        []QuotaSnapshot{snapshot},
		GapReasons:            []string{},
	}
	fingerprint, _ := fingerprint(report)
	report.InventoryFingerprint = fingerprint
	return report
}

func quotaRefreshSnapshot(provider string, capturedAt time.Time, remaining int64, terminal string) QuotaSnapshot {
	source := fixtureQuotaSource(provider, QuotaSourceFixture, false)
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:   quotaSnapshotID(provider, source.QuotaSourceID, formatTime(capturedAt), terminal),
		QuotaSourceID:     source.QuotaSourceID,
		SourceKind:        source.SourceKind,
		AdapterID:         provider,
		ScopeKey:          "provider:" + provider,
		QuantityKind:      QuantityRequests,
		Unit:              "request",
		WindowKind:        WindowRolling,
		RollingDurationMS: int64(time.Hour.Milliseconds()),
		ResetAt:           formatTime(capturedAt.Add(30 * time.Minute)),
		ResetSemantics:    ResetRolling,
		RemainingValue:    &remaining,
		ValueScale:        0,
		Confidence:        ConfidenceExact,
		FieldConfidences:  map[string]Confidence{"remaining_value": ConfidenceExact},
		FreshnessState:    FreshnessFresh,
		CapturedAt:        formatTime(capturedAt),
		StaleAfter:        formatTime(capturedAt.Add(15 * time.Minute)),
		ConflictSet:       []string{},
		GapReasons:        []string{},
		TerminalErrorCode: terminal,
		CreatedAt:         formatTime(capturedAt),
		UpdatedAt:         formatTime(capturedAt),
		PolicyVersion:     PolicyVersion,
	})
}
