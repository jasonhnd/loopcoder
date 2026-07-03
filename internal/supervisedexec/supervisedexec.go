// Package supervisedexec runs prepared commands under a hard cap and optional
// log-growth/worktree-activity stall detection.
package supervisedexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// DefaultHardCap is the package-level fallback used when Options.HardCap is
// zero or negative. Callers should still pass a site-specific cap.
const DefaultHardCap = 30 * time.Minute

var defaultHardCap = DefaultHardCap

// Outcome describes how a supervised process finished.
type Outcome int

const (
	OutcomeCompleted Outcome = iota // process exited on its own (any exit code)
	OutcomeStalled                  // killed: no log growth or worktree activity for StallTimeout
	OutcomeDeadline                 // killed: exceeded HardCap
)

// Options configures process supervision.
type Options struct {
	HardCap      time.Duration
	StallTimeout time.Duration
	LogPath      string
	WorktreePath string
	Stderr       io.Writer
	StallGrace   time.Duration
	OnStall      func(silentFor time.Duration)
	// RunID and Role tag the spawned child as loopcoder-managed and place it in
	// a per-run kill-group (spec 0390, Decision 11). Both may be empty.
	RunID string
	Role  string
}

// Result reports the outcome of a supervised command.
type Result struct {
	Outcome  Outcome
	ExitCode int
	Killed   bool
	Elapsed  time.Duration
}

type waitResult struct {
	err   error
	state *os.ProcessState
}

type logObservation struct {
	exists  bool
	size    int64
	modTime time.Time
}

type worktreeObservation struct {
	exists        bool
	latestModTime time.Time
	rootErr       error
}

var timeNow = time.Now

// Run starts cmd, supervises it, and waits until the process exits or is
// terminated by the parent context, the hard cap, or the optional stall signal.
//
// On a setup error (a nil cmd, a missing LogPath when stall detection is
// requested, or a failed cmd.Start) Run returns a non-nil error together with a
// zero-value Result whose Outcome is OutcomeCompleted. Callers must therefore
// check the returned error before interpreting Result.Outcome.
func Run(ctx context.Context, cmd *exec.Cmd, opts Options) (Result, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if cmd == nil {
		return Result{Elapsed: time.Since(start)}, errors.New("supervisedexec: nil command")
	}

	opts = normalizeOptions(opts)
	if opts.StallTimeout > 0 && opts.LogPath == "" {
		return Result{Elapsed: time.Since(start)}, errors.New("supervisedexec: LogPath is required when StallTimeout > 0")
	}

	group := newKillGroup(opts.RunID)
	group.prepare(cmd)
	applyEnvMarkers(cmd, opts.RunID, opts.Role)

	if err := cmd.Start(); err != nil {
		group.close()
		return Result{Elapsed: time.Since(start)}, err
	}
	_ = group.adopt(cmd)
	managedPID := cmd.Process.Pid
	registerProc(cmd, opts.RunID, opts.Role, group)
	defer func() {
		deregisterProc(managedPID)
		group.close()
	}()

	waitCh := make(chan waitResult, 1)
	go func() {
		err := cmd.Wait()
		waitCh <- waitResult{err: err, state: cmd.ProcessState}
	}()

	hardCap := time.NewTimer(opts.HardCap)
	defer hardCap.Stop()

	var stallTicks *time.Ticker
	var stallC <-chan time.Time
	var lastLog logObservation
	var lastWorktree worktreeObservation
	var lastWorktreeWalk time.Time
	var worktreeSignalDisabled bool
	var worktreeWarningEmitted bool
	lastProgress := start
	if opts.StallTimeout > 0 {
		lastLog = observeLog(opts.LogPath)
		lastWorktree = observeWorktree(opts.WorktreePath)
		lastWorktreeWalk = start
		if lastWorktree.rootErr != nil {
			warnWorktreeUnavailable(opts.Stderr, opts.WorktreePath, lastWorktree.rootErr, &worktreeWarningEmitted)
			worktreeSignalDisabled = true
			lastWorktree = worktreeObservation{}
		}
		stallTicks = time.NewTicker(stallPollInterval(opts.StallTimeout))
		defer stallTicks.Stop()
		stallC = stallTicks.C
	}
	worktreePoll := worktreePollInterval(opts.StallTimeout, stallPollInterval(opts.StallTimeout))

	for {
		select {
		case wr := <-waitCh:
			return completedResult(start, wr)
		case <-hardCap.C:
			return killAndDrain(start, group, cmd.Process, waitCh, OutcomeDeadline, nil)
		case <-ctx.Done():
			res, err := killAndDrain(start, group, cmd.Process, waitCh, OutcomeDeadline, ctx.Err())
			return res, err
		case <-stallC:
			now := timeNow()
			currentLog := observeLog(opts.LogPath)
			currentWorktree := lastWorktree
			logProgress := currentLog.changedFrom(lastLog)
			worktreeProgress := false
			if !worktreeSignalDisabled && shouldWalkWorktree(now, lastWorktreeWalk, worktreePoll) {
				currentWorktree = observeWorktree(opts.WorktreePath)
				lastWorktreeWalk = now
				if currentWorktree.rootErr != nil {
					warnWorktreeUnavailable(opts.Stderr, opts.WorktreePath, currentWorktree.rootErr, &worktreeWarningEmitted)
					worktreeSignalDisabled = true
					currentWorktree = worktreeObservation{}
				} else {
					worktreeProgress = currentWorktree.advancedFrom(lastWorktree)
					lastWorktree = currentWorktree
				}
			}
			lastLog = currentLog
			if logProgress || worktreeProgress {
				lastProgress = now
				continue
			}
			silentFor := now.Sub(lastProgress)
			if silentFor >= opts.StallTimeout {
				return handleStall(ctx, start, group, cmd.Process, waitCh, hardCap, opts, silentFor)
			}
		}
	}
}

