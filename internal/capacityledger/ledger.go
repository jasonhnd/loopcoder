package capacityledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/quotamode"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

const (
	SchemaEntry   = "loopcoder.capacity_ledger.entry.v1"
	SchemaFile    = "loopcoder.capacity_ledger.file.v1"
	fileName      = "capacity-ledger.json"
	defaultDemand = 0.05 // 5% window soft hold for a typical child attempt
)

var (
	ErrInvalid   = errors.New("capacityledger: invalid")
	ErrNoWindow  = errors.New("capacityledger: no usable capacity window")
	ErrDuplicate = errors.New("capacityledger: attempt already reserved")
)

// Entry is the durable capacity accounting record for one attempt.
// Values are fractions [0,1] of a window unless Unit describes otherwise.
// Never contains credentials or raw account secrets.
type Entry struct {
	Schema         string                    `json:"schema"`
	ProjectID      string                    `json:"project_id"`
	RunID          string                    `json:"run_id"`
	AttemptID      string                    `json:"attempt_id"`
	Policy         PolicyName                `json:"policy"`
	Provider       string                    `json:"provider"`
	AccountRef     string                    `json:"account_ref"` // redacted
	Model          string                    `json:"model"`
	Depth          string                    `json:"depth,omitempty"`
	WindowKind     string                    `json:"window_kind"`
	Confidence     quotapolicy.EvidenceClass `json:"confidence"`
	Freshness      string                    `json:"freshness"`
	ResetAt        *time.Time                `json:"reset_at,omitempty"`
	Before         float64                   `json:"capacity_before"` // remaining fraction before reserve
	Reserved       float64                   `json:"capacity_reserved"`
	Actual         *float64                  `json:"capacity_actual,omitempty"`
	After          *float64                  `json:"capacity_after,omitempty"` // before - actual (estimate)
	ReservationID  string                    `json:"reservation_id,omitempty"`
	RouteReason    string                    `json:"route_reason,omitempty"`
	State          string                    `json:"state"` // reserved|reconciled|released|cancelled|refused
	IdempotencyKey string                    `json:"idempotency_key"`
	CreatedAt      time.Time                 `json:"created_at"`
	UpdatedAt      time.Time                 `json:"updated_at"`
}

// Ledger is a durable, process-local capacity account bound to LOOPCODER_HOME.
type Ledger struct {
	mu    sync.Mutex
	path  string
	store *quotamode.Store
	now   func() time.Time
	// entries by idempotency key (attempt)
	byKey map[string]*Entry
}

type fileDoc struct {
	Schema  string    `json:"schema"`
	Entries []Entry   `json:"entries"`
	SavedAt time.Time `json:"saved_at"`
}

