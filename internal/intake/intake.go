package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	SchemaSnapshot = "loopcoder.intake.snapshot.v1"
	SchemaDrift    = "loopcoder.intake.drift.v1"
)

// FailureClass is a typed pre-launch intake failure.
type FailureClass string

const (
	FailNone         FailureClass = ""
	FailWrongRepo    FailureClass = "wrong_repository"
	FailClosedIssue  FailureClass = "closed_or_invalid_issue"
	FailUnauthorized FailureClass = "insufficient_authorization"
	FailRateLimit    FailureClass = "rate_limit"
	FailTimeout      FailureClass = "timeout"
	FailAmbiguous    FailureClass = "ambiguous_issue"
	FailTransferred  FailureClass = "transferred_issue"
)

var (
	ErrInvalid      = errors.New("intake: invalid")
	ErrNotFound     = errors.New("intake: not found")
	ErrConflict     = errors.New("intake: conflict")
	ErrRateLimited  = errors.New("intake: rate limited")
	ErrUnauthorized = errors.New("intake: unauthorized")
	ErrTimeout      = errors.New("intake: timeout")
)

// IssueRef is the input identity for intake.
type IssueRef struct {
	RepoOwner string
	RepoName  string
	Number    int
}

// IssueSource is the normalized GitHub issue payload (redacted-safe fields).
type IssueSource struct {
	NodeID     string            `json:"node_id"`
	Number     int               `json:"number"`
	State      string            `json:"state"` // open|closed
	Title      string            `json:"title"`
	BodyDigest string            `json:"body_digest"` // never raw body in machine logs
	Labels     []string          `json:"labels"`
	Assignees  []string          `json:"assignees"`
	UpdatedAt  time.Time         `json:"updated_at"`
	URL        string            `json:"url"`
	RepoOwner  string            `json:"repo_owner"`
	RepoName   string            `json:"repo_name"`
	AuthOK     bool              `json:"authorization_ok"`
	Extra      map[string]string `json:"extra,omitempty"` // redacted only
}

// PolicySnapshot is the frozen effective local/repository policy digest.
type PolicySnapshot struct {
	Digest     string            `json:"digest"`
	BaseBranch string            `json:"base_branch"`
	Fields     map[string]string `json:"fields,omitempty"`
}

// WorkRequest is the immutable intake snapshot.
type WorkRequest struct {
	Schema         string         `json:"schema"`
	RequestID      string         `json:"request_id"`
	ProjectID      string         `json:"project_id"`
	Issue          IssueSource    `json:"issue"`
	SourceRevision string         `json:"source_revision"` // node+updated+body digest
	Policy         PolicySnapshot `json:"policy"`
	CreatedAt      time.Time      `json:"created_at"`
	// PrivateBody is project-scoped only; never in default status.
	PrivateBody string `json:"-"`
}

// DriftEvent records source/policy change after freeze.
type DriftEvent struct {
	Schema    string    `json:"schema"`
	RequestID string    `json:"request_id"`
	Kind      string    `json:"kind"` // issue_edit|policy_change|reopen|transfer
	Message   string    `json:"message"`
	At        time.Time `json:"at"`
	// Requires explicit continue/restart — never silent overwrite.
	RequiresAction string `json:"requires_action"` // continue|restart
}

// Result is intake outcome.
type Result struct {
	OK        bool         `json:"ok"`
	Failure   FailureClass `json:"failure,omitempty"`
	Message   string       `json:"message,omitempty"`
	Request   *WorkRequest `json:"request,omitempty"`
	Duplicate bool         `json:"duplicate,omitempty"`
	Drift     *DriftEvent  `json:"drift,omitempty"`
}

// GitHubClient is the fakeable GitHub surface for intake.
type GitHubClient interface {
	FetchIssue(ctx context.Context, ref IssueRef) (IssueSource, string, error) // source, private body, err
	RateLimited() bool
}

