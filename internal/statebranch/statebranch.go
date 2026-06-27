// Package statebranch publishes loopcoder run state to a dedicated git branch.
package statebranch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/gitutil"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/state"
)

const (
	DefaultBranch     = "loopcoder/state"
	DefaultRemote     = "origin"
	DefaultTTLSeconds = 600

	logTailLines = 50
)

var logPathLinePattern = regexp.MustCompile(`(?m)^\s*-\s*Log path:\s*(.+?)\s*$`)

// Deps is the injectable surface for deterministic tests.
type Deps struct {
	Git          gitutil.Runner
	Now          func() time.Time
	Hostname     func() (string, error)
	PID          func() int
	RandomSuffix func() (string, error)
	MkdirTemp    func(dir, pattern string) (string, error)
	RemoveAll    func(path string) error
}

// PushOptions selects the local run and target state branch.
type PushOptions struct {
	RepoPath string
	RunID    string
	Branch   string
	Remote   string
}

// PullOptions selects the state branch to mirror locally.
type PullOptions struct {
	RepoPath string
	Branch   string
	Remote   string
}

// LeaseOptions selects the run lease to acquire, renew, or release.
type LeaseOptions struct {
	RepoPath string
	RunID    string
	Branch   string
	Remote   string
	TTL      time.Duration
}

// PushResult summarizes a state branch publish.
type PushResult struct {
	RepoPath    string
	RunID       string
	Branch      string
	Remote      string
	Committed   bool
	Pushed      bool
	PushError   string
	Files       []string
	WorktreeDir string
}

// PullResult summarizes a state branch fetch into the local mirror.
type PullResult struct {
	RepoPath   string
	Branch     string
	Remote     string
	MirrorPath string
	Runs       []string
}

// Lease is the durable conductor lease stored at lease.json.
type Lease struct {
	Version        int    `json:"version"`
	RunID          string `json:"run_id,omitempty"`
	LeaseID        string `json:"lease_id"`
	Host           string `json:"host"`
	PID            int    `json:"pid"`
	StartedAt      string `json:"started_at"`
	LeaseExpiresAt string `json:"lease_expires_at"`
}

// LeaseResult summarizes an acquire/renew/release attempt.
type LeaseResult struct {
	RepoPath      string
	RunID         string
	Branch        string
	Remote        string
	Status        string
	ObserveOnly   bool
	Lease         *Lease
	PreviousLease *Lease
	Pushed        bool
	PushError     string
	Message       string
}

type snapshot struct {
	Version    int             `json:"version"`
	RunID      string          `json:"run_id"`
	UpdatedAt  string          `json:"updated_at"`
	EventCount int             `json:"event_count"`
	Attempts   []state.Attempt `json:"attempts,omitempty"`
}

