package runtimefacade

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// FixtureRuntime launches local argv processes without provider policy.
// It is the primary test adapter for V090-012.
type FixtureRuntime struct {
	Clock Clock
	Sink  OutputSink
}

// Launch starts req.Argv under a cleaned environment.
func (r *FixtureRuntime) Launch(ctx context.Context, req LaunchRequest) (Handle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateLaunch(req); err != nil {
		return nil, err
	}
	clock := r.Clock
	if clock == nil {
		clock = SystemClock{}
	}
	sink := r.Sink
	if sink == nil {
		sink = NopSink{}
	}
	snap := req.Clone()
	env := snap.Env
	if env == nil {
		env = CleanEnv(nil)
	} else {
		env = CleanEnv(env)
	}

	// Plain Command so Launch's ctx cancel does not kill the child; Join owns lifetime.
	cmd := exec.Command(snap.Argv[0], snap.Argv[1:]...)
	cmd.Dir = snap.WorkDir
	cmd.Env = env
	cmd.Stdout = &sinkWriter{sink: sink, stderr: false}
	cmd.Stderr = &sinkWriter{sink: sink, stderr: true}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_ = ctx

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLaunchFailed, err)
	}
	started := clock.Now().UTC()
	h := &fixtureHandle{
		id: Identity{
			AttemptID: snap.AttemptID,
			PID:       cmd.Process.Pid,
			StartedAt: started,
			Request:   snap,
		},
		cmd:   cmd,
		clock: clock,
		done:  make(chan struct{}),
	}
	go h.wait()
	return h, nil
}

type fixtureHandle struct {
	id    Identity
	cmd   *exec.Cmd
	clock Clock

	mu       sync.Mutex
	state    ProcessState
	exitCode int
	exited   bool
	signaled bool
	waitErr  error
	done     chan struct{}
	joined   bool
}

func (h *fixtureHandle) wait() {
	err := h.cmd.Wait()
	code := 0
	if h.cmd.ProcessState != nil {
		code = h.cmd.ProcessState.ExitCode()
	}
	h.mu.Lock()
	h.exited = true
	h.exitCode = code
	h.waitErr = err
	h.state = StateExited
	h.mu.Unlock()
	close(h.done)
}

func (h *fixtureHandle) Identity() Identity { return h.id }

func (h *fixtureHandle) Observe(ctx context.Context) (Observation, error) {
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
	obs := Observation{PID: h.id.PID, ObservedAt: now, State: h.state}
	if h.state == "" {
		obs.State = StateAlive
	}
	if h.exited {
		obs.State = StateExited
		c := h.exitCode
		obs.ExitCode = &c
		obs.EvidenceNote = "process waited"
	} else if processAlive(h.id.PID) {
		obs.State = StateAlive
		obs.EvidenceNote = "os alive"
	} else {
		obs.State = StateUnknown
		obs.EvidenceNote = "pid not alive but wait incomplete"
	}
	return obs, nil
}

func (h *fixtureHandle) Signal(ctx context.Context, kind SignalKind) error {
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
	h.mu.Unlock()

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
	// Signal process group when Setpgid was used.
	if err := syscall.Kill(-h.id.PID, sig); err != nil {
		// Fall back to process signal.
		if h.cmd.Process == nil {
			return fmt.Errorf("%w: %v", ErrSignalFailed, err)
		}
		if err2 := h.cmd.Process.Signal(sig); err2 != nil {
			return fmt.Errorf("%w: %v", ErrSignalFailed, err2)
		}
	}
	h.mu.Lock()
	h.signaled = true
	h.mu.Unlock()
	return nil
}

func (h *fixtureHandle) Join(ctx context.Context) (JoinResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	if h.joined && h.exited {
		code := h.exitCode
		elapsed := h.clock.Now().UTC().Sub(h.id.StartedAt)
		signaled := h.signaled
		h.mu.Unlock()
		out := OutcomeCompleted
		if signaled {
			out = OutcomeSignalled
		}
		return JoinResult{Terminal: TerminalEvidence{
			Exited: true, ExitCode: code, Killed: signaled && code != 0,
			Elapsed: elapsed, Outcome: out,
		}}, nil
	}
	h.mu.Unlock()

	// Hard cap from request.
	var timer <-chan time.Time
	var cancel context.CancelFunc
	joinCtx := ctx
	if h.id.Request.HardCap > 0 {
		joinCtx, cancel = context.WithTimeout(ctx, h.id.Request.HardCap)
		defer cancel()
		// Also enforce hard kill on timeout.
		go func() {
			<-joinCtx.Done()
			if joinCtx.Err() == context.DeadlineExceeded {
				_ = syscall.Kill(-h.id.PID, syscall.SIGKILL)
				if h.cmd.Process != nil {
					_ = h.cmd.Process.Kill()
				}
			}
		}()
	}
	_ = timer

	select {
	case <-h.done:
		h.mu.Lock()
		h.joined = true
		code := h.exitCode
		signaled := h.signaled
		elapsed := h.clock.Now().UTC().Sub(h.id.StartedAt)
		h.mu.Unlock()
		out := OutcomeCompleted
		if signaled {
			out = OutcomeSignalled
		}
		return JoinResult{Terminal: TerminalEvidence{
			Exited: true, ExitCode: code, Killed: signaled,
			Elapsed: elapsed, Outcome: out, Note: "joined",
		}}, nil
	case <-joinCtx.Done():
		// Strongest evidence: may still be running.
		obs, _ := h.Observe(context.Background())
		elapsed := h.clock.Now().UTC().Sub(h.id.StartedAt)
		term := TerminalEvidence{
			Exited: obs.State == StateExited, Elapsed: elapsed,
			Outcome: OutcomeDeadline, Note: "join incomplete: " + string(obs.State),
		}
		if obs.ExitCode != nil {
			term.ExitCode = *obs.ExitCode
		}
		if term.Exited {
			h.mu.Lock()
			h.joined = true
			h.mu.Unlock()
			return JoinResult{Terminal: term}, nil
		}
		return JoinResult{Terminal: term}, fmt.Errorf("%w: %v", ErrJoinIncomplete, joinCtx.Err())
	}
}

func validateLaunch(req LaunchRequest) error {
	if strings.TrimSpace(req.AttemptID) == "" {
		return fmt.Errorf("%w: attempt id required", ErrInvalidLaunch)
	}
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return fmt.Errorf("%w: argv required", ErrInvalidLaunch)
	}
	return nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, Signal(0) checks existence.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

type sinkWriter struct {
	sink   OutputSink
	stderr bool
}

func (w *sinkWriter) Write(p []byte) (int, error) {
	if w.sink == nil {
		return len(p), nil
	}
	// Copy to avoid retaining caller's buffer.
	cp := append([]byte(nil), p...)
	if w.stderr {
		w.sink.WriteStderr(cp)
	} else {
		w.sink.WriteStdout(cp)
	}
	return len(p), nil
}
