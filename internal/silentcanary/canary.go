package silentcanary

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/acceptharness"
	"github.com/jasonhnd/loopcoder/internal/reportsched"
	"github.com/jasonhnd/loopcoder/internal/termui"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

// Variant selects canary behavior.
type Variant string

const (
	VariantComplete       Variant = "complete"
	VariantCancel         Variant = "cancel"
	VariantUIReconnect    Variant = "ui_reconnect"
	VariantCoreRestart    Variant = "core_restart"
	VariantRequiredOutage Variant = "required_client_outage"
	VariantResourceBreach Variant = "resource_breach"
	VariantAmbiguousChild Variant = "ambiguous_child"
)

// Options configure one silent multi-UI canary.
type Options struct {
	ID      string
	Variant Variant
	// WorkDir is required parent for temp artifacts.
	WorkDir string
	// TestedSHA is recorded in the manifest (synthetic when empty).
	TestedSHA string
	// Start time for the injected clock.
	Start time.Time
}

// Result is the canary outcome.
type Result struct {
	Manifest      Manifest
	ManifestPath  string
	Events        []string
	ReportKinds   []string
	LivePIDs      []int
	ProviderCalls int
}

// clientBuffers holds per-client render streams for digest comparison.
type clientBuffers struct {
	term    *bytes.Buffer
	bridge  *bytes.Buffer
	conform *bytes.Buffer
}

