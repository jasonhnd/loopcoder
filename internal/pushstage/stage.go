package pushstage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	SchemaIntent  = "loopcoder.push.intent.v1"
	SchemaReceipt = "loopcoder.push.receipt.v1"
)

// FailureClass is a typed pre-side-effect or reconciliation outcome.
type FailureClass string

const (
	FailNone           FailureClass = ""
	FailWrongRemote    FailureClass = "wrong_remote"
	FailConflict       FailureClass = "conflicting_branch"
	FailNonFastForward FailureClass = "non_fast_forward"
	FailAuth           FailureClass = "auth_failure"
	FailRateLimit      FailureClass = "rate_limit"
	FailHook           FailureClass = "hook_failure"
	FailTimeout        FailureClass = "timeout"
	FailUnknown        FailureClass = "unknown"
	FailNotApplied     FailureClass = "not_applied"
)

var (
	ErrInvalid     = errors.New("pushstage: invalid")
	ErrNotReady    = errors.New("pushstage: not ready")
	ErrConflict    = errors.New("pushstage: conflict")
	ErrTimeout     = errors.New("pushstage: timeout")
	ErrAuth        = errors.New("pushstage: auth")
	ErrRateLimited = errors.New("pushstage: rate limited")
	ErrHook        = errors.New("pushstage: hook")
	ErrForceDenied = errors.New("pushstage: force-push denied")
)

// Intent freezes push inputs (no credentials).
type Intent struct {
	Schema           string `json:"schema"`
	AttemptID        string `json:"attempt_id"`
	IdempotencyKey   string `json:"idempotency_key"`
	RemoteName       string `json:"remote_name"`       // e.g. origin
	RemoteURLDigest  string `json:"remote_url_digest"` // redacted identity, never full credential URL
	Branch           string `json:"branch"`
	ExpectedOldOID   string `json:"expected_old_oid"` // empty = create
	ExpectedNewOID   string `json:"expected_new_oid"` // accepted commit
	CommitReceiptKey string `json:"commit_receipt_key"`
	HookReceiptOK    bool   `json:"hook_receipt_ok"`
	CommitReceiptOK  bool   `json:"commit_receipt_ok"`
}

// Receipt is success after remote OID read-back.
type Receipt struct {
	Schema         string `json:"schema"`
	AttemptID      string `json:"attempt_id"`
	IdempotencyKey string `json:"idempotency_key"`
	RemoteName     string `json:"remote_name"`
	Branch         string `json:"branch"`
	OldOID         string `json:"old_oid"`
	NewOID         string `json:"new_oid"`
	// No credential material ever.
	CreatedAt time.Time `json:"created_at"`
}

// ReconcileState is the result of timeout/ambiguous exit inspection.
type ReconcileState string

const (
	ReconcileNotApplied ReconcileState = "not_applied"
	ReconcileApplied    ReconcileState = "applied"
	ReconcileConflict   ReconcileState = "conflict"
	ReconcileUnknown    ReconcileState = "unknown"
)

// Result is the push stage outcome.
type Result struct {
	OK        bool           `json:"ok"`
	Adopted   bool           `json:"adopted"`
	Failure   FailureClass   `json:"failure,omitempty"`
	Reconcile ReconcileState `json:"reconcile,omitempty"`
	Receipt   *Receipt       `json:"receipt,omitempty"`
	Message   string         `json:"message,omitempty"`
}

// Remote is a scrubbed, bounded transport (fakeable).
type Remote interface {
	// ReadRef returns remote branch OID if present.
	ReadRef(remote, branch string) (oid string, exists bool, err error)
	// PushNonForce pushes newOID only if remote matches expectedOld (empty = missing).
	// Must never force-update.
	PushNonForce(remote, branch, expectedOld, newOID string) error
	// RateLimited reports transport rate limit.
	RateLimited() bool
}

// Store holds intents and receipts.
type Store struct {
	mu       sync.Mutex
	intents  map[string]Intent
	receipts map[string]Receipt
	now      func() time.Time
}

// NewStore creates an empty store.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{intents: map[string]Intent{}, receipts: map[string]Receipt{}, now: now}
}

// Service performs push stage.
type Service struct {
	Store  *Store
	Remote Remote
	// FailPushWith injects first-call errors (tests: timeout after/before).
	FailPushWith error
	// AfterFailApplied when FailPushWith is timeout, remote already updated.
	AfterFailApplied bool
}

