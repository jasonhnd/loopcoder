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
	// ErrConflict is returned when an idempotent Reconcile is repeated with a
	// different actual fraction or ActualSource than the durable entry.
	ErrConflict = errors.New("capacityledger: reconcile conflict")
)

// Entry is the durable capacity accounting record for one attempt.
// Values are fractions [0,1] of a window unless Unit describes otherwise.
// Never contains credentials or raw account secrets.
type Entry struct {
	Schema    string `json:"schema"`
	ProjectID string `json:"project_id"`
	RunID     string `json:"run_id"`
	AttemptID string `json:"attempt_id"`
	// PlanDigest is the canonical ExecutionPlanDigest (workflowdef.Normalize).
	// Required for product reserve identity; never derived from AttemptID.
	PlanDigest string `json:"plan_digest,omitempty"`
	// GraphDigest is workgraph.DigestGraph (separate from PlanDigest).
	GraphDigest string `json:"graph_digest,omitempty"`
	// TaskClass is the assignment-time classified floor (luna|tera|soul).
	TaskClass string `json:"task_class,omitempty"`
	// ChildContractDigest is the assignment-time child contract digest.
	ChildContractDigest string                    `json:"child_contract_digest,omitempty"`
	Policy              PolicyName                `json:"policy"`
	Provider            string                    `json:"provider"`
	AccountRef          string                    `json:"account_ref"` // redacted
	Model               string                    `json:"model"`
	Depth               string                    `json:"depth,omitempty"`
	WindowKind          string                    `json:"window_kind"`
	Confidence          quotapolicy.EvidenceClass `json:"confidence"`
	Freshness           string                    `json:"freshness"`
	ResetAt             *time.Time                `json:"reset_at,omitempty"`
	Before              float64                   `json:"capacity_before"` // remaining fraction before reserve
	Reserved            float64                   `json:"capacity_reserved"`
	Actual              *float64                  `json:"capacity_actual,omitempty"`
	// ActualSource is the durable source of Actual (e.g. provider_usage). Empty
	// when Actual is nil (honest unknown). Survives process reopen via JSON.
	ActualSource string `json:"actual_source,omitempty"`
	// ActualConfidence is exact|estimated|unknown. Window-level before/after
	// delta is never exact under concurrent use — label estimated.
	ActualConfidence quotapolicy.EvidenceClass `json:"actual_confidence,omitempty"`
	// InstallRef binds the exact install observation used at reserve/after.
	InstallRef string `json:"install_ref,omitempty"`
	// BeforeSource / BeforeCapturedAt are the selected window's evidence at Reserve
	// (never invented; empty when unknown).
	BeforeSource     string     `json:"before_source,omitempty"`
	BeforeCapturedAt *time.Time `json:"before_captured_at,omitempty"`
	// BeforeInventoryDigest binds the exact immutable inventory report used to
	// select the reserve window.
	BeforeInventoryDigest string `json:"before_inventory_digest,omitempty"`
	// After is remaining after observation or derived estimate.
	After *float64 `json:"capacity_after,omitempty"`
	// AfterState is "observed" (fresh same-window ObserveAfter) or "derived"
	// (Before−Actual only). Derived never qualifies as fresh capacity-after.
	AfterState string `json:"after_state,omitempty"`
	// AfterSource / AfterObservedAt / AfterFreshness / AfterConfidence are set for
	// observed after; derived after uses explicit non-fresh labels and zero ObservedAt.
	AfterSource     string                    `json:"after_source,omitempty"`
	AfterObservedAt *time.Time                `json:"after_observed_at,omitempty"`
	AfterFreshness  string                    `json:"after_freshness,omitempty"`
	AfterConfidence quotapolicy.EvidenceClass `json:"after_confidence,omitempty"`
	// AfterInventoryDigest binds the exact post-run inventory report that
	// supplied the observed remaining value.
	AfterInventoryDigest string `json:"after_inventory_digest,omitempty"`
	ReservationID        string `json:"reservation_id,omitempty"`
	RouteReason          string `json:"route_reason,omitempty"`
	// ReleaseReason is set when State is released (fail/cancel/unknown usage).
	// Does not invent Actual — release remains honest-unknown when no reconcile.
	ReleaseReason  string `json:"release_reason,omitempty"`
	State          string `json:"state"` // reserved|reconciled|released|cancelled|refused
	IdempotencyKey string `json:"idempotency_key"`
	// SoftExpiresAt is the original soft-reservation expiry (restored on reopen).
	SoftExpiresAt *time.Time `json:"soft_expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// AfterStateObserved / AfterStateDerived classify capacity-after evidence.
const (
	AfterStateObserved = "observed"
	AfterStateDerived  = "derived"
)

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

// OpenPath is for tests. Fail closed on corrupt/duplicate/restore errors.
func OpenPath(path string, now func() time.Time) (*Ledger, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Ledger{path: path, store: quotamode.NewStore(now), now: now, byKey: map[string]*Entry{}}
	if err := l.load(); err != nil {
		return nil, err
	}
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
		return fmt.Errorf("%w: corrupt ledger JSON: %v", ErrInvalid, err)
	}
	if doc.Schema != "" && doc.Schema != SchemaFile {
		return fmt.Errorf("%w: ledger schema %q", ErrInvalid, doc.Schema)
	}
	now := l.now().UTC()
	seenKey := map[string]bool{}
	seenRes := map[string]bool{}
	dirty := false
	maxSeq := int64(0)
	for i := range doc.Entries {
		e := doc.Entries[i]
		if strings.TrimSpace(e.Schema) != "" && strings.TrimSpace(e.Schema) != SchemaEntry {
			return fmt.Errorf("%w: entry schema %q", ErrInvalid, e.Schema)
		}
		ikey := strings.TrimSpace(e.IdempotencyKey)
		if ikey == "" {
			ikey = idemKey(e.ProjectID, e.RunID, e.AttemptID)
			e.IdempotencyKey = ikey
		}
		if seenKey[ikey] {
			return fmt.Errorf("%w: duplicate entry key %q", ErrDuplicate, ikey)
		}
		seenKey[ikey] = true
		if rid := strings.TrimSpace(e.ReservationID); rid != "" {
			if seenRes[rid] {
				return fmt.Errorf("%w: duplicate reservation_id %q", ErrDuplicate, rid)
			}
			seenRes[rid] = true
			// Track high-water mark so reopened stores do not recycle IDs.
			if n := parseSresSeq(rid); n > maxSeq {
				maxSeq = n
			}
		}
		// Active reserved must have valid identity.
		if strings.TrimSpace(e.State) == "reserved" {
			if strings.TrimSpace(e.Provider) == "" || strings.TrimSpace(e.Model) == "" ||
				strings.TrimSpace(e.AccountRef) == "" || strings.TrimSpace(e.WindowKind) == "" ||
				strings.TrimSpace(e.ReservationID) == "" || e.Reserved <= 0 {
				return fmt.Errorf("%w: invalid active reservation identity attempt=%q", ErrInvalid, e.AttemptID)
			}
			// Legacy rows missing product execution identity: keep for audit only —
			// release so they cannot be restored as product pressure or idempotent reuse.
			if !productIdentityComplete(e) {
				e.State = "released"
				e.ReleaseReason = "legacy_missing_execution_identity"
				e.UpdatedAt = now
				dirty = true
			} else {
				var exp time.Time
				if e.SoftExpiresAt != nil {
					exp = *e.SoftExpiresAt
				} else if e.ResetAt != nil {
					exp = *e.ResetAt
				} else {
					return fmt.Errorf("%w: reserved entry missing SoftExpiresAt/ResetAt attempt=%q", ErrInvalid, e.AttemptID)
				}
				// Past original expiry: release on load so pressure and ID space stay honest.
				if !exp.After(now) {
					e.State = "released"
					e.ReleaseReason = "soft_expired_on_load"
					e.UpdatedAt = now
					dirty = true
				}
			}
		}
		cp := e
		l.byKey[ikey] = &cp
		// Rehydrate only still-active reserved pressure with original expiry.
		if strings.TrimSpace(e.State) == "reserved" && e.Reserved > 0 && strings.TrimSpace(e.ReservationID) != "" && productIdentityComplete(e) {
			wk := mapWindowKind(e.WindowKind)
			key := quotamode.WindowKey{
				Provider: e.Provider, Account: e.AccountRef,
				Model: e.Model, Window: wk,
			}
			var exp time.Time
			if e.SoftExpiresAt != nil {
				exp = *e.SoftExpiresAt
			} else if e.ResetAt != nil {
				exp = *e.ResetAt
			}
			if err := l.store.RestoreActive(e.ReservationID, key, e.Reserved, e.ProjectID, e.AttemptID, exp); err != nil {
				return fmt.Errorf("%w: restore active %s: %v", ErrInvalid, e.ReservationID, err)
			}
		}
	}
	if maxSeq > 0 {
		l.store.SeedSeq(maxSeq)
	}
	if dirty {
		if err := l.saveLocked(); err != nil {
			return fmt.Errorf("%w: persist expired releases: %v", ErrInvalid, err)
		}
	}
	return nil
}

func parseSresSeq(id string) int64 {
	id = strings.TrimSpace(id)
	const p = "sres_"
	if !strings.HasPrefix(id, p) {
		return 0
	}
	var n int64
	for _, r := range id[len(p):] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
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
// AccountRef, InstallRef, and WindowKind bind exact capacity identity —
// pickWindow selects atomically under those filters only.
// PlanDigest, GraphDigest, TaskClass, and ChildContractDigest are required
// product execution identity — never derived by parsing AttemptID.
type ReserveInput struct {
	ProjectID           string
	RunID               string
	AttemptID           string
	PlanDigest          string // canonical ExecutionPlanDigest (workflowdef.Normalize)
	GraphDigest         string
	TaskClass           string
	ChildContractDigest string
	Policy              PolicyName
	Provider            string
	Model               string
	Depth               string
	AccountRef          string
	// InstallRef exact provider installation identity (pinst_*). Required for
	// production exact capacity routing — binds account+install atomically.
	InstallRef string
	// WindowKind exact window filter (e.g. five_hour, weekly). Empty = best among account.
	WindowKind  string
	RouteReason string
	// Snapshot is the capacity truth used for before/remaining (optional; windows from it).
	Snapshot *capacitysnapshot.Snapshot
	// DemandFraction estimated consumption (0-1). Zero → defaultDemand.
	DemandFraction float64
	// DemandConfidence exact|estimated|unknown
	DemandConfidence quotapolicy.EvidenceClass
}

// Reserve holds capacity for an attempt. Idempotent on project|run|attempt only when
// identity (provider/model/depth/account/install/window) matches exactly; otherwise ErrConflict.
func (l *Ledger) Reserve(in ReserveInput) (Entry, error) {
	if l == nil {
		return Entry{}, fmt.Errorf("%w: nil ledger", ErrInvalid)
	}
	// Product execution identity required — never parse AttemptID for these fields.
	if strings.TrimSpace(in.PlanDigest) == "" {
		return Entry{}, fmt.Errorf("%w: plan_digest (execution plan) required on reserve", ErrInvalid)
	}
	if strings.TrimSpace(in.GraphDigest) == "" {
		return Entry{}, fmt.Errorf("%w: graph_digest required on reserve", ErrInvalid)
	}
	if strings.TrimSpace(in.TaskClass) == "" {
		return Entry{}, fmt.Errorf("%w: task_class required on reserve", ErrInvalid)
	}
	if strings.TrimSpace(in.ChildContractDigest) == "" {
		return Entry{}, fmt.Errorf("%w: child_contract_digest required on reserve", ErrInvalid)
	}
	// Exact capacity routing requires routable account + nonempty window kind + install.
	if !ExactRoutableAccount(CanonicalAccountRef(in.AccountRef)) {
		return Entry{}, fmt.Errorf("%w: account not exact-routable (empty or legacy truncated)", ErrInvalid)
	}
	if strings.TrimSpace(in.InstallRef) == "" {
		return Entry{}, fmt.Errorf("%w: install_ref required nonempty for exact capacity identity", ErrInvalid)
	}
	if strings.TrimSpace(in.WindowKind) == "" {
		return Entry{}, fmt.Errorf("%w: window_kind required nonempty (not default five_hour)", ErrInvalid)
	}
	if mapWindowKind(in.WindowKind) == "" {
		return Entry{}, fmt.Errorf("%w: window_kind unknown/empty", ErrInvalid)
	}
	key := idemKey(in.ProjectID, in.RunID, in.AttemptID)
	l.mu.Lock()
	defer l.mu.Unlock()
	if prev, ok := l.byKey[key]; ok && prev != nil {
		// Restart-safe only while still reserved AND product+route identity matches.
		// Legacy reserved entries missing execution identity cannot be reused.
		if strings.TrimSpace(prev.State) == "reserved" {
			if !productIdentityComplete(*prev) {
				return Entry{}, fmt.Errorf("%w: attempt %q reserved without product execution identity (audit-only; refuse reuse)", ErrInvalid, in.AttemptID)
			}
			if err := identityMatches(*prev, in); err != nil {
				return Entry{}, err
			}
			return *prev, nil
		}
		return Entry{}, fmt.Errorf("%w: attempt %q already in state %q (refuse relaunch)", ErrInvalid, in.AttemptID, prev.State)
	}
	now := l.now().UTC()
	cfg := ModeConfig(in.Policy)
	beforeInventoryDigest := ""
	if in.Snapshot != nil {
		beforeInventoryDigest = in.Snapshot.Digest
	}
	// Atomic account+install+window selection; never cross-wire install/account.
	win, before, conf, fresh, resetAt, accRef, beforeSrc, beforeCap, err := pickWindow(in)
	planDig := strings.TrimSpace(in.PlanDigest)
	graphDig := strings.TrimSpace(in.GraphDigest)
	taskClass := strings.TrimSpace(in.TaskClass)
	ccd := strings.TrimSpace(in.ChildContractDigest)
	if err != nil {
		e := Entry{
			Schema: SchemaEntry, ProjectID: in.ProjectID, RunID: in.RunID, AttemptID: in.AttemptID,
			PlanDigest: planDig, GraphDigest: graphDig, TaskClass: taskClass, ChildContractDigest: ccd,
			Policy: ParsePolicy(string(in.Policy)), Provider: in.Provider, Model: in.Model, Depth: in.Depth,
			AccountRef: CanonicalAccountRef(accRef), InstallRef: strings.TrimSpace(in.InstallRef),
			Confidence: quotapolicy.EvidenceUnknown, Freshness: "unknown",
			BeforeInventoryDigest: beforeInventoryDigest,
			State:                 "refused", IdempotencyKey: key, CreatedAt: now, UpdatedAt: now,
			RouteReason: in.RouteReason,
		}
		l.byKey[key] = &e
		if serr := l.saveLocked(); serr != nil {
			return e, fmt.Errorf("%w; capacity ledger persist refused entry: %v", err, serr)
		}
		return e, err
	}
	accCanon := CanonicalAccountRef(accRef)
	installCanon := strings.TrimSpace(in.InstallRef)
	demand := in.DemandFraction
	if demand <= 0 {
		demand = defaultDemand
	}
	demEv := in.DemandConfidence
	if demEv == "" {
		demEv = quotapolicy.EvidenceEstimated
	}
	wkey := quotamode.WindowKey{
		Provider: in.Provider, Account: accCanon, Model: in.Model, Window: win,
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
	var beforeCapPtr *time.Time
	if !beforeCap.IsZero() {
		t := beforeCap.UTC()
		beforeCapPtr = &t
	}
	if rerr != nil {
		e := Entry{
			Schema: SchemaEntry, ProjectID: in.ProjectID, RunID: in.RunID, AttemptID: in.AttemptID,
			PlanDigest: planDig, GraphDigest: graphDig, TaskClass: taskClass, ChildContractDigest: ccd,
			Policy: ParsePolicy(string(in.Policy)), Provider: in.Provider, Model: in.Model, Depth: in.Depth,
			AccountRef: accCanon, InstallRef: installCanon, WindowKind: string(win), Confidence: conf, Freshness: fresh,
			ResetAt: resetAt, Before: before, BeforeSource: beforeSrc, BeforeCapturedAt: beforeCapPtr,
			BeforeInventoryDigest: beforeInventoryDigest,
			Reserved:              0, State: "refused",
			ReservationID: res.ID, IdempotencyKey: key, CreatedAt: now, UpdatedAt: now,
			RouteReason: in.RouteReason + "; reserve_refused=" + rerr.Error(),
		}
		l.byKey[key] = &e
		if serr := l.saveLocked(); serr != nil {
			return e, fmt.Errorf("%w; capacity ledger persist refused entry: %v", rerr, serr)
		}
		return e, rerr
	}
	exp := res.ExpiresAt
	e := Entry{
		Schema: SchemaEntry, ProjectID: in.ProjectID, RunID: in.RunID, AttemptID: in.AttemptID,
		PlanDigest: planDig, GraphDigest: graphDig, TaskClass: taskClass, ChildContractDigest: ccd,
		Policy: ParsePolicy(string(in.Policy)), Provider: in.Provider, Model: in.Model, Depth: in.Depth,
		AccountRef: accCanon, InstallRef: installCanon, WindowKind: string(win), Confidence: conf, Freshness: fresh,
		ResetAt: resetAt, Before: before, BeforeSource: beforeSrc, BeforeCapturedAt: beforeCapPtr,
		BeforeInventoryDigest: beforeInventoryDigest,
		Reserved:              res.Fraction, State: "reserved",
		ReservationID: res.ID, IdempotencyKey: key, CreatedAt: now, UpdatedAt: now,
		SoftExpiresAt: &exp, RouteReason: in.RouteReason,
	}
	l.byKey[key] = &e
	if err := l.saveLocked(); err != nil {
		return e, err
	}
	return e, nil
}

// productIdentityComplete reports whether Entry carries full product execution
// identity (plan/graph/class/contract). Legacy audit rows may lack these.
func productIdentityComplete(e Entry) bool {
	return strings.TrimSpace(e.PlanDigest) != "" &&
		strings.TrimSpace(e.GraphDigest) != "" &&
		strings.TrimSpace(e.TaskClass) != "" &&
		strings.TrimSpace(e.ChildContractDigest) != ""
}

// identityMatches verifies reserved entry identity equals the reserve request.
// All identity dimensions are required when the existing reservation has them;
// missing request fields conflict (no optional skip). Includes product digests.
func identityMatches(prev Entry, in ReserveInput) error {
	// Product execution identity (explicit fields — never AttemptID-derived).
	if strings.TrimSpace(prev.PlanDigest) != strings.TrimSpace(in.PlanDigest) {
		return fmt.Errorf("%w: plan_digest %q != %q", ErrConflict, prev.PlanDigest, in.PlanDigest)
	}
	if strings.TrimSpace(prev.GraphDigest) != strings.TrimSpace(in.GraphDigest) {
		return fmt.Errorf("%w: graph_digest %q != %q", ErrConflict, prev.GraphDigest, in.GraphDigest)
	}
	if strings.TrimSpace(prev.TaskClass) != strings.TrimSpace(in.TaskClass) {
		return fmt.Errorf("%w: task_class %q != %q", ErrConflict, prev.TaskClass, in.TaskClass)
	}
	if strings.TrimSpace(prev.ChildContractDigest) != strings.TrimSpace(in.ChildContractDigest) {
		return fmt.Errorf("%w: child_contract_digest %q != %q", ErrConflict, prev.ChildContractDigest, in.ChildContractDigest)
	}
	if strings.TrimSpace(prev.Provider) != strings.TrimSpace(in.Provider) {
		return fmt.Errorf("%w: provider %q != %q", ErrConflict, prev.Provider, in.Provider)
	}
	if strings.TrimSpace(prev.Model) != strings.TrimSpace(in.Model) {
		return fmt.Errorf("%w: model %q != %q", ErrConflict, prev.Model, in.Model)
	}
	// Depth: both must match exactly; empty request when prev has depth conflicts.
	if strings.TrimSpace(prev.Depth) != "" || strings.TrimSpace(in.Depth) != "" {
		if strings.TrimSpace(prev.Depth) != strings.TrimSpace(in.Depth) {
			return fmt.Errorf("%w: depth %q != %q", ErrConflict, prev.Depth, in.Depth)
		}
	}
	// Account: require exact canonical equality; missing request when prev set conflicts.
	if strings.TrimSpace(prev.AccountRef) != "" || strings.TrimSpace(in.AccountRef) != "" {
		if strings.TrimSpace(in.AccountRef) == "" {
			return fmt.Errorf("%w: account missing (prev %q)", ErrConflict, prev.AccountRef)
		}
		got := CanonicalAccountRef(in.AccountRef)
		if prev.AccountRef != got {
			return fmt.Errorf("%w: account %q != %q", ErrConflict, prev.AccountRef, got)
		}
	}
	// Install: exact equality required; missing request when prev set conflicts.
	if strings.TrimSpace(prev.InstallRef) != "" || strings.TrimSpace(in.InstallRef) != "" {
		if strings.TrimSpace(in.InstallRef) == "" {
			return fmt.Errorf("%w: install missing (prev %q)", ErrConflict, prev.InstallRef)
		}
		if strings.TrimSpace(prev.InstallRef) != strings.TrimSpace(in.InstallRef) {
			return fmt.Errorf("%w: install %q != %q", ErrConflict, prev.InstallRef, in.InstallRef)
		}
	}
	// Window: exact equality; missing request when prev set conflicts.
	if strings.TrimSpace(prev.WindowKind) != "" || strings.TrimSpace(in.WindowKind) != "" {
		if strings.TrimSpace(in.WindowKind) == "" {
			return fmt.Errorf("%w: window missing (prev %q)", ErrConflict, prev.WindowKind)
		}
		if !windowKindExactEqual(prev.WindowKind, in.WindowKind) {
			return fmt.Errorf("%w: window %q != %q", ErrConflict, prev.WindowKind, in.WindowKind)
		}
	}
	return nil
}

// Reconcile records actual usage for an attempt and persists ActualSource.
// Idempotent only for the same (actual, source) pair: a second call with a
// different actual or source returns ErrConflict rather than silently keeping
// the old durable values.
//
// conf is exact only for provider-reported compatible units on this invocation.
// Window-level before/after aggregate deltas must use EvidenceEstimated.
func (l *Ledger) Reconcile(projectID, runID, attemptID string, actualFraction float64, source string) (Entry, error) {
	return l.ReconcileWithConfidence(projectID, runID, attemptID, actualFraction, source, quotapolicy.EvidenceEstimated)
}

// ReconcileWithConfidence is Reconcile with an explicit actual confidence class.
func (l *Ledger) ReconcileWithConfidence(projectID, runID, attemptID string, actualFraction float64, source string, conf quotapolicy.EvidenceClass) (Entry, error) {
	key := idemKey(projectID, runID, attemptID)
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byKey[key]
	if !ok || e == nil {
		return Entry{}, fmt.Errorf("%w: no reservation for attempt", ErrInvalid)
	}
	a := clamp01(actualFraction)
	src := strings.TrimSpace(source)
	if conf == "" {
		conf = quotapolicy.EvidenceEstimated
	}
	// Never claim exact for window-aggregate delta sources.
	if strings.Contains(strings.ToLower(src), "before_after_delta") || strings.Contains(strings.ToLower(src), "window_delta") {
		conf = quotapolicy.EvidenceEstimated
	}
	if e.State == "reconciled" {
		if e.Actual == nil {
			return Entry{}, fmt.Errorf("%w: reconciled entry missing actual", ErrInvalid)
		}
		sameActual := almostEqual(*e.Actual, a)
		sameSource := strings.EqualFold(strings.TrimSpace(e.ActualSource), src)
		if sameActual && sameSource {
			return *e, nil
		}
		return Entry{}, fmt.Errorf("%w: existing actual=%v source=%q vs new actual=%v source=%q",
			ErrConflict, *e.Actual, e.ActualSource, a, src)
	}
	if e.ReservationID != "" {
		storeConf := conf
		if storeConf == "" {
			storeConf = quotapolicy.EvidenceEstimated
		}
		_, _ = l.store.Reconcile(e.ReservationID, actualFraction, source, storeConf)
	}
	now := l.now().UTC()
	e.Actual = &a
	e.ActualSource = src
	e.ActualConfidence = conf
	// Derived After = Before−Actual only when no observed after yet.
	// Never classify derived as fresh observed; ObserveAfter overrides later.
	if e.After == nil || e.AfterState != AfterStateObserved {
		after := e.Before - a
		if after < 0 {
			after = 0
		}
		e.After = &after
		e.AfterState = AfterStateDerived
		e.AfterSource = "before_minus_actual"
		e.AfterFreshness = "estimated"
		e.AfterConfidence = quotapolicy.EvidenceEstimated
		e.AfterObservedAt = nil // derived has no observation timestamp
	}
	e.State = "reconciled"
	e.ReleaseReason = ""
	e.UpdatedAt = now
	if err := l.saveLocked(); err != nil {
		return *e, err
	}
	return *e, nil
}

// Release frees a reservation without usage (fail/cancel). Idempotent.
// Does not invent Actual — Actual stays nil (honest unknown). Persists
// ReleaseReason. Call ObserveAfter afterward to attach a fresh remaining
// fraction when token actual is unknown.
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
	e.ReleaseReason = strings.TrimSpace(reason)
	// Honest unknown: never invent Actual on release.
	e.Actual = nil
	e.ActualSource = ""
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

func almostEqual(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

// ObserveAfterOpts binds the post-run observation to the same account/window/
// install as the reservation. Cross-window after is rejected (fail closed).
type ObserveAfterOpts struct {
	// AccountRef, WindowKind, InstallRef must match the reservation when non-empty.
	AccountRef string
	WindowKind string
	InstallRef string
	// ObservationID optional source observation id for audit.
	ObservationID string
	// ObservedAt is required nonzero for an observed-after claim (window CapturedAt).
	ObservedAt time.Time
	// Confidence of the after observation (exact|estimated|unknown).
	Confidence quotapolicy.EvidenceClass
	// InventoryDigest is the exact capacity snapshot/report digest that supplied
	// this observation. Production qualification requires it.
	InventoryDigest string
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
// Uses the reservation's exact account/install/window identity (no wildcard).
func (l *Ledger) ObserveAfter(projectID, runID, attemptID string, afterFraction float64, source, freshness string, observedAt time.Time) (Entry, error) {
	key := idemKey(projectID, runID, attemptID)
	l.mu.Lock()
	e, ok := l.byKey[key]
	l.mu.Unlock()
	if !ok || e == nil {
		return Entry{}, fmt.Errorf("%w: no reservation for attempt", ErrInvalid)
	}
	if strings.TrimSpace(e.AccountRef) == "" || strings.TrimSpace(e.InstallRef) == "" || strings.TrimSpace(e.WindowKind) == "" {
		return Entry{}, fmt.Errorf("%w: reservation missing exact account/install/window for ObserveAfter", ErrInvalid)
	}
	return l.ObserveAfterBound(projectID, runID, attemptID, afterFraction, source, freshness, ObserveAfterOpts{
		AccountRef: e.AccountRef, InstallRef: e.InstallRef, WindowKind: e.WindowKind,
		ObservedAt: observedAt,
	})
}

// ObserveAfterBound is ObserveAfter with explicit account/window/install binding.
// Production requires exact nonempty AccountRef/InstallRef/WindowKind — no
// optional wildcard that skips identity checks.
func (l *Ledger) ObserveAfterBound(projectID, runID, attemptID string, afterFraction float64, source, freshness string, opts ObserveAfterOpts) (Entry, error) {
	key := idemKey(projectID, runID, attemptID)
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byKey[key]
	if !ok || e == nil {
		return Entry{}, fmt.Errorf("%w: no reservation for attempt", ErrInvalid)
	}
	acc := strings.TrimSpace(opts.AccountRef)
	wk := strings.TrimSpace(opts.WindowKind)
	iref := strings.TrimSpace(opts.InstallRef)
	if acc == "" || wk == "" || iref == "" {
		return Entry{}, fmt.Errorf("%w: ObserveAfter requires exact nonempty account/install/window identity", ErrInvalid)
	}
	// Same account + window + install binding — exact canonical equality only.
	if CanonicalAccountRef(acc) != CanonicalAccountRef(e.AccountRef) {
		return Entry{}, fmt.Errorf("%w: after account %q != reserved %q", ErrInvalid, acc, e.AccountRef)
	}
	if !windowKindExactEqual(wk, e.WindowKind) {
		return Entry{}, fmt.Errorf("%w: after window %q != reserved %q", ErrInvalid, wk, e.WindowKind)
	}
	if strings.TrimSpace(e.InstallRef) != "" && e.InstallRef != iref {
		return Entry{}, fmt.Errorf("%w: after install %q != reserved %q", ErrInvalid, iref, e.InstallRef)
	}
	e.InstallRef = iref
	src := strings.TrimSpace(source)
	fr := strings.TrimSpace(freshness)
	if src == "" {
		return Entry{}, fmt.Errorf("%w: ObserveAfter requires nonempty source (refuse invent capacity_snapshot)", ErrInvalid)
	}
	if fr == "" {
		return Entry{}, fmt.Errorf("%w: ObserveAfter requires nonempty freshness", ErrInvalid)
	}
	// ObservedAt must be nonzero for an observed-after claim.
	if opts.ObservedAt.IsZero() {
		return Entry{}, fmt.Errorf("%w: ObserveAfter requires nonzero ObservedAt (window CapturedAt)", ErrInvalid)
	}
	if oid := strings.TrimSpace(opts.ObservationID); oid != "" {
		e.RouteReason = appendNote(e.RouteReason, "after_obs="+oid)
	}
	e.RouteReason = appendNote(e.RouteReason, "after_at="+opts.ObservedAt.UTC().Format(time.RFC3339))
	a := clamp01(afterFraction)
	// Fail closed: after cannot rise without reset evidence (cross-window drift).
	if a > e.Before+0.001 {
		if !opts.ResetObserved || strings.TrimSpace(opts.ResetEvidence) == "" {
			return Entry{}, fmt.Errorf("%w: after %.3f > before %.3f without reset evidence (provider window=%s account=%s)",
				ErrInvalid, a, e.Before, e.WindowKind, e.AccountRef)
		}
		e.RouteReason = appendNote(e.RouteReason, "after_reset="+opts.ResetEvidence)
	}
	// Observed after overrides any derived Before−Actual estimate.
	e.After = &a
	e.AfterState = AfterStateObserved
	e.AfterSource = src
	e.AfterFreshness = fr
	obsAt := opts.ObservedAt.UTC()
	e.AfterObservedAt = &obsAt
	e.AfterInventoryDigest = opts.InventoryDigest
	if opts.Confidence != "" {
		e.AfterConfidence = opts.Confidence
	} else {
		e.AfterConfidence = quotapolicy.EvidenceEstimated
	}
	// Keep entry.Freshness aligned with latest after observation for summary.
	e.Freshness = fr
	e.RouteReason = appendNote(e.RouteReason, "after_source="+src)
	e.RouteReason = appendNote(e.RouteReason, "after_freshness="+fr)
	e.RouteReason = appendNote(e.RouteReason, "after_state="+AfterStateObserved)
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

// pickWindow selects account+install+window atomically. When AccountRef/
// InstallRef/WindowKind are requested, only exact-matching rows are considered.
// Never returns account B with a window from account A, or install X for account Y.
// Also returns the selected window's Source and CapturedAt (may be zero/empty).
func pickWindow(in ReserveInput) (quotapolicy.WindowKind, float64, quotapolicy.EvidenceClass, string, *time.Time, string, string, time.Time, error) {
	if in.Snapshot == nil {
		return "", 0, quotapolicy.EvidenceUnknown, "unknown", nil, "", "", time.Time{}, fmt.Errorf("%w: missing snapshot", ErrNoWindow)
	}
	wantAcc := ""
	if strings.TrimSpace(in.AccountRef) != "" {
		wantAcc = CanonicalAccountRef(in.AccountRef)
	}
	wantInstall := strings.TrimSpace(in.InstallRef)
	wantWin := strings.TrimSpace(in.WindowKind)
	var best *capacitysnapshot.Window
	var bestAcc string
	for _, a := range in.Snapshot.Accounts {
		if strings.TrimSpace(a.Provider) != strings.TrimSpace(in.Provider) {
			continue
		}
		accCanon := CanonicalAccountRef(a.AccountRef)
		if wantAcc != "" && accCanon != wantAcc {
			continue
		}
		// Exact install filter — never first-match across installs.
		if wantInstall != "" && strings.TrimSpace(a.InstallRef) != wantInstall {
			continue
		}
		for i := range a.Windows {
			w := &a.Windows[i]
			if w.Freshness != capacitysnapshot.FreshnessFresh {
				continue
			}
			if w.Confidence == capacitysnapshot.ConfidenceUnknown {
				continue
			}
			if wantWin != "" && !windowKindExactEqual(string(w.Kind), wantWin) {
				continue
			}
			fw := capacitysnapshot.RemainingFraction(*w)
			if fw == nil {
				continue
			}
			// Prefer highest remaining; tie-break soonest reset. Account bound to window.
			if best == nil {
				best = w
				bestAcc = accCanon
				continue
			}
			fb := capacitysnapshot.RemainingFraction(*best)
			if fb == nil || *fw > *fb {
				best = w
				bestAcc = accCanon
				continue
			}
			if *fw < *fb {
				continue
			}
			if w.ResetAt != nil && (best.ResetAt == nil || w.ResetAt.Before(*best.ResetAt)) {
				best = w
				bestAcc = accCanon
			}
		}
	}
	if best == nil {
		return "", 0, quotapolicy.EvidenceUnknown, "unknown", nil, wantAcc, "", time.Time{}, fmt.Errorf("%w: provider=%s account=%q install=%q window=%q", ErrNoWindow, in.Provider, wantAcc, wantInstall, wantWin)
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
	wk := mapWindowKind(string(best.Kind))
	// Real window source/captured_at only — never invent capacity_snapshot/now.
	src := strings.TrimSpace(best.Source)
	capAt := best.CapturedAt
	return wk, before, conf, string(best.Freshness), best.ResetAt, bestAcc, src, capAt, nil
}

// mapWindowKind normalizes known aliases but preserves distinct exact kinds.
// Empty/missing is NOT fabricated as five_hour — remains "" for fail-closed routing.
// daily and unknown tokens are NOT coerced to five_hour.
func mapWindowKind(kind string) quotapolicy.WindowKind {
	k := strings.ToLower(strings.TrimSpace(kind))
	if k == "" {
		return "" // unknown — never invent five_hour
	}
	switch k {
	case "weekly", "fixed-week", "fixed_week":
		return quotapolicy.WindowWeekly
	case "credit":
		return quotapolicy.WindowCredit
	case "rate_limit", "rate-limit":
		return quotapolicy.WindowRateLimit
	case "five_hour", "fixed_hour", "fixed-hour", "5h", "fixedhour":
		return quotapolicy.WindowFiveHour
	case "other":
		return quotapolicy.WindowOther
	default:
		// Preserve exact normalized token (e.g. "daily") — never five_hour.
		return quotapolicy.WindowKind(k)
	}
}

// windowKindExactEqual compares window kinds with known aliases only.
// Unknown tokens (e.g. "daily") do not silently equal five_hour.
func windowKindExactEqual(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return true
	}
	return normalizeWindowToken(a) == normalizeWindowToken(b)
}

func normalizeWindowToken(k string) string {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "weekly", "fixed-week", "fixed_week":
		return "weekly"
	case "credit":
		return "credit"
	case "five_hour", "fixed_hour", "fixed-hour", "5h", "fixedhour":
		return "five_hour"
	default:
		// Preserve distinct unknown kinds (daily ≠ five_hour).
		return strings.ToLower(strings.TrimSpace(k))
	}
}

// AccountRefUnknown is the sentinel for empty/missing account input.
// It is NOT a routable exact identity — never collapse all unknowns into one
// "exact" account for capacity routing.
const AccountRefUnknown = ""

// AccountRefLegacyPrefix marks collision-prone truncated opaque IDs
// (acct- + 16 hex). These cannot qualify exact real routing; callers must refresh.
const AccountRefLegacyInsufficient = "legacy-insufficient:"

// CanonicalAccountRef is the single authoritative opaque account identity.
// Collision-safe exact equality via FULL SHA-256 (64 hex) — never truncates into
// a collision-prone prefix, never exposes emails/profile IDs/credentials.
// Exact match only after canonicalization (never EqualFold/suffix/substring).
//
// Forms:
//   - Empty input → "" (unknown / non-routable; NOT "acct-unknown")
//   - New exact: "acct-" + 64 lowercase hex (full SHA-256)
//   - Legacy short "acct-"+16hex → "legacy-insufficient:"+lower (not exact-routable)
func CanonicalAccountRef(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return AccountRefUnknown
	}
	// Already full opaque form: "acct-" + 64 hex.
	if strings.HasPrefix(s, "acct-") && len(s) == 5+64 && isHexAccount(s[5:]) {
		return strings.ToLower(s)
	}
	// Legacy short opaque (16 hex) — mark insufficient; never treat as exact route ID.
	if strings.HasPrefix(s, "acct-") && len(s) == 5+16 && isHexAccount(s[5:]) {
		return AccountRefLegacyInsufficient + strings.ToLower(s)
	}
	// Already marked legacy.
	if strings.HasPrefix(s, AccountRefLegacyInsufficient) {
		return s
	}
	// Secret-like or email-like material → still deterministic full opaque hash.
	low := strings.ToLower(s)
	for _, n := range []string{"sk-", "api_key", "bearer ", "password=", "@"} {
		if strings.Contains(low, n) {
			sum := sha256.Sum256([]byte("secret|" + s))
			return "acct-" + hex.EncodeToString(sum[:])
		}
	}
	sum := sha256.Sum256([]byte("acct|" + s))
	return "acct-" + hex.EncodeToString(sum[:])
}

// ExactRoutableAccount reports whether a canonical account ref qualifies for
// exact capacity routing (full opaque only).
func ExactRoutableAccount(canon string) bool {
	c := strings.TrimSpace(canon)
	if c == "" || strings.HasPrefix(c, AccountRefLegacyInsufficient) {
		return false
	}
	return strings.HasPrefix(c, "acct-") && len(c) == 5+64 && isHexAccount(c[5:])
}

func isHexAccount(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return len(s) > 0
}

func idemKey(project, run, attempt string) string {
	return strings.TrimSpace(project) + "|" + strings.TrimSpace(run) + "|" + strings.TrimSpace(attempt)
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