type logManifestEntry struct {
	SourcePath string `json:"source_path"`
	Bytes      int64  `json:"bytes,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	TailPath   string `json:"tail_path,omitempty"`
	Error      string `json:"error,omitempty"`
}

type leaseEvent struct {
	Timestamp        string `json:"ts"`
	RunID            string `json:"run_id"`
	Event            string `json:"event"`
	Status           string `json:"status"`
	LeaseID          string `json:"lease_id,omitempty"`
	PreviousLeaseID  string `json:"previous_lease_id,omitempty"`
	LeaseExpiresAt   string `json:"lease_expires_at,omitempty"`
	Message          string `json:"message,omitempty"`
	BestEffortNotice string `json:"best_effort_notice,omitempty"`
}

// DefaultDeps returns the real filesystem/process dependencies.
func DefaultDeps() Deps {
	return Deps{
		Git:      gitutil.ExecRunner{},
		Now:      time.Now,
		Hostname: os.Hostname,
		PID:      os.Getpid,
		RandomSuffix: func() (string, error) {
			var b [8]byte
			if _, err := rand.Read(b[:]); err != nil {
				return "", err
			}
			return hex.EncodeToString(b[:]), nil
		},
		MkdirTemp: os.MkdirTemp,
		RemoveAll: os.RemoveAll,
	}
}

// Push publishes .loopcoder/runs/<runID> to runs/<runID> on the state branch.
func Push(ctx context.Context, opts PushOptions, deps Deps) (PushResult, error) {
	deps = withDefaults(deps)
	repoPath, branch, remote, err := normalizeCommon(opts.RepoPath, opts.Branch, opts.Remote)
	if err != nil {
		return PushResult{}, err
	}
	runID := strings.TrimSpace(opts.RunID)
	if runID == "" {
		return PushResult{}, errors.New("run id is required")
	}

	worktreePath, cleanup, err := prepareWritableWorktree(ctx, repoPath, branch, remote, deps)
	if err != nil {
		return PushResult{}, err
	}
	defer cleanup()

	result := PushResult{
		RepoPath:    repoPath,
		RunID:       runID,
		Branch:      branch,
		Remote:      remote,
		WorktreeDir: worktreePath,
	}

	priorEvents := readLines(filepath.Join(worktreePath, "runs", runID, "events.jsonl"))
	files, err := materializeRunState(repoPath, runID, worktreePath, deps.Now().UTC(), priorEvents)
	if err != nil {
		return result, err
	}
	result.Files = files

	committed, err := commitIfChanged(ctx, worktreePath, fmt.Sprintf("Update loopcoder state for %s", runID), deps)
	if err != nil {
		return result, err
	}
	result.Committed = committed

	if err := pushBranch(ctx, worktreePath, remote, branch, deps); err != nil {
		result.PushError = err.Error()
		return result, nil
	}
	result.Pushed = true
	return result, nil
}

// Pull fetches the state branch into .loopcoder/state-branch and hydrates
// missing .loopcoder/runs entries for resume.
func Pull(ctx context.Context, opts PullOptions, deps Deps) (PullResult, error) {
	deps = withDefaults(deps)
	repoPath, branch, remote, err := normalizeCommon(opts.RepoPath, opts.Branch, opts.Remote)
	if err != nil {
		return PullResult{}, err
	}

	worktreePath, cleanup, err := prepareReadOnlyWorktree(ctx, repoPath, branch, remote, deps)
	if err != nil {
		return PullResult{}, err
	}
	defer cleanup()

	mirrorPath := filepath.Join(repoPath, ".loopcoder", "state-branch", sanitizePathPart(branch))
	if err := makeTreeWritable(mirrorPath); err != nil {
		return PullResult{}, err
	}
	if err := safeRemoveAll(mirrorPath, repoPath); err != nil {
		return PullResult{}, err
	}
	if err := copyTree(worktreePath, mirrorPath, copyTreeOptions{skipGit: true, overwrite: true}); err != nil {
		return PullResult{}, err
	}
	if err := makeTreeReadOnly(mirrorPath); err != nil {
		return PullResult{}, err
	}

	runs, err := hydrateMissingRuns(filepath.Join(worktreePath, "runs"), state.RunsRoot(repoPath))
	if err != nil {
		return PullResult{}, err
	}
	return PullResult{
		RepoPath:   repoPath,
		Branch:     branch,
		Remote:     remote,
		MirrorPath: mirrorPath,
		Runs:       runs,
	}, nil
}

// Acquire acquires or renews the conductor lease. A valid foreign lease returns
// observe-only without writing.
func Acquire(ctx context.Context, opts LeaseOptions, deps Deps) (LeaseResult, error) {
	deps = withDefaults(deps)
	repoPath, branch, remote, ttl, err := normalizeLease(opts)
	if err != nil {
		return LeaseResult{}, err
	}
	runID := strings.TrimSpace(opts.RunID)
	if runID == "" {
		return LeaseResult{}, errors.New("run id is required")
	}

	worktreePath, cleanup, err := prepareWritableWorktree(ctx, repoPath, branch, remote, deps)
	if err != nil {
		return LeaseResult{}, err
	}
	defer cleanup()

	now := deps.Now().UTC()
	host, err := deps.Hostname()
	if err != nil {
		return LeaseResult{}, fmt.Errorf("resolve hostname: %w", err)
	}
	pid := deps.PID()
	current, err := readLease(worktreePath)
	if err != nil {
		return LeaseResult{}, err
	}

	result := LeaseResult{
		RepoPath:      repoPath,
		RunID:         runID,
		Branch:        branch,
		Remote:        remote,
		PreviousLease: cloneLease(current),
	}

	if current != nil && !leaseExpired(*current, now) && !sameConductor(*current, host, pid, runID) {
		result.Status = "observe-only"
		result.ObserveOnly = true
		result.Lease = cloneLease(current)
		result.Message = "observe only: another conductor holds a valid lease"
		return result, nil
	}

	next := Lease{
		Version:        1,
		RunID:          runID,
		Host:           host,
		PID:            pid,
		StartedAt:      state.FormatTimestamp(now),
		LeaseExpiresAt: state.FormatTimestamp(now.Add(ttl)),
	}
	status := "acquired"
	message := "acquired conductor lease"
	if current != nil && sameConductor(*current, host, pid, runID) && !leaseExpired(*current, now) {
		next.LeaseID = current.LeaseID
		next.StartedAt = current.StartedAt
		status = "renewed"
		message = "renewed conductor lease"
	} else {
		suffix, err := deps.RandomSuffix()
		if err != nil {
			return result, fmt.Errorf("create lease id: %w", err)
		}
		next.LeaseID = fmt.Sprintf("%s-%d-%s", sanitizeLeasePart(host), pid, suffix)
		if current != nil {
			status = "taken-over"
			message = "took over expired conductor lease"
		}
	}

	if err := writeLease(worktreePath, next); err != nil {
		return result, err
	}
	if err := appendLeaseEvent(worktreePath, runID, leaseEvent{
		Timestamp:        state.FormatTimestamp(now),
		RunID:            runID,
		Event:            "lease.acquire",
		Status:           status,
		LeaseID:          next.LeaseID,
		PreviousLeaseID:  leaseID(current),
		LeaseExpiresAt:   next.LeaseExpiresAt,
		Message:          message,
		BestEffortNotice: "git push compare-and-swap plus TTL; not a perfect distributed lock",
	}); err != nil {
		return result, err
	}

	if _, err := commitIfChanged(ctx, worktreePath, fmt.Sprintf("Update loopcoder lease for %s", runID), deps); err != nil {
		return result, err
	}
	result.Status = status
	result.Lease = &next
	result.Message = message
	if err := pushBranch(ctx, worktreePath, remote, branch, deps); err != nil {
		result.PushError = err.Error()
		return handleAcquirePushConflict(ctx, result, host, pid, now, deps)
	}
	result.Pushed = true
	return result, nil
}

// Release clears lease.json for the selected run and pushes the update.
func Release(ctx context.Context, opts LeaseOptions, deps Deps) (LeaseResult, error) {
	deps = withDefaults(deps)
	repoPath, branch, remote, _, err := normalizeLease(opts)
	if err != nil {
		return LeaseResult{}, err
	}
	runID := strings.TrimSpace(opts.RunID)
	if runID == "" {
		return LeaseResult{}, errors.New("run id is required")
	}

	worktreePath, cleanup, err := prepareWritableWorktree(ctx, repoPath, branch, remote, deps)
	if err != nil {
		return LeaseResult{}, err
	}
	defer cleanup()

	current, err := readLease(worktreePath)
	if err != nil {
		return LeaseResult{}, err
	}
	result := LeaseResult{
		RepoPath:      repoPath,
		RunID:         runID,
		Branch:        branch,
		Remote:        remote,
		PreviousLease: cloneLease(current),
	}
	if current == nil {
		result.Status = "none"
		result.Message = "no conductor lease exists"
		return result, nil
	}
	if current.RunID != "" && current.RunID != runID {
		result.Status = "observe-only"
		result.ObserveOnly = true
		result.Lease = cloneLease(current)
		result.Message = "observe only: lease belongs to another run"
		return result, nil
	}

	leasePath := filepath.Join(worktreePath, "lease.json")
	if err := os.Remove(leasePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("remove lease: %w", err)
	}
	if err := appendLeaseEvent(worktreePath, runID, leaseEvent{
		Timestamp:       state.FormatTimestamp(deps.Now().UTC()),
		RunID:           runID,
		Event:           "lease.release",
		Status:          "released",
		PreviousLeaseID: current.LeaseID,
		Message:         "released conductor lease",
	}); err != nil {
		return result, err
	}
	if _, err := commitIfChanged(ctx, worktreePath, fmt.Sprintf("Release loopcoder lease for %s", runID), deps); err != nil {
		return result, err
	}
	result.Status = "released"
	result.Message = "released conductor lease"
	if err := pushBranch(ctx, worktreePath, remote, branch, deps); err != nil {
		result.PushError = err.Error()
		result.Status = "cas-conflict"
		result.Message = "lease release push was rejected; re-read the state branch before acting"
		if remoteLease, readErr := readRemoteLease(ctx, repoPath, branch, remote, deps); readErr == nil {
			result.Lease = remoteLease
		}
		return result, nil
	}
	result.Pushed = true
	return result, nil
}

func withDefaults(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Git == nil {
		deps.Git = defaults.Git
	}
	if deps.Now == nil {
		deps.Now = defaults.Now
	}
	if deps.Hostname == nil {
		deps.Hostname = defaults.Hostname
	}
	if deps.PID == nil {
		deps.PID = defaults.PID
	}
	if deps.RandomSuffix == nil {
		deps.RandomSuffix = defaults.RandomSuffix
	}
	if deps.MkdirTemp == nil {
		deps.MkdirTemp = defaults.MkdirTemp
	}
	if deps.RemoveAll == nil {
		deps.RemoveAll = defaults.RemoveAll
	}
	return deps
}

func normalizeCommon(repoPath, branch, remote string) (string, string, string, error) {
	repo, err := resolveRepo(repoPath)
	if err != nil {
		return "", "", "", err
	}
	branch, err = normalizeBranch(branch)
	if err != nil {
		return "", "", "", err
	}
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = DefaultRemote
	}
	return repo, branch, remote, nil
}

func normalizeLease(opts LeaseOptions) (string, string, string, time.Duration, error) {
	repo, branch, remote, err := normalizeCommon(opts.RepoPath, opts.Branch, opts.Remote)
	if err != nil {
		return "", "", "", 0, err
	}
	ttl := opts.TTL
	if ttl == 0 {
		ttl = DefaultTTLSeconds * time.Second
	}
	if ttl < 0 {
		return "", "", "", 0, errors.New("ttl must be non-negative")
	}
	return repo, branch, remote, ttl, nil
}

func resolveRepo(repoPath string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		return "", errors.New("repo path is required")
	}
	absolute, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repo path is not a directory: %s", absolute)
	}
	return absolute, nil
}

func normalizeBranch(branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch = DefaultBranch
	}
	if branch == "main" || branch == "master" || branch == "trunk" {
		return "", fmt.Errorf("state branch must not be %q", branch)
	}
	if strings.HasPrefix(branch, "loop/issue-") || strings.HasPrefix(branch, "feature/") {
		return "", fmt.Errorf("state branch must not be a feature branch: %s", branch)
	}
	if strings.Contains(branch, "..") || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return "", fmt.Errorf("invalid state branch %q", branch)
	}
	if strings.ContainsAny(branch, " \t\r\n") {
		return "", fmt.Errorf("invalid state branch %q", branch)
	}
	return branch, nil
}

func prepareWritableWorktree(ctx context.Context, repoPath, branch, remote string, deps Deps) (string, func(), error) {
	scratch, err := deps.MkdirTemp("", "loopcoder-state-*")
	if err != nil {
		return "", nil, fmt.Errorf("create state scratch directory: %w", err)
	}
	worktreePath := filepath.Join(scratch, "wt")
	cleanup := func() {
		_, _ = runGit(ctx, deps, repoPath, "worktree", "remove", "--force", worktreePath)
		_ = deps.RemoveAll(scratch)
	}

	remoteRef := fmt.Sprintf("refs/remotes/%s/%s", remote, branch)
	fetched := fetchStateBranch(ctx, repoPath, branch, remote, deps) == nil

	if localBranchExists(ctx, repoPath, branch, deps) {
		if _, err := runGit(ctx, deps, repoPath, "worktree", "add", worktreePath, branch); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("git worktree add state branch: %w", err)
		}
		if fetched {
			if _, err := runGit(ctx, deps, worktreePath, "merge", "--ff-only", remoteRef); err != nil {
				cleanup()
				return "", nil, fmt.Errorf("fast-forward state branch: %w", err)
			}
		}
		return worktreePath, cleanup, nil
	}

	if fetched {
		if _, err := runGit(ctx, deps, repoPath, "worktree", "add", "-b", branch, worktreePath, remoteRef); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("git worktree add state branch: %w", err)
		}
		return worktreePath, cleanup, nil
	}

	if _, err := runGit(ctx, deps, repoPath, "worktree", "add", "--detach", worktreePath, "HEAD"); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git worktree add state branch seed: %w", err)
	}
	if _, err := runGit(ctx, deps, worktreePath, "checkout", "--orphan", branch); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git checkout orphan state branch: %w", err)
	}
	_, _ = runGit(ctx, deps, worktreePath, "rm", "-rf", ".")
	if err := clearWorktreeContents(worktreePath); err != nil {
		cleanup()
		return "", nil, err
	}
	return worktreePath, cleanup, nil
}

func prepareReadOnlyWorktree(ctx context.Context, repoPath, branch, remote string, deps Deps) (string, func(), error) {
	scratch, err := deps.MkdirTemp("", "loopcoder-state-read-*")
	if err != nil {
		return "", nil, fmt.Errorf("create state scratch directory: %w", err)
	}
	worktreePath := filepath.Join(scratch, "wt")
	cleanup := func() {
		_, _ = runGit(ctx, deps, repoPath, "worktree", "remove", "--force", worktreePath)
		_ = deps.RemoveAll(scratch)
	}

	remoteRef := fmt.Sprintf("refs/remotes/%s/%s", remote, branch)
	fetched := fetchStateBranch(ctx, repoPath, branch, remote, deps) == nil
	rev := remoteRef
	if !fetched {
		if !localBranchExists(ctx, repoPath, branch, deps) {
			cleanup()
			return "", nil, fmt.Errorf("state branch %q is not available locally or from %s", branch, remote)
		}
		rev = "refs/heads/" + branch
	}
	if _, err := runGit(ctx, deps, repoPath, "worktree", "add", "--detach", worktreePath, rev); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git worktree add state branch mirror: %w", err)
	}
	return worktreePath, cleanup, nil
}

func fetchStateBranch(ctx context.Context, repoPath, branch, remote string, deps Deps) error {
	refspec := fmt.Sprintf("refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)
	_, err := runGit(ctx, deps, repoPath, "fetch", "-q", remote, refspec)
	return err
}

func localBranchExists(ctx context.Context, repoPath, branch string, deps Deps) bool {
	_, err := runGit(ctx, deps, repoPath, "rev-parse", "--verify", "refs/heads/"+branch)
	return err == nil
}

func runGit(ctx context.Context, deps Deps, repoPath string, args ...string) ([]byte, error) {
	return deps.Git.RunGit(ctx, repoPath, args...)
}

func clearWorktreeContents(worktreePath string) error {
	entries, err := os.ReadDir(worktreePath)
	if err != nil {
		return fmt.Errorf("read state worktree: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(worktreePath, entry.Name())); err != nil {
			return fmt.Errorf("clear state worktree: %w", err)
		}
	}
	return nil
}

func materializeRunState(repoPath, runID, worktreePath string, now time.Time, priorEvents []string) ([]string, error) {
	sourceRun := state.RunPath(repoPath, runID)
	info, err := os.Stat(sourceRun)
	if err != nil {
		return nil, fmt.Errorf("read run state: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("run state path is not a directory: %s", sourceRun)
	}

	destRun := filepath.Join(worktreePath, "runs", runID)
	if err := safeRemoveAll(destRun, worktreePath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destRun, 0o755); err != nil {
		return nil, fmt.Errorf("create state branch run directory: %w", err)
	}

	written := map[string]bool{}
	write := func(rel string, data []byte) error {
		target := filepath.Join(destRun, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
		written[filepath.ToSlash(filepath.Join("runs", runID, rel))] = true
		return nil
	}

	stateJSONPath := filepath.Join(sourceRun, "state.json")
	if data, err := os.ReadFile(stateJSONPath); err == nil {
		if err := write("state.json", scrubContent("state.json", data)); err != nil {
			return nil, fmt.Errorf("write state snapshot: %w", err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		data, err := synthesizeStateSnapshot(repoPath, runID, now)
		if err != nil {
			return nil, err
		}
		if err := write("state.json", data); err != nil {
			return nil, fmt.Errorf("write synthesized state snapshot: %w", err)
		}
	} else {
		return nil, fmt.Errorf("read state snapshot: %w", err)
	}

	localEventLines := readLines(filepath.Join(sourceRun, "events.jsonl"))
	mergedEventLines := mergeEventLines(localEventLines, priorEvents)
	if len(mergedEventLines) > 0 {
		eventText := scrubContent("events.jsonl", []byte(strings.Join(mergedEventLines, "\n")+"\n"))
		if err := write("events.jsonl", eventText); err != nil {
			return nil, fmt.Errorf("write events: %w", err)
		}
	}

	if err := copyWorkers(sourceRun, destRun, runID, written); err != nil {
		return nil, err
	}
	if err := copyRecovery(sourceRun, destRun, runID, written); err != nil {
		return nil, err
	}
	if err := writeLogArtifacts(sourceRun, destRun, runID, written); err != nil {
		return nil, err
	}

	files := make([]string, 0, len(written))
	for file := range written {
		files = append(files, file)
	}
	sort.Strings(files)
	return files, nil
}

func synthesizeStateSnapshot(repoPath, runID string, now time.Time) ([]byte, error) {
	attempts, err := state.LoadAttempts(repoPath, runID)
	if err != nil {
		return nil, fmt.Errorf("load attempts: %w", err)
	}
	for i := range attempts {
		attempts[i].Path = ""
	}
	eventCount, err := state.CountEvents(repoPath, runID)
	if err != nil {
		return nil, fmt.Errorf("count events: %w", err)
	}
	data, err := json.MarshalIndent(snapshot{
		Version:    1,
		RunID:      runID,
		UpdatedAt:  state.FormatTimestamp(now),
		EventCount: eventCount,
		Attempts:   attempts,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal state snapshot: %w", err)
	}
	return scrubContent("state.json", append(data, '\n')), nil
}

func copyWorkers(sourceRun, destRun, runID string, written map[string]bool) error {
	sourceWorkers := filepath.Join(sourceRun, "workers")
	entries, err := os.ReadDir(sourceWorkers)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read workers directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".attempt.json") && !strings.HasSuffix(name, ".heartbeat.json") {
			continue
		}
		if err := copyScrubbedTextFile(
			filepath.Join(sourceWorkers, name),
			filepath.Join(destRun, "workers", name),
		); err != nil {
			return fmt.Errorf("copy worker state %s: %w", name, err)
		}
		written[filepath.ToSlash(filepath.Join("runs", runID, "workers", name))] = true
	}
	return nil
}

func copyRecovery(sourceRun, destRun, runID string, written map[string]bool) error {
	sourceRecovery := filepath.Join(sourceRun, "recovery")
	info, err := os.Stat(sourceRecovery)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read recovery directory: %w", err)
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(sourceRecovery, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(sourceRecovery, path)
		if err != nil {
			return err
		}
		if isRawLogFile(rel) {
			return nil
		}
		dest := filepath.Join(destRun, "recovery", rel)
		if err := copyScrubbedTextFile(path, dest); err != nil {
			return err
		}
		written[filepath.ToSlash(filepath.Join("runs", runID, "recovery", rel))] = true
		return nil
	})
}

func copyScrubbedTextFile(source, dest string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, scrubContent(source, data), 0o644)
}

func scrubContent(path string, data []byte) []byte {
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".json") {
		var value any
		if err := json.Unmarshal(data, &value); err == nil {
			scrubbed := scrubJSONValue(value)
			if formatted, err := json.MarshalIndent(scrubbed, "", "  "); err == nil {
				return append(formatted, '\n')
			}
		}
	}
	if strings.HasSuffix(lower, ".jsonl") {
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var value any
			if err := json.Unmarshal([]byte(line), &value); err == nil {
				if scrubbed, err := json.Marshal(scrubJSONValue(value)); err == nil {
					out = append(out, string(scrubbed))
					continue
				}
			}
			out = append(out, recovery.Scrub(line))
		}
		if len(out) == 0 {
			return nil
		}
		return []byte(strings.Join(out, "\n") + "\n")
	}
	return []byte(recovery.Scrub(string(data)))
}

func scrubJSONValue(value any) any {
	switch v := value.(type) {
	case string:
		return recovery.Scrub(v)
	case []any:
		for i := range v {
			v[i] = scrubJSONValue(v[i])
		}
		return v
	case map[string]any:
		for key, child := range v {
			v[key] = scrubJSONValue(child)
		}
		return v
	default:
		return value
	}
}

func writeLogArtifacts(sourceRun, destRun, runID string, written map[string]bool) error {
	sources := discoverLogSources(sourceRun)
	if len(sources) == 0 {
		return nil
	}
	logsDir := filepath.Join(destRun, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("create log artifact directory: %w", err)
	}

	manifest := make([]logManifestEntry, 0, len(sources))
	for i, source := range sources {
		entry := logManifestEntry{SourcePath: recovery.Scrub(source)}
		data, err := os.ReadFile(source)
		if err != nil {
			entry.Error = recovery.Scrub(err.Error())
			manifest = append(manifest, entry)
			continue
		}
		sum := sha256.Sum256(data)
		tailName := fmt.Sprintf("log-%02d-%s.tail.txt", i+1, shortHash(source))
		tailRel := filepath.ToSlash(filepath.Join("logs", tailName))
		tail := recovery.Scrub(lastLines(string(data), logTailLines))
		if err := os.WriteFile(filepath.Join(logsDir, tailName), []byte(tail), 0o644); err != nil {
			return fmt.Errorf("write scrubbed log tail: %w", err)
		}
		entry.Bytes = int64(len(data))
		entry.SHA256 = "sha256:" + hex.EncodeToString(sum[:])
		entry.TailPath = tailRel
		manifest = append(manifest, entry)
		written[filepath.ToSlash(filepath.Join("runs", runID, tailRel))] = true
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal log manifest: %w", err)
	}
	manifestPath := filepath.Join(logsDir, "log_manifest.json")
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write log manifest: %w", err)
	}
	written[filepath.ToSlash(filepath.Join("runs", runID, "logs", "log_manifest.json"))] = true
	return nil
}

func discoverLogSources(sourceRun string) []string {
	seen := map[string]bool{}
	add := func(path string) {
		path = strings.Trim(strings.TrimSpace(path), `"`)
		if path == "" {
			return
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(sourceRun, path)
		}
		cleaned := filepath.Clean(path)
		if seen[cleaned] {
			return
		}
		seen[cleaned] = true
	}

	_ = filepath.WalkDir(sourceRun, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(sourceRun, path)
		if relErr == nil && isRawLogFile(rel) {
			add(path)
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".md") || strings.HasSuffix(strings.ToLower(d.Name()), ".txt") {
			data, readErr := os.ReadFile(path)
			if readErr == nil {
				for _, match := range logPathLinePattern.FindAllStringSubmatch(string(data), -1) {
					if len(match) > 1 {
						add(match[1])
					}
				}
			}
		}
		return nil
	})

	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func isRawLogFile(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	return name == "codex.log" || strings.HasSuffix(name, ".log")
}