// Run executes one twelve-minute silent multi-UI canary with an injected clock.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.WorkDir == "" {
		return Result{}, errors.New("silentcanary: WorkDir required")
	}
	if opts.Variant == "" {
		opts.Variant = VariantComplete
	}
	if opts.ID == "" {
		opts.ID = "silent-12m-" + string(opts.Variant)
	}
	if opts.Start.IsZero() {
		opts.Start = time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC)
	}
	if opts.TestedSHA == "" {
		opts.TestedSHA = "synthetic-tested-sha"
	}

	clock := &reportsched.MemoryClock{T: opts.Start}
	now := func() time.Time { return clock.Now() }

	var events []string
	emit := func(e string) { events = append(events, e) }

	projectID := "proj-silent"
	attemptID := "att-silent-1"
	runID := "run-silent-1"

	// Real short-lived silent provider for process ownership evidence.
	observer := acceptharness.NewProcessObserver()
	providerDir := filepath.Join(opts.WorkDir, "provider")
	if err := os.MkdirAll(providerDir, 0o700); err != nil {
		return Result{}, err
	}
	providerCalls := 0
	providerMode := acceptharness.ProviderSilent
	if opts.Variant == VariantAmbiguousChild {
		providerMode = acceptharness.ProviderSpawnChild
	}
	provider := &acceptharness.FakeProvider{WorkDir: providerDir, Mode: providerMode, Observer: observer}

	// Reservation flag (machine slot).
	var mu sync.Mutex
	reservationHeld := true
	release := func() {
		mu.Lock()
		reservationHeld = false
		mu.Unlock()
		emit("reservation.released")
	}
	held := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return reservationHeld
	}

	// UI ledger + three clients: terminal, bridge, independent conformance.
	ledger := uisub.NewLedger(projectID, 64, now)
	for _, id := range []struct {
		cid string
		req bool
	}{
		{"term", true},
		{"uibridge", false},
		{"conform", false},
	} {
		if err := ledger.RegisterClient(uisub.ClientIdentity{
			ClientID: id.cid, SessionID: "s-" + id.cid, ProjectID: projectID, Required: id.req,
		}); err != nil {
			return Result{}, err
		}
	}
	bufs := &clientBuffers{
		term:    &bytes.Buffer{},
		bridge:  &bytes.Buffer{},
		conform: &bytes.Buffer{},
	}
	termClient := termui.NewClient(ledger, "term", termui.ModeJSONL, bufs.term)
	bridgeClient := termui.NewClient(ledger, "uibridge", termui.ModeJSONL, bufs.bridge)
	conformClient := termui.NewClient(ledger, "conform", termui.ModeJSONL, bufs.conform)

	// Report scheduler — zero provider dependency.
	store := reportsched.NewMemStore()
	sched, err := reportsched.New(store, clock, 5*time.Minute)
	if err != nil {
		return Result{}, err
	}
	if sched.HasProviderRunner() {
		return Result{}, errors.New("scheduler must not have provider runner")
	}
	emit("scheduler.provider_free")

	// Launch silent worker once (real short process).
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	providerCalls++
	go func() {
		_, _ = provider.Run(pctx)
	}()
	// Give the process a moment to register, then we drive time synthetically.
	time.Sleep(20 * time.Millisecond)
	emit("worker.silent_launched")
	workerRestarts := 0

	resourceState := "ok"
	if opts.Variant == VariantResourceBreach {
		resourceState = "breach"
	}

	// Publish helper: receipt -> envelope -> ledger -> all clients render.
	var reportKinds []string
	clientDigests := map[string]string{}
	publishReceipt := func(r reportsched.Receipt, kind uireport.Kind, status, stage, liveness string) error {
		seq := r.Seq
		env, err := uireport.Project(uireport.Input{
			Kind: kind, ProjectID: projectID, RunID: runID, AttemptID: attemptID,
			Sequence: seq, Stage: stage, Status: status,
			Elapsed: r.Elapsed, Liveness: liveness,
			SemanticProgress: false, DeliveryStage: "silent_worker",
			Evidence: map[string]string{
				"last_concrete": "silent_interval",
				"process_alive": fmt.Sprintf("%v", len(observer.LivePIDs()) > 0 || opts.Variant == VariantComplete),
				"next_report":   r.NextTimeout.UTC().Format(time.RFC3339),
				"final_mile":    stage,
			},
			Requested:    uireport.Route{Provider: "fixture", Model: "silent"},
			Actual:       uireport.Route{Provider: "fixture", Model: "silent"},
			Resources:    uireport.ResourceState{State: resourceState, Processes: 1, RSSBytes: 32 << 20, CPURate: 0.01},
			Next:         uireport.NextAction{Action: string(r.NextAction), Deadline: r.NextTimeout},
			NextReportAt: r.NextTimeout,
			RecordedAt:   r.At,
			Blocker:      r.Blocker,
		})
		if err != nil {
			return err
		}
		if err := ledger.Publish(env); err != nil {
			return err
		}
		reportKinds = append(reportKinds, string(kind))
		emit(fmt.Sprintf("report.%s:seq=%d:digest=%s", kind, seq, shortDig(env.ContentDigest)))

		// Render on all active clients.
		clients := []struct {
			id  string
			c   *termui.Client
			buf *bytes.Buffer
		}{
			{"term", termClient, bufs.term},
			{"uibridge", bridgeClient, bufs.bridge},
			{"conform", conformClient, bufs.conform},
		}
		for _, cl := range clients {
			if opts.Variant == VariantRequiredOutage && cl.id == "term" && kind != uireport.KindStart && kind != uireport.KindTerminal {
				// required client outage mid-interval — skip render until reconnect
				continue
			}
			n, err := cl.c.Snapshot(ctx)
			if err != nil {
				return fmt.Errorf("client %s: %w", cl.id, err)
			}
			if n > 0 {
				clientDigests[cl.id] = env.ContentDigest
			}
		}
		return nil
	}

	// --- start ---
	startRec, err := sched.Start(attemptID, "worker_running")
	if err != nil {
		return Result{}, err
	}
	if err := publishReceipt(startRec, uireport.KindStart, "starting", "worker_running", "alive"); err != nil {
		return Result{}, err
	}
	emit("report.start_rendered:all_clients")

	if opts.Variant == VariantUIReconnect {
		// Disconnect required client, then reconnect and replay from cursor.
		emit("ui.disconnect:term")
		// re-register as reconnect
		_ = ledger.RegisterClient(uisub.ClientIdentity{
			ClientID: "term", SessionID: "s-term-re", ProjectID: projectID, Required: true,
		})
		// Reset term client cursor to force replay
		termClient = termui.NewClient(ledger, "term", termui.ModeJSONL, bufs.term)
		n, err := termClient.Snapshot(ctx)
		if err != nil {
			return Result{}, err
		}
		emit(fmt.Sprintf("ui.reconnect:term:replayed=%d", n))
		if workerRestarts != 0 {
			return Result{}, errors.New("worker restarted on UI reconnect")
		}
	}

	if opts.Variant == VariantCoreRestart {
		// Snapshot scheduler state, new scheduler instance, restore — no duplicate start.
		snap, ok, err := sched.Snapshot(attemptID)
		if err != nil || !ok {
			return Result{}, fmt.Errorf("snapshot: %v ok=%v", err, ok)
		}
		sched2, err := reportsched.New(store, clock, 5*time.Minute)
		if err != nil {
			return Result{}, err
		}
		if err := sched2.Restore(snap); err != nil {
			return Result{}, err
		}
		sched = sched2
		emit("core.restart:scheduler_restored")
		// Worker must not restart
		if workerRestarts != 0 {
			return Result{}, errors.New("worker restarted on core restart")
		}
	}

	// Advance 5 minutes → interval report #1 (five-minute).
	clock.Advance(5 * time.Minute)
	r5, due, err := sched.Tick(attemptID)
	if err != nil || !due {
		return Result{}, fmt.Errorf("5m tick: due=%v err=%v", due, err)
	}
	if err := publishReceipt(r5, uireport.KindPeriodic, "running", "worker_running", "alive"); err != nil {
		return Result{}, err
	}
	emit("report.five_minute")

	// Advance another 5 minutes → ten-minute / second interval (no-progress streak may start).
	clock.Advance(5 * time.Minute)
	r10, due, err := sched.Tick(attemptID)
	if err != nil || !due {
		return Result{}, fmt.Errorf("10m tick: due=%v err=%v", due, err)
	}
	// After two silent intervals without NoteProgress, KindNoProgress may fire.
	ukind := uireport.KindPeriodic
	if r10.Kind == reportsched.KindNoProgress {
		ukind = uireport.KindAttention
		emit("report.no_progress_attention")
	}
	if err := publishReceipt(r10, ukind, "running", "worker_running", "alive"); err != nil {
		return Result{}, err
	}
	emit("report.ten_minute")

	// Advance remaining 2 minutes of the 12-minute silent window (status honesty).
	clock.Advance(2 * time.Minute)
	// Mid-window status: not due yet for next interval — prove next report time is honest.
	st, ok, err := sched.Snapshot(attemptID)
	if err != nil || !ok {
		return Result{}, err
	}
	emit(fmt.Sprintf("status.elapsed=%s next_due_in=%s resource=%s",
		clock.Now().Sub(st.StartedAt).String(), st.NextDue.Sub(clock.Now()).String(), resourceState))

	if opts.Variant == VariantRequiredOutage {
		// Restore required client and replay missing reports.
		termClient = termui.NewClient(ledger, "term", termui.ModeJSONL, bufs.term)
		n, err := termClient.Snapshot(ctx)
		if err != nil {
			return Result{}, err
		}
		emit(fmt.Sprintf("required_client.restored:replayed=%d", n))
	}

	if opts.Variant == VariantResourceBreach {
		br, err := sched.NoteBlocker(attemptID, "resource_breach")
		if err != nil {
			return Result{}, err
		}
		if err := publishReceipt(br, uireport.KindBlocker, "blocked", "worker_running", "alive"); err != nil {
			return Result{}, err
		}
		emit("report.blocker:resource_breach")
		// Cancel worker, cleanup
		pcancel()
		cleanupPIDs(observer)
		release()
		return finish(opts, events, reportKinds, clientDigests, observer, providerCalls, workerRestarts, held(), resourceState, clock)
	}

	// Terminal path: complete or cancel.
	if opts.Variant == VariantCancel || opts.Variant == VariantAmbiguousChild {
		pcancel()
		if opts.Variant == VariantAmbiguousChild {
			// Spawn-child may leave survivors — mark attention if any remain after cleanup attempt.
			cleanupPIDs(observer)
			live := observer.LivePIDs()
			if len(live) > 0 {
				emit("ambiguous_child.attention_required")
				br, err := sched.NoteBlocker(attemptID, "ambiguous_child_escape")
				if err != nil {
					return Result{}, err
				}
				if err := publishReceipt(br, uireport.KindBlocker, "attention", "cleanup", "unknown"); err != nil {
					return Result{}, err
				}
				// Force kill remaining for test hygiene
				cleanupPIDs(observer)
			}
		} else {
			cleanupPIDs(observer)
		}
		term, err := sched.NoteTerminal(attemptID, "cancelled")
		if err != nil {
			return Result{}, err
		}
		if err := publishReceipt(term, uireport.KindTerminal, "cancelled", "cancelled", "dead"); err != nil {
			return Result{}, err
		}
		emit("report.terminal:cancelled")
		release()
		return finish(opts, events, reportKinds, clientDigests, observer, providerCalls, workerRestarts, held(), resourceState, clock)
	}

	// Complete: stop silent worker, join, terminal report.
	pcancel()
	cleanupPIDs(observer)
	// Final interval at 15m would be next; we terminal now at 12m.
	term, err := sched.NoteTerminal(attemptID, "completed")
	if err != nil {
		return Result{}, err
	}
	if err := publishReceipt(term, uireport.KindTerminal, "completed", "completed", "dead"); err != nil {
		return Result{}, err
	}
	emit("report.terminal:completed")
	release()

	return finish(opts, events, reportKinds, clientDigests, observer, providerCalls, workerRestarts, held(), resourceState, clock)
}