// Freeze validates and stores immutable intent.
func (s *Service) Freeze(in Intent) (Intent, error) {
	if in.AttemptID == "" || in.IdempotencyKey == "" || in.RemoteName == "" || in.Branch == "" {
		return Intent{}, ErrInvalid
	}
	if in.ExpectedNewOID == "" || len(in.ExpectedNewOID) < 7 {
		return Intent{}, ErrInvalid
	}
	if !in.CommitReceiptOK {
		return Intent{}, fmt.Errorf("%w: commit receipt required", ErrNotReady)
	}
	if !in.HookReceiptOK {
		return Intent{}, fmt.Errorf("%w: hook policy receipt required", ErrNotReady)
	}
	if strings.Contains(in.Branch, "..") || strings.HasPrefix(in.Branch, "/") {
		return Intent{}, ErrInvalid
	}
	// Reject credential-looking remote digests being raw URLs with secrets
	if strings.Contains(strings.ToLower(in.RemoteURLDigest), "ghp_") || strings.Contains(in.RemoteURLDigest, "@") {
		// digest should be hash-like; if looks like URL with userinfo, reject
		if strings.Contains(in.RemoteURLDigest, "://") {
			return Intent{}, fmt.Errorf("%w: remote url must be digests only", ErrInvalid)
		}
	}
	in.Schema = SchemaIntent
	if in.RemoteURLDigest == "" {
		in.RemoteURLDigest = digestStr(in.RemoteName)
	}
	s.Store.mu.Lock()
	defer s.Store.mu.Unlock()
	s.Store.intents[in.IdempotencyKey] = in
	return in, nil
}

// PushOrAdopt publishes or adopts matching remote state.
func (s *Service) PushOrAdopt(ctx context.Context, key string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{Failure: FailTimeout, Message: "context done"}, ErrTimeout
	}
	s.Store.mu.Lock()
	if r, ok := s.Store.receipts[key]; ok {
		s.Store.mu.Unlock()
		cp := r
		return Result{OK: true, Adopted: true, Receipt: &cp, Reconcile: ReconcileApplied}, nil
	}
	in, ok := s.Store.intents[key]
	s.Store.mu.Unlock()
	if !ok {
		return Result{Failure: FailUnknown, Message: "no intent"}, ErrInvalid
	}

	if s.Remote.RateLimited() {
		return Result{Failure: FailRateLimit, Message: "rate limited"}, ErrRateLimited
	}

	// Pre-read remote
	cur, exists, err := s.Remote.ReadRef(in.RemoteName, in.Branch)
	if err != nil {
		return Result{Failure: FailUnknown, Message: redact(err.Error())}, err
	}
	if exists && cur == in.ExpectedNewOID {
		// already at intent — adopt without push
		return s.persistReceipt(in, in.ExpectedOldOID, cur)
	}
	if exists && in.ExpectedOldOID != "" && cur != in.ExpectedOldOID {
		return Result{Failure: FailConflict, Message: "remote moved", Reconcile: ReconcileConflict}, ErrConflict
	}
	if exists && in.ExpectedOldOID == "" && cur != in.ExpectedNewOID {
		// create expected but ref exists with other OID
		return Result{Failure: FailNonFastForward, Message: "ref exists", Reconcile: ReconcileConflict}, ErrConflict
	}
	if exists && in.ExpectedOldOID != "" && cur == in.ExpectedOldOID {
		// fast-forward candidate
	}
	if !exists && in.ExpectedOldOID != "" {
		return Result{Failure: FailConflict, Message: "expected old missing", Reconcile: ReconcileConflict}, ErrConflict
	}

	// Push (never force)
	pushErr := s.FailPushWith
	if pushErr == nil {
		pushErr = s.Remote.PushNonForce(in.RemoteName, in.Branch, in.ExpectedOldOID, in.ExpectedNewOID)
	} else if s.AfterFailApplied {
		// simulate timeout after remote applied
		_ = s.Remote.PushNonForce(in.RemoteName, in.Branch, in.ExpectedOldOID, in.ExpectedNewOID)
		s.FailPushWith = nil
	} else {
		// timeout before apply — leave remote unchanged
		s.FailPushWith = nil
	}

	if pushErr != nil {
		return s.reconcileAfterError(in, pushErr)
	}

	// Success path: read-back required
	return s.readBackSuccess(in)
}

func (s *Service) reconcileAfterError(in Intent, pushErr error) (Result, error) {
	// Map typed errors
	switch {
	case errors.Is(pushErr, ErrAuth):
		return Result{Failure: FailAuth, Message: "auth failed"}, pushErr
	case errors.Is(pushErr, ErrRateLimited):
		return Result{Failure: FailRateLimit, Message: "rate limited"}, pushErr
	case errors.Is(pushErr, ErrHook):
		return Result{Failure: FailHook, Message: "pre-push hook failed"}, pushErr
	case errors.Is(pushErr, ErrConflict):
		return Result{Failure: FailNonFastForward, Message: "non-fast-forward", Reconcile: ReconcileConflict}, pushErr
	case errors.Is(pushErr, ErrForceDenied):
		return Result{Failure: FailConflict, Message: "force denied"}, pushErr
	case errors.Is(pushErr, ErrTimeout) || errors.Is(pushErr, context.DeadlineExceeded):
		// reconcile remote
		cur, exists, err := s.Remote.ReadRef(in.RemoteName, in.Branch)
		if err != nil {
			return Result{Failure: FailUnknown, Reconcile: ReconcileUnknown, Message: "timeout; read-back failed"}, ErrTimeout
		}
		if exists && cur == in.ExpectedNewOID {
			// applied despite timeout
			res, e := s.persistReceipt(in, in.ExpectedOldOID, cur)
			if e != nil {
				return res, e
			}
			res.Adopted = true
			res.Reconcile = ReconcileApplied
			res.Message = "timeout after apply; adopted"
			return res, nil
		}
		if !exists || cur == in.ExpectedOldOID {
			return Result{Failure: FailNotApplied, Reconcile: ReconcileNotApplied, Message: "timeout; not applied"}, ErrTimeout
		}
		return Result{Failure: FailConflict, Reconcile: ReconcileConflict, Message: "timeout; conflicting remote"}, ErrConflict
	default:
		// wrong remote style errors
		if strings.Contains(strings.ToLower(pushErr.Error()), "wrong remote") {
			return Result{Failure: FailWrongRemote, Message: pushErr.Error()}, pushErr
		}
		return Result{Failure: FailUnknown, Message: redact(pushErr.Error())}, pushErr
	}
}

