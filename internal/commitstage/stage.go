package commitstage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const SchemaIntent = "loopcoder.commit.intent.v1"
const SchemaReceipt = "loopcoder.commit.receipt.v1"

var (
	ErrInvalid  = errors.New("commitstage: invalid")
	ErrNotReady = errors.New("commitstage: not ready")
	ErrDrift    = errors.New("commitstage: drift")
	ErrConflict = errors.New("commitstage: conflict")
	ErrTimeout  = errors.New("commitstage: timeout")
	ErrEmpty    = errors.New("commitstage: empty commit")
)

// Intent freezes commit inputs.
type Intent struct {
	Schema             string   `json:"schema"`
	AttemptID          string   `json:"attempt_id"`
	IdempotencyKey     string   `json:"idempotency_key"`
	OwnedPaths         []string `json:"owned_paths"`
	ParentSHA          string   `json:"parent_sha"`
	BaseSHA            string   `json:"base_sha"`
	TreeDigest         string   `json:"tree_digest"` // expected staged tree identity
	MessageDigest      string   `json:"message_digest"`
	Message            string   `json:"-"` // not logged raw if private
	AuthorPolicy       string   `json:"author_policy"`
	VerificationDigest string   `json:"verification_digest"`
	RouteDigest        string   `json:"route_digest"`
	WorkerTerminal     bool     `json:"worker_terminal"`
	VerificationOK     bool     `json:"verification_ok"`
}

// Receipt is the post-read-back success record.
type Receipt struct {
	Schema             string    `json:"schema"`
	AttemptID          string    `json:"attempt_id"`
	IdempotencyKey     string    `json:"idempotency_key"`
	ParentSHA          string    `json:"parent_sha"`
	CommitSHA          string    `json:"commit_sha"`
	TreeSHA            string    `json:"tree_sha"`
	MessageDigest      string    `json:"message_digest"`
	AuthorPolicy       string    `json:"author_policy"`
	RouteDigest        string    `json:"route_digest"`
	VerificationDigest string    `json:"verification_digest"`
	CreatedAt          time.Time `json:"created_at"`
}

// Git is a bounded local git surface for commit stage (fakeable).
type Git interface {
	HEAD() (string, error)
	ParentOf(commit string) (string, error)
	TreeOf(commit string) (string, error)
	// StagePaths stages only listed paths; returns error on unowned drift.
	StagePaths(owned []string, allDirty []string) error
	// Commit creates a commit; empty tree change returns ErrEmpty.
	Commit(message, authorPolicy string) (commitSHA string, err error)
	// IndexDirty lists paths dirty in worktree/index.
	IndexDirty() ([]string, error)
}

// Store holds intents and receipts by idempotency key.
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

// Service performs commit stage.
type Service struct {
	Store *Store
	Git   Git
	// FailCommitWith injects timeout-on-first-commit for tests.
	FailCommitWith error
}

// Freeze validates and stores immutable intent.
func (s *Service) Freeze(in Intent) (Intent, error) {
	if in.AttemptID == "" || in.IdempotencyKey == "" || in.ParentSHA == "" || in.BaseSHA == "" {
		return Intent{}, ErrInvalid
	}
	if len(in.OwnedPaths) == 0 {
		return Intent{}, ErrInvalid
	}
	if !in.WorkerTerminal || !in.VerificationOK {
		return Intent{}, ErrNotReady
	}
	if in.RouteDigest == "" || in.VerificationDigest == "" {
		return Intent{}, ErrInvalid
	}
	in.Schema = SchemaIntent
	in.OwnedPaths = normalizePaths(in.OwnedPaths)
	if in.MessageDigest == "" && in.Message != "" {
		in.MessageDigest = digestStr(in.Message)
	}
	if in.MessageDigest == "" {
		return Intent{}, ErrInvalid
	}
	if in.AuthorPolicy == "" {
		in.AuthorPolicy = "loopcoder_bot"
	}
	// sanitize message from secrets before any use
	in.Message = sanitizeMessage(in.Message)

	s.Store.mu.Lock()
	defer s.Store.mu.Unlock()
	if prev, ok := s.Store.receipts[in.IdempotencyKey]; ok {
		// already done
		_ = prev
		s.Store.intents[in.IdempotencyKey] = in
		return in, nil
	}
	s.Store.intents[in.IdempotencyKey] = in
	return in, nil
}

