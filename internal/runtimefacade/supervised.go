package runtimefacade

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

// SupervisedRuntime adapts internal/supervisedexec for generic local commands.
// It reuses the existing kill-group / hard-cap / stall mechanics; it does not
// add a second supervisor.
type SupervisedRuntime struct {
	Clock Clock
	Sink  OutputSink
	// StallTimeout is forwarded to supervisedexec when > 0 (requires LogPath).
	StallTimeout time.Duration
	LogPath      string
}

// Launch starts argv under supervisedexec.Run in the background.
func (r *SupervisedRuntime) Launch(ctx context.Context, req LaunchRequest) (Handle, error) {
	if err := validateLaunch(req); err != nil {
		return nil, err
	}
	clock := r.Clock
	if clock == nil {
		clock = SystemClock{}
	}
	snap := req.Clone()
	env := snap.Env
	if env == nil {
		env = CleanEnv(nil)
	} else {
		env = CleanEnv(env)
	}

	cmd := exec.Command(snap.Argv[0], snap.Argv[1:]...)
	cmd.Dir = snap.WorkDir
	cmd.Env = env
	sink := r.Sink
	if sink == nil {
		sink = NopSink{}
	}
	cmd.Stdout = &sinkWriter{sink: sink, stderr: false}
	cmd.Stderr = &sinkWriter{sink: sink, stderr: true}

	h := &supervisedHandle{
		clock: clock,
		done:  make(chan struct{}),
		id: Identity{
			AttemptID: snap.AttemptID,
			StartedAt: clock.Now().UTC(),
			Request:   snap,
		},
	}

	opts := supervisedexec.Options{
		HardCap:      snap.HardCap,
		StallTimeout: r.StallTimeout,
		LogPath:      r.LogPath,
		RunID:        snap.AttemptID,
		Role:         firstNonEmpty(snap.Role, "runtimefacade"),
		OnLaunch: func(pid int) {
			h.mu.Lock()
			h.id.PID = pid
			h.launched = true
			h.mu.Unlock()
			close(h.launchCh)
		},
	}
	h.launchCh = make(chan struct{})

	runCtx := context.Background()
	if snap.HardCap > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(context.Background(), snap.HardCap+time.Second)
		h.cancel = cancel
	}

	go func() {
		res, err := supervisedexec.Run(runCtx, cmd, opts)
		h.mu.Lock()
		h.supResult = res
		h.runErr = err
		h.exited = true
		if h.id.PID == 0 && cmd.Process != nil {
			h.id.PID = cmd.Process.Pid
		}
		h.mu.Unlock()
		// Ensure launch waiters unblock even if OnLaunch never fired.
		select {
		case <-h.launchCh:
		default:
			close(h.launchCh)
		}
		close(h.done)
	}()

	// Wait briefly for OnLaunch so Identity has a PID; failed launch returns error.
	select {
	case <-h.launchCh:
	case <-h.done:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		if h.cancel != nil {
			h.cancel()
		}
		return nil, fmt.Errorf("%w: %v", ErrLaunchFailed, ctx.Err())
	}

	h.mu.Lock()
	pid := h.id.PID
	runErr := h.runErr
	exitedQuick := h.exited && !h.launched
	h.mu.Unlock()
	if pid == 0 && exitedQuick {
		if runErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrLaunchFailed, runErr)
		}
		return nil, fmt.Errorf("%w: process exited before launch callback", ErrLaunchFailed)
	}
	if pid == 0 {
		// Still unknown — treat as launch failure and cancel.
		if h.cancel != nil {
			h.cancel()
		}
		return nil, fmt.Errorf("%w: no pid", ErrLaunchFailed)
	}
	return h, nil
}

type supervisedHandle struct {
	id       Identity
	clock    Clock
	cancel   context.CancelFunc
	launchCh chan struct{}
	done     chan struct{}

	mu        sync.Mutex
	launched  bool
	exited    bool
	supResult supervisedexec.Result
	runErr    error
	joined    bool
	signaled  bool
}

