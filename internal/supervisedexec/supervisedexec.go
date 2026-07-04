// Package supervisedexec runs prepared commands under a hard cap and optional
// log-growth/worktree-activity stall detection.
package supervisedexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

const (
	LivenessModeWorktreeMTime = "worktree-mtime"
	LivenessModeLogOnly       = "log-only"
	LivenessModeCustom        = "custom"

	maxCustomLivenessOutputBytes = 8 * 1024
)

// Options configures process supervision.
type Options struct {
	HardCap         time.Duration
	StallTimeout    time.Duration
	LogPath         string
	WorktreePath    string
	Stderr          io.Writer
	StallGrace      time.Duration
	OnStall         func(silentFor time.Duration)
	LivenessMode    string
	LivenessCommand LivenessCommand
	// RunID and Role tag the spawned child as loopcoder-managed and place it in
	// a per-run kill-group (spec 0390, Decision 11). Both may be empty.
	RunID string
	Role  string
}

type LivenessCommand struct {
	Command string
	Args    []string
	Timeout time.Duration
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

type customLivenessObservation struct {
	ok       bool
	exitCode int
	output   string
	timedOut bool
	errText  string
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
	if opts.StallTimeout > 0 {
		switch opts.LivenessMode {
		case LivenessModeWorktreeMTime, LivenessModeLogOnly:
		case LivenessModeCustom:
			if strings.TrimSpace(opts.LivenessCommand.Command) == "" {
				return Result{Elapsed: time.Since(start)}, errors.New("supervisedexec: custom liveness command is required when LivenessMode is custom")
			}
		default:
			return Result{Elapsed: time.Since(start)}, fmt.Errorf("supervisedexec: invalid liveness mode %q", opts.LivenessMode)
		}
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
	var lastCustom customLivenessObservation
	var lastWorktreeWalk time.Time
	var worktreeSignalDisabled bool
	var worktreeWarningEmitted bool
	lastProgress := start
	if opts.StallTimeout > 0 {
		lastLog = observeLog(opts.LogPath)
		switch opts.LivenessMode {
		case LivenessModeWorktreeMTime:
			lastWorktree = observeWorktree(opts.WorktreePath)
			lastWorktreeWalk = start
			if lastWorktree.rootErr != nil {
				warnWorktreeUnavailable(opts.Stderr, opts.WorktreePath, lastWorktree.rootErr, &worktreeWarningEmitted)
				worktreeSignalDisabled = true
				lastWorktree = worktreeObservation{}
			}
		case LivenessModeCustom:
			lastCustom = runCustomLiveness(ctx, opts.WorktreePath, opts.LivenessCommand, opts.StallTimeout)
			logCustomLiveness(opts.LogPath, opts.LivenessCommand, lastCustom)
			lastLog = observeLog(opts.LogPath)
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
			now := time.Now()
			currentLog := observeLog(opts.LogPath)
			currentWorktree := lastWorktree
			logProgress := currentLog.changedFrom(lastLog)
			worktreeProgress := false
			customProgress := false
			if opts.LivenessMode == LivenessModeWorktreeMTime && !worktreeSignalDisabled && shouldWalkWorktree(now, lastWorktreeWalk, worktreePoll) {
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
			if opts.LivenessMode == LivenessModeCustom {
				currentCustom := runCustomLiveness(ctx, opts.WorktreePath, opts.LivenessCommand, opts.StallTimeout)
				logCustomLiveness(opts.LogPath, opts.LivenessCommand, currentCustom)
				customProgress = currentCustom.changedFrom(lastCustom)
				lastCustom = currentCustom
				lastLog = observeLog(opts.LogPath)
			} else {
				lastLog = currentLog
			}
			if logProgress || worktreeProgress || customProgress {
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
	opts.LivenessMode = normalizeLivenessMode(opts.LivenessMode)
	return opts
}

func normalizeLivenessMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "", LivenessModeWorktreeMTime:
		return LivenessModeWorktreeMTime
	case LivenessModeLogOnly:
		return LivenessModeLogOnly
	case LivenessModeCustom:
		return LivenessModeCustom
	default:
		return strings.TrimSpace(strings.ToLower(mode))
	}
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

func runCustomLiveness(ctx context.Context, worktreePath string, command LivenessCommand, stallTimeout time.Duration) customLivenessObservation {
	timeout := customLivenessTimeout(stallTimeout, command.Timeout)
	cmd := customLivenessCommand(ctx, command)
	if strings.TrimSpace(worktreePath) != "" {
		cmd.Dir = worktreePath
	}
	var output limitedBuffer
	output.limit = maxCustomLivenessOutputBytes
	cmd.Stdout = &output
	cmd.Stderr = &output
	result, err := Run(ctx, cmd, Options{HardCap: timeout})
	exitCode := result.ExitCode
	if result.Outcome != OutcomeCompleted || (err != nil && cmd.ProcessState == nil) {
		exitCode = -1
	}
	observation := customLivenessObservation{
		exitCode: exitCode,
		output:   strings.TrimRight(output.String(), "\r\n"),
	}
	if result.Outcome == OutcomeDeadline {
		observation.timedOut = true
		if err != nil {
			observation.errText = err.Error()
		} else {
			observation.errText = fmt.Sprintf("deadline exceeded after %s", timeout)
		}
		return observation
	}
	if result.Outcome == OutcomeCompleted && err == nil && result.ExitCode == 0 {
		observation.ok = true
		return observation
	}
	if err != nil {
		observation.errText = err.Error()
	}
	return observation
}

func customLivenessCommand(ctx context.Context, command LivenessCommand) *exec.Cmd {
	if len(command.Args) > 0 {
		return exec.CommandContext(ctx, command.Command, command.Args...)
	}
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command.Command)
	}
	return exec.CommandContext(ctx, "sh", "-c", command.Command)
}

func customLivenessTimeout(stallTimeout, configured time.Duration) time.Duration {
	if stallTimeout <= 0 {
		stallTimeout = time.Second
	}
	maximum := stallTimeout / 2
	if maximum < 100*time.Millisecond {
		maximum = stallTimeout
	}
	if maximum <= 0 {
		maximum = 100 * time.Millisecond
	}
	if configured > 0 && configured < maximum {
		return configured
	}
	timeout := stallTimeout / 4
	if timeout < 100*time.Millisecond {
		timeout = 100 * time.Millisecond
	}
	if timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	if timeout > maximum {
		timeout = maximum
	}
	return timeout
}

func (o customLivenessObservation) changedFrom(prev customLivenessObservation) bool {
	return o.ok && (!prev.ok || o.exitCode != prev.exitCode || o.output != prev.output)
}

func logCustomLiveness(logPath string, command LivenessCommand, observation customLivenessObservation) {
	if strings.TrimSpace(logPath) == "" {
		return
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer file.Close()

	fmt.Fprintf(file, "\n[loopcoder] custom liveness command=%q exit=%d", commandLineForLog(command), observation.exitCode)
	if observation.timedOut {
		fmt.Fprint(file, " timeout=true")
	}
	if observation.errText != "" {
		fmt.Fprintf(file, " error=%s", observation.errText)
	}
	fmt.Fprintln(file)
	if strings.TrimSpace(observation.output) != "" {
		fmt.Fprintln(file, strings.TrimRight(observation.output, "\r\n"))
	}
}

func commandLineForLog(command LivenessCommand) string {
	if len(command.Args) == 0 {
		return command.Command
	}
	parts := append([]string{command.Command}, command.Args...)
	return strings.Join(parts, " ")
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - b.Buffer.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.Buffer.Write(p[:remaining])
		} else {
			_, _ = b.Buffer.Write(p)
		}
	}
	return len(p), nil
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
