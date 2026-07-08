//go:build windows

package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAliveTasklistTimesOut(t *testing.T) {
	oldCommand := tasklistCommand
	oldHardCap := livenessHardCap
	sentinel := filepath.Join(t.TempDir(), "helper-completed")
	tasklistCommand = func(int) (string, []string) {
		return os.Args[0], []string{"-test.run=TestProcessWindowsExecHelper", "--", "sleep-then-write", "500ms", sentinel}
	}
	livenessHardCap = 50 * time.Millisecond
	t.Setenv("GO_WANT_PROCESS_WINDOWS_HELPER", "1")
	t.Cleanup(func() {
		tasklistCommand = oldCommand
		livenessHardCap = oldHardCap
	})

	start := time.Now()
	if Alive(os.Getpid()) {
		t.Fatal("Alive = true, want false after tasklist timeout")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Alive elapsed = %s, want bounded timeout", elapsed)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tasklist helper completed after hard cap; sentinel stat err = %v", err)
	}
}

func TestProcessWindowsExecHelper(t *testing.T) {
	if os.Getenv("GO_WANT_PROCESS_WINDOWS_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "missing helper mode")
		os.Exit(2)
	}
	switch mode := os.Args[separator+1]; mode {
	case "sleep":
		time.Sleep(parseHelperDuration(os.Args[separator+2]))
	case "sleep-then-write":
		time.Sleep(parseHelperDuration(os.Args[separator+2]))
		if err := os.WriteFile(os.Args[separator+3], []byte("completed"), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write sentinel: %v\n", err)
			os.Exit(2)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

func parseHelperDuration(value string) time.Duration {
	duration, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse duration %q: %v\n", value, err)
		os.Exit(2)
	}
	return duration
}