func (h *supervisedHandle) Identity() Identity {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id
}

func (h *supervisedHandle) Observe(ctx context.Context) (Observation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return Observation{}, fmt.Errorf("%w: %v", ErrObserveFailed, ctx.Err())
	default:
	}
	now := h.clock.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	obs := Observation{PID: h.id.PID, ObservedAt: now}
	if h.exited {
		obs.State = StateExited
		c := h.supResult.ExitCode
		obs.ExitCode = &c
		obs.EvidenceNote = "supervisedexec joined"
		return obs, nil
	}
	if processAlive(h.id.PID) {
		obs.State = StateAlive
		obs.EvidenceNote = "os alive"
	} else {
		obs.State = StateUnknown
		obs.EvidenceNote = "pid not alive; wait pending"
	}
	return obs, nil
}

func (h *supervisedHandle) Signal(ctx context.Context, kind SignalKind) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", ErrSignalFailed, ctx.Err())
	default:
	}
	h.mu.Lock()
	if h.exited {
		h.mu.Unlock()
		return ErrAlreadyTerminal
	}
	pid := h.id.PID
	h.mu.Unlock()
	if pid <= 0 {
		return fmt.Errorf("%w: no pid", ErrSignalFailed)
	}
	sig := syscall.SIGTERM
	switch kind {
	case SignalKill:
		sig = syscall.SIGKILL
	case SignalInt:
		sig = syscall.SIGINT
	case SignalTerm, "":
		sig = syscall.SIGTERM
	default:
		return fmt.Errorf("%w: unknown signal %q", ErrSignalFailed, kind)
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		if err2 := syscall.Kill(pid, sig); err2 != nil {
			return fmt.Errorf("%w: %v", ErrSignalFailed, err2)
		}
	}
	h.mu.Lock()
	h.signaled = true
	h.mu.Unlock()
	return nil
}

func (h *supervisedHandle) Join(ctx context.Context) (JoinResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-h.done:
		return h.finishJoin()
	case <-ctx.Done():
		obs, _ := h.Observe(context.Background())
		elapsed := h.clock.Now().UTC().Sub(h.id.StartedAt)
		term := TerminalEvidence{
			Exited: obs.State == StateExited, Elapsed: elapsed,
			Outcome: OutcomeUnknown, Note: "join context ended before supervised wait",
		}
		if obs.ExitCode != nil {
			term.ExitCode = *obs.ExitCode
			term.Exited = true
		}
		if term.Exited {
			return JoinResult{Terminal: term}, nil
		}
		return JoinResult{Terminal: term}, fmt.Errorf("%w: %v", ErrJoinIncomplete, ctx.Err())
	}
}

func (h *supervisedHandle) finishJoin() (JoinResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.joined = true
	elapsed := h.supResult.Elapsed
	if elapsed == 0 {
		elapsed = h.clock.Now().UTC().Sub(h.id.StartedAt)
	}
	out := OutcomeCompleted
	switch h.supResult.Outcome {
	case supervisedexec.OutcomeDeadline:
		out = OutcomeDeadline
	case supervisedexec.OutcomeStalled:
		out = OutcomeDeadline
	case supervisedexec.OutcomeCompleted:
		if h.signaled {
			out = OutcomeSignalled
		} else {
			out = OutcomeCompleted
		}
	}
	term := TerminalEvidence{
		Exited:   true,
		ExitCode: h.supResult.ExitCode,
		Killed:   h.supResult.Killed || h.signaled,
		Elapsed:  elapsed,
		Outcome:  out,
		Note:     "supervisedexec terminal",
	}
	// Process completed from OS perspective even if runErr is non-nil (e.g. ctx).
	if h.runErr != nil && !h.exited {
		return JoinResult{Terminal: term}, fmt.Errorf("%w: %v", ErrJoinFailed, h.runErr)
	}
	return JoinResult{Terminal: term}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
