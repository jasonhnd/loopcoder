// Package supervisedexec runs prepared commands under a hard cap and optional
// log-growth stall detection.
package supervisedexec

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
	OutcomeStalled                  // killed: no log growth for StallTimeout
	OutcomeDeadline                 // killed: exceeded HardCap
)

// Options configures process supervision.
type Options struct {
	HardCap      time.Duration
	StallTimeout time.Duration
	LogPath      string
	StallGrace   time.Duration
	OnStall      func(silentFor time.Duration)
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

// Run starts cmd, supervises it, and waits until the process exits or is
// terminated by the parent context, the hard cap, or the optional stall signal.
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

	if err := cmd.Start(); err != nil {
		return Result{Elapsed: time.Since(start)}, err
	}

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
	lastProgress := start
	if opts.StallTimeout > 0 {
		lastLog = observeLog(opts.LogPath)
		stallTicks = time.NewTicker(stallPollInterval(opts.StallTimeout))
		defer stallTicks.Stop()
		stallC = stallTicks.C
	}

	for {
		select {
		case wr := <-waitCh:
			return completedResult(start, wr)
		case <-hardCap.C:
			return killAndDrain(start, cmd.Process, waitCh, OutcomeDeadline, nil)
		case <-ctx.Done():
			res, err := killAndDrain(start, cmd.Process, waitCh, OutcomeDeadline, ctx.Err())
			return res, err
		case <-stallC:
			now := time.Now()
			currentLog := observeLog(opts.LogPath)
			if currentLog.changedFrom(lastLog) {
				lastLog = currentLog
				lastProgress = now
				continue
			}
			silentFor := now.Sub(lastProgress)
			if silentFor >= opts.StallTimeout {
				return handleStall(ctx, start, cmd.Process, waitCh, hardCap, opts, silentFor)
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

func handleStall(ctx context.Context, start time.Time, process *os.Process, waitCh <-chan waitResult, hardCap *time.Timer, opts Options, silentFor time.Duration) (Result, error) {
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
			return killAndDrain(start, process, waitCh, OutcomeDeadline, nil)
		case <-ctx.Done():
			res, err := killAndDrain(start, process, waitCh, OutcomeDeadline, ctx.Err())
			return res, err
		case <-grace.C:
		}
	}
	return killAndDrain(start, process, waitCh, OutcomeStalled, nil)
}

func killAndDrain(start time.Time, process *os.Process, waitCh <-chan waitResult, outcome Outcome, cause error) (Result, error) {
	select {
	case wr := <-waitCh:
		return completedResult(start, wr)
	default:
	}

	_ = process.Kill()
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