func lastLines(text string, maxLines int) string {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n") + "\n"
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	raw := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func mergeEventLines(local, prior []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(local)+len(prior))
	for _, line := range local {
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	for _, line := range prior {
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	return out
}

func commitIfChanged(ctx context.Context, repoPath, message string, deps Deps) (bool, error) {
	status, err := runGit(ctx, deps, repoPath, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status state branch: %w", err)
	}
	if strings.TrimSpace(string(status)) == "" {
		return false, nil
	}
	if _, err := runGit(ctx, deps, repoPath, "add", "-A"); err != nil {
		return false, fmt.Errorf("git add state branch: %w", err)
	}
	if _, err := runGit(ctx, deps, repoPath, "commit", "-m", message); err != nil {
		return false, fmt.Errorf("git commit state branch: %w", err)
	}
	return true, nil
}

func pushBranch(ctx context.Context, repoPath, remote, branch string, deps Deps) error {
	_, err := runGit(ctx, deps, repoPath, "push", remote, "HEAD:refs/heads/"+branch)
	return err
}

func readLease(worktreePath string) (*Lease, error) {
	data, err := os.ReadFile(filepath.Join(worktreePath, "lease.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read lease: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" || strings.TrimSpace(string(data)) == "null" {
		return nil, nil
	}
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		return nil, fmt.Errorf("parse lease: %w", err)
	}
	return &lease, nil
}

func writeLease(worktreePath string, lease Lease) error {
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lease: %w", err)
	}
	return os.WriteFile(filepath.Join(worktreePath, "lease.json"), append(data, '\n'), 0o644)
}

func appendLeaseEvent(worktreePath, runID string, event leaseEvent) error {
	path := filepath.Join(worktreePath, "runs", runID, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create lease event directory: %w", err)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal lease event: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open lease event file: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append lease event: %w", err)
	}
	return nil
}

func handleAcquirePushConflict(ctx context.Context, result LeaseResult, host string, pid int, now time.Time, deps Deps) (LeaseResult, error) {
	remoteLease, err := readRemoteLease(ctx, result.RepoPath, result.Branch, result.Remote, deps)
	if err != nil {
		result.Status = "push-failed"
		result.Message = "lease update was committed locally but push failed"
		return result, nil
	}
	result.Lease = remoteLease
	if remoteLease != nil && !leaseExpired(*remoteLease, now) && !sameConductor(*remoteLease, host, pid, result.RunID) {
		result.Status = "observe-only"
		result.ObserveOnly = true
		result.Message = "observe only: another conductor won the lease compare-and-swap"
		return result, nil
	}
	result.Status = "cas-conflict"
	result.Message = "lease push was rejected; re-read the state branch before acting"
	return result, nil
}

func readRemoteLease(ctx context.Context, repoPath, branch, remote string, deps Deps) (*Lease, error) {
	worktreePath, cleanup, err := prepareReadOnlyWorktree(ctx, repoPath, branch, remote, deps)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return readLease(worktreePath)
}

func leaseExpired(lease Lease, now time.Time) bool {
	expiresAt, err := state.ParseTimestamp(lease.LeaseExpiresAt)
	if err != nil {
		return true
	}
	return !expiresAt.After(now.UTC())
}

func sameConductor(lease Lease, host string, pid int, runID string) bool {
	return lease.Host == host && lease.PID == pid && lease.RunID == runID
}

func cloneLease(lease *Lease) *Lease {
	if lease == nil {
		return nil
	}
	copied := *lease
	return &copied
}

func leaseID(lease *Lease) string {
	if lease == nil {
		return ""
	}
	return lease.LeaseID
}

func sanitizeLeasePart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "host"
	}
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			out.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			out.WriteRune(r)
		case r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '-' || r == '_':
			out.WriteRune(r)
		default:
			out.WriteRune('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func sanitizePathPart(value string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return replacer.Replace(value)
}

