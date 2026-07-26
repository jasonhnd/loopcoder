package wtclaim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const SchemaClaim = "loopcoder.wtclaim.v1"

var (
	ErrInvalid     = errors.New("wtclaim: invalid")
	ErrConflict    = errors.New("wtclaim: conflict")
	ErrNotFound    = errors.New("wtclaim: not found")
	ErrTimeout     = errors.New("wtclaim: timeout")
	ErrDirty       = errors.New("wtclaim: dirty state")
	ErrNotOwned    = errors.New("wtclaim: not owned")
	ErrBaseChanged = errors.New("wtclaim: base changed")
)

// FailureClass is typed claim failure.
type FailureClass string

const (
	FailConflict    FailureClass = "conflict_owner"
	FailDirty       FailureClass = "dirty_state"
	FailMoved       FailureClass = "worktree_moved_or_deleted"
	FailBaseChanged FailureClass = "base_changed"
	FailUnrelated   FailureClass = "unrelated_branch"
	FailTimeout     FailureClass = "timeout"
)

// Intent is the deterministic claim request.
type Intent struct {
	ProjectID   string
	RunID       string
	AttemptID   string
	BranchName  string
	BaseSHA     string
	OwnerID     string // claim generation owner
	RuntimeRoot string // project runtime storage (outside customer checkout)
}

// Claim is durable ownership evidence.
type Claim struct {
	Schema       string    `json:"schema"`
	ClaimID      string    `json:"claim_id"`
	Generation   int64     `json:"generation"`
	ProjectID    string    `json:"project_id"`
	RunID        string    `json:"run_id"`
	AttemptID    string    `json:"attempt_id"`
	BranchName   string    `json:"branch_name"`
	BaseSHA      string    `json:"base_sha"`
	WorktreePath string    `json:"worktree_path"`
	OwnerID      string    `json:"owner_id"`
	CreatedAt    time.Time `json:"created_at"`
	VerifiedAt   time.Time `json:"verified_at"`
}

// Result of Claim/Reuse.
type Result struct {
	OK      bool         `json:"ok"`
	Reused  bool         `json:"reused"`
	Claim   *Claim       `json:"claim,omitempty"`
	Failure FailureClass `json:"failure,omitempty"`
	Message string       `json:"message,omitempty"`
}

// GitBackend is a scrubbed, bounded git surface (fakeable).
type GitBackend interface {
	// BranchExists reports if branch exists (anywhere).
	BranchExists(branch string) bool
	// WorktreePath returns path if worktree for branch exists.
	WorktreePath(branch string) (string, bool)
	// CreateWorktree creates branch+worktree at baseSHA under path.
	CreateWorktree(branch, path, baseSHA string) error
	// VerifyWorktree checks path exists, branch matches, base SHA match, clean.
	VerifyWorktree(path, branch, baseSHA string) error
	// IsDirty reports dirty state at path.
	IsDirty(path string) bool
	// OwnerMeta returns stored owner generation for path if any.
	OwnerMeta(path string) (owner string, gen int64, ok bool)
	// SetOwnerMeta stamps ownership after verify.
	SetOwnerMeta(path, owner string, gen int64) error
	// RemoveWorktree removes only if owned and clean.
	RemoveWorktree(path string) error
}

// Store is in-memory claim ledger.
type Store struct {
	mu    sync.Mutex
	byRun map[string]*Claim // runID -> claim
	byID  map[string]*Claim
	seq   int64
	now   func() time.Time
}

func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{byRun: map[string]*Claim{}, byID: map[string]*Claim{}, now: now}
}

// Service performs claims.
type Service struct {
	Store *Store
	Git   GitBackend
	Now   func() time.Time
}