func normalizeOptions(opts Options) Options {
	if opts.HardCap <= 0 {
		opts.HardCap = defaultHardCap
	}
	if opts.StallTimeout < 0 {
		opts.StallTimeout = 0
	}
	if opts.StallGrace < 0 {
		opts.StallGrace = 0
	}
	return opts
}

func completedResult(start time.Time, wr waitResult) (Result, error) {
	result := Result{
		Outcome: OutcomeCompleted,
		Elapsed: time.Since(start),
	}
	if wr.state != nil {
		result.ExitCode = wr.state.ExitCode()
	}
	if wr.err == nil {
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(wr.err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}

	return result, wr.err
}

func handleStall(ctx context.Context, start time.Time, group killGroup, process *os.Process, waitCh <-chan waitResult, hardCap *time.Timer, opts Options, silentFor time.Duration) (Result, error) {
	if opts.OnStall != nil {
		go opts.OnStall(silentFor)
	}
	if opts.StallGrace > 0 {
		grace := time.NewTimer(opts.StallGrace)
		defer grace.Stop()
		select {
		case wr := <-waitCh:
			return completedResult(start, wr)
		case <-hardCap.C:
			return killAndDrain(start, group, process, waitCh, OutcomeDeadline, nil)
		case <-ctx.Done():
			res, err := killAndDrain(start, group, process, waitCh, OutcomeDeadline, ctx.Err())
			return res, err
		case <-grace.C:
		}
	}
	return killAndDrain(start, group, process, waitCh, OutcomeStalled, nil)
}

func killAndDrain(start time.Time, group killGroup, process *os.Process, waitCh <-chan waitResult, outcome Outcome, cause error) (Result, error) {
	select {
	case wr := <-waitCh:
		return completedResult(start, wr)
	default:
	}

	// Kill the whole kill-group (child + its descendant subtree) first, then the
	// direct process as a fallback if the group handle was unavailable.
	if group != nil {
		_ = group.kill()
	}
	if process != nil {
		_ = process.Kill()
	}
	<-waitCh

	result := Result{
		Outcome: outcome,
		Killed:  true,
		Elapsed: time.Since(start),
	}
	if cause != nil {
		return result, cause
	}
	return result, nil
}

func observeLog(path string) logObservation {
	info, err := os.Stat(path)
	if err != nil {
		return logObservation{}
	}
	return logObservation{
		exists:  true,
		size:    info.Size(),
		modTime: info.ModTime(),
	}
}

func (o logObservation) changedFrom(prev logObservation) bool {
	return o.exists != prev.exists || o.size != prev.size || !o.modTime.Equal(prev.modTime)
}

func observeWorktree(path string) worktreeObservation {
	if path == "" {
		return worktreeObservation{}
	}

	var observation worktreeObservation
	_ = filepath.WalkDir(path, func(currentPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if currentPath == path {
				observation.rootErr = walkErr
				return filepath.SkipDir
			}
			return nil
		}
		if currentPath != path && entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Stat(currentPath)
		if err != nil || info.IsDir() {
			return nil
		}
		observation.exists = true
		if info.ModTime().After(observation.latestModTime) {
			observation.latestModTime = info.ModTime()
		}
		return nil
	})
	return observation
}

func (o worktreeObservation) advancedFrom(prev worktreeObservation) bool {
	return o.exists && (!prev.exists || o.latestModTime.After(prev.latestModTime))
}

func stallPollInterval(timeout time.Duration) time.Duration {
	interval := timeout / 4
	if interval < time.Millisecond {
		return time.Millisecond
	}
	if interval > 500*time.Millisecond {
		return 500 * time.Millisecond
	}
	return interval
}

func worktreePollInterval(timeout, logInterval time.Duration) time.Duration {
	interval := timeout / 8
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < logInterval {
		interval = logInterval
	}
	return interval
}

func shouldWalkWorktree(current, last time.Time, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	if last.IsZero() {
		return true
	}
	return current.Sub(last) >= interval
}

func warnWorktreeUnavailable(w io.Writer, path string, err error, emitted *bool) {
	if emitted == nil || *emitted || err == nil || path == "" {
		return
	}
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "[loopcoder] warning: worktree liveness signal unavailable for %s: %v; falling back to log-only stall detection\n", path, err)
	*emitted = true
}
