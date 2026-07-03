package supervisedexec

import (
	"os/exec"
	"testing"
)

func clearRegistry() {
	registry.mu.Lock()
	registry.procs = make(map[int]*managedProc)
	registry.mu.Unlock()
}

func TestApplyEnvMarkers(t *testing.T) {
	cmd := &exec.Cmd{}
	applyEnvMarkers(cmd, "run-123", "worker")
	found := map[string]bool{}
	for _, e := range cmd.Env {
		found[e] = true
	}
	for _, want := range []string{"LOOPCODER_MANAGED=1", "LOOPCODER_RUN_ID=run-123", "LOOPCODER_ROLE=worker"} {
		if !found[want] {
			t.Fatalf("env marker %q missing from %v", want, cmd.Env)
		}
	}
}

func TestApplyEnvMarkersOmitsEmptyRunIDAndRole(t *testing.T) {
	cmd := &exec.Cmd{}
	applyEnvMarkers(cmd, "", "")
	managed := false
	for _, e := range cmd.Env {
		if e == "LOOPCODER_MANAGED=1" {
			managed = true
		}
		if len(e) >= 16 && e[:16] == "LOOPCODER_RUN_ID" {
			t.Fatalf("unexpected empty run-id marker %q", e)
		}
	}
	if !managed {
		t.Fatal("LOOPCODER_MANAGED marker missing")
	}
}

type fakeGroup struct {
	killed bool
	closed bool
}

func (f *fakeGroup) prepare(cmd *exec.Cmd)     {}
func (f *fakeGroup) adopt(cmd *exec.Cmd) error { return nil }
func (f *fakeGroup) kill() error               { f.killed = true; return nil }
func (f *fakeGroup) close()                    { f.closed = true }

func TestManagedListsRegisteredAndShutdownReaps(t *testing.T) {
	clearRegistry()
	fake := &fakeGroup{}
	registry.mu.Lock()
	registry.procs[424242] = &managedProc{
		info:  ProcInfo{PID: 424242, RunID: "run-x", Role: "worker"},
		group: fake,
	}
	registry.mu.Unlock()

	managed := Managed()
	if len(managed) != 1 || managed[0].PID != 424242 || managed[0].Role != "worker" {
		t.Fatalf("Managed() = %+v, want one worker pid 424242", managed)
	}

	n := Shutdown()
	if n != 1 {
		t.Fatalf("Shutdown() = %d, want 1", n)
	}
	if !fake.killed || !fake.closed {
		t.Fatalf("fake group not reaped: killed=%v closed=%v", fake.killed, fake.closed)
	}
	if len(Managed()) != 0 {
		t.Fatalf("registry not cleared after Shutdown: %+v", Managed())
	}
}

func TestShutdownEmptyRegistry(t *testing.T) {
	clearRegistry()
	if n := Shutdown(); n != 0 {
		t.Fatalf("Shutdown() on empty registry = %d, want 0", n)
	}
}