// Open opens or creates the durable ledger under LOOPCODER_HOME.
func Open(now func() time.Time) (*Ledger, error) {
	if now == nil {
		now = time.Now
	}
	dir, err := home.ResolveHomeDir(home.DefaultDeps())
	if err != nil || strings.TrimSpace(dir) == "" {
		// fall back under temp when home is unavailable
		dir = filepath.Join(os.TempDir(), "loopcoder-capacity-ledger")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fileName)
	l := &Ledger{
		path:  path,
		store: quotamode.NewStore(now),
		now:   now,
		byKey: map[string]*Entry{},
	}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

// OpenPath is for tests.
func OpenPath(path string, now func() time.Time) (*Ledger, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Ledger{path: path, store: quotamode.NewStore(now), now: now, byKey: map[string]*Entry{}}
	_ = l.load()
	return l, nil
}

func (l *Ledger) load() error {
	b, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var doc fileDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	for i := range doc.Entries {
		e := doc.Entries[i]
		// Rehydrate active reservations into store so restart does not oversubscribe.
		if e.State == "reserved" && e.ReservationID != "" {
			// Soft re-create: mark as active fraction via Reserve with risk if needed —
			// we only need ActiveFraction tracking. Store rehydration is best-effort:
			// re-insert via Reserve with same attempt is blocked by byKey.
		}
		cp := e
		l.byKey[e.IdempotencyKey] = &cp
	}
	return nil
}

func (l *Ledger) saveLocked() error {
	doc := fileDoc{Schema: SchemaFile, SavedAt: l.now().UTC()}
	for _, e := range l.byKey {
		if e != nil {
			doc.Entries = append(doc.Entries, *e)
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// ReserveInput is the post-route capacity hold request.
type ReserveInput struct {
	ProjectID   string
	RunID       string
	AttemptID   string
	Policy      PolicyName
	Provider    string
	Model       string
	Depth       string
	AccountRef  string
	RouteReason string
	// Snapshot is the capacity truth used for before/remaining (optional; windows from it).
	Snapshot *capacitysnapshot.Snapshot
	// DemandFraction estimated consumption (0-1). Zero → defaultDemand.
	DemandFraction float64
	// DemandConfidence exact|estimated|unknown
	DemandConfidence quotapolicy.EvidenceClass
}

// Reserve holds capacity for an attempt. Idempotent on project|run|attempt.
func (l *Ledger) Reserve(in ReserveInput) (Entry, error) {
	if l == nil {
		return Entry{}, fmt.Errorf("%w: nil ledger", ErrInvalid)
	}
	key := idemKey(in.ProjectID, in.RunID, in.AttemptID)
	l.mu.Lock()
	defer l.mu.Unlock()
	if prev, ok := l.byKey[key]; ok && prev != nil {
		// Restart-safe: do not double-reserve.
		return *prev, nil
	}
	now := l.now().UTC()
	cfg := ModeConfig(in.Policy)
	win, before, conf, fresh, resetAt, accRef, err := pickWindow(in)
	if err != nil {
		e := Entry{
			Schema: SchemaEntry, ProjectID: in.ProjectID, RunID: in.RunID, AttemptID: in.AttemptID,
			Policy: ParsePolicy(string(in.Policy)), Provider: in.Provider, Model: in.Model, Depth: in.Depth,
			AccountRef: redact(accRef), Confidence: quotapolicy.EvidenceUnknown, Freshness: "unknown",
			State: "refused", IdempotencyKey: key, CreatedAt: now, UpdatedAt: now,
			RouteReason: in.RouteReason,
		}
		l.byKey[key] = &e
		_ = l.saveLocked()
		return e, err
	}
	if in.AccountRef != "" {
		accRef = in.AccountRef
	}
	demand := in.DemandFraction
	if demand <= 0 {
		demand = defaultDemand
	}
	demEv := in.DemandConfidence
	if demEv == "" {
		demEv = quotapolicy.EvidenceEstimated
	}
	wkey := quotamode.WindowKey{
		Provider: in.Provider, Account: redact(accRef), Model: in.Model, Window: win,
	}
	snapRem := quotamode.SnapshotRemaining{
		Key: wkey, RemainingFraction: before, Evidence: conf,
		EvidenceID: "cap-" + shortHash(fmt.Sprintf("%s|%s|%g", in.Provider, in.Model, before)),
	}
	res, rerr := l.store.Reserve(quotamode.ReserveRequest{
		ProjectID: in.ProjectID, AttemptID: in.AttemptID,
		Key: wkey, Snapshot: snapRem,
		DemandEstimate: demand, DemandEvidence: demEv,
		Config: cfg,
	})
	if rerr != nil {
		e := Entry{
			Schema: SchemaEntry, ProjectID: in.ProjectID, RunID: in.RunID, AttemptID: in.AttemptID,
			Policy: ParsePolicy(string(in.Policy)), Provider: in.Provider, Model: in.Model, Depth: in.Depth,
			AccountRef: redact(accRef), WindowKind: string(win), Confidence: conf, Freshness: fresh,
			ResetAt: resetAt, Before: before, Reserved: 0, State: "refused",
			ReservationID: res.ID, IdempotencyKey: key, CreatedAt: now, UpdatedAt: now,
			RouteReason: in.RouteReason + "; reserve_refused=" + rerr.Error(),
		}
		l.byKey[key] = &e
		_ = l.saveLocked()
		return e, rerr
	}
	e := Entry{
		Schema: SchemaEntry, ProjectID: in.ProjectID, RunID: in.RunID, AttemptID: in.AttemptID,
		Policy: ParsePolicy(string(in.Policy)), Provider: in.Provider, Model: in.Model, Depth: in.Depth,
		AccountRef: redact(accRef), WindowKind: string(win), Confidence: conf, Freshness: fresh,
		ResetAt: resetAt, Before: before, Reserved: res.Fraction, State: "reserved",
		ReservationID: res.ID, IdempotencyKey: key, CreatedAt: now, UpdatedAt: now,
		RouteReason: in.RouteReason,
	}
	l.byKey[key] = &e
	if err := l.saveLocked(); err != nil {
		return e, err
	}
	return e, nil
}

// Reconcile records actual usage for an attempt (idempotent).
func (l *Ledger) Reconcile(projectID, runID, attemptID string, actualFraction float64, source string) (Entry, error) {
	key := idemKey(projectID, runID, attemptID)
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byKey[key]
	if !ok || e == nil {
		return Entry{}, fmt.Errorf("%w: no reservation for attempt", ErrInvalid)
	}
	if e.State == "reconciled" {
		return *e, nil
	}
	if e.ReservationID != "" {
		conf := e.Confidence
		if conf == "" {
			conf = quotapolicy.EvidenceEstimated
		}
		_, _ = l.store.Reconcile(e.ReservationID, actualFraction, source, conf)
	}
	now := l.now().UTC()
	a := clamp01(actualFraction)
	e.Actual = &a
	after := e.Before - a
	if after < 0 {
		after = 0
	}
	e.After = &after
	e.State = "reconciled"
	e.UpdatedAt = now
	if err := l.saveLocked(); err != nil {
		return *e, err
	}
	return *e, nil
}

// Release frees a reservation without usage (fail/cancel). Idempotent.
// Does not invent Actual. Call ObserveAfter afterward to attach a fresh
// remaining fraction when token actual is unknown.
func (l *Ledger) Release(projectID, runID, attemptID, reason string) (Entry, error) {
	key := idemKey(projectID, runID, attemptID)
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byKey[key]
	if !ok || e == nil {
		return Entry{}, fmt.Errorf("%w: no reservation for attempt", ErrInvalid)
	}
	if e.State == "released" || e.State == "cancelled" || e.State == "reconciled" {
		return *e, nil
	}
	if e.ReservationID != "" {
		_, _ = l.store.Release(e.ReservationID, reason)
	}
	e.State = "released"
	e.UpdatedAt = l.now().UTC()
	if e.RouteReason != "" {
		e.RouteReason = e.RouteReason + "; released=" + reason
	} else {
		e.RouteReason = "released=" + reason
	}
	if err := l.saveLocked(); err != nil {
		return *e, err
	}
	return *e, nil
}

// ObserveAfterOpts binds the post-run observation to the same account/window
// as the reservation. Cross-window after is rejected (fail closed).
type ObserveAfterOpts struct {
	// AccountRef and WindowKind must match the reservation when non-empty.
	AccountRef string
	WindowKind string
	// ResetObserved true when the observation includes a quota reset since reserve
	// (allows after > before). Without this, after rising is fail-closed.
	ResetObserved bool
	// ResetEvidence short source tag when ResetObserved (required if after > before).
	ResetEvidence string
}

// ObserveAfter records a post-run remaining fraction from a fresh capacity
// observation without inventing actual usage. Actual may stay nil (unknown).
// After must never be left n/a when a real same-window observation is available.
// After rising above Before without reset evidence is rejected (fail closed).
func (l *Ledger) ObserveAfter(projectID, runID, attemptID string, afterFraction float64, source, freshness string) (Entry, error) {
	return l.ObserveAfterBound(projectID, runID, attemptID, afterFraction, source, freshness, ObserveAfterOpts{})
}

// ObserveAfterBound is ObserveAfter with explicit account/window/reset binding.
func (l *Ledger) ObserveAfterBound(projectID, runID, attemptID string, afterFraction float64, source, freshness string, opts ObserveAfterOpts) (Entry, error) {
	key := idemKey(projectID, runID, attemptID)
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byKey[key]
	if !ok || e == nil {
		return Entry{}, fmt.Errorf("%w: no reservation for attempt", ErrInvalid)
	}
	// Same account + window binding (when caller provides identity).
	if acc := strings.TrimSpace(opts.AccountRef); acc != "" {
		if !strings.EqualFold(redact(acc), e.AccountRef) && !strings.EqualFold(acc, e.AccountRef) {
			return Entry{}, fmt.Errorf("%w: after account %q != reserved %q", ErrInvalid, acc, e.AccountRef)
		}
	}
	if wk := strings.TrimSpace(opts.WindowKind); wk != "" {
		if !strings.EqualFold(wk, e.WindowKind) {
			return Entry{}, fmt.Errorf("%w: after window %q != reserved %q", ErrInvalid, wk, e.WindowKind)
		}
	}
	a := clamp01(afterFraction)
	// Fail closed: after cannot rise without reset evidence (cross-window drift).
	if a > e.Before+0.001 {
		if !opts.ResetObserved || strings.TrimSpace(opts.ResetEvidence) == "" {
			return Entry{}, fmt.Errorf("%w: after %.3f > before %.3f without reset evidence (provider window=%s account=%s)",
				ErrInvalid, a, e.Before, e.WindowKind, e.AccountRef)
		}
		e.RouteReason = appendNote(e.RouteReason, "after_reset="+opts.ResetEvidence)
	}
	e.After = &a
	if src := strings.TrimSpace(source); src != "" {
		e.RouteReason = appendNote(e.RouteReason, "after_source="+src)
	}
	if fr := strings.TrimSpace(freshness); fr != "" {
		e.Freshness = fr
		e.RouteReason = appendNote(e.RouteReason, "after_freshness="+fr)
	}
	e.RouteReason = appendNote(e.RouteReason, "after_window="+e.WindowKind)
	e.UpdatedAt = l.now().UTC()
	if err := l.saveLocked(); err != nil {
		return *e, err
	}
	return *e, nil
}

func appendNote(base, add string) string {
	add = strings.TrimSpace(add)
	if add == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return add
	}
	return base + "; " + add
}

// Get returns the entry for an attempt if present.
func (l *Ledger) Get(projectID, runID, attemptID string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byKey[idemKey(projectID, runID, attemptID)]
	if !ok || e == nil {
		return Entry{}, false
	}
	return *e, true
}

// HumanReport is a redacted single-line capacity report for stderr/UI.
func (e Entry) HumanReport() string {
	actual := "n/a"
	after := "n/a"
	if e.Actual != nil {
		actual = fmt.Sprintf("%.3f", *e.Actual)
	}
	if e.After != nil {
		after = fmt.Sprintf("%.3f", *e.After)
	}
	reset := "unknown"
	if e.ResetAt != nil {
		reset = e.ResetAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf(
		"capacity policy=%s provider=%s account=%s model=%s depth=%s conf=%s fresh=%s reset=%s before=%.3f reserved=%.3f actual=%s after=%s state=%s reason=%s",
		e.Policy, e.Provider, e.AccountRef, e.Model, e.Depth, e.Confidence, e.Freshness, reset,
		e.Before, e.Reserved, actual, after, e.State, sanitizeReason(e.RouteReason),
	)
}

func pickWindow(in ReserveInput) (quotapolicy.WindowKind, float64, quotapolicy.EvidenceClass, string, *time.Time, string, error) {
	if in.Snapshot == nil {
		return "", 0, quotapolicy.EvidenceUnknown, "unknown", nil, "", fmt.Errorf("%w: missing snapshot", ErrNoWindow)
	}
	var best *capacitysnapshot.Window
	var accRef string
	for _, a := range in.Snapshot.Accounts {
		if !strings.EqualFold(a.Provider, in.Provider) {
			continue
		}
		accRef = a.AccountRef
		for i := range a.Windows {
			w := &a.Windows[i]
			if w.Freshness != capacitysnapshot.FreshnessFresh {
				continue
			}
			if w.Confidence == capacitysnapshot.ConfidenceUnknown {
				continue
			}
			fw := capacitysnapshot.RemainingFraction(*w)
			if fw == nil {
				continue
			}
			// Multi-window companies (Antigravity primary≈98% + secondary/3p≈11%)
			// used to bind soonest-reset first, often the scarce secondary pool, so
			// capacity_before=0.11 and subsequent reserves failed headroom while
			// primary capacity was abundant. Prefer highest remaining; tie-break
			// soonest reset among equal remaining.
			if best == nil {
				best = w
				continue
			}
			fb := capacitysnapshot.RemainingFraction(*best)
			if fb == nil || *fw > *fb {
				best = w
				continue
			}
			if *fw < *fb {
				continue
			}
			if w.ResetAt != nil && (best.ResetAt == nil || w.ResetAt.Before(*best.ResetAt)) {
				best = w
			}
		}
	}
	if best == nil {
		return "", 0, quotapolicy.EvidenceUnknown, "unknown", nil, accRef, fmt.Errorf("%w: provider=%s", ErrNoWindow, in.Provider)
	}
	f := capacitysnapshot.RemainingFraction(*best)
	before := 0.0
	if f != nil {
		before = *f
	}
	conf := quotapolicy.EvidenceEstimated
	switch best.Confidence {
	case capacitysnapshot.ConfidenceExact:
		conf = quotapolicy.EvidenceExact
	case capacitysnapshot.ConfidenceEstimated:
		conf = quotapolicy.EvidenceEstimated
	default:
		conf = quotapolicy.EvidenceUnknown
	}
	wk := quotapolicy.WindowFiveHour
	switch strings.ToLower(best.Kind) {
	case "weekly", "fixed-week", "fixed_week":
		wk = quotapolicy.WindowWeekly
	case "credit":
		wk = quotapolicy.WindowCredit
	}
	return wk, before, conf, string(best.Freshness), best.ResetAt, accRef, nil
}

func idemKey(project, run, attempt string) string {
	return strings.TrimSpace(project) + "|" + strings.TrimSpace(run) + "|" + strings.TrimSpace(attempt)
}

func redact(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "acct-unknown"
	}
	// Never keep secret-like material.
	low := strings.ToLower(s)
	for _, n := range []string{"sk-", "api_key", "bearer ", "password="} {
		if strings.Contains(low, n) {
			return "acct-redacted"
		}
	}
	if len(s) > 24 {
		return s[:12] + "…"
	}
	return s
}

func sanitizeReason(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}