// ClaimOrReuse creates or reuses an idempotent claim.
func (s *Service) ClaimOrReuse(ctx context.Context, in Intent) (Result, error) {
	if err := validateIntent(in); err != nil {
		return Result{Failure: FailConflict, Message: err.Error()}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{Failure: FailTimeout, Message: "context done"}, ErrTimeout
	}
	now := s.now()

	s.Store.mu.Lock()
	if prev, ok := s.Store.byRun[in.RunID]; ok {
		s.Store.mu.Unlock()
		// inspect actual git state before reuse
		if err := s.Git.VerifyWorktree(prev.WorktreePath, prev.BranchName, prev.BaseSHA); err != nil {
			if errors.Is(err, ErrDirty) {
				return Result{Failure: FailDirty, Message: "existing claim dirty"}, err
			}
			if errors.Is(err, ErrBaseChanged) {
				return Result{Failure: FailBaseChanged, Message: "base sha changed"}, err
			}
			return Result{Failure: FailMoved, Message: "worktree missing or moved"}, err
		}
		owner, gen, ok := s.Git.OwnerMeta(prev.WorktreePath)
		if !ok || owner != prev.OwnerID || gen != prev.Generation {
			return Result{Failure: FailConflict, Message: "owner mismatch on reuse"}, ErrConflict
		}
		if prev.BranchName != in.BranchName || prev.BaseSHA != in.BaseSHA {
			return Result{Failure: FailUnrelated, Message: "intent differs from existing claim"}, ErrConflict
		}
		cp := *prev
		cp.VerifiedAt = now
		return Result{OK: true, Reused: true, Claim: &cp}, nil
	}
	s.Store.mu.Unlock()

	// Conflict: unrelated existing branch without our claim
	if s.Git.BranchExists(in.BranchName) {
		if path, ok := s.Git.WorktreePath(in.BranchName); ok {
			owner, _, has := s.Git.OwnerMeta(path)
			if !has || owner != in.OwnerID {
				return Result{Failure: FailUnrelated, Message: "unrelated branch/worktree exists"}, ErrConflict
			}
		} else {
			return Result{Failure: FailUnrelated, Message: "branch exists without owned worktree"}, ErrConflict
		}
	}

	path := filepath.Join(in.RuntimeRoot, "worktrees", sanitize(in.RunID))
	// Simulate timeout path: if context already deadline exceeded mid-op, inspect
	if err := s.Git.CreateWorktree(in.BranchName, path, in.BaseSHA); err != nil {
		if errors.Is(err, ErrTimeout) {
			// inspect whether side effect completed
			if e2 := s.Git.VerifyWorktree(path, in.BranchName, in.BaseSHA); e2 == nil {
				// adopt
				return s.persistNew(in, path, now)
			}
			return Result{Failure: FailTimeout, Message: "git timeout; incomplete"}, ErrTimeout
		}
		return Result{Failure: FailConflict, Message: err.Error()}, err
	}
	if err := s.Git.VerifyWorktree(path, in.BranchName, in.BaseSHA); err != nil {
		return Result{Failure: FailConflict, Message: err.Error()}, err
	}
	return s.persistNew(in, path, now)
}

func (s *Service) persistNew(in Intent, path string, now time.Time) (Result, error) {
	s.Store.mu.Lock()
	defer s.Store.mu.Unlock()
	s.Store.seq++
	gen := s.Store.seq
	id := fmt.Sprintf("claim_%d", gen)
	if err := s.Git.SetOwnerMeta(path, in.OwnerID, gen); err != nil {
		return Result{Failure: FailConflict, Message: err.Error()}, err
	}
	c := &Claim{
		Schema: SchemaClaim, ClaimID: id, Generation: gen,
		ProjectID: in.ProjectID, RunID: in.RunID, AttemptID: in.AttemptID,
		BranchName: in.BranchName, BaseSHA: in.BaseSHA, WorktreePath: path,
		OwnerID: in.OwnerID, CreatedAt: now, VerifiedAt: now,
	}
	s.Store.byRun[in.RunID] = c
	s.Store.byID[id] = c
	cp := *c
	return Result{OK: true, Reused: false, Claim: &cp}, nil
}

// CleanupOwned removes only an exactly owned clean worktree.
func (s *Service) CleanupOwned(claimID string) error {
	s.Store.mu.Lock()
	c, ok := s.Store.byID[claimID]
	s.Store.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	owner, gen, ok := s.Git.OwnerMeta(c.WorktreePath)
	if !ok || owner != c.OwnerID || gen != c.Generation {
		return ErrNotOwned
	}
	if s.Git.IsDirty(c.WorktreePath) {
		return ErrDirty
	}
	return s.Git.RemoveWorktree(c.WorktreePath)
}

