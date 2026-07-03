// Package supervisedexec runs prepared commands under a hard cap and optional
// log/worktree-progress stall detection.
package supervisedexec

import (
	"context"
	"errors"
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
	OutcomeStalled                  // killed: no log/worktree progress for StallTimeout
	OutcomeDeadline                 // killed: exceeded HardCap
)

// Options configures process supervision.
type Options struct {
	HardCap      time.Duration
	StallTimeout time.Duration
	LogPath      string
	WorktreePath string
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
	exists bool
	mtimes map[string]time.Time
}

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
	lastProgress := start
	if opts.StallTimeout > 0 {
		lastLog = observeLog(opts.LogPath)
		lastWorktree = observeWorktree(opts.WorktreePath)
		stallTicks = time.NewTicker(stallPollInterval(opts.StallTimeout))
		defer stallTicks.Stop()
		stallC = stallTicks.C
	}

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
			now := time.Now()
			currentLog := observeLog(opts.LogPath)
			currentWorktree := observeWorktree(opts.WorktreePath)
			logChanged := currentLog.changedFrom(lastLog)
			worktreeChanged := currentWorktree.changedFrom(lastWorktree)
			if logChanged {
				lastLog = currentLog
			}
			if worktreeChanged {
				lastWorktree = currentWorktree
			}
			if logChanged || worktreeChanged {
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
	info, err := os.Stat(path)
	if err != nil {
		return worktreeObservation{}
	}
	observation := worktreeObservation{
		exists: true,
		mtimes: map[string]time.Time{},
	}
	if !info.IsDir() {
		observation.mtimes[path] = info.ModTime()
		return observation
	}
	_ = filepath.Walk(path, func(candidate string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		info, err := os.Stat(candidate)
		if err != nil {
			return nil
		}
		observation.mtimes[candidate] = info.ModTime()
		return nil
	})
	return observation
}

func (o worktreeObservation) changedFrom(prev worktreeObservation) bool {
	if o.exists != prev.exists {
		return true
	}
	if !o.exists {
		return false
	}
	if len(o.mtimes) != len(prev.mtimes) {
		return true
	}
	for path, modTime := range o.mtimes {
		prevModTime, ok := prev.mtimes[path]
		if !ok || !modTime.Equal(prevModTime) {
			return true
		}
	}
	return false
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
