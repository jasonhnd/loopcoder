package prstage

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
	SchemaIntent  = "loopcoder.pr.intent.v1"
	SchemaReceipt = "loopcoder.pr.receipt.v1"
)

// FailureClass is typed PR stage failure.
type FailureClass string

const (
	FailNone         FailureClass = ""
	FailWrongRepo    FailureClass = "wrong_repository"
	FailConflict     FailureClass = "conflicting_pr"
	FailChangedHead  FailureClass = "changed_head"
	FailPermission   FailureClass = "permission"
	FailRateLimit    FailureClass = "rate_limit"
	FailBodyMismatch FailureClass = "body_mismatch"
	FailTimeout      FailureClass = "timeout"
	FailSourceDrift  FailureClass = "source_drift"
)

var (
	ErrInvalid     = errors.New("prstage: invalid")
	ErrNotReady    = errors.New("prstage: not ready")
	ErrConflict    = errors.New("prstage: conflict")
	ErrTimeout     = errors.New("prstage: timeout")
	ErrPermission  = errors.New("prstage: permission")
	ErrRateLimited = errors.New("prstage: rate limited")
)

// Intent freezes PR create inputs.
type Intent struct {
	Schema              string `json:"schema"`
	AttemptID           string `json:"attempt_id"`
	IdempotencyKey      string `json:"idempotency_key"`
	RepoOwner           string `json:"repo_owner"`
	RepoName            string `json:"repo_name"`
	BaseRef             string `json:"base_ref"`
	BaseOID             string `json:"base_oid"`
	HeadRef             string `json:"head_ref"`
	HeadOID             string `json:"head_oid"`
	SourceIssue         int    `json:"source_issue"`
	TitleDigest         string `json:"title_digest"`
	BodyDigest          string `json:"body_digest"`
	Title               string `json:"-"`
	Body                string `json:"-"`
	RouteSummary        string `json:"route_summary"` // redacted requested/actual
	VerificationSummary string `json:"verification_summary"`
	HookSummary         string `json:"hook_summary"`
	RunIDRedacted       string `json:"run_id_redacted"`
	PushReceiptOK       bool   `json:"push_receipt_ok"`
}

// PRView is read-back PR state.
type PRView struct {
	Number    int
	RepoOwner string
	RepoName  string
	BaseRef   string
	BaseOID   string
	HeadRef   string
	HeadOID   string
	Title     string
	Body      string
	URL       string
	Open      bool
}

// Receipt is success after GitHub read-back.
type Receipt struct {
	Schema              string    `json:"schema"`
	AttemptID           string    `json:"attempt_id"`
	IdempotencyKey      string    `json:"idempotency_key"`
	PRNumber            int       `json:"pr_number"`
	URL                 string    `json:"url"`
	RepoOwner           string    `json:"repo_owner"`
	RepoName            string    `json:"repo_name"`
	BaseRef             string    `json:"base_ref"`
	BaseOID             string    `json:"base_oid"`
	HeadRef             string    `json:"head_ref"`
	HeadOID             string    `json:"head_oid"`
	SourceIssue         int       `json:"source_issue"`
	TitleDigest         string    `json:"title_digest"`
	BodyDigest          string    `json:"body_digest"`
	RouteSummary        string    `json:"route_summary"`
	VerificationSummary string    `json:"verification_summary"`
	HookSummary         string    `json:"hook_summary"`
	RunIDRedacted       string    `json:"run_id_redacted"`
	CreatedAt           time.Time `json:"created_at"`
}

// Result is create/adopt outcome.
type Result struct {
	OK      bool         `json:"ok"`
	Adopted bool         `json:"adopted"`
	Failure FailureClass `json:"failure,omitempty"`
	Receipt *Receipt     `json:"receipt,omitempty"`
	Message string       `json:"message,omitempty"`
}

// GitHub is fakeable PR API.
type GitHub interface {
	// FindCompatible finds open PR with exact owner/repo/base/head refs.
	FindCompatible(owner, name, baseRef, headRef string) (PRView, bool, error)
	// CreatePR creates a PR; never merges.
	CreatePR(owner, name, baseRef, headRef, title, body string) (PRView, error)
	// ReadPR reads PR by number.
	ReadPR(owner, name string, number int) (PRView, error)
	RateLimited() bool
	Authorized() bool
}

// Store holds intents and receipts.
type Store struct {
	mu       sync.Mutex
	intents  map[string]Intent
	receipts map[string]Receipt
	now      func() time.Time
}

func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{intents: map[string]Intent{}, receipts: map[string]Receipt{}, now: now}
}

// Service performs PR stage.
type Service struct {
	Store  *Store
	GitHub GitHub
	// FailCreateWith injects create errors for tests.
	FailCreateWith error
	// AfterFailCreated when timeout after create applied.
	AfterFailCreated bool
}