// PolicyProvider freezes policy digests.
type PolicyProvider interface {
	Snapshot(repoOwner, repoName, baseBranch string) (PolicySnapshot, error)
}

// Store is durable intake memory (in-process for core/tests).
type Store struct {
	mu     sync.Mutex
	byKey  map[string]*WorkRequest // project|node|sourceRev or active key
	active map[string]string       // project|node -> request_id
	byID   map[string]*WorkRequest
	drifts []DriftEvent
	now    func() time.Time
	seq    int64
}

// NewStore creates an empty store.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		byKey:  map[string]*WorkRequest{},
		active: map[string]string{},
		byID:   map[string]*WorkRequest{},
		now:    now,
	}
}

// IntakeOptions configure one intake attempt.
type IntakeOptions struct {
	ProjectID  string
	Ref        IssueRef
	BaseBranch string
	// ExpectedRepo must match fetched issue repo (wrong-repo rejection).
	ExpectedRepoOwner string
	ExpectedRepoName  string
}

// Service performs intake against a client and policy provider.
type Service struct {
	Store  *Store
	GitHub GitHubClient
	Policy PolicyProvider
	Now    func() time.Time
}

// Intake fetches, validates, and freezes a work request.
func (s *Service) Intake(ctx context.Context, opts IntakeOptions) (Result, error) {
	if opts.ProjectID == "" || opts.Ref.Number <= 0 {
		return Result{Failure: FailAmbiguous, Message: "missing project or issue number"}, ErrInvalid
	}
	if opts.ExpectedRepoOwner == "" {
		opts.ExpectedRepoOwner = opts.Ref.RepoOwner
	}
	if opts.ExpectedRepoName == "" {
		opts.ExpectedRepoName = opts.Ref.RepoName
	}
	if opts.BaseBranch == "" {
		opts.BaseBranch = "pre-prod"
	}
	now := s.now()
	if s.GitHub != nil && s.GitHub.RateLimited() {
		return Result{Failure: FailRateLimit, Message: "github rate limited"}, ErrRateLimited
	}
	if err := ctx.Err(); err != nil {
		return Result{Failure: FailTimeout, Message: err.Error()}, ErrTimeout
	}

	src, body, err := s.GitHub.FetchIssue(ctx, opts.Ref)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return Result{Failure: FailUnauthorized, Message: "authorization failed"}, err
		}
		if errors.Is(err, ErrRateLimited) {
			return Result{Failure: FailRateLimit, Message: "rate limited"}, err
		}
		if errors.Is(err, ErrTimeout) {
			return Result{Failure: FailTimeout, Message: "timeout"}, err
		}
		if errors.Is(err, ErrNotFound) {
			return Result{Failure: FailClosedIssue, Message: "issue not found"}, err
		}
		return Result{Failure: FailAmbiguous, Message: redact(err.Error())}, err
	}

	// Normalize digests
	if src.BodyDigest == "" {
		src.BodyDigest = digestBody(body)
	}
	src.Title = sanitizeTitle(src.Title)
	src.Labels = sanitizeList(src.Labels)
	src.Assignees = sanitizeList(src.Assignees)

	if !src.AuthOK {
		return Result{Failure: FailUnauthorized, Message: "insufficient authorization"}, ErrUnauthorized
	}
	if strings.EqualFold(src.State, "closed") {
		return Result{Failure: FailClosedIssue, Message: "issue closed"}, ErrInvalid
	}
	if src.Extra != nil {
		if _, ok := src.Extra["transferred"]; ok {
			return Result{Failure: FailTransferred, Message: "issue transferred"}, ErrInvalid
		}
	}
	if !strings.EqualFold(src.RepoOwner, opts.ExpectedRepoOwner) || !strings.EqualFold(src.RepoName, opts.ExpectedRepoName) {
		return Result{Failure: FailWrongRepo, Message: "issue repository mismatch"}, ErrInvalid
	}

	pol, err := s.Policy.Snapshot(src.RepoOwner, src.RepoName, opts.BaseBranch)
	if err != nil {
		return Result{Failure: FailAmbiguous, Message: "policy snapshot failed"}, err
	}
	if pol.Digest == "" {
		pol.Digest = digestPolicy(pol)
	}

	rev := sourceRevision(src)
	activeKey := opts.ProjectID + "|" + src.NodeID

	s.Store.mu.Lock()
	defer s.Store.mu.Unlock()

	if id, ok := s.Store.active[activeKey]; ok {
		prev := s.Store.byID[id]
		if prev != nil && prev.SourceRevision == rev && prev.Policy.Digest == pol.Digest {
			// idempotent retry
			cp := *prev
			return Result{OK: true, Request: &cp, Duplicate: true}, nil
		}
		if prev != nil {
			// drift — do not overwrite
			drift := DriftEvent{
				Schema: SchemaDrift, RequestID: prev.RequestID,
				Kind: "issue_edit", Message: "source or policy changed since freeze",
				At: now, RequiresAction: "restart",
			}
			if prev.SourceRevision == rev && prev.Policy.Digest != pol.Digest {
				drift.Kind = "policy_change"
			}
			s.Store.drifts = append(s.Store.drifts, drift)
			cp := *prev
			d := drift
			return Result{OK: false, Request: &cp, Drift: &d, Message: "active snapshot frozen; explicit restart required"}, ErrConflict
		}
	}

	s.Store.seq++
	id := fmt.Sprintf("wr_%d", s.Store.seq)
	wr := &WorkRequest{
		Schema: SchemaSnapshot, RequestID: id, ProjectID: opts.ProjectID,
		Issue: src, SourceRevision: rev, Policy: pol, CreatedAt: now,
		PrivateBody: body,
	}
	s.Store.byID[id] = wr
	s.Store.active[activeKey] = id
	s.Store.byKey[activeKey+"|"+rev] = wr
	cp := *wr
	return Result{OK: true, Request: &cp}, nil
}

