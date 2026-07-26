package runtimefacade_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/runtimefacade"
)

func TestFixtureLaunchObserveJoinSuccess(t *testing.T) {
	ctx := context.Background()
	rt := &runtimefacade.FixtureRuntime{}
	h, err := rt.Launch(ctx, runtimefacade.LaunchRequest{
		AttemptID: "att-1",
		Argv:      []string{"/bin/sh", "-c", "echo hello; exit 0"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	id := h.Identity()
	if id.PID <= 0 || id.AttemptID != "att-1" {
		t.Fatalf("identity = %#v", id)
	}
	// Request must be immutable snapshot (clone).
	if len(id.Request.Argv) != 3 {
		t.Fatalf("argv snapshot = %#v", id.Request.Argv)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		obs, err := h.Observe(ctx)
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		if obs.State == runtimefacade.StateExited {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	join, err := h.Join(ctx)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if !join.Success() || !join.Terminal.Exited {
		t.Fatalf("join = %#v", join)
	}
	if join.Terminal.ExitCode != 0 {
		t.Fatalf("exit = %d", join.Terminal.ExitCode)
	}
}

func TestFixtureSignalAndJoin(t *testing.T) {
	ctx := context.Background()
	rt := &runtimefacade.FixtureRuntime{}
	h, err := rt.Launch(ctx, runtimefacade.LaunchRequest{
		AttemptID: "att-sig",
		Argv:      []string{"/bin/sh", "-c", "sleep 30"},
		HardCap:   10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := h.Signal(ctx, runtimefacade.SignalTerm); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	join, err := h.Join(context.Background())
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if !join.Terminal.Exited {
		t.Fatalf("expected exited after signal: %#v", join)
	}
}

func TestJoinIncompleteWhenCancelled(t *testing.T) {
	rt := &runtimefacade.FixtureRuntime{}
	h, err := rt.Launch(context.Background(), runtimefacade.LaunchRequest{
		AttemptID: "att-hang",
		Argv:      []string{"/bin/sh", "-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	join, err := h.Join(ctx)
	if !errors.Is(err, runtimefacade.ErrJoinIncomplete) {
		t.Fatalf("err = %v, join=%#v", err, join)
	}
	// Strongest evidence retained on incomplete join.
	if join.Terminal.Elapsed <= 0 {
		t.Fatalf("expected elapsed evidence: %#v", join.Terminal)
	}
	// Cleanup
	_ = h.Signal(context.Background(), runtimefacade.SignalKill)
	_, _ = h.Join(context.Background())
}

func TestInvalidLaunchOwnsNoProcess(t *testing.T) {
	rt := &runtimefacade.FixtureRuntime{}
	_, err := rt.Launch(context.Background(), runtimefacade.LaunchRequest{
		AttemptID: "",
		Argv:      []string{"/bin/true"},
	})
	if !errors.Is(err, runtimefacade.ErrInvalidLaunch) {
		t.Fatalf("err = %v", err)
	}
	_, err = rt.Launch(context.Background(), runtimefacade.LaunchRequest{
		AttemptID: "x",
		Argv:      []string{"/nonexistent/loopcoder-runtime-fixture-binary"},
	})
	if !errors.Is(err, runtimefacade.ErrLaunchFailed) {
		t.Fatalf("err = %v", err)
	}
}

func TestCleanEnvStripsSecrets(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"OPENAI_API_KEY=sk-test",
		"GITHUB_TOKEN=ghs_x",
		"SAFE=1",
	}
	got := runtimefacade.CleanEnv(base)
	joined := ""
	for _, e := range got {
		joined += e + "\n"
		if e == "OPENAI_API_KEY=sk-test" || e == "GITHUB_TOKEN=ghs_x" {
			t.Fatalf("secret leaked: %s", e)
		}
	}
	if !containsLine(got, "SAFE=1") {
		t.Fatalf("SAFE lost: %v", got)
	}
	_ = joined
}

func TestSupervisedRuntimeGenericCommand(t *testing.T) {
	ctx := context.Background()
	rt := &runtimefacade.SupervisedRuntime{}
	h, err := rt.Launch(ctx, runtimefacade.LaunchRequest{
		AttemptID: "sup-1",
		Argv:      []string{"/bin/sh", "-c", "echo supervised; exit 3"},
		HardCap:   10 * time.Second,
		Role:      "test",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if h.Identity().PID <= 0 {
		t.Fatal("expected pid")
	}
	join, err := h.Join(ctx)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if !join.Success() || join.Terminal.ExitCode != 3 {
		t.Fatalf("join = %#v", join)
	}
	// Provider prose cannot declare completion: ExitCode alone without Exited is not Success.
	partial := runtimefacade.JoinResult{Terminal: runtimefacade.TerminalEvidence{ExitCode: 0, Exited: false}}
	if partial.Success() {
		t.Fatal("Success must require Exited")
	}
}

func TestLaunchRequestCloneImmutable(t *testing.T) {
	req := runtimefacade.LaunchRequest{
		AttemptID: "c",
		Argv:      []string{"a", "b"},
		Env:       []string{"X=1"},
	}
	cl := req.Clone()
	req.Argv[0] = "mutated"
	req.Env[0] = "Y=2"
	if cl.Argv[0] != "a" || cl.Env[0] != "X=1" {
		t.Fatalf("clone not independent: %#v", cl)
	}
}

func TestFixtureWorkDir(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")
	rt := &runtimefacade.FixtureRuntime{}
	h, err := rt.Launch(context.Background(), runtimefacade.LaunchRequest{
		AttemptID: "wd",
		WorkDir:   dir,
		Argv:      []string{"/bin/sh", "-c", "pwd > marker.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("empty marker")
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