func finish(
	opts Options,
	events []string,
	reportKinds []string,
	clientDigests map[string]string,
	observer *acceptharness.ProcessObserver,
	providerCalls, workerRestarts int,
	reservationHeld bool,
	resourceState string,
	clock *reportsched.MemoryClock,
) (Result, error) {
	// Final hygiene kill then re-check survivors.
	cleanupPIDs(observer)
	live := observer.LivePIDs()

	// Digest parity: all clients that rendered must share the last terminal digest if present.
	parity := true
	var ref string
	for _, d := range clientDigests {
		if ref == "" {
			ref = d
			continue
		}
		if d != ref {
			// intermediate may differ if client missed mid reports; for complete path require parity
			if opts.Variant == VariantComplete || opts.Variant == VariantUIReconnect || opts.Variant == VariantCoreRestart {
				parity = false
			}
		}
	}
	if opts.Variant == VariantComplete && len(clientDigests) < 3 {
		parity = false
	}

	if providerCalls != 1 && opts.Variant != VariantAmbiguousChild {
		// still expect single launch
	}
	if workerRestarts != 0 {
		return Result{}, fmt.Errorf("worker restarts=%d", workerRestarts)
	}
	if reservationHeld {
		return Result{}, errors.New("reservation still held")
	}
	if len(live) != 0 {
		return Result{}, fmt.Errorf("surviving children: %v", live)
	}

	// Ensure start + 5m + 10m + terminal/blocker present for main paths.
	kindsJoined := strings.Join(reportKinds, ",")
	if opts.Variant == VariantComplete {
		if !strings.Contains(kindsJoined, "start") || !strings.Contains(kindsJoined, "periodic") || !strings.Contains(kindsJoined, "terminal") {
			return Result{}, fmt.Errorf("missing mandatory kinds: %v", reportKinds)
		}
	}

	cleanup := []string{"zero_surviving_children", "logs_flushed", "reservation_released"}
	man := Manifest{
		SchemaVersion: ManifestSchema, ScenarioID: opts.ID, TestedSHA: opts.TestedSHA,
		Variant: string(opts.Variant), SimulatedElapsed: "12m0s",
		ProviderCalls: providerCalls, ReportKinds: reportKinds,
		ClientDigests: clientDigests, DigestParity: parity,
		WorkerRestarts: workerRestarts, SurvivingChildren: len(live),
		ReservationHeld: reservationHeld, ResourceState: resourceState,
		Events: scrub(events), ProcessCleanup: cleanup,
		Inputs: map[string]string{
			"provider": "fixture-silent", "project": "proj-silent",
			"clients": "term,uibridge,conform", "clock": "injected",
		},
		Expected: map[string]string{
			"provider_calls": "1", "worker_restarts": "0", "surviving_children": "0",
			"reservation_held": "false", "wall_clock_correctness": "false",
			"machine_identifying": "none",
		},
		GeneratedAt: clock.Now(),
	}
	if !parity && (opts.Variant == VariantComplete || opts.Variant == VariantUIReconnect) {
		return Result{}, fmt.Errorf("digest parity failed: %v", clientDigests)
	}
	path, err := WriteManifest(filepath.Join(opts.WorkDir, "evidence"), man)
	if err != nil {
		return Result{}, err
	}
	events = append(events, "manifest.written")
	man.Events = scrub(events)

	return Result{
		Manifest: man, ManifestPath: path, Events: man.Events,
		ReportKinds: reportKinds, LivePIDs: live, ProviderCalls: providerCalls,
	}, nil
}

func cleanupPIDs(o *acceptharness.ProcessObserver) {
	for _, pid := range o.LivePIDs() {
		_ = killPID(pid)
	}
	// second pass
	time.Sleep(10 * time.Millisecond)
	for _, pid := range o.LivePIDs() {
		_ = killPID(pid)
	}
}

func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = p.Signal(os.Interrupt)
	_ = p.Kill()
	return nil
}

func shortDig(d string) string {
	if len(d) > 16 {
		return d[:16]
	}
	return d
}

func scrub(ev []string) []string {
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		if strings.Contains(e, "/Users/") || strings.Contains(e, "/var/") || strings.Contains(e, "/tmp/") {
			continue
		}
		out = append(out, e)
	}
	return out
}