// Get returns a frozen request by id (private body retained for project scope).
func (s *Store) Get(requestID string) (WorkRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wr, ok := s.byID[requestID]
	if !ok {
		return WorkRequest{}, ErrNotFound
	}
	return *wr, nil
}

// PublicView strips private body for default status.
func PublicView(wr WorkRequest) WorkRequest {
	wr.PrivateBody = ""
	return wr
}

// Drifts returns recorded drift events.
func (s *Store) Drifts() []DriftEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DriftEvent, len(s.drifts))
	copy(out, s.drifts)
	return out
}

// ExplicitRestart replaces active snapshot after drift acknowledgment.
func (s *Service) ExplicitRestart(ctx context.Context, opts IntakeOptions) (Result, error) {
	// clear active then intake fresh
	if opts.ProjectID == "" {
		return Result{Failure: FailAmbiguous}, ErrInvalid
	}
	// Fetch first to know node id
	src, _, err := s.GitHub.FetchIssue(ctx, opts.Ref)
	if err != nil {
		return Result{Failure: FailAmbiguous, Message: redact(err.Error())}, err
	}
	activeKey := opts.ProjectID + "|" + src.NodeID
	s.Store.mu.Lock()
	delete(s.Store.active, activeKey)
	s.Store.mu.Unlock()
	return s.Intake(ctx, opts)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	if s.Store != nil && s.Store.now != nil {
		return s.Store.now().UTC()
	}
	return time.Now().UTC()
}

// --- fakes for tests ---

// FakeGitHub is an in-memory issue server.
type FakeGitHub struct {
	mu      sync.Mutex
	Issues  map[string]IssueSource // owner/name#n
	Bodies  map[string]string
	Limited bool
	Auth    bool // default true
	Delay   time.Duration
}

func NewFakeGitHub() *FakeGitHub {
	return &FakeGitHub{
		Issues: map[string]IssueSource{},
		Bodies: map[string]string{},
		Auth:   true,
	}
}