// ScrubEnv removes dangerous git env redirects (documentation helper for callers).
func ScrubEnv(env []string) []string {
	block := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
		"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_COMMON_DIR": true, "GIT_DIR_CEILING_DIR": true,
		"GIT_TEMPLATE_DIR": true, "GIT_CONFIG": true, "GIT_CONFIG_GLOBAL": true,
		"GIT_CONFIG_SYSTEM": true, "GIT_EXEC_PATH": true, "GIT_PAGER": true,
		"PAGER": true,
	}
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if block[k] || strings.HasPrefix(k, "GIT_TRACE") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return s.Store.now().UTC()
}

func validateIntent(in Intent) error {
	if in.ProjectID == "" || in.RunID == "" || in.BranchName == "" || in.BaseSHA == "" || in.OwnerID == "" || in.RuntimeRoot == "" {
		return fmt.Errorf("%w: incomplete intent", ErrInvalid)
	}
	if strings.Contains(in.BranchName, "..") || strings.HasPrefix(in.BranchName, "/") {
		return fmt.Errorf("%w: branch", ErrInvalid)
	}
	if len(in.BaseSHA) < 7 {
		return fmt.Errorf("%w: base sha", ErrInvalid)
	}
	return nil
}

func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
	if s == "" {
		sum := sha256.Sum256([]byte("x"))
		return hex.EncodeToString(sum[:4])
	}
	return s
}

// --- Fake backend for tests ---

type wtMeta struct {
	branch string
	base   string
	dirty  bool
	owner  string
	gen    int64
	exists bool
}

// FakeGit is an in-memory worktree/branch map.
type FakeGit struct {
	mu       sync.Mutex
	branches map[string]string // branch -> path
	trees    map[string]*wtMeta
	// FailCreateOnce forces first CreateWorktree to timeout then succeed on adopt path
	FailCreateWith error
}

func NewFakeGit() *FakeGit {
	return &FakeGit{branches: map[string]string{}, trees: map[string]*wtMeta{}}
}

func (f *FakeGit) BranchExists(branch string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.branches[branch]
	return ok
}

func (f *FakeGit) WorktreePath(branch string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.branches[branch]
	return p, ok
}

func (f *FakeGit) CreateWorktree(branch, path, baseSHA string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailCreateWith != nil {
		err := f.FailCreateWith
		// if timeout, still create so adopt can succeed (completed side effect)
		if errors.Is(err, ErrTimeout) {
			f.branches[branch] = path
			f.trees[path] = &wtMeta{branch: branch, base: baseSHA, exists: true}
			f.FailCreateWith = nil
			return ErrTimeout
		}
		return err
	}
	if _, ok := f.branches[branch]; ok {
		return ErrConflict
	}
	f.branches[branch] = path
	f.trees[path] = &wtMeta{branch: branch, base: baseSHA, exists: true}
	return nil
}

func (f *FakeGit) VerifyWorktree(path, branch, baseSHA string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.trees[path]
	if !ok || !m.exists {
		return fmt.Errorf("%w: missing", ErrNotFound)
	}
	if m.branch != branch {
		return ErrConflict
	}
	if m.base != baseSHA {
		return ErrBaseChanged
	}
	if m.dirty {
		return ErrDirty
	}
	return nil
}

func (f *FakeGit) IsDirty(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.trees[path]
	return m != nil && m.dirty
}

func (f *FakeGit) OwnerMeta(path string) (string, int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.trees[path]
	if !ok || m.owner == "" {
		return "", 0, false
	}
	return m.owner, m.gen, true
}

func (f *FakeGit) SetOwnerMeta(path, owner string, gen int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.trees[path]
	if !ok {
		return ErrNotFound
	}
	m.owner = owner
	m.gen = gen
	return nil
}

func (f *FakeGit) RemoveWorktree(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.trees[path]
	if !ok {
		return ErrNotFound
	}
	if m.dirty {
		return ErrDirty
	}
	delete(f.branches, m.branch)
	delete(f.trees, path)
	return nil
}

func (f *FakeGit) SetDirty(path string, dirty bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.trees[path]; m != nil {
		m.dirty = dirty
	}
}

func (f *FakeGit) DeletePath(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.trees[path]; m != nil {
		delete(f.branches, m.branch)
		m.exists = false
	}
}
