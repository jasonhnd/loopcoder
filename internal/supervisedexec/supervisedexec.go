// Package supervisedexec runs prepared commands under a hard cap and optional
// log-growth/worktree-activity stall detection.
package supervisedexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/process"
)

// DefaultHardCap is the package-level fallback used when Options.HardCap is
// zero or negative. Callers should still pass a site-specific cap.
const DefaultHardCap = lcdefaults.SupervisedExecHardCap

var defaultHardCap = DefaultHardCap
var worktreeLivenessMaxFiles = lcdefaults.WorktreeLivenessMaxFiles

const livenessArgvCommandPrefix = "\x00loopcoder-liveness-argv:"

var generatedWorktreeDirNames = map[string]bool{
	".cache":        true,
	".git":          true,
	".loopcoder":    true,
	".next":         true,
	".parcel-cache": true,
	".turbo":        true,
	"build":         true,
	"coverage":      true,
	"dist":          true,
	"node_modules":  true,
	"target":        true,
	"vendor":        true,
}

// LivenessMode selects the stall watchdog's progress signal.
type LivenessMode string

const (
	LivenessModeWorktreeMTime LivenessMode = "worktree-mtime"
	LivenessModeLogOnly       LivenessMode = "log-only"
	LivenessModeCustom        LivenessMode = "custom"
)

// Outcome describes how a supervised process finished.
type Outcome int

const (
	OutcomeCompleted Outcome = iota // process exited on its own (any exit code)
	OutcomeStalled                  // killed: no meaningful log, worktree, or process-tree activity for StallTimeout
	OutcomeDeadline                 // killed: exceeded HardCap
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
	LivenessMode    LivenessMode
	LivenessCommand string
	// LivenessCommandHardCap bounds one custom liveness probe. When zero, Run
	// derives a small cap from StallTimeout. The parent HardCap remains the
	// absolute ceiling.
	LivenessCommandHardCap time.Duration
	// RunID and Role tag the spawned child as loopcoder-managed and place it in
	// a per-run kill-group (spec 0390, Decision 11). Both may be empty.
	RunID string
	Role  string
	// OnStart is called after the provider process starts and is adopted into
	// its kill-group, before Run reports any running state to callers.
	OnStart func(StartedProcess) error
}

// Result reports the outcome of a supervised command.
type Result struct {
	Outcome  Outcome
	ExitCode int
	Killed   bool
	Elapsed  time.Duration
}

type StartedProcess struct {
	PID                   int
	PGID                  int
	ProcessBirthIdentity  string
	ExecutableIdentity    string
	ObservedAt            time.Time
	IdentityAmbiguous     bool
	IdentityAmbiguityNote string
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
	filesExamined int
}