func (s *Service) readBackSuccess(in Intent) (Result, error) {
	cur, exists, err := s.Remote.ReadRef(in.RemoteName, in.Branch)
	if err != nil {
		return Result{Failure: FailUnknown, Message: "read-back failed"}, err
	}
	if !exists || cur != in.ExpectedNewOID {
		return Result{Failure: FailUnknown, Message: "remote oid mismatch after push", Reconcile: ReconcileUnknown}, ErrConflict
	}
	return s.persistReceipt(in, in.ExpectedOldOID, cur)
}

func (s *Service) persistReceipt(in Intent, oldOID, newOID string) (Result, error) {
	// Never store credentials
	rec := Receipt{
		Schema: SchemaReceipt, AttemptID: in.AttemptID, IdempotencyKey: in.IdempotencyKey,
		RemoteName: in.RemoteName, Branch: in.Branch, OldOID: oldOID, NewOID: newOID,
		CreatedAt: s.Store.now().UTC(),
	}
	s.Store.mu.Lock()
	s.Store.receipts[in.IdempotencyKey] = rec
	s.Store.mu.Unlock()
	cp := rec
	return Result{OK: true, Receipt: &cp, Reconcile: ReconcileApplied}, nil
}

// GetReceipt returns stored receipt.
func (s *Store) GetReceipt(key string) (Receipt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[key]
	return r, ok
}

// ScrubEnv removes transport-dangerous and secret env for Git push.
func ScrubEnv(env []string) []string {
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		uk := strings.ToUpper(k)
		if strings.Contains(uk, "TOKEN") || strings.Contains(uk, "SECRET") || strings.Contains(uk, "PASSWORD") {
			continue
		}
		if strings.HasPrefix(uk, "GIT_DIR") || strings.HasPrefix(uk, "GIT_WORK_TREE") || uk == "GIT_INDEX_FILE" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func digestStr(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

func redact(s string) string {
	lower := strings.ToLower(s)
	for _, bad := range []string{"sk-", "ghp_", "password", "token=", "bearer "} {
		if strings.Contains(lower, bad) {
			return "[redacted]"
		}
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// --- Fake remote ---

// FakeRemote is an in-memory remote ref map.
type FakeRemote struct {
	mu      sync.Mutex
	refs    map[string]string // remote/branch -> oid
	limited bool
	auth    bool // false => push fails auth
	// RejectForce if true, any force-like call fails
}

// NewFakeRemote creates an empty remote.
func NewFakeRemote() *FakeRemote {
	return &FakeRemote{refs: map[string]string{}, auth: true}
}

func (f *FakeRemote) key(remote, branch string) string {
	return remote + "/" + branch
}

// SetRef sets a remote ref (tests).
func (f *FakeRemote) SetRef(remote, branch, oid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[f.key(remote, branch)] = oid
}

// SetRateLimited toggles rate limit.
func (f *FakeRemote) SetRateLimited(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limited = v
}

// SetAuth toggles auth.
func (f *FakeRemote) SetAuth(ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = ok
}

// RateLimited implements Remote.
func (f *FakeRemote) RateLimited() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.limited
}

// ReadRef implements Remote.
func (f *FakeRemote) ReadRef(remote, branch string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	oid, ok := f.refs[f.key(remote, branch)]
	return oid, ok, nil
}

// PushNonForce implements Remote without force.
func (f *FakeRemote) PushNonForce(remote, branch, expectedOld, newOID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.auth {
		return ErrAuth
	}
	if f.limited {
		return ErrRateLimited
	}
	k := f.key(remote, branch)
	cur, exists := f.refs[k]
	if expectedOld == "" {
		if exists {
			return ErrConflict
		}
		f.refs[k] = newOID
		return nil
	}
	if !exists || cur != expectedOld {
		return ErrConflict
	}
	// non-fast-forward would be if we tried to rewrite — we only set newOID from expected old
	f.refs[k] = newOID
	return nil
}