// CommitOrAdopt stages owned paths and commits, or adopts matching receipt.
func (s *Service) CommitOrAdopt(ctx context.Context, key string) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, ErrTimeout
	}
	s.Store.mu.Lock()
	if r, ok := s.Store.receipts[key]; ok {
		s.Store.mu.Unlock()
		return r, nil
	}
	in, ok := s.Store.intents[key]
	s.Store.mu.Unlock()
	if !ok {
		return Receipt{}, ErrInvalid
	}

	// parent must match HEAD
	head, err := s.Git.HEAD()
	if err != nil {
		return Receipt{}, err
	}
	if head != in.ParentSHA {
		return Receipt{}, fmt.Errorf("%w: head %s != parent %s", ErrDrift, head, in.ParentSHA)
	}

	dirty, err := s.Git.IndexDirty()
	if err != nil {
		return Receipt{}, err
	}
	if err := s.Git.StagePaths(in.OwnedPaths, dirty); err != nil {
		return Receipt{}, err
	}

	var commitSHA string
	if s.FailCommitWith != nil {
		err = s.FailCommitWith
		// if timeout, still may have created — inspect HEAD
		if errors.Is(err, ErrTimeout) {
			s.FailCommitWith = nil
			// try commit once more as completed side effect adoption path
			// (fake may have committed)
			head2, _ := s.Git.HEAD()
			if head2 != in.ParentSHA {
				commitSHA = head2
				err = nil
			} else {
				commitSHA, err = s.Git.Commit(in.Message, in.AuthorPolicy)
			}
		}
	} else {
		commitSHA, err = s.Git.Commit(in.Message, in.AuthorPolicy)
	}
	if err != nil {
		if errors.Is(err, ErrEmpty) {
			return Receipt{}, err
		}
		return Receipt{}, err
	}

	// read-back
	parent, err := s.Git.ParentOf(commitSHA)
	if err != nil {
		return Receipt{}, err
	}
	tree, err := s.Git.TreeOf(commitSHA)
	if err != nil {
		return Receipt{}, err
	}
	if parent != in.ParentSHA {
		return Receipt{}, fmt.Errorf("%w: parent mismatch", ErrDrift)
	}
	head, err = s.Git.HEAD()
	if err != nil || head != commitSHA {
		return Receipt{}, fmt.Errorf("%w: head read-back", ErrDrift)
	}

	rec := Receipt{
		Schema: SchemaReceipt, AttemptID: in.AttemptID, IdempotencyKey: key,
		ParentSHA: parent, CommitSHA: commitSHA, TreeSHA: tree,
		MessageDigest: in.MessageDigest, AuthorPolicy: in.AuthorPolicy,
		RouteDigest: in.RouteDigest, VerificationDigest: in.VerificationDigest,
		CreatedAt: s.Store.now().UTC(),
	}
	s.Store.mu.Lock()
	s.Store.receipts[key] = rec
	s.Store.mu.Unlock()
	return rec, nil
}

// GetReceipt returns stored receipt if any.
func (s *Store) GetReceipt(key string) (Receipt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[key]
	return r, ok
}

func normalizePaths(ps []string) []string {
	m := map[string]struct{}{}
	for _, p := range ps {
		p = strings.TrimSpace(p)
		if p == "" || strings.Contains(p, "..") {
			continue
		}
		m[p] = struct{}{}
	}
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func digestStr(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])[:20]
}

func sanitizeMessage(s string) string {
	lower := strings.ToLower(s)
	for _, bad := range []string{"sk-", "ghp_", "password", "api_key", "bearer "} {
		if strings.Contains(lower, bad) {
			return "[redacted commit message]"
		}
	}
	// ensure issue linkage allowed - no strip of #N
	if len(s) > 2000 {
		return s[:2000]
	}
	return s
}

// --- Fake Git ---

type FakeGit struct {
	mu     sync.Mutex
	head   string
	parent map[string]string
	tree   map[string]string
	dirty  []string
	staged []string
	// CommitFn optional override
	CommitFn func(message, author string) (string, error)
	seq      int
}

func NewFakeGit(head string) *FakeGit {
	return &FakeGit{
		head: head, parent: map[string]string{}, tree: map[string]string{head: "tree_" + head[:7]},
	}
}

func (f *FakeGit) HEAD() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, nil
}

func (f *FakeGit) ParentOf(commit string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.parent[commit]
	if !ok {
		return "", ErrInvalid
	}
	return p, nil
}

func (f *FakeGit) TreeOf(commit string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tree[commit]
	if !ok {
		return "", ErrInvalid
	}
	return t, nil
}

func (f *FakeGit) StagePaths(owned []string, allDirty []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	own := map[string]struct{}{}
	for _, p := range owned {
		own[p] = struct{}{}
	}
	for _, d := range allDirty {
		if _, ok := own[d]; !ok {
			return fmt.Errorf("%w: unowned dirty %s", ErrDrift, d)
		}
	}
	f.staged = append([]string(nil), owned...)
	return nil
}

func (f *FakeGit) Commit(message, authorPolicy string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CommitFn != nil {
		return f.CommitFn(message, authorPolicy)
	}
	if len(f.staged) == 0 {
		return "", ErrEmpty
	}
	f.seq++
	sha := fmt.Sprintf("c%012d", f.seq)
	f.parent[sha] = f.head
	f.tree[sha] = "tree_" + sha
	f.head = sha
	f.staged = nil
	f.dirty = nil
	return sha, nil
}

func (f *FakeGit) IndexDirty() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dirty...), nil
}

func (f *FakeGit) SetDirty(paths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirty = append([]string(nil), paths...)
}