type copyTreeOptions struct {
	skipGit   bool
	overwrite bool
	readOnly  bool
}

func copyTree(sourceRoot, destRoot string, opts copyTreeOptions) error {
	return filepath.WalkDir(sourceRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if opts.skipGit {
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) > 0 && parts[0] == ".git" {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		target := filepath.Join(destRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !opts.overwrite {
			if _, err := os.Stat(target); err == nil {
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if opts.readOnly {
			mode = 0o444
		}
		return os.WriteFile(target, data, mode)
	})
}

func hydrateMissingRuns(sourceRuns, destRuns string) ([]string, error) {
	entries, err := os.ReadDir(sourceRuns)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state branch runs: %w", err)
	}
	runs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		runs = append(runs, runID)
		if err := copyTree(filepath.Join(sourceRuns, runID), filepath.Join(destRuns, runID), copyTreeOptions{overwrite: false}); err != nil {
			return nil, fmt.Errorf("hydrate run %s: %w", runID, err)
		}
	}
	sort.Strings(runs)
	return runs, nil
}

func safeRemoveAll(path, root string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing to remove path outside root: %s", path)
	}
	return os.RemoveAll(absPath)
}

func makeTreeReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, 0o444)
	})
}

func makeTreeWritable(root string) error {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o755)
		}
		return os.Chmod(path, 0o644)
	})
}