// EncodeLivenessArgv encodes a no-shell custom liveness command for callers
// constrained to the legacy string-only liveness command boundary.
func EncodeLivenessArgv(argv []string) string {
	data, _ := json.Marshal(argv)
	return livenessArgvCommandPrefix + base64.StdEncoding.EncodeToString(data)
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
	if err := validateOptions(opts); err != nil {
		return Result{Elapsed: time.Since(start)}, err
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
	if opts.OnStart != nil {
		started, snapshotErr := startedProcessSnapshot(managedPID, start)
		if snapshotErr != nil {
			_, _ = killAndDrain(start, group, cmd.Process, waitStartedProcess(cmd), OutcomeDeadline, snapshotErr)
			return Result{Outcome: OutcomeDeadline, Killed: true, Elapsed: time.Since(start)}, snapshotErr
		}
		if err := opts.OnStart(started); err != nil {
			_, _ = killAndDrain(start, group, cmd.Process, waitStartedProcess(cmd), OutcomeDeadline, err)
			return Result{Outcome: OutcomeDeadline, Killed: true, Elapsed: time.Since(start)}, err
		}
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
	var lastWorktree worktreeObservation
	var lastWorktreeWalk time.Time
	var lastProcess processActivityObservation
	var lastProcessPoll time.Time
	var worktreeSignalDisabled bool
	var worktreeWarningEmitted bool
	processActivityEnabled := opts.LivenessMode == LivenessModeWorktreeMTime
	lastProgress := start
	if opts.StallTimeout > 0 {
		lastLog = observeLog(opts.LogPath)
		if processActivityEnabled {
			lastProcess = group.activity()
			lastProcessPoll = start
		}
		if opts.LivenessMode == LivenessModeWorktreeMTime {
			lastWorktree = observeWorktree(opts.WorktreePath)
			lastWorktreeWalk = start
			if lastWorktree.rootErr != nil {
				warnWorktreeUnavailable(opts.Stderr, opts.WorktreePath, lastWorktree.rootErr, &worktreeWarningEmitted)
				worktreeSignalDisabled = true
				lastWorktree = worktreeObservation{}
			}
		}
		stallTicks = time.NewTicker(stallPollInterval(opts.StallTimeout))
		defer stallTicks.Stop()
		stallC = stallTicks.C
	}
	worktreePoll := worktreePollInterval(opts.StallTimeout, stallPollInterval(opts.StallTimeout))
	processPoll := processPollInterval(opts.StallTimeout, stallPollInterval(opts.StallTimeout))

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
			remainingHardCap := opts.HardCap - now.Sub(start)
			if remainingHardCap <= 0 {
				return killAndDrain(start, group, cmd.Process, waitCh, OutcomeDeadline, nil)
			}
			currentLog := observeLog(opts.LogPath)
			currentWorktree := lastWorktree
			logProgress := meaningfulLogProgress(opts.LogPath, lastLog, currentLog)
			worktreeProgress := false
			if opts.LivenessMode == LivenessModeWorktreeMTime && !worktreeSignalDisabled && shouldWalkWorktree(now, lastWorktreeWalk, worktreePoll) {
				currentWorktree = observeWorktreeAfter(opts.WorktreePath, lastWorktree.latestModTime)
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
			processProgress := false
			if processActivityEnabled && shouldObserveProcessActivity(now, lastProcessPoll, processPoll) {
				currentProcess := group.activity()
				lastProcessPoll = now
				processProgress = currentProcess.changedFrom(lastProcess)
				if currentProcess.available {
					lastProcess = currentProcess
				}
			}
			customProgress := false
			if opts.LivenessMode == LivenessModeCustom && !logProgress {
				customProgress = runCustomLivenessProbe(ctx, opts, remainingHardCap)
				// The probe output is diagnostic. Reset the log baseline after
				// logging it so probe logs do not self-satisfy provider log growth
				// on the next tick.
				currentLog = observeLog(opts.LogPath)
				now = time.Now()
				if opts.HardCap-now.Sub(start) <= 0 {
					return killAndDrain(start, group, cmd.Process, waitCh, OutcomeDeadline, nil)
				}
			}
			lastLog = currentLog
			if logProgress || worktreeProgress || processProgress || customProgress {
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

func startedProcessSnapshot(pid int, observedAt time.Time) (StartedProcess, error) {
	identity, err := process.Snapshot(pid, observedAt)
	if err != nil {
		return StartedProcess{}, err
	}
	return StartedProcess{
		PID:                   identity.PID,
		PGID:                  identity.PGID,
		ProcessBirthIdentity:  identity.ProcessBirthIdentity,
		ExecutableIdentity:    identity.ExecutableIdentity,
		ObservedAt:            identity.ObservedAt,
		IdentityAmbiguous:     identity.Ambiguous,
		IdentityAmbiguityNote: identity.AmbiguityReason,
	}, nil
}

func waitStartedProcess(cmd *exec.Cmd) <-chan waitResult {
	waitCh := make(chan waitResult, 1)
	go func() {
		err := cmd.Wait()
		waitCh <- waitResult{err: err, state: cmd.ProcessState}
	}()
	return waitCh
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

func validateOptions(opts Options) error {
	if opts.StallTimeout <= 0 {
		return nil
	}
	if opts.LogPath == "" {
		return errors.New("supervisedexec: LogPath is required when StallTimeout > 0")
	}
	switch opts.LivenessMode {
	case LivenessModeWorktreeMTime, LivenessModeLogOnly:
		return nil
	case LivenessModeCustom:
		if strings.TrimSpace(opts.LivenessCommand) == "" {
			return errors.New("supervisedexec: LivenessCommand is required when LivenessMode is custom")
		}
		if isEncodedLivenessArgv(opts.LivenessCommand) {
			if _, err := decodeLivenessArgv(opts.LivenessCommand); err != nil {
				return fmt.Errorf("supervisedexec: invalid LivenessCommand argv encoding: %w", err)
			}
		}
		return nil
	default:
		return fmt.Errorf("supervisedexec: unsupported LivenessMode %q", opts.LivenessMode)
	}
}

func normalizeLivenessMode(mode LivenessMode) LivenessMode {
	switch strings.ToLower(strings.TrimSpace(string(mode))) {
	case "", string(LivenessModeWorktreeMTime):
		return LivenessModeWorktreeMTime
	case string(LivenessModeLogOnly):
		return LivenessModeLogOnly
	case string(LivenessModeCustom):
		return LivenessModeCustom
	default:
		return LivenessMode(strings.ToLower(strings.TrimSpace(string(mode))))
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

func meaningfulLogProgress(path string, prev, current logObservation) bool {
	if !current.exists {
		return false
	}
	if !prev.exists || current.size < prev.size {
		return logContainsProviderProgress(path, 0)
	}
	if current.size > prev.size {
		return logContainsProviderProgress(path, prev.size)
	}
	return !current.modTime.Equal(prev.modTime)
}

func logContainsProviderProgress(path string, offset int64) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return false
		}
	}
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadString('\n')
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "[loopcoder]") {
			return true
		}
		if err != nil {
			return false
		}
	}
}

func observeWorktree(path string) worktreeObservation {
	return observeWorktreeAfter(path, time.Time{})
}

func observeWorktreeAfter(path string, after time.Time) worktreeObservation {
	if path == "" {
		return worktreeObservation{}
	}

	var observation worktreeObservation
	_ = filepath.WalkDir(path, func(currentPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if currentPath == path {
				observation.rootErr = walkErr
				return filepath.SkipAll
			}
			return nil
		}
		if currentPath != path && entry.IsDir() && shouldSkipWorktreeDir(entry.Name()) {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		observation.filesExamined++
		if worktreeLivenessMaxFiles > 0 && observation.filesExamined > worktreeLivenessMaxFiles {
			observation.rootErr = fmt.Errorf("worktree liveness file cap exceeded after %d files", worktreeLivenessMaxFiles)
			return filepath.SkipAll
		}
		info, err := entry.Info()
		if err != nil || info.IsDir() {
			return nil
		}
		observation.exists = true
		if info.ModTime().After(observation.latestModTime) {
			observation.latestModTime = info.ModTime()
		}
		if !after.IsZero() && info.ModTime().After(after) {
			return filepath.SkipAll
		}
		return nil
	})
	return observation
}

func shouldSkipWorktreeDir(name string) bool {
	return generatedWorktreeDirNames[name]
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

func shouldObserveProcessActivity(current, last time.Time, interval time.Duration) bool {
	return shouldWalkWorktree(current, last, interval)
}

func runCustomLivenessProbe(ctx context.Context, opts Options, remainingHardCap time.Duration) bool {
	var output bytes.Buffer
	cmd, displayCommand, buildErr := customLivenessCommand(opts.LivenessCommand, opts.WorktreePath)
	if buildErr != nil {
		appendCustomLivenessLog(opts.LogPath, opts.LivenessCommand, "", Result{}, buildErr, false)
		return false
	}
	cmd.Stdout = &output
	cmd.Stderr = &output

	result, err := Run(ctx, cmd, Options{
		HardCap: customLivenessHardCap(opts, remainingHardCap),
		RunID:   customLivenessRunID(opts.RunID),
		Role:    customLivenessRole(opts.Role),
	})
	success := err == nil && result.Outcome == OutcomeCompleted && result.ExitCode == 0
	appendCustomLivenessLog(opts.LogPath, displayCommand, output.String(), result, err, success)
	return success
}

func customLivenessCommand(command, worktreePath string) (*exec.Cmd, string, error) {
	if isEncodedLivenessArgv(command) {
		argv, err := decodeLivenessArgv(command)
		if err != nil {
			return nil, "", err
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = worktreePath
		return cmd, fmt.Sprintf("argv:%q", argv), nil
	}
	return customLivenessShellCommand(command, worktreePath), command, nil
}

func customLivenessShellCommand(command, worktreePath string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("cmd", "/c", command)
		cmd.Dir = worktreePath
		return cmd
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = worktreePath
	return cmd
}

func isEncodedLivenessArgv(command string) bool {
	return strings.HasPrefix(command, livenessArgvCommandPrefix)
}

func decodeLivenessArgv(command string) ([]string, error) {
	encoded := strings.TrimPrefix(command, livenessArgvCommandPrefix)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	var argv []string
	if err := json.Unmarshal(data, &argv); err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, errors.New("argv is empty")
	}
	for index, arg := range argv {
		if strings.TrimSpace(arg) == "" {
			return nil, fmt.Errorf("argv[%d] is empty", index)
		}
	}
	return argv, nil
}

func customLivenessHardCap(opts Options, remainingHardCap time.Duration) time.Duration {
	cap := opts.LivenessCommandHardCap
	if cap <= 0 {
		cap = opts.StallTimeout / 2
	}
	if cap <= 0 {
		cap = lcdefaults.ProcessLivenessCommandCap
	}
	if cap < 100*time.Millisecond {
		cap = opts.StallTimeout
	}
	if cap <= 0 {
		cap = 100 * time.Millisecond
	}
	if cap > lcdefaults.ProcessLivenessCommandCap {
		cap = lcdefaults.ProcessLivenessCommandCap
	}
	if opts.HardCap > 0 && cap > opts.HardCap {
		cap = opts.HardCap
	}
	if remainingHardCap > 0 && cap > remainingHardCap {
		cap = remainingHardCap
	}
	return cap
}

func customLivenessRunID(runID string) string {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return ""
	}
	return runID + "-liveness"
}

func customLivenessRole(role string) string {
	role = strings.TrimSpace(role)
	if role == "" {
		return "liveness"
	}
	return role + "-liveness"
}

func appendCustomLivenessLog(logPath, command, output string, result Result, runErr error, success bool) {
	if strings.TrimSpace(logPath) == "" {
		return
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()

	status := "failed"
	if success {
		status = "ok"
	}
	fmt.Fprintf(file, "\n[loopcoder] custom liveness %s exit=%d outcome=%d elapsed=%s command=%q\n", status, result.ExitCode, result.Outcome, result.Elapsed.Round(time.Millisecond), command)
	if runErr != nil {
		fmt.Fprintf(file, "[loopcoder] custom liveness error: %v\n", runErr)
	}
	if text := boundedCustomLivenessOutput(output); text != "" {
		fmt.Fprintln(file, text)
	}
}

func boundedCustomLivenessOutput(output string) string {
	output = strings.TrimRight(strings.ReplaceAll(output, "\r\n", "\n"), "\r\n")
	if len(output) <= lcdefaults.CustomLivenessOutputMaxBytes {
		return output
	}
	return output[:lcdefaults.CustomLivenessOutputMaxBytes] + "\n[loopcoder] custom liveness output truncated"
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