func (f *FakeGitHub) key(ref IssueRef) string {
	return fmt.Sprintf("%s/%s#%d", strings.ToLower(ref.RepoOwner), strings.ToLower(ref.RepoName), ref.Number)
}

func (f *FakeGitHub) Put(src IssueSource, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fmt.Sprintf("%s/%s#%d", strings.ToLower(src.RepoOwner), strings.ToLower(src.RepoName), src.Number)
	if src.BodyDigest == "" {
		src.BodyDigest = digestBody(body)
	}
	src.AuthOK = f.Auth
	f.Issues[k] = src
	f.Bodies[k] = body
}

func (f *FakeGitHub) RateLimited() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Limited
}

func (f *FakeGitHub) FetchIssue(ctx context.Context, ref IssueRef) (IssueSource, string, error) {
	if f.Delay > 0 {
		select {
		case <-ctx.Done():
			return IssueSource{}, "", ErrTimeout
		case <-time.After(f.Delay):
		}
	}
	if err := ctx.Err(); err != nil {
		return IssueSource{}, "", ErrTimeout
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Limited {
		return IssueSource{}, "", ErrRateLimited
	}
	if !f.Auth {
		return IssueSource{}, "", ErrUnauthorized
	}
	src, ok := f.Issues[f.key(ref)]
	if !ok {
		return IssueSource{}, "", ErrNotFound
	}
	body := f.Bodies[f.key(ref)]
	return src, body, nil
}

// StaticPolicy returns a fixed policy digest.
type StaticPolicy struct {
	Base  string
	Extra map[string]string
}

func (p StaticPolicy) Snapshot(owner, name, base string) (PolicySnapshot, error) {
	if base == "" {
		base = p.Base
	}
	ps := PolicySnapshot{BaseBranch: base, Fields: map[string]string{
		"repo": strings.ToLower(owner + "/" + name),
	}}
	for k, v := range p.Extra {
		ps.Fields[k] = v
	}
	ps.Digest = digestPolicy(ps)
	return ps, nil
}

func sourceRevision(src IssueSource) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|%s|%s", src.NodeID, src.Number, src.BodyDigest, src.UpdatedAt.UTC().Format(time.RFC3339Nano), src.State)
	return "rev_" + hex.EncodeToString(h.Sum(nil))[:16]
}

func digestBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

func digestPolicy(p PolicySnapshot) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s", p.BaseBranch)
	keys := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "|%s=%s", k, p.Fields[k])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:20]
}

func sanitizeTitle(t string) string {
	t = strings.TrimSpace(t)
	if len(t) > 240 {
		t = t[:240]
	}
	return redact(t)
}

func sanitizeList(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, redact(s))
	}
	sort.Strings(out)
	return out
}

func redact(s string) string {
	lower := strings.ToLower(s)
	for _, bad := range []string{"sk-", "ghp_", "password", "api_key", "bearer ", "-----begin"} {
		if strings.Contains(lower, bad) {
			return "[redacted]"
		}
	}
	return s
}

// ParseIssueRef parses "owner/name#123" or number with default repo.
func ParseIssueRef(spec, defaultOwner, defaultName string) (IssueRef, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return IssueRef{}, ErrInvalid
	}
	if strings.Contains(spec, "#") {
		parts := strings.SplitN(spec, "#", 2)
		repo := parts[0]
		num, err := strconv.Atoi(parts[1])
		if err != nil || num <= 0 {
			return IssueRef{}, ErrInvalid
		}
		rp := strings.Split(repo, "/")
		if len(rp) != 2 {
			return IssueRef{}, ErrInvalid
		}
		return IssueRef{RepoOwner: rp[0], RepoName: rp[1], Number: num}, nil
	}
	num, err := strconv.Atoi(spec)
	if err != nil || num <= 0 {
		return IssueRef{}, ErrInvalid
	}
	if defaultOwner == "" || defaultName == "" {
		return IssueRef{}, ErrInvalid
	}
	return IssueRef{RepoOwner: defaultOwner, RepoName: defaultName, Number: num}, nil
}