// Freeze stores immutable intent after sanitizing title/body digests.
func (s *Service) Freeze(in Intent) (Intent, error) {
	if in.AttemptID == "" || in.IdempotencyKey == "" || in.RepoOwner == "" || in.RepoName == "" {
		return Intent{}, ErrInvalid
	}
	if in.BaseRef == "" || in.HeadRef == "" || in.HeadOID == "" || in.SourceIssue <= 0 {
		return Intent{}, ErrInvalid
	}
	if !in.PushReceiptOK {
		return Intent{}, fmt.Errorf("%w: push receipt required", ErrNotReady)
	}
	in.Schema = SchemaIntent
	in.Title = sanitize(in.Title)
	in.Body = sanitize(in.Body)
	if in.TitleDigest == "" {
		in.TitleDigest = digestStr(in.Title)
	}
	if in.BodyDigest == "" {
		in.BodyDigest = digestStr(in.Body)
	}
	// ensure issue linkage digests, not private bodies
	if strings.Contains(strings.ToLower(in.Body), "sk-") || strings.Contains(strings.ToLower(in.Body), "ghp_") {
		in.Body = "[redacted]"
		in.BodyDigest = digestStr(in.Body)
	}
	s.Store.mu.Lock()
	s.Store.intents[in.IdempotencyKey] = in
	s.Store.mu.Unlock()
	return in, nil
}

// CreateOrAdopt creates once or adopts exact compatible PR.
func (s *Service) CreateOrAdopt(ctx context.Context, key string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{Failure: FailTimeout}, ErrTimeout
	}
	s.Store.mu.Lock()
	if r, ok := s.Store.receipts[key]; ok {
		s.Store.mu.Unlock()
		cp := r
		return Result{OK: true, Adopted: true, Receipt: &cp}, nil
	}
	in, ok := s.Store.intents[key]
	s.Store.mu.Unlock()
	if !ok {
		return Result{Failure: FailNone, Message: "no intent"}, ErrInvalid
	}

	if s.GitHub.RateLimited() {
		return Result{Failure: FailRateLimit, Message: "rate limited"}, ErrRateLimited
	}
	if !s.GitHub.Authorized() {
		return Result{Failure: FailPermission, Message: "permission denied"}, ErrPermission
	}

	// Read before create
	existing, found, err := s.GitHub.FindCompatible(in.RepoOwner, in.RepoName, in.BaseRef, in.HeadRef)
	if err != nil {
		return Result{Failure: FailNone, Message: redact(err.Error())}, err
	}
	if found {
		return s.adoptIfExact(in, existing)
	}

	// Create
	var view PRView
	createErr := s.FailCreateWith
	if createErr == nil {
		view, createErr = s.GitHub.CreatePR(in.RepoOwner, in.RepoName, in.BaseRef, in.HeadRef, in.Title, in.Body)
	} else if s.AfterFailCreated {
		view, _ = s.GitHub.CreatePR(in.RepoOwner, in.RepoName, in.BaseRef, in.HeadRef, in.Title, in.Body)
		s.FailCreateWith = nil
		createErr = ErrTimeout
	} else {
		s.FailCreateWith = nil
		// timeout before create
		createErr = ErrTimeout
	}

	if createErr != nil {
		return s.reconcileCreateError(in, createErr)
	}

	// Read-back
	rb, err := s.GitHub.ReadPR(in.RepoOwner, in.RepoName, view.Number)
	if err != nil {
		return Result{Failure: FailNone, Message: "read-back failed"}, err
	}
	return s.persistIfMatch(in, rb)
}

func (s *Service) reconcileCreateError(in Intent, createErr error) (Result, error) {
	switch {
	case errors.Is(createErr, ErrPermission):
		return Result{Failure: FailPermission}, createErr
	case errors.Is(createErr, ErrRateLimited):
		return Result{Failure: FailRateLimit}, createErr
	case errors.Is(createErr, ErrConflict):
		return Result{Failure: FailConflict}, createErr
	case errors.Is(createErr, ErrTimeout):
		// re-find
		existing, found, err := s.GitHub.FindCompatible(in.RepoOwner, in.RepoName, in.BaseRef, in.HeadRef)
		if err != nil {
			return Result{Failure: FailTimeout, Message: "timeout; find failed"}, ErrTimeout
		}
		if found {
			res, e := s.adoptIfExact(in, existing)
			if e == nil {
				res.Adopted = true
				res.Message = "timeout after create; adopted"
			}
			return res, e
		}
		return Result{Failure: FailTimeout, Message: "timeout; not created"}, ErrTimeout
	default:
		return Result{Failure: FailNone, Message: redact(createErr.Error())}, createErr
	}
}

func (s *Service) adoptIfExact(in Intent, pr PRView) (Result, error) {
	if !strings.EqualFold(pr.RepoOwner, in.RepoOwner) || !strings.EqualFold(pr.RepoName, in.RepoName) {
		return Result{Failure: FailWrongRepo}, ErrConflict
	}
	if pr.BaseRef != in.BaseRef || pr.HeadRef != in.HeadRef {
		return Result{Failure: FailConflict}, ErrConflict
	}
	if in.HeadOID != "" && pr.HeadOID != "" && pr.HeadOID != in.HeadOID {
		return Result{Failure: FailChangedHead, Message: "head oid changed"}, ErrConflict
	}
	// title/body digests if available
	if pr.Title != "" && digestStr(pr.Title) != in.TitleDigest && in.Title != "" && pr.Title != in.Title {
		// allow if digests match sanitize
		if digestStr(sanitize(pr.Title)) != in.TitleDigest {
			return Result{Failure: FailBodyMismatch, Message: "title mismatch"}, ErrConflict
		}
	}
	return s.persistIfMatch(in, pr)
}

