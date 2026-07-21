package acceptharness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// ProviderMode controls fake provider process behavior.
type ProviderMode string

const (
	ProviderEmitOutput     ProviderMode = "emit"
	ProviderSilent         ProviderMode = "silent"
	ProviderSpawnChild     ProviderMode = "spawn_child"
	ProviderNonZero        ProviderMode = "nonzero"
	ProviderHang           ProviderMode = "hang"
	ProviderIgnoreStop     ProviderMode = "ignore_stop"
	ProviderFlood          ProviderMode = "flood"
	ProviderCompleteRecord ProviderMode = "complete"
)

// ProviderResult is the fixed completion record.
type ProviderResult struct {
	Mode       ProviderMode
	ExitCode   int
	Stdout     string
	Completion string
	PID        int
	Children   []int
}

// ProcessObserver tracks live fixture process PIDs.
type ProcessObserver struct {
	mu   sync.Mutex
	pids map[int]struct{}
}

// NewProcessObserver returns an empty observer.
func NewProcessObserver() *ProcessObserver {
	return &ProcessObserver{pids: map[int]struct{}{}}
}

func (o *ProcessObserver) track(pid int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pids[pid] = struct{}{}
}

func (o *ProcessObserver) untrack(pid int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.pids, pid)
}

// LivePIDs returns currently tracked PIDs that still appear to be alive.
func (o *ProcessObserver) LivePIDs() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	var live []int
	for pid := range o.pids {
		if processAlive(pid) {
			live = append(live, pid)
		}
	}
	return live
}

// FakeProvider runs a synthetic provider helper subprocess.
type FakeProvider struct {
	WorkDir  string
	Mode     ProviderMode
	Observer *ProcessObserver
	// StopSignal is sent on graceful stop; default SIGTERM.
	StopSignal syscall.Signal
	// helperPath is built once.
	helperPath string
}

// EnsureHelper builds the fake-provider helper binary under workDir.
func (p *FakeProvider) EnsureHelper() error {
	if p.WorkDir == "" {
		return fmt.Errorf("acceptharness: provider WorkDir required")
	}
	if p.helperPath != "" {
		return nil
	}
	srcDir := filepath.Join(p.WorkDir, "fakeprovider-src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		return err
	}
	src := filepath.Join(srcDir, "main.go")
	if err := os.WriteFile(src, []byte(fakeProviderMain), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte("module fakeprovider\n\ngo 1.22\n"), 0o600); err != nil {
		return err
	}
	out := filepath.Join(p.WorkDir, "fakeprovider")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = CleanProcessEnv(nil)
	cmd.Dir = srcDir
	if body, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build fakeprovider: %w: %s", err, strings.TrimSpace(string(body)))
	}
	p.helperPath = out
	return nil
}

// Run starts the fake provider under ctx and waits for completion.
func (p *FakeProvider) Run(ctx context.Context) (ProviderResult, error) {
	if err := p.EnsureHelper(); err != nil {
		return ProviderResult{}, err
	}
	if p.Observer == nil {
		p.Observer = NewProcessObserver()
	}
	mode := p.Mode
	if mode == "" {
		mode = ProviderCompleteRecord
	}
	cmd := exec.CommandContext(ctx, p.helperPath, "-mode", string(mode))
	cmd.Env = CleanProcessEnv(nil)
	cmd.Dir = p.WorkDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ProviderResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return ProviderResult{}, err
	}
	pid := cmd.Process.Pid
	p.Observer.track(pid)
	defer p.Observer.untrack(pid)

	// For hang/ignore_stop, allow caller cancel; we still wait.
	done := make(chan error, 1)
	var output strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := stdout.Read(buf)
			if n > 0 {
				output.Write(buf[:n])
			}
			if readErr != nil {
				break
			}
		}
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		exitCode := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
			} else if ctx.Err() != nil {
				_ = killProcessGroup(pid)
				return ProviderResult{Mode: mode, ExitCode: -1, Stdout: output.String(), PID: pid}, ctx.Err()
			} else {
				return ProviderResult{}, err
			}
		}
		children := parseChildPIDs(output.String())
		for _, cpid := range children {
			p.Observer.track(cpid)
			// children exit with parent modes; ensure cleanup tracking
			p.Observer.untrack(cpid)
		}
		completion := ""
		if strings.Contains(output.String(), "COMPLETION_RECORD") {
			completion = "loopcoder.fake_provider.completion.v1"
		}
		return ProviderResult{
			Mode:       mode,
			ExitCode:   exitCode,
			Stdout:     output.String(),
			Completion: completion,
			PID:        pid,
			Children:   children,
		}, nil
	case <-ctx.Done():
		_ = killProcessGroup(pid)
		<-done
		return ProviderResult{Mode: mode, ExitCode: -1, Stdout: output.String(), PID: pid}, ctx.Err()
	}
}

// GracefulStop sends StopSignal to the process group. Used by ignore_stop tests
// that start the process externally; Run itself relies on context cancel.
func (p *FakeProvider) GracefulStop(pid int) error {
	sig := p.StopSignal
	if sig == 0 {
		sig = syscall.SIGTERM
	}
	return syscall.Kill(-pid, sig)
}

func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

func parseChildPIDs(stdout string) []int {
	var out []int
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "CHILD_PID=") {
			n, err := strconv.Atoi(strings.TrimPrefix(line, "CHILD_PID="))
			if err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// fakeProviderMain is a tiny helper compiled at test time.
const fakeProviderMain = `package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	mode := flag.String("mode", "complete", "provider mode")
	flag.Parse()
	switch *mode {
	case "emit":
		fmt.Println("synthetic provider output line")
		fmt.Println("COMPLETION_RECORD")
		os.Exit(0)
	case "silent":
		fmt.Println("COMPLETION_RECORD")
		os.Exit(0)
	case "spawn_child":
		cmd := exec.Command(os.Args[0], "-mode", "silent")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Printf("CHILD_PID=%d\n", cmd.Process.Pid)
		_ = cmd.Wait()
		fmt.Println("COMPLETION_RECORD")
		os.Exit(0)
	case "nonzero":
		fmt.Fprintln(os.Stderr, "synthetic failure")
		os.Exit(7)
	case "flood":
		for i := 0; i < 200; i++ {
			fmt.Printf("flood-line-%d\n", i)
		}
		fmt.Println("COMPLETION_RECORD")
		os.Exit(0)
	case "hang":
		select {}
	case "ignore_stop":
		signal.Ignore(syscall.SIGTERM)
		// stay alive until SIGKILL
		for {
			time.Sleep(50 * time.Millisecond)
		}
	case "complete":
		fmt.Println("COMPLETION_RECORD")
		os.Exit(0)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", *mode)
		os.Exit(2)
	}
	_ = strings.TrimSpace
}
`
