//go:build darwin && arm64

package providerinventory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type claudePTYWaitResult struct {
	err   error
	state *os.ProcessState
}

type claudePTYReadResult struct {
	err       error
	truncated bool
}

func runClaudeUsagePTY(ctx context.Context, req ClaudePTYRequest) (ClaudePTYResult, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return ClaudePTYResult{ExitCode: -1}, errors.New("claude pty argv is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = claudeQuotaTimeout
	}
	cols := req.Columns
	if cols <= 0 {
		cols = claudeQuotaColumns
	}
	rows := req.Rows
	if rows <= 0 {
		rows = claudeQuotaRows
	}

	budget := newOutputBudget(req.CombinedLimitBytes)
	output := newBoundedBuffer(req.StdoutLimitBytes, budget)
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Cwd
	cmd.Env = append([]string{}, req.Env...)
	master, slave, err := pty.Open()
	if err != nil {
		return ClaudePTYResult{ExitCode: -1}, err
	}
	if err := pty.Setsize(master, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return ClaudePTYResult{ExitCode: -1}, err
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return ClaudePTYResult{ExitCode: -1}, err
	}
	_ = slave.Close()

	waitCh := make(chan claudePTYWaitResult, 1)
	go func() {
		err := cmd.Wait()
		waitCh <- claudePTYWaitResult{err: err, state: cmd.ProcessState}
	}()

	readCh := make(chan claudePTYReadResult, 1)
	go func() {
		readCh <- readClaudePTY(master, output)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	killed := false
	closeMaster := func() {
		_ = master.Close()
	}
	killProcessTree := func() {
		if cmd.Process == nil {
			return
		}
		killed = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
	drain := func() (claudePTYWaitResult, claudePTYReadResult) {
		closeMaster()
		wr := <-waitCh
		rr := <-readCh
		return wr, rr
	}
	drainRemaining := func(waitDone bool, wr claudePTYWaitResult, readDone bool, rr claudePTYReadResult) (claudePTYWaitResult, claudePTYReadResult) {
		closeMaster()
		if !waitDone {
			wr = <-waitCh
		}
		if !readDone {
			rr = <-readCh
		}
		return wr, rr
	}

	if req.Input != "" {
		if _, err := io.WriteString(master, req.Input); err != nil {
			killProcessTree()
			wr, rr := drain()
			out := claudePTYResultFrom(output, wr, rr, false, killed)
			return out, fmt.Errorf("claude pty write: %w", err)
		}
	}

	var wr claudePTYWaitResult
	var rr claudePTYReadResult
	waitDone := false
	readDone := false
	for {
		select {
		case wr = <-waitCh:
			waitDone = true
			if !readDone {
				continue
			}
			closeMaster()
			return claudePTYResultFrom(output, wr, rr, false, killed), claudePTYWaitError(wr)
		case rr = <-readCh:
			readDone = true
			if rr.truncated {
				killProcessTree()
				wr, rr = drainRemaining(waitDone, wr, readDone, rr)
				return claudePTYResultFrom(output, wr, rr, false, killed), nil
			}
			if rr.err != nil {
				killProcessTree()
				wr, rr = drainRemaining(waitDone, wr, readDone, rr)
				out := claudePTYResultFrom(output, wr, rr, false, killed)
				return out, fmt.Errorf("claude pty read: %w", rr.err)
			}
			if !waitDone {
				continue
			}
			closeMaster()
			return claudePTYResultFrom(output, wr, rr, false, killed), claudePTYWaitError(wr)
		case <-timer.C:
			killProcessTree()
			wr, rr = drainRemaining(waitDone, wr, readDone, rr)
			return claudePTYResultFrom(output, wr, rr, true, killed), context.DeadlineExceeded
		case <-ctx.Done():
			killProcessTree()
			wr, rr = drainRemaining(waitDone, wr, readDone, rr)
			return claudePTYResultFrom(output, wr, rr, false, killed), ctx.Err()
		}
	}
}

func readClaudePTY(master *os.File, output *boundedBuffer) claudePTYReadResult {
	buf := make([]byte, 4096)
	for {
		n, err := master.Read(buf)
		if n > 0 {
			_, _ = output.Write(buf[:n])
			if output.Truncated() {
				return claudePTYReadResult{truncated: true}
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, os.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) {
			return claudePTYReadResult{}
		}
		return claudePTYReadResult{err: err}
	}
}

func claudePTYResultFrom(output *boundedBuffer, wait claudePTYWaitResult, read claudePTYReadResult, timedOut bool, killed bool) ClaudePTYResult {
	exitCode := -1
	if wait.state != nil {
		exitCode = wait.state.ExitCode()
	}
	out := ClaudePTYResult{
		Output:    output.String(),
		ExitCode:  exitCode,
		TimedOut:  timedOut,
		Killed:    killed,
		Truncated: output.Truncated() || read.truncated,
	}
	if out.Truncated {
		out.Stderr = "[loopcoder] claude usage PTY output truncated"
	}
	return out
}

func claudePTYWaitError(wait claudePTYWaitResult) error {
	if wait.err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(wait.err, &exitErr) {
		return nil
	}
	return wait.err
}