func (s *Service) persistIfMatch(in Intent, pr PRView) (Result, error) {
	if !pr.Open {
		return Result{Failure: FailConflict, Message: "pr not open"}, ErrConflict
	}
	if pr.HeadOID != "" && in.HeadOID != "" && pr.HeadOID != in.HeadOID {
		return Result{Failure: FailChangedHead}, ErrConflict
	}
	rec := Receipt{
		Schema: SchemaReceipt, AttemptID: in.AttemptID, IdempotencyKey: in.IdempotencyKey,
		PRNumber: pr.Number, URL: pr.URL, RepoOwner: pr.RepoOwner, RepoName: pr.RepoName,
		BaseRef: pr.BaseRef, BaseOID: firstNonEmpty(pr.BaseOID, in.BaseOID),
		HeadRef: pr.HeadRef, HeadOID: firstNonEmpty(pr.HeadOID, in.HeadOID),
		SourceIssue: in.SourceIssue, TitleDigest: in.TitleDigest, BodyDigest: in.BodyDigest,
		RouteSummary: in.RouteSummary, VerificationSummary: in.VerificationSummary,
		HookSummary: in.HookSummary, RunIDRedacted: in.RunIDRedacted,
		CreatedAt: s.Store.now().UTC(),
	}
	s.Store.mu.Lock()
	s.Store.receipts[in.IdempotencyKey] = rec
	s.Store.mu.Unlock()
	cp := rec
	return Result{OK: true, Receipt: &cp}, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func digestStr(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

func sanitize(s string) string {
	lower := strings.ToLower(s)
	for _, bad := range []string{"sk-", "ghp_", "password", "api_key", "bearer ", "-----begin"} {
		if strings.Contains(lower, bad) {
			return "[redacted]"
		}
	}
	if len(s) > 4000 {
		return s[:4000]
	}
	return s
}

func redact(s string) string {
	return sanitize(s)
}

// --- Fake GitHub ---

type FakeGitHub struct {
	mu      sync.Mutex
	prs     map[string]PRView // owner/name#n
	byHead  map[string]int    // owner/name/base/head -> number
	seq     int
	limited bool
	auth    bool
}

func NewFakeGitHub() *FakeGitHub {
	return &FakeGitHub{prs: map[string]PRView{}, byHead: map[string]int{}, auth: true}
}

func (f *FakeGitHub) RateLimited() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.limited
}

func (f *FakeGitHub) Authorized() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth
}

func (f *FakeGitHub) SetRateLimited(v bool) { f.mu.Lock(); f.limited = v; f.mu.Unlock() }
func (f *FakeGitHub) SetAuth(v bool)        { f.mu.Lock(); f.auth = v; f.mu.Unlock() }

func (f *FakeGitHub) headKey(owner, name, base, head string) string {
	return strings.ToLower(owner + "/" + name + "/" + base + "/" + head)
}

func (f *FakeGitHub) FindCompatible(owner, name, baseRef, headRef string) (PRView, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.byHead[f.headKey(owner, name, baseRef, headRef)]
	if !ok {
		return PRView{}, false, nil
	}
	pr, ok := f.prs[fmt.Sprintf("%s/%s#%d", owner, name, n)]
	return pr, ok, nil
}

func (f *FakeGitHub) CreatePR(owner, name, baseRef, headRef, title, body string) (PRView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.auth {
		return PRView{}, ErrPermission
	}
	if f.limited {
		return PRView{}, ErrRateLimited
	}
	k := f.headKey(owner, name, baseRef, headRef)
	if _, ok := f.byHead[k]; ok {
		return PRView{}, ErrConflict
	}
	f.seq++
	n := f.seq
	pr := PRView{
		Number: n, RepoOwner: owner, RepoName: name,
		BaseRef: baseRef, HeadRef: headRef, Title: title, Body: body,
		URL:  fmt.Sprintf("https://example.test/%s/%s/pull/%d", owner, name, n),
		Open: true, HeadOID: "", BaseOID: "",
	}
	// allow tests to set OIDs via fields after — default empty means match any in adopt
	f.prs[fmt.Sprintf("%s/%s#%d", owner, name, n)] = pr
	f.byHead[k] = n
	return pr, nil
}

func (f *FakeGitHub) ReadPR(owner, name string, number int) (PRView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.prs[fmt.Sprintf("%s/%s#%d", owner, name, number)]
	if !ok {
		return PRView{}, ErrInvalid
	}
	return pr, nil
}

// SetHeadOID updates stored PR head for drift tests.
func (f *FakeGitHub) SetHeadOID(owner, name string, number int, oid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fmt.Sprintf("%s/%s#%d", owner, name, number)
	pr := f.prs[k]
	pr.HeadOID = oid
	f.prs[k] = pr
}
